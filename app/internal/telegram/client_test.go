package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer returns an httptest server emulating just enough of the Bot API
// envelope for the library, and a Client pointed at it (WithServerURL). The
// handler receives the method name (last path segment) and the parsed form.
func newTestServer(t *testing.T, handler func(method string) (any, error), skipGetMe bool) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /bot<token>/<method> (or /bot<token>/test/<method>).
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		result, err := handler(method)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			apiErr := &APIError{Code: 400, Description: err.Error(), Method: method}
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": apiErr.Code, "description": apiErr.Description})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	c, err := NewClientWithOptions("123:TEST", Options{ServerURL: srv.URL, SkipGetMe: skipGetMe})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	t.Cleanup(srv.Close)
	return c, srv
}

func TestGetMe(t *testing.T) {
	c, _ := newTestServer(t, func(method string) (any, error) {
		if method != "getMe" {
			t.Errorf("method = %s", method)
		}
		return map[string]any{"id": 123, "is_bot": true, "username": "AdBot"}, nil
	}, true)
	u, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 123 || u.Username != "AdBot" || !u.IsBot {
		t.Errorf("getMe = %+v", u)
	}
}

func TestGetChatReturnsBio(t *testing.T) {
	c, _ := newTestServer(t, func(method string) (any, error) {
		if method != "getChat" {
			t.Errorf("method = %s", method)
		}
		return map[string]any{"id": 555, "bio": "办证 加V"}, nil
	}, true)
	info, err := c.GetChat(context.Background(), 555)
	if err != nil {
		t.Fatal(err)
	}
	if info.Bio != "办证 加V" {
		t.Errorf("bio = %q", info.Bio)
	}
}

func TestAPIErrorCarriesDescription(t *testing.T) {
	c, _ := newTestServer(t, func(method string) (any, error) {
		return nil, &APIError{Code: 400, Description: "can't delete that message for everyone", Method: method}
	}, true)
	err := c.DeleteMessage(context.Background(), 1, 2)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "can't delete") {
		t.Errorf("error = %v, want description included", err)
	}
}

func TestBanUnbanDelete(t *testing.T) {
	seq := []string{}
	c, _ := newTestServer(t, func(method string) (any, error) {
		seq = append(seq, method)
		return true, nil
	}, true)
	ctx := context.Background()
	if err := c.BanChatMember(ctx, 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := c.UnbanChatMember(ctx, 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteMessage(ctx, 10, 99); err != nil {
		t.Fatal(err)
	}
	if err := c.LeaveChat(ctx, 10); err != nil {
		t.Fatal(err)
	}
	want := []string{"banChatMember", "unbanChatMember", "deleteMessage", "leaveChat"}
	if strings.Join(seq, "|") != strings.Join(want, "|") {
		t.Errorf("seq = %v", seq)
	}
}

func TestSetMyCommands(t *testing.T) {
	c, _ := newTestServer(t, func(method string) (any, error) {
		if method != "setMyCommands" {
			t.Errorf("method = %s", method)
		}
		return true, nil
	}, true)
	if err := c.SetMyCommands(context.Background(), []BotCommand{
		{Command: "start", Description: "开始"},
		{Command: "help", Description: "帮助"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSendMessage(t *testing.T) {
	c, _ := newTestServer(t, func(method string) (any, error) {
		if method != "sendMessage" {
			t.Errorf("method = %s", method)
		}
		return map[string]any{"message_id": 777}, nil
	}, true)
	id, err := c.SendMessage(context.Background(), -100, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if id != 777 {
		t.Errorf("message id = %d", id)
	}
}

// TestNotifySenderPlainText pins §2.4: the notify sender must be PLAIN TEXT.
// The library serialises sendMessage as a multipart form, and parse_mode is
// only present when non-empty — so "no parse_mode in the request body" is
// asserted exactly, and is a request-content assertion, not a render one.
func TestNotifySenderPlainText(t *testing.T) {
	var reqBody string
	var reqCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		reqCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{"message_id":777}}`))
	}))
	defer srv.Close()
	c, err := NewClientWithOptions("123:TEST", Options{ServerURL: srv.URL, SkipGetMe: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NotifySender(context.Background(), -100, "✅ 已封禁 · 90分\n📍 位置"); err != nil {
		t.Fatal(err)
	}
	// parse_mode must be absent from the multipart fields.
	if strings.Contains(reqBody, `name="parse_mode"`) {
		t.Errorf("NotifySender sent parse_mode:\n%s", reqBody)
	}
	// It must be multipart form-data (the library's transport).
	if !strings.Contains(reqCT, "multipart/form-data") {
		t.Errorf("content-type = %q", reqCT)
	}
	// The text itself must be present.
	if !strings.Contains(reqBody, "已封禁") {
		t.Errorf("text missing from request body:\n%s", reqBody)
	}
}

// TestNotifySenderNoPreview pins §2.4 link preview explicitly disabled: the
// multipart form must carry is_disabled=true for link_preview_options.
func TestNotifySenderNoPreview(t *testing.T) {
	var reqBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()
	c, err := NewClientWithOptions("123:TEST", Options{ServerURL: srv.URL, SkipGetMe: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NotifySender(context.Background(), -100, "x"); err != nil {
		t.Fatal(err)
	}
	// The LinkPreviewOptions part is a JSON object with is_disabled=true.
	if !strings.Contains(reqBody, `"is_disabled":true`) {
		t.Errorf("link preview not disabled:\n%s", reqBody)
	}
}

func TestUpdateConversion(t *testing.T) {
	raw := `{"update_id":42,"chat_member":{"chat":{"id":-100,"type":"supergroup","title":"grp"},"from":{"id":9,"is_bot":false,"first_name":"U","username":"U9"},"date":100,"old_chat_member":{"status":"left","user":{"id":9}},"new_chat_member":{"status":"member","user":{"id":9,"username":"U9"}}}}`
	out, err := convertUpdateJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.UpdateID != 42 {
		t.Errorf("update id = %d", out.UpdateID)
	}
	if out.ChatMember == nil {
		t.Fatal("chat_member not converted")
	}
	if out.ChatMember.Chat.ID != -100 || out.ChatMember.Chat.Type != "supergroup" || out.ChatMember.Chat.Title != "grp" {
		t.Errorf("chat = %+v", out.ChatMember.Chat)
	}
	if out.ChatMember.NewChatMember.Status != "member" || out.ChatMember.NewChatMember.User.Username != "U9" {
		t.Errorf("new member = %+v", out.ChatMember.NewChatMember)
	}
	if out.ChatMember.OldChatMember.Status != "left" {
		t.Errorf("old member = %+v", out.ChatMember.OldChatMember)
	}
	if out.ChatMember.From.ID != 9 {
		t.Errorf("from = %+v", out.ChatMember.From)
	}
}

func TestIsRetriable(t *testing.T) {
	if !IsRetriable(&APIError{Code: 409}) {
		t.Error("409 should be retriable")
	}
	if !IsRetriable(&APIError{Code: 502}) {
		t.Error("502 should be retriable")
	}
	if !IsRetriable(&APIError{Code: 429}) {
		t.Error("429 should be retriable")
	}
	if IsRetriable(&APIError{Code: 400}) {
		t.Error("400 should not be retriable")
	}
	if IsRetriable(&APIError{Code: 403}) {
		t.Error("403 should not be retriable")
	}
}

// --- amend6 A.1: UpdateWorkers → library WithWorkers --------------------------

// TestClientUpdateWorkersFunctional: with UpdateWorkers=4, four updates arrive
// while one handler is parked on a 400ms sleep — the other three handlers run
// CONCURRENTLY (total wall time < 4×400ms), proving the client no longer
// serializes updates through a single worker (defaultWorkers=1 in the
// library). Fully offline: an httptest fake Bot API with a fast getUpdates
// response, never Start()ing a real long-poll.
func TestClientUpdateWorkersFunctional(t *testing.T) {
	const updates = 4
	const sleep = 400 * time.Millisecond
	var inflight, maxInflight atomic.Int64
	var polls atomic.Int64

	// Own server (not newTestServer) so UpdateWorkers can be passed through
	// NewClientWithOptions — the exact path main.go takes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
			return
		}
		// Two rounds: first delivers `updates` messages, second is empty.
		if polls.Add(1) == 1 {
			msgs := make([]map[string]any, updates)
			for i := range msgs {
				msgs[i] = map[string]any{
					"update_id": i + 1,
					"message": map[string]any{"message_id": i + 1, "text": fmt.Sprintf("m%d", i),
						"chat": map[string]any{"id": -500, "type": "supergroup"},
						"from": map[string]any{"id": 7, "is_bot": false, "first_name": "F"}},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": msgs})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{}})
	}))
	defer srv.Close()
	c, err := NewClientWithOptions("123:TEST", Options{
		ServerURL: srv.URL, SkipGetMe: true, UpdateWorkers: updates,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	if got := c.UpdateWorkers(); got != updates {
		t.Fatalf("UpdateWorkers = %d, want %d", got, updates)
	}

	handlerDone := make(chan struct{}, updates)
	c.RegisterDefaultHandler(func(ctx context.Context, u Update) {
		cur := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if cur <= old || maxInflight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(sleep)
		inflight.Add(-1)
		handlerDone <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go c.Start(ctx)
	for i := 0; i < updates; i++ {
		select {
		case <-handlerDone:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("handlers did not finish")
		}
	}
	elapsed := time.Since(start)
	cancel()
	// With one worker: 4 × 400ms = 1600ms serialized. With 4 workers the four
	// handlers overlap (≈400ms + polling slack). Assert with a wide margin.
	if maxInflight.Load() < 2 {
		t.Errorf("handlers must run concurrently with UpdateWorkers=4, max inflight = %d", maxInflight.Load())
	}
	if elapsed >= 3*sleep {
		t.Errorf("elapsed = %v; updates look serialized (1 worker), want overlap", elapsed)
	}
}

// TestClientUpdateWorkersConfigured: the workers option maps to the
// configured count on the underlying library bot (unit-level).
func TestClientUpdateWorkersConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}))
	defer srv.Close()
	for _, n := range []int{1, 8, 128} {
		c, err := NewClientWithOptions("123:TEST", Options{ServerURL: srv.URL, SkipGetMe: true, UpdateWorkers: n})
		if err != nil {
			t.Fatalf("NewClientWithOptions(workers=%d): %v", n, err)
		}
		if got := c.UpdateWorkers(); got != n {
			t.Errorf("workers = %d, want %d", got, n)
		}
	}
	// 0 = library default (1) — the option is simply not passed.
	c, err := NewClientWithOptions("123:TEST", Options{ServerURL: srv.URL, SkipGetMe: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.UpdateWorkers(); got != 0 {
		t.Errorf("default workers option = %d, want 0 (library default of 1 applies)", got)
	}
}
