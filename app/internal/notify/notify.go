// Package notify renders and delivers the amend4 push notifications
// (disposition + operational errors → BOT_LOG_CHAT).
//
// Rendering contract (TASK_antispam_amend4.md §1): the disposition notice is
// the user-approved 4-line template (⏱️ / 💬 / 👤 / ✅) with anomaly lines
// appended only when they occur; error notices are 2–3 lines (event first,
// location last). Everything is plain text — no parse_mode is ever set.
//
// Delivery contract (§2): a bounded queue + a single worker goroutine so a
// slow or failing SendMessage can never block message handling or punishment;
// a token bucket caps BOT_NOTIFY_RATE messages/minute; identical errors are
// deduplicated per fingerprint inside BOT_NOTIFY_DEDUPE_WINDOW (first push +
// one end-of-window aggregate); queue overflow drops and is reported once per
// window; 429 backs off via retry_after (≤3 attempts); shutdown drains the
// queue for at most BOT_NOTIFY_DRAIN.
package notify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"adbot/internal/logx"
	"adbot/internal/telegram"
)

// telegramMaxMessageLen is Telegram's hard sendMessage length limit (4096).
const telegramMaxMessageLen = 4096

// Config carries the BOT_NOTIFY_* knobs (already defaulted by config.Load).
type Config struct {
	LogChat      int64         // 0 = everything disabled
	OnPunish     bool          // BOT_NOTIFY_ON_PUNISH
	OnError      bool          // BOT_NOTIFY_ON_ERROR
	Level        logx.Level    // BOT_NOTIFY_LEVEL threshold for error events
	RatePerMin   int           // BOT_NOTIFY_RATE (token bucket refill)
	QueueSize    int           // BOT_NOTIFY_QUEUE (bounded channel capacity)
	DedupeWindow time.Duration // BOT_NOTIFY_DEDUPE_WINDOW
	MaxLen       int           // BOT_NOTIFY_MAX_LEN (per-message text cap)
	TextLen      int           // BOT_NOTIFY_TEXT_LEN (evidence excerpt cap)
	IncludeText  bool          // BOT_NOTIFY_INCLUDE_TEXT
	Drain        time.Duration // BOT_NOTIFY_DRAIN (shutdown drain budget)
}

func (c Config) sanitize() Config {
	if c.RatePerMin <= 0 {
		c.RatePerMin = 20
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 200
	}
	if c.DedupeWindow <= 0 {
		c.DedupeWindow = 5 * time.Minute
	}
	if c.MaxLen <= 0 {
		c.MaxLen = 1024
	}
	if c.TextLen <= 0 {
		c.TextLen = 120
	}
	if c.Drain <= 0 {
		c.Drain = 5 * time.Second
	}
	return c
}

// Sender is the delivery surface (satisfied by the telegram client and fakes).
type Sender interface {
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
}

// Notifier is the async notification subsystem. Submit methods (Punish,
// Error, SetTargetOK) never block; a single worker delivers.
type Notifier struct {
	cfg     Config
	tg      Sender
	log     *logx.Logger
	q       chan string
	limiter *rateLimiter
	dedupe  *tracker

	sent          atomic.Int64 // successfully delivered messages
	dropped       atomic.Int64 // dropped on queue overflow
	suppressed    atomic.Int64 // deduped repeats (not pushed)
	reportedDrops atomic.Int64 // dropped count already reported

	targetOK  atomic.Bool
	targetSet atomic.Bool
	firstSend atomic.Bool

	cancel  context.CancelFunc
	done    chan struct{}
	started atomic.Bool
}

// New builds a Notifier. Call Start to launch the worker; Stop to drain.
func New(cfg Config, tg Sender, log *logx.Logger) *Notifier {
	cfg = cfg.sanitize()
	if log == nil {
		log = logx.New(nil, logx.Error, "text", "notify")
	}
	return &Notifier{
		cfg:     cfg,
		tg:      tg,
		log:     log,
		q:       make(chan string, cfg.QueueSize),
		limiter: &rateLimiter{rate: float64(cfg.RatePerMin) / 60.0},
		dedupe:  &tracker{window: cfg.DedupeWindow, entries: map[string]*trackEntry{}},
		done:    make(chan struct{}),
	}
}

// Enabled reports whether a target chat is configured.
func (n *Notifier) Enabled() bool { return n != nil && n.cfg.LogChat != 0 }

// Punish renders and queues a disposition notification (BOT_NOTIFY_ON_PUNISH).
func (n *Notifier) Punish(d PunishDetails) {
	if !n.Enabled() || !n.cfg.OnPunish {
		return
	}
	n.enqueue(RenderPunish(d, FormatOptions{
		IncludeText: n.cfg.IncludeText,
		MaxLen:      n.cfg.MaxLen,
	}))
}

// Error renders and queues an operational error notification, honoring the
// level threshold and the per-fingerprint dedupe window (repeats are counted
// and aggregated at window end, not pushed).
func (n *Notifier) Error(ev ErrorEvent) {
	if !n.Enabled() || !n.cfg.OnError {
		return
	}
	if ev.Level < n.cfg.Level {
		return
	}
	if !n.dedupe.record(ev.fingerprint(), ev.lastSeen()) {
		n.suppressed.Add(1)
		return
	}
	n.enqueue(RenderError(ev, FormatOptions{MaxLen: n.cfg.MaxLen}))
}

// SetTargetOK records the startup self-check outcome (§4).
func (n *Notifier) SetTargetOK(ok bool) {
	if n == nil {
		return
	}
	n.targetOK.Store(ok)
	n.targetSet.Store(true)
}

// TargetOK reports (ok, checked): checked=false means the self-check never
// ran (disabled target or getMe failure).
func (n *Notifier) TargetOK() (ok, checked bool) {
	if n == nil {
		return false, false
	}
	return n.targetOK.Load(), n.targetSet.Load()
}

// Stats is the /statusz snapshot.
type Stats struct {
	Enabled       bool
	TargetOK      bool
	TargetChecked bool
	Sent          int64
	Dropped       int64
	Suppressed    int64
	QueueLen      int
	QueueCap      int
}

// Snapshot copies the current counters.
func (n *Notifier) Snapshot() Stats {
	if n == nil {
		return Stats{}
	}
	ok, checked := n.TargetOK()
	return Stats{
		Enabled:       n.Enabled(),
		TargetOK:      ok,
		TargetChecked: checked,
		Sent:          n.sent.Load(),
		Dropped:       n.dropped.Load(),
		Suppressed:    n.suppressed.Load(),
		QueueLen:      len(n.q),
		QueueCap:      cap(n.q),
	}
}

// Start launches the worker goroutine; it stops when ctx is canceled or Stop
// is called. Starting an enabled-never or already-started notifier is a no-op.
func (n *Notifier) Start(ctx context.Context) {
	if !n.Enabled() || !n.started.CompareAndSwap(false, true) {
		return
	}
	n.cancel = func() {} // replaced below via the derived context
	ctx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	go n.run(ctx)
}

// Stop cancels the worker and waits for it to exit; the worker drains the
// remaining queue on the way out, bounded by BOT_NOTIFY_DRAIN.
func (n *Notifier) Stop() {
	if n == nil {
		return
	}
	if n.cancel != nil {
		n.cancel()
	}
	if n.started.Load() {
		<-n.done
	}
}

// enqueue is the single non-blocking admission point.
func (n *Notifier) enqueue(text string) {
	select {
	case n.q <- text:
	default:
		n.dropped.Add(1)
		n.log.Warn("notify_queue_overflow",
			"queue_cap", cap(n.q),
			"dropped_total", n.dropped.Load())
	}
}

func (n *Notifier) run(ctx context.Context) {
	defer close(n.done)
	window := time.NewTicker(n.cfg.DedupeWindow)
	defer window.Stop()
	for {
		select {
		case <-ctx.Done():
			n.drainRemaining()
			return
		case <-window.C:
			n.windowTick()
		case text := <-n.q:
			// Cancellation can land between the select and this branch;
			// delivering with a dead ctx makes the token bucket abort and the
			// message silently drop. Finish this item on a fresh deadline ctx
			// and hand the rest of the queue to the drain path.
			if ctx.Err() != nil {
				ctxD, cancel := context.WithDeadline(context.Background(), time.Now().Add(n.cfg.Drain))
				n.deliver(ctxD, text)
				cancel()
				n.drainRemaining()
				return
			}
			n.deliver(ctx, text)
		}
	}
}

// windowTick closes expired dedupe windows (pushing the "又发生 N 次"
// aggregate for repeated errors) and reports queue-overflow suppression once
// per window (§2.3).
func (n *Notifier) windowTick() {
	for _, agg := range n.dedupe.expire() {
		if agg.Count > 1 {
			text := fmt.Sprintf("同类错误在 %s内又发生 %d 次（最近：%s）",
				humanWindow(n.cfg.DedupeWindow), agg.Count-1, agg.Last)
			n.enqueue(clipHard(text, n.cfg.MaxLen))
		}
	}
	if d := n.dropped.Load() - n.reportedDrops.Load(); d > 0 {
		n.reportedDrops.Store(n.dropped.Load())
		text := fmt.Sprintf("🗑️ 通知队列溢出：%s内已抑制 %d 条通知",
			humanWindow(n.cfg.DedupeWindow), d)
		n.enqueue(clipHard(text, n.cfg.MaxLen))
	}
}

// drainRemaining sends what is left in the queue, bounded by the drain
// budget; anything past the deadline is abandoned (logged).
func (n *Notifier) drainRemaining() {
	deadline := time.Now().Add(n.cfg.Drain)
	for {
		select {
		case text := <-n.q:
			if time.Now().After(deadline) {
				n.log.Warn("notify_drain_deadline", "summary", "关闭前未发完的通知已放弃")
				return
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			n.deliver(ctx, text)
			cancel()
		default:
			return
		}
	}
}

// deliver sends one already-dequeued message. Cancellation can race with the
// token-bucket wait (Stop fires mid-delivery); a dequeued item is still owed,
// so the bucket is retried under the drain deadline before the message is
// abandoned. Only queue overflow (a different path) legitimately drops.
func (n *Notifier) deliver(ctx context.Context, text string) {
	text = clipHard(text, telegramMaxMessageLen)
	if err := n.limiter.wait(ctx); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			dctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(n.cfg.Drain))
			defer cancel()
			if err := n.limiter.wait(dctx); err == nil {
				n.doDeliver(dctx, text)
				return
			}
		}
		n.dropped.Add(1)
		n.log.Warn("notify_rate_aborted", "err", err.Error())
		return
	}
	n.doDeliver(ctx, text)
}

func (n *Notifier) doDeliver(ctx context.Context, text string) {
	if err := n.send(ctx, text); err != nil {
		n.log.Error("notify_send_error", "chat", n.cfg.LogChat, "err", err.Error())
		return
	}
	n.sent.Add(1)
	if n.firstSend.CompareAndSwap(false, true) {
		n.log.Info("notify_ok", "chat", n.cfg.LogChat)
	}
}

// send performs the SendMessage with 429 retry_after backoff (≤3 attempts);
// non-429 failures are terminal (logged by the caller).
func (n *Notifier) send(ctx context.Context, text string) error {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		ctxT, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := n.tg.SendMessage(ctxT, n.cfg.LogChat, text)
		cancel()
		if err == nil {
			n.targetOK.Store(true) // recover from a failed self-check
			n.targetSet.Store(true)
			return nil
		}
		lastErr = err
		var api *telegram.APIError
		if errors.As(err, &api) && api.Code == 429 {
			wait := time.Duration(api.RetryAfter) * time.Second
			if wait <= 0 || wait > 15*time.Second {
				wait = 15 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return err
	}
	return fmt.Errorf("sendMessage 429 backoff exhausted after %d attempts: %w", attempts, lastErr)
}

// ---------------------------------------------------------------------------
// Dedupe tracker (§2.3)
// ---------------------------------------------------------------------------

type trackEntry struct {
	expires time.Time
	count   int
	last    string
}

type tracker struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]*trackEntry
}

// record returns true when fp is the first occurrence inside its window (the
// caller pushes); repeats just bump the count.
func (t *tracker) record(fp, last string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[fp]; ok {
		if now.Before(e.expires) {
			e.count++
			e.last = last
			return false
		}
		delete(t.entries, fp) // stale window: start a fresh one
	}
	t.entries[fp] = &trackEntry{expires: now.Add(t.window), count: 1, last: last}
	return true
}

type aggregate struct {
	Fingerprint string
	Count       int
	Last        string
}

// expire removes entries whose window has closed and returns the ones that
// repeated (count > 1) for end-of-window summaries.
func (t *tracker) expire() []aggregate {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []aggregate
	for fp, e := range t.entries {
		if !now.Before(e.expires) {
			if e.count > 1 {
				out = append(out, aggregate{Fingerprint: fp, Count: e.count, Last: e.last})
			}
			delete(t.entries, fp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

// ---------------------------------------------------------------------------
// Token bucket (§2.2)
// ---------------------------------------------------------------------------

type rateLimiter struct {
	mu     sync.Mutex
	rate   float64   // tokens per second
	tokens float64   // current tokens (≤ 1)
	last   time.Time // last refill time
}

// wait blocks until one token is available or ctx is done.
func (r *rateLimiter) wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()
		if r.last.IsZero() {
			r.last = now
		}
		r.tokens += now.Sub(r.last).Seconds() * r.rate
		if r.tokens > 1 {
			r.tokens = 1
		}
		r.last = now
		if r.tokens >= 1 {
			r.tokens -= 1
			r.mu.Unlock()
			return nil
		}
		need := (1 - r.tokens) / r.rate
		r.mu.Unlock()
		timer := time.NewTimer(time.Duration(need * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// humanWindow renders a duration the way the templates say it ("5 分钟").
func humanWindow(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + " 分钟"
	case d >= time.Second:
		return strconv.Itoa(int(d.Seconds())) + " 秒"
	default:
		return strconv.Itoa(int(d.Milliseconds())) + "ms"
	}
}

// clipHard truncates to at most max runes (Telegram 4096 / BOT_NOTIFY_MAX_LEN).
func clipHard(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// excerpt truncates to max runes and marks the cut with an ellipsis.
func excerpt(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
