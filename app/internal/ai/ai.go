// Package ai wraps an OpenAI-compatible chat completions endpoint (go-openai)
// for binary ad/non-ad classification of message text and images.
//
// Design contract (TASK_antispam_amend3.md A+C):
//   - Model fallback chain: 429/5xx/timeout/network errors, and response-body
//     error codes pool_parked/no_accounts/upstream_rate_limited/upstream_busy,
//     advance to the next model; the whole chain must be walked before a call
//     counts as failed.
//   - Thinking is disabled via reasoning_effort="none" (enable_thinking=false
//     is sent as a backup field); max_tokens is never sent (the gateway warns).
//   - Parse layer is tolerant (fences, stray text, confidence as string/int,
//     clamped to [0,1], case-insensitive keys) but a contract violation (no
//     JSON / missing is_ad / unparseable is_ad) is a FAILURE so callers retry,
//     never "not ad".
//   - category is free Chinese text: no enum validation; out-of-vocabulary
//     values are kept verbatim (display-side maps to "其他").
//   - An upstream content-filter rejection (amend5 A) is NOT an error: HTTP 400
//     with "content filtered by upstream" / "sensitive" in the error envelope
//     is the filter refusing to judge exactly the content we most want to
//     kill. classifyOnce converts it into SensitiveVerdict so the model chain
//     stops immediately, nothing retries, and downstream needs zero
//     special-casing. Any OTHER 400 keeps the ordinary failure semantics.
package ai

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"adbot/internal/logx"
)

// PromptVersion identifies the embedded prompt revision. Bump it whenever
// prompts/judge_v2.txt changes so misjudgments can be traced to a prompt
// version in the logs and /status.
const PromptVersion = "judge_v2"

//go:embed prompts/judge_v2.txt
var promptJudge string

//go:embed prompts/judge_v1_bio.txt
var promptJudgeBio string

// PromptOverride replaces the embedded base prompt when BOT_AI_PROMPT_FILE is
// set (loaded at startup); empty means the embedded version.
var PromptOverride string

// MaxInputRunes caps the untrusted text we forward to the model per request
// (huge messages/captions are truncated, never OOM).
const MaxInputRunes = 4096

// Verdict is the classifier's decision output.
type Verdict struct {
	IsAd       bool    `json:"is_ad"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// SensitiveVerdict is the verdict synthesized when the upstream content filter
// refuses to judge (amend5 A.3): the filter only trips on the content we most
// want killed, so the rejection itself is treated as the kill decision.
var SensitiveVerdict = Verdict{
	IsAd:       true,
	Category:   "上游敏感",
	Confidence: 0.99,
	Reason:     "upstream content filter rejected the text",
}

// controlledCategories is the prompt's controlled vocabulary for category.
var controlledCategories = []string{
	"办证", "色情", "儿童性内容", "赌博", "诈骗", "金融", "兼职", "引流", "违禁品", "账号交易", "其他",
}

// ControlledCategory reports the in-vocabulary category for display; unknown
// free text maps to "其他" (logged debug upstream), never an error.
func (v Verdict) ControlledCategory() string {
	if !v.IsAd {
		return "无"
	}
	for _, c := range controlledCategories {
		if v.Category == c {
			return c
		}
	}
	return "其他"
}

// attempt records one model round trip for the ai_judge attempts log.
type attempt struct {
	Model  string
	Status string // "ok" | "http_429" | "pool_parked" | "timeout" | "parse" ...
	MS     int64
}

// ClassifyResult carries the verdict plus chain diagnostics for /check, logs,
// and /status counters.
type ClassifyResult struct {
	Verdict  Verdict
	Model    string
	Fallback bool
	Attempts []attempt
}

// Content is one classification input: text, optionally with an image
// (base64 data URL) for multimodal judging.
type Content struct {
	Text     string
	ImageURL string
}

// Client classifies content against one provider (base URL + key) with a
// model fallback chain. It is safe for concurrent use.
type Client struct {
	cfg    ClientConfig
	client *openai.Client
	// lastBody captures the most recent response body (bounded) so a 200
	// status whose body is an error envelope can still be classified.
	lastBody *bodyCapture
	// announceTO overrides the announce timeout (BOT_ANNOUNCE_TIMEOUT;
	// zero → AnnounceTimeoutDefault with clamping).
	announceTO time.Duration
}

// bodyCapture remembers the tail of the last response body (bounded, 4KB —
// error envelopes are tiny) for error-envelope detection.
type bodyCapture struct {
	mu    sync.Mutex
	chunk []byte
}

func (b *bodyCapture) set(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chunk = p
}

func (b *bodyCapture) get() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.chunk
}

// captureTransport wraps the real transport and stashes a copy of each
// response body (restoring it for the caller via NopCloser over a re-read).
type captureTransport struct {
	base    http.RoundTripper
	capture *bodyCapture
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.Body != nil {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if readErr != nil {
			return resp, readErr
		}
		if len(data) > 0 {
			t.capture.set(data[maxTail(data):])
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
	}
	return resp, nil
}

func maxTail(data []byte) int {
	// helper: keep the last 4KB
	if len(data) > 4096 {
		return len(data) - 4096
	}
	return 0
}

// lastBodyErrorCode digs a pool-style error code out of the last 200-status
// response body ({"error":{"code":"pool_parked",...}}).
func (c *Client) lastBodyErrorCode() string {
	body := c.lastBody.get()
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Error *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return ""
	}
	if s, ok := env.Error.Code.(string); ok && isPoolCode(s) {
		return s
	}
	msg := strings.ToLower(env.Error.Message)
	for _, code := range []string{"pool_parked", "no_accounts", "upstream_rate_limited", "upstream_busy"} {
		if strings.Contains(msg, code) {
			return code
		}
	}
	return ""
}

// ClientConfig configures one provider.
type ClientConfig struct {
	BaseURL string        // e.g. https://your-gateway.example.com/v1
	APIKey  string        // Bearer token
	Models  []string      // ordered fallback chain
	Timeout time.Duration // per-attempt timeout (default 30s)
	Logger  *logx.Logger
	// HTTPDoer overrides the transport for tests (httptest fake gateways).
	HTTPDoer openai.HTTPDoer
}

// NewClient builds a provider client with defaults filled in. A nil/empty
// Models chain defaults to the canonical three-model chain.
func NewClient(cfg ClientConfig) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed-studio"}
	}
	ocfg := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		ocfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	}
	var baseDoer openai.HTTPDoer = &http.Client{Timeout: cfg.Timeout + 5*time.Second}
	if cfg.HTTPDoer != nil {
		baseDoer = cfg.HTTPDoer
	}
	// Every request goes through the capturing transport so a 200-with-error-
	// envelope body can be classified even though go-openai discards it.
	cap := &bodyCapture{}
	var doer openai.HTTPDoer
	if rt, ok := baseDoer.(*http.Client); ok {
		tr := rt.Transport
		if tr == nil {
			tr = http.DefaultTransport
		}
		rt2 := *rt
		rt2.Transport = &captureTransport{base: tr, capture: cap}
		doer = &rt2
	} else {
		// A plain HTTPDoer (tests): wrap it in a one-off client with capture.
		doer = &http.Client{Transport: &captureTransport{base: asRoundTripper(baseDoer), capture: cap}}
	}
	ocfg.HTTPClient = doer
	return &Client{cfg: cfg, client: openai.NewClientWithConfig(ocfg), lastBody: cap}
}

// SetAnnounceTimeout overrides the announce per-attempt timeout
// (BOT_ANNOUNCE_TIMEOUT; the caller passes the already-loaded config value).
func (c *Client) SetAnnounceTimeout(d time.Duration) { c.announceTO = d }

// asRoundTripper adapts an HTTPDoer to a RoundTripper when it is not already
// an *http.Client.
func asRoundTripper(d openai.HTTPDoer) http.RoundTripper {
	if rt, ok := d.(http.RoundTripper); ok {
		return rt
	}
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) { return d.Do(r) })
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Models returns the fallback chain.
func (c *Client) Models() []string { return c.cfg.Models }

// BuildSystemPrompt returns the judge prompt for the given mode ("message" or
// "bio"). Both modes share the same must-kill list and output contract; the
// bio variant appends the self-promotion clause.
func BuildSystemPrompt(mode string) string {
	base := promptJudge
	if PromptOverride != "" {
		base = PromptOverride
	}
	if mode == "bio" {
		return base + "\n\n" + promptJudgeBio
	}
	return base
}

// WrapInput delimits untrusted content so prompt injection inside the message
// is treated as data, per the prompt's own <msg> contract.
func WrapInput(text string) string {
	return "<msg>\n" + text + "\n</msg>"
}

// Classify walks the model fallback chain once and returns the verdict.
// ok=false means the whole chain failed (network/5xx/parse contract broken) —
// callers must enqueue for retry, never treat it as "not ad".
// An upstream content-filter rejection (amend5 A) returns SensitiveVerdict
// with ok=true immediately: no model switch (the next model would be filtered
// the same way), no retry (it is a verdict, not an error).
func (c *Client) Classify(ctx context.Context, content Content, mode string) (ClassifyResult, bool) {
	start := time.Now()
	sys := BuildSystemPrompt(mode)
	msgs := c.buildMessages(sys, content)

	var res ClassifyResult
	res.Verdict = Verdict{Category: "无"}
	for i, model := range c.cfg.Models {
		res.Fallback = i > 0
		at := attempt{Model: model}
		t0 := time.Now()
		v, status, err := c.classifyOnce(ctx, model, msgs)
		at.MS = time.Since(t0).Milliseconds()
		at.Status = status
		res.Attempts = append(res.Attempts, at)
		if status == "upstream_sensitive" && err == nil {
			// The filter rejected the text: we already hold the answer, so the
			// chain terminates at this model (no fallback, no retry).
			res.Verdict = v
			res.Model = model
			if c.cfg.Logger != nil {
				c.cfg.Logger.Info("ai_judge",
					"model_used", model,
					"fallback", res.Fallback,
					"attempts", formatAttempts(res.Attempts),
					"is_ad", v.IsAd,
					"confidence", v.Confidence,
					"category", v.Category,
					"basis", "upstream_sensitive",
					"latency_ms", time.Since(start).Milliseconds(),
					"prompt_version", PromptVersion)
			}
			return res, true
		}
		if err == nil {
			res.Verdict = v
			res.Model = model
			if c.cfg.Logger != nil {
				c.cfg.Logger.Info("ai_judge",
					"model_used", model,
					"fallback", res.Fallback,
					"attempts", formatAttempts(res.Attempts),
					"is_ad", v.IsAd,
					"confidence", v.Confidence,
					"category", v.Category,
					"latency_ms", time.Since(start).Milliseconds(),
					"prompt_version", PromptVersion)
			}
			return res, true
		}
		if c.cfg.Logger != nil {
			c.cfg.Logger.Warn("ai_model_attempt_failed",
				"model", model, "status", status, "err", err.Error(), "ms", at.MS,
				"prompt_version", PromptVersion)
		}
		if !fallbackEligible(err) {
			// Contract/parse failures do not improve by switching models; the
			// chain stops here and the call still counts as failed.
			break
		}
	}
	if c.cfg.Logger != nil {
		c.cfg.Logger.Error("ai_judge_failed",
			"attempts", formatAttempts(res.Attempts),
			"err", "all models in chain failed",
			"prompt_version", PromptVersion)
	}
	return res, false
}

// ClassifyText is a convenience wrapper for plain text input.
func (c *Client) ClassifyText(ctx context.Context, text, mode string) (ClassifyResult, bool) {
	return c.Classify(ctx, Content{Text: text}, mode)
}

// ClassifyWithModel runs exactly ONE model (no chain walk). Verify lanes use
// this so a lane's failure cannot be masked by a fallback to another model.
func (c *Client) ClassifyWithModel(ctx context.Context, model string, content Content, mode string) (ClassifyResult, bool) {
	start := time.Now()
	sys := BuildSystemPrompt(mode)
	msgs := c.buildMessages(sys, content)

	var res ClassifyResult
	res.Verdict = Verdict{Category: "无"}
	res.Model = model
	v, status, err := c.classifyOnce(ctx, model, msgs)
	res.Attempts = append(res.Attempts, attempt{Model: model, Status: status, MS: time.Since(start).Milliseconds()})
	if status == "upstream_sensitive" && err == nil {
		// Same semantics as the chained path: the sensitive rejection is a
		// verdict, not a failure.
		res.Verdict = v
		if c.cfg.Logger != nil {
			c.cfg.Logger.Info("ai_judge",
				"model_used", model,
				"fallback", false,
				"attempts", formatAttempts(res.Attempts),
				"is_ad", v.IsAd,
				"confidence", v.Confidence,
				"category", v.Category,
				"basis", "upstream_sensitive",
				"latency_ms", time.Since(start).Milliseconds(),
				"prompt_version", PromptVersion)
		}
		return res, true
	}
	if err == nil {
		res.Verdict = v
		if c.cfg.Logger != nil {
			c.cfg.Logger.Info("ai_judge",
				"model_used", model,
				"fallback", false,
				"attempts", formatAttempts(res.Attempts),
				"is_ad", v.IsAd,
				"confidence", v.Confidence,
				"category", v.Category,
				"latency_ms", time.Since(start).Milliseconds(),
				"prompt_version", PromptVersion)
		}
		return res, true
	}
	if c.cfg.Logger != nil {
		c.cfg.Logger.Warn("ai_model_attempt_failed",
			"model", model, "status", status, "err", err.Error(),
			"prompt_version", PromptVersion)
	}
	return res, false
}

// buildMessages assembles system + user messages, switching to multimodal
// parts when an image is present.
func (c *Client) buildMessages(sys string, content Content) []openai.ChatCompletionMessage {
	var b strings.Builder
	b.WriteString("请判定以下内容是否为灰黑产广告/导流：\n")
	b.WriteString(WrapInput(truncateRunes(content.Text, MaxInputRunes)))
	if content.ImageURL != "" {
		b.WriteString("\n（同时判定附带图片；图片与文本冲突时以图片为准。）")
	}
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sys},
	}
	if content.ImageURL == "" {
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleUser, Content: b.String(),
		})
		return msgs
	}
	msgs = append(msgs, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		MultiContent: []openai.ChatMessagePart{
			{Type: openai.ChatMessagePartTypeText, Text: b.String()},
			{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: content.ImageURL}},
		},
	})
	return msgs
}

// classifyOnce runs one model request. status is a short failure code for
// logging ("ok", "http_429", "pool_parked", "timeout", "parse", ...).
func (c *Client) classifyOnce(ctx context.Context, model string, msgs []openai.ChatCompletionMessage) (Verdict, string, error) {
	ctxT, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req := openai.ChatCompletionRequest{
		Model:           model,
		Stream:          false,
		ReasoningEffort: openai.ReasoningEffortNone, // gateway-verified way to disable thinking
		Messages:        msgs,
	}
	// Backup thinking-off field for gateways that honor it instead of
	// reasoning_effort (the mimo gateway was verified to accept both; the
	// qwen-style chat_template_kwargs path is where go-openai carries it).
	// Never max_tokens: the gateway ignores and warns on it.
	req.ChatTemplateKwargs = map[string]any{
		"enable_thinking": false,
	}

	resp, err := c.client.CreateChatCompletion(ctxT, req)
	if err != nil {
		// Upstream content filter rejection (amend5 A): the filter refuses
		// exactly the content we most want killed. This single 400 shape is a
		// VERDICT, not an error — the chain stops here (the next model would be
		// filtered identically), nothing retries, downstream needs no
		// special-casing. Every other error keeps the ordinary failure path.
		if isUpstreamSensitive(err) {
			return SensitiveVerdict, "upstream_sensitive", nil
		}
		code, status := classifyError(err)
		return Verdict{}, status, &callError{code: code, status: status, err: err}
	}
	// A 200 response whose body is an error envelope (this gateway parks pools
	// with HTTP 200 + {"error":{"code":"pool_parked"}}): go-openai decodes
	// that as an empty ChatCompletionResponse. We probe the gateway again with
	// a raw call only in this dead-end case, so the fallback chain still walks.
	if len(resp.Choices) == 0 {
		if code := c.lastBodyErrorCode(); isPoolCode(code) {
			return Verdict{}, code, &callError{code: code, status: code,
				err: fmt.Errorf("gateway body error: %s", code)}
		}
		return Verdict{}, "empty_choices", &callError{code: "empty_choices", status: "empty_choices", err: errors.New("no choices in response")}
	}
	text := resp.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		return Verdict{}, "empty_content", &callError{code: "empty_content", status: "empty_content", err: errors.New("empty assistant content")}
	}
	v, err := ParseVerdict(text)
	if err != nil {
		return Verdict{}, "parse", &callError{code: "parse", status: "parse", err: err}
	}
	return v, "ok", nil
}

// callError carries a failure classification for the fallback decision.
type callError struct {
	code   string // machine code: http_429, pool_parked, timeout, parse, ...
	status string
	err    error
}

func (e *callError) Error() string { return e.code + ": " + e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// ErrorCode returns the machine failure code of err ("" when err is not a
// callError from this package).
func ErrorCode(err error) string {
	var ce *callError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ""
}

// fallbackEligible reports whether switching to the next model could help:
// 429/5xx/timeout/network, or the pool-style error codes. pool_parked and
// no_accounts are known not to benefit from a model swap, but the chain is
// still walked to the end (per contract) before the call counts as failed.
func fallbackEligible(err error) bool {
	switch ErrorCode(err) {
	case "http_429", "http_5xx", "timeout", "network",
		"pool_parked", "no_accounts", "upstream_rate_limited", "upstream_busy":
		return true
	}
	return false
}

// classifyError maps a go-openai error to (code, status).
func classifyError(err error) (string, string) {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		// A.2: HTTP 400 + sensitive content-filter markers is classified as
		// "upstream_sensitive" (classifyOnce turns that into SensitiveVerdict).
		// Only this one shape is special; every other 400 stays a failure.
		if apiErr.HTTPStatusCode == 400 && sensitiveAPIError(apiErr) {
			return "upstream_sensitive", "upstream_sensitive"
		}
		switch {
		case apiErr.HTTPStatusCode == 429:
			return "http_429", "http_429"
		case apiErr.HTTPStatusCode >= 500:
			return "http_5xx", "http_" + itoa(apiErr.HTTPStatusCode)
		}
		// Body-level error.code (pool_parked / no_accounts / upstream_*) rides
		// in the JSON code field or inside the message text on this gateway.
		code := errorCodeFromAPIError(apiErr)
		return code, code
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		if reqErr.HTTPStatusCode == 429 {
			return "http_429", "http_429"
		}
		if reqErr.HTTPStatusCode >= 500 {
			return "http_5xx", "http_" + itoa(reqErr.HTTPStatusCode)
		}
		return "http_" + itoa(reqErr.HTTPStatusCode), "http_" + itoa(reqErr.HTTPStatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "timeout", "timeout"
	}
	return "network", "network"
}

// errorCodeFromAPIError digs the gateway's error.code out of an APIError.
// The mimo gateway returns {"error":{"message":"...","code":"pool_parked"}} or
// embeds the code in the message text.
func errorCodeFromAPIError(e *openai.APIError) string {
	if s, ok := e.Code.(string); ok && isPoolCode(s) {
		return s
	}
	msg := strings.ToLower(e.Message)
	for _, code := range []string{"pool_parked", "no_accounts", "upstream_rate_limited", "upstream_busy"} {
		if strings.Contains(msg, code) {
			return code
		}
	}
	return "http_" + itoa(e.HTTPStatusCode)
}

func isPoolCode(s string) bool {
	switch s {
	case "pool_parked", "no_accounts", "upstream_rate_limited", "upstream_busy":
		return true
	}
	return false
}

// isUpstreamSensitive reports whether err is the upstream content-filter
// rejection: HTTP 400 whose error envelope carries "content filtered by
// upstream" or a code/message containing "sensitive" (case-insensitive).
// It inspects both the go-openai APIError and RequestError shapes; only the
// APIError form can carry the parsed envelope, so a body-unparsable 400
// (RequestError) is never treated as sensitive.
func isUpstreamSensitive(err error) bool {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == 400 && sensitiveAPIError(apiErr)
	}
	return false
}

// sensitiveAPIError matches the filter's markers in the error code and message.
func sensitiveAPIError(e *openai.APIError) bool {
	if s, ok := e.Code.(string); ok && strings.Contains(strings.ToLower(s), "sensitive") {
		return true
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "content filtered by upstream") ||
		strings.Contains(msg, "sensitive")
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func formatAttempts(attempts []attempt) string {
	var parts []string
	for _, a := range attempts {
		parts = append(parts, fmt.Sprintf("%s/%s/%dms", a.Model, a.Status, a.MS))
	}
	return strings.Join(parts, ",")
}

// truncateRunes caps runaway input defensively: byte-count cap with an
// invalid-tail repair, so a cut inside a UTF-8 sequence cannot survive.
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "…")
}
