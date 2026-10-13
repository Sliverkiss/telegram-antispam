package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Verdict
	}{

		{"plain", `{"is_ad":true,"category":"gambling","confidence":0.9,"reason":"赌博"}`,
			Verdict{IsAd: true, Category: "gambling", Confidence: 0.9, Reason: "赌博"}},
		{"fenced", "```json\n{\"is_ad\":false,\"category\":\"none\",\"confidence\":0.1,\"reason\":\"正常\"}\n```",
			Verdict{IsAd: false, Category: "none", Confidence: 0.1, Reason: "正常"}},
		{"dirty", "好的，结论如下：{\"is_ad\":true,\"category\":\"fraud\",\"confidence\":0.8,\"reason\":\"诈骗\"} 以上。",
			Verdict{IsAd: true, Category: "fraud", Confidence: 0.8, Reason: "诈骗"}},
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

func TestParseVerdictFails(t *testing.T) {
	for _, in := range []string{"", "no json here", "```json\n```", "{bad json"} {
		if _, err := ParseVerdict(in); err == nil {
			t.Errorf("ParseVerdict(%q) should fail", in)
		}
	}
}

func completionBody(v Verdict) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": mustJSON(v)}}},
	})
	return string(b)
}

func mustJSON(v Verdict) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestClassifyRequestShape(t *testing.T) {
	var got map[string]any
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
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(completionBody(Verdict{IsAd: true, Category: "loan", Confidence: 0.7, Reason: "贷"})))
	}))
	defer srv.Close()

	v, ok := Classify(context.Background(), Config{
		BaseURL: srv.URL,
		APIKey:  "testkey",
		Model:   "m",
		Timeout: 2 * time.Second,
		Retries: 0,
	}, "专业贷款")
	if !ok || !v.IsAd {
		t.Fatalf("ok=%v v=%+v", ok, v)
	}
	// Request must NOT carry max_tokens (gateway warns on it).
	if _, bad := got["max_tokens"]; bad {
		t.Error("request must not send max_tokens")
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, want false", got["stream"])
	}
	if got["model"] != "m" {
		t.Errorf("model = %v", got["model"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("messages = %v", got["messages"])
	}
}

func TestClassifyTimeoutRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Always fail with a 500 → retriable, exhausting retries then failing.
		w.WriteHeader(500)
	}))
	defer srv.Close()

	_, ok := Classify(context.Background(), Config{
		BaseURL: srv.URL,
		Timeout: 200 * time.Millisecond,
		Retries: 2,
	}, "x")
	if ok {
		t.Fatal("5xx should yield undecided")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("calls = %d, want 3 (1 + 2 retries)", n)
	}
}

func TestClassifyRateLimitNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, ok := Classify(context.Background(), Config{
		BaseURL: srv.URL, Timeout: 3 * time.Second, Retries: 0}, "x")
	if ok {
		t.Fatal("429 should not classify")
	}
	// 429 is retriable in our policy (respect back-off later) — assert it
	// still surfaced as undecided and attempted more than once is NOT required;
	// just assert it did not misclassify as ad.
	if n := atomic.LoadInt32(&calls); n < 1 {
		t.Errorf("expected at least one call, got %d", n)
	}
}

func TestClassifyParseFailureFailsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"content":"thanks, not json"}}]}`))
	}))
	defer srv.Close()
	_, ok := Classify(context.Background(), Config{
		BaseURL: srv.URL, Timeout: 3 * time.Second, Retries: 0}, "x")
	if ok {
		t.Fatal("unparseable answer must be undecided (fail-safe)")
	}
}

func TestClassifyEmptyContentFailSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
	}))
	defer srv.Close()
	_, ok := Classify(context.Background(), Config{
		BaseURL: srv.URL, Timeout: 3 * time.Second, Retries: 0}, "x")
	if ok {
		t.Fatal("empty assistant content must be undecided")
	}
}

func TestStripFences(t *testing.T) {
	if got := stripFences("```json\n{x}\n```"); strings.TrimSpace(got) != "{x}" {
		t.Errorf("stripFences = %q", got)
	}
}
