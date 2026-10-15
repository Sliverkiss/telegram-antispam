package bot

// Hermes 独立验收（amend14：入群贴纸验证失效「发了贴纸仍被踢」）——不复用执行体断言。
//
// H14-1 连续多条非贴纸消息后再发贴纸 → 必须通过（直接证伪需求方的"只检查第一条"怀疑）
//       + 欢迎消息被删 + 零处置
// H14-2 竞态救援：条目已被扫描移出 pending（处置尚未认领）→ 贴纸必须救回：
//       零 ban、欢迎已删、late_pass 计数 1、pending/expiring 两个 map 都清空
// H14-3 反向保护：条目已被处置认领 → 贴纸不得救回（不被消费），ban 照落地，late_pass=0
// H14-4 到期三条路径（dispose / gone / unable_to_verify）都必须删掉欢迎消息
// H14-5 通过之后时钟大幅前进再扫描 → 绝不 ban（通过即终局）
// H14-6 跨群贴纸不得通过（条目保留、贴纸不被消费）

import (
	"context"
	"testing"

	"adbot/internal/telegram"
)

// h14Member builds the getChatMember answer for one timeout lane.
func h14Member(status string, canSend bool) telegram.ChatMember {
	return telegram.ChatMember{
		Status:               status,
		IsMember:             status != "left",
		CanSendMessages:      canSend,
		CanSendOtherMessages: canSend,
		User:                 telegram.User{ID: 14010},
	}
}

// h14JoinOne drives one no-bio joiner and returns the welcome message id we
// must expect to be deleted.
func h14JoinOne(t *testing.T, uid int64) (*Bot, *fakeTG) {
	t.Helper()
	b, tg, _ := a14Join(t, a14Cfg(), uid, "u14", "阿一")
	return b, tg
}

// h14WelcomeGone reports whether the welcome message (id 1 in the fake TG)
// was deleted.
func h14WelcomeGone(tg *fakeTG) bool {
	for _, id := range tg.deleteList() {
		if id == "1" {
			return true
		}
	}
	return false
}

// --- H14-1: 多条非贴纸消息之后再发贴纸必须通过 ------------------------------------

func TestH14MyStickerPassesAfterSeveralTextMessages(t *testing.T) {
	b, tg := h14JoinOne(t, 14001)
	ctx := context.Background()

	// 线上事件原样重放：欢迎 → "啥" → "😂" → 贴纸
	b.HandleMessage(ctx, adMsg(-500, 14001, 201, "啥"))
	b.HandleMessage(ctx, adMsg(-500, 14001, 202, "😂"))
	b.HandleMessage(ctx, stickerMsg(-500, 14001, 203))

	if n := len(tg.banList()); n != 0 {
		t.Fatalf("发了贴纸绝不能被踢：%d 次处置", n)
	}
	if !h14WelcomeGone(tg) {
		t.Fatalf("通过后必须删掉欢迎消息：deletes=%v", tg.deleteList())
	}
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("pending=%d passed=%d，want 0/1", pending, passed)
	}
	if n := b.joinVerify.latePassed(); n != 0 {
		t.Fatalf("准时贴纸不该算迟到救援：%d", n)
	}
}

// --- H14-2: 到期扫描与贴纸的竞态救援 -----------------------------------------------

func TestH14MyLateStickerRescuesInFlightEntry(t *testing.T) {
	b, tg := h14JoinOne(t, 14002)
	ctx := context.Background()

	// 模拟"扫描 tick 已把条目移出 pending、处置链条还没认领"的窗口
	a14MarkExpiring(b)
	v := b.joinVerify
	if !v.isPending(14002) {
		t.Fatal("到期处置未落地前，用户必须仍算 pending（否则贴纸会被静默忽略）")
	}

	b.HandleMessage(ctx, stickerMsg(-500, 14002, 211))

	if n := b.joinVerify.latePassed(); n != 1 {
		t.Fatalf("迟到救援计数：%d，want 1", n)
	}
	if !h14WelcomeGone(tg) {
		t.Fatalf("救援后必须删欢迎消息：deletes=%v", tg.deleteList())
	}
	v.mu.Lock()
	_, stillPending := v.pending[14002]
	_, stillExpiring := v.expiring[14002]
	v.mu.Unlock()
	if stillPending || stillExpiring {
		t.Fatalf("救援后两个状态表都必须清空：pending=%v expiring=%v", stillPending, stillExpiring)
	}

	// 处置链条随后跑到 → 认领失败 → 放弃处置
	a14RunExpiring(b)
	if n := len(tg.banList()); n != 0 {
		t.Fatalf("被贴纸救回的人绝不能被踢：%d", n)
	}
	if _, pending, _, timeouts := b.JoinVerifyStats(); pending != 0 || timeouts != 0 {
		t.Fatalf("pending=%d timeouts=%d，want 0/0", pending, timeouts)
	}
}

// --- H14-3: 已认领的条目不得被贴纸救回（反向保护） --------------------------------

func TestH14MyClaimedEntryCannotBeRescued(t *testing.T) {
	b, tg := h14JoinOne(t, 14003)
	ctx := context.Background()

	a14MarkExpiring(b)
	v := b.joinVerify
	v.mu.Lock()
	p := v.expiring[14003]
	v.mu.Unlock()
	if p == nil {
		t.Fatal("条目应在 expiring 里")
	}
	if !v.claimExpiring(p) {
		t.Fatal("首次认领必须成功")
	}
	// 处置已拥有该条目：贴纸不得被验证消费
	if v.handleStickerMessage(stickerMsg(-500, 14003, 221)) {
		t.Fatal("处置已认领后，贴纸不得再算通过")
	}
	if n := b.joinVerify.latePassed(); n != 0 {
		t.Fatalf("不该有迟到救援：%d", n)
	}
	// 到期结论照常落地
	v.expireOneClaimed(p)
	if n := len(tg.banList()); n != 1 {
		t.Fatalf("到期处置必须落地：%d", n)
	}
	if !h14WelcomeGone(tg) {
		t.Fatalf("到期路径必须删欢迎消息：deletes=%v", tg.deleteList())
	}
	_ = ctx
}

// --- H14-4: 三条到期路径都必须删欢迎消息 ------------------------------------------

func TestH14MyEveryTimeoutLaneDeletesWelcome(t *testing.T) {
	cases := []struct {
		name    string
		member  string
		canSend bool
		wantBan bool
	}{
		{"dispose 正常成员", "member", true, true},
		{"gone 人已离开", "left", true, false},
		{"unable_to_verify 被限制发言", "restricted", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, tg := h14JoinOne(t, 14010)
			tg.member[fkey(-500, 14010)] = h14Member(tc.member, tc.canSend)

			a14ExpireScan(b)

			if !h14WelcomeGone(tg) {
				t.Fatalf("到期必须删欢迎消息（%s）：deletes=%v", tc.name, tg.deleteList())
			}
			if got := len(tg.banList()); (got == 1) != tc.wantBan {
				t.Fatalf("%s：ban 次数=%d，wantBan=%v", tc.name, got, tc.wantBan)
			}
		})
	}
}

// --- H14-5: 通过即终局 ------------------------------------------------------------

func TestH14MyNoDisposalAfterPass(t *testing.T) {
	b, tg := h14JoinOne(t, 14005)

	b.HandleMessage(context.Background(), stickerMsg(-500, 14005, 231))
	if _, _, passed, _ := b.JoinVerifyStats(); passed != 1 {
		t.Fatalf("贴纸必须通过：passed=%d", passed)
	}

	// 时钟大幅前进 + 再来一次扫描：绝不处置
	a14ExpireScan(b)
	a14RunExpiring(b)
	if n := len(tg.banList()); n != 0 {
		t.Fatalf("通过后绝不能再被处置：%d", n)
	}
}

// --- H14-6: 跨群贴纸不得通过 ------------------------------------------------------

func TestH14MyCrossGroupStickerDoesNotPass(t *testing.T) {
	b, _ := h14JoinOne(t, 14006)
	v := b.joinVerify

	if v.handleStickerMessage(stickerMsg(-600, 14006, 241)) {
		t.Fatal("跨群贴纸不得算通过")
	}
	if !v.isPending(14006) {
		t.Fatal("跨群贴纸不该清掉等待条目")
	}
	if n := v.latePassed(); n != 0 {
		t.Fatalf("不该有迟到救援：%d", n)
	}
}
