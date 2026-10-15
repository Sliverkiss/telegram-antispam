// Package retryq is the background pending queue + worker that keeps failed
// AI judgments from being dropped (TASK_antispam_amend3.md A.5 / amend2 §1.1).
//
// Contract: a failed classification is re-enqueued and retried every
// BOT_AI_RETRY_INTERVAL (fixed 10s, no backoff, no circuit breaker) until
// BOT_AI_RETRY_MAX_ATTEMPTS is exhausted, at which point the local rule engine
// takes over and produces a terminal, fully automatic decision. The queue has
// a hard cap (BOT_AI_RETRY_QUEUE): overflow logs ERROR and evicts the oldest.
// The worker never blocks the long-poll loop — HandleUpdate returns as soon
// as the item is enqueued.
package retryq

import (
	"context"
	"sync"
	"time"

	"adbot/internal/logx"
)

// Item is one pending judgment: a message, or a join (bio), with everything
// the retry round needs.
type Item struct {
	Key       string // dedup key ("chat:msg" / "join:uid")
	Kind      string // "message" | "join"
	ChatID    int64
	MessageID int64 // triggering message to delete (0 for join/bio)
	UID       int64
	Username  string // notification identity (amend4 👤 line)
	FirstName string
	Text      string
	// SigText is the bio lanes' ORIGINAL bio (the signature/flash/notice
	// text) when bio-resolve enriched Text with 【相关简介】 blocks; empty =
	// use Text (message lane). bio-resolve round.
	SigText     string
	Mode        string // ai prompt mode: "message" or "bio"
	ImageURL    string // data URL when the message carried a judged image
	Attempts    int    // retry rounds already consumed (round 1 = the initial call)
	EnqueuedAt  time.Time
	LastErrCode string
	Reason      string // why it is pending ("chain_failed", "vote_unresolved", ...)
}

// RetryFunc executes one full retry round for an item. ok=true means the item
// reached a terminal verdict (handled inside the closure: punish or clear).
// ok=false means it stays pending for the next round.
type RetryFunc func(ctx context.Context, it Item) (ok bool, errCode string)

// Queue is the bounded pending queue + background worker.
type Queue struct {
	mu       sync.Mutex
	items    []Item         // FIFO; index 0 is the oldest
	index    map[string]int // key → position hint (re-scanned on pop)
	capacity int
	interval time.Duration
	maxTries int
	retry    RetryFunc
	log      *logx.Logger

	wake   chan struct{} // signal the worker early (insert path)
	closed bool
	done   chan struct{}
	// workerStarted records whether Start launched the worker loop (Stop
	// without Start must not block on done).
	workerStarted bool
}

// New builds a queue. interval ≤ 0 → 10s; maxTries ≤ 0 → 5; capacity ≤ 0 → 500.
func New(retry RetryFunc, interval time.Duration, maxTries, capacity int, log *logx.Logger) *Queue {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if maxTries <= 0 {
		maxTries = 5
	}
	if capacity <= 0 {
		capacity = 500
	}
	if log == nil {
		log = logx.New(nil, logx.Warn, "text", "retryq")
	}
	return &Queue{
		index:    map[string]int{},
		capacity: capacity,
		interval: interval,
		maxTries: maxTries,
		retry:    retry,
		log:      log,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Enqueue adds an item unless its key is already pending (dedup). Overflow
// evicts the oldest with an ERROR log. It never blocks.
func (q *Queue) Enqueue(it Item) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if _, ok := q.index[it.Key]; ok {
		return false // already pending — dedup, no double queueing
	}
	if it.EnqueuedAt.IsZero() {
		it.EnqueuedAt = time.Now()
	}
	q.items = append(q.items, it)
	q.index[it.Key] = len(q.items) - 1
	if len(q.items) > q.capacity {
		evicted := q.items[0]
		q.items = q.items[1:]
		q.reindex()
		q.log.Error("retry_queue_overflow",
			"evicted_key", evicted.Key,
			"capacity", q.capacity)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// Len returns the pending count.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Start launches the background worker loop; it stops when ctx is canceled or
// Stop is called; the done channel closes when the worker has exited.
func (q *Queue) Start(ctx context.Context) {
	q.mu.Lock()
	q.workerStarted = true
	q.mu.Unlock()
	go func() {
		defer close(q.done)
		ticker := time.NewTicker(q.interval)
		defer ticker.Stop()
		for {
			if q.isClosed() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-q.wake:
			}
			q.drain(ctx)
		}
	}()
}

func (q *Queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// drain processes every due item once (due = enqueued ≥ interval ago).
func (q *Queue) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		it, ok := q.popDue()
		if !ok {
			return
		}
		q.log.Info("ai_retry",
			"key", it.Key,
			"attempt", it.Attempts+1,
			"max_attempts", q.maxTries,
			"age_ms", time.Since(it.EnqueuedAt).Milliseconds(),
			"pending_queue", q.Len(),
			"error_code", it.LastErrCode)
		done, errCode := q.retry(ctx, it)
		if done {
			continue
		}
		// Still undecided: bump attempts; at the cap, drop the item (the
		// retry closure itself runs the local-rule terminal path on the last
		// round — errCode "local_fallback" means it already concluded).
		it.Attempts++
		it.LastErrCode = errCode
		if it.Attempts >= q.maxTries {
			q.log.Error("ai_unresolved",
				"key", it.Key,
				"attempts", it.Attempts,
				"err_code", errCode)
			continue
		}
		q.Enqueue(it)
	}
}

// popDue removes and returns the oldest item that has waited ≥ interval.
func (q *Queue) popDue() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return Item{}, false
	}
	now := time.Now()
	for i, it := range q.items {
		if now.Sub(it.EnqueuedAt) >= q.interval {
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.reindex()
			return it, true
		}
		break // FIFO: only the head is due if none is (order is enqueue order)
	}
	return Item{}, false
}

func (q *Queue) reindex() {
	q.index = make(map[string]int, len(q.items))
	for i, it := range q.items {
		q.index[it.Key] = i
	}
}

// Stop closes the queue and waits for the worker to exit (goroutine-leak
// guard). Enqueue after Stop is a no-op. Stop without Start returns directly
// (the done channel is only closed by the worker itself).
func (q *Queue) Stop() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		<-q.done
		return
	}
	q.closed = true
	started := q.workerStarted
	q.mu.Unlock()
	if !started {
		return // Start was never called: nothing to wait for.
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	<-q.done
}

// Stopped reports whether Stop has completed.
func (q *Queue) Stopped() bool {
	select {
	case <-q.done:
		return true
	default:
		return false
	}
}
