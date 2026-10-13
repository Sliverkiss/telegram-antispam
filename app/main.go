// adbot is a Telegram anti-spam/promotion bot: it screens member-join bios
// and in-group messages, keeps a watchlist, maintains sha1 signature
// libraries (bio + message), and enforces ban/kick via a stdlib-only Bot API
// client. State persists in Redis (hand-written RESP) or in memory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"adbot/internal/ai"
	"adbot/internal/bot"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/server"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

func main() {
	selftest := flag.Bool("selftest", false, "run connectivity acceptance checks and exit")
	flag.Parse()

	cfg := config.Load()
	log := cfg.Logger()

	if *selftest {
		runSelftest(cfg)
		return
	}

	// Startup summary (redacted).
	log.Info("startup", "version", cfg.Version)
	for k, v := range cfg.Redacted() {
		log.Debug("config", "key", k, "value", v)
	}

	if err := cfg.Validate(); err != nil {
		log.Error("config_invalid", "err", err.Error())
		os.Exit(2)
	}

	// Store: Redis when BOT_REDIS_URL is set (fail-fast), else memory.
	st, storeName, err := openStore(cfg, log)
	if err != nil {
		log.Error("store_open_error", "err", err.Error())
		os.Exit(1)
	}
	defer st.Close()

	// AI classifier.
	aicfg := ai.New(ai.Config{
		BaseURL: cfg.AIBaseURL,
		APIKey:  cfg.AIKey,
		Model:   cfg.AIModel,
		Timeout: cfg.AITimeout,
		Retries: 2,
		Logger:  log.WithComponent("ai"),
	})

	// Telegram client.
	tg := telegram.NewClient(cfg.Token)

	b := bot.New(st, tg, classify(aicfg), cfg, log.WithComponent("bot"))

	// HTTP server (port 3000 for belmo health checks).
	srv := server.New(st, b, cfg.AdminToken, cfg.Version, log.WithComponent("http"))
	httpSrv := &http.Server{Addr: cfg.BotAddr, Handler: srv.Handler()}

	// Long-poll in a goroutine, stopped on shutdown.
	pollCtx, stopPoll := context.WithCancel(context.Background())
	stop := startPoller(pollCtx, tg, b, pollerConfig{
		timeout:        cfg.PollTimeout,
		allowedUpdates: []string{"message", "chat_member"},
		maxBackoff:     30 * time.Second,
	}, log.WithComponent("poller"))
	log.Info("poll_started", "store", storeName, "addr", cfg.BotAddr)

	// Graceful shutdown: SIGINT/SIGTERM → stop poll, close redis, shutdown http.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("shutdown_signal")
		stopPoll()
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Warn("http_shutdown_error", "err", err.Error())
		}
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http_listen_error", "err", err.Error())
		os.Exit(1)
	}
	log.Info("shutdown_complete")
}

// openStore wires Redis or memory from config.
func openStore(cfg config.Config, log *logx.Logger) (store.Store, string, error) {
	if cfg.RedisURL == "" {
		log.Warn("redis_not_configured")
		return store.NewMemory(), "memory", nil
	}
	rs, err := store.OpenRedis(cfg.RedisURL, cfg.SuspectTTL, cfg.RedisInsecureTLS, log.WithComponent("store"))
	if err != nil {
		return nil, "", fmt.Errorf("redis: %w", err)
	}
	return rs, "redis", nil
}
