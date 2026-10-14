package bot

// Hermes：amend12 三条链路的真实日志序列（给肉眼核对顺序与零 AI 断言）。
// 跑法：go test ./internal/bot/ -run TestH12ZSequences -v -count=1

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/telegram"
)

func TestH12ZSequences(t *testing.T) {
	// ---------- (a) 频道帖子评论 + 简介带链接 → 送审 → 判广告 → 处置 ----------
	var logs strings.Builder
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := a12LogBot(t, tg, fa, &logs)
	a12Comment(b, tg, 9301, "办证 加V t.me/xxx", 1001, "这是广告")
	waitFor(t, func() bool { return len(tg.banList()) >= 1 }, "判广告处置")
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (a) 评论+简介带链接 → 送审 → 判广告 → 处置（AI 调用=%d）=====\n%s",
		fa.callCount(), logs.String())

	// ---------- (b) 判非广告 → 零处置 ----------
	logs.Reset()
	tg2 := newFakeTG()
	fa2 := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b2 := a12LogBot(t, tg2, fa2, &logs)
	a12Comment(b2, tg2, 9302, "个人主页 https://me.example", 1002, "正常评论")
	waitFor(t, func() bool { return fa2.callCount() == 1 }, "送审")
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (b) 判非广告 → 零处置（ban=%d, 删消息=%d, AI 调用=%d）=====\n%s",
		len(tg2.banList()), len(tg2.deletes), fa2.callCount(), logs.String())

	// ---------- (c) 防刷缓存命中 → 直接踢、零 AI ----------
	logs.Reset()
	tg3 := newFakeTG()
	fa3 := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b3 := a12LogBot(t, tg3, fa3, &logs)
	a12Comment(b3, tg3, 9303, "办证 加V t.me/xxx", 1003, "广告评论")
	waitFor(t, func() bool { return len(tg3.banList()) >= 1 }, "首次判广告")
	// 同一判广告简介的重复（含混淆变体）→ 30 分钟内直接踢、零 AI。
	a12Comment(b3, tg3, 9304, "办证​ 加Ｖ t.me/xxx", 1004, "广告评论")
	waitFor(t, func() bool { return len(tg3.banList()) >= 2 }, "防刷命中踢出")
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (c) 防刷命中 → 直接踢、零 AI（AI 调用=%d, ban=%d）=====\n%s",
		fa3.callCount(), len(tg3.banList()), logs.String())
	if fa3.callCount() != 1 {
		t.Fatalf("链路 (c) 必须零 AI：第二次命中不得再送审, AI 调用=%d", fa3.callCount())
	}
}

// a12LogBot builds an armed comment-screen bot whose log drains into logs.
func a12LogBot(t *testing.T, tg *fakeTG, fa *fakeAI, logs *strings.Builder) *Bot {
	t.Helper()
	return a11LogBot(t, tg, fa, logs, commentCfg())
}

// a12Comment drives one channel-post comment: fixed chat + uid + bio + text.
func a12Comment(b *Bot, tg *fakeTG, uid int64, bio string, msgID int64, text string) {
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: bio}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	b.HandleMessage(context.Background(), commentMsg(-500, uid, msgID, text))
}
