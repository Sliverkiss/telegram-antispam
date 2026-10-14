package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearBOT wipes every BOT_* variable so tests start from a clean slate.
func clearBOT(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.HasPrefix(kv[:i], "BOT_") {
			t.Setenv(kv[:i], "")
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearBOT(t)
	c := Load()
	if c.BotAddr != ":3000" {
		t.Errorf("BotAddr = %q", c.BotAddr)
	}
	if c.Action != "kick" {
		t.Errorf("Action default = %q, want kick", c.Action)
	}
	if c.Store != "memory" {
		t.Errorf("Store default = %q, want memory", c.Store)
	}
	if !c.IgnoreAdmins {
		t.Error("IgnoreAdmins default should be true")
	}
	if c.Retention != 72*time.Hour {
		t.Errorf("Retention default = %s, want 72h", c.Retention)
	}
	if c.RetentionSuspect != 0 || c.RetentionSignature != 0 {
		t.Errorf("per-category retention should default to 0 (follow global)")
	}
	if !c.RetentionSliding {
		t.Error("RetentionSliding default should be true")
	}
	if c.SweepInterval != 10*time.Minute {
		t.Errorf("SweepInterval = %s, want 10m", c.SweepInterval)
	}
	if c.MaxSuspects != 50000 || c.MaxSignatures != 50000 {
		t.Errorf("capacity guards = %d/%d, want 50000/50000", c.MaxSuspects, c.MaxSignatures)
	}
	if c.AIBaseURL != "https://api.your-gateway.invalid/v1" {
		t.Errorf("default AI base URL = %q", c.AIBaseURL)
	}
	if c.MinConfidence != 0.85 {
		t.Errorf("MinConfidence = %f", c.MinConfidence)
	}
	if len(c.AIEffectiveModels()) == 0 {
		t.Error("effective models must be non-empty")
	}
	if c.Version != "dev" {
		t.Errorf("Version = %q", c.Version)
	}
	if c.PollTimeout != 30 || c.Workers != 4 {
		t.Errorf("PollTimeout/Workers = %d/%d", c.PollTimeout, c.Workers)
	}
	if c.AITimeout != 90*time.Second {
		t.Errorf("AITimeout = %s", c.AITimeout)
	}
	// amend6 B: whole-chain judge budget + async pool knobs.
	if c.AIJudgeTimeout != 150*time.Second {
		t.Errorf("AIJudgeTimeout = %s", c.AIJudgeTimeout)
	}
	if !c.JudgeAsync {
		t.Error("JudgeAsync default should be true")
	}
	if c.JudgeQueue != 500 || c.JudgeConcurrency != 8 {
		t.Errorf("judge pool = %d/%d, want 500/8", c.JudgeQueue, c.JudgeConcurrency)
	}
	if c.JudgeDrain != 10*time.Second {
		t.Errorf("JudgeDrain = %s", c.JudgeDrain)
	}
	if c.UpdateWorkers != 16 {
		t.Errorf("UpdateWorkers = %d, want 16", c.UpdateWorkers)
	}
	if c.RetryInterval != 10*time.Second || c.RetryMax != 5 || c.RetryQueue != 500 {
		t.Errorf("retry = %s/%d/%d", c.RetryInterval, c.RetryMax, c.RetryQueue)
	}
	if c.VerifyVotes != 3 || c.VerifyMajority != 2 || c.VerifyTimeout != 45*time.Second {
		t.Errorf("verify = %d/%d/%s", c.VerifyVotes, c.VerifyMajority, c.VerifyTimeout)
	}
	if c.AIReasoningEffort != "none" {
		t.Errorf("AIReasoningEffort = %q, want none", c.AIReasoningEffort)
	}
	// amend5 B: BOT_REPORT_DAILY_LIMIT is gone from the contract entirely —
	// a leftover variable must surface as unrecognized.
	t.Setenv("BOT_REPORT_DAILY_LIMIT", "3")
	if got := UnrecognizedBotVars(); len(got) != 1 || got[0] != "BOT_REPORT_DAILY_LIMIT" {
		t.Errorf("BOT_REPORT_DAILY_LIMIT must be unrecognized after removal: %v", got)
	}
	if c.SimhashDistance != 3 {
		t.Errorf("SimhashDistance = %d", c.SimhashDistance)
	}
	if !c.LocalRulesEnabled || c.LocalRuleThreshold != 3 {
		t.Errorf("local rules = %v/%d", c.LocalRulesEnabled, c.LocalRuleThreshold)
	}
	if c.LeaveOtherChats != true || c.LeaveNotice != true {
		t.Errorf("leave defaults = %v/%v, want true/true", c.LeaveOtherChats, c.LeaveNotice)
	}
	// amend4: notify + admin-cache defaults.
	if c.NotifyOnPunish != true || c.NotifyOnError != true {
		t.Errorf("notify switches = %v/%v, want on/on", c.NotifyOnPunish, c.NotifyOnError)
	}
	if c.NotifyLevel != "error" {
		t.Errorf("NotifyLevel default = %q, want error", c.NotifyLevel)
	}
	if c.NotifyRatePerMin != 20 || c.NotifyQueue != 200 {
		t.Errorf("notify rate/queue = %d/%d", c.NotifyRatePerMin, c.NotifyQueue)
	}
	if c.NotifyDedupeWindow != 5*time.Minute {
		t.Errorf("NotifyDedupeWindow = %s", c.NotifyDedupeWindow)
	}
	if c.NotifyMaxLen != 1024 || c.NotifyTextLen != 120 || !c.NotifyIncludeText {
		t.Errorf("notify len = %d/%d/%v", c.NotifyMaxLen, c.NotifyTextLen, c.NotifyIncludeText)
	}
	if c.NotifyDrain != 5*time.Second {
		t.Errorf("NotifyDrain = %s", c.NotifyDrain)
	}
	if c.AdminCacheTTL != 5*time.Minute || c.AdminCacheMax != 5000 {
		t.Errorf("admin cache = %s/%d", c.AdminCacheTTL, c.AdminCacheMax)
	}
	// amend7: kick announcement defaults.
	if !c.AnnounceKickEnabled {
		t.Error("AnnounceKickEnabled default should be true")
	}
	if c.AnnounceTimeout != 8*time.Second {
		t.Errorf("AnnounceTimeout = %s, want 8s", c.AnnounceTimeout)
	}
	// amend8: the unified chat-message TTL defaults to 60s.
	if c.MsgTTL != 60*time.Second {
		t.Errorf("MsgTTL = %s, want 60s", c.MsgTTL)
	}
}

func TestAnnounceEnvOverrides(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_ANNOUNCE_KICK", "false")
	t.Setenv("BOT_ANNOUNCE_TIMEOUT", "90s") // judgment-scale typo → clamp to 20s
	c := Load()
	if c.AnnounceKickEnabled {
		t.Error("BOT_ANNOUNCE_KICK=false must disable")
	}
	if c.AnnounceTimeout != 20*time.Second {
		t.Errorf("AnnounceTimeout = %s, want clamped 20s", c.AnnounceTimeout)
	}
}

// amend8: BOT_MSG_TTL contract — 0 = never delete (survives verbatim), a
// runaway positive value clamps to 24h, and the absorbed legacy knobs
// (BOT_CMD_AUTODELETE_SEC / BOT_ANNOUNCE_TTL) are gone from the contract.
func TestMsgTTLEnvOverrides(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_MSG_TTL", "0")
	c := Load()
	if c.MsgTTL != 0 {
		t.Errorf("BOT_MSG_TTL=0 must mean never-delete, got %s", c.MsgTTL)
	}
	t.Setenv("BOT_MSG_TTL", "720h")
	c = Load()
	if c.MsgTTL != 24*time.Hour {
		t.Errorf("BOT_MSG_TTL=720h must clamp to 24h, got %s", c.MsgTTL)
	}
	t.Setenv("BOT_MSG_TTL", "5m")
	c = Load()
	if c.MsgTTL != 5*time.Minute {
		t.Errorf("BOT_MSG_TTL=5m should parse verbatim, got %s", c.MsgTTL)
	}
	// The absorbed knobs must surface as unrecognized (typo guard).
	t.Setenv("BOT_CMD_AUTODELETE_SEC", "30")
	t.Setenv("BOT_ANNOUNCE_TTL", "10m")
	got := UnrecognizedBotVars()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "BOT_CMD_AUTODELETE_SEC") || !strings.Contains(joined, "BOT_ANNOUNCE_TTL") {
		t.Errorf("absorbed knobs must be unrecognized after the amend8 merge: %v", got)
	}
}

func TestLoadEnv(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_TOKEN", "123:abc")
	t.Setenv("BOT_ACTION", "ban")
	t.Setenv("BOT_LOG_CHAT", "-100500")
	t.Setenv("BOT_IGNORE_ADMINS", "false")
	t.Setenv("BOT_DRY_RUN", "true")
	t.Setenv("BOT_STORE", "file")
	t.Setenv("BOT_RETENTION", "24h")
	t.Setenv("BOT_RETENTION_SUSPECT", "6h")
	c := Load()
	if c.Token != "123:abc" {
		t.Errorf("Token = %q", c.Token)
	}
	if c.Action != "ban" {
		t.Errorf("Action = %q", c.Action)
	}
	if c.LogChat != -100500 {
		t.Errorf("LogChat = %d", c.LogChat)
	}
	if c.IgnoreAdmins {
		t.Error("IgnoreAdmins should be false")
	}
	if !c.DryRun {
		t.Error("DryRun should be true")
	}
	if c.Store != "file" {
		t.Errorf("Store = %q", c.Store)
	}
	if c.Retention != 24*time.Hour {
		t.Errorf("Retention = %s", c.Retention)
	}
	if c.RetentionSuspect != 6*time.Hour {
		t.Errorf("RetentionSuspect = %s", c.RetentionSuspect)
	}
	if c.MsgTTL != 60*time.Second {
		t.Errorf("MsgTTL default = %s, want 60s", c.MsgTTL)
	}
	t.Setenv("BOT_NOTIFY_ON_PUNISH", "false")
	t.Setenv("BOT_NOTIFY_ON_ERROR", "false")
	t.Setenv("BOT_NOTIFY_LEVEL", "warn")
	t.Setenv("BOT_NOTIFY_RATE", "7")
	t.Setenv("BOT_NOTIFY_QUEUE", "13")
	t.Setenv("BOT_NOTIFY_DEDUPE_WINDOW", "90s")
	t.Setenv("BOT_NOTIFY_MAX_LEN", "512")
	t.Setenv("BOT_NOTIFY_TEXT_LEN", "64")
	t.Setenv("BOT_NOTIFY_INCLUDE_TEXT", "false")
	t.Setenv("BOT_NOTIFY_DRAIN", "2s")
	t.Setenv("BOT_ADMIN_CACHE_TTL", "1m")
	t.Setenv("BOT_ADMIN_CACHE_MAX", "64")
	c2 := Load()
	if c2.NotifyOnPunish || c2.NotifyOnError {
		t.Errorf("notify switches = %v/%v, want off/off", c2.NotifyOnPunish, c2.NotifyOnError)
	}
	if c2.NotifyLevel != "warn" {
		t.Errorf("NotifyLevel = %q", c2.NotifyLevel)
	}
	if c2.NotifyRatePerMin != 7 || c2.NotifyQueue != 13 {
		t.Errorf("notify rate/queue = %d/%d", c2.NotifyRatePerMin, c2.NotifyQueue)
	}
	if c2.NotifyDedupeWindow != 90*time.Second {
		t.Errorf("NotifyDedupeWindow = %s", c2.NotifyDedupeWindow)
	}
	if c2.NotifyMaxLen != 512 || c2.NotifyTextLen != 64 || c2.NotifyIncludeText {
		t.Errorf("notify len = %d/%d/%v", c2.NotifyMaxLen, c2.NotifyTextLen, c2.NotifyIncludeText)
	}
	if c2.NotifyDrain != 2*time.Second {
		t.Errorf("NotifyDrain = %s", c2.NotifyDrain)
	}
	if c2.AdminCacheTTL != time.Minute || c2.AdminCacheMax != 64 {
		t.Errorf("admin cache = %s/%d", c2.AdminCacheTTL, c2.AdminCacheMax)
	}
}

func TestAIBaseURLDiagnostics(t *testing.T) {
	clearBOT(t)
	c := Load()
	if c.AIBaseURLConfigured() {
		t.Error("shipped placeholder must not count as configured")
	}
	if got := c.AIBaseURLHost(); got != "api.your-gateway.invalid" {
		t.Errorf("placeholder host = %q", got)
	}

	t.Setenv("BOT_AI_BASE_URL", "https://gw.example.com/v1")
	c = Load()
	if !c.AIBaseURLConfigured() {
		t.Error("real endpoint must count as configured")
	}
	if got := c.AIBaseURLHost(); got != "gw.example.com" {
		t.Errorf("configured host = %q", got)
	}

	t.Setenv("BOT_AI_BASE_URL", "not a url")
	c = Load()
	if got := c.AIBaseURLHost(); got != "<invalid>" {
		t.Errorf("invalid host = %q", got)
	}
	if !c.AIBaseURLConfigured() {
		t.Error("non-placeholder junk still counts as configured (WARN is separate)")
	}

	t.Setenv("BOT_AI_BASE_URL", "")
	c = Load()
	if c.AIBaseURLConfigured() {
		t.Error("empty must not count as configured")
	}
}

func TestChatsNegativeIDsAndList(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_CHATS", "-1001234567890,-100200300,")
	t.Setenv("BOT_ADMIN_USERS", "123456789,100")
	c := Load()
	if !c.ChatsSet {
		t.Error("ChatsSet should be true")
	}
	if !c.Chats[-1001234567890] {
		t.Error("-1001234567890 should be in the whitelist")
	}
	if c.Chats[-100200300] != true {
		t.Error("-100200300 should be in the whitelist")
	}
	if len(c.Chats) != 2 {
		t.Errorf("Chats = %v", c.Chats)
	}
	if !c.AdminUsers[123456789] {
		t.Error("admin user should be parsed")
	}
	if c.AuthorizedChat(-1001234567890) != true {
		t.Error("AuthorizedChat should agree with whitelist")
	}
	if c.AuthorizedChat(-999999) != false {
		t.Error("unknown chat must not be authorized")
	}
}

func TestChatsEmptyMeansNoAuthorization(t *testing.T) {
	clearBOT(t)
	c := Load()
	if c.ChatsSet {
		t.Error("ChatsSet should be false when BOT_CHATS unset")
	}
	if c.AuthorizedChat(-1001234567890) {
		t.Error("empty whitelist must authorize nothing")
	}
}

func TestAIModelsFallbackChain(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_AI_MODELS", "m1,m2,m3")
	t.Setenv("BOT_AI_MODEL", "single")
	c := Load()
	got := c.AIEffectiveModels()
	if strings.Join(got, ",") != "m1,m2,m3" {
		t.Errorf("effective models = %v, want BOT_AI_MODELS to win", got)
	}
}

func TestAIModelCompatSingle(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_AI_MODEL", "only-model")
	c := Load()
	got := c.AIEffectiveModels()
	if len(got) != 1 || got[0] != "only-model" {
		t.Errorf("effective models = %v", got)
	}
}

// Regression: a deployment that sets only BOT_AI_BASE_URL / BOT_AI_API_KEY
// (typical on a PaaS) must still get the full three-model fallback chain.
// Before the fix, AIEffectiveModels fell through to the single default model
// id, silently disabling fallback so one upstream timeout killed the
// judgment outright.
// amend6 B.3: the default chain is exactly
// pro → flash → ultraspeed-studio with pro first (the hanging ultraspeed
// model moved to the tail), and BOT_AI_MODELS still overrides it verbatim.
func TestDefaultChainOrderProFirst(t *testing.T) {
	clearBOT(t)
	c := Load()
	got := c.AIEffectiveModels()
	want := []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed-studio"}
	if len(got) != len(want) {
		t.Fatalf("default chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default chain = %v, want %v (pro must be first)", got, want)
		}
	}
	// The four shipped surfaces agree (config / .env.example /
	// docker-compose.yml / README).
	if d := DefaultAIModelChain(); strings.Join(d, ",") != strings.Join(want, ",") {
		t.Errorf("DefaultAIModelChain = %v, want %v", d, want)
	}
	envChain, err := os.ReadFile(".env.example")
	if err == nil {
		line := ""
		for _, l := range strings.Split(string(envChain), "\n") {
			if strings.HasPrefix(l, "BOT_AI_MODELS=") {
				line = strings.TrimPrefix(l, "BOT_AI_MODELS=")
				break
			}
		}
		if line != strings.Join(want, ",") {
			t.Errorf(".env.example BOT_AI_MODELS = %q, want %q", line, strings.Join(want, ","))
		}
	}
	// BOT_AI_MODELS set → it wins.
	t.Setenv("BOT_AI_MODELS", "x-model,y-model")
	c2 := Load()
	if strings.Join(c2.AIEffectiveModels(), ",") != "x-model,y-model" {
		t.Errorf("override = %v, want BOT_AI_MODELS to win", c2.AIEffectiveModels())
	}
}

func TestAIEffectiveModelsDefaultsToFullChain(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_AI_BASE_URL", "https://gw.example.com/v1")
	t.Setenv("BOT_AI_API_KEY", "k")
	c := Load()
	got := c.AIEffectiveModels()
	want := strings.Join(DefaultAIModelChain(), ",")
	if strings.Join(got, ",") != want {
		t.Fatalf("default chain = %v, want %v (full fallback chain)", got, want)
	}
	if len(got) < 2 {
		t.Fatalf("chain must contain fallbacks, got %v", got)
	}
	// The chain must not depend on BOT_AI_MODEL's own default value leaking in.
	for _, m := range got {
		if m == "" {
			t.Fatalf("empty model id in chain %v", got)
		}
	}
}

func TestRetentionForFollowsGlobal(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_RETENTION", "48h")
	t.Setenv("BOT_RETENTION_SIGNATURE", "72h")
	c := Load()
	if got := c.RetentionFor("suspect"); got != 48*time.Hour {
		t.Errorf("suspect retention = %s, want 48h", got)
	}
	if got := c.RetentionFor("signature"); got != 72*time.Hour {
		t.Errorf("signature retention = %s, want 72h (per-category)", got)
	}
	if got := c.RetentionFor("unknown"); got != 48*time.Hour {
		t.Errorf("unknown category = %s, want global 48h", got)
	}
}

func TestInvalidDurationsFallBack(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_RETENTION", "not-a-duration")
	t.Setenv("BOT_SWEEP_INTERVAL", "0")
	c := Load()
	if c.Retention != 72*time.Hour {
		t.Errorf("bad retention should fall back to default: %s", c.Retention)
	}
	if c.SweepInterval != 10*time.Minute {
		t.Errorf("bad sweep interval should fall back to default: %s", c.SweepInterval)
	}
}

func TestUnrecognizedBotVars(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_MAX_TRACKED_MSGS", "200")
	t.Setenv("BOT_TYPO_VAR", "1")
	got := UnrecognizedBotVars()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "BOT_MAX_TRACKED_MSGS") {
		t.Errorf("unrecognized = %v, want BOT_MAX_TRACKED_MSGS", got)
	}
	if !strings.Contains(joined, "BOT_TYPO_VAR") {
		t.Errorf("unrecognized = %v, want BOT_TYPO_VAR", got)
	}
	if strings.Contains(joined, "BOT_TOKEN") || strings.Contains(joined, "BOT_ACTION") {
		t.Errorf("recognized vars should not be reported: %v", got)
	}
}

func TestUnrecognizedBotVarsEmpty(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_TOKEN", "123:abc")
	if got := UnrecognizedBotVars(); len(got) != 0 {
		t.Errorf("unexpected unrecognized vars: %v", got)
	}
}

func TestRedactedNoSecrets(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_TOKEN", "123456:AAHsecret")
	t.Setenv("BOT_AI_API_KEY", "sk-supersecret")
	t.Setenv("BOT_REDIS_URL", "rediss://default:mypassword@host:6379")
	c := Load()
	r := c.Redacted()
	for _, v := range r {
		if strings.Contains(v, "secret") || strings.Contains(v, "mypassword") {
			t.Errorf("redacted summary leaked a secret: %q", v)
		}
	}
}

func TestRedactURLKeepsHost(t *testing.T) {
	if got := redactURL("rediss://:pw@redis.example.com:6379"); strings.Contains(got, "pw") {
		t.Errorf("redactURL leaked password: %q", got)
	}
}

func TestValidateRequiresToken(t *testing.T) {
	clearBOT(t)
	c := Load()
	if err := c.Validate(); err == nil {
		t.Error("Validate should require BOT_TOKEN")
	}
	t.Setenv("BOT_TOKEN", "1:x")
	if err := Load().Validate(); err != nil {
		t.Errorf("Validate should pass with token: %v", err)
	}
}

func TestValidateCapacityGuards(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_TOKEN", "1:x")
	t.Setenv("BOT_MAX_SUSPECTS", "0")
	if err := Load().Validate(); err == nil {
		t.Error("BOT_MAX_SUSPECTS=0 should fail validation")
	}
}
