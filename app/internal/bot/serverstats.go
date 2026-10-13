package bot

import "adbot/internal/server"

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
		Reports:       sn.Reports,
		LocalFallback: sn.LocalFallback,
	}
}
