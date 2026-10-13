package main

import (
	"context"
	"runtime"
	"time"

	"adbot/internal/bot"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/store"
)

// janitorStats tracks the most recent sweep for /statusz.
type janitorStats struct {
	lastSweep         time.Time
	droppedSuspects   int64
	droppedSignatures int64
	nextSweepIn       time.Duration
}

// startJanitor runs the TTL/capacity sweep loop on an interval, sweeping once
// at startup and then every BOT_SWEEP_INTERVAL. It returns a stop func.
func startJanitor(b *bot.Bot, cfg config.Config, log *logx.Logger) func() {
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
