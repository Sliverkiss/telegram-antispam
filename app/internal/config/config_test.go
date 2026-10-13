package config

import (
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("BOT_TOKEN", "")
	// Ensure no overriding env leaks in.
	for _, k := range []string{"BOT_ADDR", "BOT_ACTION", "BOT_REDIS_URL", "BOT_LOG_CHAT", "BOT_IGNORE_ADMINS", "BOT_MAX_TRACKED_MSGS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.BotAddr != ":3000" {
		t.Errorf("BotAddr = %q", c.BotAddr)
	}
	if c.Action != "ban" {
		t.Errorf("Action = %q", c.Action)
	}
	if !c.IgnoreAdmins {
		t.Error("IgnoreAdmins default should be true")
	}
	if c.MaxTrackedMsgs != 200 {
		t.Errorf("MaxTrackedMsgs = %d", c.MaxTrackedMsgs)
	}
	if c.AIBaseURL != "https://mimo.xn--ug8h.eu.org/v1" {
		t.Errorf("AIBaseURL = %q", c.AIBaseURL)
	}
}

func TestLoadEnv(t *testing.T) {
	t.Setenv("BOT_TOKEN", "123:abc")
	t.Setenv("BOT_ACTION", "kick")
	t.Setenv("BOT_LOG_CHAT", "-100500")
	t.Setenv("BOT_IGNORE_ADMINS", "false")
	t.Setenv("BOT_MAX_TRACKED_MSGS", "50")
	c := Load()
	if c.Token != "123:abc" {
		t.Errorf("Token = %q", c.Token)
	}
	if c.Action != "kick" {
		t.Errorf("Action = %q", c.Action)
	}
	if c.LogChat != -100500 {
		t.Errorf("LogChat = %d", c.LogChat)
	}
	if c.IgnoreAdmins {
		t.Error("IgnoreAdmins should be false")
	}
	if c.MaxTrackedMsgs != 50 {
		t.Errorf("MaxTrackedMsgs = %d", c.MaxTrackedMsgs)
	}
}

func TestRedactedNoSecrets(t *testing.T) {
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
