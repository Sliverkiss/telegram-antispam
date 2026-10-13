package bot

import (
	"adbot/internal/notify"
	"adbot/internal/server"
)

// AIStatsSnapshot adapts the ai.Stats snapshot into the server's plain
// AISnapshot type. bot → server is a one-way dependency (server never imports
// bot), so the Inspector interface stays satisfied without duplication.
func (b *Bot) AIStatsSnapshot() server.AISnapshot {
	sn := b.AIStats.Snapshot()
	return server.AISnapshot{
		ModelCalls:    sn.ModelCalls,
		ModelOKs:      sn.ModelOKs,
		Fallbacks:     sn.Fallbacks,
		VerifyRounds:  sn.VerifyRounds,
		VerifyFlipped: sn.VerifyFlipped,
		LocalFallback: sn.LocalFallback,
	}
}

// NotifySnapshot adapts the notify.Stats snapshot into the server's plain
// NotifyStatsView (same one-way adaptation as AIStatsSnapshot).
func (b *Bot) NotifySnapshot() server.NotifyStatsView {
	ns := b.NotifySnapshotRaw()
	return server.NotifyStatsView{
		Enabled:       ns.Enabled,
		TargetOK:      ns.TargetOK,
		TargetChecked: ns.TargetChecked,
		Sent:          ns.Sent,
		Dropped:       ns.Dropped,
		Suppressed:    ns.Suppressed,
		QueueLen:      ns.QueueLen,
		QueueCap:      ns.QueueCap,
	}
}

// NotifySnapshotRaw returns the notify package's own Stats snapshot.
func (b *Bot) NotifySnapshotRaw() notify.Stats {
	if b.Notifier == nil {
		return notify.Stats{}
	}
	return b.Notifier.Snapshot()
}
