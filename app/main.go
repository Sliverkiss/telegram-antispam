// adbot is a Telegram anti-spam/promotion bot: it screens member-join bios
// and in-group messages, keeps a watchlist, maintains sha1 signature libraries
// (bio + message), and enforces ban/kick — deleting only the triggering ad
// message. State persists in memory by default (BOT_STORE=memory) with a
// TTL janitor; Redis/file backends are opt-in. The Telegram client is
// github.com/go-telegram/bot v1.27.0 wrapped behind a thin internal interface.
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

	// Unrecognized BOT_* variables → WARN (typo guard).
	for _, name := range config.UnrecognizedBotVars() {
		log.Warn("unrecognized_env", "var", name)
	}

	// Startup summary (redacted).
	log.Info("startup", "version", cfg.Version, "store", cfg.Store, "dry_run", cfg.DryRun)

	if err := cfg.Validate(); err != nil {
		log.Error("config_invalid", "err", err.Error())
		os.Exit(2)
	}

	// BOT_CHATS whitelist warning (all groups will be left when empty).
	if !cfg.ChatsSet {
		log.Warn("chats_not_configured", "hint", "BOT_CHATS 未配置，将退出所有群，只应答私聊命令")
	}

	// Store: default memory; redis/file only when explicitly requested.
	st, storeName, err := openStore(cfg, log)
	if err != nil {
		log.Error("store_open_error", "err", err.Error())
		os.Exit(1)
	}
	defer st.Close()
	if storeName == "memory" {
		log.Warn("memory_mode", "summary", "内存模式：重启后观察名单/签名库/统计将丢失")
	}
	if cfg.AIKey == "" {
		log.Warn("ai_key_missing", "summary", "BOT_AI_API_KEY 未配置：将跳过 AI 判定，绝不误封")
	}

	// AI classifier.
	classify := func(ctx context.Context, content string) (ai.Verdict, bool) {
		return ai.Classify(ctx, ai.Config{
			BaseURL: cfg.AIBaseURL,
			APIKey:  cfg.AIKey,
			Model:   cfg.AIModel,
			Timeout: cfg.AITimeout,
			Retries: 2,
			Logger:  log.WithComponent("ai"),
		}, content)
	}

	// Telegram client (library-backed; real endpoint unless tests override).
	tg, err := telegram.NewClient(cfg.Token)
	if err != nil {
		log.Error("telegram_init_error", "err", err.Error())
		os.Exit(1)
	}

	b := bot.New(st, tg, classify, cfg, log.WithComponent("bot"))

	// Janitor goroutine (TTL sweep + capacity caps), stop on shutdown.
	janitorStop := startJanitor(b, cfg, log.WithComponent("janitor"))

	// HTTP server (port 3000 for belmo health checks).
	srv := server.New(st, b, cfg, log.WithComponent("http"))
	httpSrv := &http.Server{Addr: cfg.BotAddr, Handler: srv.Handler()}

	// Register commands with Telegram + dispatch long-poll via the library.
	if err := registerCommands(tg, log); err != nil {
		log.Warn("set_my_commands_error", "err", err.Error())
	}
	pollCtx, stopPoll := context.WithCancel(context.Background())
	go runPoller(pollCtx, tg, cfg, b, log.WithComponent("poller"))

	// Graceful shutdown: SIGINT/SIGTERM → stop poll, close store, shutdown http.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("shutdown_signal")
		stopPoll()
		janitorStop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tg.Close(ctx)
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Warn("http_shutdown_error", "err", err.Error())
		}
	}()

	log.Info("poll_started", "store", storeName, "addr", cfg.BotAddr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http_listen_error", "err", err.Error())
		os.Exit(1)
	}
	log.Info("shutdown_complete")
}

// openStore wires memory (default) or explicitly-configured redis backends.
// The file backend (BOT_STORE=file) is not implemented in this round; it falls
// back to memory with a warning so the process still starts safely.
func openStore(cfg config.Config, log *logx.Logger) (store.Store, string, error) {
	switch cfg.Store {
	case "redis":
		if cfg.RedisURL == "" {
			return nil, "", fmt.Errorf("BOT_STORE=redis but BOT_REDIS_URL is not set")
		}
		rs, err := store.OpenRedis(cfg.RedisURL, 0, cfg.RedisInsecureTLS, log.WithComponent("store"))
		if err != nil {
			return nil, "", fmt.Errorf("redis: %w", err)
		}
		return rs, "redis", nil
	case "auto":
		if cfg.RedisURL != "" {
			rs, err := store.OpenRedis(cfg.RedisURL, 0, cfg.RedisInsecureTLS, log.WithComponent("store"))
			if err != nil {
				return nil, "", fmt.Errorf("redis: %w", err)
			}
			return rs, "redis", nil
		}
		return store.NewMemory(), "memory", nil
	case "file":
		log.Warn("file_store_unimplemented", "summary", "BOT_STORE=file 后端本轮未实现，回落到内存模式")
		return store.NewMemory(), "memory", nil
	default: // "memory"
		return store.NewMemory(), "memory", nil
	}
}

// registerCommands registers /start /help /status /check with Telegram.
func registerCommands(tg *telegram.Client, log *logx.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmds := []telegram.BotCommand{
		{Command: "start", Description: "查看在线状态与运行信息"},
		{Command: "help", Description: "命令与行为说明"},
		{Command: "status", Description: "管理状态（仅管理员）"},
		{Command: "check", Description: "AI 判定测试（仅管理员）"},
	}
	return tg.SetMyCommands(ctx, cmds)
}

// runPoller drives the library's long-poll loop and forwards updates to the bot.
func runPoller(ctx context.Context, tg *telegram.Client, cfg config.Config, b *bot.Bot, log *logx.Logger) {
	tg.RegisterDefaultHandler(func(ctx context.Context, u telegram.Update) {
		b.NotePoll()
		b.HandleUpdate(ctx, u)
	})
	tg.Start(ctx)
	log.Info("poll_stopped")
}
