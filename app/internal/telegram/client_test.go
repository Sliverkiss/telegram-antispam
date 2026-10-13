package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestClient returns a client pointed at an httptest server whose handler
// routes method calls from the form body's method query param… but Bot API
// encodes the method in the URL path, so we parse the path instead.
func newTestServer(t *testing.T, handler func(method string, form url.Values) (any, error)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path = /bot<token>/<method>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 2 {
			w.WriteHeader(404)
			return
		}
		method := parts[1]
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		result, err := handler(method, r.Form)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	c := NewClient("TOKEN")
	c.Base = srv.URL
	t.Cleanup(srv.Close)
	return c
}

func writeErr(w http.ResponseWriter, err error) {
	var api *APIError
	if cause := errAs(err); cause != nil {
		api = cause
	}
	code := 400
	desc := err.Error()
	if api != nil {
		code = api.Code
		desc = api.Description
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": code, "description": desc})
}

func errAs(err error) *APIError {
	if api, ok := err.(*APIError); ok {
		return api
	}
	return nil
}

func TestGetMe(t *testing.T) {
	c := newTestServer(t, func(method string, form url.Values) (any, error) {
		if method != "getMe" {
			t.Errorf("method = %s", method)
		}
		return map[string]any{"id": 123, "is_bot": true, "username": "AdBot"}, nil
	})
	u, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 123 || u.Username != "AdBot" {
		t.Errorf("getMe = %+v", u)
	}
}

func TestGetUpdatesParams(t *testing.T) {
	c := newTestServer(t, func(method string, form url.Values) (any, error) {
		if method != "getUpdates" {
			t.Errorf("method = %s", method)
		}
		if form.Get("offset") != "42" || form.Get("timeout") != "25" || form.Get("allowed_updates") != "message,chat_member" {
			t.Errorf("form = %v", form)
		}
		return []any{}, nil
	})
	_, err := c.GetUpdates(context.Background(), 42, 25, []string{"message", "chat_member"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetChatReturnsBio(t *testing.T) {
	c := newTestServer(t, func(method string, form url.Values) (any, error) {
		if method != "getChat" || form.Get("chat_id") != "555" {
			t.Errorf("method=%s form=%v", method, form)
		}
		return map[string]any{"id": 555, "bio": "办证 加V"}, nil
	})
	info, err := c.GetChat(context.Background(), 555)
	if err != nil {
		t.Fatal(err)
	}
	if info.Bio != "办证 加V" {
		t.Errorf("bio = %q", info.Bio)
	}
}

func TestAPIErrorCarriesDescription(t *testing.T) {
	c := newTestServer(t, func(method string, form url.Values) (any, error) {
		return nil, &APIError{Code: 400, Description: "can't delete that message for everyone", Method: method}
	})
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
	c := newTestServer(t, func(method string, form url.Values) (any, error) {
		seq = append(seq, method+":"+form.Get("chat_id")+":"+form.Get("user_id")+":"+form.Get("message_id"))
		return true, nil
	})
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
	want := []string{"banChatMember:10:20:", "unbanChatMember:10:20:", "deleteMessage:10::99"}
	if strings.Join(seq, "|") != strings.Join(want, "|") {
		t.Errorf("seq = %v", seq)
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
}
