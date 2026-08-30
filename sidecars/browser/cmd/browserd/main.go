// Command browserd hosts bob's browser subsystem out-of-process
// (docs/browserd.md P1 — control-plane isolation). It reuses the full
// tools/browser core (Pool / actions / tabs / custodian) in-process and
// exposes it over the private-network JSON control plane in package
// browserd; bob points tools.browser.browserd_url at it and its 13
// browser_* tools become thin HTTP shells.
//
// Like `bob`, this entry is just flag parsing + wiring; all logic lives in
// the browserd + tools/browser packages. It reads the SAME
// $BOB_HOME/config.yaml (tools.browser.* drives chromium flags / timeouts /
// caps) so one config file governs both processes — point --home (or
// $BOB_HOME) at browserd's own data home when deploying it in a separate
// container.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentbob/sidecars/browser/bobhome"
	"agentbob/sidecars/browser/browserd"
	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/tools/browser"
)

// dataDirSweepTTL mirrors bob's sandboxTTL (pipeline-startup): a legacy
// per-scope chromedp data dir untouched this long with no live session is
// reaped. Long enough that an idle-but-live conversation's dir is never
// removed out from under a warm re-spawn.
const dataDirSweepTTL = 24 * time.Hour

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultListen := os.Getenv("BROWSERD_LISTEN")
	if defaultListen == "" {
		defaultListen = ":8377"
	}
	listen := flag.String("listen", defaultListen,
		"control-plane listen address (PRIVATE network only — no auth on these endpoints; also $BROWSERD_LISTEN)")
	defaultTakeover := os.Getenv("BROWSERD_TAKEOVER_LISTEN")
	if defaultTakeover == "" {
		defaultTakeover = ":8378"
	}
	takeoverListen := flag.String("takeover-listen", defaultTakeover,
		"takeover-face listen address (api-key authed, same key as the control plane; reachable by bob only — bob's webui proxies it to the human; \"off\" disables; also $BROWSERD_TAKEOVER_LISTEN)")
	home := flag.String("home", "", "data home directory (overrides $BOB_HOME; default ~/.bob)")
	flag.Parse()

	if *home != "" {
		bobhome.SetHome(*home)
	}
	if err := bobhome.Init(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// The pool is the single owner of chromium instances + profile checkouts
	// (single-authority state, docs/browserd.md §1). No profile gate is set:
	// authorization is bob's job; browserd only executes pre-gated routes.
	pool := browser.NewPool(cfg.Tools.Browser, cfg.Tools.Filesystem, cfg.Memory.NamespaceByBotEff())

	bsrv := browserd.NewServer(pool, cfg.Tools.Browser)
	srv := &http.Server{Addr: *listen, Handler: bsrv.Handler()}

	// Reap profile-vault staging dirs stranded by a crash mid-import (OOM /
	// SIGKILL bypasses profileImport's defer cleanup). One-shot at startup —
	// each can be hundreds of MB on the persistent volume.
	if n, serr := browserd.SweepStaleStaging(); serr != nil {
		slog.Warn("browserd: stale staging sweep failed", "err", serr)
	} else if n > 0 {
		slog.Info("browserd: removed stale import staging dir(s)", "count", n)
	}

	// Control-plane auth (A1): optional app-layer bearer key on top of network
	// isolation. When unset, warn loudly — the control plane is then guarded by
	// network trust alone and must never be reachable beyond the private subnet
	// / tunnel (docs/browserd.md §6).
	if bsrv.APIKeyConfigured() {
		slog.Info("browserd control plane: API key auth enabled")
	} else {
		slog.Warn("browserd control plane: NO api key configured — relying on network isolation; do NOT expose this port (set BROWSERD_API_KEY or tools.browser.api_key)")
	}

	// Takeover face (docs/browserd.md §4) — its OWN listener so the long-lived
	// SSE streams never tie up the control plane. Consumed by bob's webui
	// proxy only (default port 8378 — bob derives the address from
	// browserd_url's host); like the control plane it must stay private.
	// Timeouts mirror bob's webui server: the SSE handler overrides the
	// write timeout per frame via its ResponseController deadline.
	var tsrv *http.Server
	if *takeoverListen != "" && *takeoverListen != "off" {
		tsrv = &http.Server{
			Addr:         *takeoverListen,
			Handler:      bsrv.TakeoverHandler(),
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 30 * time.Second,
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Idle sweep — same cadence bob's housekeeping drives Pool.SweepIdle at
	// (cleanup.sweep_interval_hours, default 6h) with the same TTL
	// (tools.browser.idle_ttl_seconds, default 30 min). Data dirs are
	// preserved; instances re-spawn warm on the next action.
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.Cleanup.SweepInterval()) * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ttl := time.Duration(cfg.Tools.Browser.IdleTTLSecondsEff()) * time.Second
				if n := pool.SweepIdle(ttl); n > 0 {
					slog.Info("browserd: closed idle browser session(s)", "count", n)
				}
				// Data-dir sweep: SweepIdle preserves data dirs by contract, so
				// without this the legacy per-scope chromedp dirs grow without
				// bound on browserd's volume. In-process mode gets this for free
				// from bob's sandbox sweep; the browserd process has no such
				// reaper of its own. Mirror that sweep's 24h mtime TTL. Runs
				// AFTER SweepIdle so a just-closed scope's dir isn't spared by a
				// stale live-session entry.
				if n := pool.SweepDataDirs(dataDirSweepTTL); n > 0 {
					slog.Info("browserd: removed stale browser data dir(s)", "count", n)
				}
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	if tsrv != nil {
		// A takeover-listener failure is not fatal: the control plane (the
		// reason browserd exists) keeps serving; only human takeover is off.
		go func() {
			if err := tsrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Warn("browserd: takeover listener exited", "addr", tsrv.Addr, "err", err)
			}
		}()
		slog.Info("browserd takeover face listening", "addr", *takeoverListen)
	}
	slog.Info("browserd listening", "addr", *listen, "home", bobhome.Home())

	shutdownTakeover := func() {
		if tsrv == nil {
			return
		}
		// Close (not Shutdown): an open SSE stream never drains, so a
		// graceful wait would always burn the full timeout for nothing.
		_ = tsrv.Close()
	}

	select {
	case <-ctx.Done():
		// Graceful shutdown: stop accepting, then close every chromium —
		// data dirs / profiles preserved (same semantics as bob shutdown,
		// Pool.CloseAll), so the next boot resumes warm.
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
		shutdownTakeover()
		pool.CloseAll()
		return nil
	case err := <-errCh:
		shutdownTakeover()
		pool.CloseAll()
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}
