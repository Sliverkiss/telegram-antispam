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

// JudgePoolView adapts the judge-pool gauges into the server's plain view
// (amend6 A.4). BOT_UPDATE_WORKERS rides along so /statusz shows the library
// worker count next to the pool gauges.
func (b *Bot) JudgePoolView() server.JudgePoolView {
	async, conc, queued, inflight, cap, enq, full := b.JudgePoolStats()
	return server.JudgePoolView{
		Async:          async,
		Concurrency:    conc,
		Queued:         queued,
		Inflight:       inflight,
		QueueCap:       cap,
		EnqueuedTotal:  enq,
		QueueFullTotal: full,
		UpdateWorkers:  b.Cfg.UpdateWorkers,
	}
}

// AnnounceView adapts the amend7 announcement counters into the server's
// plain view.
func (b *Bot) AnnounceView() server.AnnounceView {
	enabled, sent, fallback, errs := b.AnnounceStats()
	return server.AnnounceView{
		Enabled:       enabled,
		SentTotal:     sent,
		FallbackTotal: fallback,
		ErrorTotal:    errs,
	}
}

// MsgTTLView adapts the amend8 chat-message-TTL gauges into the server's
// plain view.
func (b *Bot) MsgTTLView() server.MsgTTLView {
	scheduled, deleted, errs := b.MsgTTLStats()
	return server.MsgTTLView{
		TTL:            b.MsgTTLConfig().String(),
		ScheduledTotal: scheduled,
		DeletedTotal:   deleted,
		ErrorTotal:     errs,
	}
}

// JoinRuleView adapts the amend9 裸号必杀 gauges into the server's plain
// view: (BOT_JOIN_NAKED_PREMIUM, join_rule_hits_total).
func (b *Bot) JoinRuleView() server.JoinRuleView {
	enabled, hits := b.JoinRuleStats()
	return server.JoinRuleView{Enabled: enabled, Hits: hits}
}

// JoinVerifyView adapts the amend10/amend11 join-verification gauges into
// the server's plain view: (BOT_JOIN_VERIFY, join_verify_pending,
// join_verify_passed_total, join_verify_timeout_total,
// join_verify_skipped_total).
func (b *Bot) JoinVerifyView() server.JoinVerifyView {
	enabled, pending, passed, timeouts := b.JoinVerifyStats()
	return server.JoinVerifyView{
		Enabled:      enabled,
		Pending:      pending,
		PassedTotal:  passed,
		TimeoutTotal: timeouts,
		SkippedTotal: b.JoinVerifySkippedTotal(),
	}
}

// CommentBioView adapts the amend12 comment-bio gauges into the server's
// plain view: (BOT_COMMENT_BIO_SCREEN, comment_bio_triggered_total,
// comment_bio_flash_hits_total, comment_bio_flash_cached,
// comment_bio_flash_ttl).
func (b *Bot) CommentBioView() server.CommentBioView {
	enabled, triggered, hits, cached := b.CommentBioStats()
	return server.CommentBioView{
		Enabled:          enabled,
		TriggeredTotal:   triggered,
		FlashHitsTotal:   hits,
		FlashCachedTotal: cached,
		FlashTTL:         b.CommentBioFlashConfig().String(),
	}
}
