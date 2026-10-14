package bot

// amend10/amend11 join-verification tests. Everything offline: fakeTG /
// fakeAI / fakeTTLTG, no long-polling, no real token (iron rule 3).
//
// amend11 re-ordered the gate (判定走完才欢迎): a bio joiner is welcomed only
// after the join-bio AI judgment concludes NON-AD (first round, retry round,
// or local-rule fallback); an AD conclusion disposes with no welcome; a
// failing judgment retries with no welcome. No-bio joiners are welcomed at
// join time. The amend10 §F assertions still hold where the order did not
// move them:
//
//	 F.1 join → welcome sent + pending + NO AI call (no-bio joiner);
//	 F.2 sticker passes → pending cleared, welcome deleted (the bio round
//	     ALREADY ran before the welcome — there is nothing parked to resume);
//	 F.3 只认贴纸： a real sticker passes; pure emoji TEXT / plain text /
//	     photos NEVER pass (the standalone regression test — iron rule 1);
//	 F.4 timeout → exactly one real ban (no unban), the notify evidence line
//	     🔎 入群 300 秒未验证（未发贴纸）, the in-group AI announcement, pending
//	     cleared;
//	 F.5 a sticker in ANOTHER chat does not verify;
//	 F.6 dry-run → join_verify_dry_run only, zero bans, timeout counter
//	     NOT incremented;
//	 F.7 BOT_JOIN_VERIFY=false → no welcome, no registration, the bio
//	     judgment runs at join time (original flow);
//	 F.8 ad text during pending → zero AI calls; the deadline still disposes;
//	 F.9 leave (chat_member left/kicked) → pending cleared, welcome deleted,
//	     NO disposal (不踢空气); repeated clear is idempotent;
//	F.10 the welcome message is NOT in the 60s TTL deletion registry (iron
//	     rule 8) while command replies ARE;
//	F.11 the scan ticker dies with Stop (no further disposals);
//	F.12 the join handler returns fast (< 100ms) — the 300s wait is the
//	     ticker's job, never the handler's;
//	F.13 the variable contract — pinned by config's
//	    TestEnvContractThreeWayConsistent / TestEnvContractREADMESubset; here
//	    the six BOT_JOIN_VERIFY_* names are pinned explicitly.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// --- helpers -------------------------------------------------------------------

// verifyCfg arms the join verification with a long timeout (deadline expiry
// is driven by setScanClock time-travel, never by sleeping).
func verifyCfg() (cfg config.Config) {
	cfg = defaultCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 300 * time.Second
	cfg.JoinVerifyScan = 5 * time.Second
	return cfg
}

// verifyBot builds an armed bot around fakeTG.
func verifyBot(t *testing.T, tg *fakeTG, fa *fakeAI, cfg config.Config) *Bot {
	t.Helper()
	return testBot(t, tg, fa, cfg)
}

// verifyJoin drives one joiner through the full gate and returns the bot.
// chatInfo sets the GetChat response (bio/photo).
func verifyJoin(t *testing.T, cfg config.Config, uid int64, username, first, bio string, hasPhoto bool) (*Bot, *fakeTG, *fakeAI) {
	t.Helper()
	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: bio, HasPhoto: hasPhoto}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(uid, false, username, first)}, 0)
	return b, tg, fa
}

// stickerMsg builds a real sticker message (the ONLY passing shape).
func stickerMsg(chatID, uid, msgID int64) telegram.Message {
	m := adMsg(chatID, uid, msgID, "")
	m.Sticker = &telegram.Sticker{FileID: "f1", FileUniqueID: "u1", Emoji: "🐉"}
	return m
}

// emojiMsg builds a pure-emoji TEXT message (iron rule 1: must NOT pass).
func emojiMsg(chatID, uid, msgID int64, text string) telegram.Message {
	return adMsg(chatID, uid, msgID, text)
}

// photoMsg builds a photo message.
func verifyPhotoMsg(chatID, uid, msgID int64) telegram.Message {
	m := adMsg(chatID, uid, msgID, "")
	m.Photo = []telegram.PhotoSize{{FileID: "p1", FileUniqueID: "pu1", Width: 100, Height: 100}}
	return m
}

// memberLeftUpdate builds a chat_member update for uid leaving chatID.
func memberLeftUpdate(chatID, uid int64) telegram.Update {
	return telegram.Update{
		ChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: chatID, Type: "supergroup"},
			From:          telegram.User{ID: uid},
			OldChatMember: telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}},
			NewChatMember: telegram.ChatMember{Status: "left", User: telegram.User{ID: uid}},
		},
	}
}

// expireNow time-travels the scan clock past every pending deadline and runs
// one synchronous scan (no ticker sleeping).
func expireNow(b *Bot) {
	b.joinVerify.setScanClock(func() time.Time {
		return time.Now().Add(24 * time.Hour)
	})
	b.joinVerify.scanDeadlines()
}

// --- F.1 join → welcome + pending + no AI --------------------------------------

func TestJoinVerifyWelcomeAndPendingNoAI(t *testing.T) {
	cfg := verifyCfg()
	b, tg, fa := verifyJoin(t, cfg, 701, "newbie", "小新", "", false)
	if got := tg.sendsSnapshot(); len(got) != 1 {
		t.Fatalf("exactly one welcome message, got %v", got)
	}
	want := "👋 欢迎 @newbie！请在 300 秒内发一个贴纸，证明你不是机器人，否则会被移出本群～"
	if tg.sendsSnapshot()[0] != want {
		t.Fatalf("welcome text = %q, want %q", tg.sendsSnapshot()[0], want)
	}
	if fa.callCount() != 0 {
		t.Fatalf("pending joiner must consume zero AI: %d", fa.callCount())
	}
	if enabled, pending, _, _ := b.JoinVerifyStats(); !enabled || pending != 1 {
		t.Fatalf("join_verify enabled=%v pending=%d, want true/1", enabled, pending)
	}
	// Empty bio → the parked bio round is nothing; the watchlist record from
	// screenNewMember exists (观察名单 recorded before the gate).
	if ok, _ := b.Store.IsSuspect(context.Background(), 701); !ok {
		t.Fatal("joiner must be on the watchlist while pending")
	}
}

// F.1b: no-username joiner → the welcome uses the display name.
func TestJoinVerifyWelcomeLabelFallback(t *testing.T) {
	cfg := verifyCfg()
	_, tg, _ := verifyJoin(t, cfg, 702, "", "张三", "", false)
	got := tg.sendsSnapshot()
	if len(got) != 1 {
		t.Fatalf("welcome = %v", got)
	}
	if !strings.Contains(got[0], "欢迎 张三！") {
		t.Fatalf("welcome must address by first name: %q", got[0])
	}
}

// --- F.2 sticker passes → cleared + welcome deleted -----------------------------

// 有简介 (amend11 order): the bio round runs BEFORE the welcome — a non-ad
// verdict is what arms the gate. The sticker then clears the entry; nothing
// is parked to resume (the join screen already concluded).
func TestJoinVerifyPassThenBioAI(t *testing.T) {
	cfg := verifyCfg()
	b, tg, fa := verifyJoin(t, cfg, 703, "bioU", "阿三", "我的正常简介", false)
	// amend11: the bio judgment runs FIRST (inline path — pool not started),
	// and its non-ad conclusion is what sends the welcome.
	waitFor(t, func() bool { return fa.callCount() == 1 }, "bio judgment before the welcome")
	if fa.lastMode != "bio" {
		t.Fatalf("join round must be bio mode, got %q", fa.lastMode)
	}
	if fa.lastContent.Text != "我的正常简介" {
		t.Fatalf("bio content = %q", fa.lastContent.Text)
	}
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "welcome after non-ad verdict")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("non-ad conclusion must register pending: %d", pending)
	}
	b.HandleMessage(context.Background(), stickerMsg(-500, 703, 900))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("pending=%d passed=%d, want 0/1", pending, passed)
	}
	// Welcome deleted on pass (msg id 1 from fakeTG.SendMessage).
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("welcome must be deleted on pass: %v", got)
	}
	if len(tg.banList()) != 0 {
		t.Fatalf("clean bio verdict must not dispose: %v", tg.banList())
	}
}

// 无简介: after the sticker passes, nothing is submitted — the watchlist
// record simply waits for the first message.
func TestJoinVerifyPassNoBioWaitsFirstMessage(t *testing.T) {
	cfg := verifyCfg()
	b, _, fa := verifyJoin(t, cfg, 704, "noBio", "阿四", "", false)
	b.HandleMessage(context.Background(), stickerMsg(-500, 704, 901))
	if fa.callCount() != 0 {
		t.Fatalf("no-bio pass must not submit any AI: %d", fa.callCount())
	}
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("pending=%d passed=%d, want 0/1", pending, passed)
	}
	// After passing, the user's first REAL message goes to the AI normally.
	b.HandleMessage(context.Background(), adMsg(-500, 704, 902, "正常发言第一条"))
	waitFor(t, func() bool { return fa.callCount() == 1 }, "first message judged after pass")
}

// --- F.3 只认贴纸 (iron rule 1) --------------------------------------------------

// THE regression test: a pure emoji TEXT message must NEVER pass. A future
// edit that widens the passing shape fails here.
func TestJoinVerifyPureEmojiTextDoesNotPass(t *testing.T) {
	cfg := verifyCfg()
	b, _, fa := verifyJoin(t, cfg, 705, "emojiU", "阿五", "", false)
	// Pure emoji text — looks "sticker-ish", carries no judgeable content.
	b.HandleMessage(context.Background(), emojiMsg(-500, 705, 903, "😀🐉🔥✨🤡"))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 1 || passed != 0 {
		t.Fatalf("pure emoji TEXT must not pass: pending=%d passed=%d", pending, passed)
	}
	if fa.callCount() != 0 {
		t.Fatalf("emoji text while pending must consume zero AI: %d", fa.callCount())
	}
	// The same shape with the STICKER field set passes.
	b.HandleMessage(context.Background(), stickerMsg(-500, 705, 904))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("sticker must pass: pending=%d passed=%d", pending, passed)
	}
}

// Plain words and photos are equally rejected (table-driven complement).
func TestJoinVerifyOnlyStickerShapeTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  telegram.Message
	}{
		{"plain_text", emojiMsg(-500, 706, 910, "你好我是真人")},
		{"pure_emoji_text", emojiMsg(-500, 706, 911, "😊😊")},
		{"photo", verifyPhotoMsg(-500, 706, 912)},
		{"photo_with_emoji_caption", func() telegram.Message {
			m := verifyPhotoMsg(-500, 706, 913)
			m.Caption = "😀"
			return m
		}()},
		{"empty_message", adMsg(-500, 706, 914, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := verifyCfg()
			b, tg, fa := verifyJoin(t, cfg, 706, "shapeU", "阿六", "", false)
			nbans := len(tg.banList())
			_ = nbans
			b.HandleMessage(context.Background(), tc.msg)
			if _, pending, passed, _ := b.JoinVerifyStats(); pending != 1 || passed != 0 {
				t.Fatalf("%s must not pass: pending=%d passed=%d", tc.name, pending, passed)
			}
			if fa.callCount() != 0 {
				t.Fatalf("%s must consume zero AI: %d", tc.name, fa.callCount())
			}
		})
	}
}

// A sticker from a user NOT in pending is irrelevant (not consumed).
func TestJoinVerifyStickerFromUnknownUserIgnored(t *testing.T) {
	cfg := verifyCfg()
	b, _, fa := verifyJoin(t, cfg, 707, "knownU", "阿七", "", false)
	b.HandleMessage(context.Background(), stickerMsg(-500, 999, 920))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 1 || passed != 0 {
		t.Fatalf("unknown uid sticker must not affect pending: %d/%d", pending, passed)
	}
	if fa.callCount() != 0 {
		t.Fatalf("unknown sticker must consume zero AI: %d", fa.callCount())
	}
}

// --- F.4 timeout → one real ban + evidence + announcement + cleared -------------

func TestJoinVerifyTimeoutRealBanAndEvidence(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	tg.chat[710] = telegram.ChatInfo{ID: 710, Bio: ""}
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	ann := &fakeAnnouncer{out: "🚪 未验证已送出门外～"}
	b.Announcer = NewAnnouncer(tg, ann, true, time.Second, time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil, b)

	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(710, false, "slowU", "慢慢")}, 777)
	if len(tg.sendsSnapshot()) != 1 {
		t.Fatalf("welcome must be sent: %v", tg.sendsSnapshot())
	}

	expireNow(b)

	// ONE real ban — no unban (ban, not kick-and-readmit).
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("timeout must be exactly one real ban, got %v", got)
	}
	// Welcome deleted via the punish chain's explicit msgIDs.
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("welcome must be deleted by the disposal: %v", got)
	}
	// Zero AI on the whole timeout path.
	if fa.callCount() != 0 {
		t.Fatalf("timeout disposal must consume zero AI: %d", fa.callCount())
	}
	// The notify evidence line (4-line template).
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "timeout disposition notify")
	nb := rec.texts()[0]
	for _, want := range []string{
		"⏱️ 处理时间:",
		"💬 🔎 入群 300 秒未验证（未发贴纸）",
		"👤 @slowU",
		"✅ 已封禁 · 100分",
	} {
		if !strings.Contains(nb, want) {
			t.Errorf("notify missing %q:\n%s", want, nb)
		}
	}
	if got := strings.Count(nb, "\n"); got != 3 {
		t.Errorf("notify must be 4 lines, got %d:\n%s", got+1, nb)
	}
	// The in-group AI announcement rides the chain.
	waitFor(t, func() bool { return ann.callCount() == 1 }, "in-group announcement")
	// Pending cleared + timeout counter.
	if _, pending, _, timeouts := b.JoinVerifyStats(); pending != 0 || timeouts != 1 {
		t.Fatalf("pending=%d timeouts=%d, want 0/1", pending, timeouts)
	}
	// Disposed user leaves the watchlist.
	if ok, _ := b.Store.IsSuspect(ctx, 710); ok {
		t.Fatal("timed-out user must leave the watchlist")
	}
}

// F.4b: kick action → ban+unban (the action knob is honored).
func TestJoinVerifyTimeoutKickAction(t *testing.T) {
	cfg := verifyCfg()
	cfg.JoinVerifyAction = "kick"
	b, tg, _ := verifyJoin(t, cfg, 711, "kickU", "阿八", "", false)
	expireNow(b)
	if got := tg.banList(); len(got) != 2 || got[0] != "ban" || got[1] != "unban" {
		t.Fatalf("kick action = ban+unban, got %v", got)
	}
}

// --- F.5 cross-group sticker does not verify ------------------------------------

func TestJoinVerifyCrossChatStickerDoesNotVerify(t *testing.T) {
	cfg := verifyCfg()
	b, tg, fa := verifyJoin(t, cfg, 712, "crossU", "阿九", "", false)
	// Sticker in ANOTHER whitelisted chat: must not verify, entry stays.
	b.HandleMessage(context.Background(), stickerMsg(-200, 712, 930))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 1 || passed != 0 {
		t.Fatalf("cross-chat sticker must not verify: pending=%d passed=%d", pending, passed)
	}
	// The same sticker in the registering chat then passes.
	b.HandleMessage(context.Background(), stickerMsg(-500, 712, 931))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("same-chat sticker must verify: pending=%d passed=%d", pending, passed)
	}
	_ = fa
	_ = tg
}

// --- F.6 dry-run → log only, counter excluded -----------------------------------

func TestJoinVerifyDryRunLogsOnly(t *testing.T) {
	cfg := verifyCfg()
	cfg.DryRun = true
	var logs strings.Builder
	lg := logx.New(&logs, logx.Info, "text", "bot")
	tg := newFakeTG()
	tg.chat[713] = telegram.ChatInfo{ID: 713, Bio: ""}
	b := New(store.NewMemory(), tg, &fakeAI{}, nil, cfg, lg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(713, false, "dryU", "阿十")}, 0)
	// The welcome still goes out in dry-run (it is not a punishment).
	if len(tg.sendsSnapshot()) != 1 {
		t.Fatalf("welcome must be sent in dry-run: %v", tg.sendsSnapshot())
	}
	expireNow(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("dry-run must zero bans: %v", got)
	}
	if _, pending, _, timeouts := b.JoinVerifyStats(); pending != 0 || timeouts != 0 {
		t.Fatalf("dry-run timeout must not count: pending=%d timeouts=%d", pending, timeouts)
	}
	if !strings.Contains(logs.String(), "join_verify_dry_run") {
		t.Fatalf("join_verify_dry_run must be logged:\n%s", logs.String())
	}
	if n, _ := b.Store.Punished(ctx); n != 0 {
		t.Fatal("dry-run must not record punishments")
	}
}

// --- F.7 disabled → original flow (bio judged at join) ---------------------------

func TestJoinVerifyDisabledOriginalFlow(t *testing.T) {
	cfg := verifyCfg()
	cfg.JoinVerify = false
	b, tg, fa := verifyJoin(t, cfg, 714, "offU", "阿甲", "我的简介内容", false)
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("disabled gate must send no welcome: %v", got)
	}
	if enabled, pending, _, _ := b.JoinVerifyStats(); enabled || pending != 0 {
		t.Fatalf("join_verify_enabled must mirror the config: %v/%d", enabled, pending)
	}
	// Bio judged immediately (original amend5 flow).
	waitFor(t, func() bool { return fa.callCount() == 1 }, "bio judgment at join")
	_ = b
}

// --- F.8 messages during pending → zero AI; deadline still disposes ---------------

func TestJoinVerifyAdTextDuringPendingBlocked(t *testing.T) {
	cfg := verifyCfg()
	cfg.LogChat = -999
	tg := newFakeTG()
	tg.chat[715] = telegram.ChatInfo{ID: 715, Bio: ""}
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(715, false, "adU", "阿乙")}, 0)
	// Ad text while pending: swallowed, zero AI.
	b.HandleMessage(ctx, adMsg(-500, 715, 940, "加V赚钱 上门服务"))
	if fa.callCount() != 0 {
		t.Fatalf("pending messages must not reach AI: %d", fa.callCount())
	}
	if len(tg.banList()) != 0 {
		t.Fatalf("pending messages must not dispose: %v", tg.banList())
	}
	if len(tg.deleteList()) != 0 {
		t.Fatalf("pending messages must not be deleted: %v", tg.deleteList())
	}
	// The deadline (not the content) disposes them.
	expireNow(b)
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("deadline must dispose the pending user: %v", got)
	}
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "timeout notify")
}

// --- F.9 leave clears pending, idempotent, no disposal ---------------------------

func TestJoinVerifyLeaveClearsPending(t *testing.T) {
	cfg := verifyCfg()
	b, tg, fa := verifyJoin(t, cfg, 716, "leftU", "阿丙", "", false)
	b.HandleUpdate(context.Background(), memberLeftUpdate(-500, 716))
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("leave must clear pending: %d", pending)
	}
	// Welcome deleted (residual cleanup).
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("welcome must be deleted on leave: %v", got)
	}
	// No disposal — 不踢空气.
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("leave must not dispose: %v", got)
	}
	// Idempotent: a second clear (another leave update) is a no-op.
	b.HandleUpdate(context.Background(), memberLeftUpdate(-500, 716))
	if got := tg.deleteList(); len(got) != 1 {
		t.Fatalf("repeated clear must be idempotent: %v", got)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("repeated clear must still not dispose: %v", got)
	}
	// The deadline can no longer reach the cleared entry.
	expireNow(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("cleared entry must not be disposed by the deadline: %v", got)
	}
	_ = fa
}

// --- F.10 welcome NOT in the TTL registry (iron rule 8) ---------------------------

func TestJoinVerifyWelcomeNotInTTLRegistry(t *testing.T) {
	tg := newFakeTTLTG()
	tg.chat[717] = telegram.ChatInfo{ID: 717, Bio: ""}
	cfg := verifyCfg()
	cfg.MsgTTL = 80 * time.Millisecond
	b := testBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(717, false, "ttlU", "阿丁")}, 0)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "welcome sent")
	// Long enough for any stray TTL timer — the welcome must NOT be deleted.
	time.Sleep(300 * time.Millisecond)
	if got := tg.deletePairsSnapshot(); len(got) != 0 {
		t.Fatalf("welcome must NOT be in the TTL deletion registry: %v", got)
	}
	if scheduled, _, _ := b.MsgTTLStats(); scheduled != 0 {
		t.Fatalf("msg_ttl_scheduled_total = %d, want 0 (welcome never registered)", scheduled)
	}
	// Contrast: a command reply IS scheduled and deleted by the TTL.
	cfg2 := verifyCfg()
	cfg2.MsgTTL = 80 * time.Millisecond
	cfg2.AdminUsers = map[int64]bool{42: true}
	tg2 := newFakeTTLTG()
	b2 := testBot(t, tg2, &fakeAI{}, cfg2)
	b2.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 1, "/start")))
	waitFor(t, func() bool { return len(tg2.sendsSnapshot()) == 1 }, "command reply sent")
	waitForTTLDeletes(t, tg2, 1, "command reply TTL delete")
	if scheduled, _, _ := b2.MsgTTLStats(); scheduled != 1 {
		t.Fatalf("command reply must be TTL-scheduled: %d", scheduled)
	}
}

// --- F.11 the ticker dies with Stop ----------------------------------------------

func TestJoinVerifyTickerStopsWithStop(t *testing.T) {
	cfg := verifyCfg()
	cfg.JoinVerifyScan = 20 * time.Millisecond
	cfg.JoinVerifyTimeout = 60 * time.Millisecond
	tg := newFakeTG()
	tg.chat[720] = telegram.ChatInfo{ID: 720, Bio: ""}
	b := verifyBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(720, false, "tickU", "阿戊")}, 0)
	b.StartJoinVerify()
	t.Cleanup(b.StopJoinVerify)
	// The real ticker fires the expiry within the scan interval.
	waitFor(t, func() bool { return len(tg.banList()) == 1 }, "ticker-driven expiry")
	// Stop the ticker; a NEW pending joiner must never expire now.
	b.StopJoinVerify()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(721, false, "tickV", "阿己")}, 0)
	time.Sleep(300 * time.Millisecond)
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("no disposal after Stop: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("post-Stop joiner stays pending (no ticker): %d", pending)
	}
}

// --- F.12 handler fast return -----------------------------------------------------

func TestJoinVerifyHandlerFastReturn(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	start := time.Now()
	for i := 0; i < 50; i++ {
		uid := int64(730 + i)
		tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: ""}
		b.HandleNewMembers(context.Background(), -500, []telegram.User{u(uid, false, "fast", "快")}, 0)
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("50 joins took %v; the handler must never wait for the deadline", elapsed)
	}
	if len(tg.sendsSnapshot()) != 50 {
		t.Fatalf("welcomes = %d, want 50", len(tg.sendsSnapshot()))
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 50 {
		t.Fatalf("pending = %d, want 50", pending)
	}
}

// --- F.8b over-cap: registration refused → no gate for that joiner ----------------

func TestJoinVerifyOverCapFallsThrough(t *testing.T) {
	cfg := verifyCfg()
	cfg.JoinVerifyMax = 1
	tg := newFakeTG()
	tg.chat[740] = telegram.ChatInfo{ID: 740, Bio: "第一个人的简介"}
	tg.chat[741] = telegram.ChatInfo{ID: 741, Bio: "第二个人的简介"}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(740, false, "cap1", "一")}, 0)
	b.HandleNewMembers(ctx, -500, []telegram.User{u(741, false, "cap2", "二")}, 0)
	// Both bio judgments ran (amend11: the AI round precedes the gate).
	waitFor(t, func() bool { return fa.callCount() == 2 }, "both bio judgments")
	// The FIRST joiner (non-ad conclusion → registration) occupies the cap.
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "first joiner welcome")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("pending = %d, want 1 (cap=1)", pending)
	}
	if got := tg.sendsSnapshot(); len(got) != 1 {
		t.Fatalf("only the first joiner gets a welcome: %v", got)
	}
	// The over-cap joiner's non-ad conclusion arrived but registration was
	// refused → they are simply NOT gated (no welcome, no pending, no trap);
	// their messages screen normally.
	if isP := b.joinVerify.isPending(741); isP {
		t.Fatal("over-cap joiner must not be pending")
	}
	b.HandleMessage(ctx, adMsg(-500, 741, 907, "正常发言"))
	waitFor(t, func() bool { return fa.callCount() == 3 }, "over-cap joiner message judged normally")
}

// --- welcome send failure → gate skipped (never trap the joiner) -------------------

func TestJoinVerifyWelcomeSendErrorSkipsGate(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTG()
	tg.chat[742] = telegram.ChatInfo{ID: 742, Bio: "发送失败者的简介"}
	tg.sendErr = errors.New("send boom")
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(742, false, "errU", "阿庚")}, 0)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("send failure must skip registration: %d", pending)
	}
	// Falls through to the bio round (not trapped).
	waitFor(t, func() bool { return fa.callCount() == 1 }, "send-failure bio judgment")
}

// --- re-join while pending: idempotent — ONE welcome, ONE entry ------------------

// amend11 铁律 2（幂等）: a duplicate join (service-message replay / re-join
// while the entry is still live) must not send a second welcome or re-register
// the deadline. The pass deletes THE welcome.
func TestJoinVerifyRejoinWhilePendingIdempotent(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTTLTG() // incrementing ids: first welcome=1
	tg.chat[743] = telegram.ChatInfo{ID: 743, Bio: ""}
	b := testBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(743, false, "reU", "阿辛")}, 0)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "first welcome")
	b.HandleNewMembers(ctx, -500, []telegram.User{u(743, false, "reU", "阿辛")}, 0)
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("duplicate join keeps ONE entry: %d", pending)
	}
	if got := tg.sendsSnapshot(); len(got) != 1 {
		t.Fatalf("duplicate join must not send a second welcome: %v", got)
	}
	// The entry passes on a sticker and deletes THE (single) welcome.
	b.HandleMessage(ctx, stickerMsg(-500, 743, 950))
	if got := tg.deletePairsSnapshot(); len(got) != 1 || got[0] != "-500:1" {
		t.Fatalf("the welcome must be deleted on pass: %v", got)
	}
}

// re-join AFTER a completed episode: the woken pin was cleared at pass, so
// the second no-bio join gets a fresh welcome (not silently swallowed).
func TestJoinVerifyRejoinAfterEpisodeWelcomesAgain(t *testing.T) {
	cfg := verifyCfg()
	tg := newFakeTTLTG()
	tg.chat[744] = telegram.ChatInfo{ID: 744, Bio: ""}
	b := testBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(744, false, "again", "阿子")}, 0)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "first welcome")
	b.HandleMessage(ctx, stickerMsg(-500, 744, 951))
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("first episode must pass: pending=%d passed=%d", pending, passed)
	}
	// Second join (e.g. left and came back): fresh welcome + fresh pending.
	b.HandleNewMembers(ctx, -500, []telegram.User{u(744, false, "again", "阿子")}, 0)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 2 }, "second welcome after re-join")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("re-join after a passed episode must re-register: %d", pending)
	}
}

// --- F.13 the six variable names are pinned ---------------------------------------

func TestJoinVerifyEnvVarNamesPinned(t *testing.T) {
	// Redacted() lists every BOT_* variable the loader knows; the six
	// amend10 names must all appear there (the three-surface equality with
	// .env.example / docker-compose / README is enforced by config's
	// TestEnvContractThreeWayConsistent — this pins the bot-side view).
	c := defaultCfg()
	r := c.Redacted()
	for _, name := range []string{
		"BOT_JOIN_VERIFY",
		"BOT_JOIN_VERIFY_TIMEOUT",
		"BOT_JOIN_VERIFY_SCAN",
		"BOT_JOIN_VERIFY_ACTION",
		"BOT_JOIN_VERIFY_MSG",
		"BOT_JOIN_VERIFY_MAX_PENDING",
	} {
		if _, ok := r[name]; !ok {
			t.Errorf("Redacted() missing %s", name)
		}
	}
}

// --- /statusz gauges -------------------------------------------------------------

func TestJoinVerifyStatsGauges(t *testing.T) {
	cfg := verifyCfg()
	b, _, _ := verifyJoin(t, cfg, 745, "statU", "阿壬", "", false)
	b.HandleMessage(context.Background(), stickerMsg(-500, 745, 960))
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(746, false, "statV", "阿癸")}, 0)
	expireNow(b)
	enabled, pending, passed, timeouts := b.JoinVerifyStats()
	if !enabled || pending != 0 || passed != 1 || timeouts != 1 {
		t.Fatalf("gauges enabled=%v pending=%d passed=%d timeouts=%d, want true/0/1/1",
			enabled, pending, passed, timeouts)
	}
	// JoinVerifyView mirrors the gauges for /statusz.
	v := b.JoinVerifyView()
	if !v.Enabled || v.Pending != 0 || v.PassedTotal != 1 || v.TimeoutTotal != 1 {
		t.Fatalf("JoinVerifyView = %+v", v)
	}
}
