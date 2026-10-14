// The amend7 in-group "send-off announcement" (送客公告) subsystem.
//
// After a SUCCESSFUL kick/ban disposal, the bot posts one 1–2 sentence cute
// AI-generated notice into the SAME group saying who was shown the door.
// Contract (TASK_antispam_amend7.md C+D, amend8 merge):
//   - The announcement NEVER occupies judgment pool slots and never runs
//     inline on the punishment path: each disposal spawns its own goroutine
//     (amendAnnouncer), which does the AI call → send on its own schedule.
//     The punishment chain measures its own latency — tests assert < 100ms —
//     and is unaffected by a slow/hanging AI.
//   - The send goes through the amend8 unified TTL sender (BOT_MSG_TTL): the
//     announcement self-deletes like every other bot chat message. The
//     amend7-private BOT_ANNOUNCE_TTL knob is gone.
//   - AI failure / timeout / non-compliant output → the local template pool
//     (never a drop, never a retry storm).
//   - dry-run sends NOTHING (announce_dry_run log only), matching every
//     other write action.
//   - Idempotency: one disposal (chat+uid+messageID) announces at most once;
//     the dedupe map is a bounded TTL cache (default 1h, pruned on insert).
//   - Send failure → announce_error log + the existing notifier error push;
//     no re-send loop.
//   - Shutdown: StopAnnouncer drains up to BOT_JUDGE_DRAIN's budget (the
//     "with the BOT_JUDGE_DRAIN magnitude" clause of the task). The TTL
//     deletion goroutines belong to the msgTTL subsystem and drain with it.
package bot

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"adbot/internal/ai"
	"adbot/internal/logx"
	"adbot/internal/notify"
)

// Announcer generates and posts kick announcements. A nil *Announcer (or
// one with announce nil) is a no-op — tests and dry-run rely on that.
type Announcer struct {
	tg      TelegramClient
	ann     AnnounceGenerator
	cfg     announceConfig
	log     *logx.Logger
	notify  *notify.Notifier
	dedupe  *dedupeCache
	timeout time.Duration
	// sender is the amend8 unified TTL path: send + delayed self-delete
	// (BOT_MSG_TTL). nil → plain SendMessage (tests / disabled TTL).
	sender  announceSender
	enabled bool

	wg      sync.WaitGroup
	stopped chan struct{}
	once    sync.Once
	drainV  time.Duration

	// stats (atomic, /statusz)
	sentTotal     atomic.Int64
	fallbackTotal atomic.Int64
	errorTotal    atomic.Int64
}

// AnnounceGenerator is the AI surface (ai.Client in production, a fake in
// tests). error → template fallback.
type AnnounceGenerator interface {
	Announce(ctx context.Context, subject, category string) (string, error)
}

// announceSender is the amend8 unified send surface the announcement uses.
type announceSender interface {
	SendChat(ctx context.Context, chatID int64, text string) (int64, error)
}

// announceConfig carries the BOT_ANNOUNCE_* knobs.
type announceConfig struct {
	kick bool // BOT_ANNOUNCE_KICK
}

// announceTemplates is the local fallback pool (C.3): ≥ 8 one-liners, name
// only, no ad content. Selected at random to avoid monotony.
var announceTemplates = []string{
	"🚪 %s 已请出群聊，广告请去别处发～",
	"👋 %s 发广告被送出群，慢走不送",
	"🧹 %s 已被清出群，群风清朗靠大家",
	"✨ %s 带着广告悄悄溜走，被我们请了出去",
	"🛒 %s 的广告牌被没收，人也被请出了群",
	"🪄 一挥魔杖，%s 和 TA 的广告一起消失了～",
	"🥾 %s 被请出群聊，下次记得带正经话来",
	"🧼 群里刚做了一次大扫除，%s 被打扫出门啦",
	"📦 %s 已被贴上退货标签，送出本群",
}

// announceSubject renders the display name for the kicked user (username
// preferred, first name fallback, uid last resort).
func announceSubject(username, firstName string, uid int64) string {
	if s := strings.TrimSpace(username); s != "" {
		return s
	}
	if s := strings.TrimSpace(firstName); s != "" {
		return s
	}
	return strconv.FormatInt(uid, 10)
}

// renderTemplate picks a random fallback template and fills the name.
func renderTemplate(subject string) string {
	tpl := announceTemplates[rand.Intn(len(announceTemplates))]
	return fmt.Sprintf(tpl, subject)
}

// NewAnnouncer builds the subsystem; nil tg / nil generator → disabled.
// sender is the amend8 unified TTL path (bot.sendChat) — nil keeps the plain
// SendMessage behavior (legacy tests); the ttl parameter of amend7 is gone
// (BOT_MSG_TTL owns the deletion now).
func NewAnnouncer(tg TelegramClient, gen AnnounceGenerator, kickEnabled bool, timeout, drain time.Duration, log *logx.Logger, n *notify.Notifier, sender announceSender) *Announcer {
	if log == nil {
		log = logx.New(nil, logx.Error, "text", "announce")
	}
	if timeout <= 0 {
		timeout = ai.AnnounceTimeoutDefault
	}
	if drain <= 0 {
		drain = 10 * time.Second
	}
	return &Announcer{
		tg:      tg,
		ann:     gen,
		cfg:     announceConfig{kick: kickEnabled},
		log:     log,
		notify:  n,
		dedupe:  newDedupeCache(time.Hour),
		timeout: timeout,
		sender:  sender,
		drainV:  drain,
		enabled: tg != nil && gen != nil && kickEnabled,
		stopped: make(chan struct{}),
	}
}

// Enabled reports whether announcements will be attempted.
func (a *Announcer) Enabled() bool { return a != nil && a.enabled }

// Stats returns the /statusz counters.
func (a *Announcer) Stats() (enabled bool, sent, fallback, errs int64) {
	if a == nil {
		return false, 0, 0, 0
	}
	return a.enabled, a.sentTotal.Load(), a.fallbackTotal.Load(), a.errorTotal.Load()
}

// Stop drains in-flight announcements up to the drain budget.
func (a *Announcer) Stop() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		close(a.stopped)
		done := make(chan struct{})
		go func() {
			a.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(a.drainV):
		}
	})
}

// drainBudget mirrors the Announcer's graceful-shutdown budget.
func (a *Announcer) drain() time.Duration { return a.drainV }

// Announce is the entry point called after a successful disposal: idempotent
// (chat+uid+messageID), asynchronous (own goroutine), never blocking the
// caller. dryRun short-circuits with a log line, exactly like the other
// write actions.
func (a *Announcer) Announce(ctx context.Context, chatID, uid, messageID int64, username, firstName, category string, dryRun bool) {
	if a == nil || !a.enabled {
		return
	}
	if dryRun {
		a.log.Info("announce_dry_run",
			"chat", chatID, "uid", uid, "message_id", messageID,
			"summary", "干跑：公告只打日志不发送")
		return
	}
	if !a.dedupe.add(chatID, uid, messageID) {
		a.log.Debug("announce_dedupe",
			"chat", chatID, "uid", uid, "message_id", messageID,
			"summary", "同一处置只发一条公告")
		return
	}
	// Detached context: the announcement must outlive the request/update
	// handler that triggered the disposal (cancel → abort, never hang).
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.run(ctx, chatID, uid, username, firstName, category)
	}()
}

// run performs generate → send on the announcement goroutine. Every failure
// path is terminal (no retry storm). The send goes through the amend8
// unified TTL path when wired: the delayed self-delete (BOT_MSG_TTL) is the
// msgTTL subsystem's business, not this one's.
func (a *Announcer) run(ctx context.Context, chatID, uid int64, username, firstName, category string) {
	_ = ctx
	subject := announceSubject(username, firstName, uid)
	ctxGen, cancel := context.WithTimeout(context.Background(), a.timeout+2*time.Second)
	defer cancel()

	text, source := "", "template"
	start := time.Now()
	gen, genErr := a.ann.Announce(ctxGen, subject, category)
	if genErr == nil {
		// Same hard post-processing/compliance pipeline as the ai package
		// (F.3/F.4/F.5 all apply even when the fake generator bypasses the
		// real Client): a violation here falls to the template pool.
		cleaned, err := ai.CleanAnnounce(gen, subject)
		if err != nil {
			genErr = err
		} else {
			text, source = cleaned, "ai"
		}
	}
	if genErr != nil {
		a.fallbackTotal.Add(1)
		a.log.Debug("announce_ai_fallback", "uid", uid, "err", genErr.Error())
		text = renderTemplate(subject)
	}

	send := func(ctx context.Context, chatID int64, text string) (int64, error) {
		return a.tg.SendMessage(ctx, chatID, text)
	}
	if a.sender != nil {
		send = a.sender.SendChat
	}
	msgID, err := send(context.Background(), chatID, text)
	if err != nil {
		a.errorTotal.Add(1)
		a.log.Error("announce_error",
			"chat", chatID, "uid", uid, "err", err.Error())
		if a.notify != nil {
			a.notify.Error(notify.ErrorEvent{
				Kind:      "announce",
				Level:     logx.Error,
				ChatID:    chatID,
				UID:       uid,
				MessageID: msgID,
				Method:    "sendMessage",
				Detail:    err.Error(),
				LastSeen:  "announce sendMessage " + err.Error(),
			})
		}
		return
	}
	a.sentTotal.Add(1)
	a.log.Info("announce_generated",
		"ms", time.Since(start).Milliseconds(),
		"source", source)
	a.log.Info("announce_sent", "chat", chatID, "uid", uid)
}

// dedupeCache is a bounded TTL map for announcement idempotency: one
// disposal (chat+uid+messageID) → one announcement. Entries expire so a
// re-join + re-dispose of the same uid can legitimately re-announce.
type dedupeCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
}

func newDedupeCache(ttl time.Duration) *dedupeCache {
	return &dedupeCache{entries: map[string]time.Time{}, ttl: ttl}
}

func (d *dedupeCache) add(chatID, uid, messageID int64) bool {
	key := fmt.Sprintf("%d:%d:%d", chatID, uid, messageID)
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	// prune expired entries (bounded: the map holds at most one entry per
	// recent disposal; prune-on-insert keeps it flat without a janitor).
	if len(d.entries) > 4096 {
		for k, ts := range d.entries {
			if now.Sub(ts) > d.ttl {
				delete(d.entries, k)
			}
		}
	}
	if ts, ok := d.entries[key]; ok && now.Sub(ts) < d.ttl {
		return false // duplicate disposal
	}
	d.entries[key] = now
	return true
}
