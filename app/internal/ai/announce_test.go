package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// decodeJSONBody decodes a request body into a map (no error return; a
// malformed body just yields a nil map).
func decodeJSONBody(r *http.Request, out *map[string]any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

// announceGateway is an httptest fake gateway that answers a fixed assistant
// content and records request bodies (per model counts).
type announceGateway struct {
	mu     sync.Mutex
	bodies []map[string]any
	reply  string // assistant content
	status int    // non-200 → error body
	delay  time.Duration
}

func (g *announceGateway) handler(w http.ResponseWriter, r *http.Request) {
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	var body map[string]any
	if err := decodeJSONBody(r, &body); err == nil {
		g.mu.Lock()
		g.bodies = append(g.bodies, body)
		g.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	if g.status != 0 {
		w.WriteHeader(g.status)
		fmt.Fprintf(w, `{"error":{"message":"boom","code":"kaboom"}}`)
		return
	}
	fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%s}}]}`, mustQuote(g.reply))
}

func (g *announceGateway) lastBody() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return nil
	}
	return g.bodies[len(g.bodies)-1]
}

func (g *announceGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.bodies)
}

func newAnnounceClient(url string, timeout time.Duration) *Client {
	c := NewClient(ClientConfig{
		BaseURL: url,
		APIKey:  "k",
		Models:  []string{"m1", "m2"},
		Timeout: timeout,
	})
	return c
}

// --- Request shape + model chain walk ----------------------------------------

func TestAnnounceRequestShapeThinkingOffNoMaxTokens(t *testing.T) {
	g := &announceGateway{reply: "🚪 spammer 已送出门外，广告请去别处发～"}
	srv := httptest.NewServer(http.HandlerFunc(g.handler))
	defer srv.Close()
	c := newAnnounceClient(srv.URL, 2*time.Second)

	if _, err := c.Announce(context.Background(), "spammer", "办证"); err != nil {
		t.Fatalf("Announce failed: %v", err)
	}
	body := g.lastBody()
	if body["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v, want none", body["reasoning_effort"])
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs == nil || kwargs["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v, want {enable_thinking:false}", body["chat_template_kwargs"])
	}
	if _, bad := body["max_tokens"]; bad {
		t.Error("max_tokens must never be sent")
	}
	// The user message must carry the delimited subject (injection guard).
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "送客") {
		t.Errorf("system message = %v", sys)
	}
	uc, _ := user["content"].(string)
	if !strings.Contains(uc, "<subject>\nspammer\n</subject>") {
		t.Errorf("user message missing delimited subject: %q", uc)
	}
	if !strings.Contains(uc, "<category>\n办证\n</category>") {
		t.Errorf("user message missing delimited category: %q", uc)
	}
}

func TestAnnounceFallsThroughModelChain(t *testing.T) {
	// First model 500s, second succeeds → chain walk, one usable text.
	g := &announceGateway{reply: "🧹 bot已请出群，慢走不送"}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":"kaboom"}`)
			return
		}
		g.handler(w, r)
	}))
	defer srv.Close()
	c := newAnnounceClient(srv.URL, 2*time.Second)

	out, err := c.Announce(context.Background(), "bot", "")
	if err != nil {
		t.Fatalf("Announce should succeed via the second model: %v", err)
	}
	if !strings.Contains(out, "bot") {
		t.Fatalf("output must contain the subject: %q", out)
	}
	if calls != 2 {
		t.Fatalf("chain must walk exactly 2 models, got %d", calls)
	}
}

func TestAnnounceWholeChainFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"kaboom"}`)
	}))
	defer srv.Close()
	c := newAnnounceClient(srv.URL, 1*time.Second)
	if _, err := c.Announce(context.Background(), "spammer", ""); err == nil {
		t.Fatal("all-models-failed must return an error (→ template fallback)")
	}
}

func TestAnnounceIndependentShortTimeout(t *testing.T) {
	// A gateway that hangs longer than the announce timeout: the call must
	// fail fast (not wait the 30s judgment-style timeout).
	g := &announceGateway{delay: 5 * time.Second}
	srv := httptest.NewServer(http.HandlerFunc(g.handler))
	defer srv.Close()
	c := newAnnounceClient(srv.URL, 30*time.Second) // LONG judgment timeout
	c.SetAnnounceTimeout(500 * time.Millisecond)    // short announce timeout

	start := time.Now()
	if _, err := c.Announce(context.Background(), "spammer", ""); err == nil {
		t.Fatal("hanging gateway must fail via the announce timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("announce must respect its own short timeout, took %s", elapsed)
	}
}

func TestAnnounceTimeoutClamped(t *testing.T) {
	c := newAnnounceClient("http://127.0.0.1:1", time.Second)
	c.SetAnnounceTimeout(90 * time.Second) // typo'd judgment-scale value
	if got := c.announceTimeout(); got != AnnounceTimeoutMax {
		t.Errorf("90s must clamp to %s, got %s", AnnounceTimeoutMax, got)
	}
	c.SetAnnounceTimeout(100 * time.Millisecond)
	if got := c.announceTimeout(); got != AnnounceTimeoutMin {
		t.Errorf("100ms must clamp to %s, got %s", AnnounceTimeoutMin, got)
	}
	c2 := newAnnounceClient("http://127.0.0.1:1", time.Second)
	if got := c2.announceTimeout(); got != AnnounceTimeoutDefault {
		t.Errorf("zero config must default to %s, got %s", AnnounceTimeoutDefault, got)
	}
}

// --- Post-processing (B.4 hard rules, all table-tested) ----------------------

func TestCleanAnnouncePostProcessing(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		subject string
		want    string
		wantErr bool
	}{
		{"plain", "🚪 spammer 已送出门外", "spammer", "🚪 spammer 已送出门外", false},
		{"fenced", "```\n🚪 spammer 已送出门外\n```", "spammer", "🚪 spammer 已送出门外", false},
		{"json fenced", "```json\n🚪 spammer 已送出门外\n```", "spammer", "🚪 spammer 已送出门外", false},
		{"quoted", `"🚪 spammer 已送出门外"`, "spammer", "🚪 spammer 已送出门外", false},
		{"curly quoted", "“spammer 已送出门外”", "spammer", "spammer 已送出门外", false},
		{"multiline", "🚪 spammer 已送出门外\n广告请去别处发～", "spammer", "🚪 spammer 已送出门外 广告请去别处发～", false},
		{"empty", "   ", "spammer", "", true},
		{"missing subject", "🚪 已送出门外，广告请去别处发～", "spammer", "spammer 🚪 已送出门外，广告请去别处发～", false},
		{"url", "spammer 被踢，详见 https://t.me/chan", "spammer", "", true},
		{"bare domain", "spammer 被踢，详见 t.me/chan", "spammer", "", true},
		{"foreign mention", "spammer 被踢，加 @real_contact", "spammer", "", true},
		{"own mention ok", "@spammer 已送出门外", "spammer", "@spammer 已送出门外", false},
		{"phone", "spammer 被踢，联系 13812345678", "spammer", "", true},
		{"wechat", "spammer 被踢，加V:wx12345", "spammer", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanAnnounce(tc.in, tc.subject)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CleanAnnounce(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanAnnounce(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("CleanAnnounce(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanAnnounceTruncatesTo80(t *testing.T) {
	long := strings.Repeat("长", 120)
	long = long[:2] + "名" + long[2:] // subject "长名" inside
	subject := long[:2]
	out, err := CleanAnnounce(long, subject)
	if err != nil {
		t.Fatalf("truncate path must not error: %v", err)
	}
	if n := len([]rune(out)); n != 80 {
		t.Fatalf("length = %d runes, want 80", n)
	}
	if !strings.Contains(out, subject) {
		t.Fatalf("truncated output must still contain the subject: %q", out)
	}
}

func TestCleanAnnounceTruncationKeepsSubject(t *testing.T) {
	// Subject missing from a long text → prefix survives truncation.
	long := strings.Repeat("句", 120)
	out, err := CleanAnnounce(long, "名字")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(out, "名字 ") {
		t.Fatalf("subject must be prefixed: %q", out)
	}
	if n := len([]rune(out)); n != 80 {
		t.Fatalf("length = %d runes, want 80", n)
	}
}

func TestAnnounceNoncompliantOutputRejected(t *testing.T) {
	// The model echoes a contact string: the call must FAIL (→ template
	// fallback) — never a retry on the next model for the same violation.
	g := &announceGateway{reply: "spammer 被踢，加V:abc123"}
	srv := httptest.NewServer(http.HandlerFunc(g.handler))
	defer srv.Close()
	c := newAnnounceClient(srv.URL, 2*time.Second)
	if _, err := c.Announce(context.Background(), "spammer", ""); err == nil {
		t.Fatal("contact-style output must be rejected (template fallback)")
	}
	if g.callCount() != 1 {
		t.Fatalf("compliance violation must not walk the chain, calls = %d", g.callCount())
	}
}
