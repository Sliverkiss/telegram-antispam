package bot

// amend12 验收测试（关联频道帖子评论 → 审简介 + 防刷缓存）。全部离线假
// TG / fakeAI，不起真实长轮询。场景清单（TASK_antispam_amend12.md §5.2）：
//   1. 触发/不触发（频道帖子评论 vs 普通回复 vs 普通消息）
//   2. 简介有链接 / 有 @ / 空 / 无链接无@ → 送审或回落
//   3. 判广告 → 处置（标准 punish 链：删消息 + ban + 通知 + 写签名库）
//   4. 判非广告 → 零处置、不消费观察名额
//   5. 防刷缓存命中 → 直接踢、零 AI 调用
//   6. 缓存窗口过期 → 重新送审
//   7. 普通成员 vs 管理员差异
//   8. 开关关闭 → 判据整体不存在（零变化）
//   9. getChat 报错 → 降级回落原流程
//  10. 防刷窗口语义（固定窗口，非滑动）+ 容量淘汰 + dry-run 零写入
//  11. 判定失败 → 重试轮广告结论同样入缓存
//  12. 解析层字段映射（见 telegram 包 client_test.go）
//  13. /statusz 新字段（见本文件 TestCommentBioStatuszView）

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"adbot/internal/config"
	"adbot/internal/normalize"
	"adbot/internal/retryq"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// --- helpers ---------------------------------------------------------------------

// commentCfg arms the comment screen with default 30m window.
func commentCfg() config.Config {
	cfg := defaultCfg()
	cfg.CommentBioScreen = true
	cfg.CommentBioFlashTTL = 30 * time.Minute
	cfg.CommentBioFlashMax = 5000
	return cfg
}

// commentMsg builds a channel-post comment: a reply whose ReplyToMessage is
// the automatic forward of the linked channel's post.
func commentMsg(chatID, uid, messageID int64, text string) telegram.Message {
	m := adMsg(chatID, uid, messageID, text)
	m.ReplyToMessage = &telegram.Message{
		MessageID:          messageID - 1,
		Chat:               telegram.Chat{ID: chatID, Type: "supergroup"},
		Text:               "频道帖子正文",
		IsAutomaticForward: true,
		SenderChat:         &telegram.Chat{ID: -1001324890214, Type: "channel", Title: "Sliverkissの废弃文化研究所"},
	}
	return m
}

// plainReplyMsg builds a reply to an ordinary group message (must NOT trigger).
func plainReplyMsg(chatID, uid, messageID int64, text string) telegram.Message {
	m := adMsg(chatID, uid, messageID, text)
	m.ReplyToMessage = &telegram.Message{
		MessageID: messageID - 1,
		Chat:      telegram.Chat{ID: chatID, Type: "supergroup"},
		Text:      "普通消息",
	}
	return m
}

// commentJoin seeds the user's bio and drives one comment message.
func commentDrive(b *Bot, tg *fakeTG, uid int64, bio string, m telegram.Message) {
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: bio}
	b.HandleMessage(context.Background(), m)
}

// --- pure predicates ---------------------------------------------------------------

func TestIsChannelPostCommentPredicate(t *testing.T) {
	if !isChannelPostComment(commentMsg(-500, 7, 30, "评论")) {
		t.Error("channel-post comment must trigger")
	}
	if isChannelPostComment(plainReplyMsg(-500, 7, 31, "回复")) {
		t.Error("plain reply must NOT trigger")
	}
	if isChannelPostComment(adMsg(-500, 7, 32, "不是回复")) {
		t.Error("non-reply must NOT trigger")
	}
	m := commentMsg(-500, 7, 33, "评论")
	m.ReplyToMessage.IsAutomaticForward = false
	if isChannelPostComment(m) {
		t.Error("reply to non-forward must NOT trigger")
	}
}

func TestBioCarriesContactPredicate(t *testing.T) {
	cases := []struct {
		bio  string
		want bool
	}{
		{"看 t.me/xxx 广告", true},
		{"https://example.com/abc", true},
		{"http://a.com", true},
		{"www.foo.com/bar", true},
		{"加我 @spammer", true},
		{"bare.example.com", true},
		{"我是正常人，喜欢爬山", false},
		{"", false},
		{"纯文本简介 3.5 度", false},
		{"联系微信 abc123", false},
	}
	for _, c := range cases {
		if got := bioCarriesContact(c.bio); got != c.want {
			t.Errorf("bioCarriesContact(%q) = %v, want %v", c.bio, got, c.want)
		}
	}
}

// --- 1/2/8: trigger, bio gate, switch off ------------------------------------------

// Channel-post comment + bio with a link → AI bio review runs (mode="bio").
func TestCommentBioLinkTriggersAIBioReview(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	commentDrive(b, tg, 7, "看 t.me/xxx 广告", commentMsg(-500, 7, 30, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("exactly one AI call expected: %d", fa.callCount())
	}
	if fa.lastMode != "bio" {
		t.Fatalf("mode = %q, want bio", fa.lastMode)
	}
	if fa.lastContent.Text != "看 t.me/xxx 广告" {
		t.Fatalf("AI must judge the BIO, got %q", fa.lastContent.Text)
	}
	if _, triggered, _, _ := b.CommentBioStats(); triggered != 1 {
		t.Fatalf("comment_bio_triggered_total = %d, want 1", triggered)
	}
}

// Bio with @ only → also sent to the AI review.
func TestCommentBioAtTriggersAIBioReview(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	commentDrive(b, tg, 8, "加我 @spam_handle", commentMsg(-500, 8, 31, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("@ bio must reach AI once: %d", fa.callCount())
	}
}

// Empty bio → falls through to the ordinary flow (not judged as bio; the
// comment text itself is subject to the ordinary screen only when judgeable
// AND the user is a suspect — here they are not, so zero AI).
func TestCommentBioEmptyFallsThrough(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	commentDrive(b, tg, 9, "", commentMsg(-500, 9, 32, "评论"))
	if fa.callCount() != 0 {
		t.Fatalf("empty bio must not send AI: %d", fa.callCount())
	}
	if _, triggered, _, _ := commentStats(b); triggered != 1 {
		t.Fatalf("triggered = %d (screen entered, then fell through)", triggered)
	}
}

// Bio without link and without @ → falls through (zero AI on the bio).
func TestCommentBioNoContactFallsThrough(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	commentDrive(b, tg, 10, "我是正常人，喜欢爬山", commentMsg(-500, 10, 33, "评论"))
	if fa.callCount() != 0 {
		t.Fatalf("plain bio must not send AI: %d", fa.callCount())
	}
}

// Plain reply / ordinary message → the screen never enters (triggered stays
// 0) and the ordinary flow behaves exactly as before (no suspect → no AI).
func TestCommentPlainReplyNeverEntersScreen(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	tg.chat[11] = telegram.ChatInfo{ID: 11, Bio: "带链接 t.me/x"}
	b.HandleMessage(context.Background(), plainReplyMsg(-500, 11, 34, "普通回复"))
	b.HandleMessage(context.Background(), adMsg(-500, 11, 35, "普通消息"))
	if fa.callCount() != 0 {
		t.Fatalf("non-comment messages must not run the bio screen: %d", fa.callCount())
	}
	if _, triggered, _, _ := commentStats(b); triggered != 0 {
		t.Fatalf("triggered = %d, want 0", triggered)
	}
}

// Switch off → the criterion does not exist at all.
func TestCommentScreenDisabledZeroChange(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	cfg := commentCfg()
	cfg.CommentBioScreen = false
	b := testBot(t, tg, fa, cfg)
	commentDrive(b, tg, 12, "带链接 t.me/x 广告", commentMsg(-500, 12, 36, "评论"))
	if fa.callCount() != 0 {
		t.Fatalf("disabled screen must send zero AI: %d", fa.callCount())
	}
	if _, triggered, _, _ := commentStats(b); triggered != 0 {
		t.Fatalf("triggered = %d, want 0", triggered)
	}
}

// --- 3: ad verdict → standard punish chain -----------------------------------------

// Ad bio verdict → delete the triggering comment + ban + notify + write the
// bio signature library + write the flash cache.
func TestCommentAdBioPunishedFullChain(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 13, bio, commentMsg(-500, 13, 40, "评论"))

	if len(tg.bans) != 1 {
		t.Fatalf("ad bio must ban: %v", tg.bans)
	}
	if len(tg.deletes) != 1 || tg.deletes[0] != "40" {
		t.Fatalf("the triggering comment must be deleted: %v", tg.deletes)
	}
	if ok, _ := b.Store.HasBioSignature(context.Background(), normalize.Signature(bio)); !ok {
		t.Fatal("ad bio must be persisted to the bio signature library")
	}
	// Notification (async): four-line template with the BIO as evidence.
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "disposition notify")
	nb := rec.texts()[0]
	for _, want := range []string{"💬 " + bio, "👤 无用户名 · 13", "✅ 已封禁 · 95分"} {
		if !strings.Contains(nb, want) {
			t.Fatalf("notify missing %q:\n%s", want, nb)
		}
	}
	// Flash cache written.
	if _, _, _, cached := commentStats(b); cached != 1 {
		t.Fatalf("flash cache must hold the condemned bio: %d", cached)
	}
}

// --- 4: non-ad verdict → zero disposal, watchlist intact ---------------------------

func TestCommentNonAdVerdictZeroDisposal(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 14, ChatID: -500})
	bio := "我的频道 t.me/normal_channel"
	commentDrive(b, tg, 14, bio, commentMsg(-500, 14, 41, "评论"))
	if len(tg.bans) != 0 {
		t.Fatalf("non-ad bio must not dispose: %v", tg.bans)
	}
	if ok, _ := b.Store.IsSuspect(ctx, 14); !ok {
		t.Fatal("comment bio judgment must NOT consume the watchlist slot (amend5 D.2)")
	}
	if _, _, _, cached := commentStats(b); cached != 0 {
		t.Fatalf("non-ad bio must not enter the flash cache: %d", cached)
	}
}

// --- 5: flash cache hit → direct kick, ZERO AI ------------------------------------

// 防刷链路：同一被判广告的简介在窗口内再次出现（另一个 uid 的评论）→ 直接
// 踢、零 AI 调用、通知带「防刷命中」后缀。
func TestCommentFlashCacheHitDirectKickZeroAI(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	bio := "办证 加V t.me/xxx"

	// First occurrence: judged ad → punished + cached.
	commentDrive(b, tg, 20, bio, commentMsg(-500, 20, 50, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("first occurrence: one AI call, got %d", fa.callCount())
	}
	if len(tg.bans) != 1 {
		t.Fatalf("first occurrence must punish: %v", tg.bans)
	}

	// Second occurrence (different uid, SAME bio) inside the window.
	commentDrive(b, tg, 21, bio, commentMsg(-500, 21, 51, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("flash hit must be ZERO AI calls, got %d", fa.callCount())
	}
	if len(tg.bans) != 2 {
		t.Fatalf("flash hit must kick directly: %v", tg.bans)
	}
	if len(tg.deletes) != 2 || tg.deletes[1] != "51" {
		t.Fatalf("flash hit must delete the triggering comment: %v", tg.deletes)
	}
	// The flash hit must NOT write another signature (punishAndClear with
	// writeSig=false on this lane) — but it did delete + ban + notify.
	waitFor(t, func() bool { return len(rec.texts()) == 2 }, "flash notify")
	nb := rec.texts()[1]
	if !strings.Contains(nb, "防刷命中") {
		t.Fatalf("flash notify must carry the 防刷命中 suffix:\n%s", nb)
	}
	if !strings.Contains(nb, "💬 "+bio) {
		t.Fatalf("flash notify evidence must be the bio:\n%s", nb)
	}
	if _, _, hits, _ := commentStats(b); hits != 1 {
		t.Fatalf("comment_bio_flash_hits_total = %d, want 1", hits)
	}
}

// Obfuscated repeat (fullwidth / zero-width) still counts as 重复 (normalized
// sha1 exact match).
func TestCommentFlashCacheHitNormalizedRepeat(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, commentCfg())
	commentDrive(b, tg, 22, "办证 加V t.me/xxx", commentMsg(-500, 22, 52, "评论"))
	// Zero-width + fullwidth obfuscation of the SAME bio.
	commentDrive(b, tg, 23, "办证\u200b 加Ｖ t.me/xxx", commentMsg(-500, 23, 53, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("obfuscated repeat must still be a flash hit (zero AI): %d", fa.callCount())
	}
	if len(tg.bans) != 2 {
		t.Fatalf("obfuscated repeat must be kicked: %v", tg.bans)
	}
}

// --- 6: window expiry → re-judged ----------------------------------------------

func TestCommentFlashCacheWindowExpiry(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, commentCfg())
	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 24, bio, commentMsg(-500, 24, 54, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("first occurrence: %d", fa.callCount())
	}
	// Time-travel past the 30-minute window.
	base := time.Now()
	b.commentFlash.setClock(func() time.Time { return base.Add(31 * time.Minute) })
	commentDrive(b, tg, 25, bio, commentMsg(-500, 25, 55, "评论"))
	if fa.callCount() != 2 {
		t.Fatalf("expired window must re-send the AI review: %d", fa.callCount())
	}
	if len(tg.bans) != 2 {
		t.Fatalf("the re-judged ad verdict must punish: %v", tg.bans)
	}
	// A flash hit right at the boundary counts as expired (>= ttl): the
	// entry must be stamped under a clock at base, then read at base+30m+1ns.
	b.commentFlash.setClock(func() time.Time { return base })
	b.commentFlash.put(flashKeyOf(bio), "诈骗")
	b.commentFlash.setClock(func() time.Time { return base.Add(30 * time.Minute).Add(time.Nanosecond) })
	if _, ok := b.commentFlash.hit(flashKeyOf(bio)); ok {
		t.Fatal("exactly-at-ttl must be expired (>=)")
	}
}

// --- 7: admins --------------------------------------------------------------------

// Group admin commenting with a link bio → skipped by the existing gate
// (BOT_IGNORE_ADMINS) before the screen ever runs.
func TestCommentAdminExempt(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, commentCfg())
	tg.member[fkey(-500, 30)] = telegram.ChatMember{Status: "administrator", User: telegram.User{ID: 30}}
	tg.chat[30] = telegram.ChatInfo{ID: 30, Bio: "管理员频道 t.me/adm"}
	b.HandleMessage(context.Background(), commentMsg(-500, 30, 56, "评论"))
	if fa.callCount() != 0 {
		t.Fatalf("group admin must be exempt: %d AI calls", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("group admin must not be disposed: %v", tg.bans)
	}
	// BOT_ADMIN_USERS likewise.
	tg2 := newFakeTG()
	fa2 := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg := commentCfg()
	cfg.AdminUsers = map[int64]bool{31: true}
	b2 := testBot(t, tg2, fa2, cfg)
	tg2.chat[31] = telegram.ChatInfo{ID: 31, Bio: "管理员频道 t.me/adm"}
	b2.HandleMessage(context.Background(), commentMsg(-500, 31, 57, "评论"))
	if fa2.callCount() != 0 || len(tg2.bans) != 0 {
		t.Fatalf("BOT_ADMIN_USERS must be exempt: ai=%d bans=%v", fa2.callCount(), tg2.bans)
	}
}

// --- 9: getChat error → degrade to the ordinary flow -------------------------------

// errTG fakes a failing GetChat; everything else delegates to fakeTG.
type errGetChatTG struct{ *fakeTG }

func (e errGetChatTG) GetChat(ctx context.Context, chatID int64) (telegram.ChatInfo, error) {
	_, _ = e.fakeTG.GetChat(ctx, chatID) // keep the call sequence honest
	return telegram.ChatInfo{}, errors.New("getchat boom")
}

func TestCommentGetChatErrorFallsThrough(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, errGetChatTG{tg}, fa, commentCfg())
	b.HandleMessage(context.Background(), commentMsg(-500, 32, 58, "评论"))
	if fa.callCount() != 0 {
		t.Fatalf("getChat failure must NOT send AI (bio unavailable): %d", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("getChat failure must not dispose: %v", tg.bans)
	}
	if _, triggered, _, _ := commentStats(b); triggered != 1 {
		t.Fatalf("screen entered once (then degraded): %d", triggered)
	}
}

// --- 10: window semantics / capacity / dry-run -------------------------------------

// Fixed window (never sliding): a hit does NOT refresh the entry's ts.
func TestCommentFlashFixedWindowNotSliding(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, commentCfg())
	bio := "办证 加V t.me/xxx"
	base := time.Now()
	b.commentFlash.setClock(func() time.Time { return base })
	commentDrive(b, tg, 40, bio, commentMsg(-500, 40, 60, "评论"))
	// Hit at +29m (inside the window).
	b.commentFlash.setClock(func() time.Time { return base.Add(29 * time.Minute) })
	commentDrive(b, tg, 41, bio, commentMsg(-500, 41, 61, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("inside-window hit must be zero AI: %d", fa.callCount())
	}
	// The hit did NOT slide the entry: at +31m (window from the WRITE) the
	// bio is expired and re-judged.
	b.commentFlash.setClock(func() time.Time { return base.Add(31 * time.Minute) })
	commentDrive(b, tg, 42, bio, commentMsg(-500, 42, 62, "评论"))
	if fa.callCount() != 2 {
		t.Fatalf("fixed window: the hit must not have extended the expiry: %d", fa.callCount())
	}
}

// Capacity: oldest-first eviction past the cap.
func TestCommentFlashCapacityEviction(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg := commentCfg()
	cfg.CommentBioFlashMax = 2
	b := testBot(t, tg, fa, cfg)
	for i, uid := range []int64{50, 51, 52} {
		commentDrive(b, tg, uid, "办证"+strconv.FormatInt(uid, 10)+" 加V t.me/x"+strconv.FormatInt(uid, 10), commentMsg(-500, uid, 70+int64(i), "评论"))
	}
	if _, _, _, cached := commentStats(b); cached != 2 {
		t.Fatalf("cap=2 must hold 2 entries, got %d", cached)
	}
	// The oldest (uid 50) was evicted → a repeat of THAT bio is re-judged.
	commentDrive(b, tg, 53, "办证50 加V t.me/x50", commentMsg(-500, 53, 73, "评论"))
	if fa.callCount() != 4 {
		t.Fatalf("evicted bio must be re-judged (4 total AI): %d", fa.callCount())
	}
}

// Dry-run: judgments run, disposal is the dry-run branch, and the flash
// cache gets ZERO writes (zero-writes contract).
func TestCommentDryRunNoFlashWrite(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg := commentCfg()
	cfg.DryRun = true
	b := testBot(t, tg, fa, cfg)
	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 60, bio, commentMsg(-500, 60, 80, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("dry-run still judges: %d", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("dry-run must not dispose: %v", tg.bans)
	}
	if _, _, _, cached := commentStats(b); cached != 0 {
		t.Fatalf("dry-run must not write the flash cache: %d", cached)
	}
}

// --- 11: retry round's ad conclusion also enters the cache -------------------------

func TestCommentRetryAdEntersFlashCache(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := testBot(t, tg, fa, commentCfg())
	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 70, bio, commentMsg(-500, 70, 90, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("first (failing) round: %d", fa.callCount())
	}
	// Retry round concludes ad.
	fa.fail = false
	fa.verdict = fakeVerdict(true, "诈骗", 0.95)
	ok, _ := b.RetryRound(context.Background(), commentRetryItem(70, 90, bio, 1))
	if !ok {
		t.Fatal("the ad retry round should conclude")
	}
	if len(tg.bans) != 1 {
		t.Fatalf("retry ad conclusion must punish: %v", tg.bans)
	}
	if _, _, _, cached := commentStats(b); cached != 1 {
		t.Fatalf("retry ad conclusion must enter the flash cache: %d", cached)
	}
	// Repeat → flash hit, zero additional AI.
	commentDrive(b, tg, 71, bio, commentMsg(-500, 71, 91, "评论"))
	if fa.callCount() != 2 {
		t.Fatalf("flash hit after retry-cached bio: AI=%d, want 2 (first+retry)", fa.callCount())
	}
	if len(tg.bans) != 2 {
		t.Fatalf("flash hit must kick: %v", tg.bans)
	}
}

// --- 13: /statusz view -------------------------------------------------------------

func TestCommentBioStatuszView(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, commentCfg())
	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 80, bio, commentMsg(-500, 80, 95, "评论"))
	v := b.CommentBioView()
	if !v.Enabled {
		t.Fatal("CommentBioView.Enabled should be true")
	}
	if v.TriggeredTotal != 1 {
		t.Fatalf("TriggeredTotal = %d", v.TriggeredTotal)
	}
	if v.FlashCachedTotal != 1 {
		t.Fatalf("FlashCachedTotal = %d", v.FlashCachedTotal)
	}
	if v.FlashTTL != (30 * time.Minute).String() {
		t.Fatalf("FlashTTL = %s", v.FlashTTL)
	}
}

// commentStats is a small read helper for the bot gauges.
func commentStats(b *Bot) (enabled bool, triggered, flashHits, cached int64) {
	return b.CommentBioStats()
}

// commentRetryItem builds the retryq item for one comment-bio judgment
// (the queue's static shape; tests drive RetryRound directly).
func commentRetryItem(uid, messageID int64, bio string, attempts int) retryq.Item {
	return retryq.Item{
		Key: commentKey(uid, messageID), Kind: "comment", ChatID: -500,
		MessageID: messageID, UID: uid, Text: bio, Mode: "bio",
		Attempts: attempts, EnqueuedAt: time.Now(),
	}
}
