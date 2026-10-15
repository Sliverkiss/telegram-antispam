// The amend6 A async judgement pool.
//
// Problem (production evidence): go-telegram/bot v1.27.0 dispatches updates
// through exactly ONE worker goroutine by default, and b.judge() ran inline
// inside the update handler — a single 60s+ AI chain walk (measured
// elapsed_ms=66459) parked the whole bot: every other message, join and
// command queued behind it.
//
// Contract:
//   - The update handler does light parsing + submitJudge and returns in
//     milliseconds; the AI chain, vote round, punishment and notifications run
//     on pool chain runners.
//   - Same-key serialization (hard requirement): judgments for one uid run in
//     submission order; different uids run concurrently. Reason: the join-bio
//     judgment (joinKey(uid)) and the user's first judgeable message right
//     after must not interleave, or the "one judgment = one consumption"
//     watchlist semantics break. Implementation: a central per-key FIFO
//     waiting room (map[key][]judgmentInput); each key has at most one chain
//     runner at a time and that runner drains the key's queue strictly in
//     order. Different keys → different goroutines → real concurrency.
//   - BOT_JUDGE_QUEUE bounds the waiting room; overflow NEVER drops: the item
//     is handed to the existing retryq (enqueueRetry) + WARN + counter.
//   - BOT_AI_JUDGE_TIMEOUT bounds ONE judgment's whole AI chain (3 × 90s
//     timeouts would otherwise pin a slot for 270s when a model hangs).
//   - BOT_JUDGE_ASYNC=false is the escape hatch: submitJudge degrades to the
//     legacy inline call (one-line rollback in production).
//   - Graceful shutdown: no new submissions, in-flight chains get
//     BOT_JUDGE_DRAIN to finish; judge_pool_stopped is logged before
//     shutdown_complete.
package bot

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"adbot/internal/config"
)

// judgePool is the bounded, same-key-serialized judgement worker pool.
type judgePool struct {
	b *Bot

	async       bool
	cap         int
	drain       time.Duration
	budget      time.Duration // BOT_AI_JUDGE_TIMEOUT (≤0 = off)
	concurrency int           // BOT_AI_CONCURRENCY: max parallel chains (sem)

	mu      sync.Mutex
	queues  map[string][]judgmentInput // key → FIFO waiting room
	running map[string]bool            // key → a chain runner is active
	closed  bool                       // Stop was called

	// sem enforces BOT_AI_CONCURRENCY at the gateway: at most len(sem) AI
	// chains run at once; the rest wait in the room (and keep counting against
	// BOT_JUDGE_QUEUE). Without it a burst of distinct uids would fan out into
	// unbounded concurrent AI calls — the self-inflicted pool_parked overload.
	sem    chan struct{}
	stopCh chan struct{} // closed by Stop: releases parked acquirers

	queued      atomic.Int64 // items waiting (gauge)
	inflight    atomic.Int64 // chains executing (gauge)
	enqueuedTot atomic.Int64
	fullTot     atomic.Int64

	startedWorkers bool
	stopOnce       sync.Once
	stopped        chan struct{} // closed once Stop finished
}

// newJudgePool builds the pool from config (defaults applied defensively).
func newJudgePool(b *Bot, cfg config.Config) *judgePool {
	p := &judgePool{
		b:           b,
		async:       cfg.JudgeAsync,
		cap:         cfg.JudgeQueue,
		drain:       cfg.JudgeDrain,
		budget:      cfg.AIJudgeTimeout,
		concurrency: cfg.JudgeConcurrency,
		queues:      map[string][]judgmentInput{},
		running:     map[string]bool{},
		stopped:     make(chan struct{}),
		stopCh:      make(chan struct{}),
	}
	if p.cap <= 0 {
		p.cap = 500
	}
	if p.drain <= 0 {
		p.drain = 10 * time.Second
	}
	if p.concurrency <= 0 {
		p.concurrency = 8
	}
	p.sem = make(chan struct{}, p.concurrency)
	return p
}

// StatsSnapshot returns the /statusz gauges and counters.
func (p *judgePool) StatsSnapshot() (async bool, concurrency, queued, inflight, cap, enqueued, full int64) {
	if p == nil {
		return false, 0, 0, 0, 0, 0, 0
	}
	return p.async, int64(p.concurrency), p.queued.Load(), p.inflight.Load(),
		int64(p.cap), p.enqueuedTot.Load(), p.fullTot.Load()
}

// enabled reports whether async submission is active (escape hatch off or the
// workers not launched → the handler falls back to the inline legacy call).
func (p *judgePool) enabled() bool { return p.async && p.startedWorkers }

// serialKey is the same-key serialization key. It MUST be the uid, not the
// per-item key: message keys are msgKey(chat,msgID) — unique per message — so
// keying on them would run one user's messages fully concurrently. The
// invariant that matters is per-USER ordering (join-bio judgment vs. the first
// judgeable message after it), which is what keeps the "one judgment = one
// consumption" watchlist semantics intact.
func serialKey(in judgmentInput) string {
	if in.uid != 0 {
		return fmt.Sprintf("uid:%d", in.uid)
	}
	return in.key
}

// submitJudge is the handler entry: enqueue-and-return (async mode) or run
// inline (BOT_JUDGE_ASYNC=false / pool not started / pool unwired). Queue-full
// items are handed to the retry queue — never dropped. Returns false only on
// the queue-full handoff.
func (p *judgePool) submitJudge(ctx context.Context, in judgmentInput) bool {
	if !p.enabled() {
		// Escape hatch / not started: the legacy synchronous path, exactly the
		// pre-amend6 behavior.
		p.b.judge(ctx, in)
		return true
	}
	key := serialKey(in)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.b.judge(ctx, in) // shutting down: run inline rather than drop
		return true
	}
	// Admission bounds the WHOLE pipeline: items waiting in the room plus
	// judgments already running. Counting only the waiting room would make the
	// cap toothless (a runner pops its item immediately and blocks on the AI
	// chain, so the gauge stays near zero while hundreds are in flight) and
	// made this path timing-dependent. Running + waiting ≥ cap → overflow.
	if p.queued.Load()+p.inflight.Load() >= int64(p.cap) {
		p.mu.Unlock()
		p.fullTot.Add(1)
		p.b.Log.Warn("judge_queue_full",
			"key", key,
			"uid", in.uid,
			"cap", p.cap,
			"inflight", p.inflight.Load())
		// Overflow is never a drop: the existing retry queue owns the item
		// (it retries the judgment on its own cadence and concludes via the
		// local-rule fallback).
		p.b.enqueueRetry(in, "judge_queue_full")
		return false
	}
	if in.enqueuedAt.IsZero() {
		in.enqueuedAt = time.Now()
	}
	p.queues[key] = append(p.queues[key], in)
	p.mu.Unlock()

	p.enqueuedTot.Add(1)
	p.queued.Add(1)
	p.b.Log.Info("judge_enqueued",
		"key", key, "kind", in.kind, "uid", in.uid, "depth", p.queued.Load())

	p.dispatch(key)
	return true
}

// dispatch hands a key to a chain runner if the key is idle.
func (p *judgePool) dispatch(key string) {
	p.mu.Lock()
	if p.running[key] {
		p.mu.Unlock()
		return // the active runner drains the key's queue itself
	}
	p.running[key] = true
	p.mu.Unlock()
	go p.runKey(key)
}

// acquire takes one concurrency slot, releasing early when the pool stops.
func (p *judgePool) acquire() bool {
	select {
	case p.sem <- struct{}{}:
		return true
	case <-p.stopCh:
		return false
	}
}

// release returns one concurrency slot.
func (p *judgePool) release() { <-p.sem }

// flushKey releases one key's waiting items into the retry queue (never a
// drop) and frees the key so a later submission can start a fresh runner.
func (p *judgePool) flushKey(key, reason string) {
	p.mu.Lock()
	queued := p.queues[key]
	delete(p.queues, key)
	p.running[key] = false
	p.mu.Unlock()
	for _, in := range queued {
		p.queued.Add(-1)
		p.b.enqueueRetry(in, reason)
	}
}

// runKey processes every queued judgment for one key, strictly in submission
// order, then releases the key. It holds ONE concurrency slot (BOT_AI_CONCURRENCY)
// for the whole chain, so a burst of distinct uids cannot fan out into
// unbounded parallel AI calls.
func (p *judgePool) runKey(key string) {
	if !p.acquire() {
		// Stopped while parked: waiting items go to the retry queue.
		p.flushKey(key, "judge_pool_stopping")
		return
	}
	defer p.release()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			// Shutdown: still-waiting items are handed to the retry queue
			// (never dropped) instead of being run.
			p.flushKey(key, "judge_pool_stopping")
			return
		}
		q := p.queues[key]
		if len(q) == 0 {
			delete(p.queues, key)
			p.running[key] = false
			p.mu.Unlock()
			return
		}
		in := q[0]
		p.queues[key] = q[1:]
		// Dequeue accounting (queued-- and inflight++) is one atomic step
		// under p.mu: the admission bound reads queued+inflight, and a
		// window where BOTH counters have stepped down (dequeued, not yet
		// inflight) let one extra submission past the cap under race.
		p.queued.Add(-1)
		p.inflight.Add(1)
		p.mu.Unlock()

		waitMS := time.Since(in.enqueuedAt).Milliseconds()
		p.b.Log.Info("judge_dequeued",
			"key", key, "kind", in.kind, "uid", in.uid, "wait_ms", waitMS)
		p.runBounded(in)
		p.inflight.Add(-1)
	}
}

// runBounded runs one judgment under the whole-chain budget
// (BOT_AI_JUDGE_TIMEOUT). Budget exceeded → retry queue, not a drop. The
// abandoned chain keeps running to completion in the background: its terminal
// writes are idempotent at the punishment layer (dedup by key, suspect
// removal), and the retry queue re-judging is the fail-safe direction.
func (p *judgePool) runBounded(in judgmentInput) {
	if p.budget <= 0 {
		p.b.judge(context.Background(), in)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.b.judge(context.Background(), in)
	}()
	select {
	case <-done:
	case <-time.After(p.budget):
		p.b.aiFails.Add(1)
		p.b.Log.Warn("judge_budget_exceeded",
			"key", in.key,
			"uid", in.uid,
			"budget", p.budget.String())
		p.b.enqueueRetry(in, "judge_budget_exceeded")
		<-done // reaping: never leak the abandoned chain's goroutine
	}
}

// startWorkers flags the pool as live and ties its lifetime to ctx (cancel →
// Stop). Chain runners are spawned on demand by dispatch.
func (p *judgePool) startWorkers(ctx context.Context) {
	p.mu.Lock()
	p.startedWorkers = true
	p.mu.Unlock()
	if ctx != nil {
		go func() {
			<-ctx.Done()
			p.Stop()
		}()
	}
}

// Stop refuses new submissions and waits up to the drain budget for in-flight
// chains; still-queued items are flushed into the retry queue. Idempotent.
func (p *judgePool) Stop() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		keys := make([]string, 0, len(p.queues))
		for k := range p.queues {
			keys = append(keys, k)
		}
		p.mu.Unlock()
		// Release runners parked on the concurrency semaphore; each flushes its
		// key's waiting items into the retry queue.
		close(p.stopCh)

		// Wake every parked key runner so the closed branch flushes its
		// waiting items into the retry queue. dispatch is a no-op when a key
		// is still executing (that runner observes closed itself once its
		// current chain returns).
		for _, k := range keys {
			p.dispatch(k)
		}

		deadline := time.Now().Add(p.drain)
		for time.Now().Before(deadline) {
			if p.inflight.Load() == 0 && p.queued.Load() == 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		p.b.Log.Info("judge_pool_stopped",
			"drained", p.enqueuedTot.Load(),
			"inflight_timeout", p.inflight.Load(),
			"queued_remaining", p.queued.Load(),
			"timeout", p.drain.String())
		close(p.stopped)
	})
}

// Stopped reports whether Stop completed.
func (p *judgePool) Stopped() bool {
	select {
	case <-p.stopped:
		return true
	default:
		return false
	}
}
