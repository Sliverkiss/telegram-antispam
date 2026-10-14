package bot

// E2E (amend8): real telegram.Client over real HTTP against a fake Bot API —：不信任 fakeTG 单测——用【真实 telegram.Client】
// 走真实 HTTP 打到一个假 Bot API 服务，断言：
//   1. 聊天里的命令回复确实在 TTL 到期后发出 deleteMessage（真实请求体）
//   2. 通知频道（BOT_LOG_CHAT）的推送【从不】触发 deleteMessage
//   3. BOT_MSG_TTL=0 时零 deleteMessage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/notify"
	"adbot/internal/telegram"
)

type fakeAPI struct {
	mu      sync.Mutex
	sends   []string // "chat|text"
	deletes []string // "chat:msg"
	nextID  int64
}

// asInt64: the library marshals chat_id/message_id as JSON strings ("−500").
func asInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		var n int64
		_, _ = fmt.Sscan(x, &n)
		return n
	}
	return 0
}

func (f *fakeAPI) snapshot() (sends, deletes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...), append([]string(nil), f.deletes...)
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// go-telegram/bot v1.27 sends every method as multipart/form-data.
		_ = r.ParseMultipartForm(1 << 20)
		get := func(k string) string {
			if r.MultipartForm == nil {
				return ""
			}
			if v := r.MultipartForm.Value[k]; len(v) > 0 {
				return v[0]
			}
			return ""
		}
		method := path.Base(r.URL.Path)
		t.Logf("API %s chat_id=%q text=%.40q", method, get("chat_id"), get("text"))
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage":
			chatID := asInt64(get("chat_id"))
			text := get("text")
			f.mu.Lock()
			f.nextID++
			id := f.nextID
			f.sends = append(f.sends, fmt.Sprintf("%d|%s", chatID, strings.SplitN(text, "\n", 2)[0]))
			f.mu.Unlock()
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":%d,"chat":{"id":%d,"type":"private"},"text":"ok"}}`,
				id, time.Now().Unix(), chatID)
		case "deleteMessage":
			f.mu.Lock()
			f.deletes = append(f.deletes, fmt.Sprintf("%d:%d", asInt64(get("chat_id")), asInt64(get("message_id"))))
			f.mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default: // getMe / getChat / setMyCommands / getUpdates ...
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"adbot","username":"adbot_bot"}}`)
		}
	}))
	return f, srv
}

func realClient(t *testing.T, url string) *telegram.Client {
	t.Helper()
	c, err := telegram.NewClientWithOptions("123456:FAKE-TOKEN-FOR-HERMES-E2E", telegram.Options{
		ServerURL: url, SkipGetMe: true, UpdateWorkers: 4,
	})
	if err != nil {
		t.Fatalf("real client: %v", err)
	}
	return c
}

func waitForStr(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// --- 1+3. 聊天消息真的会被删 / TTL=0 不删 ------------------------------------

func TestHermesE2E_RealClientChatMessageDeleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ttl   time.Duration
		wantD bool
	}{
		{"ttl=300ms_删", 300 * time.Millisecond, true},
		{"ttl=0_不删", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, srv := newFakeAPI(t)
			defer srv.Close()
			tg := realClient(t, srv.URL)

			cfg := defaultCfg()
			cfg.MsgTTL = tc.ttl
			cfg.AdminUsers = map[int64]bool{42: true}
			b := testBot(t, tg, &fakeAI{}, cfg)
			ctx := context.Background()

			b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 1, "/start")))
			waitForStr(t, "命令回复发出（真实 sendMessage）", func() bool {
				s, _ := api.snapshot()
				return len(s) >= 1
			})
			sends, _ := api.snapshot()
			if !strings.HasPrefix(sends[0], "-500|") {
				t.Fatalf("sendMessage chat = %q, want -500", sends[0])
			}

			// 等 TTL（给足裕量）
			time.Sleep(900 * time.Millisecond)
			_, deletes := api.snapshot()
			if tc.wantD {
				if len(deletes) != 1 || deletes[0] != "-500:1" {
					t.Fatalf("真实 deleteMessage = %v, want [-500:1]", deletes)
				}
				if _, d, _ := b.MsgTTLStats(); d != 1 {
					t.Fatalf("msg_ttl_deleted_total=%d want 1", d)
				}
			} else if len(deletes) != 0 {
				t.Fatalf("BOT_MSG_TTL=0 却发出了 deleteMessage: %v", deletes)
			}
		})
	}
}

// --- 2. 通知频道推送永不触发删除（真 Notifier + 真 HTTP client）--------------

func TestHermesE2E_RealClientNotifyChannelNeverDeleted(t *testing.T) {
	api, srv := newFakeAPI(t)
	defer srv.Close()
	tg := realClient(t, srv.URL)

	const logChat = int64(-1003999888777)
	n := notify.New(notify.Config{
		LogChat: logChat, OnPunish: true, OnError: true,
		Level: logx.Debug, RatePerMin: 6000, QueueSize: 64,
		DedupeWindow: time.Millisecond, MaxLen: 4000, TextLen: 200, Drain: time.Second,
	}, tg, logx.New(nil, logx.Debug, "text", "notify"))
	n.Start(context.Background())
	defer n.Stop()

	n.Punish(notify.PunishDetails{
		Action: "kick", UID: 555, Username: "@hermes_e2e", Category: "诈骗",
		Score: 97, ChatID: -500, Text: "加V赚钱",
	})

	waitForStr(t, "频道推送落到 BOT_LOG_CHAT（真实 sendMessage）", func() bool {
		s, _ := api.snapshot()
		for _, x := range s {
			if strings.HasPrefix(x, fmt.Sprintf("%d|", logChat)) {
				return true
			}
		}
		return false
	})

	// 推送到频道后：无论等多久，都不许出现任何以频道 id 为参数的 deleteMessage
	time.Sleep(1500 * time.Millisecond)
	sends, deletes := api.snapshot()
	for _, d := range deletes {
		if strings.HasPrefix(d, fmt.Sprintf("%d:", logChat)) {
			t.Fatalf("致命：频道推送被删了！deletes=%v (sends=%v)", deletes, sends)
		}
	}
	if len(deletes) != 0 {
		t.Fatalf("非聊天路径出现 deleteMessage: %v", deletes)
	}
	t.Logf("OK：频道推送 %d 条，deleteMessage 0 次", len(sends))
}
