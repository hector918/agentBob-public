package session

import (
	"context"
	"log/slog"
)

// RecoverPendingOnBoot clears the in-flight WAL rows left by the last shutdown so they
// don't resurface or accumulate across crashes. A restart no longer PUSHES anything to
// users (the "I restarted — please re-send" notice was removed): the entry WAL is
// recovered silently. Internal-source events never reach this WAL (recordPending
// skips them); their durability is planned for agora's own dispatch-queue table
// (bob_agora_dispatch_queue), not here. Synchronous + quick (a few store ops) — the
// inbound flow calls it on boot.
func (m *Manager) RecoverPendingOnBoot(ctx context.Context) {
	chats, err := m.store.ListPendingChats(ctx)
	if err != nil {
		slog.Warn("restart recovery: could not list pending chats; skipping", "err", err)
		return
	}
	if len(chats) == 0 {
		return
	}
	for _, c := range chats {
		if ctx.Err() != nil {
			return // boot cancelled (fast shutdown) — stop clearing; the rest reaps next boot
		}
		// Remove one chat's SNAPSHOT pending rows. Rows recorded after the snapshot
		// are left alone (they keep their crash-recovery protection). Best-effort.
		if err := m.store.RemovePendingForChat(ctx, c.Source, c.ChatID, c.ThreadID, c.MsgIDs); err != nil {
			slog.Warn("restart recovery: pending clear failed; rows will resurface on next boot",
				"source", c.Source, "chat", c.ChatID, "thread", c.ThreadID, "err", err)
		}
	}
	slog.Info("restart recovery: cleared stale pending rows", "chats", len(chats))
}
