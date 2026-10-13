package main

import (
	"context"
	"time"

	"adbot/internal/ai"
	"adbot/internal/bot"
	"adbot/internal/logx"
	"adbot/internal/telegram"
)

// poller drives the getUpdates long-polling loop with offset tracking and
// 409/network backoff. It runs outside the main goroutine; the returned
// stop func ends the current poll and the loop.
func startPoller(ctx context.Context, tg *telegram.Client, b *bot.Bot, cfg pollerConfig, log *logx.Logger) func() {
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		var offset int64
		backoff := time.Second
		for {
			if child.Err() != nil {
				return
			}
			updates, err := tg.GetUpdates(child, offset, cfg.timeout, cfg.allowedUpdates)
			if err != nil {
				if telegram.IsRetriable(err) {
					log.Warn("poll_error", "err", err.Error(), "backoff", backoff.String())
					select {
					case <-child.Done():
						return
					case <-time.After(backoff):
					}
					if backoff < cfg.maxBackoff {
						backoff *= 2
					}
					continue
				}
				log.Error("poll_fatal_error", "err", err.Error())
				select {
				case <-child.Done():
					return
				case <-time.After(cfg.maxBackoff):
				}
				continue
			}

			b.NotePoll()
			backoff = time.Second
			for _, u := range updates {
				if u.UpdateID >= offset {
					offset = u.UpdateID + 1
				}
				log.Debug("update_received", "update_id", u.UpdateID)
				b.HandleUpdate(child, u)
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

type pollerConfig struct {
	timeout        int
	allowedUpdates []string
	maxBackoff     time.Duration
}

// classify defaults the bot's AI callback to the ai.Classify wrapper with the
// given config.
func classify(cfg ai.Config) bot.Classifier {
	return func(ctx context.Context, content string) (ai.Verdict, bool) {
		return ai.Classify(ctx, cfg, content)
	}
}
