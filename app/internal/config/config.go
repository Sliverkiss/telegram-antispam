// Package config loads every knob of the antispam bot from environment
// variables (nothing is hardcoded in code beyond defaults). It also renders
// credential-redacted summaries for startup and selftest logs.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"adbot/internal/logx"
)

// Config is the full runtime configuration.
type Config struct {
	Token     string
	BotAddr   string
	LogLevel  string
	LogFormat string

	RedisURL         string
	RedisInsecureTLS bool
	SuspectTTL       time.Duration
	SuspectTTLSet    bool

	Action         string // "ban" or "kick"
	IgnoreAdmins   bool
	DeleteJoinMsg  bool
	LogChat        int64 // 0 = disabled
	AdminToken     string
	MaxTrackedMsgs int

	AIBaseURL string
	AIKey     string
	AIModel   string
	AITimeout time.Duration

	PollTimeout int
	AllUpdates  string // canonical member-join update kinds
	Version     string
}

// Load reads the environment into a Config with defaults applied.
func Load() Config {
	c := Config{
		BotAddr:          envOr("BOT_ADDR", ":3000"),
		LogLevel:         envOr("BOT_LOG_LEVEL", "info"),
		LogFormat:        envOr("BOT_LOG_FORMAT", "text"),
		Action:           envOr("BOT_ACTION", "ban"),
		AIBaseURL:        envOr("BOT_AI_BASE_URL", "https://mimo.xn--ug8h.eu.org/v1"),
		AIModel:          envOr("BOT_AI_MODEL", "mimo-v2.6-pro-ultraspeed-studio"),
		PollTimeout:      intEnv("BOT_POLL_TIMEOUT", 25),
		DeleteJoinMsg:    boolEnv("BOT_DELETE_JOIN_MSG", false),
		IgnoreAdmins:     boolEnv("BOT_IGNORE_ADMINS", true),
		MaxTrackedMsgs:   intEnv("BOT_MAX_TRACKED_MSGS", 200),
		RedisInsecureTLS: boolEnv("BOT_REDIS_INSECURE_TLS", false),
		Version:          "0.1.0",
	}
	c.Token = os.Getenv("BOT_TOKEN")
	c.RedisURL = os.Getenv("BOT_REDIS_URL")
	c.AIKey = os.Getenv("BOT_AI_API_KEY")
	c.AdminToken = os.Getenv("BOT_ADMIN_TOKEN")
	if v := os.Getenv("BOT_LOG_CHAT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.LogChat = n
		}
	}
	c.AITimeout = durationEnv("BOT_AI_TIMEOUT", 30*time.Second)
	if v := os.Getenv("BOT_SUSPECT_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.SuspectTTL = d
			c.SuspectTTLSet = true
		} else if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.SuspectTTL = time.Duration(n) * time.Second
			c.SuspectTTLSet = true
		}
	}
	if c.Action != "ban" && c.Action != "kick" {
		c.Action = "ban"
	}
	return c
}

// Redacted renders a credential-safe summary (tokens/passwords → prefix only).
func (c Config) Redacted() map[string]string {
	return map[string]string{
		"BOT_ADDR":             c.BotAddr,
		"BOT_LOG_LEVEL":        c.LogLevel,
		"BOT_LOG_FORMAT":       c.LogFormat,
		"BOT_ACTION":           c.Action,
		"BOT_IGNORE_ADMINS":    strconv.FormatBool(c.IgnoreAdmins),
		"BOT_DELETE_JOIN_MSG":  strconv.FormatBool(c.DeleteJoinMsg),
		"BOT_LOG_CHAT":         strconv.FormatInt(c.LogChat, 10),
		"BOT_REDIS_URL":        redactURL(c.RedisURL),
		"BOT_SUSPECT_TTL":      c.SuspectTTL.String(),
		"BOT_AI_BASE_URL":      c.AIBaseURL,
		"BOT_AI_MODEL":         c.AIModel,
		"BOT_AI_TIMEOUT":       c.AITimeout.String(),
		"BOT_AI_API_KEY":       redactToken(c.AIKey),
		"BOT_ADMIN_TOKEN":      redactToken(c.AdminToken),
		"BOT_TOKEN":            redactToken(c.Token),
		"BOT_MAX_TRACKED_MSGS": strconv.Itoa(c.MaxTrackedMsgs),
	}
}

// SummaryLogger returns a logger for the given config.
func (c Config) Logger() *logx.Logger {
	return logx.New(os.Stderr, logx.ParseLevel(c.LogLevel), c.LogFormat, "adbot")
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
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
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
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
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
	// Strip userinfo manually to avoid leaking a password substring.
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

// Validate reports a human-readable error if the config is unusable.
func (c Config) Validate() error {
	if c.Token == "" && os.Getenv("BOT_TOKEN") == "" {
		return fmt.Errorf("BOT_TOKEN is required")
	}
	return nil
}
