package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/model/modelcfg"

	"gopkg.in/yaml.v3"
)

// panel is the model pool's self-description for the webui (docs/webui-trunk.md).
// It translates the bespoke PoolSnapshot into the generic field vocabulary: a
// couple of stats plus a table whose "state" column carries a status color. The
// PoolSnapshot stays the CLI's shape; this is the ONE place it meets the webui.
// The models.yaml editor rides the same write discipline as flow/normal's
// config.yaml (validate → backup → write); the pool auto-reloads on the file's
// mtime (maybeReload per request), so a save takes effect on the next call.
func (m *Module) panel() contract.Panel {
	path := filepath.Join(m.home, "models.yaml")
	return contract.Panel{
		ID:    "model-pool",
		Title: "Model Pool",
		// AdminOnly: the pool table carries operational detail — providers, token
		// counts, and each entry's LastError (contract marks it admin-only) — so the
		// whole panel is redacted from /api/state until a /webui token (mirrors the
		// skeleton, which redacted Models for non-admin callers).
		AdminOnly: true,
		Settings:  []contract.SettingSpec{{ID: "models.yaml", Kind: "code", Label: "models.yaml", Lang: "yaml"}},
		Read: func(_ context.Context, sid string) (string, error) {
			if sid != "models.yaml" {
				return "", fmt.Errorf("unknown setting %q", sid)
			}
			b, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				return "", nil
			}
			return string(b), err
		},
		Apply: func(_ context.Context, sid, body string) contract.WriteResult {
			if sid != "models.yaml" {
				return contract.WriteResult{Phase: "validate", Error: "unknown setting"}
			}
			// Validate against the typed ModelsConfig + its Validate (same as LoadModels),
			// not just YAML syntax — else a syntactically-valid but semantically-broken config
			// saves "Success" and the pool reload silently rejects it.
			var cfg modelcfg.ModelsConfig
			if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
				return contract.WriteResult{Phase: "validate", Error: err.Error()}
			}
			if err := cfg.Validate(); err != nil {
				return contract.WriteResult{Phase: "validate", Error: err.Error()}
			}
			if b, err := os.ReadFile(path); err == nil {
				_ = os.WriteFile(path+".bak", b, 0o600) // best-effort backup
			}
			// Write temp + rename (atomic on POSIX): the pool's mtime watch
			// re-parses this file asynchronously, so a direct O_TRUNC write
			// risks a torn read (false "hot-reload failed" page) and a crash
			// mid-write would leave a truncated config → NullPool at boot.
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			if err := os.Rename(tmp, path); err != nil {
				_ = os.Remove(tmp)
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			return contract.WriteResult{Success: true, Phase: "reload"} // pool picks it up via mtime watch
		},
		State: func(_ context.Context) []contract.StateField {
			snap := m.pool.Snapshot()
			now := time.Now()
			// "active" is how many BACKENDS are carrying traffic right now, which
			// "in flight" (total concurrent requests) does not say: 9 requests can be
			// one saturated entry or nine idle-ish ones, and those are different
			// operational stories. The denominator counts only ROUTABLE entries, the
			// same set the pool's own capacity math counts (deadKinds: enabled and not
			// paused): a config may carry hundreds of disabled models and an operator
			// may pause most of the rest for maintenance, and counting those would
			// render a fully saturated pool as single-digit utilisation — besides just
			// repeating the "entries" stat sitting two cells over. Cooling entries stay
			// in: they come back on their own, so they are capacity, just not right now.
			active, usable := 0, 0
			for _, e := range snap.Entries {
				if e.State == "disabled" || e.State == "paused" {
					continue
				}
				usable++
				if e.InFlight > 0 {
					active++
				}
			}
			fields := []contract.StateField{
				{Kind: "stat", Label: "in flight", Value: strconv.Itoa(snap.InFlight)},
				// The line BEHIND in-flight. A saturated pool and a saturated pool with
				// callers queued behind it look identical in "in flight" / "active" — this
				// is the only place the waiting side shows.
				{Kind: "stat", Label: "waiting", Value: waitingText(snap.Queues), Status: waitingStatus(snap.Queues)},
				{Kind: "stat", Label: "active", Value: fmt.Sprintf("%d/%d", active, usable)},
				{Kind: "stat", Label: "heartbeat", Value: heartbeatText(snap.Heartbeat.Running)},
				{Kind: "stat", Label: "entries", Value: strconv.Itoa(len(snap.Entries))},
				// "did my models.yaml edit take effect yet" — this is the mtime of the
				// config the pool is SERVING, so a failed reload keeps showing the old
				// one (and pages the admin) instead of acknowledging an edit that
				// never landed.
				{Kind: "stat", Label: "config", Value: clock.Stamp(snap.Reload.LastMtime)},
			}
			cols := []string{"name", "provider", "state", "in", "calls", "err", "tok i/o"}
			rows := make([][]contract.Cell, 0, len(snap.Entries))
			for _, e := range snap.Entries {
				// name cell packs the second line "kind  [tags]  model" (indented under the
				// name) so the routing tags an entry carries are visible at a glance.
				sub := e.Kind
				if len(e.Tags) > 0 {
					sub += "  [" + strings.Join(e.Tags, " ") + "]"
				}
				sub += "  " + e.Model
				// Tags also as a STRUCTURED filter dimension (separate from the display
				// text above) so the table's tag-filter bar can filter by them.
				// The routing limits ride as a "why" detail rather than as more columns:
				// they answer "why was this entry passed over" (too small a window,
				// concurrency full, priority) and are read on demand, not scanned. The
				// renderer keeps ONE detail per row, so the limits and the last error
				// share it — on separate cells the error would shadow the limits for
				// exactly the cooling entries the limits exist to explain.
				detail := entryLimits(e)
				if e.LastError != "" {
					detail += "\n\nlast error: " + e.LastError
				}
				name := contract.Cell{Text: e.Name + "\n" + sub, Tags: e.Tags, Detail: detail}
				// err cell is warn-colored when the entry has a recorded error; the error
				// TEXT rides the row detail above.
				errCell := contract.Cell{Text: errText(e)}
				if e.LastError != "" {
					errCell.Status = "warn"
				}
				rows = append(rows, []contract.Cell{
					name,
					{Text: e.Provider},
					{Text: stateText(e, now), Status: entryStatus(e.State)},
					{Text: strconv.Itoa(e.InFlight)},
					{Text: strconv.FormatInt(e.TotalCalls, 10)},
					errCell,
					{Text: groupDigits(e.TotalInputTokens) + "/" + groupDigits(e.TotalOutputTokens)},
				})
			}
			fields = append(fields, contract.StateField{
				Kind: "table", Label: "pool", Columns: cols, Rows: rows, TagFilter: true,
			})
			return fields
		},
	}
}

// groupDigits renders n with thousands separators (1234567 → "1,234,567") so a
// token counter that has run into the millions is readable at a glance instead
// of being a digit wall the eye has to count.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}

// stateText is the entry's state plus, while it is cooling, how much of the
// cooldown is left. "cooling" alone says an entry is out; it does not say
// whether the next request can use it in 3 seconds or in 5 minutes.
//
// Only "cooling" gets the countdown. A paused or disabled entry can carry a
// live deadUntil too (Pause does not clear it; a reload copies it across), and
// those states do NOT lift on their own — a countdown beside them would read as
// a pause that expires by itself, which no timer will ever do.
func stateText(e contract.ModelInfo, now time.Time) string {
	if e.State != "cooling" || e.DeadUntilUnix <= 0 {
		return e.State
	}
	left := time.Unix(e.DeadUntilUnix, 0).Sub(now).Round(time.Second)
	if left <= 0 {
		return e.State // cooldown elapsed; the pool flips the state on its next touch
	}
	return e.State + " " + left.String()
}

// errText is the error count with the share of this entry's BOOKED outcomes
// that were errors — 300 errors alongside 80,000 successes and 300 alongside
// 300 render identically as a bare count, and they are opposite diagnoses.
//
// TotalCalls books only calls that SUCCEEDED (recordChatSuccess is its sole
// increment; RecordError bumps TotalErrors instead), so the denominator is
// calls + errors. Dividing by calls alone would report 900% for a backend that
// fails nine times per success, and would print no rate at all for the worst
// entry of them all — one that has never succeeded.
//
// NOT the share of attempts: RecordError deliberately declines to book several
// failure classes against the entry (tool-args-truncated, prompt-level 4xx,
// caller cancel / deadline — see its comments), because those are the request's
// fault and every candidate would hit them identically. Those attempts appear
// in neither counter. So a backend rejecting every oversized prompt with a 400
// still reads 0 here — that is the breaker's view of the entry's own health,
// which is also what the cooling state is computed from.
func errText(e contract.ModelInfo) string {
	n := strconv.FormatInt(e.TotalErrors, 10)
	if e.TotalErrors == 0 {
		return n
	}
	rate := 100 * float64(e.TotalErrors) / float64(e.TotalCalls+e.TotalErrors)
	if rate < 0.1 {
		// A real error count beside a parenthetical "0.0%" contradicts itself.
		return n + " (<0.1%)"
	}
	return fmt.Sprintf("%s (%.1f%%)", n, rate)
}

// entryLimits renders the routing limits an entry was built with, each tagged
// with where the value came from (yaml / a provider probe / the built-in
// default). This is the evidence behind a routing decision: an entry skipped for
// a long prompt (window too small) or passed over under load (concurrency cap)
// looks identical to an idle healthy one in every other column.
func entryLimits(e contract.ModelInfo) string {
	parts := make([]string, 0, 3)
	if e.ContextWindow > 0 {
		parts = append(parts, "context "+groupDigits(int64(e.ContextWindow))+provenance(e.ContextSource))
	}
	if e.Concurrency > 0 {
		parts = append(parts, "concurrency "+strconv.Itoa(e.Concurrency)+provenance(e.ConcurrencySource))
	} else {
		parts = append(parts, "concurrency unlimited")
	}
	// Priority prints even at 0: that is the yaml default AND an ordinary rank the
	// picker compares, so omitting it would read as "no data" on exactly the most
	// common entries — the ones an operator is comparing a boosted entry against.
	parts = append(parts, "priority "+strconv.Itoa(e.Priority))
	return strings.Join(parts, " · ")
}

// provenance renders a value's source label as a parenthetical, or nothing when
// the pool didn't record one.
func provenance(src string) string {
	if src == "" {
		return ""
	}
	return " (" + src + ")"
}

// waitingText renders the wait-queue stat: the total blocked, then which kinds
// they are blocked on and how close each is to its cap. A bare total would say
// "someone is queued" without saying on what, which is the only actionable part
// (one saturated kind is a backend problem, every kind is a pool problem).
func waitingText(queues []contract.QueueInfo) string {
	if len(queues) == 0 {
		return "0"
	}
	total := 0
	parts := make([]string, 0, len(queues))
	for _, q := range queues {
		total += q.Waiting
		part := q.Kind + " " + strconv.Itoa(q.Waiting)
		if q.Capacity > 0 {
			part += "/" + strconv.Itoa(q.Capacity)
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

// waitingStatus colors the wait-queue stat: anyone queued is worth noticing
// (it means no free slot anywhere for that kind), a latched full queue is a
// failure — callers are being rejected outright.
func waitingStatus(queues []contract.QueueInfo) string {
	if len(queues) == 0 {
		return ""
	}
	for _, q := range queues {
		if q.Full {
			return "down"
		}
	}
	return "warn"
}

func heartbeatText(running bool) string {
	if running {
		return "probing"
	}
	return "idle"
}

// entryStatus maps a pool entry's State string to a webui status color.
func entryStatus(state string) string {
	switch state {
	case "live":
		return "ok"
	case "tentative", "cooling":
		return "warn"
	case "paused", "disabled":
		return "down"
	default:
		return ""
	}
}
