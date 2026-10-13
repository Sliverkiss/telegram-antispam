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
	if c.AIBaseURL != "https://mimo.xn--ug8h.eu.org/v1" {
		t.Errorf("AIBaseURL = %q", c.AIBaseURL)
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
	if c.AITimeout != 30*time.Second {
		t.Errorf("AITimeout = %s", c.AITimeout)
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
	if c.ReportDailyLimit != 3 {
		t.Errorf("ReportDailyLimit = %d, want 3", c.ReportDailyLimit)
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
	t.Setenv("BOT_CMD_AUTODELETE_SEC", "10")
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
	if c.CmdAutodeleteSec != 10 {
		t.Errorf("CmdAutodeleteSec = %d", c.CmdAutodeleteSec)
	}
}

func TestChatsNegativeIDsAndList(t *testing.T) {
	clearBOT(t)
	t.Setenv("BOT_CHATS", "-1003888828164,-100200300,")
	t.Setenv("BOT_ADMIN_USERS", "5232284790,100")
	c := Load()
	if !c.ChatsSet {
		t.Error("ChatsSet should be true")
	}
	if !c.Chats[-1003888828164] {
		t.Error("-1003888828164 should be in the whitelist")
	}
	if c.Chats[-100200300] != true {
		t.Error("-100200300 should be in the whitelist")
	}
	if len(c.Chats) != 2 {
		t.Errorf("Chats = %v", c.Chats)
	}
	if !c.AdminUsers[5232284790] {
		t.Error("admin user should be parsed")
	}
	if c.AuthorizedChat(-1003888828164) != true {
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
	if c.AuthorizedChat(-1003888828164) {
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
