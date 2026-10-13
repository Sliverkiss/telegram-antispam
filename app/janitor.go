package main

import (
	"context"
	"runtime"
	"sync"
	"time"

	"adbot/internal/bot"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/store"
)

// SweepState is the shared janitor status consumed by /statusz
// (next_sweep_in, last_sweep at + dropped counts). The janitor goroutine
// writes; the HTTP handlers read under the mutex.
type SweepState struct {
	mu                sync.Mutex
	lastSweepAt       time.Time
	nextSweepAt       time.Time
	droppedSuspects   int64
	droppedSignatures int64
	remainingTotal    int64
	ok                bool
}

// Snapshot copies the current sweep state.
func (s *SweepState) Snapshot() (at time.Time, next time.Time, droppedSuspects, droppedSigs, remaining int64, ran bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSweepAt, s.nextSweepAt, s.droppedSuspects, s.droppedSignatures, s.remainingTotal, s.ok
}

// statusProvider lets the server read the sweep state without importing main.
type statusProvider interface {
	SweepSnapshot() (at time.Time, next time.Time, droppedSuspects, droppedSigs, remaining int64, ran bool)
}

// SweepSnapshot implements the server's statusProvider.
func (s *SweepState) SweepSnapshot() (time.Time, time.Time, int64, int64, int64, bool) {
	return s.Snapshot()
}

// startJanitor runs the TTL/capacity sweep loop on an interval, sweeping once
// at startup and then every BOT_SWEEP_INTERVAL. It returns a stop func.
// sweepState (when non-nil) receives each sweep result for /statusz.
func startJanitor(b *bot.Bot, cfg config.Config, log *logx.Logger, sweepState *SweepState) func() {
	sw, ok := b.Store.(store.Sweeper)
	if !ok {
		log.Warn("janitor_skipped", "summary", "存储后端不支持本机清扫（仅内存模式具备 TTL 清理）")
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		sweep := func() {
			start := time.Now()
			res, err := sw.Sweep(ctx, store.SweepOptions{
				SuspectTTL:    cfg.RetentionFor("suspect"),
				SignatureTTL:  cfg.RetentionFor("signature"),
				MaxSuspects:   cfg.MaxSuspects,
				MaxSignatures: cfg.MaxSignatures,
			})
			elapsed := time.Since(start)
			if err != nil {
				log.Error("sweep_error", "err", err.Error())
				return
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Info("sweep",
				"dropped_suspects", res.DroppedSuspects,
				"dropped_signatures", res.DroppedSignatures,
				"remaining_total", res.RemainingTotal,
				"heap_kib", m.HeapAlloc/1024,
				"elapsed_ms", elapsed.Milliseconds())
			if res.EvictedSuspects > 0 {
				log.Warn("sweep_evict_suspects", "evicted", res.EvictedSuspects)
			}
			if res.EvictedSignatures > 0 {
				log.Warn("sweep_evict_signatures", "evicted", res.EvictedSignatures)
			}
			if sweepState != nil {
				sweepState.mu.Lock()
				sweepState.lastSweepAt = start
				sweepState.nextSweepAt = time.Now().Add(cfg.SweepInterval)
				sweepState.droppedSuspects = res.DroppedSuspects
				sweepState.droppedSignatures = res.DroppedSignatures
				sweepState.remainingTotal = res.RemainingTotal
				sweepState.ok = true
				sweepState.mu.Unlock()
			}
		}

		sweep() // startup pass
		ticker := time.NewTicker(cfg.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
