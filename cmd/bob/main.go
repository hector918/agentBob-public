// Command bob is the entry point of the trunk-rebuild architecture: it builds
// the trunk, registers modules, starts them in dependency order, serves until a
// signal, then stops them in reverse order.
//
// The core request path is roughly gateway→source→session→turn: a passed message
// reaches the session arbiter, the router selects a flow, which composes the prompt
// and drives the turn to stream a model reply back out through stoma. That is only a
// sketch — the authoritative, never-stale inventory of every registered module is the
// t.Register block in main() below.
//
// Env:
//
//	BOB_POSTGRES_DSN  Postgres DSN (required; BOB_DSN accepted as a fallback)
//	BOB_HOME       config + policy dir (default "."); $BOB_HOME/config.yaml,
//	               $BOB_HOME/sources/*.yaml
//	BOB_LOG_LEVEL  debug|info|warn|error (default "info")
//	BOB_LOG_TARGET stdout|file|both (default "both" — a rotating $BOB_HOME/logs/
//	               agent.log PLUS stderr; set "stdout"/"file" to pick one)
//	BOB_SANDBOX_RETENTION_DAYS  age-prune window for captured inbound files (default 30)
//	BOB_SANDBOX_MAX_MB  total sandbox cap in MB (default 0 = off, no cap)
//	BOB_WEBUI_ADDR  webui http listen addr (default "127.0.0.1:9877"; "off" disables the server)
//	BOB_ADMIN_OUTLET  admin outlet address for adminline (default unattached)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"agentbob/contract"
	flowagora "agentbob/flow/agora"
	"agentbob/flow/inbound"
	"agentbob/flow/intro"
	"agentbob/flow/normal"
	"agentbob/flow/router"
	"agentbob/heartwood/clock"
	"agentbob/heartwood/files"
	"agentbob/heartwood/prompt"
	"agentbob/i18n"
	"agentbob/leaf/accounts"
	"agentbob/leaf/adminline"
	"agentbob/leaf/agora"
	"agentbob/leaf/arrangement"
	"agentbob/leaf/asr"
	"agentbob/leaf/claimtoken"
	"agentbob/leaf/credentials"
	"agentbob/leaf/gate"
	"agentbob/leaf/learn"
	"agentbob/leaf/model"
	"agentbob/leaf/modelgate"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/retrieval"
	"agentbob/leaf/session"
	"agentbob/leaf/skills"
	"agentbob/leaf/slash"
	"agentbob/leaf/stoma"
	"agentbob/leaf/stoma/sources"
	"agentbob/leaf/tools"
	"agentbob/leaf/tools/wordpress"
	"agentbob/leaf/turn"
	"agentbob/leaf/urllib"
	"agentbob/leaf/warrant"
	"agentbob/leaf/webui"
	"agentbob/logging"
	"agentbob/trunk"
)

func main() {
	home := envOr("BOB_HOME", ".")
	// BOB_POSTGRES_DSN is the conventional name (matches .env.example / the existing
	// deploys); BOB_DSN is accepted as a fallback.
	dsn := envOr("BOB_POSTGRES_DSN", os.Getenv("BOB_DSN"))
	if dsn == "" {
		slog.Error("Postgres DSN required — set BOB_POSTGRES_DSN")
		os.Exit(2)
	}

	// Default to "both": a rotating file at $BOB_HOME/logs/agent.log (10MB × 3,
	// the writer's defaults) AND stderr — so logs persist on disk by default
	// (docker logs alone vanish on container recreate) without losing `docker logs`.
	if _, err := logging.Setup(envOr("BOB_LOG_TARGET", "both"), filepath.Join(home, "logs"),
		envOr("BOB_LOG_LEVEL", "info")); err != nil {
		slog.Error("logging setup failed", "err", err)
		os.Exit(1)
	}

	// Stamp the running build into $BOB_HOME/VERSION + the log so a deploy can
	// confirm which version is live.
	slog.Info("bob starting", "version", recordVersion(home))

	// Load the i18n catalog (embedded base + optional $BOB_HOME overrides) so
	// T() renders text instead of raw keys.
	if err := i18n.Reload(home); err != nil {
		// Reload degrades: a broken optional overlay still serves the embedded
		// catalog (only the overlay is lost); raw keys appear only if the
		// EMBEDDED catalog itself failed to parse (a build defect).
		slog.Error("i18n overlay unusable — serving embedded strings without it", "err", err)
	}

	srcCfg, err := sources.LoadConfig(filepath.Join(home, "config.yaml"))
	if err != nil {
		slog.Error("source config load failed", "err", err)
		os.Exit(1)
	}
	fileStore := files.New(filepath.Join(home, "sandbox"))
	srcs := sources.Assemble(srcCfg, fileStore)

	// Cold-memory feed config (off by default). A parse error → disabled defaults
	// (optional feature, don't abort boot). See docs/cold-memory-trunk.md.
	retrCfg, err := retrieval.LoadConfig(filepath.Join(home, "config.yaml"))
	if err != nil {
		slog.Warn("retrieval config load failed; cold-memory feed disabled", "err", err)
	}

	t := trunk.New()
	// Sandbox attachment housekeeping: age-prune (default 30d) + optional total cap
	// (BOB_SANDBOX_MAX_MB, 0=off) so captured inbound files don't grow unbounded.
	t.Register(files.NewSweeper(fileStore,
		envInt("BOB_SANDBOX_RETENTION_DAYS", 30),
		int64(envInt("BOB_SANDBOX_MAX_MB", 0))*1024*1024,
		filepath.Join(home, "spaces"))) // also age-prune placed attachments under spaces/*/inbox
	t.Register(pgpool.New(dsn))
	t.Register(clock.New())      // DB-calibrated process clock — after pgpool (provides DB)
	t.Register(claimtoken.New()) // token auth facility — before gate (gate-admit token mint/verify)
	t.Register(gate.New(filepath.Join(home, "sources")))
	// webui before the panel-owning modules: it provides contract.PanelRegistry,
	// which they Need (hard edge) to describe their layers. BOB_WEBUI_ADDR="off"
	// disables the http server (the registry is still provided).
	t.Register(webui.New(envOr("BOB_WEBUI_ADDR", "127.0.0.1:9877")))
	t.Register(model.New(home))
	t.Register(asr.New()) // voice→text at ingestion (contract.Transcriber over the pool's KindASR); session consumes optionally
	credMod := credentials.New(home)
	// Register credential kinds whose factory lives in the consuming tool's package
	// (keeps provider types out of leaf/credentials). wordpress: a kind=wordpress
	// credential builds a *wordpress.Client the wordpress tools resolve per identity.
	credMod.RegisterKind("wordpress", wordpress.BuildClient)
	t.Register(credMod)
	t.Register(accounts.New())
	// modelgate: OpenAI-compatible API over the pool (needs ModelPool; keys via
	// accounts' contract.APIKeys, resolved lazily). Default off — set the addr to
	// serve. Registration order irrelevant (hard-Needs ModelPool; the trunk topo-sorts).
	t.Register(modelgate.New(envOr("BOB_MODELGATE_ADDR", "off")))
	t.Register(slash.New())
	// Inbound attachments are relocated from their staged path into the resolved
	// scope's SPACE at submit, so user files live where the fs/ocr/vision tools read
	// and persist past the turn. The destination is computed with the same
	// space-dir authority the FileChannel uses (warrant.SpaceLocalDir).
	t.Register(session.New(session.Config{
		PlaceAttachments: func(scope string, atts []contract.Attachment) []contract.Attachment {
			return warrant.PlaceAttachments(home, scope, atts)
		},
	}))
	t.Register(prompt.New())
	t.Register(learn.New())            // before tools: tools registers its learn source at Start
	t.Register(urllib.New())           // resolved lazily by consumers (tools/flows) — registration order irrelevant
	t.Register(retrieval.New(retrCfg)) // cold-memory feed; resolved lazily by consumers — registration order irrelevant
	t.Register(tools.New(home))
	t.Register(skills.New(home)) // before warrant: warrant reconciles skill grants at Start
	t.Register(warrant.New(home))
	t.Register(agora.New(home))   // self-authorizes from tools/skills catalogs (lazy/optional)
	t.Register(arrangement.New()) // role-bucket orchestration; soft-needs agora (role→member) + gateway (nudge) + session (idle gate)
	t.Register(turn.New())
	t.Register(router.New())
	t.Register(normal.New(home))
	t.Register(flowagora.New(home)) // higher Priority (10>0) claims agora-routed scopes; registration order is irrelevant (router sorts by Priority).
	t.Register(intro.New())         // mechanical onboarding for unbound senders (router routes here explicitly)
	t.Register(stoma.New(srcs))
	t.Register(adminline.New(os.Getenv("BOB_ADMIN_OUTLET")))
	t.Register(inbound.New())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := t.Start(ctx); err != nil {
		slog.Error("trunk: start failed", "err", err)
		os.Exit(1)
	}
	slog.Info("bob serving", "sources", len(srcs))

	<-ctx.Done()
	// Restore the default signal disposition before running Stop: a second
	// SIGINT/SIGTERM during a hung shutdown then terminates the process instead
	// of being swallowed by the (still-registered) Notify channel.
	stop()
	slog.Info("bob shutting down")
	if err := t.Stop(context.Background()); err != nil {
		slog.Error("trunk: stop failed", "err", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads an integer env var, falling back to def on unset / unparseable.
// An unparseable value is warned about — for a knob like BOB_SANDBOX_MAX_MB the
// silent fallback would fail open (no cap at all).
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
		slog.Warn("ignoring unparseable integer env var", "key", key, "value", v, "using", def)
	}
	return def
}
