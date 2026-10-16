package bot

// Join-dispatch single-screening contract (the "一次清理推送两条" fix).
//
// One physical Telegram join is reported through TWO updates at once once a
// bot subscribes to chat_member (amend14 put it into allowed_updates):
//
//	a message update carrying new_chat_members (the join service message), and
//	a chat_member update with old=left/"" → new=member.
//
// Both used to funnel into HandleNewMembers → screenNewMember, so every
// join-triggered disposal ran its WHOLE chain twice: two banChatMember calls,
// two disposition pushes to the notification channel, two kick announcements
// (different dedupe keys: messageID from the service message vs 0 from
// chat_member), and a re-added watchlist record.
//
// The contract pinned here:
//   - joins are screened EXACTLY ONCE — from the new_chat_members service
//     message (the richer source: it carries the join message id for
//     BOT_DELETE_JOIN_MSG). chat_member describes the same event and must not
//     screen it again;
//   - the leave-side job of chat_member (amend11 B.1 / amend14: anyone removes
//     a pending joiner → clear pending + delete welcome, zero disposal) is
//     untouched;
//   - one disposal → exactly ONE channel push, ONE ban, ONE in-group
//     announcement.

import (
	"context"
	"testing"
	"time"

	"adbot/internal/telegram"
)

// joinServiceMsgUpdate builds the new_chat_members service-message update of
// one join (message id msgID; From is the joiner — the canonical shape).
func joinServiceMsgUpdate(chatID, uid, msgID int64, username, first string) telegram.Update {
	joiner := u(uid, false, username, first)
	return telegram.Update{
		UpdateID: 100,
		Message: &telegram.Message{
			MessageID:      msgID,
			From:           &joiner,
			Chat:           telegram.Chat{ID: chatID, Type: "supergroup"},
			NewChatMembers: []telegram.User{joiner},
		},
	}
}

// chatMemberJoinUpdate builds the chat_member twin of the same join
// (old=left → new=member), as Telegram delivers alongside the service message.
func chatMemberJoinUpdate(chatID, uid int64) telegram.Update {
	return telegram.Update{
		UpdateID: 101,
		ChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: chatID, Type: "supergroup"},
			From:          telegram.User{ID: uid},
			OldChatMember: telegram.ChatMember{Status: "left", User: telegram.User{ID: uid}},
			NewChatMember: telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}},
		},
	}
}

// --- 1. one join, both twin updates (either order) → ONE disposal chain ------

func TestJoinDispatchTwinUpdatesSingleDisposal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(b *Bot, ctx context.Context)
		other func(b *Bot, ctx context.Context)
	}{
		{"service_message_first", func(b *Bot, ctx context.Context) {
			b.HandleUpdate(ctx, joinServiceMsgUpdate(-500, 9501, 700, "twinU", "阿双"))
		}, func(b *Bot, ctx context.Context) {
			b.HandleUpdate(ctx, chatMemberJoinUpdate(-500, 9501))
		}},
		{"chat_member_first", func(b *Bot, ctx context.Context) {
			b.HandleUpdate(ctx, chatMemberJoinUpdate(-500, 9502))
		}, func(b *Bot, ctx context.Context) {
			b.HandleUpdate(ctx, joinServiceMsgUpdate(-500, 9502, 701, "twinU", "阿双"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultCfg()
			cfg.LogChat = -999
			tg := newFakeTG()
			fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.95)}
			b := testBot(t, tg, fa, cfg)
			rec := wireTestNotifier(t, b, cfg)
			gen := &h11AnnGen{text: "🧹 双推测试 已被清理"}
			h11ArmAnnouncer(t, b, tg, gen)

			// Seed the joiner's chat/member records the fake needs.
			tg.chat[9501] = telegram.ChatInfo{ID: 9501, Bio: "博彩代理 日结 上分"}
			tg.chat[9502] = telegram.ChatInfo{ID: 9502, Bio: "博彩代理 日结 上分"}
			tg.member[fkey(-500, 9501)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 9501}}
			tg.member[fkey(-500, 9502)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 9502}}

			ctx := context.Background()
			tc.first(b, ctx)
			tc.other(b, ctx)

			// Channel push: exactly ONE disposition notification.
			waitFor(t, func() bool { return len(rec.texts()) == 1 }, "频道推送恰好一条")
			time.Sleep(150 * time.Millisecond) // let any illegal second chain land
			if got := rec.texts(); len(got) != 1 {
				t.Fatalf("一次处置必须只推送一条频道通知，got %d:\n%v", len(got), got)
			}
			// Disposal: exactly ONE ban (action=ban, no unban).
			if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
				t.Fatalf("一次处置必须只 ban 一次, got %v", got)
			}
			// In-group announcement: generated and sent exactly once.
			waitFor(t, func() bool { return gen.calls() == 1 }, "公告只生成一次")
			time.Sleep(150 * time.Millisecond)
			if got := gen.calls(); got != 1 {
				t.Fatalf("一次处置只生成一条群内公告, got %d", got)
			}
			if !groupHas(tg, "已被清理") {
				t.Fatalf("群内公告应按原规则发出: %v", groupTexts(tg))
			}
			// The judgment ran exactly once (AI lane, not a signature re-hit).
			if got := fa.callCount(); got != 1 {
				t.Fatalf("同一次入群只送审一次, got %d", got)
			}
		})
	}
}

// --- 2. chat_member join transitions alone never screen ----------------------

func TestChatMemberJoinUpdateDoesNotScreen(t *testing.T) {
	cfg := defaultCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	const uid = int64(9503)
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: "博彩代理 日结 上分"}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)

	b.HandleUpdate(context.Background(), chatMemberJoinUpdate(-500, uid))

	time.Sleep(150 * time.Millisecond)
	if got := fa.callCount(); got != 0 {
		t.Fatalf("chat_member 入群事件不得触发 AI 送审, got %d", got)
	}
	if n, _ := b.Store.SuspectCount(context.Background()); n != 0 {
		t.Fatalf("chat_member 入群事件不得登记观察名单, suspects=%d", n)
	}
	if got := rec.texts(); len(got) != 0 {
		t.Fatalf("chat_member 入群事件不得推送频道通知, got %d", len(got))
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("chat_member 入群事件不得处置, got %v", got)
	}
}

// --- 3. chat_member leave-side cleanup unchanged (amend14's actual goal) -----

func TestChatMemberLeaveCleanupUnchanged(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	const uid = int64(9504)
	tg.chat[uid] = telegram.ChatInfo{ID: uid}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	b := verifyBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()

	// No-bio join via the service message → welcome + pending (the only
	// screening path).
	b.HandleUpdate(ctx, joinServiceMsgUpdate(-500, uid, 710, "lvU", "小离"))
	waitFor(t, func() bool { return welcomeCount(tg) == 1 }, "欢迎消息")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("应已登记 pending=%d", pending)
	}

	// Someone else kicks them: chat_member member→kicked → clear + delete
	// welcome, ZERO disposal (unchanged amend11 B.1 semantics).
	b.HandleUpdate(ctx, memberUpdate(-500, uid, "member", "kicked"))
	waitFor(t, func() bool {
		_, pending, _, _ := b.JoinVerifyStats()
		return pending == 0
	}, "pending 清空")
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("被他人踢走不得由我们处置, got %v", got)
	}
	if got := tg.deleteList(); len(got) == 0 {
		t.Fatalf("欢迎消息应被删除")
	}
}
