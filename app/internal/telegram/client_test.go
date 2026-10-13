package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
