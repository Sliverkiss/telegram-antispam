package bot

// amend14 入群验证失效（发了贴纸仍被踢）回归测试。线上事件（群
// -1003888828164）：新人发两条文本 + 一个贴纸，人被踢、欢迎消息没删。
//
// 根因（两项，均已修）：
//   R1 贴纸与到期扫描的竞态：scanDeadlines 先把到期条目从 pending 里删掉，
//      expireOne 的 getChatMember 二次确认（重试 2×1s）+ 删欢迎 + ban 一路
//      走完要数秒——这个窗口里贴纸到达 → isPending()==false →
//      handleStickerMessage 落 !ok → 普通流程（贴纸不可判，直接忽略）→
//      ban 照样落地 = 「发了贴纸仍被踢」。修复：到期条目移入 expiring，
//      贴纸仍可救（join_verify_late_pass），处置前 claim 失败即放弃。
//   R2 chat_member 更新在生产环境从未订阅：main.go 没配 allowed_updates，
//      Bot API 默认（不传该字段）不含 chat_member / my_chat_member ——
//      clearOnLeft（别人踢/自己退 → 清 pending + 删欢迎 + 零处置）线上
//      从未触发。修复：RequiredUpdates() 显式订阅。
//
// 测试分五类（TASK amend14 §4 要求）：
//   A. 连续多条非贴纸消息后再发贴纸仍必须通过（证伪「只检查第一条」）
//   B. 贴纸与到期的竞态（贴纸在扫描之后、处置落地之前 → 必须通过、绝不处置）
//   C. 通过后绝不处置（pass 之后任何路径都不再踢）
//   D. 到期必删欢迎消息（dispose/gone/unable_to_verify 三条到期路径都删）
//   E. 非相关场景零变化（普通消息/观察名单/签名库/评论简介/门限 85 等）
//
// 全部离线：fakeTG / fakeAI，不起长轮询（铁律）。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// a14Cfg arms the gate with the standard 300s deadline (expiry is driven by
// time-travel or manual scanDeadlines, never by sleeping).
func a14Cfg() config.Config {
	return verifyCfg()
}

// a14Join drives one no-bio joiner (welcome at join time; pending armed).
func a14Join(t *testing.T, cfg config.Config, uid int64, username, first string) (*Bot, *fakeTG, *fakeAI) {
	t.Helper()
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: ""}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(uid, false, username, first)}, 0)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("joiner must be pending: %d", pending)
	}
	return b, tg, fa
}

// a14LogBot builds an armed bot whose log drains into logs (assert on event
// names — the three real log sequences live in the report).
func a14LogBot(t *testing.T, tg *fakeTG, fa AIClassifier, logs *strings.Builder, cfg config.Config) *Bot {
	t.Helper()
	st := store.NewMemory()
	lg := logx.New(logs, logx.Info, "text", "bot")
	return New(st, tg, fa, nil, cfg, lg)
}

// a14ExpireScan time-travels the clock past every deadline and runs ONE
// synchronous scan: pending entries move to expiring, expireOne runs to
// completion (the fake TG answers instantly — the ban lands before return).
func a14ExpireScan(b *Bot) {
	b.joinVerify.setScanClock(func() time.Time {
		return time.Now().Add(24 * time.Hour)
	})
	b.joinVerify.scanDeadlines()
}

// a14RunExpiring claims and runs the timeout outcome for every entry
// currently parked in expiring (a14MarkExpiring left them there with no
// disposal chain attached) — the deferred half of a scan whose entries were
// already moved.
func a14RunExpiring(b *Bot) {
	v := b.joinVerify
	v.mu.Lock()
	due := make([]*pendingVerify, 0, len(v.expiring))
	for _, p := range v.expiring {
		due = append(due, p)
	}
	v.mu.Unlock()
	for _, p := range due {
		v.expireOne(p)
	}
}

// a14MarkExpiring moves every pending entry to expiring WITHOUT running
// expireOne — the exact instant after the scan tick but before the disposal
// chain takes the entry (the R1 race window). Deadlines are forced into the
// past so the late-pass accounting sees a genuine past-deadline rescue.
func a14MarkExpiring(b *Bot) {
	v := b.joinVerify
	now := v.now()
	v.mu.Lock()
	for uid, p := range v.pending {
		p.deadline = now.Add(-time.Second) // force-due for the test
		delete(v.pending, uid)
		delete(v.woken, uid)
		v.expiring[uid] = p
	}
	v.mu.Unlock()
}

// --- A. 连续多条非贴纸消息后再发贴纸仍必须通过 --------------------------------

// A.1 THE 直接证伪：两条文本（"啥"/"😂"——线上事件的原样重放）之后贴纸到，
// 必须通过。旧代码每条消息都先过贴纸判定（不是只看第一条）——这条测试
// 把该行为钉死：任何把判定改成「只看第一条」的回归都会在这里爆。
func TestA14StickerPassesAfterManyNonStickerMessages(t *testing.T) {
	cfg := a14Cfg()
	b, tg, fa := a14Join(t, cfg, 14001, "jasmine", "Jasmine24.08")
	// 线上事件重放：两条文本消息。
	b.HandleMessage(context.Background(), adMsg(-500, 14001, 11797, "啥"))
	b.HandleMessage(context.Background(), adMsg(-500, 14001, 11798, "😂"))
	if fa.callCount() != 0 {
		t.Fatalf("pending 期间两条文本都不得送 AI: %d", fa.callCount())
	}
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 1 || passed != 0 {
		t.Fatalf("文本消息不得通过也不得清 pending: pending=%d passed=%d", pending, passed)
	}
	// 第三条：贴纸 → 必须通过。
	b.HandleMessage(context.Background(), stickerMsg(-500, 14001, 11799))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("连续两条文本后的贴纸必须通过: pending=%d passed=%d", pending, passed)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("贴纸通过后绝不处置: %v", got)
	}
	// 欢迎消息被删（需求方第二条观察）。
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("通过后必须删欢迎消息: %v", got)
	}
}

// A.2 加大剂量：5 条混合非贴纸消息（文本/emoji/图片）后再贴纸 → 仍通过。
func TestA14StickerPassesAfterFiveMixedMessages(t *testing.T) {
	cfg := a14Cfg()
	b, _, _ := a14Join(t, cfg, 14002, "mixU", "小明")
	for i, m := range []telegram.Message{
		emojiMsg(-500, 14002, 200, "你好我是新来的"),
		emojiMsg(-500, 14002, 201, "🐔🎉✨"),
		verifyPhotoMsg(-500, 14002, 202),
		emojiMsg(-500, 14002, 203, "啥"),
		emojiMsg(-500, 14002, 204, "😂"),
	} {
		b.HandleMessage(context.Background(), m)
		if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
			t.Fatalf("第 %d 条非贴纸消息不得清 pending", i+1)
		}
	}
	b.HandleMessage(context.Background(), stickerMsg(-500, 14002, 205))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("5 条非贴纸后的贴纸必须通过: pending=%d passed=%d", pending, passed)
	}
}

// --- B. 贴纸与到期的竞态 --------------------------------------------------------

// B.1 THE 竞态修复：条目已过 deadline 且 scanDeadlines 已把它移入 expiring
// （处置链已起步），贴纸这时才到 → 必须按通过处理、ban 绝不落地、
// 欢迎删除。这正是线上「发了贴纸仍被踢」的机制级重放。
func TestA14RaceStickerAfterScanBeforeDisposalPasses(t *testing.T) {
	cfg := a14Cfg()
	b, tg, _ := a14Join(t, cfg, 14003, "raceU", "小竞")
	// 竞态窗口：到期条目移入 expiring，expireOne 尚未 claim。
	a14MarkExpiring(b)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("expiring 条目仍计入 pending（门必须继续拦）: %d", pending)
	}
	// 验证期间门还拦着：一条文本不得送 AI。
	b.HandleMessage(context.Background(), adMsg(-500, 14003, 300, "啥"))
	// 贴纸到达 → 晚到也救。
	b.HandleMessage(context.Background(), stickerMsg(-500, 14003, 301))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("竞态窗口内的贴纸必须通过: pending=%d passed=%d", pending, passed)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("竞态贴纸通过后绝不 ban: %v", got)
	}
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("竞态通过后必须删欢迎: %v", got)
	}
	// 之后 scanDeadlines 再跑（处置链的另一半终于醒来）→ claim 失败 → 零处置。
	a14ExpireScan(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("已被贴纸救走后到期链不得补刀: %v", got)
	}
}

// B.2 反向竞态：expireOne 已 claim（处置正在落地），贴纸后到 → 维持处置
// （不放宽——处置落地后不可回滚是既有语义，零变化）。
func TestA14RaceStickerAfterClaimStillDisposes(t *testing.T) {
	cfg := a14Cfg()
	b, tg, _ := a14Join(t, cfg, 14004, "lateU", "小晚")
	a14ExpireScan(b) // claim + dispose 完成一次
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("到期处置必须先落地: %v", got)
	}
	// 处置后的贴纸：人已被 ban，消息走到普通流程（不可判，忽略）。
	b.HandleMessage(context.Background(), stickerMsg(-500, 14004, 310))
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("处置已落地不得二次处置: %v", got)
	}
}

// B.3 late-pass 计数 + 日志：竞态贴纸通过必须 join_verify_late_pass 计一次
// （/statusz join_verify_late_pass_total = 线上事故的直接观测探针）。
func TestA14LatePassCountedAndLogged(t *testing.T) {
	cfg := a14Cfg()
	var logs strings.Builder
	tg := newFakeTG()
	tg.chat[14005] = telegram.ChatInfo{ID: 14005, Bio: ""}
	tg.member[fkey(-500, 14005)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 14005}}
	b := a14LogBot(t, tg, &fakeAI{}, &logs, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(14005, false, "cntU", "小计")}, 0)
	a14MarkExpiring(b)
	b.HandleMessage(context.Background(), stickerMsg(-500, 14005, 320))
	if got := b.JoinVerifyLatePassedTotal(); got != 1 {
		t.Fatalf("join_verify_late_pass_total = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "join_verify_late_pass") {
		t.Fatalf("应有 join_verify_late_pass 日志:\n%s", logs.String())
	}
	// 正常通过（未到期）不计 late_pass。
	b2, _, _ := a14Join(t, cfg, 14006, "normU", "小常")
	b2.HandleMessage(context.Background(), stickerMsg(-500, 14006, 321))
	if got := b2.JoinVerifyLatePassedTotal(); got != 0 {
		t.Fatalf("未到期通过不得计入 late_pass: %d", got)
	}
}

// B.4 处置链中途用户被别人踢（chat_member 到达）→ expiring 双胞胎被清，
// 随后的 claim 失败 → 零处置、零补刀（amend14: clearOnLeft 也清 expiring）。
func TestA14LeftDuringDisposalChainAbortsBan(t *testing.T) {
	cfg := a14Cfg()
	b, tg, _ := a14Join(t, cfg, 14007, "chainU", "小链")
	a14MarkExpiring(b)
	// 别的 bot 在处置链走到一半时把人踢了。
	b.HandleUpdate(context.Background(), memberUpdate(-500, 14007, "member", "kicked"))
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("处置中途被踢必须清 pending: %d", pending)
	}
	// 处置链的另一半（expireOne）醒来 → claim 失败 → 不 ban。
	a14ExpireScan(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("处置中途被踢后不得补刀: %v", got)
	}
}

// --- C. 通过后绝不处置 -----------------------------------------------------------

// C.1 pass 之后到期扫描不得处置（新竞态：pass 与 scanDeadlines 并发时，
// pass 删掉的条目绝不能被 scan 复活处置）。顺序驱动：先 pass，再扫描。
func TestA14NoDisposalAfterPassEvenWithLateScan(t *testing.T) {
	cfg := a14Cfg()
	b, tg, _ := a14Join(t, cfg, 14008, "scanU", "小扫")
	b.HandleMessage(context.Background(), stickerMsg(-500, 14008, 330))
	a14ExpireScan(b) // the scan that would have disposed them
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("通过后到期扫描绝不处置: %v", got)
	}
}

// C.2 pass 之后本方任何处置链不得因旧 pending 触发（clearDisposedUid 对
// expiring 双胞胎同样清——本方 ban 之后到期链不再二次处置）。
func TestA14OwnDisposalDuringExpiryNoDoubleKick(t *testing.T) {
	cfg := a14Cfg()
	tg := newFakeTG()
	tg.chat[14009] = telegram.ChatInfo{ID: 14009, Bio: "正常简介"}
	tg.member[fkey(-500, 14009)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 14009}}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(14009, false, "dblU", "小双")}, 0)
	// 已登记（非广告结论落地）。
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("应已登记: %d", pending)
	}
	// 到期扫描把条目标记 expiring；随后一轮迟到的同判定重试轮改判广告 →
	// 本方 punishAndClear 处置 → clearDisposedUid 必须把 expiring 也清掉。
	a14MarkExpiring(b)
	b.punishAndClear(ctx, punishInfo{
		chatID: -500, uid: 14009, username: "dblU", firstName: "小双",
		text: "迟到的广告结论",
	}, b.Cfg.Action, "广告", "迟到广告", notifyDetailsEmpty())
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("本方处置应恰好一次: %v", got)
	}
	// 到期链的另一半醒来 → claim 失败 → 不得第二次 ban。
	a14ExpireScan(b)
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("本方已处置后到期链不得补刀: %v", got)
	}
}

// --- D. 到期必删欢迎消息 -----------------------------------------------------------

// D.1 三条到期路径全部删欢迎：dispose（真封禁）、gone（人已不在）、
// unable_to_verify（被禁言）。表驱动。
func TestA14EveryTimeoutLaneDeletesWelcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		member telegram.ChatMember
	}{
		{"dispose", telegram.ChatMember{Status: "member"}},
		{"gone", telegram.ChatMember{Status: "left"}},
		{"unable_to_verify", telegram.ChatMember{
			Status: "restricted", IsMember: true,
			CanSendMessages: false, CanSendOtherMessages: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := a14Cfg()
			tg := newFakeTG()
			tg.chat[14010] = telegram.ChatInfo{ID: 14010, Bio: ""}
			// Join while still a member (the pre-registration health check
			// requires it), THEN flip the membership to the lane's state —
			// the timeout re-check is what must see it.
			tg.member[fkey(-500, 14010)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 14010}}
			b := verifyBot(t, tg, &fakeAI{}, cfg)
			b.HandleNewMembers(context.Background(), -500,
				[]telegram.User{u(14010, false, "laneU", "小路")}, 0)
			tg.member[fkey(-500, 14010)] = tc.member
			a14ExpireScan(b)
			if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
				t.Fatalf("%s 路径到期必须删欢迎: %v", tc.name, got)
			}
		})
	}
}

// D.2 处置 API 报错（USER_NOT_PARTICIPANT）→ 欢迎仍删、pending 清空、
// 不重试风暴（既有语义回归，零变化）。
func TestA14DisposalErrorStillDeletesWelcome(t *testing.T) {
	cfg := a14Cfg()
	tg := newFakeTG()
	tg.chat[14011] = telegram.ChatInfo{ID: 14011, Bio: ""}
	tg.member[fkey(-500, 14011)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 14011}}
	tg.banErr = &telegram.APIError{
		Code: 400, Description: "Bad Request: USER_NOT_PARTICIPANT", Method: "banChatMember"}
	b := verifyBot(t, tg, &fakeAI{}, cfg)
	b.HandleNewMembers(context.Background(), -500,
		[]telegram.User{u(14011, false, "errU", "小错")}, 0)
	a14ExpireScan(b)
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("处置报错照样删欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("处置报错照样清 pending: %d", pending)
	}
}

// --- E. 非相关场景零变化 -----------------------------------------------------------

// E.1 验证期间的非贴纸消息（文本/emoji/图片）：不送 AI、不删消息、不处置
// ——pending 期间「只吞不删」是设计（报告里留给需求方拍板），此处钉死
// 现状语义在 amend14 零变化。
func TestA14PendingNonStickerMessagesStillSwallowedOnly(t *testing.T) {
	cfg := a14Cfg()
	b, tg, fa := a14Join(t, cfg, 14012, "swU", "小吞")
	b.HandleMessage(context.Background(), adMsg(-500, 14012, 340, "啥"))
	b.HandleMessage(context.Background(), emojiMsg(-500, 14012, 341, "😂"))
	b.HandleMessage(context.Background(), verifyPhotoMsg(-500, 14012, 342))
	if fa.callCount() != 0 {
		t.Fatalf("pending 期间零 AI: %d", fa.callCount())
	}
	if got := tg.deleteList(); len(got) != 0 {
		t.Fatalf("pending 期间不删用户消息（现状语义零变化）: %v", got)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("pending 期间不处置: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending 不变: %d", pending)
	}
}

// E.2 纯 emoji 文本永不算通过（铁律回归在 amend14 竞态下同样成立：expiring
// 窗口里 emoji 文本也不得救）。
func TestA14ExpiringWindowEmojiTextDoesNotRescue(t *testing.T) {
	cfg := a14Cfg()
	b, tg, _ := a14Join(t, cfg, 14013, "emoU", "小表")
	a14MarkExpiring(b)
	b.HandleMessage(context.Background(), emojiMsg(-500, 14013, 350, "😂🐉"))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 1 || passed != 0 {
		t.Fatalf("expiring 窗口纯 emoji 文本不得通过: pending=%d passed=%d", pending, passed)
	}
	// 窗口过后处置照常落地（没被救 → 到期语义零变化）。
	a14RunExpiring(b)
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("没被救则到期照常处置: %v", got)
	}
}

// E.3 普通老用户（非 pending）的消息流零变化：可判内容送 AI，签名命中处置。
func TestA14NonPendingUserFlowUnchanged(t *testing.T) {
	cfg := a14Cfg()
	// 签名库路径：老用户发已知广告签名 → 直接处置（零 AI）。
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.95)}
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	const ad = "加V赚钱 上门服务 一天6k"
	if err := b.Store.AddMessageSignature(ctx, sigOf(ad), ad); err != nil {
		t.Fatal(err)
	}
	b.Store.AddSuspect(ctx, store.Suspect{UID: 14014, ChatID: -500})
	b.HandleMessage(ctx, adMsg(-500, 14014, 360, ad))
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("签名命中必须处置（零变化）: %v", got)
	}
	if fa.callCount() != 0 {
		t.Fatalf("签名命中零 AI（零变化）: %d", fa.callCount())
	}
	if got := tg.deleteList(); len(got) != 1 || got[0] != "360" {
		t.Fatalf("签名命中删触发消息（零变化）: %v", got)
	}
}

// E.4 amend12 评论简介门在 pending 门之后（顺序零变化）：非 pending 用户的
// 频道帖子评论照旧触发审简介；pending 用户的消息先被贴纸门吃掉，评论门
// 不会抢在贴纸判定前面。
func TestA14CommentScreenStillAfterStickerGate(t *testing.T) {
	// 非 pending：评论门照旧触发（零变化）。
	cfg := commentCfg()
	cfg.JoinVerify = false
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, cfg)
	commentDrive(b, tg, 14015, "我的频道 t.me/mychan", commentMsg(-500, 14015, 370, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("非 pending 用户的评论照旧审简介: %d", fa.callCount())
	}

	// pending 用户的评论（回复频道帖子）：贴纸门先吃掉（不消费就吞），
	// 评论门不触发、AI 零调用。
	cfg2 := commentCfg()
	cfg2.JoinVerify = true
	cfg2.JoinVerifyTimeout = 300 * time.Second
	cfg2.JoinVerifyScan = 5 * time.Second
	tg2 := newFakeTG()
	tg2.chat[14016] = telegram.ChatInfo{ID: 14016, Bio: ""}
	tg2.member[fkey(-500, 14016)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 14016}}
	fa2 := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b2 := verifyBot(t, tg2, fa2, cfg2)
	b2.HandleNewMembers(context.Background(), -500, []telegram.User{u(14016, false, "cmU", "小评")}, 0)
	// pending 用户发一条「回复频道帖子的评论」：贴纸门先拦（非贴纸 → 吞），
	// 绝不进评论门。
	b2.HandleMessage(context.Background(), commentMsg(-500, 14016, 371, "评论正文"))
	if fa2.callCount() != 0 {
		t.Fatalf("pending 期间评论门不得抢在贴纸门前面: %d", fa2.callCount())
	}
	if _, pending, _, _ := b2.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending 不变: %d", pending)
	}
	// 贴纸通过后：该用户的评论照旧触发评论门（恢复原流程）。
	b2.HandleMessage(context.Background(), stickerMsg(-500, 14016, 372))
	b2.HandleMessage(context.Background(), commentMsg(-500, 14016, 373, "评论正文"))
	if fa2.callCount() != 1 {
		t.Fatalf("通过后评论照旧触发: %d", fa2.callCount())
	}
}

// E.5 /statusz 新字段 join_verify_late_pass_total 渲染（零值）。
func TestA14StatuszLatePassFieldRendered(t *testing.T) {
	// server 侧渲染由 server 包的 stub 断言覆盖；这里钉 bot 侧视图适配器。
	cfg := a14Cfg()
	b, _, _ := a14Join(t, cfg, 14017, "vU", "小视")
	v := b.JoinVerifyView()
	if v.LatePassedTotal != 0 {
		t.Fatalf("初始 late_pass 必须为 0: %d", v.LatePassedTotal)
	}
	a14MarkExpiring(b)
	b.HandleMessage(context.Background(), stickerMsg(-500, 14017, 380))
	if v := b.JoinVerifyView(); v.LatePassedTotal != 1 {
		t.Fatalf("竞态通过后视图 late_pass = %d, want 1", v.LatePassedTotal)
	}
}

// notifyDetailsEmpty is a zero notifyDetails (test brevity).
func notifyDetailsEmpty() notifyDetails { return notifyDetails{} }

// --- F. 真实并发竞态（-race 探测）--------------------------------------------------

// F.1 并发压力：到期扫描 goroutine 与贴纸到达真并发（不再是手工 MarkExpiring
// 的顺序模拟）。门挡 TG 的 getChatMember 每次睡 5ms（拉长竞态窗口），扫描
// 与贴纸在同一瞬间发出；无论交错如何，结局只有两种合法：
//
//	a) 贴纸先 claim → pass（late_pass +1，零 ban）
//	b) 处置先 claim → 一次 ban（贴纸落到普通流程，忽略）
//
// 绝不合法：零处置且零通过（贴纸被吞掉又没踢 —— 旧 bug 的静默形态），
// 或双重 ban。跑 50 轮。
func TestA14ConcurrentScanVsStickerOnlyTwoOutcomes(t *testing.T) {
	for round := 0; round < 50; round++ {
		cfg := a14Cfg()
		tg := &slowMemberTG{fakeTG: newFakeTG()}
		uid := int64(14200 + round)
		tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: ""}
		b := testBot(t, tg, &fakeAI{}, cfg)
		b.HandleNewMembers(context.Background(), -500, []telegram.User{u(uid, false, "racer", "小赛")}, 0)

		// Time-travel the clock to due, then fire scan and sticker together.
		b.joinVerify.setScanClock(func() time.Time { return time.Now().Add(24 * time.Hour) })
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); b.joinVerify.scanDeadlines() }()
		go func() {
			defer wg.Done()
			b.HandleMessage(context.Background(), stickerMsg(-500, uid, int64(400+round)))
		}()
		wg.Wait()

		_, _, passed, timeouts := b.JoinVerifyStats()
		bans := len(tg.banList())
		if passed == 1 && timeouts == 0 && bans == 0 {
			continue // outcome a) — the rescue
		}
		if passed == 0 && timeouts == 1 && bans == 1 {
			continue // outcome b) — the disposal won the claim
		}
		t.Fatalf("round %d: illegal interleaving passed=%d timeouts=%d bans=%d (sticker swallowed silently?)", round, passed, timeouts, bans)
	}
}

// slowMemberTG adds latency to every GetChatMember (widens the race window
// so the concurrent test actually explores the interleavings).
type slowMemberTG struct {
	*fakeTG
}

func (f *slowMemberTG) GetChatMember(ctx context.Context, chatID, userID int64) (telegram.ChatMember, error) {
	time.Sleep(5 * time.Millisecond)
	return f.fakeTG.GetChatMember(ctx, chatID, userID)
}
