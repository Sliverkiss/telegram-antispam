// Package ai wraps an OpenAI-compatible chat completions endpoint for binary
// ad/non-ad classification. It deliberately strips noisy fields the gateway
// (mimo) rejects, parses the JSON verdict tolerantly, and fails safe: any
// parse or transport failure is "undecided", never "ad".
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"adbot/internal/logx"
)

// Verdict is the classifier's decision output.
type Verdict struct {
	IsAd       bool    `json:"is_ad"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// Config holds the endpoint and behavior knobs.
type Config struct {
	BaseURL string        // e.g. https://mimo.xn--ug8h.eu.org/v1
	APIKey  string        // Bearer token
	Model   string        // e.g. mimo-v2.6-pro-ultraspeed-studio
	Timeout time.Duration // per-attempt timeout (default 30s)
	Retries int           // extra attempts after the first (default 2)
	Logger  *logx.Logger
	Client  *http.Client // test hook; nil → shared default client
}

// SystemPrompt instructs the model to classify and answer JSON only.
const SystemPrompt = "你是反垃圾/反灰黑产审核助手。判断给定的内容是不是灰产/黑产广告导流（如赌博、色情引流、诈骗、套路贷、办假证、发票、作弊、刷量等）。只输出一个 JSON 对象，格式：{\"is_ad\":bool,\"category\":\"gambling|porn|fraud|loan|fake_cert|invoice|cheat|traffic_farming|none\",\"confidence\":0.0到1.0,\"reason\":\"简短中文理由\"}，不要输出任何其他文字。"

// New builds a Config with defaults filled in.
func New(cfg Config) Config {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://mimo.xn--ug8h.eu.org/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "mimo-v2.6-pro-ultraspeed-studio"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Retries <= 0 {
		cfg.Retries = 2
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout + 5*time.Second}
	}
	return cfg
}

// Classify asks the model whether content is ad/promotion. The returned
// (verdict, ok) is (zero, false) when the model could not be reached or its
// answer could not be parsed — callers must treat "undecided" as "do not act".
func Classify(ctx context.Context, cfg Config, content string) (Verdict, bool) {
	cfg = New(cfg)

	body := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": SystemPrompt},
			{"role": "user", "content": content},
		},
		"stream": false,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Verdict{}, false
	}

	var lastErr error
	for attempt := 0; attempt <= cfg.Retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 500ms, 1s, 2s…
			select {
			case <-ctx.Done():
				return Verdict{}, false
			case <-time.After(time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond):
			}
		}
		v, retriable, err := classifyOnce(ctx, cfg, payload)
		if err == nil {
			return v, true
		}
		lastErr = err
		if cfg.Logger != nil {
			cfg.Logger.Warn("ai_classify_error", "attempt", attempt+1, "retriable", retriable, "err", err.Error())
		}
		if !retriable {
			break
		}
	}
	if cfg.Logger != nil {
		cfg.Logger.Warn("ai_classify_failed", "err", fmt.Sprint(lastErr))
	}
	return Verdict{}, false
}

// classifyOnce performs one HTTP round trip; retriable reports whether
// another attempt could plausibly succeed (timeout, 5xx, network error, but
// not an unparseable 200 response).
func classifyOnce(ctx context.Context, cfg Config, payload []byte) (Verdict, bool, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Verdict{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return Verdict{}, true, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{}, true, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return Verdict{}, true, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, false, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	v, err := parseCompletion(data)
	ms := time.Since(start)
	if err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Warn("ai_parse_error", "duration_ms", ms.Milliseconds(), "body", truncate(string(data), 200))
		}
		return Verdict{}, false, err
	}
	if cfg.Logger != nil {
		cfg.Logger.Info("ai_verdict", "is_ad", v.IsAd, "category", v.Category, "confidence", v.Confidence, "duration_ms", ms.Milliseconds())
	}
	return v, false, nil
}

// parseCompletion extracts the assistant's text and decodes the verdict JSON.
func parseCompletion(data []byte) (Verdict, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return Verdict{}, fmt.Errorf("decode completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Verdict{}, errors.New("empty choices")
	}
	text := resp.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		return Verdict{}, errors.New("empty assistant content")
	}
	return ParseVerdict(text)
}

// ParseVerdict turns model text into a Verdict, tolerating ```json fences,
// preamble/trailing chatter, and mild field noise. A failure returns an
// error so callers can fail safe.
func ParseVerdict(text string) (Verdict, error) {
	text = stripFences(text)
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end < start {
		return Verdict{}, fmt.Errorf("no JSON object in %q", truncate(text, 120))
	}
	raw := text[start : end+1]
	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, fmt.Errorf("decode verdict: %w", err)
	}
	return v, nil
}

// stripFences removes ```json … ``` code fences and backticks.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			rest := s[i+1:]
			if j := strings.LastIndex(rest, "```"); j >= 0 {
				rest = rest[:j]
			}
			s = rest
		}
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
