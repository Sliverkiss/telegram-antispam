package bot

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// fakeTG records every call for sequence assertions.
type fakeTG struct {
	chat    map[int64]telegram.ChatInfo
	member  map[string]telegram.ChatMember // "chat:uid"
	bans    []string                       // "ban" / "unban"
	deletes []string                       // message ids deleted (as string)
	sends   []string
	leaves  []int64
	seq     []string
}

func newFakeTG() *fakeTG {
	return &fakeTG{chat: map[int64]telegram.ChatInfo{}, member: map[string]telegram.ChatMember{}}
}

func (f *fakeTG) GetChatMember(_ context.Context, chatID, userID int64) (telegram.ChatMember, error) {
	f.seq = append(f.seq, "getchatmember")
	if m, ok := f.member[fkey(chatID, userID)]; ok {
		return m, nil
	}
	return telegram.ChatMember{Status: "member"}, nil
}
func (f *fakeTG) GetChat(_ context.Context, chatID int64) (telegram.ChatInfo, error) {
	f.seq = append(f.seq, "getchat")
	return f.chat[chatID], nil
}
func (f *fakeTG) BanChatMember(_ context.Context, chatID, userID int64) error {
	f.seq = append(f.seq, "ban")
	f.bans = append(f.bans, "ban")
	return nil
}
func (f *fakeTG) UnbanChatMember(_ context.Context, chatID, userID int64) error {
	f.seq = append(f.seq, "unban")
	f.bans = append(f.bans, "unban")
	return nil
}
func (f *fakeTG) DeleteMessage(_ context.Context, chatID, messageID int64) error {
	f.seq = append(f.seq, "delete")
	f.deletes = append(f.deletes, strconv.FormatInt(messageID, 10))
	return nil
}
func (f *fakeTG) SendMessage(_ context.Context, chatID int64, text string) (int64, error) {
	f.seq = append(f.seq, "send")
	f.sends = append(f.sends, text)
	return 1, nil
}
func (f *fakeTG) LeaveChat(_ context.Context, chatID int64) error {
	f.seq = append(f.seq, "leave")
	f.leaves = append(f.leaves, chatID)
	return nil
}

func fkey(chatID, userID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(userID, 10)
}

func fakeClassify(v ai.Verdict, ok bool) Classifier {
	return func(context.Context, string) (ai.Verdict, bool) { return v, ok }
}

func testBot(t *testing.T, tg *fakeTG, classify Classifier, cfg config.Config) *Bot {
	t.Helper()
	st := store.NewMemory()
	lg := logx.New(nil, logx.Debug, "text", "bot")
	return New(st, tg, classify, cfg, lg)
}

func defaultCfg() config.Config {
	return config.Config{
		Action:       "ban",
		IgnoreAdmins: true,
		LogChat:      0,
		Chats:        map[int64]bool{-100: true, -200: true, -300: true, -400: true, -500: true, -600: true, -700: true, -800: true, -900: true, -10: true, -1000: true},
		Store:        "memory",
		Version:      "test",
	}
}

func u(id int64, isBot bool, username, first string) telegram.User {
	return telegram.User{ID: id, IsBot: isBot, Username: username, FirstName: first}
}

func TestJoinAdBioDirectDisposal(t *testing.T) {
	tg := newFakeTG()
	tg.chat[100] = telegram.ChatInfo{ID: 100, Bio: "专业办理各种证件 加V信 xxx"}
	cfg := defaultCfg()
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	if err := b.Store.AddBioSignature(context.Background(), normalize.Signature("专业办理各种证件 加V信 xxx"), "专业办理各种证件 加V信 xxx"); err != nil {
		t.Fatal(err)
	}
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(100, false, "spammer", "Spammer")}, 0)

	if len(tg.bans) != 1 || tg.bans[0] != "ban" {
		t.Fatalf("bans = %v, want one ban", tg.bans)
	}
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("AI must not run on signature hit: %d", aiCalls)
	}
}

func TestJoinNormalBioKeepsWatchlistOnly(t *testing.T) {
	tg := newFakeTG()
	tg.chat[200] = telegram.ChatInfo{ID: 200, Bio: "我是正常人"}
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: false, Category: "none"}, true), defaultCfg())
	b.HandleNewMembers(context.Background(), -200, []telegram.User{u(200, false, "normal", "Normal")}, 0)
	if len(tg.bans) != 0 {
		t.Fatalf("normal bio must not dispose: %v", tg.bans)
	}
	if ok, _ := b.Store.IsSuspect(context.Background(), 200); !ok {
		t.Fatal("normal user should stay on watchlist")
	}
}

func TestJoinAdBioWritesSignature(t *testing.T) {
	tg := newFakeTG()
	tg.chat[300] = telegram.ChatInfo{ID: 300, Bio: "满18进 在线发牌"}
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true, Category: "gambling", Confidence: 0.9, Reason: "赌博"}, true), defaultCfg())
	b.HandleNewMembers(context.Background(), -300, []telegram.User{u(300, false, "g", "G")}, 0)
	if len(tg.bans) != 1 {
		t.Fatalf("ad bio must ban: %v", tg.bans)
	}
	if ok, _ := b.Store.HasBioSignature(context.Background(), normalize.Signature("满18进 在线发牌")); !ok {
		t.Fatal("ad bio should be persisted to signature library")
	}
	if sus, _ := b.Store.IsSuspect(context.Background(), 300); sus {
		t.Fatal("disposed suspect should leave watchlist")
	}
}

func TestJoinBotSkipped(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true}, true), defaultCfg())
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(400, true, "bot", "B")}, 0)
	if n, _ := b.Store.SuspectCount(context.Background()); n != 0 {
		t.Fatalf("bots must be skipped; suspects=%d", n)
	}
}

func TestJoinNonWhitelistedIgnored(t *testing.T) {
	tg := newFakeTG()
	tg.chat[999] = telegram.ChatInfo{ID: 999, Bio: "广告 加V"}
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true}, true), defaultCfg())
	b.HandleNewMembers(context.Background(), -777, []telegram.User{u(999, false, "spam", "S")}, 0)
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("non-whitelisted join must be zero AI: %d", aiCalls)
	}
	if n, _ := b.Store.SuspectCount(context.Background()); n != 0 {
		t.Fatalf("non-whitelisted join must not write: %d suspects", n)
	}
}

func TestMessageSignatureHitDirectDisposal(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 5, ChatID: -500})
	b.Store.AddMessageSignature(ctx, normalize.Signature("专业办证 加V"), "专业办证 加V")

	b.HandleMessage(ctx, telegram.Message{MessageID: 13, From: &telegram.User{ID: 5}, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "专业办证 加V"})

	// Only the triggering message is deleted now (no tracked-message dump).
	if len(tg.deletes) != 1 || tg.deletes[0] != "13" {
		t.Fatalf("deletes = %v, want just the triggering id 13", tg.deletes)
	}
	if len(tg.bans) != 1 {
		t.Fatalf("bans = %v", tg.bans)
	}
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("AI must not run on signature hit: %d", aiCalls)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 5); sus {
		t.Fatal("suspect should be removed after disposal")
	}
}

func TestMessageAIAdFullSequence(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LogChat = -999
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true, Category: "fraud", Confidence: 0.8, Reason: "诈骗"}, true), cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})

	b.HandleMessage(ctx, telegram.Message{MessageID: 22, From: &telegram.User{ID: 6}, Chat: telegram.Chat{ID: -600, Type: "supergroup"}, Text: "加QQ赚钱 日入过万"})

	joined := strings.Join(tg.seq, ",")
	if !strings.Contains(joined, "delete,ban") {
		t.Fatalf("sequence missing delete→ban: %s", joined)
	}
	if len(tg.deletes) != 1 || tg.deletes[0] != "22" {
		t.Fatalf("deletes = %v, want [22]", tg.deletes)
	}
	if len(tg.bans) != 1 {
		t.Fatalf("bans = %v", tg.bans)
	}
	if len(tg.sends) != 1 {
		t.Fatalf("notify sends = %v", tg.sends)
	}
	if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature("加QQ赚钱 日入过万")); !ok {
		t.Fatal("ad message should persist to message signature library")
	}
	if sus, _ := b.Store.IsSuspect(ctx, 6); sus {
		t.Fatal("suspect should be removed")
	}
}

func TestMessageNormalNoop(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: false, Category: "none"}, true), defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 7, ChatID: -700})
	b.HandleMessage(ctx, telegram.Message{MessageID: 30, From: &telegram.User{ID: 7}, Chat: telegram.Chat{ID: -700, Type: "supergroup"}, Text: "今天天气不错"})
	if len(tg.bans) != 0 {
		t.Fatalf("non-ad must not dispose: %v", tg.bans)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 7); !sus {
		t.Fatal("non-ad suspect should stay on watchlist")
	}
}

func TestMessageAdminSkipped(t *testing.T) {
	tg := newFakeTG()
	tg.member[fkey(-800, 8)] = telegram.ChatMember{Status: "administrator"}
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 8, ChatID: -800})
	b.HandleMessage(ctx, telegram.Message{MessageID: 40, From: &telegram.User{ID: 8}, Chat: telegram.Chat{ID: -800, Type: "supergroup"}, Text: "广告"})
	if len(tg.bans) != 0 {
		t.Fatalf("admin must be skipped: %v", tg.bans)
	}
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("AI must not run for admins: %d", aiCalls)
	}
}

func TestMessageAIFailureDoesNotBan(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 9, ChatID: -900})
	b.HandleMessage(ctx, telegram.Message{MessageID: 50, From: &telegram.User{ID: 9}, Chat: telegram.Chat{ID: -900, Type: "supergroup"}, Text: "可疑内容"})
	if len(tg.bans) != 0 {
		t.Fatalf("AI failure must fail safe (no ban): %v", tg.bans)
	}
	if _, _, _, aiFails := b.Stats(); aiFails == 0 {
		t.Fatal("AI failure should be counted")
	}
}

func TestMessageBotOrEmptySkipped(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), defaultCfg())
	ctx := context.Background()
	b.HandleMessage(ctx, telegram.Message{MessageID: 60, From: &telegram.User{ID: 10, IsBot: true}, Chat: telegram.Chat{ID: -10, Type: "supergroup"}, Text: "广告"})
	b.HandleMessage(ctx, telegram.Message{MessageID: 61, From: &telegram.User{ID: 11}, Chat: telegram.Chat{ID: -10, Type: "supergroup"}, Text: "   "})
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("bot/empty messages must not hit AI: %d", aiCalls)
	}
}

func TestKickModeUnbans(t *testing.T) {
	tg := newFakeTG()
	tg.chat[100] = telegram.ChatInfo{ID: 100, Bio: "办证 加V"}
	cfg := defaultCfg()
	cfg.Action = "kick"
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true, Category: "fake_cert", Confidence: 0.9}, true), cfg)
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(100, false, "k", "K")}, 0)
	if len(tg.bans) != 2 || tg.bans[0] != "ban" || tg.bans[1] != "unban" {
		t.Fatalf("kick mode should ban then unban: %v", tg.bans)
	}
}

// --- Whitelist / auto-leave -------------------------------------------------

func TestMyChatMemberLeaveForeignChat(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LeaveOtherChats = true
	cfg.LeaveNotice = true
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	b.HandleUpdate(context.Background(), telegram.Update{
		MyChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: -424242, Type: "supergroup", Title: "陌生群"},
			From:          telegram.User{ID: 77},
			OldChatMember: telegram.ChatMember{Status: "left"},
			NewChatMember: telegram.ChatMember{Status: "administrator"},
		},
	})
	if len(tg.leaves) != 1 || tg.leaves[0] != -424242 {
		t.Fatalf("leaves = %v, want [-424242]", tg.leaves)
	}
	// Notice should have been sent first.
	if !strings.Contains(strings.Join(tg.seq, ","), "send,leave") {
		t.Fatalf("expected send (notice) before leave: %v", tg.seq)
	}
}

func TestMyChatMemberLeaveWithoutNotice(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LeaveOtherChats = true
	cfg.LeaveNotice = false
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	b.HandleUpdate(context.Background(), telegram.Update{
		MyChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: -424243, Type: "supergroup"},
			From:          telegram.User{ID: 77},
			OldChatMember: telegram.ChatMember{Status: "left"},
			NewChatMember: telegram.ChatMember{Status: "member"},
		},
	})
	if len(tg.leaves) != 1 {
		t.Fatalf("leaves = %v", tg.leaves)
	}
	if len(tg.sends) != 0 {
		t.Fatalf("notice disabled, but sends = %v", tg.sends)
	}
}

func TestMyChatMemberWhitelistedStays(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LeaveOtherChats = true
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	b.HandleUpdate(context.Background(), telegram.Update{
		MyChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: -100, Type: "supergroup"},
			OldChatMember: telegram.ChatMember{Status: "left"},
			NewChatMember: telegram.ChatMember{Status: "administrator"},
		},
	})
	if len(tg.leaves) != 0 {
		t.Fatalf("whitelisted chat must not be left: %v", tg.leaves)
	}
}

func TestNonWhitelistedMessageZeroAIZeroWrite(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true}, true), cfg)
	ctx := context.Background()
	b.HandleMessage(ctx, telegram.Message{MessageID: 99, From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: -777, Type: "supergroup"}, Text: "广告 加V"})
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("non-whitelisted message must be zero AI: %d", aiCalls)
	}
	if n, _ := b.Store.SuspectCount(ctx); n != 0 {
		t.Fatalf("non-whitelisted message must be zero writes: %d", n)
	}
	if len(tg.bans) != 0 {
		t.Fatalf("must not punish: %v", tg.bans)
	}
}

// --- Commands ----------------------------------------------------------------

func TestCommandSkippedFromAdJudgment(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true}, true), cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 5, ChatID: -500})
	// A /start command in a whitelisted group from a suspect must not trigger AI.
	b.HandleUpdate(ctx, telegram.Update{
		Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: 5}, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "/start"},
	})
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 0 {
		t.Fatalf("command must not trigger AI: %d", aiCalls)
	}
}

func TestStartCommandReplies(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	b.HandleUpdate(context.Background(), telegram.Update{
		Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: 5}, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "/start"},
	})
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "🟢 在线") {
		t.Fatalf("start reply = %v", tg.sends)
	}
}

func TestStatusAdminOnly(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	// uid 5 is not admin → /status should be refused with a friendly reply.
	b.HandleUpdate(context.Background(), telegram.Update{
		Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: 5}, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "/status"},
	})
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "仅管理员") {
		t.Fatalf("non-admin /status = %v", tg.sends)
	}
}

func TestStatusAdminUserAllowed(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.AdminUsers = map[int64]bool{42: true}
	b := testBot(t, tg, fakeClassify(ai.Verdict{}, false), cfg)
	b.HandleUpdate(context.Background(), telegram.Update{
		Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "/status"},
	})
	// Configured admin user is allowed even when not a group admin.
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "状态") {
		t.Fatalf("admin user /status = %v", tg.sends)
	}
}

func TestCheckRunsAIOnlyNoDisposal(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.AdminUsers = map[int64]bool{42: true}
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true, Category: "fraud", Confidence: 0.9, Reason: "诈骗"}, true), cfg)
	b.HandleUpdate(context.Background(), telegram.Update{
		Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "/check 广告"},
	})
	if len(tg.bans) != 0 {
		t.Fatalf("/check must not dispose: %v", tg.bans)
	}
	if _, _, aiCalls, _ := b.Stats(); aiCalls != 1 {
		t.Fatalf("/check should run exactly one AI call: %d", aiCalls)
	}
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "is_ad") {
		t.Fatalf("/check reply = %v", tg.sends)
	}
}

func TestDryRunNoDispose(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.DryRun = true
	b := testBot(t, tg, fakeClassify(ai.Verdict{IsAd: true, Category: "fraud"}, true), cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})
	b.HandleMessage(ctx, telegram.Message{MessageID: 2, From: &telegram.User{ID: 6}, Chat: telegram.Chat{ID: -600, Type: "supergroup"}, Text: "加V"})
	if len(tg.bans) != 0 {
		t.Fatalf("dry-run must not ban: %v", tg.bans)
	}
	if len(tg.deletes) != 0 {
		t.Fatalf("dry-run must not delete: %v", tg.deletes)
	}
}
