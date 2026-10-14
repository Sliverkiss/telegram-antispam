package bot

// Hermes 独立验收（amend10 入群验证）。不依赖执行体写的测试，重点验用户最关心的：
//   1. 【只认贴纸】纯 emoji 文本 / 文字 都不算通过（必须有回归）
//   2. 300s 超时 → 真封禁（ban 一次、不 unban）+ 通知 4 行 + 群内 AI 公告，且不重复处置
//   3. 欢迎消息不登记 60 秒 TTL（命令回复仍登记）
//   4. 验证未通过期间不送 AI；通过后恢复（有简介 → 判简介）
//   5. 别群贴纸不顶用；退群清理 pending；开关关闭回原流程

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/telegram"
)

// --- 1. 只认贴纸（含 emoji 不算通过的回归）-----------------------------------

func TestHermesJoinVerifyStickerOnly(t *testing.T) {
	const uid = int64(8001)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "养鸡三年"}
	cfg := defaultCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 3 * time.Second
	cfg.JoinVerifyScan = 50 * time.Millisecond
	cfg.MsgTTL = 0
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.StartJoinVerify()
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "newbie", "小明")}, 0)

	if enabled, pending, _, _ := b.JoinVerifyStats(); !enabled || pending != 1 {
		t.Fatalf("入群应进入 pending：enabled=%v pending=%d", enabled, pending)
	}
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) >= 1 }, "欢迎消息")
	if s := tg.sendsSnapshot()[0]; !strings.Contains(s, "欢迎") || !strings.Contains(s, "贴纸") {
		t.Fatalf("欢迎文案不对: %q", s)
	}
	if fa.callCount() != 0 {
		t.Fatalf("验证期间不该送 AI，却被调 %d 次", fa.callCount())
	}
	if sched, _, _ := b.MsgTTLStats(); sched != 0 {
		t.Fatalf("欢迎消息不该登记 60 秒 TTL，scheduled=%d", sched)
	}

	// 纯 emoji 文本 —— 用户明确要求【不算通过】
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, uid, 11, "🐔🎉✨")))
	time.Sleep(80 * time.Millisecond)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("【口径被放宽】纯 emoji 文本竟然算通过了")
	}
	// 普通文字 —— 也不算
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, uid, 12, "你好我是新来的")))
	time.Sleep(80 * time.Millisecond)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("普通文字竟然算通过了")
	}

	// 贴纸 —— 通过
	sm := adMsg(-500, uid, 13, "")
	sm.Sticker = &telegram.Sticker{FileID: "st1", FileUniqueID: "stu1"}
	b.HandleUpdate(ctx, cmdUpdate(sm))

	waitFor(t, func() bool {
		_, pending, passed, _ := b.JoinVerifyStats()
		return pending == 0 && passed == 1
	}, "贴纸通过（pending 清空 + passed=1）")
	waitFor(t, func() bool { return len(tg.deleteList()) >= 1 }, "通过后删欢迎消息")
	// 通过后恢复原流程：有简介 → 送 AI 判简介
	waitFor(t, func() bool { return fa.callCount() == 1 }, "通过后恢复 AI 审核")
}

// --- 2. 超时：真封禁 + 通知 + 公告 + 不重复处置 -----------------------------

func TestHermesJoinVerifyTimeoutBansOnce(t *testing.T) {
	const uid = int64(8002)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid} // 无简介
	cfg := defaultCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 200 * time.Millisecond
	cfg.JoinVerifyScan = 50 * time.Millisecond
	cfg.JoinVerifyAction = "ban"
	cfg.LogChat = -999
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	ann := &fakeAnnouncer{out: "🚪 小张 已被请出门外，广告请去别处发～"}
	b.Announcer = NewAnnouncer(tg, ann, true, time.Second, time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil, b)
	b.StartJoinVerify()
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "slowU", "小张")}, 0)

	waitFor(t, func() bool { return len(tg.banList()) >= 1 }, "超时封禁")
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("超时必须【真封禁】= 仅一次 ban（不带 unban），got %v", got)
	}

	// 通知频道：4 行模板 + 证据 + 100 分 + 已封禁
	waitFor(t, func() bool { return len(rec.texts()) >= 1 }, "通知推送")
	nb := rec.texts()[0]
	for _, frag := range []string{"⏱️ 处理时间:", "💬 🔎 入群 300 秒未验证", "👤 @slowU", "✅ 已封禁 · 100分"} {
		if !strings.Contains(nb, frag) {
			t.Errorf("通知缺 %q:\n%s", frag, nb)
		}
	}
	if n := strings.Count(nb, "\n"); n != 3 {
		t.Errorf("通知必须 4 行，got %d 行:\n%s", n+1, nb)
	}

	// 群内 AI 公告
	waitFor(t, func() bool {
		for _, s := range tg.sendsSnapshot() {
			if strings.Contains(s, "请出门外") {
				return true
			}
		}
		return false
	}, "群内 AI 公告")

	// 不重复处置 + 状态清空
	time.Sleep(400 * time.Millisecond)
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("重复处置了！bans=%v", got)
	}
	if _, pending, _, timeouts := b.JoinVerifyStats(); pending != 0 || timeouts != 1 {
		t.Fatalf("状态不对：pending=%d timeouts=%d", pending, timeouts)
	}
	if len(rec.texts()) != 1 {
		t.Errorf("通知重复推送：%d 条", len(rec.texts()))
	}
}

// --- 3. 欢迎消息不进 TTL；命令回复照旧进 -------------------------------------

func TestHermesJoinVerifyWelcomeSkipsGlobalTTL(t *testing.T) {
	const uid = int64(8003)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid}
	cfg := defaultCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 5 * time.Second
	cfg.JoinVerifyScan = 50 * time.Millisecond
	cfg.MsgTTL = 50 * time.Millisecond
	cfg.AdminUsers = map[int64]bool{42: true}
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.StartJoinVerify()
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "newbie", "小明")}, 0)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) >= 1 }, "欢迎消息")
	time.Sleep(250 * time.Millisecond) // 远超 50ms 的全局 TTL

	if sched, _, _ := b.MsgTTLStats(); sched != 0 {
		t.Fatalf("欢迎消息登记了 TTL（scheduled=%d）——它必须活到验证结束", sched)
	}
	if got := tg.deleteList(); len(got) != 0 {
		t.Fatalf("欢迎消息被删了：%v", got)
	}

	// 对照：普通命令回复照旧走 60 秒 TTL
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 21, "/start")))
	waitFor(t, func() bool { s, _, _ := b.MsgTTLStats(); return s == 1 }, "命令回复登记 TTL")
	waitFor(t, func() bool { return len(tg.deleteList()) >= 1 }, "命令回复到期删除")
}

// --- 4. 验证期间不送 AI；通过后恢复 ------------------------------------------

func TestHermesJoinVerifyGatesAIUntilPassed(t *testing.T) {
	const uid = int64(8004)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "养鸡专业户 联系方式见群"}
	cfg := defaultCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 3 * time.Second
	cfg.JoinVerifyScan = 50 * time.Millisecond
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.StartJoinVerify()
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "gate", "阿强")}, 0)
	time.Sleep(120 * time.Millisecond)
	if fa.callCount() != 0 {
		t.Fatalf("入群简介不该在验证通过前送 AI，却被调 %d 次", fa.callCount())
	}
	// 验证期间发广告文本也不判
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, uid, 31, "加V赚钱 一天6k 懂的来")))
	time.Sleep(120 * time.Millisecond)
	if fa.callCount() != 0 {
		t.Fatalf("验证期间的消息不该送 AI，却被调 %d 次", fa.callCount())
	}
	// 贴纸通过 → 恢复（简介判定）
	sm := adMsg(-500, uid, 32, "")
	sm.Sticker = &telegram.Sticker{FileID: "st2", FileUniqueID: "stu2"}
	b.HandleUpdate(ctx, cmdUpdate(sm))
	waitFor(t, func() bool { return fa.callCount() >= 1 }, "通过后恢复 AI 审核")
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("通过状态不对：pending=%d passed=%d", pending, passed)
	}
}

// --- 5. 别群贴纸不管用 -------------------------------------------------------

func TestHermesJoinVerifyStickerOtherChatIgnored(t *testing.T) {
	const uid = int64(8005)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid}
	cfg := defaultCfg()
	cfg.Chats = map[int64]bool{-500: true, -600: true}
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 5 * time.Second
	cfg.JoinVerifyScan = 50 * time.Millisecond
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.StartJoinVerify()
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "x", "小明")}, 0)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending=%d want 1", pending)
	}
	sm := adMsg(-600, uid, 41, "")
	sm.Sticker = &telegram.Sticker{FileID: "st3", FileUniqueID: "stu3"}
	b.HandleUpdate(ctx, cmdUpdate(sm))
	time.Sleep(100 * time.Millisecond)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("别的群的贴纸竟然让本群通过了")
	}
}

// --- 6. 退群清理 pending（不处置空气）---------------------------------------

func TestHermesJoinVerifyLeaveClearsPending(t *testing.T) {
	const uid = int64(8006)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid}
	cfg := defaultCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 5 * time.Second
	cfg.JoinVerifyScan = 50 * time.Millisecond
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.StartJoinVerify()
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "gone", "小明")}, 0)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending=%d want 1", pending)
	}
	cmu := telegram.ChatMemberUpdate{
		Chat:          telegram.Chat{ID: -500},
		OldChatMember: telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}},
		NewChatMember: telegram.ChatMember{Status: "left", User: telegram.User{ID: uid}},
	}
	b.HandleUpdate(ctx, telegram.Update{UpdateID: 99, ChatMember: &cmu})
	time.Sleep(100 * time.Millisecond)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("退群后 pending 应清空，got %d", pending)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("退群的人不该被处置，got %v", got)
	}
}

// --- 7. 开关关闭 = 完全回原流程 ----------------------------------------------

func TestHermesJoinVerifyDisabled(t *testing.T) {
	const uid = int64(8007)
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "养鸡专业户"}
	cfg := defaultCfg()
	cfg.JoinVerify = false
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.StartJoinVerify() // 开关关时必须是 no-op
	defer b.StopJoinVerify()
	ctx := context.Background()

	b.HandleNewMembers(ctx, -500, []telegram.User{u(uid, false, "normal", "阿强")}, 0)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("开关关闭却登记了 pending=%d", pending)
	}
	for _, s := range tg.sendsSnapshot() {
		if strings.Contains(s, "贴纸") {
			t.Fatalf("开关关闭却发了欢迎消息: %q", s)
		}
	}
	waitFor(t, func() bool { return fa.callCount() == 1 }, "开关关闭时应照旧送 AI 判简介")
}
