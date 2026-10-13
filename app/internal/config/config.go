// Package config loads every knob of the antispam bot from environment
// variables (nothing is hardcoded in code beyond defaults). It also renders
// credential-redacted summaries for startup and selftest logs, and reports
// unrecognized BOT_* variables so a typo does not silently disable a feature.
//
// The environment-variable contract is the single authority: TASK_antispam_amend1.md
// §6 (minus the deletions the user requested — no BOT_DELETE_QPS, no tracked
// message id cap/retention, no observation-message id tracking — and no
// BOT_REPORT_*; the /report flow was removed in the amend5 round).
// See app/.env.example for the annotated list.
package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"adbot/internal/logx"
)

// Config is the full runtime configuration.
type Config struct {
	// Core -------------------------------------------------------------------
	Token   string
	BotAddr string
	// Chats is the authorized group whitelist (BOT_CHATS). Empty/absent means
	// no group is authorized; ChatsSet records whether the variable was present.
	Chats           map[int64]bool
	ChatsSet        bool
	LeaveOtherChats bool
	LeaveNotice     bool
	AdminUsers      map[int64]bool
	DryRun          bool

	// Behavior ----------------------------------------------------------------
	Action           string // "ban" or "kick"
	IgnoreAdmins     bool
	DeleteJoinMsg    bool
	LogChat          int64 // 0 = disabled
	CmdAutodeleteSec int

	// Store & retention -------------------------------------------------------
	Store            string // memory (default) | redis | file | auto
	RedisURL         string
	RedisInsecureTLS bool
	DBPath           string
	DBFsync          bool

	Retention          time.Duration // 0 = never expire
	RetentionSuspect   time.Duration // 0 = follow Retention
	RetentionSignature time.Duration // 0 = follow Retention
	RetentionSliding   bool
	SweepInterval      time.Duration
	MaxSuspects        int
	MaxSignatures      int

	// AI ----------------------------------------------------------------------
	AIBaseURL          string
	AIKey              string
	AIModels           []string // model fallback chain (BOT_AI_MODELS)
	AIModel            string   // single-model compat (BOT_AI_MODEL)
	AIModelSet         bool     // BOT_AI_MODEL was explicitly set in the env
	AITimeout          time.Duration
	AIJudgeTimeout     time.Duration // whole-chain budget for ONE judgment (BOT_AI_JUDGE_TIMEOUT)
	AIReasoningEffort  string        // "none" default; overrides the thinking-off field
	AIPromptFile       string        // BOT_AI_PROMPT_FILE: override the embedded judge prompt
	RetryInterval      time.Duration
	RetryMax           int
	RetryQueue         int
	MinConfidence      float64
	VerifyVotes        int
	VerifyModels       []string
	VerifyMajority     int
	VerifyTimeout      time.Duration
	FallbackBaseURL    string
	FallbackAPIKey     string
	FallbackModels     []string
	SimhashDistance    int
	LocalRulesEnabled  bool
	LocalRuleThreshold int
	LocalKeywords      []string // appended keyword list (BOT_LOCAL_KEYWORDS)

	// Runtime / logging / admin ----------------------------------------------
	PollTimeout int
	Workers     int
	LogLevel    string
	LogFormat   string
	AdminToken  string
	Version     string

	// Judgement pool (amend6 A) -------------------------------------------------
	JudgeAsync       bool          // BOT_JUDGE_ASYNC: false = legacy inline judging
	JudgeQueue       int           // BOT_JUDGE_QUEUE: bounded submit queue
	JudgeConcurrency int           // BOT_AI_CONCURRENCY: pool workers
	JudgeDrain       time.Duration // BOT_JUDGE_DRAIN: graceful-shutdown budget
	UpdateWorkers    int           // BOT_UPDATE_WORKERS: library WithWorkers (1..128)

	// Notifications (amend4) ---------------------------------------------------
	NotifyOnPunish     bool          // BOT_NOTIFY_ON_PUNISH
	NotifyOnError      bool          // BOT_NOTIFY_ON_ERROR
	NotifyLevel        string        // BOT_NOTIFY_LEVEL: "error" | "warn"
	NotifyRatePerMin   int           // BOT_NOTIFY_RATE
	NotifyQueue        int           // BOT_NOTIFY_QUEUE
	NotifyDedupeWindow time.Duration // BOT_NOTIFY_DEDUPE_WINDOW
	NotifyMaxLen       int           // BOT_NOTIFY_MAX_LEN
	NotifyTextLen      int           // BOT_NOTIFY_TEXT_LEN
	NotifyIncludeText  bool          // BOT_NOTIFY_INCLUDE_TEXT
	NotifyDrain        time.Duration // BOT_NOTIFY_DRAIN
	AdminCacheTTL      time.Duration // BOT_ADMIN_CACHE_TTL
	AdminCacheMax      int           // BOT_ADMIN_CACHE_MAX
}

// Load reads the environment into a Config with defaults applied. It is pure
// (reads only os.LookupEnv) and never logs.
func Load() Config {
	var models []string
	if v := os.Getenv("BOT_AI_MODELS"); v != "" {
		models = csvList(v)
	}
	c := Config{
		BotAddr:         envOr("BOT_ADDR", ":3000"),
		LeaveOtherChats: boolEnv("BOT_LEAVE_OTHER_CHATS", true),
		LeaveNotice:     boolEnv("BOT_LEAVE_NOTICE", true),
		DryRun:          boolEnv("BOT_DRY_RUN", false),

		Action:           envOr("BOT_ACTION", "kick"),
		IgnoreAdmins:     boolEnv("BOT_IGNORE_ADMINS", true),
		DeleteJoinMsg:    boolEnv("BOT_DELETE_JOIN_MSG", false),
		CmdAutodeleteSec: intEnv("BOT_CMD_AUTODELETE_SEC", 0),

		Store:            envOr("BOT_STORE", "memory"),
		RedisURL:         os.Getenv("BOT_REDIS_URL"),
		RedisInsecureTLS: boolEnv("BOT_REDIS_INSECURE_TLS", false),
		DBPath:           envOr("BOT_DB_PATH", "data/adbot.db"),
		DBFsync:          boolEnv("BOT_DB_FSYNC", false),

		Retention:        durationEnv("BOT_RETENTION", 72*time.Hour),
		RetentionSliding: boolEnv("BOT_RETENTION_SLIDING", true),
		SweepInterval:    durationEnv("BOT_SWEEP_INTERVAL", 10*time.Minute),
		MaxSuspects:      intEnv("BOT_MAX_SUSPECTS", 50000),
		MaxSignatures:    intEnv("BOT_MAX_SIGNATURES", 50000),

		AIBaseURL:          envOr("BOT_AI_BASE_URL", "https://api.your-gateway.invalid/v1"),
		AIKey:              os.Getenv("BOT_AI_API_KEY"),
		AIModels:           models,
		AIModel:            envOr("BOT_AI_MODEL", "mimo-v2.6-pro-ultraspeed-studio"),
		AIModelSet:         os.Getenv("BOT_AI_MODEL") != "",
		AITimeout:          durationEnv("BOT_AI_TIMEOUT", 90*time.Second),
		AIJudgeTimeout:     durationEnv("BOT_AI_JUDGE_TIMEOUT", 150*time.Second),
		AIReasoningEffort:  envOr("BOT_AI_REASONING_EFFORT", "none"),
		AIPromptFile:       os.Getenv("BOT_AI_PROMPT_FILE"),
		RetryInterval:      durationEnv("BOT_AI_RETRY_INTERVAL", 10*time.Second),
		RetryMax:           intEnv("BOT_AI_RETRY_MAX_ATTEMPTS", 5),
		RetryQueue:         intEnv("BOT_AI_RETRY_QUEUE", 500),
		MinConfidence:      floatEnv("BOT_AI_MIN_CONFIDENCE", 0.85),
		VerifyVotes:        intEnv("BOT_AI_VERIFY_VOTES", 3),
		VerifyModels:       csvList(envOr("BOT_AI_VERIFY_MODELS", "mimo-v2.6-flash")),
		VerifyMajority:     intEnv("BOT_AI_VERIFY_MAJORITY", 2),
		VerifyTimeout:      durationEnv("BOT_AI_VERIFY_TIMEOUT", 45*time.Second), // flash measured up to 23.5s; 20s misfires
		FallbackBaseURL:    os.Getenv("BOT_AI_FALLBACK_BASE_URL"),
		FallbackAPIKey:     os.Getenv("BOT_AI_FALLBACK_API_KEY"),
		FallbackModels:     csvList(os.Getenv("BOT_AI_FALLBACK_MODELS")),
		SimhashDistance:    intEnv("BOT_SIMHASH_DISTANCE", 3),
		LocalRulesEnabled:  boolEnv("BOT_LOCAL_RULES_ENABLED", true),
		LocalRuleThreshold: intEnv("BOT_LOCAL_RULE_THRESHOLD", 3),
		LocalKeywords:      csvList(os.Getenv("BOT_LOCAL_KEYWORDS")),

		PollTimeout: intEnv("BOT_POLL_TIMEOUT", 30),
		Workers:     intEnv("BOT_WORKERS", 4),
		LogLevel:    envOr("BOT_LOG_LEVEL", "info"),
		LogFormat:   envOr("BOT_LOG_FORMAT", "text"),
		AdminToken:  os.Getenv("BOT_ADMIN_TOKEN"),
		Version:     envOr("BOT_VERSION", "dev"),

		// amend6 A: async judgement pool knobs.
		JudgeAsync:       boolEnv("BOT_JUDGE_ASYNC", true),
		JudgeQueue:       clampInt(intEnv("BOT_JUDGE_QUEUE", 500), 1, 100000),
		JudgeConcurrency: clampInt(intEnv("BOT_AI_CONCURRENCY", 8), 1, 256),
		JudgeDrain:       durationEnv("BOT_JUDGE_DRAIN", 10*time.Second),
		UpdateWorkers:    clampInt(intEnv("BOT_UPDATE_WORKERS", 16), 1, 128),

		NotifyOnPunish:     boolEnv("BOT_NOTIFY_ON_PUNISH", true),
		NotifyOnError:      boolEnv("BOT_NOTIFY_ON_ERROR", true),
		NotifyLevel:        envOr("BOT_NOTIFY_LEVEL", "error"),
		NotifyRatePerMin:   intEnv("BOT_NOTIFY_RATE", 20),
		NotifyQueue:        intEnv("BOT_NOTIFY_QUEUE", 200),
		NotifyDedupeWindow: durationEnv("BOT_NOTIFY_DEDUPE_WINDOW", 5*time.Minute),
		NotifyMaxLen:       intEnv("BOT_NOTIFY_MAX_LEN", 1024),
		NotifyTextLen:      intEnv("BOT_NOTIFY_TEXT_LEN", 120),
		NotifyIncludeText:  boolEnv("BOT_NOTIFY_INCLUDE_TEXT", true),
		NotifyDrain:        durationEnv("BOT_NOTIFY_DRAIN", 5*time.Second),
		AdminCacheTTL:      durationEnv("BOT_ADMIN_CACHE_TTL", 5*time.Minute),
		AdminCacheMax:      intEnv("BOT_ADMIN_CACHE_MAX", 5000),
	}
	c.Token = os.Getenv("BOT_TOKEN")
	c.ChatsSet = os.Getenv("BOT_CHATS") != ""
	c.Chats = parseIntSet(os.Getenv("BOT_CHATS"))
	c.AdminUsers = parseIntSet(os.Getenv("BOT_ADMIN_USERS"))
	if v := os.Getenv("BOT_LOG_CHAT"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			c.LogChat = n
		}
	}
	// Notify level must be "error" (default) or "warn"; anything else (with
	// the default "error" itself) stays "error" — fail-quiet over fail-noisy.
	if c.NotifyLevel != "warn" {
		c.NotifyLevel = "error"
	}
	// Per-category retention: empty/unparsable follows BOT_RETENTION.
	c.RetentionSuspect = optionalDuraenv("BOT_RETENTION_SUSPECT")
	c.RetentionSignature = optionalDuraenv("BOT_RETENTION_SIGNATURE")

	// The sweep interval may not be zero/negative (the janitor must run); an
	// explicit 0 or a negative value falls back to the default cadence.
	if c.SweepInterval <= 0 {
		c.SweepInterval = 10 * time.Minute
	}

	if c.Action != "ban" && c.Action != "kick" {
		c.Action = "ban"
	}
	return c
}

// DefaultAIModelChain is the canonical primary → fallback chain, matching
// .env.example and the README. It is the default when neither BOT_AI_MODELS
// nor BOT_AI_MODEL is set. amend6 B.3: ultraspeed-studio is LAST — it is
// currently hanging (120s read timeout), so it must never waste the head of
// the chain; BOT_AI_MODELS still overrides without a code change.
func DefaultAIModelChain() []string {
	return []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed-studio"}
}

// AIEffectiveModels returns the model fallback chain: BOT_AI_MODELS when set,
// else the explicitly-set legacy BOT_AI_MODEL, else DefaultAIModelChain.
//
// The last branch matters: a deployment that sets only BOT_AI_API_KEY /
// BOT_AI_BASE_URL (typical on a PaaS UI) must still get the full chain — a
// single-model chain silently disables fallback, so one upstream timeout
// becomes an outright failed judgment.
func (c Config) AIEffectiveModels() []string {
	if len(c.AIModels) > 0 {
		return c.AIModels
	}
	if c.AIModelSet && c.AIModel != "" {
		return []string{c.AIModel}
	}
	return DefaultAIModelChain()
}

// RetentionFor returns the TTL to apply to a record category ("suspect" or
// "signature"); a ≤0 duration means "follow the global BOT_RETENTION".
func (c Config) RetentionFor(category string) time.Duration {
	switch category {
	case "suspect":
		if d := c.RetentionSuspect; d > 0 {
			return d
		}
	case "signature":
		if d := c.RetentionSignature; d > 0 {
			return d
		}
	}
	return c.Retention
}

// AuthorizedChat reports whether chatID is inside the BOT_CHATS whitelist.
// An empty whitelist authorizes nothing (fail-safe), except that a DM to the
// bot may additionally be treated as authorized by the caller (it is).
func (c Config) AuthorizedChat(chatID int64) bool {
	return c.Chats[chatID]
}

// IsAdminUser reports whether uid is a configured admin (BOT_ADMIN_USERS).
func (c Config) IsAdminUser(uid int64) bool { return c.AdminUsers[uid] }

// Redacted renders a credential-safe summary (tokens/passwords → prefix only).
func (c Config) Redacted() map[string]string {
	return map[string]string{
		"BOT_ADDR":              c.BotAddr,
		"BOT_CHATS":             formatIDSet(c.Chats),
		"BOT_LEAVE_OTHER_CHATS": strconv.FormatBool(c.LeaveOtherChats),
		"BOT_LEAVE_NOTICE":      strconv.FormatBool(c.LeaveNotice),
		"BOT_ADMIN_USERS":       formatIDSet(c.AdminUsers),
		"BOT_DRY_RUN":           strconv.FormatBool(c.DryRun),
		"BOT_ACTION":            c.Action,
		"BOT_IGNORE_ADMINS":     strconv.FormatBool(c.IgnoreAdmins),
		"BOT_DELETE_JOIN_MSG":   strconv.FormatBool(c.DeleteJoinMsg),
		"BOT_LOG_CHAT":          strconv.FormatInt(c.LogChat, 10),
		// amend4 notification + admin-cache contract
		"BOT_NOTIFY_ON_PUNISH":      strconv.FormatBool(c.NotifyOnPunish),
		"BOT_NOTIFY_ON_ERROR":       strconv.FormatBool(c.NotifyOnError),
		"BOT_NOTIFY_LEVEL":          c.NotifyLevel,
		"BOT_NOTIFY_RATE":           strconv.Itoa(c.NotifyRatePerMin),
		"BOT_NOTIFY_QUEUE":          strconv.Itoa(c.NotifyQueue),
		"BOT_NOTIFY_DEDUPE_WINDOW":  c.NotifyDedupeWindow.String(),
		"BOT_NOTIFY_MAX_LEN":        strconv.Itoa(c.NotifyMaxLen),
		"BOT_NOTIFY_TEXT_LEN":       strconv.Itoa(c.NotifyTextLen),
		"BOT_NOTIFY_INCLUDE_TEXT":   strconv.FormatBool(c.NotifyIncludeText),
		"BOT_NOTIFY_DRAIN":          c.NotifyDrain.String(),
		"BOT_ADMIN_CACHE_TTL":       c.AdminCacheTTL.String(),
		"BOT_ADMIN_CACHE_MAX":       strconv.Itoa(c.AdminCacheMax),
		"BOT_CMD_AUTODELETE_SEC":    strconv.Itoa(c.CmdAutodeleteSec),
		"BOT_STORE":                 c.Store,
		"BOT_REDIS_URL":             redactURL(c.RedisURL),
		"BOT_REDIS_INSECURE_TLS":    strconv.FormatBool(c.RedisInsecureTLS),
		"BOT_DB_PATH":               c.DBPath,
		"BOT_DB_FSYNC":              strconv.FormatBool(c.DBFsync),
		"BOT_RETENTION":             c.Retention.String(),
		"BOT_RETENTION_SUSPECT":     fmtDura(c.RetentionSuspect),
		"BOT_RETENTION_SIGNATURE":   fmtDura(c.RetentionSignature),
		"BOT_RETENTION_SLIDING":     strconv.FormatBool(c.RetentionSliding),
		"BOT_SWEEP_INTERVAL":        c.SweepInterval.String(),
		"BOT_MAX_SUSPECTS":          strconv.Itoa(c.MaxSuspects),
		"BOT_MAX_SIGNATURES":        strconv.Itoa(c.MaxSignatures),
		"BOT_AI_BASE_URL":           c.AIBaseURL,
		"BOT_AI_API_KEY":            redactToken(c.AIKey),
		"BOT_AI_MODELS":             strings.Join(c.AIEffectiveModels(), ","),
		"BOT_AI_TIMEOUT":            c.AITimeout.String(),
		"BOT_AI_JUDGE_TIMEOUT":      c.AIJudgeTimeout.String(),
		"BOT_AI_REASONING_EFFORT":   c.AIReasoningEffort,
		"BOT_AI_PROMPT_FILE":        c.AIPromptFile,
		"BOT_AI_RETRY_INTERVAL":     c.RetryInterval.String(),
		"BOT_AI_RETRY_MAX_ATTEMPTS": strconv.Itoa(c.RetryMax),
		"BOT_AI_RETRY_QUEUE":        strconv.Itoa(c.RetryQueue),
		"BOT_AI_MIN_CONFIDENCE":     strconv.FormatFloat(c.MinConfidence, 'f', 2, 64),
		"BOT_AI_VERIFY_VOTES":       strconv.Itoa(c.VerifyVotes),
		"BOT_AI_VERIFY_MODELS":      strings.Join(c.VerifyModels, ","),
		"BOT_AI_VERIFY_MAJORITY":    strconv.Itoa(c.VerifyMajority),
		"BOT_AI_VERIFY_TIMEOUT":     c.VerifyTimeout.String(),
		"BOT_AI_FALLBACK_BASE_URL":  c.FallbackBaseURL,
		"BOT_AI_FALLBACK_API_KEY":   redactToken(c.FallbackAPIKey),
		"BOT_AI_FALLBACK_MODELS":    strings.Join(c.FallbackModels, ","),
		"BOT_SIMHASH_DISTANCE":      strconv.Itoa(c.SimhashDistance),
		"BOT_LOCAL_RULES_ENABLED":   strconv.FormatBool(c.LocalRulesEnabled),
		"BOT_LOCAL_RULE_THRESHOLD":  strconv.Itoa(c.LocalRuleThreshold),
		"BOT_LOCAL_KEYWORDS":        strings.Join(c.LocalKeywords, ","),
		"BOT_POLL_TIMEOUT":          strconv.Itoa(c.PollTimeout),
		"BOT_WORKERS":               strconv.Itoa(c.Workers),
		// amend6 A: judgement-pool contract
		"BOT_JUDGE_ASYNC":    strconv.FormatBool(c.JudgeAsync),
		"BOT_JUDGE_QUEUE":    strconv.Itoa(c.JudgeQueue),
		"BOT_AI_CONCURRENCY": strconv.Itoa(c.JudgeConcurrency),
		"BOT_JUDGE_DRAIN":    c.JudgeDrain.String(),
		"BOT_UPDATE_WORKERS": strconv.Itoa(c.UpdateWorkers),
		"BOT_LOG_LEVEL":      c.LogLevel,
		"BOT_LOG_FORMAT":     c.LogFormat,
		"BOT_ADMIN_TOKEN":    redactToken(c.AdminToken),
		"BOT_TOKEN":          redactToken(c.Token),
	}
}

// SummaryLogger returns a logger for the given config.
func (c Config) Logger() *logx.Logger {
	return logx.New(os.Stderr, logx.ParseLevel(c.LogLevel), c.LogFormat, "adbot")
}

// Validate reports a human-readable error if the config is unusable.
func (c Config) Validate() error {
	if c.Token == "" && os.Getenv("BOT_TOKEN") == "" {
		return fmt.Errorf("BOT_TOKEN is required")
	}
	if c.MaxSuspects < 1 {
		return fmt.Errorf("BOT_MAX_SUSPECTS must be >= 1")
	}
	if c.MaxSignatures < 1 {
		return fmt.Errorf("BOT_MAX_SIGNATURES must be >= 1")
	}
	if c.SweepInterval <= 0 {
		return fmt.Errorf("BOT_SWEEP_INTERVAL must be > 0")
	}
	return nil
}

// AIBaseURLHost returns the host of the configured AI gateway (no scheme, no
// path, no credentials) for ops diagnostics — never exposes the API key.
// Empty when BOT_AI_BASE_URL is unset or unparseable.
func (c Config) AIBaseURLHost() string {
	if strings.TrimSpace(c.AIBaseURL) == "" {
		return ""
	}
	u, err := url.Parse(c.AIBaseURL)
	if err != nil || u.Host == "" {
		return "<invalid>"
	}
	return u.Host
}

// AIBaseURLConfigured reports whether BOT_AI_BASE_URL holds a real endpoint
// (i.e. is set and not the shipped placeholder).
func (c Config) AIBaseURLConfigured() bool {
	h := strings.TrimSpace(c.AIBaseURL)
	return h != "" && !strings.Contains(h, ".invalid")
}

// UnrecognizedBotVars returns the BOT_* variables that were set but are not
// part of the recognized contract. Unknown prefixes are warned at startup so a
// typo (e.g. BOT_MAX_TRACKED) is loud instead of silently ignored.
func UnrecognizedBotVars() []string {
	known := map[string]bool{
		"BOT_TOKEN": true, "BOT_ADDR": true, "BOT_CHATS": true,
		"BOT_LEAVE_OTHER_CHATS": true, "BOT_LEAVE_NOTICE": true,
		"BOT_ADMIN_USERS": true, "BOT_DRY_RUN": true, "BOT_ACTION": true,
		"BOT_IGNORE_ADMINS": true, "BOT_DELETE_JOIN_MSG": true,
		"BOT_LOG_CHAT": true, "BOT_CMD_AUTODELETE_SEC": true,
		"BOT_STORE": true, "BOT_REDIS_URL": true,
		"BOT_REDIS_INSECURE_TLS": true, "BOT_DB_PATH": true,
		"BOT_DB_FSYNC": true, "BOT_RETENTION": true,
		"BOT_RETENTION_SUSPECT": true, "BOT_RETENTION_SIGNATURE": true,
		"BOT_RETENTION_SLIDING": true, "BOT_SWEEP_INTERVAL": true,
		"BOT_MAX_SUSPECTS": true, "BOT_MAX_SIGNATURES": true,
		"BOT_AI_BASE_URL": true, "BOT_AI_API_KEY": true,
		"BOT_AI_MODELS": true, "BOT_AI_MODEL": true, "BOT_AI_TIMEOUT": true,
		"BOT_AI_REASONING_EFFORT": true, "BOT_AI_PROMPT_FILE": true,
		"BOT_AI_RETRY_INTERVAL": true, "BOT_AI_RETRY_MAX_ATTEMPTS": true,
		"BOT_AI_RETRY_QUEUE": true, "BOT_AI_MIN_CONFIDENCE": true,
		"BOT_AI_VERIFY_VOTES": true, "BOT_AI_VERIFY_MODELS": true,
		"BOT_AI_VERIFY_MAJORITY": true, "BOT_AI_VERIFY_TIMEOUT": true,
		"BOT_AI_FALLBACK_BASE_URL": true, "BOT_AI_FALLBACK_API_KEY": true,
		"BOT_AI_FALLBACK_MODELS": true, "BOT_SIMHASH_DISTANCE": true,
		"BOT_LOCAL_RULES_ENABLED": true, "BOT_LOCAL_RULE_THRESHOLD": true,
		"BOT_LOCAL_KEYWORDS": true,
		"BOT_POLL_TIMEOUT":   true, "BOT_WORKERS": true,
		"BOT_LOG_LEVEL": true, "BOT_LOG_FORMAT": true,
		"BOT_ADMIN_TOKEN": true, "BOT_VERSION": true,
		// amend6 A (judge pool) + B (whole-chain budget)
		"BOT_JUDGE_ASYNC": true, "BOT_JUDGE_QUEUE": true,
		"BOT_AI_CONCURRENCY": true, "BOT_JUDGE_DRAIN": true,
		"BOT_UPDATE_WORKERS": true, "BOT_AI_JUDGE_TIMEOUT": true,
		// amend4 (notify + admin cache)
		"BOT_NOTIFY_ON_PUNISH": true, "BOT_NOTIFY_ON_ERROR": true,
		"BOT_NOTIFY_LEVEL": true, "BOT_NOTIFY_RATE": true,
		"BOT_NOTIFY_QUEUE": true, "BOT_NOTIFY_DEDUPE_WINDOW": true,
		"BOT_NOTIFY_MAX_LEN": true, "BOT_NOTIFY_TEXT_LEN": true,
		"BOT_NOTIFY_INCLUDE_TEXT": true, "BOT_NOTIFY_DRAIN": true,
		"BOT_ADMIN_CACHE_TTL": true, "BOT_ADMIN_CACHE_MAX": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := kv[:i]
		if !strings.HasPrefix(name, "BOT_") {
			continue
		}
		if known[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// clampInt bounds a value into [lo, hi] (amend6 A: worker/queue counts must
// never reach zero-or-negative or absurd values from a typo'd env var).
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func boolEnv(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func durationEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
		return d
	}
	return def
}

// optionalDurationEnv reads a duration-typed variable that may legitimately be
// absent or empty; it returns d (or 0) on any parse failure so callers know it
// was unset and can fall back to the global retention.
func optionalDuraenv(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return d
}

func floatEnv(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

// csvList splits a comma-separated list, trimming whitespace and dropping
// empty items.
func csvList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseIntSet parses a comma-separated list of int64 ids (e.g. "-1001234567890").
// Unparseable items are skipped; want all-or-nothing semantics for the warning
// is not required — the set is simply best-effort.
func parseIntSet(s string) map[int64]bool {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[int64]bool{}
	for _, part := range csvList(s) {
		if n, err := strconv.ParseInt(part, 10, 64); err == nil {
			out[n] = true
		}
	}
	return out
}

func formatIDSet(m map[int64]bool) string {
	if len(m) == 0 {
		return ""
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func fmtDura(d time.Duration) string {
	if d == 0 {
		return "" // follows BOT_RETENTION
	}
	return d.String()
}

// redactToken shows only the first 4 chars (or "set"/"unset").
func redactToken(t string) string {
	if t == "" {
		return "unset"
	}
	return "set:" + truncatePrefix(t, 4)
}

// redactURL strips any credentials from a URL, keeping scheme://host.
func redactURL(raw string) string {
	if raw == "" {
		return "unset"
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "@"); j >= 0 {
			rest = rest[j+1:]
		}
		return raw[:i+3] + rest
	}
	return "set:redacted"
}

func truncatePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
