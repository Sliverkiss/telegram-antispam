// adbot is a Telegram anti-spam/promotion bot: it screens member-join bios
// and in-group messages (text + photo multimodal), keeps a watchlist,
// maintains sha1 signature libraries (bio + message) with SimHash
// near-duplicate matching, and enforces ban/kick — deleting only the
// triggering ad message. AI judgment runs a model fallback chain with
// fixed-interval background retries and a local rule engine terminal
// fallback. State persists in memory by default (BOT_STORE=memory) with a
// TTL janitor; Redis backends are opt-in (file is explicitly not implemented
// and falls back to memory with a WARN). The Telegram client is
// github.com/go-telegram/bot v1.27.0 and the AI client
// github.com/sashabaranov/go-openai v1.43.0, both behind thin internal
// interfaces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"adbot/internal/ai"
	"adbot/internal/bot"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/notify"
	"adbot/internal/retryq"
	"adbot/internal/server"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

func main() {
	selftest := flag.Bool("selftest", false, "run connectivity acceptance checks (getMe only, no polling) and exit")
	promptEval := registerPromptEvalFlags(flag.CommandLine)
	flag.Parse()

	cfg := config.Load()
	log := cfg.Logger()

	// -prompteval is the only explicitly-outbound path; run it and exit.
	if promptEval.enabled {
		os.Exit(runPromptEval(cfg, promptEvalModels(cfg, promptEval), promptEval.samples))
	}

	if *selftest {
		runSelftest(cfg)
		return
	}

	// Unrecognized BOT_* variables → WARN (typo guard).
	for _, name := range config.UnrecognizedBotVars() {
		log.Warn("unrecognized_env", "var", name)
	}

	// Startup summary (redacted).
	log.Info("startup", "version", cfg.Version, "store", cfg.Store, "dry_run", cfg.DryRun,
		"prompt_version", ai.PromptVersion)

	if err := cfg.Validate(); err != nil {
		log.Error("config_invalid", "err", err.Error())
		os.Exit(2)
	}

	// BOT_AI_PROMPT_FILE overrides the embedded judge prompt (versioned).
	if cfg.AIPromptFile != "" {
		data, err := os.ReadFile(cfg.AIPromptFile)
		if err != nil {
			log.Warn("prompt_file_error", "path", cfg.AIPromptFile, "err", err.Error(),
				"summary", "继续使用内嵌提示词")
		} else if len(data) > 0 {
			ai.PromptOverride = strings.TrimSpace(string(data))
			log.Warn("prompt_file_loaded", "path", cfg.AIPromptFile,
				"summary", "内嵌提示词已被文件覆盖；注意版本追踪由文件自行维护")
		}
	}

	// BOT_CHATS whitelist warning (all groups will be left when empty).
	if !cfg.ChatsSet {
		log.Warn("chats_not_configured", "hint", "BOT_CHATS 未配置，将退出所有群，只应答私聊命令")
	}

	// Store: default memory; redis only when explicitly requested. The file
	// backend is explicitly NOT implemented (memory is the final choice).
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
	if !cfg.AIBaseURLConfigured() {
		log.Warn("ai_base_url_missing",
			"summary", "BOT_AI_BASE_URL 未配置（仍是占位符）：AI 判定会全部失败并走本地规则兜底，请在环境变量里填入你的 OpenAI 兼容网关")
	}
	// Ops visibility: which gateway actually arrived in this process's
	// environment (host only — never the key). Makes a platform variable that
	// was not injected obvious at a glance, instead of surfacing later as a
	// DNS error deep in the AI chain.
	log.Info("ai_config",
		"base_url_host", cfg.AIBaseURLHost(),
		"base_url_configured", cfg.AIBaseURLConfigured(),
		"api_key_set", cfg.AIKey != "",
		"models", strings.Join(cfg.AIEffectiveModels(), ","),
		"reasoning_effort", cfg.AIReasoningEffort,
		"enable_thinking", false,
		"prompt_version", ai.PromptVersion)

	// AI provider chain: primary + optional fallback provider.
	primary := ai.NewClient(ai.ClientConfig{
		BaseURL: cfg.AIBaseURL,
		APIKey:  cfg.AIKey,
		Models:  cfg.AIEffectiveModels(),
		Timeout: cfg.AITimeout,
		Logger:  log.WithComponent("ai"),
	})
	chain := providerChain{primary: primary, cfg: cfg, log: log.WithComponent("ai")}

	// Verify closure: 3 concurrent lanes on the verify models, per-lane timeout.
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.Verify(ctx, primary, ai.VerifyConfig{
			Votes:    cfg.VerifyVotes,
			Models:   cfg.VerifyModels,
			Majority: cfg.VerifyMajority,
			Timeout:  cfg.VerifyTimeout,
		}, nil, content)
	}

	// Telegram client (library-backed; real endpoint unless tests override).
	// BOT_UPDATE_WORKERS (default 16, clamped 1..128) replaces the library's
	// single update worker — without it ONE slow handler parks the whole
	// update stream (measured: a 66s AI chain blocked every other update).
	// AllowedUpdates (amend14): go-telegram/bot only sends allowed_updates
	// when configured, and the Bot API DEFAULT (field omitted / empty list)
	// EXCLUDES chat_member — so the amend11 B.1 leave-cleanup (清 pending +
	// 删欢迎 + 零处置 when anyone removes a pending joiner) never fired in
	// production. my_chat_member is in the default set but is listed
	// explicitly so the contract doesn't depend on the API default. The
	// explicit list keeps the previous message/edited_message delivery intact.
	tg, err := telegram.NewClientWithOptions(cfg.Token, telegram.Options{
		UpdateWorkers:  cfg.UpdateWorkers,
		AllowedUpdates: telegram.RequiredUpdates(),
	})
	if err != nil {
		log.Error("telegram_init_error", "err", err.Error())
		os.Exit(1)
	}

	b := bot.New(st, tg, chain, verify, cfg, log.WithComponent("bot"))

	// Async judgement pool (amend6 A): update handlers return in milliseconds;
	// AI chains run on pool chain runners (same-uid serial, cross-uid
	// concurrent, bounded by BOT_JUDGE_QUEUE + BOT_AI_CONCURRENCY config,
	// whole-chain budget BOT_AI_JUDGE_TIMEOUT). BOT_JUDGE_ASYNC=false is the
	// one-line rollback to the legacy inline behavior.
	judgePoolCtx, stopJudgePool := context.WithCancel(context.Background())
	b.StartJudgePool(judgePoolCtx)

	// amend10: the join-verification deadline scanner (single ticker, dies
	// with the process). No-op when BOT_JOIN_VERIFY=false.
	b.StartJoinVerify()

	// Notification subsystem (amend4): async bounded queue + worker; the
	// sender is the plain-text NotifySender (no parse_mode, preview off).
	notifier := notify.New(notify.Config{
		LogChat:      cfg.LogChat,
		OnPunish:     cfg.NotifyOnPunish,
		OnError:      cfg.NotifyOnError,
		Level:        logx.ParseLevel(cfg.NotifyLevel),
		RatePerMin:   cfg.NotifyRatePerMin,
		QueueSize:    cfg.NotifyQueue,
		DedupeWindow: cfg.NotifyDedupeWindow,
		MaxLen:       cfg.NotifyMaxLen,
		TextLen:      cfg.NotifyTextLen,
		IncludeText:  cfg.NotifyIncludeText,
		Drain:        cfg.NotifyDrain,
	}, tg, log.WithComponent("notify"))
	b.Notifier = notifier
	notifier.Start(context.Background())

	// Kick announcement (amend7, amend8 merge): after a successful disposal
	// the bot posts a 1–2 sentence AI-generated notice into the same group
	// (own goroutine per disposal — never a judgment-pool slot, never blocking
	// the punishment chain; AI failure falls back to the local template pool;
	// dry-run sends nothing; one announcement per chat+uid+messageID). The
	// send rides the amend8 unified TTL path (BOT_MSG_TTL): the announcement
	// self-deletes like every other bot chat message. Wired after the notifier
	// so send failures reach the existing error-push channel.
	primary.SetAnnounceTimeout(cfg.AnnounceTimeout)
	b.Announcer = bot.NewAnnouncer(tg, primary, cfg.AnnounceKickEnabled,
		cfg.AnnounceTimeout, cfg.JudgeDrain,
		log.WithComponent("announce"), notifier, b)

	// Startup self-check (§4): getChat + getChatMember(bot) on the target;
	// failure WARNS (never blocks) and surfaces as notify.target_ok=false.
	checkNotifyTarget(cfg, tg, notifier, log)

	// Background retry queue: failed AI judgments re-run every 10s (fixed),
	// max 5 rounds, then the local rule engine concludes. The janitor state is
	// shared with /statusz.
	sweepState := &SweepState{}
	retryQ := retryq.New(b.RetryRound, cfg.RetryInterval, cfg.RetryMax, cfg.RetryQueue, log.WithComponent("retryq"))
	b.RetryQ = retryQ
	retryCtx, stopRetry := context.WithCancel(context.Background())
	retryQ.Start(retryCtx)

	// Janitor goroutine (TTL sweep + capacity caps), stop on shutdown.
	janitorStop := startJanitor(b, cfg, log.WithComponent("janitor"), sweepState)

	// HTTP server (port 3000 for belmo health checks).
	srv := server.New(st, b, sweepState, cfg, log.WithComponent("http"))
	httpSrv := &http.Server{Addr: cfg.BotAddr, Handler: srv.Handler()}

	// Register commands with Telegram + dispatch long-poll via the library.
	if err := registerCommands(tg, log); err != nil {
		log.Warn("set_my_commands_error", "err", err.Error())
	}
	pollCtx, stopPoll := context.WithCancel(context.Background())
	go runPoller(pollCtx, tg, cfg, b, log.WithComponent("poller"))

	// Graceful shutdown: SIGINT/SIGTERM → stop poll, stop retry worker, stop
	// janitor, drain the notifier, close telegram, shutdown http.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("shutdown_signal")
		stopPoll()
		stopJudgePool()
		b.StopJudgePool()  // drains up to BOT_JUDGE_DRAIN, logs judge_pool_stopped
		b.StopJoinVerify() // closes the amend10 deadline scan ticker
		b.StopAnnouncer()  // drains in-flight announcements (BOT_JUDGE_DRAIN magnitude)
		b.StopMsgTTL()     // drains in-flight TTL deletions (BOT_JUDGE_DRAIN magnitude)
		stopRetry()
		retryQ.Stop()
		janitorStop()
		notifier.Stop() // drains up to BOT_NOTIFY_DRAIN
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

// checkNotifyTarget runs the §4 startup self-check: the target chat must be
// reachable (getChat) and the bot must be able to post (getChatMember status
// member/administrator/creator). Failure WARNS and flips notify.target_ok to
// false — it never blocks startup. A successful first SendMessage later
// re-flips the flag (self-healing).
func checkNotifyTarget(cfg config.Config, tg *telegram.Client, n *notify.Notifier, log *logx.Logger) {
	if cfg.LogChat == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fail := func(event, detail string) {
		log.Warn(event, "chat", cfg.LogChat, "err", detail,
			"summary", "通知目标不可用：处置/报错推送可能全部失败；notify.target_ok=false")
		n.SetTargetOK(false)
	}
	if _, err := tg.GetChat(ctx, cfg.LogChat); err != nil {
		fail("notify_target_unreachable", err.Error())
		return
	}
	me, err := tg.GetMe(ctx)
	if err != nil {
		fail("notify_target_getme_error", err.Error())
		return
	}
	member, err := tg.GetChatMember(ctx, cfg.LogChat, me.ID)
	if err != nil {
		fail("notify_target_member_error", err.Error())
		return
	}
	switch member.Status {
	case "member", "administrator", "creator":
		n.SetTargetOK(true)
		log.Info("notify_target_ok", "chat", cfg.LogChat, "bot_status", member.Status)
	default:
		// left/kicked/restricted: the bot cannot post.
		fail("notify_target_no_permission", "bot status in target chat: "+member.Status)
	}
}

// promptEvalModels resolves which models -prompteval evaluates.
func promptEvalModels(cfg config.Config, p *promptEvalFlags) []string {
	if p.models != "" {
		var out []string
		for _, m := range strings.Split(p.models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return cfg.AIEffectiveModels()
}

// providerChain routes classification through the primary provider's model
// chain, then the fallback provider's chain when configured
// (BOT_AI_FALLBACK_*). It implements bot.AIClassifier.
type providerChain struct {
	primary *ai.Client
	cfg     config.Config
	log     *logx.Logger
}

func (c providerChain) Classify(ctx context.Context, content ai.Content, mode string) (ai.ClassifyResult, bool) {
	res, ok := c.primary.Classify(ctx, content, mode)
	if ok {
		return res, true
	}
	// Fallback provider (optional): same client shape, its own model chain.
	if c.cfg.FallbackBaseURL != "" && c.cfg.FallbackAPIKey != "" {
		models := c.cfg.FallbackModels
		if len(models) == 0 {
			models = c.primary.Models()
		}
		fb := ai.NewClient(ai.ClientConfig{
			BaseURL: c.cfg.FallbackBaseURL,
			APIKey:  c.cfg.FallbackAPIKey,
			Models:  models,
			Timeout: c.cfg.AITimeout,
			Logger:  c.log,
		})
		c.log.Warn("ai_fallback_provider", "base_url", c.cfg.FallbackBaseURL)
		res2, ok2 := fb.Classify(ctx, content, mode)
		if ok2 {
			res2.Model = "fallback:" + res2.Model
			return res2, true
		}
	}
	return res, false
}

// openStore wires memory (default) or explicitly-configured redis backends.
// The file backend (BOT_STORE=file) is deliberately NOT implemented (belmo
// has no writable filesystem; memory is the final choice): it WARNS and falls
// back to memory so the process still starts safely.
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
		// Explicit non-implementation (design decision, not a bug): memory is
		// the final choice; restart loses watchlist/signatures/stats.
		log.Warn("file_store_unimplemented", "summary", "BOT_STORE=file 后端明确不实现（内存模式是最终选择），已回落到内存模式；重启会丢失观察名单/签名库/统计")
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

// runPoller drives the library's long-poll loop and forwards updates to the
// bot. Network errors are retried by the library with backoff (it never
// exits on transient errors); the loop exits on ctx cancel.
func runPoller(ctx context.Context, tg *telegram.Client, cfg config.Config, b *bot.Bot, log *logx.Logger) {
	tg.RegisterDefaultHandler(func(ctx context.Context, u telegram.Update) {
		b.NotePoll()
		b.HandleUpdate(ctx, u)
	})
	tg.Start(ctx)
	log.Info("poll_stopped")
}
