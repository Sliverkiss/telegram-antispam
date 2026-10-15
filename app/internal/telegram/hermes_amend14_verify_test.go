package telegram

// amend14 独立验收：allowed_updates 契约。线上事故的一半根因在这里——
// main.go 此前从不配置 allowed_updates，而 go-telegram/bot 只在显式配置时
// 才发该字段，Bot API 的缺省（不传字段）不含 chat_member / my_chat_member：
// clearOnLeft（被他人踢 → 清 pending + 删欢迎 + 零处置）在生产从未触发。
//
// 用 httptest 假服务器 + 真实库轮询循环（铁律：零真实 token、零真实群）
// 钉住两件事：RequiredUpdates 的内容、以及 getUpdates 请求体确实携带
// allowed_updates（= 生产进程修复的端到端证明）。

import (
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// h14MultipartField extracts one form field from a captured multipart
// request body (the library sends getUpdates as multipart form-data). The
// bool is false when the field is ABSENT (the unconfigured-client case).
func h14MultipartField(body string, name string) (string, bool) {
	values, err := parseMultipartForm(body)
	if err != nil {
		return "", false
	}
	v, ok := values[name]
	return v, ok
}

// parseMultipartForm pulls the form fields out of a raw multipart body
// (Content-Type boundary included in the body prefix we capture — the
// boundary is recovered from the first line).
func parseMultipartForm(body string) (map[string]string, error) {
	first := body
	if i := strings.Index(body, "\r\n"); i >= 0 {
		first = body[:i]
	}
	boundary := strings.TrimPrefix(first, "--")
	if boundary == first || boundary == "" {
		return nil, io.ErrUnexpectedEOF
	}
	r := strings.NewReader(body)
	mr := multipart.NewReader(struct{ io.Reader }{r}, boundary)
	out := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return out, nil // io.EOF terminates the form; fields collected
		}
		if part.FormName() != "" {
			data, _ := io.ReadAll(part)
			out[part.FormName()] = string(data)
		}
	}
}

// T1: RequiredUpdates 必须含全部四个类型（message 打头，保证普通消息投递
// 与旧缺省逐字节一致）。
func TestH14RequiredUpdatesContent(t *testing.T) {
	got := RequiredUpdates()
	want := []string{"message", "edited_message", "chat_member", "my_chat_member"}
	if len(got) != len(want) {
		t.Fatalf("RequiredUpdates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RequiredUpdates[%d] = %q, want %q (message must stay first)", i, got[i], want[i])
		}
	}
}

// T2: 带 AllowedUpdates 的客户端的 getUpdates 请求体必须真的携带这四个
// 类型（服务端捕获请求体断言）。跑真实的库轮询循环一轮即取消。
func TestH14GetUpdatesCarriesAllowedUpdates(t *testing.T) {
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	c, err := NewClientWithOptions("123:TEST", Options{
		ServerURL: srv.URL, SkipGetMe: true,
		AllowedUpdates: RequiredUpdates(),
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go c.Start(ctx) // real library loop, offline fake server
	<-ctx.Done()
	c.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("getUpdates was never called (poll loop did not run)")
	}
	raw, ok := h14MultipartField(bodies[0], "allowed_updates")
	if !ok {
		t.Fatalf("getUpdates body must carry the allowed_updates form field:\n%s", bodies[0])
	}
	var au []string
	if err := json.Unmarshal([]byte(raw), &au); err != nil {
		t.Fatalf("allowed_updates field is not a JSON list: %v (%q)", err, raw)
	}
	has := func(s string) bool {
		for _, v := range au {
			if v == s {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"message", "edited_message", "chat_member", "my_chat_member"} {
		if !has(want) {
			t.Fatalf("allowed_updates missing %q: %v", want, au)
		}
	}
}

// T3（对照）：不配 AllowedUpdates 的客户端，getUpdates 请求体【不含】
// allowed_updates 字段 —— 这就是线上旧进程的状态，chat_member 永远不来。
func TestH14NoAllowedUpdatesFieldOmitted(t *testing.T) {
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	c, err := NewClientWithOptions("123:TEST", Options{
		ServerURL: srv.URL, SkipGetMe: true,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go c.Start(ctx)
	<-ctx.Done()
	c.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("getUpdates was never called (poll loop did not run)")
	}
	if _, ok := h14MultipartField(bodies[0], "allowed_updates"); ok {
		t.Fatalf("unconfigured client must OMIT allowed_updates (the old production state), got %q", bodies[0])
	}
}
