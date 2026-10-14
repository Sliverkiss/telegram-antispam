package bot

// Hermes 独立验收（用户提问：300s 超时清理人时，为什么只有频道推送、群里没有清理消息？）
// 这里把【每一条处置分支】都真跑一遍，断言群里确实收到清理公告 + 频道确实收到推送。
// 跑法：go test ./internal/bot/ -run TestH11Ann -v -count=1

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

type h11AnnGen struct {
	mu   sync.Mutex
	text string
	err  error
	n    int
}

func (g *h11AnnGen) Announce(_ context.Context, subject, category string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	if g.err != nil {
		return "", g.err
	}
	if strings.TrimSpace(g.text) != "" {
		return g.text, nil
	}
	return "🧹 " + subject + " 未通过入群验证，已被清理", nil
}

func (g *h11AnnGen) calls() int { g.mu.Lock(); defer g.mu.Unlock(); return g.n }

func h11ArmAnnouncer(t *testing.T, b *Bot, tg *fakeTG, gen *h11AnnGen) {
	t.Helper()
	b.Announcer = NewAnnouncer(tg, gen, true, 8*time.Second, time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil, b)
	t.Cleanup(b.StopAnnouncer)
}

func groupTexts(tg *fakeTG) []string { return tg.sendsSnapshot() }

// h11RecTG records (chatID, text) for every SendMessage so tests can prove
// WHICH chat a message went to (群里 vs 频道) — the fakeTG alone keeps texts
// only, which is exactly the ambiguity the user asked about.
type h11RecTG struct {
	TelegramClient
	mu  sync.Mutex
	out []string // "chatID|text"
}

func (r *h11RecTG) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	r.mu.Lock()
	r.out = append(r.out, strconv.FormatInt(chatID, 10)+"|"+text)
	r.mu.Unlock()
	return r.TelegramClient.SendMessage(ctx, chatID, text)
}

func (r *h11RecTG) sentTo(chatID int64, needle string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := strconv.FormatInt(chatID, 10) + "|"
	for _, s := range r.out {
		if strings.HasPrefix(s, prefix) && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func (r *h11RecTG) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.out...)
}

func groupHas(tg *fakeTG, needle string) bool {
	for _, s := range groupTexts(tg) {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// --- 0. 送客公告本身也走 60s 自删（用户"事后看不到群里公告"的最可能原因） ---------

func TestH11AnnAnnouncementSelfDeletesByTTL(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	cfg.MsgTTL = 100 * time.Millisecond // 压缩 60s 便于测试
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	wireTestNotifier(t, b, cfg)
	gen := &h11AnnGen{text: "🧹 TTL 测试公告"}
	h11ArmAnnouncer(t, b, tg, gen)

	a11Join(b, tg, 9305, "ttlU", "阿七", "")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎")
	expireNow(b)
	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "处置")
	waitFor(t, func() bool { return groupHas(tg, "TTL 测试公告") }, "群内公告已发")

	// 公告登记了 TTL → 到期被自动删除（欢迎消息删除 + 公告删除 = 2 条删除）
	waitFor(t, func() bool { return len(tg.deleteList()) >= 2 }, "公告按 BOT_MSG_TTL 自删")
	if sched, _, _ := b.MsgTTLStats(); sched > 1 {
		t.Logf("TTL 登记数=%d（公告在自删队列里）", sched)
	}
}

// --- 1. 300s 超时处置 → 群里必须有清理公告（本轮的争议点） --------------------

func TestH11AnnTimeoutDisposalPostsInGroup(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	fa := &fakeAI{}
	rec := &h11RecTG{TelegramClient: tg}
	b := testBot(t, rec, fa, cfg)
	notifyRec := wireTestNotifier(t, b, cfg)
	gen := &h11AnnGen{text: "🧹 阿慢 一直没发贴纸，已被清理出群"}
	h11ArmAnnouncer(t, b, tg, gen)

	a11Join(b, tg, 9301, "slowU", "阿慢", "")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎消息")

	expireNow(b) // 到期 + 人在群里 → 真处置

	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "超时真封禁")
	waitFor(t, func() bool { return groupHas(tg, "已被清理出群") }, "群内清理公告")

	// 【关键断言】公告确实发到了【群】这个 chat id（不是只发频道）
	if !rec.sentTo(-500, "已被清理出群") {
		t.Fatalf("公告没有发到群里（-500）。实际发出：%v", rec.all())
	}
	// 频道推送（四行模板）也必须同时到达
	waitFor(t, func() bool { return len(notifyRec.texts()) > 0 }, "频道推送")
	if got := notifyRec.texts(); !strings.Contains(got[0], "未验证") {
		t.Fatalf("频道推送内容不对：%v", got)
	}
	if !rec.sentTo(-500, "发一个贴纸") {
		t.Fatalf("欢迎消息也必须在群里：%v", rec.all())
	}
	if gen.calls() != 1 {
		t.Fatalf("公告应只生成 1 次：%d", gen.calls())
	}
}

// --- 2. 当 AI 生成公告失败 → 群里仍必须有一条（走模板池） ----------------------

func TestH11AnnAIFailureStillPostsInGroup(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	wireTestNotifier(t, b, cfg)
	gen := &h11AnnGen{err: errors.New("ai down")}
	h11ArmAnnouncer(t, b, tg, gen)

	a11Join(b, tg, 9302, "slowU2", "阿慢", "")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎")
	expireNow(b)
	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "处置")

	// 兜底模板池：必须提到名字（@username 或名字）
	waitFor(t, func() bool { return groupHas(tg, "slowU2") || groupHas(tg, "阿慢") },
		"AI 挂了也必须发兜底公告")
}

// --- 3. 四条件裸号必杀 → 群里也要有公告 -------------------------------------

func TestH11AnnJoinRuleDisposalPostsInGroup(t *testing.T) {
	cfg := verifyCfg()
	cfg.JoinNakedPremium = true
	cfg.LogChat = -999
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	wireTestNotifier(t, b, cfg)
	gen := &h11AnnGen{text: "🧹 阿三 这类裸号，本群不留"}
	h11ArmAnnouncer(t, b, tg, gen)

	for uid := int64(9303); uid <= 9303; uid++ {
		tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "", HasPhoto: false}
		tg.member[fkey(-500, uid)] = telegram.ChatMember{
			Status: "member", User: telegram.User{ID: uid, IsPremium: true}}
	}
	joiner := telegram.User{ID: 9303, IsPremium: false, Username: "nakedU", FirstName: "Lynne Bla"}
	b.HandleNewMembers(context.Background(), -500, []telegram.User{joiner}, 0)

	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "裸号必杀")
	waitFor(t, func() bool { return groupHas(tg, "本群不留") }, "群内公告")
	if n := welcomeCount(tg); n != 0 {
		t.Fatalf("命中的裸号不该收到欢迎：%d（群消息=%v）", n, groupTexts(tg))
	}
}

// --- 4. 发言判广告（punishAndClear 路径）→ 群里也要有公告 ---------------------

func TestH11AnnMessageAdDisposalPostsInGroup(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.96)}
	b := verifyBot(t, tg, fa, cfg)
	wireTestNotifier(t, b, cfg)
	gen := &h11AnnGen{text: "🧹 广告哥已被送客，别再来啦"}
	h11ArmAnnouncer(t, b, tg, gen)

	// 老成员发广告（不在验证名单里，但 AI 通道只判观察名单内的人）
	tg.member[fkey(-500, 9304)] = telegram.ChatMember{
		Status: "member", User: telegram.User{ID: 9304}}
	if err := b.Store.AddSuspect(context.Background(), store.Suspect{UID: 9304, ChatID: -500}); err != nil {
		t.Fatal(err)
	}
	b.HandleMessage(context.Background(), adMsg(-500, 9304, 500, "加V信 kfw123 一天6k"))

	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "发言判广告处置")
	waitFor(t, func() bool { return groupHas(tg, "别再来啦") }, "群内公告")
	if len(tg.deleteList()) == 0 {
		t.Fatalf("触发消息应被删除")
	}
}
