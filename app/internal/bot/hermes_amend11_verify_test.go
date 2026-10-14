package bot

// Hermes 独立验收（amend11 顺序修正 + 健壮性）。不采信执行体自述，自己造场景：
//   1. 判定是广告 → 群里【一条消息都不能有】（尤其不能有欢迎）
//   2. 判定失败 → 重试期间零欢迎；结论落地才欢迎，且只有【一条】（幂等）
//   3. 无简介 → 直接欢迎 + 零 AI
//   4. 我们自己先处置 → 到期【不再】处置一次
//   5. 别人踢（通知到达）→ 零处置 + 清记录 + 删欢迎
//   6. 通知丢失 + 人已不在 → 到期零处置 + skipped+1

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/retryq"
	"adbot/internal/telegram"
)

func welcomeCount(tg *fakeTG) int {
	n := 0
	for _, s := range tg.sendsSnapshot() {
		// 严格 needle：真正的欢迎文案（别被公告里的“欢迎”字样骗到）
		if strings.Contains(s, "发一个贴纸") {
			n++
		}
	}
	return n
}

// --- 1. 判定广告 → 群里一条消息都不该有 --------------------------------------

func TestH11AdVerdictSendsNothingToGroup(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.97)}
	b := verifyBot(t, tg, fa, cfg)

	a11Join(b, tg, 9101, "adU", "阿一", "办证 加V kfw123")
	waitFor(t, func() bool { return len(tg.banList()) >= 1 }, "广告简介处置")

	time.Sleep(200 * time.Millisecond) // 给欢迎/登记任何"迟到"的机会
	if n := welcomeCount(tg); n != 0 {
		t.Fatalf("判广告却发了 %d 条欢迎：%v", n, tg.sendsSnapshot())
	}
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("判广告后群里不该有任何 bot 消息，got %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("判广告不该登记验证：pending=%d", pending)
	}
}

// --- 2. 失败重试期间零欢迎；结论落地才欢迎，且只有一条 ------------------------

func TestH11RetryNoWelcomeThenExactlyOne(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := verifyBot(t, tg, fa, cfg)
	rq := retryq.New(b.RetryRound, time.Hour, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()

	a11Join(b, tg, 9102, "retryU", "阿二", "慢慢判定")

	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("判定失败期间不得发欢迎：%v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("判定失败期间不得登记：pending=%d", pending)
	}
	if rq.Len() != 1 {
		t.Fatalf("失败判定必须进重试队列：len=%d", rq.Len())
	}

	// 重试一轮 → 结论非广告 → 此刻才欢迎
	fa.fail = false
	fa.verdict = fakeVerdict(false, "无", 0.9)
	if ok, code := b.RetryRound(ctx, a11RetryItem(9102, "慢慢判定", 1)); !ok {
		t.Fatalf("重试轮应得出结论：ok=%v code=%s", ok, code)
	}
	waitFor(t, func() bool { return welcomeCount(tg) >= 1 }, "结论落地后的欢迎")

	// 再喂一轮同样的结论（模拟重复结论）→ 欢迎仍然只有一条（幂等）
	_, _ = b.RetryRound(ctx, a11RetryItem(9102, "慢慢判定", 2))
	time.Sleep(200 * time.Millisecond)
	if n := welcomeCount(tg); n != 1 {
		t.Fatalf("欢迎消息必须恰好 1 条（幂等），得到 %d 条：%v", n, tg.sendsSnapshot())
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending 应恰好 1：%d", pending)
	}
}

// --- 3. 无简介 → 直接欢迎，零 AI ---------------------------------------------

func TestH11NoBioImmediateWelcomeZeroAI(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)

	a11Join(b, tg, 9103, "noBio", "阿三", "")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "无简介直接欢迎")
	if fa.callCount() != 0 {
		t.Fatalf("无简介不该有任何 AI 简介判定，却被调 %d 次", fa.callCount())
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending 应为 1：%d", pending)
	}
}

// --- 4. 我们自己先处置 → 到期不再处置 -----------------------------------------

func TestH11OwnDisposalThenTimeoutNoSecondBan(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.99)}
	b := verifyBot(t, tg, fa, cfg)

	a11Join(b, tg, 9104, "ad2U", "阿四", "办证 加V kfw123")
	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "我们自己的处置")

	expireNow(b)
	time.Sleep(200 * time.Millisecond)

	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("我们自己处置过了，300s 到期又动手了一次：%v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("pending 应为 0：%d", pending)
	}
	if n := welcomeCount(tg); n != 0 {
		t.Fatalf("被判广告的人不该收到欢迎，却发了 %d 条", n)
	}
}

// --- 5. 别人踢（通知到达）→ 零处置 + 清记录 + 删欢迎 --------------------------

func TestH11ThirdPartyKickClearsNoDisposal(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)

	a11Join(b, tg, 9105, "victim", "阿五", "")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎")

	b.HandleUpdate(context.Background(), memberUpdate(-500, 9105, "member", "kicked"))
	time.Sleep(150 * time.Millisecond)

	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("别人踢的，我们不该再处置：%v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("pending 应清空：%d", pending)
	}
	if len(tg.deleteList()) == 0 {
		t.Fatalf("欢迎消息应被删除")
	}
}

// --- 6. 通知丢失 + 人已不在 → 到期只清理、零处置、skipped+1 ---------------------

func TestH11NotificationLostGoneRecheck(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)

	a11Join(b, tg, 9106, "lost", "阿六", "")
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎")

	// 不投任何 chat_member 通知（模拟通知丢失），但人其实已经不在群里
	tg.member[fkey(-500, 9106)] = telegram.ChatMember{Status: "left", User: telegram.User{ID: 9106}}
	expireNow(b)
	time.Sleep(150 * time.Millisecond)

	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("通知丢失 + 人已不在 → 到期必须零处置，got %v", got)
	}
	if got := b.JoinVerifySkippedTotal(); got != 1 {
		t.Fatalf("join_verify_skipped_total 应为 1：%d", got)
	}
	if _, pending, _, timeouts := b.JoinVerifyStats(); pending != 0 || timeouts != 0 {
		t.Fatalf("状态不对：pending=%d timeouts=%d", pending, timeouts)
	}
	if len(tg.deleteList()) == 0 {
		t.Fatalf("欢迎消息应被清理")
	}
}
