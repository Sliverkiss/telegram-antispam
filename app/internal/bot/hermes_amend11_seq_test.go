package bot

// Hermes：把四条链路的真实日志序列打出来（给用户肉眼核对顺序）。
// 跑法：go test ./internal/bot/ -run TestH11ZSequences -v -count=1

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/retryq"
	"adbot/internal/telegram"
)

func TestH11ZSequences(t *testing.T) {
	// ---------- (a) 有简介 → 结论非广告 → 欢迎 → 贴纸通过 ----------
	var logs strings.Builder
	cfg := verifyCfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := a11LogBot(t, tg, fa, &logs, cfg)
	a11Join(b, tg, 9201, "alice", "阿珍", "喜欢爬山")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎")
	b.HandleMessage(context.Background(), stickerMsg(-500, 9201, 900))
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (a) 结论非广告 → 欢迎 → 贴纸通过 =====\n%s", logs.String())

	// ---------- (b) 有简介 → 结论广告（必须无欢迎） ----------
	logs.Reset()
	tg2 := newFakeTG()
	fa2 := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.97)}
	b2 := a11LogBot(t, tg2, fa2, &logs, cfg)
	a11Join(b2, tg2, 9202, "bob", "阿强", "办证 加V kfw123")
	waitFor(t, func() bool { return len(tg2.banList()) >= 1 }, "处置")
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (b) 结论广告 → 直接杀，欢迎消息数=%d =====\n%s", welcomeCount(tg2), logs.String())

	// ---------- (c) 判定失败 → 重试 → 兜底非广告 → 才欢迎 ----------
	logs.Reset()
	tg3 := newFakeTG()
	fa3 := &fakeAI{fail: true}
	b3 := a11LogBot(t, tg3, fa3, &logs, cfg)
	b3.RetryQ = retryq.New(b3.RetryRound, time.Hour, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	a11Join(b3, tg3, 9203, "carol", "阿兰", "普通简介")
	time.Sleep(80 * time.Millisecond)
	fa3.fail = false
	fa3.verdict = fakeVerdict(false, "无", 0.9)
	_, _ = b3.RetryRound(context.Background(), a11RetryItem(9203, "普通简介", 1))
	waitFor(t, func() bool { return welcomeCount(tg3) == 1 }, "重试结论后的欢迎")
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (c) 判定失败 → 重试结论非广告 → 才欢迎 =====\n%s", logs.String())

	// ---------- (d) 通知丢失 + 人已不在 → 到期只清理不处置 ----------
	logs.Reset()
	tg4 := newFakeTG()
	fa4 := &fakeAI{}
	b4 := a11LogBot(t, tg4, fa4, &logs, cfg)
	a11Join(b4, tg4, 9204, "dave", "阿明", "")
	waitFor(t, func() bool { return welcomeCount(tg4) == 1 }, "欢迎")
	tg4.member[fkey(-500, 9204)] = telegram.ChatMember{
		Status: "left", User: telegram.User{ID: 9204}}
	expireNow(b4)
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (d) 通知丢失+人已不在 → 只清理不处置（ban 次数=%d）=====\n%s", len(tg4.banList()), logs.String())
}
