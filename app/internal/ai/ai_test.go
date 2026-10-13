package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGateway answers one request per handler invocation, counting per-model
// calls so fallback-chain tests can assert the walk.
type fakeGateway struct {
	mu     sync.Mutex
	calls  map[string]int   // model → call count
	bodies []map[string]any // request bodies in order
}

func (f *fakeGateway) record(model string, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[model]++
	f.bodies = append(f.bodies, body)
}

func (f *fakeGateway) count(model string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[model]
}

func (f *fakeGateway) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeGateway) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

// completionBody builds an OpenAI-shaped completion carrying verdictJSON as
// the assistant content.
func completionBody(verdictJSON string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s}}]}`,
		mustQuote(verdictJSON))
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func verdictJSON(isAd bool, category string, conf float64, reason string) string {
	b, _ := json.Marshal(map[string]any{
		"is_ad": isAd, "category": category, "confidence": conf, "reason": reason,
	})
	return string(b)
}

func newClient(url string, models []string, timeout time.Duration) *Client {
	return NewClient(ClientConfig{
		BaseURL: url,
		APIKey:  "testkey",
		Models:  models,
		Timeout: timeout,
	})
}

// --- Parse contract (tolerant shape, strict decision) ------------------------

func TestParseVerdictTolerantForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Verdict
	}{
		{"plain", `{"is_ad":true,"category":"办证","confidence":0.9,"reason":"办证"}`,
			Verdict{IsAd: true, Category: "办证", Confidence: 0.9, Reason: "办证"}},
		{"fenced", "```json\n{\"is_ad\":false,\"category\":\"无\",\"confidence\":0.1,\"reason\":\"正常\"}\n```",
			Verdict{IsAd: false, Category: "无", Confidence: 0.1, Reason: "正常"}},
		{"chatter", "好的，结论如下：{\"is_ad\":true,\"category\":\"诈骗\",\"confidence\":0.8,\"reason\":\"诈骗\"} 以上。",
			Verdict{IsAd: true, Category: "诈骗", Confidence: 0.8, Reason: "诈骗"}},
		{"confidence-string", `{"is_ad":true,"category":"赌博","confidence":"0.75","reason":"赌"}`,
			Verdict{IsAd: true, Category: "赌博", Confidence: 0.75, Reason: "赌"}},
		{"confidence-int", `{"is_ad":true,"category":"金融","confidence":1,"reason":"x"}`,
			Verdict{IsAd: true, Category: "金融", Confidence: 1, Reason: "x"}},
		{"confidence-percent-clamped", `{"is_ad":true,"category":"兼职","confidence":85,"reason":"x"}`,
			Verdict{IsAd: true, Category: "兼职", Confidence: 0.85, Reason: "x"}},
		{"confidence-small-percent", `{"is_ad":true,"category":"引流","confidence":5,"reason":"x"}`,
			Verdict{IsAd: true, Category: "引流", Confidence: 0.05, Reason: "x"}},
		{"confidence-huge-clamps", `{"is_ad":true,"category":"引流","confidence":250,"reason":"x"}`,
			Verdict{IsAd: true, Category: "引流", Confidence: 1, Reason: "x"}},
		{"confidence-negative", `{"is_ad":false,"category":"无","confidence":-3,"reason":"x"}`,
			Verdict{IsAd: false, Category: "无", Confidence: 0, Reason: "x"}},
		{"case-insensitive-keys", `{"Is_Ad":true,"Category":"账号交易","Confidence":"70%","Reason":"x"}`,
			Verdict{IsAd: true, Category: "账号交易", Confidence: 0.7, Reason: "x"}},
		{"missing-category-defaults", `{"is_ad":false,"confidence":0.2}`,
			Verdict{IsAd: false, Category: "其他", Confidence: 0.2}},
		{"braces-in-strings", `{"is_ad":true,"category":"其他","confidence":0.9,"reason":"含 }{ 花括号"}`,
			Verdict{IsAd: true, Category: "其他", Confidence: 0.9, Reason: "含 }{ 花括号"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseVerdict(c.in)
			if err != nil {
				t.Fatalf("ParseVerdict: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// A parse failure is a contract violation → error → the caller retries; it may
// NEVER be silently read as "not ad".
func TestParseVerdictContractViolationsFail(t *testing.T) {
	for _, in := range []string{
		"",
		"纯文本没有JSON",
		"```json\n```",
		"{bad json",
		`{"category":"办证","confidence":0.9}`, // missing is_ad
		`{"is_ad":"也许是","category":"办证"}`, // non-boolean is_ad
		`{"is_ad":1,"category":"办证"}`,     // numeric is_ad (strict)
		`[1,2,3]`,                         // array, not object
		`{"is_ad":null}`,                  // null is_ad
	} {
		if _, err := ParseVerdict(in); err == nil {
			t.Errorf("ParseVerdict(%q) should fail (contract violation → retry)", in)
		}
	}
}

func TestControlledCategoryMapsOutOfVocab(t *testing.T) {
	v := Verdict{IsAd: true, Category: "自创词汇"}
	if got := v.ControlledCategory(); got != "其他" {
		t.Errorf("out-of-vocab category = %q, want 其他", got)
	}
	if got := (Verdict{IsAd: true, Category: "赌博"}).ControlledCategory(); got != "赌博" {
		t.Errorf("in-vocab = %q", got)
	}
	if got := (Verdict{IsAd: false, Category: "赌博"}).ControlledCategory(); got != "无" {
		t.Errorf("non-ad = %q, want 无", got)
	}
}

// --- Request shape: thinking-off + no max_tokens -----------------------------

func TestClassifyRequestShape(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer testkey" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(completionBody(verdictJSON(true, "贷款", 0.7, "贷"))))
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"m"}, 2*time.Second).ClassifyText(context.Background(), "专业贷款", "message")
	if !ok || !res.Verdict.IsAd {
		t.Fatalf("ok=%v res=%+v", ok, res)
	}
	body := fg.lastBody()
	// Thinking off: reasoning_effort must be "none".
	if body["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v, want none", body["reasoning_effort"])
	}
	// Backup thinking-off field rides in chat_template_kwargs.
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs == nil || kwargs["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v, want {enable_thinking:false}", body["chat_template_kwargs"])
	}
	// max_tokens must NEVER be sent.
	if _, bad := body["max_tokens"]; bad {
		t.Error("request must not send max_tokens")
	}
	if body["stream"] != false {
		t.Errorf("stream = %v, want false", body["stream"])
	}
	// Messages: system prompt + wrapped user input.
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || strings.TrimSpace(fmt.Sprint(sys["content"])) == "" {
		t.Errorf("system message = %v", msgs[0])
	}
	user, _ := msgs[1].(map[string]any)
	content := fmt.Sprint(user["content"])
	if !strings.Contains(content, "<msg>") || !strings.Contains(content, "专业贷款") {
		t.Errorf("user content missing <msg> wrapper: %q", content)
	}
}

// --- Thinking-off on EVERY request (amend6 C) -----------------------------------
//
// The user asked for proof that thinking is off. These tests capture EVERY
// request body via httptest and assert reasoning_effort="none" AND
// chat_template_kwargs.enable_thinking=false per request — fallback chain
// models 2/3, every verify lane, the multimodal image request, and the bio
// prompt mode. Not just the first model.

func assertThinkingOff(t *testing.T, body map[string]any, context string) {
	t.Helper()
	if body["reasoning_effort"] != "none" {
		t.Errorf("[%s] reasoning_effort = %v, want none", context, body["reasoning_effort"])
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs == nil || kwargs["enable_thinking"] != false {
		t.Errorf("[%s] chat_template_kwargs = %v, want {enable_thinking:false}", context, body["chat_template_kwargs"])
	}
	if _, bad := body["max_tokens"]; bad {
		t.Errorf("[%s] request must not send max_tokens", context)
	}
}

// TestThinkingOffEveryFallbackModel: models 2 and 3 of the fallback chain are
// reached via 429/500 on the earlier ones; each captured request body must
// carry both thinking-off fields.
func TestThinkingOffEveryFallbackModel(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model := fmt.Sprint(body["model"])
		fg.record(model, body)
		if model == "m1" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if model == "m2" {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(completionBody(verdictJSON(false, "无", 0.9, "ok"))))
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"m1", "m2", "m3"}, 2*time.Second).
		ClassifyText(context.Background(), "x", "message")
	if !ok || res.Model != "m3" {
		t.Fatalf("chain must succeed on the third model: ok=%v res=%+v", ok, res)
	}
	if fg.count("m1") != 1 || fg.count("m2") != 1 || fg.count("m3") != 1 {
		t.Fatalf("all three models must be walked: %v", fg.calls)
	}
	fg.mu.Lock()
	bodies := append([]map[string]any(nil), fg.bodies...)
	fg.mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("captured bodies = %d, want 3", len(bodies))
	}
	for i, body := range bodies {
		assertThinkingOff(t, body, fmt.Sprintf("fallback model #%d", i+1))
	}
}

// TestThinkingOffEveryVerifyLane: 3 concurrent vote lanes on the same model —
// each lane's request body carries both thinking-off fields.
func TestThinkingOffEveryVerifyLane(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		w.Write([]byte(completionBody(verdictJSON(true, "赌博", 0.9, "ad"))))
	}))
	defer srv.Close()

	c := newClient(srv.URL, []string{"flash"}, 2*time.Second)
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"flash"}, Majority: 2, Timeout: 2 * time.Second}, nil, Content{Text: "x"})
	if res.Final != "ad" || res.VotesTotal != 3 {
		t.Fatalf("verify = %+v", res)
	}
	fg.mu.Lock()
	bodies := append([]map[string]any(nil), fg.bodies...)
	fg.mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("captured bodies = %d, want 3 (one per lane)", len(bodies))
	}
	for i, body := range bodies {
		assertThinkingOff(t, body, fmt.Sprintf("verify lane #%d", i+1))
	}
}

// TestThinkingOffMultimodalAndBioMode: the multimodal (image) request and the
// bio prompt mode request both carry both thinking-off fields.
func TestThinkingOffMultimodalAndBioMode(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		w.Write([]byte(completionBody(verdictJSON(false, "无", 0.9, "ok"))))
	}))
	defer srv.Close()

	c := newClient(srv.URL, []string{"m"}, 2*time.Second)
	if _, ok := c.Classify(context.Background(), Content{Text: "看图", ImageURL: "data:image/jpeg;base64,AAAA"}, "message"); !ok {
		t.Fatal("multimodal classify failed")
	}
	if _, ok := c.ClassifyText(context.Background(), "我擅长推广引流", "bio"); !ok {
		t.Fatal("bio-mode classify failed")
	}
	fg.mu.Lock()
	bodies := append([]map[string]any(nil), fg.bodies...)
	fg.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("captured bodies = %d, want 2 (multimodal + bio)", len(bodies))
	}
	assertThinkingOff(t, bodies[0], "multimodal image request")
	assertThinkingOff(t, bodies[1], "bio mode request")
}

// --- Multimodal request shape --------------------------------------------------

func TestClassifyMultimodalRequestShape(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record("m", body)
		w.Write([]byte(completionBody(verdictJSON(true, "色情", 0.95, "图"))))
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"m"}, 2*time.Second).Classify(context.Background(),
		Content{Text: "看图", ImageURL: "data:image/jpeg;base64,AAAA"}, "message")
	if !ok || !res.Verdict.IsAd {
		t.Fatalf("ok=%v res=%+v", ok, res)
	}
	body := fg.lastBody()
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("role = %v", user["role"])
	}
	// The user message must be multipart with a text part + an image_url part.
	content, ok := user["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content not 2 parts: %v", user["content"])
	}
	textPart, _ := content[0].(map[string]any)
	imgPart, _ := content[1].(map[string]any)
	if textPart["type"] != "text" {
		t.Errorf("part0 type = %v", textPart["type"])
	}
	if imgPart["type"] != "image_url" {
		t.Errorf("part1 type = %v", imgPart["type"])
	}
	imgURL, _ := imgPart["image_url"].(map[string]any)
	if imgURL["url"] != "data:image/jpeg;base64,AAAA" {
		t.Errorf("image_url.url = %v", imgURL)
	}
}

// --- Fallback chain -------------------------------------------------------------

func TestFallbackChainWalksToEndOn429(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model := fmt.Sprint(body["model"])
		fg.record(model, body)
		if model == "fast" {
			w.Write([]byte(completionBody(verdictJSON(false, "无", 0.9, "ok"))))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests) // 429 → next model
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"a", "b", "fast"}, 2*time.Second).ClassifyText(context.Background(), "x", "message")
	if !ok {
		t.Fatal("chain should succeed on the third model")
	}
	if res.Model != "fast" || !res.Fallback {
		t.Errorf("model=%s fallback=%v", res.Model, res.Fallback)
	}
	if fg.count("a") != 1 || fg.count("b") != 1 || fg.count("fast") != 1 {
		t.Errorf("walked calls = %v", fg.calls)
	}
	if len(res.Attempts) != 3 || res.Attempts[0].Status != "http_429" || res.Attempts[2].Status != "ok" {
		t.Errorf("attempts = %+v", res.Attempts)
	}
}

func TestFallbackChainPoolCodesWalkToEnd(t *testing.T) {
	// pool_parked / no_accounts do not benefit from a model switch but the
	// chain is still walked to the end before counting as failure.
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model := fmt.Sprint(body["model"])
		fg.record(model, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"error":{"message":"pool is parked","code":"pool_parked"}}`))
	}))
	defer srv.Close()

	_, ok := newClient(srv.URL, []string{"a", "b", "c"}, 2*time.Second).ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("pool_parked on every model must fail the call")
	}
	if fg.totalCalls() != 3 {
		t.Errorf("chain must walk all 3 models, got %d", fg.totalCalls())
	}
}

func TestFallbackChain5xxThenSuccess(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model := fmt.Sprint(body["model"])
		fg.record(model, body)
		if model == "a" {
			w.WriteHeader(500) // 5xx → next model
			return
		}
		w.Write([]byte(completionBody(verdictJSON(true, "赌博", 0.95, "赌"))))
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"a", "b", "c"}, 2*time.Second).ClassifyText(context.Background(), "x", "message")
	if !ok || res.Model != "b" || !res.Fallback {
		t.Fatalf("5xx on a must fall through to b: ok=%v res=%+v", ok, res)
	}
	if fg.count("a") != 1 || fg.count("b") != 1 || fg.count("c") != 0 {
		t.Errorf("walked calls = %v", fg.calls)
	}
}

func TestFallbackChainStopsOnParseFailure(t *testing.T) {
	// A parse/contract violation cannot be fixed by switching models: the chain
	// stops at the first model and the call counts as failed (→ retry queue).
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		w.Write([]byte(`{"choices":[{"message":{"content":"thanks, not json"}}]}`))
	}))
	defer srv.Close()

	_, ok := newClient(srv.URL, []string{"a", "b", "c"}, 2*time.Second).ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("parse violation must fail the call")
	}
	if fg.totalCalls() != 1 {
		t.Errorf("chain must stop after the parse failure, calls=%d", fg.totalCalls())
	}
}

func TestFallbackChainAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	res, ok := newClient(srv.URL, []string{"a", "b", "c"}, 1*time.Second).ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("all-5xx chain must fail")
	}
	if len(res.Attempts) != 3 {
		t.Errorf("attempts = %d, want 3", len(res.Attempts))
	}
}

func TestClassifyEmptyContentFailSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
	}))
	defer srv.Close()
	_, ok := newClient(srv.URL, []string{"m"}, 2*time.Second).ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("empty assistant content must fail (never 'not ad')")
	}
}

func TestClassifyTimeoutFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(completionBody(verdictJSON(false, "无", 0.9, "late"))))
	}))
	defer srv.Close()
	_, ok := newClient(srv.URL, []string{"m", "m2"}, 80*time.Millisecond).ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("timeout must fail the chain (walk both models, then fail)")
	}
}

// --- Prompt handling ---------------------------------------------------------

func TestBuildSystemPromptBioAppendsClause(t *testing.T) {
	base := BuildSystemPrompt("message")
	bio := BuildSystemPrompt("bio")
	if !strings.Contains(bio, base) {
		t.Error("bio prompt must embed the base prompt (shared must-kill list)")
	}
	if len(bio) <= len(base) {
		t.Error("bio prompt must append the self-promotion clause")
	}
}

func TestPromptOverrideReplacesBase(t *testing.T) {
	old := PromptOverride
	defer func() { PromptOverride = old }()
	PromptOverride = "OVERRIDE PROMPT"
	if got := BuildSystemPrompt("message"); got != "OVERRIDE PROMPT" {
		t.Errorf("override = %q", got)
	}
	if got := BuildSystemPrompt("bio"); got != "OVERRIDE PROMPT\n\n"+promptJudgeBio {
		t.Errorf("bio override = %q", got)
	}
}

func TestWrapInputDelimits(t *testing.T) {
	got := WrapInput("加V:abc")
	if !strings.HasPrefix(got, "<msg>") || !strings.HasSuffix(got, "</msg>") {
		t.Errorf("WrapInput = %q", got)
	}
}

func TestTruncateRunesRepairsInvalidTail(t *testing.T) {
	s := strings.Repeat("证", 5000)
	got := truncateRunes(s, MaxInputRunes)
	if len(got) > MaxInputRunes+3 {
		t.Errorf("truncated length = %d", len(got))
	}
	// Cut inside a multibyte sequence must be repaired to valid UTF-8.
	multi := "汉汉汉汉汉"
	cut := truncateRunes(multi, 4) // 4 bytes cuts mid-character
	if !isMostlyValidUTF8(cut) {
		t.Errorf("cut = %q is not valid UTF-8-tolerant", cut)
	}
}

func isMostlyValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func TestPromptVersionConstant(t *testing.T) {
	if PromptVersion == "" {
		t.Fatal("PromptVersion must be a non-empty versioned constant")
	}
}

// --- Stats -------------------------------------------------------------------

func TestStatsCounters(t *testing.T) {
	s := NewStats()
	s.NoteAttempt("a", true)
	s.NoteAttempt("a", false)
	s.NoteAttempt("b", true)
	s.NoteFallback()
	s.NoteVerify(false)
	s.NoteVerify(true)
	s.NoteLocalFallback()
	sn := s.Snapshot()
	if sn.ModelCalls["a"] != 2 || sn.ModelOKs["a"] != 1 || sn.ModelCalls["b"] != 1 {
		t.Errorf("model stats = %v/%v", sn.ModelCalls, sn.ModelOKs)
	}
	if sn.Fallbacks != 1 || sn.VerifyRounds != 2 || sn.VerifyFlipped != 1 ||
		sn.LocalFallback != 1 {
		t.Errorf("counters = %+v", sn)
	}
	if len(sn.SortedModels()) != 2 {
		t.Errorf("sorted models = %v", sn.SortedModels())
	}
}

func TestStatsConcurrent(t *testing.T) {
	s := NewStats()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.NoteAttempt("m", true)
			s.NoteFallback()
		}()
	}
	wg.Wait()
	sn := s.Snapshot()
	if sn.ModelCalls["m"] != 32 || sn.Fallbacks != 32 {
		t.Errorf("race-lost updates: %d/%d", sn.ModelCalls["m"], sn.Fallbacks)
	}
}

// --- Verify (concurrent vote) ------------------------------------------------

func voteServer(t *testing.T, fn func(model string, call int) (string, int)) (*httptest.Server, *fakeGateway) {
	t.Helper()
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model := fmt.Sprint(body["model"])
		n := fg.count(model) + 1
		fg.record(model, body)
		content, code := fn(model, n)
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		w.Write([]byte(completionBody(content)))
	}))
	return srv, fg
}

func TestVerifyMajorityAd(t *testing.T) {
	srv, _ := voteServer(t, func(model string, call int) (string, int) {
		return verdictJSON(true, "赌博", 0.9, "ad"), 0
	})
	defer srv.Close()
	c := newClient(srv.URL, []string{"flash"}, 2*time.Second)
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"flash"}, Majority: 2, Timeout: 2 * time.Second}, nil, Content{Text: "x"})
	if res.Final != "ad" || res.VotesAd != 3 {
		t.Errorf("res = %+v", res)
	}
}

func TestVerifyAbstainThenTieUnresolved(t *testing.T) {
	// Lane model "ok" votes ad, lane model "no" votes not-ad, lane model "fail"
	// 500s (abstain): votes_ad=1 < majority=2 → unresolved.
	srv, _ := voteServer(t, func(model string, call int) (string, int) {
		switch model {
		case "ok":
			return verdictJSON(true, "赌博", 0.9, "ad"), 0
		case "no":
			return verdictJSON(false, "无", 0.9, "no"), 0
		default:
			return "", 500
		}
	})
	defer srv.Close()
	c := newClient(srv.URL, []string{"ok", "no", "fail"}, 2*time.Second)
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"ok", "no", "fail"}, Majority: 2, Timeout: 2 * time.Second}, nil, Content{Text: "x"})
	if res.Final != "unresolved" {
		t.Errorf("1-ad/1-no/1-abstain must be unresolved: %+v", res)
	}
	if res.Abstentions != 1 || res.VotesAd != 1 || res.VotesNo != 1 {
		t.Errorf("tally = ad:%d no:%d abstain:%d", res.VotesAd, res.VotesNo, res.Abstentions)
	}
}

func TestVerifyAllAbstainUnresolved(t *testing.T) {
	srv, _ := voteServer(t, func(model string, call int) (string, int) {
		return "", 503
	})
	defer srv.Close()
	c := newClient(srv.URL, []string{"m"}, 2*time.Second)
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"m"}, Majority: 2, Timeout: 2 * time.Second}, nil, Content{Text: "x"})
	if res.Final != "unresolved" || res.Abstentions != 3 {
		t.Errorf("all abstain must be unresolved: %+v", res)
	}
}

func TestVerifyMajorityNotAd(t *testing.T) {
	srv, _ := voteServer(t, func(model string, call int) (string, int) {
		return verdictJSON(false, "无", 0.9, "no"), 0
	})
	defer srv.Close()
	c := newClient(srv.URL, []string{"flash"}, 2*time.Second)
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"flash"}, Majority: 2, Timeout: 2 * time.Second}, nil, Content{Text: "x"})
	if res.Final != "not_ad" || res.VotesNo != 3 {
		t.Errorf("res = %+v", res)
	}
}

func TestVerifyRunsLanesConcurrently(t *testing.T) {
	var inflight, maxInflight atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if cur <= old || maxInflight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		inflight.Add(-1)
		w.Write([]byte(completionBody(verdictJSON(true, "赌博", 0.9, "ad"))))
	}))
	defer srv.Close()
	c := newClient(srv.URL, []string{"flash"}, 2*time.Second)
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"flash"}, Majority: 2, Timeout: 2 * time.Second}, nil, Content{Text: "x"})
	if res.Final != "ad" {
		t.Fatalf("res = %+v", res)
	}
	if maxInflight.Load() < 3 {
		t.Errorf("lanes must run concurrently, max inflight = %d", maxInflight.Load())
	}
}

func TestVerifyPerLaneTimeoutAbstains(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(completionBody(verdictJSON(true, "赌博", 0.9, "ad"))))
	}))
	defer srv.Close()
	c := newClient(srv.URL, []string{"flash"}, 5*time.Second) // client timeout long
	res := Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"flash"}, Majority: 2, Timeout: 60 * time.Millisecond}, nil, Content{Text: "x"})
	if res.Abstentions != 3 {
		t.Fatalf("per-lane timeout must abstain all 3 lanes: %+v", res)
	}
}

func TestVerifyStatsNoteAttempts(t *testing.T) {
	srv, _ := voteServer(t, func(model string, call int) (string, int) {
		return verdictJSON(true, "赌博", 0.9, "ad"), 0
	})
	defer srv.Close()
	c := newClient(srv.URL, []string{"flash"}, 2*time.Second)
	stats := NewStats()
	Verify(context.Background(), c, VerifyConfig{Votes: 3, Models: []string{"flash"}, Majority: 2, Timeout: 2 * time.Second}, stats, Content{Text: "x"})
	if stats.Snapshot().ModelCalls["flash"] != 3 {
		t.Errorf("stats must count the 3 lane attempts: %+v", stats.Snapshot().ModelCalls)
	}
}

func TestVerifyFormatRendersLanes(t *testing.T) {
	res := VerifyResult{
		Lanes: []VoteLane{
			{Model: "flash", IsAd: true, Confidence: 0.9, MS: 10},
			{Model: "flash", Err: "undecided", MS: 11},
		},
		VotesTotal: 2, VotesAd: 1, Abstentions: 1, Final: "unresolved",
	}
	out := res.Format()
	if !strings.Contains(out, "abstain") || !strings.Contains(out, "final=unresolved") {
		t.Errorf("Format = %q", out)
	}
}

// --- Upstream sensitive interception (amend5 A) -------------------------------

// sensitive400 writes the 400 + content-filter envelope the interception rule
// matches (HTTP 400 AND message contains "content filtered by upstream").
func sensitive400(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"error":{"message":%s,"code":"content_filter"}}`, mustQuote(msg))
}

// TestUpstreamSensitiveKillsImmediately: a 400 whose message contains
// "content filtered by upstream" yields SensitiveVerdict, terminates the
// chain at the FIRST model (no fallback — exactly 1 HTTP request), and
// returns ok=true so callers treat it as a normal verdict (no retry queue).
func TestUpstreamSensitiveKillsImmediately(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		sensitive400(w, "content filtered by upstream (sensitive words detected)")
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"a", "b", "c"}, 2*time.Second).
		ClassifyText(context.Background(), "加V接单", "message")
	if !ok {
		t.Fatal("sensitive interception must return ok=true (a verdict, not an error)")
	}
	if !res.Verdict.IsAd {
		t.Fatal("sensitive interception must be a kill (IsAd=true)")
	}
	if res.Verdict != SensitiveVerdict {
		t.Errorf("verdict = %+v, want SensitiveVerdict", res.Verdict)
	}
	if fg.totalCalls() != 1 {
		t.Fatalf("chain must terminate at the first model: %d requests", fg.totalCalls())
	}
	if res.Model != "a" {
		t.Errorf("model = %s, want a (first model)", res.Model)
	}
}

// TestUpstreamSensitiveCodeVariant: "sensitive" in the error code (400) is
// also a hit — case-insensitive.
func TestUpstreamSensitiveCodeVariant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"rejected","code":"SENSITIVE_Content"}}`))
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"a", "b"}, 2*time.Second).
		ClassifyText(context.Background(), "x", "message")
	if !ok || res.Verdict != SensitiveVerdict {
		t.Fatalf("code-variant 400 must be a sensitive kill: ok=%v verdict=%+v", ok, res.Verdict)
	}
}

// TestUpstreamSensitiveStopsRetries: the sensitive result must not look like
// a failure to the retry queue — ClassifyWithModel (the verify-lane shape)
// also short-circuits.
func TestUpstreamSensitiveStopsRetries(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		sensitive400(w, "this request was content filtered by upstream provider")
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"only"}, 2*time.Second).
		ClassifyWithModel(context.Background(), "only", Content{Text: "x"}, "message")
	if !ok || res.Verdict != SensitiveVerdict {
		t.Fatalf("single-model sensitive must return the verdict: ok=%v verdict=%+v", ok, res.Verdict)
	}
	if fg.totalCalls() != 1 {
		t.Errorf("exactly one request expected, got %d", fg.totalCalls())
	}
}

// TestOther400StillFails: a 400 with a NON-sensitive message (e.g. invalid
// model) keeps the original failure semantics — not ok, chain walks.
func TestOther400StillFails(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"invalid request: model not found","code":"invalid_model"}}`))
	}))
	defer srv.Close()

	res, ok := newClient(srv.URL, []string{"a", "b", "c"}, 2*time.Second).
		ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("non-sensitive 400 must keep failure semantics (ok=false)")
	}
	if res.Verdict == SensitiveVerdict {
		t.Fatal("non-sensitive 400 must not synthesize the sensitive verdict")
	}
	if fg.totalCalls() != 1 {
		t.Errorf("non-sensitive 400 is not fallback-eligible: exactly 1 call expected, got %d", fg.totalCalls())
	}
	if res.Attempts[len(res.Attempts)-1].Status != "http_400" {
		t.Errorf("final attempt status = %q, want http_400", res.Attempts[len(res.Attempts)-1].Status)
	}
}

// TestPoolParked429Unchanged: 429 / pool codes keep the retryable failure
// semantics — regression guard for "ONLY the sensitive 400 shape changes".
func TestPoolParked429Unchanged(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fg.record(fmt.Sprint(body["model"]), body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"error":{"message":"pool is parked","code":"pool_parked"}}`))
	}))
	defer srv.Close()

	_, ok := newClient(srv.URL, []string{"a", "b"}, 2*time.Second).
		ClassifyText(context.Background(), "x", "message")
	if ok {
		t.Fatal("pool_parked must keep failure semantics")
	}
	if fg.totalCalls() != 2 {
		t.Errorf("chain walk expected, got %d", fg.totalCalls())
	}
}

// TestSensitiveVerdictConstant: the synthesized verdict shape is part of the
// contract (source label / notify suffix depend on equality).
func TestSensitiveVerdictConstant(t *testing.T) {
	if !SensitiveVerdict.IsAd || SensitiveVerdict.Category != "上游敏感" || SensitiveVerdict.Confidence != 0.99 {
		t.Errorf("SensitiveVerdict = %+v", SensitiveVerdict)
	}
	if SensitiveVerdict.Reason == "" {
		t.Error("reason must be set (feeds notify/logs)")
	}
}
