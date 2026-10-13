package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/telegram"
)

// fakeSender records delivered messages and can script failures (a non-zero
// status surfaces as a *telegram.APIError; 429 carries retry_after). The
// worker is the sole caller of SendMessage, so blockOnce/blocked need no lock.
type fakeSender struct {
	mu        sync.Mutex
	sends     []string
	status    int           // APIError code when != 0
	retries   int           // 429 retry_after seconds
	failN     int           // fail this many times, then succeed
	blockOnce chan struct{} // if set, first send blocks until the chan closes
	blocked   bool
}

func (f *fakeSender) SendMessage(_ context.Context, _ int64, text string) (int64, error) {
	if f.blockOnce != nil && !f.blocked {
		f.blocked = true
		<-f.blockOnce
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		return 0, &telegram.APIError{Code: f.status, RetryAfter: f.retries, Description: "boom"}
	}
	f.sends = append(f.sends, text)
	return int64(len(f.sends)), nil
}

func (f *fakeSender) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...)
}

func testCfg() Config {
	return Config{
		LogChat:      -10003723027717,
		OnPunish:     true,
		OnError:      true,
		Level:        logx.Error,
		RatePerMin:   100000, // effectively unlimited in most tests
		QueueSize:    200,
		DedupeWindow: 80 * time.Millisecond,
		MaxLen:       1024,
		TextLen:      120,
		IncludeText:  true,
		Drain:        500 * time.Millisecond,
	}
}

// newNotifier builds and starts a notifier; the caller owns Stop.
func newNotifier(cfg Config, tg Sender) *Notifier {
	n := New(cfg, tg, logx.New(nil, logx.Error, "text", "notify"))
	n.Start(context.Background())
	return n
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- §1.1 rendering: user-approved template, exact ----------------------------

func TestRenderPunishTemplateBanAI(t *testing.T) {
	d := PunishDetails{
		ChatID: -100, UID: 9, Username: "aMPMvzapuDpedgzadP",
		Text: "微信收钱 一天6k", Action: "ban", Category: "诈骗",
		Source: "ai", Score: ConfidenceScore(0.98), Elapsed: 1480 * time.Millisecond,
	}
	got := RenderPunish(d, FormatOptions{IncludeText: true, MaxLen: 1024})
	want := "⏱️ 处理时间: 1.48 s\n" +
		"💬 微信收钱 一天6k\n" +
		"👤 @aMPMvzapuDpedgzadP\n" +
		"✅ 已封禁 · 98分"
	if got != want {
		t.Fatalf("render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderPunishKick(t *testing.T) {
	d := PunishDetails{Username: "u", Text: "hi", Action: "kick", Category: "无",
		Source: "ai", Score: 90, Elapsed: 0}
	got := RenderPunish(d, FormatOptions{IncludeText: true})
	if !strings.HasPrefix(got, "⏱️ 处理时间: 0.00 s\n💬 hi\n👤 @u\n✅ 已踢出 · 90分") {
		t.Fatalf("kick render = %q", got)
	}
}

func TestUserLabelPrecedence(t *testing.T) {
	if got := UserLabel(PunishDetails{Username: " u "}); got != "@u" {
		t.Errorf("username = %q", got)
	}
	if got := UserLabel(PunishDetails{FirstName: "白白"}); got != "白白" {
		t.Errorf("first name = %q", got)
	}
	if got := UserLabel(PunishDetails{UID: 42}); got != "无用户名 · 42" {
		t.Errorf("uid = %q", got)
	}
}

func TestConfidenceScore(t *testing.T) {
	cases := map[float64]int{0.5: 50, 0.98: 98, 0.985: 99, 0.995: 100, 1.0: 100, 0: 0, -0.5: 0}
	for in, want := range cases {
		if got := ConfidenceScore(in); got != want {
			t.Errorf("ConfidenceScore(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestRenderPunishSuffixes(t *testing.T) {
	base := PunishDetails{Username: "u", Action: "ban", Category: "无", Text: "t"}
	sig := RenderPunish(func() PunishDetails {
		d := base
		d.Source, d.Score = "signature", 100
		return d
	}(), FormatOptions{})
	if !strings.HasSuffix(sig, "✅ 已封禁 · 100分 · 签名命中") {
		t.Errorf("signature suffix: %q", sig)
	}
	vote := RenderPunish(func() PunishDetails {
		d := base
		d.Source, d.Score, d.VoteFor, d.VoteTotal = "vote", 97, 3, 3
		return d
	}(), FormatOptions{})
	if !strings.HasSuffix(vote, "✅ 已封禁 · 97分 · 投票 3/3") {
		t.Errorf("vote suffix: %q", vote)
	}
	local := RenderPunish(func() PunishDetails {
		d := base
		d.Source, d.Score = "local", 4
		return d
	}(), FormatOptions{})
	if !strings.Contains(local, "🧩 已封禁 · 兜底 4分") {
		t.Errorf("local line: %q", local)
	}
	high := RenderPunish(func() PunishDetails {
		d := base
		d.Source, d.Score, d.Category = "ai", 99, "儿童性内容"
		return d
	}(), FormatOptions{})
	if !strings.HasSuffix(high, "· 🚨 儿童性内容") {
		t.Errorf("high-risk suffix: %q", high)
	}
}

func TestRenderPunishAnomalies(t *testing.T) {
	d := PunishDetails{Username: "u", Action: "ban", Category: "无",
		Source: "ai", Score: 90, Retries: 3, DeleteFailed: true, Text: "t"}
	got := RenderPunish(d, FormatOptions{})
	if !strings.Contains(got, "\n⚠️ 消息删除失败 · 超 48 小时") {
		t.Errorf("delete-failed line missing: %q", got)
	}
	if !strings.Contains(got, "\n🔁 重试 3 次") {
		t.Errorf("retry line missing: %q", got)
	}
	// Clean case: no anomaly lines.
	clean := RenderPunish(func() PunishDetails {
		c := d
		c.Retries, c.DeleteFailed = 1, false
		return c
	}(), FormatOptions{})
	if strings.Contains(clean, "\n⚠️") || strings.Contains(clean, "\n🔁") {
		t.Errorf("clean render has anomaly lines: %q", clean)
	}
}

func TestRenderPunishNoText(t *testing.T) {
	d := PunishDetails{Username: "u", Action: "ban", Category: "无",
		Source: "ai", Score: 90, Text: "trigger"}
	got := RenderPunish(d, FormatOptions{IncludeText: false})
	if strings.Contains(got, "💬") {
		t.Errorf("include_text=false still renders the evidence line: %q", got)
	}
	if !strings.HasPrefix(got, "⏱️") {
		t.Errorf("first line lost: %q", got)
	}
}

func TestRenderPunishTextExcerpt(t *testing.T) {
	long := strings.Repeat("字", 90)
	d := PunishDetails{Username: "u", Action: "ban", Category: "无",
		Source: "ai", Score: 90, Text: long}
	got := RenderPunish(d, FormatOptions{IncludeText: true})
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "💬 ") {
			body := strings.TrimPrefix(l, "💬 ")
			if len([]rune(body)) != 81 || !strings.HasSuffix(body, "…") {
				t.Errorf("excerpt = rune-len %d, want 81 with …. %q", len([]rune(body)), body)
			}
			return
		}
	}
	t.Fatal("no 💬 line")
}

// --- §1.2 error rendering -----------------------------------------------------

func TestRenderErrorTelegramWrite(t *testing.T) {
	e := ErrorEvent{
		Kind: "telegram_write", Level: logx.Error, ChatID: -1003888828164,
		MessageID: 12345, Method: "deleteMessage", Detail: "400 message can't be deleted",
	}
	got := RenderError(e, FormatOptions{MaxLen: 1024})
	want := "📵 Telegram 写入失败\ndeleteMessage 400 message can't be deleted\n📍 -1003888828164 · msg 12345"
	if got != want {
		t.Fatalf("render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderErrorAIChain(t *testing.T) {
	ev := ErrorEvent{
		Kind: "ai_chain", Level: logx.Error, ChatID: -1003888828164, MessageID: 12346,
		Attempts: 5, Method: "🔁 已重试 5 次 · 52s · 转本地兜底", Detail: "pool_parked",
	}
	got := RenderError(ev, FormatOptions{MaxLen: 1024})
	if got != "⚠️ AI 连续 5 次失败 · 🧊 pool_parked\n🔁 已重试 5 次 · 52s · 转本地兜底\n📍 -1003888828164 · msg 12346" {
		t.Fatalf("ai chain render: %q", got)
	}
}

func TestRenderErrorPool(t *testing.T) {
	for _, tc := range []struct{ code, emoji string }{
		{"pool_parked", "🧊"}, {"no_accounts", "🕳️"}, {"upstream_busy", "🚦"},
	} {
		ev := ErrorEvent{Kind: "pool", Level: logx.Error, Method: "繁忙", Detail: tc.code}
		got := RenderError(ev, FormatOptions{MaxLen: 1024})
		if !strings.Contains(got, tc.emoji) {
			t.Errorf("%s: missing %s in %q", tc.code, tc.emoji, got)
		}
	}
}

func TestRenderErrorQueueOverflow(t *testing.T) {
	got := RenderError(ErrorEvent{Kind: "queue_overflow", Level: logx.Error,
		Method: "notify queue", Detail: "cap 200"}, FormatOptions{MaxLen: 1024})
	if !strings.Contains(got, "🗑️ 通知队列溢出") {
		t.Errorf("overflow headline: %q", got)
	}
}

func TestRenderErrorJanitorEvict(t *testing.T) {
	got := RenderError(ErrorEvent{Kind: "janitor_evict", Level: logx.Warn,
		Method: "观察名单 3 · 签名库 2", Detail: "容量上限淘汰 5 条"}, FormatOptions{MaxLen: 1024})
	if !strings.Contains(got, "📦 容量淘汰触发上限") {
		t.Errorf("evict headline: %q", got)
	}
}

func TestRenderErrorSelftest(t *testing.T) {
	got := RenderError(ErrorEvent{Kind: "selftest", Level: logx.Warn,
		Method: "getChatMember", Detail: "network down"}, FormatOptions{MaxLen: 1024})
	if !strings.Contains(got, "⚠️ 启动自检失败 · network down") {
		t.Errorf("selftest headline: %q", got)
	}
}

// --- fingerprint: event type + chat/uid + error code --------------------------

func TestFingerprintUniqueness(t *testing.T) {
	base := ErrorEvent{Kind: "telegram_write", ChatID: -1, UID: 3, Method: "deleteMessage", Detail: "401"}
	diff := base
	diff.UID = 4
	if base.fingerprint() == diff.fingerprint() {
		t.Error("different uid must produce different fingerprints")
	}
	diff2 := base
	diff2.Kind = "banChatMember"
	if base.fingerprint() == diff2.fingerprint() {
		t.Error("different kind must produce different fingerprints")
	}
}

// --- dedupe: same error 10× → 1 push + 1 aggregate -----------------------------

func TestErrorDedupeAggregate(t *testing.T) {
	tg := &fakeSender{}
	n := newNotifier(testCfg(), tg)
	ev := ErrorEvent{
		Kind: "telegram_write", Level: logx.Error, ChatID: -1, MessageID: 7,
		Method: "deleteMessage", Detail: "400 can't delete",
	}
	for i := 0; i < 10; i++ {
		n.Error(ev)
	}
	waitFor(t, func() bool { return len(tg.texts()) == 1 }, "first push")
	if len(tg.texts()) != 1 {
		t.Fatalf("dedupe window must push only the first: %v", tg.texts())
	}
	// Window closes (80ms) → exactly one aggregate, counting the 9 repeats.
	waitFor(t, func() bool { return len(tg.texts()) == 2 }, "aggregate push")
	texts := tg.texts()
	if !strings.Contains(texts[1], "同类错误在") || !strings.Contains(texts[1], "又发生 9 次") {
		t.Errorf("aggregate line = %q", texts[1])
	}
	n.Stop()
}

// --- rate limit: bucket caps push rate, queued not dropped ---------------------

func TestRateLimit(t *testing.T) {
	tg := &fakeSender{}
	cfg := testCfg()
	cfg.RatePerMin = 1000 // ~16.7 tokens/s → ~60ms spacing
	cfg.DedupeWindow = time.Hour
	cfg.Drain = 3 * time.Second
	n := newNotifier(cfg, tg)
	for i := 0; i < 10; i++ {
		n.Error(ErrorEvent{Kind: "jed", Level: logx.Error, ChatID: int64(i),
			Method: "x", Detail: "y"}) // unique chat → unique fingerprint
	}
	// The bucket paces delivery but must not drop any.
	waitFor(t, func() bool { return len(tg.texts()) == 10 }, "10 deliveries")
	if len(tg.texts()) != 10 {
		t.Fatalf("rate limit must queue, not drop: %d", len(tg.texts()))
	}
	n.Stop()
}

// --- queue overflow: drop + periodic suppression report ------------------------

func TestQueueOverflowSuppressionReport(t *testing.T) {
	tg := &fakeSender{blockOnce: make(chan struct{})}
	cfg := testCfg()
	cfg.RatePerMin = 10000000 // no pacing
	cfg.QueueSize = 2
	cfg.DedupeWindow = 60 * time.Millisecond
	cfg.Drain = 5 * time.Millisecond
	n := newNotifier(cfg, tg)
	// The worker is parked on the very first SendMessage (blockOnce), so the
	// queue is guaranteed to saturate and the rest are dropped.
	for i := 0; i < 500; i++ {
		n.Punish(PunishDetails{
			Username: "u", Text: "t", Action: "ban", Category: "无",
			Source: "ai", Score: i % 100,
		})
	}
	if n.dropped.Load() == 0 {
		t.Fatal("queue must have dropped notifications")
	}
	// Close the block so the worker can drain; the window tick emits the
	// suppression report.
	close(tg.blockOnce)
	waitFor(t, func() bool {
		for _, s := range tg.texts() {
			if strings.Contains(s, "通知队列溢出") {
				return true
			}
		}
		return false
	}, "suppression report push")
	n.Stop()
}

// --- 429 retry with retry_after backoff -----------------------------------------

func TestSend429Retries(t *testing.T) {
	tg := &fakeSender{status: 429, retries: 1, failN: 1}
	cfg := testCfg()
	n := newNotifier(cfg, tg)
	start := time.Now()
	n.Punish(PunishDetails{Username: "u", Text: "t", Action: "ban", Category: "无", Source: "ai", Score: 90})
	waitFor(t, func() bool { return len(tg.texts()) == 1 }, "429 retry recovers")
	if time.Since(start) > 4*time.Second {
		t.Fatalf("429 retry budget too slow: %s", time.Since(start))
	}
	n.Stop()
}

// --- SendMessage 500 failure must not break the notifier ------------------------

func TestSend500DoesNotBreakWorker(t *testing.T) {
	tg := &fakeSender{status: 500, failN: 1} // first delivery fails, second succeeds
	cfg := testCfg()
	n := newNotifier(cfg, tg)
	// These are enqueued before the worker's first send; the 500 failure is
	// terminal for that message (logged) but the worker keeps draining.
	for i := 0; i < 3; i++ {
		n.Punish(PunishDetails{Username: "u", Text: "t", Action: "ban", Category: "无",
			Source: "ai", Score: 90 + i})
	}
	waitFor(t, func() bool { return len(tg.texts()) == 2 }, "worker survives send failure")
	if len(tg.texts()) != 2 {
		t.Fatalf("want 2 survives, got %v", tg.texts())
	}
	n.Stop()
}

// --- 4096 hard limit at delivery time -------------------------------------------

func TestHardClip4096(t *testing.T) {
	tg := &fakeSender{}
	cfg := testCfg()
	cfg.MaxLen = 1024 * 1024 // render cap off; only the 4096 floor applies
	n := newNotifier(cfg, tg)
	// RenderError passes Detail through unclipped — a >4096 detail line.
	n.Error(ErrorEvent{Kind: "telegram_write", Level: logx.Error, ChatID: -1,
		Method: "deleteMessage", Detail: strings.Repeat("A", 5000)})
	waitFor(t, func() bool { return len(tg.texts()) == 1 }, "delivery")
	if n := len([]rune(tg.texts()[0])); n > 4096 {
		t.Fatalf("delivered %d runes, hard limit is 4096", n)
	}
	n.Stop()
}

func TestClipHard(t *testing.T) {
	if got := clipHard("", 10); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got := clipHard("ok", 10); got != "ok" {
		t.Errorf("short = %q", got)
	}
	if got := clipHard(strings.Repeat("A", 5000), 4096); len([]rune(got)) != 4096 {
		t.Errorf("clipped len = %d", len([]rune(got)))
	}
	if got := excerpt(strings.Repeat("字", 10), 5); got != strings.Repeat("字", 5)+"…" {
		t.Errorf("excerpt = %q", got)
	}
}

// --- BOT_LOG_CHAT=0: zero calls --------------------------------------------------

func TestDisabledNoCalls(t *testing.T) {
	tg := &fakeSender{}
	cfg := testCfg()
	cfg.LogChat = 0 // notifier disabled
	n := New(cfg, tg, logx.New(nil, logx.Error, "text", "notify"))
	if n.Enabled() {
		t.Fatal("LogChat=0 must disable the notifier")
	}
	n.Error(ErrorEvent{Kind: "telegram_write", Level: logx.Error, Method: "x", Detail: "y"})
	n.Punish(PunishDetails{Username: "u", Text: "t", Action: "ban", Category: "无", Source: "ai", Score: 90})
	n.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	n.Stop()
	if len(tg.texts()) != 0 {
		t.Fatalf("LogChat=0 must never send: %v", tg.texts())
	}
}

// --- BOT_NOTIFY_ON_PUNISH=false: disposition is not pushed -----------------------

func TestOnPunishFalseNoPush(t *testing.T) {
	tg := &fakeSender{}
	cfg := testCfg()
	cfg.OnPunish = false
	n := newNotifier(cfg, tg)
	n.Punish(PunishDetails{Username: "u", Text: "t", Action: "ban", Category: "无", Source: "ai", Score: 90})
	time.Sleep(30 * time.Millisecond)
	n.Stop()
	if len(tg.texts()) != 0 {
		t.Fatalf("BOT_NOTIFY_ON_PUNISH=false must not push: %v", tg.texts())
	}
}

// --- level threshold -------------------------------------------------------------

func TestLevelThreshold(t *testing.T) {
	cfg := testCfg()
	cfg.Level = logx.Error
	tg := &fakeSender{}
	n := newNotifier(cfg, tg)
	n.Error(ErrorEvent{Kind: "janitor_evict", Level: logx.Warn, Method: "x", Detail: "y"})
	n.Error(ErrorEvent{Kind: "telegram_write", Level: logx.Error, Method: "x", Detail: "y"})
	waitFor(t, func() bool { return len(tg.texts()) == 1 }, "only error-level delivered")
	if len(tg.texts()) != 1 {
		t.Fatalf("level gate = %d texts\n%v", len(tg.texts()), tg.texts())
	}
	if !strings.Contains(tg.texts()[0], "📵") {
		t.Errorf("warn below threshold leaked through: %q", tg.texts()[0])
	}
	n.Stop()
}

// --- graceful drain ---------------------------------------------------------------

// TestStopDrains: the worker holds the first delivery hostage (blockOnce),
// both messages are enqueued, then Stop is called — the drain must flush the
// second message after the block lifts. Deterministic: no race between the
// enqueue and the cancel.
func TestStopDrains(t *testing.T) {
	tg := &fakeSender{blockOnce: make(chan struct{})}
	cfg := testCfg()
	cfg.Drain = 600 * time.Millisecond
	n := newNotifier(cfg, tg)
	n.Punish(PunishDetails{Username: "u", Text: "one", Action: "ban", Category: "无", Source: "ai", Score: 90})
	n.Punish(PunishDetails{Username: "u2", Text: "two", Action: "ban", Category: "无", Source: "ai", Score: 90})
	// Both are now in the queue; the worker is parked in the first send.
	go func() { time.Sleep(20 * time.Millisecond); close(tg.blockOnce) }()
	start := time.Now()
	n.Stop() // cancel → drainRemaining flushes what the queue holds
	if len(tg.texts()) != 2 {
		t.Fatalf("drain must flush the queue, got %d:\n%v", len(tg.texts()), tg.texts())
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("drain should actually send the rest, completed too fast: %s", time.Since(start))
	}
}

// --- rate limiter unit -------------------------------------------------------------

func TestRateLimiterWait(t *testing.T) {
	r := &rateLimiter{rate: 10} // 10 tokens/s → 100ms per token
	ctx := context.Background()
	start := time.Now()
	// Fresh bucket: first token also waits ~100ms (fills from 0 to 1).
	if err := r.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.wait(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("two tokens at 10/s should take ~200ms, took %s", elapsed)
	}
}

// --- human window formatting -------------------------------------------------------

func TestHumanWindow(t *testing.T) {
	if got := humanWindow(5 * time.Minute); got != "5 分钟" {
		t.Errorf("5m = %q", got)
	}
	if got := humanWindow(30 * time.Second); got != "30 秒" {
		t.Errorf("30s = %q", got)
	}
	if got := humanWindow(80 * time.Millisecond); got != "80ms" {
		t.Errorf("80ms = %q", got)
	}
}

// --- fakeSender is safe concurrently -------------------------------------------------

func TestFakeSenderConcurrent(t *testing.T) {
	tg := &fakeSender{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tg.SendMessage(context.Background(), -1, "x") }()
	}
	wg.Wait()
	if len(tg.texts()) != 50 {
		t.Fatalf("lost sends: %d", len(tg.texts()))
	}
}
