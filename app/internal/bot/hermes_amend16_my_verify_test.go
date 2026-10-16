package bot

// Hermes 独立验收（"一次清理推两条"修复）——不复用执行体断言。
//
// H16-1 闸门不能成为绕过口：被处置（踢出）后再入群，必须重新筛、重新处置、再推一条
//       （即"两条推送"只能来自两次真实处置，不能来自同一次）
// H16-2 闸门不能吞掉正常路径：只有入群服务消息（没有 chat_member 孪生）时照常筛一次、推一次
// H16-3 普通广告消息（非入群路径）也必须恰好推一条

import (
	"context"
	"testing"

	"adbot/internal/store"
	"adbot/internal/telegram"
)

func TestH16MyKickedUserRejoinScreensAgain(t *testing.T) {
	cfg := defaultCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	const uid = int64(9601)
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)

	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "博彩代理 日结 上分"}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	ctx := context.Background()

	// 第一次入群：筛一次、处置一次、推一条
	b.HandleUpdate(ctx, joinServiceMsgUpdate(-500, uid, 720, "evade", "绕"))
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "第一次处置推送")
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("第一次必须处置一次: %v", got)
	}
	if got := fa.callCount(); got != 1 {
		t.Fatalf("第一次只送审一次: %d", got)
	}

	// 踢出后 Telegram 的离开事件（真实离开）
	b.HandleUpdate(ctx, memberUpdate(-500, uid, "member", "kicked"))
	// 同一入群事件的孪生 update：不得再筛、不得再推
	b.HandleUpdate(ctx, chatMemberJoinUpdate(-500, uid))
	if got := len(rec.texts()); got != 1 {
		t.Fatalf("同一入群事件的孪生 update 不得再推: %v", rec.texts())
	}
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("孪生 update 不得再处置: %v", got)
	}

	// 真实再入群（离开之后再加入）：必须重新筛、重新处置、再推一条
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	b.HandleUpdate(ctx, joinServiceMsgUpdate(-500, uid, 721, "evade", "绕"))
	waitFor(t, func() bool { return len(rec.texts()) == 2 }, "再入群必须重新处置")
	if got := tg.banList(); len(got) != 2 {
		t.Fatalf("再入群必须再次处置: %v", got)
	}
}

func TestH16MyServiceMessageOnlyStillScreens(t *testing.T) {
	cfg := defaultCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	const uid = int64(9602)
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)

	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "博彩代理 日结"}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}

	// 只投递入群服务消息（没有 chat_member 孪生）：闸门不得把它吞掉
	b.HandleUpdate(context.Background(), joinServiceMsgUpdate(-500, uid, 722, "one", "单"))
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "仅有服务消息也要筛一次")
	if got := fa.callCount(); got != 1 {
		t.Fatalf("送审次数=%d，want 1", got)
	}
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("处置=%v，want 一次 ban", got)
	}
}

func TestH16MyPlainAdMessagePushesOnce(t *testing.T) {
	cfg := defaultCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	const uid = int64(9603)
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	// 观察名单：消息车道只判定在册用户（与既有测试一致）
	if err := b.Store.AddSuspect(context.Background(), store.Suspect{UID: uid, ChatID: -500}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}

	// 群里一条普通广告消息（非入群路径）
	b.HandleMessage(context.Background(), adMsg(-500, uid, 40, "博彩 上分下分 加V aaa123"))
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "普通广告消息处置推送")
	if got := len(rec.texts()); got != 1 {
		t.Fatalf("一条广告消息必须只推一条: %v", rec.texts())
	}
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("处置=%v，want 一次", got)
	}
}
