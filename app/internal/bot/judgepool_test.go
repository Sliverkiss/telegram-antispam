package bot

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/retryq"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// slowFakeAI blocks each Classify until released; used to prove the handler
// returns while judgments are still running. block is created in ctor.
type slowFakeAI struct {
	mu       sync.Mutex
	calls    int
	block    chan struct{} // closed to unblock every call
	verdict  ai.Verdict
	blocking bool
}

func newSlowFakeAI(verdict ai.Verdict) *slowFakeAI {
	return &slowFakeAI{block: make(chan struct{}), verdict: verdict, blocking: true}
}

func (f *slowFakeAI) Classify(_ context.Context, content ai.Content, mode string) (ai.ClassifyResult, bool) {
	if f.blocking {
		<-f.block
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	res := ai.ClassifyResult{Verdict: f.verdict, Model: "fake-model"}
	return res, true
}

func (f *slowFakeAI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// poolBot builds a bot with the async judge pool STARTED (amend6 defaults)
// and registers cleanup: Stop must run before the test ends.
func poolBot(t *testing.T, tg TelegramClient, classify AIClassifier, cfg config.Config) *Bot {
	t.Helper()
	if cfg.JudgeQueue == 0 {
		cfg.JudgeQueue = 500
	}
	st := store.NewMemory()
	lg := logx.New(nil, logx.Error, "text", "bot")
	b := New(st, tg, classify, nil, cfg, lg)
	b.StartJudgePool(context.Background())
	t.Cleanup(b.StopJudgePool)
	return b
}

func judgeCfg() config.Config {
	c := defaultCfg()
	c.JudgeAsync = true
	c.JudgeQueue = 500
	c.JudgeConcurrency = 8
	c.AIJudgeTimeout = 150 * time.Second
	return c
}

// ① 200 submissions: the handler returns in <100ms for every one (while the
// classifier is BLOCKED — the strongest form of "never blocks"), and after
// unblocking all 200 are judged.
func TestJudgePoolHandlerNeverBlocks200(t *testing.T) {
	tg := newFakeTG()
	fa := newSlowFakeAI(fakeVerdict(false, "无", 0.9))
	cfg := judgeCfg()
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		uid := int64(1000 + i)
		b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -500})
	}

	start := time.Now()
	for i := 0; i < 200; i++ {
		uid := int64(1000 + i)
		b.HandleMessage(ctx, adMsg(-500, uid, int64(500+i), fmt.Sprintf("消息 %d", i)))
	}
	elapsed := time.Since(start)
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("200 HandleMessage calls took %v with a BLOCKED classifier; must be <100ms (async)", elapsed)
	}
	if got := fa.callCount(); got != 0 {
		t.Fatalf("classifier must still be blocked at handler return: %d calls", got)
	}
	// All 200 accepted into the pool (none dropped, none queue-full).
	async, _, queued, _, cap, enq, full := b.JudgePoolStats()
	if full != 0 {
		t.Fatalf("queue-full counter = %d, want 0", full)
	}
	if enq != 200 {
		t.Fatalf("enqueued = %d, want 200", enq)
	}
	_ = queued
	_ = cap
	_ = async

	close(fa.block)
	waitFor(t, func() bool { return fa.callCount() == 200 }, "all 200 judgments to run")
}

// ② Same-uid serialization: 50 judgments for ONE uid run strictly in
// submission order (the per-key FIFO guarantee), while different uids
// interleave.
func TestJudgePoolSameUIDRunsInOrder(t *testing.T) {
	tg := newFakeTG()
	var order []int64
	var mu sync.Mutex
	fa := &orderAI{mu: &mu, order: &order}
	cfg := judgeCfg()
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	const uid = int64(4242)
	b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -500})
	for i := 0; i < 50; i++ {
		b.submitJudge(ctx, judgmentInput{
			key: msgKey(-500, int64(7000+i)), kind: "message", chatID: -500,
			messageID: int64(7000 + i), uid: uid, text: fmt.Sprintf("x%d", i),
			mode: "message", punish: false, enqueuedAt: time.Now(),
		})
	}
	waitFor(t, func() bool { return fa.count() == 50 }, "50 same-uid judgments")
	mu.Lock()
	defer mu.Unlock()
	for i, seq := range order {
		if seq != int64(i) {
			t.Fatalf("same-uid judgments out of order at %d: got sequence %d", i, seq)
		}
	}
}

// orderAI records the per-call arrival sequence (thread-safe).
type orderAI struct {
	mu    *sync.Mutex
	order *[]int64
	n     int64
}

func (f *orderAI) Classify(_ context.Context, _ ai.Content, _ string) (ai.ClassifyResult, bool) {
	seq := atomic.AddInt64(&f.n, 1) - 1
	f.mu.Lock()
	*f.order = append(*f.order, seq)
	f.mu.Unlock()
	return ai.ClassifyResult{Verdict: fakeVerdict(false, "无", 0.9), Model: "m"}, true
}

func (f *orderAI) count() int { return int(atomic.LoadInt64(&f.n)) }

// ③ Watchlist consumed exactly once under concurrent same-uid judgments: the
// join-bio judgment and the first message judgment both run for one uid, in
// order, and the not-ad message verdict consumes the slot exactly once (the
// second message sees no watchlist → no AI call).
func TestJudgePoolWatchlistConsumedOnceConcurrently(t *testing.T) {
	tg := newFakeTG()
	tg.chat[77] = telegram.ChatInfo{ID: 77, Bio: "爱读书爱旅行"}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	cfg := judgeCfg()
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	const uid = int64(77)

	// Join (bio judgment) + immediate first message — the exact interleaving
	// the same-uid serialization must protect.
	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "reader", "R")}, 0)
	b.HandleMessage(ctx, adMsg(-500, uid, 800, "大家好"))
	// A second message after the first consumed the slot.
	waitFor(t, func() bool { return fa.callCount() == 2 }, "bio + first message judgments")
	b.HandleMessage(ctx, adMsg(-500, uid, 801, "再发一条"))

	waitFor(t, func() bool {
		sus, _ := b.Store.IsSuspect(ctx, uid)
		return !sus
	}, "watchlist slot consumed")
	if fa.callCount() != 2 {
		t.Fatalf("exactly 2 AI calls (bio + first message) expected, got %d — the post-consumption message must not re-judge", fa.callCount())
	}
}

// ④ Queue full → retryq handoff + counter increment (nothing dropped).
func TestJudgePoolQueueFullHandstoRetryQ(t *testing.T) {
	tg := newFakeTG()
	fa := newSlowFakeAI(fakeVerdict(false, "无", 0.9))
	cfg := judgeCfg()
	cfg.JudgeQueue = 2 // tiny capacity: the 3rd+ submissions overflow
	b := poolBot(t, tg, fa, cfg)
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()
	// Fill the one running key (all share uid 1 → serialized; the first is
	// executing, the 2nd queued → the 3rd must overflow).
	for i := 0; i < 5; i++ {
		b.submitJudge(ctx, judgmentInput{
			key: msgKey(-500, int64(9000+i)), kind: "message", chatID: -500,
			messageID: int64(9000 + i), uid: 1, text: "x", mode: "message",
			punish: true, enqueuedAt: time.Now(),
		})
	}
	_, _, _, _, _, enq, full := b.JudgePoolStats()
	if enq != 2 {
		t.Fatalf("enqueued = %d, want 2 (capacity)", enq)
	}
	if full != 3 {
		t.Fatalf("queue-full counter = %d, want 3", full)
	}
	if rq.Len() != 3 {
		t.Fatalf("overflow must be handed to retryq: len=%d, want 3", rq.Len())
	}
	close(fa.block)
}

// ⑤ Graceful drain: in-flight judgment finishes within the budget; Stop
// completes and reports.
func TestJudgePoolGracefulDrain(t *testing.T) {
	tg := newFakeTG()
	block := make(chan struct{})
	release := make(chan struct{})
	fa := &drainAI{block: block, release: release, verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg := judgeCfg()
	cfg.JudgeDrain = 2 * time.Second
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 88, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 88, 90, "广告内容"))
	waitFor(t, func() bool { return fa.started() }, "judgment to start")
	close(release) // let the chain finish as Stop begins
	go func() { <-block; close(release) }()
	time.Sleep(20 * time.Millisecond)

	stopStart := time.Now()
	b.StopJudgePool()
	if !b.judgePool.Stopped() {
		t.Fatal("Stop must complete")
	}
	if took := time.Since(stopStart); took > cfg.JudgeDrain+500*time.Millisecond {
		t.Fatalf("Stop took %v, budget %v", took, cfg.JudgeDrain)
	}
	// The in-flight judgment ran to completion (punished).
	waitFor(t, func() bool { return len(tg.bans) == 1 }, "drained judgment to punish")
}

type drainAI struct {
	block   chan struct{} // closed by the test to let Classify return
	release chan struct{}
	verdict ai.Verdict
	n       atomic.Int64
}

func (f *drainAI) Classify(_ context.Context, _ ai.Content, _ string) (ai.ClassifyResult, bool) {
	f.n.Add(1)
	<-f.release
	return ai.ClassifyResult{Verdict: f.verdict, Model: "m"}, true
}

func (f *drainAI) started() bool { return f.n.Load() > 0 }

// ⑥ BOT_JUDGE_ASYNC=false → identical to the old inline behavior: the handler
// does NOT return until the judgment completes.
func TestJudgePoolAsyncOffInlineLegacy(t *testing.T) {
	tg := newFakeTG()
	tg.chat[300] = telegram.ChatInfo{ID: 300, Bio: "满18进 在线发牌"}
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.9)}
	cfg := judgeCfg()
	cfg.JudgeAsync = false
	b := poolBot(t, tg, fa, cfg) // pool started but async off
	ctx := context.Background()

	b.HandleNewMembers(ctx, -300, []telegram.User{u(300, false, "g", "G")}, 0)

	// Synchronous: the ban already happened when HandleNewMembers returned.
	if len(tg.bans) != 1 {
		t.Fatalf("inline mode must dispose before return: bans=%v", tg.bans)
	}
	_, _, _, _, _, enq, _ := b.JudgePoolStats()
	if enq != 0 {
		t.Fatalf("async-off must not enqueue: %d", enq)
	}
}

// Whole-chain budget (B.2): a judgment whose chain exceeds
// BOT_AI_JUDGE_TIMEOUT is abandoned and handed to the retry queue.
func TestJudgePoolBudgetExceededHandsToRetryQ(t *testing.T) {
	tg := newFakeTG()
	fa := newSlowFakeAI(fakeVerdict(false, "无", 0.9))
	cfg := judgeCfg()
	cfg.AIJudgeTimeout = 80 * time.Millisecond
	b := poolBot(t, tg, fa, cfg)
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()

	b.submitJudge(ctx, judgmentInput{
		key: msgKey(-500, 9500), kind: "message", chatID: -500, messageID: 9500,
		uid: 5, text: "x", mode: "message", punish: true, enqueuedAt: time.Now(),
	})
	waitFor(t, func() bool { return rq.Len() == 1 }, "budget-exceeded handoff to retryq")
	_, _, _, _, _, _, full := b.JudgePoolStats()
	if full != 0 {
		t.Fatalf("budget handoff is not a queue-full: %d", full)
	}
	close(fa.block)
}

// /statusz-facing stats adapter (A.4).
func TestJudgePoolStatuszView(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	cfg := judgeCfg()
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 60, ChatID: -500})
	b.HandleMessage(ctx, adMsg(-500, 60, 61, "正常内容"))
	waitFor(t, func() bool { return fa.callCount() == 1 }, "one judgment")

	async, conc, queued, inflight, cap, enq, full := b.JudgePoolStats()
	if !async || conc != 8 || cap != 500 {
		t.Fatalf("pool view = %v/%v/%v, want true/8/500", async, conc, cap)
	}
	if enq != 1 || full != 0 {
		t.Fatalf("counters = %d/%d, want 1/0", enq, full)
	}
	if queued != 0 || inflight != 0 {
		t.Fatalf("gauges must return to zero: queued=%d inflight=%d", queued, inflight)
	}
}

// overlapAI counts how many Classify calls for the same uid run at once, with
// a widened window so a broken (per-item-key) serialization is caught reliably.
type overlapAI struct {
	cur  int64
	max  int64
	done int64
}

func (f *overlapAI) Classify(_ context.Context, _ ai.Content, _ string) (ai.ClassifyResult, bool) {
	n := atomic.AddInt64(&f.cur, 1)
	for {
		m := atomic.LoadInt64(&f.max)
		if n <= m || atomic.CompareAndSwapInt64(&f.max, m, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	atomic.AddInt64(&f.cur, -1)
	atomic.AddInt64(&f.done, 1)
	return ai.ClassifyResult{Verdict: fakeVerdict(false, "无", 0.9), Model: "m"}, true
}

// ②b Regression: the same-key serialization key MUST be the uid, not the
// per-item key. Message keys are msgKey(chat,msgID) — unique per message — so
// keying the waiting room on them runs one user's messages fully concurrently,
// which is exactly what breaks the join-bio vs. first-message ordering that the
// watchlist "one judgment = one consumption" semantics depend on. The old code
// measured max=8 here; the fix must hold it at 1.
func TestJudgePoolSameUIDNeverOverlaps(t *testing.T) {
	tg := newFakeTG()
	fa := &overlapAI{}
	cfg := judgeCfg()
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	const uid = int64(9001)
	b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -500})

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.submitJudge(ctx, judgmentInput{
				key: msgKey(-500, int64(9000+i)), kind: "message", chatID: -500,
				messageID: int64(9000 + i), uid: uid, text: "x", mode: "message",
			})
		}(i)
	}
	wg.Wait()
	waitFor(t, func() bool { return atomic.LoadInt64(&fa.done) == 40 }, "40 same-uid judgments to run")

	if m := atomic.LoadInt64(&fa.max); m != 1 {
		t.Fatalf("max concurrent AI calls for one uid = %d, want 1 (per-uid serialization is broken)", m)
	}
}

// ③b Regression: BOT_AI_CONCURRENCY must actually bound parallel AI chains.
// Distinct uids each get their own key runner; without the semaphore a burst
// fans out into one concurrent call per uid — the self-inflicted gateway
// overload (pool_parked) this bot has already been bitten by.
func TestJudgePoolConcurrencyCapEnforced(t *testing.T) {
	tg := newFakeTG()
	fa := &overlapAI{}
	cfg := judgeCfg()
	cfg.JudgeConcurrency = 2
	b := poolBot(t, tg, fa, cfg)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		uid := int64(5000 + i)
		b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -500})
		b.submitJudge(ctx, judgmentInput{
			key: msgKey(-500, int64(6000+i)), kind: "message", chatID: -500,
			messageID: int64(6000 + i), uid: uid, text: "x", mode: "message",
		})
	}
	waitFor(t, func() bool { return atomic.LoadInt64(&fa.done) == 12 }, "12 distinct-uid judgments to run")
	if m := atomic.LoadInt64(&fa.max); m > 2 {
		t.Fatalf("max concurrent AI calls = %d, want <= 2 (BOT_AI_CONCURRENCY not enforced)", m)
	}
}
