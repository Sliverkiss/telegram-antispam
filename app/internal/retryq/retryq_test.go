package retryq

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"adbot/internal/logx"
)

func testQueue(retry RetryFunc, interval time.Duration, maxTries, capacity int) *Queue {
	return New(retry, interval, maxTries, capacity, logx.New(nil, logx.Debug, "text", "retryq"))
}

func TestDefaults(t *testing.T) {
	q := New(nil, 0, 0, 0, nil)
	if q.interval != 10*time.Second || q.maxTries != 5 || q.capacity != 500 {
		t.Errorf("defaults = %s/%d/%d", q.interval, q.maxTries, q.capacity)
	}
}

func TestEnqueueDedupByKey(t *testing.T) {
	q := testQueue(nil, time.Hour, 5, 100)
	if !q.Enqueue(Item{Key: "msg:1:1"}) {
		t.Fatal("first enqueue should succeed")
	}
	if q.Enqueue(Item{Key: "msg:1:1"}) {
		t.Error("duplicate key must be deduped")
	}
	if q.Len() != 1 {
		t.Errorf("len = %d", q.Len())
	}
}

func TestEnqueueOverflowEvictsOldest(t *testing.T) {
	q := testQueue(nil, time.Hour, 5, 3)
	for i := 0; i < 5; i++ {
		q.Enqueue(Item{Key: string(rune('a' + i))})
	}
	if q.Len() != 3 {
		t.Fatalf("len = %d, want cap 3", q.Len())
	}
	// The oldest two (a, b) were evicted; the head must now be "c".
	q.mu.Lock()
	head := q.items[0].Key
	q.mu.Unlock()
	if head != "c" {
		t.Errorf("head = %q, want c (oldest evicted)", head)
	}
}

func TestEnqueueAfterStopIsNoop(t *testing.T) {
	q := testQueue(func(context.Context, Item) (bool, string) { return true, "" }, time.Hour, 5, 10)
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	cancel()
	q.Stop()
	if q.Enqueue(Item{Key: "x"}) {
		t.Error("enqueue after Stop must be refused")
	}
}

// The fixed-interval contract: an item is retried every `interval` until it
// resolves or the attempt budget is exhausted. No backoff: the second retry
// waits exactly the same interval.
func TestFixedIntervalRetryUntilTerminal(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	q := testQueue(func(_ context.Context, it Item) (bool, string) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n >= 3 {
			return true, "" // terminal on the 3rd round
		}
		return false, "chain_failed"
	}, 20*time.Millisecond, 5, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)
	q.Enqueue(Item{Key: "msg:1:1"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.Stop()
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries + terminal)", calls)
	}
	if q.Len() != 0 {
		t.Errorf("queue should be empty after terminal, len=%d", q.Len())
	}
}

func TestMaxAttemptsDropsItem(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	q := testQueue(func(_ context.Context, it Item) (bool, string) {
		mu.Lock()
		calls++
		mu.Unlock()
		return false, "chain_failed" // never resolves
	}, 10*time.Millisecond, 3, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)
	q.Enqueue(Item{Key: "msg:2:2"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.Stop()
	if calls != 3 {
		t.Errorf("calls = %d, want maxTries 3", calls)
	}
	if q.Len() != 0 {
		t.Errorf("exhausted item must be dropped, len=%d", q.Len())
	}
}

func TestAttemptsBumpPerRound(t *testing.T) {
	var mu sync.Mutex
	var lastAttempts int
	q := testQueue(func(_ context.Context, it Item) (bool, string) {
		mu.Lock()
		lastAttempts = it.Attempts
		mu.Unlock()
		return false, "chain_failed"
	}, 10*time.Millisecond, 3, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)
	q.Enqueue(Item{Key: "msg:3:3"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := lastAttempts
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.Stop()
	if lastAttempts < 2 {
		t.Errorf("attempts observed = %d (round 3 should see attempts=2)", lastAttempts)
	}
}

// Goroutine-leak guard: Start → Stop must return the worker goroutine count
// to baseline (B section of the task book).
func TestStopReturnsGoroutinesToBaseline(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		q := testQueue(func(context.Context, Item) (bool, string) { return true, "" }, time.Hour, 5, 10)
		ctx, cancel := context.WithCancel(context.Background())
		q.Start(ctx)
		cancel()
		q.Stop()
	}
	// Give the scheduler a moment to reap.
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Errorf("goroutines: before=%d after=%d — worker leaked", before, after)
	}
}

func TestStopWithoutStartDoesNotHang(t *testing.T) {
	q := testQueue(nil, time.Hour, 5, 10)
	done := make(chan struct{})
	go func() {
		q.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop without Start must not hang")
	}
}

func TestConcurrentEnqueue(t *testing.T) {
	q := testQueue(nil, time.Hour, 5, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q.Enqueue(Item{Key: string(rune('a'+i%26)) + string(rune('0'+i%10))})
		}(i)
	}
	wg.Wait()
	if q.Len() > 50 {
		t.Errorf("len = %d", q.Len())
	}
}
