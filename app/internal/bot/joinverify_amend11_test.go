package bot

// amend11 验收测试（TASK_antispam_amend11.md §D，16 条）。全部离线假 TG：
// fakeTG / fakeAI / retryq 未 Start 的静态队列，不起真实长轮询（铁律 3）。
//
// 顺序类（1–10，本轮重点）：
//   1. 有简介 + 结论广告 → 处置发生，且没有欢迎消息、pending==0
//   2. 有简介 + 结论非广告 → 结论落地前无欢迎、无 pending；落地后才出现
//   3. 有简介 + 判定失败 → 不欢迎不登记；重试一轮结论非广告 → 才欢迎
//   4. 判定失败 → 重试耗尽 → 本地规则兜底：非广告 → 欢迎；广告 → 杀且无欢迎
//   5. 幂等：多轮结论落地只发一条欢迎、登记一次
//   6. 无简介 → 直接欢迎 + pending=1（入群 AI 简介调用 0 次）
//   7. 四条件/签名库命中 → 不欢迎、不登记
//   8. 结论非广告但人已被踢（getChatMember=left）→ 不欢迎、不登记（gone）
//   9. 验证期间发广告文本 → AI 调用数不变；贴纸通过后再发 → 判（+1）
//  10. 超时真封禁（ban 一次、不 unban）回归不变
// 健壮性类（11–16）：
//  11. chat_member kicked（别的 bot 踢的）→ 清 pending + 删欢迎 + 零处置
//  12. 通知丢失：无 chat_member，getChatMember=left → 到期零处置 +
//      join_verify_skipped_total +1 + 欢迎删除
//  13. restricted 且不能发言 → 到期零处置 + WARN
//  14. getChatMember 连续失败 → 仍处置一次（fail-open）
//  15. 处置报 USER_NOT_PARTICIPANT → 不 panic、照样清理 pending
//  16. 变量契约三方一致 + 四关全绿（config 契约测试 + go build/vet/fmt/test）
//     ——本文件仅钉 /statusz 新字段；四关由构建管线保证。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/retryq"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// --- shared helpers ---------------------------------------------------------------

// a11Cfg arms the gate with a long timeout (expiry is driven by expireNow
// time-travel, never by sleeping).
func a11Cfg() config.Config {
	return verifyCfg()
}

// a11PoolCfg arms the gate AND the async judge pool (for tests that must see
// the "before the conclusion" state — the handler returns while the AI chain
// is parked).
func a11PoolCfg() config.Config {
	c := verifyCfg()
	c.JudgeAsync = true
	c.JudgeQueue = 500
	c.JudgeConcurrency = 8
	c.AIJudgeTimeout = 150 * time.Second
	return c
}

// a11LogBot builds an armed bot whose log drains into logs (string assert on
// WARN/INFO lines).
func a11LogBot(t *testing.T, tg *fakeTG, fa AIClassifier, logs *strings.Builder, cfg config.Config) *Bot {
	t.Helper()
	st := store.NewMemory()
	lg := logx.New(logs, logx.Info, "text", "bot")
	return New(st, tg, fa, nil, cfg, lg)
}

// a11Join drives one bio joiner: join → (inline) AI bio round.
func a11Join(b *Bot, tg *fakeTG, uid int64, username, first, bio string) {
	tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: bio}
	tg.member[fkey(-500, uid)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: uid}}
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(uid, false, username, first)}, 0)
}

// a11RetryItem builds the retryq item for one join-bio judgment (the queue's
// static shape; tests drive RetryRound directly, like bot_test.go).
func a11RetryItem(uid int64, bio string, attempts int) retryq.Item {
	return retryq.Item{
		Key: joinKey(uid), Kind: "join", ChatID: -500, MessageID: 0, UID: uid,
		Text: bio, Mode: "bio", Attempts: attempts, EnqueuedAt: time.Now(),
	}
}

// memberUpdate builds a chat_member update for uid: old → new in chatID.
func memberUpdate(chatID, uid int64, old, new string) telegram.Update {
	return telegram.Update{
		UpdateID: 1,
		ChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: chatID, Type: "supergroup"},
			From:          telegram.User{ID: uid},
			OldChatMember: telegram.ChatMember{Status: old, User: telegram.User{ID: uid}},
			NewChatMember: telegram.ChatMember{Status: new, User: telegram.User{ID: uid}},
		},
	}
}

// --- D.1 有简介 + 结论广告 → 处置，无欢迎、pending==0 -----------------------------

func TestAmend11AdVerdictDisposesWithoutWelcome(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.95)}
	b := verifyBot(t, tg, fa, cfg)
	a11Join(b, tg, 9001, "adU", "阿甲", "专业办理各种证件")

	// The disposal happened (inline path).
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("ad verdict must dispose: %v", got)
	}
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("广告结论绝不发欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("广告结论绝不登记验证: pending=%d", pending)
	}
}

// --- D.2 有简介 + 结论非广告 → 结论落地前无欢迎、落地后才出现 ----------------------

func TestAmend11NonAdVerdictWelcomesOnlyAfterConclusion(t *testing.T) {
	cfg := a11PoolCfg()
	tg := newFakeTG()
	fa := newSlowFakeAI(fakeVerdict(false, "无", 0.9))
	b := poolBot(t, tg, fa, cfg)
	a11Join(b, tg, 9002, "okU", "阿乙", "爱读书爱旅行")

	// BEFORE the conclusion: no welcome, no pending.
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("结论落地前不得发欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("结论落地前不得登记: pending=%d", pending)
	}
	// The conclusion lands (unblock the AI).
	close(fa.block)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "结论落地后的欢迎")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("结论落地后应登记: pending=%d", pending)
	}
}

// --- D.3 判定失败 → 不欢迎不登记；重试非广告 → 才欢迎 ------------------------------

func TestAmend11RetryFailureThenNonAdWelcomes(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := verifyBot(t, tg, fa, cfg)
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()
	a11Join(b, tg, 9003, "retryU", "阿丙", "慢慢判定")
	// The first round failed → the retry queue holds it (no welcome, no reg).
	if fa.callCount() != 1 {
		t.Fatalf("首轮应已判定: %d", fa.callCount())
	}
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("判定失败期间不得发欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("判定失败期间不得登记: pending=%d", pending)
	}
	if rq.Len() != 1 {
		t.Fatalf("失败判定必须入重试队列: %d", rq.Len())
	}
	// Round 2 (retry) concludes non-ad → NOW the welcome.
	fa.fail = false
	fa.verdict = fakeVerdict(false, "无", 0.9)
	item := a11RetryItem(9003, "慢慢判定", 1)
	ok, code := b.RetryRound(ctx, item)
	if !ok || code != "" {
		t.Fatalf("retry round should conclude: ok=%v code=%q", ok, code)
	}
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "重试非广告结论后的欢迎")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("重试结论后应登记: pending=%d", pending)
	}
}

// --- D.4 重试耗尽 → 本地规则兜底 ---------------------------------------------------

func TestAmend11LocalFallbackNonAdWelcomes(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	a11Join(b, tg, 9004, "fbU", "阿丁", "我只是一个普通爱好者")
	// Last round → local fallback, below threshold → non-ad conclusion.
	item := a11RetryItem(9004, "我只是一个普通爱好者", cfg.RetryMax-1)
	ok, code := b.RetryRound(ctx, item)
	if !ok || code != "local_fallback" {
		t.Fatalf("兜底应终结: ok=%v code=%q", ok, code)
	}
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "兜底非广告后的欢迎")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("兜底非广告应登记: pending=%d", pending)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("兜底非广告不得处置: %v", got)
	}
}

func TestAmend11LocalFallbackAdKillsNoWelcome(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	a11Join(b, tg, 9005, "fbAdU", "阿戊", "办证 加V信 kfw123")
	item := a11RetryItem(9005, "办证 加V信 kfw123", cfg.RetryMax-1)
	ok, code := b.RetryRound(ctx, item)
	if !ok || code != "local_fallback" {
		t.Fatalf("兜底应终结: ok=%v code=%q", ok, code)
	}
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("兜底广告应杀: %v", got)
	}
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("兜底广告绝不欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("兜底广告不得登记: pending=%d", pending)
	}
}

// --- D.5 幂等：多轮结论落地只发一条欢迎 --------------------------------------------

func TestAmend11IdempotentWelcomeAcrossRounds(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	a11Join(b, tg, 9006, "idemU", "阿己", "兴趣爱好广泛")
	// Round 2 concludes non-ad (welcome fires).
	fa.fail = false
	fa.verdict = fakeVerdict(false, "无", 0.9)
	item := a11RetryItem(9006, "兴趣爱好广泛", 1)
	ok, _ := b.RetryRound(ctx, item)
	if !ok {
		t.Fatal("round 2 should conclude")
	}
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "唯一一条欢迎")
	// Round 3+ (a late retry round / the local fallback) re-delivers the
	// same non-ad conclusion → NO second welcome, NO re-registration.
	fa.fail = true
	item2 := a11RetryItem(9006, "兴趣爱好广泛", cfg.RetryMax-1)
	b.RetryRound(ctx, item2)
	if got := tg.sendsSnapshot(); len(got) != 1 {
		t.Fatalf("多轮结论只发一条欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("只登记一次: pending=%d", pending)
	}
}

// --- D.6 无简介 → 直接欢迎（入群 AI 简介调用 0 次） ---------------------------------

func TestAmend11NoBioWelcomesImmediately(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	a11Join(b, tg, 9007, "noBioU", "阿庚", "")
	if fa.callCount() != 0 {
		t.Fatalf("无简介者入群 AI 简介调用应为 0: %d", fa.callCount())
	}
	if got := tg.sendsSnapshot(); len(got) != 1 {
		t.Fatalf("无简介应直接欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("无简介应登记: pending=%d", pending)
	}
}

// --- D.7 四条件 / 签名库命中 → 不欢迎、不登记 ----------------------------------------

func TestAmend11NakedPremiumNoWelcome(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	// 四条件全中：premium + 英文名 + 无简介 + 无头像。
	tg.chat[9008] = telegram.ChatInfo{ID: 9008, Bio: ""}
	tg.member[fkey(-500, 9008)] = telegram.ChatMember{
		Status: "member", User: telegram.User{ID: 9008, IsPremium: true}}
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	b.HandleNewMembers(context.Background(), -500,
		[]telegram.User{u(9008, false, "nakedU", "Lynne")}, 0)
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("四条件应处置: %v", got)
	}
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("四条件命中绝不欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("四条件命中不得登记: pending=%d", pending)
	}
}

func TestAmend11SignatureHitNoWelcome(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	const bio = "专业办理各种证件 加V信 xxx"
	tg.chat[9009] = telegram.ChatInfo{ID: 9009, Bio: bio}
	tg.member[fkey(-500, 9009)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 9009}}
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	if err := b.Store.AddBioSignature(context.Background(),
		sigOf(bio), bio); err != nil {
		t.Fatal(err)
	}
	b.HandleNewMembers(context.Background(), -500,
		[]telegram.User{u(9009, false, "sigU", "阿辛")}, 0)
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("签名库应处置: %v", got)
	}
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("签名库命中绝不欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("签名库命中不得登记: pending=%d", pending)
	}
}

// sigOf is normalize.Signature for test brevity.
func sigOf(s string) string {
	return normalize.Signature(s)
}

// --- D.8 结论非广告但人已被踢 → 不欢迎、不登记（gone） -------------------------------

func TestAmend11NonAdButAlreadyLeftSkipsWelcome(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	// The joiner is already gone BEFORE the conclusion fires (kicked by
	// someone between the join and the AI verdict landing).
	tg.chat[9010] = telegram.ChatInfo{ID: 9010, Bio: "正常简介"}
	tg.member[fkey(-500, 9010)] = telegram.ChatMember{Status: "left", User: telegram.User{ID: 9010}}
	b.HandleNewMembers(context.Background(), -500,
		[]telegram.User{u(9010, false, "goneU", "阿壬")}, 0)
	if got := tg.sendsSnapshot(); len(got) != 0 {
		t.Fatalf("人已不在群不得欢迎: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("人已不在群不得登记: pending=%d", pending)
	}
	if got := b.JoinVerifySkippedTotal(); got != 1 {
		t.Fatalf("join_verify_skipped_total 应 +1: %d", got)
	}
}

// --- D.9 验证期间广告文本不送 AI；通过后判 --------------------------------------------

func TestAmend11PendingBlocksAIUntilPass(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	a11Join(b, tg, 9011, "waitU", "阿癸", "")
	if fa.callCount() != 0 {
		t.Fatalf("无简介入群 0 次 AI: %d", fa.callCount())
	}
	// Ad text during pending: swallowed, AI count unchanged.
	b.HandleMessage(context.Background(), adMsg(-500, 9011, 970, "加V赚钱 上门服务"))
	if fa.callCount() != 0 {
		t.Fatalf("验证期间广告文本不得送 AI: %d", fa.callCount())
	}
	// Sticker passes → the next ad text IS judged (+1).
	b.HandleMessage(context.Background(), stickerMsg(-500, 9011, 971))
	waitFor(t, func() bool { return b.joinVerify == nil || true }, "noop") // pass is synchronous
	if _, pending, passed, _ := b.JoinVerifyStats(); pending != 0 || passed != 1 {
		t.Fatalf("贴纸应通过: pending=%d passed=%d", pending, passed)
	}
	b.HandleMessage(context.Background(), adMsg(-500, 9011, 972, "加V赚钱 上门服务"))
	waitFor(t, func() bool { return fa.callCount() == 1 }, "通过后广告文本应送 AI")
}

// --- D.10 超时真封禁回归不变 ---------------------------------------------------------

func TestAmend11TimeoutRealBanRegression(t *testing.T) {
	cfg := a11Cfg()
	b, tg, _ := verifyJoin(t, cfg, 9012, "slowU2", "阿子", "", false)
	expireNow(b)
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("超时必须一次真封禁: %v", got)
	}
	if _, pending, _, timeouts := b.JoinVerifyStats(); pending != 0 || timeouts != 1 {
		t.Fatalf("超时计数: pending=%d timeouts=%d", pending, timeouts)
	}
}

// --- D.11 别的 bot 踢（chat_member kicked）→ 清 pending+删欢迎+零处置 ------------------

func TestAmend11KickedByOtherBotClearsNoDisposal(t *testing.T) {
	cfg := a11Cfg()
	b, tg, _ := verifyJoin(t, cfg, 9013, "kickU2", "阿丑", "", false)
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	// Kicked by another bot (the chat_member update fires).
	b.HandleUpdate(context.Background(), memberUpdate(-500, 9013, "member", "kicked"))
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("被踢应清 pending: %d", pending)
	}
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("被踢应删欢迎: %v", got)
	}
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("被别人踢零处置: %v", got)
	}
	// The deadline can no longer reach the cleared entry.
	expireNow(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("到期不得再处置被踢者: %v", got)
	}
}

// --- D.12 通知丢失：getChatMember=left → 到期零处置 + skipped +1 + 删欢迎 ---------------

func TestAmend11TimeoutNotificationLostLeftNoDisposal(t *testing.T) {
	cfg := a11Cfg()
	var logs strings.Builder
	tg := newFakeTG()
	fa := &fakeAI{}
	b := a11LogBot(t, tg, fa, &logs, cfg)
	a11Join(b, tg, 9014, "lostU", "阿寅", "")
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	// NO chat_member update at all (notification lost); the deadline re-check
	// sees the user left.
	tg.member[fkey(-500, 9014)] = telegram.ChatMember{Status: "left", User: telegram.User{ID: 9014}}
	expireNow(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("通知丢失+人已走 → 到期零处置: %v", got)
	}
	if got := b.JoinVerifySkippedTotal(); got != 1 {
		t.Fatalf("join_verify_skipped_total 应 +1: %d", got)
	}
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("欢迎消息应删除: %v", got)
	}
	if !strings.Contains(logs.String(), "join_verify_timeout_skipped") {
		t.Fatalf("应有 join_verify_timeout_skipped 日志:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `reason="gone"`) {
		t.Fatalf("应有 reason=gone:\n%s", logs.String())
	}
}

// --- D.13 restricted 且不能发言 → 到期零处置 + WARN -----------------------------------

func TestAmend11TimeoutRestrictedMuteNoDisposal(t *testing.T) {
	cfg := a11Cfg()
	var logs strings.Builder
	tg := newFakeTG()
	fa := &fakeAI{}
	b := a11LogBot(t, tg, fa, &logs, cfg)
	a11Join(b, tg, 9015, "mutU", "阿卯", "")
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	tg.member[fkey(-500, 9015)] = telegram.ChatMember{
		Status: "restricted", User: telegram.User{ID: 9015},
		IsMember: true, CanSendMessages: false, CanSendOtherMessages: false}
	expireNow(b)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("被限制不能发言 → 到期零处置: %v", got)
	}
	if got := b.JoinVerifySkippedTotal(); got != 1 {
		t.Fatalf("skipped 应 +1: %d", got)
	}
	if !strings.Contains(logs.String(), "unable_to_verify") {
		t.Fatalf("应 WARN unable_to_verify:\n%s", logs.String())
	}
}

// --- D.14 getChatMember 连续失败 → 仍处置一次（fail-open） ------------------------------

func TestAmend11TimeoutMemberCheckFailsOpen(t *testing.T) {
	cfg := a11Cfg()
	var logs strings.Builder
	tg := newFakeTG()
	fa := &fakeAI{}
	b := a11LogBot(t, tg, fa, &logs, cfg)
	a11Join(b, tg, 9016, "failU", "阿辰", "")
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	tg.memberErr = errors.New("flood wait")
	expireNow(b)
	if got := tg.banList(); len(got) != 1 || got[0] != "ban" {
		t.Fatalf("查询失败 fail-open 应仍处置一次: %v", got)
	}
	if !strings.Contains(logs.String(), "join_verify_member_check_failed") {
		t.Fatalf("应 WARN fail-open:\n%s", logs.String())
	}
	_, _, _, timeouts := b.JoinVerifyStats()
	if timeouts != 1 {
		t.Fatalf("fail-open 处置计入 timeout: %d", timeouts)
	}
}

// --- D.15 处置报 USER_NOT_PARTICIPANT → 不 panic、照样清理 ------------------------------

func TestAmend11DisposeUserNotParticipantNonFatal(t *testing.T) {
	cfg := a11Cfg()
	var logs strings.Builder
	tg := newFakeTG()
	fa := &fakeAI{}
	b := a11LogBot(t, tg, fa, &logs, cfg)
	a11Join(b, tg, 9017, "gone2U", "阿巳", "")
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	// The user left without any notification; the re-check ALSO fails hard,
	// so the disposal fires and the API rejects it (user not participant).
	tg.memberErr = &telegram.APIError{
		Code: 400, Description: "Bad Request: user not found", Method: "getChatMember"}
	tg.banErr = &telegram.APIError{
		Code: 400, Description: "Bad Request: USER_NOT_PARTICIPANT", Method: "banChatMember"}
	expireNow(b)
	// No panic (the test reaching here proves it), pending is cleared, and
	// the welcome was still deleted.
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("处置报错后照样清理 pending: %d", pending)
	}
	if got := tg.deleteList(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("处置报错后照样删欢迎: %v", got)
	}
	if !strings.Contains(logs.String(), "ban_error") {
		t.Fatalf("应 WARN/ERROR ban_error（不致命）:\n%s", logs.String())
	}
}

// --- D.16 /statusz 新字段（join_verify_skipped_total）----------------------------------

func TestAmend11StatuszSkippedField(t *testing.T) {
	// The server-side rendering is covered by server's
	// TestStatuszJoinVerifyBlock; here pin the bot-side view adapter.
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{}
	b := verifyBot(t, tg, fa, cfg)
	a11Join(b, tg, 9018, "statU2", "阿午", "")
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	tg.member[fkey(-500, 9018)] = telegram.ChatMember{Status: "left", User: telegram.User{ID: 9018}}
	expireNow(b)
	v := b.JoinVerifyView()
	if v.SkippedTotal != 1 {
		t.Fatalf("JoinVerifyView.SkippedTotal = %d, want 1", v.SkippedTotal)
	}
	if got := b.JoinVerifySkippedTotal(); got != 1 {
		t.Fatalf("JoinVerifySkippedTotal = %d, want 1", got)
	}
}

// --- bonus: 本方处置链处置了 pending 用户 → 立刻清 pending，到期不再处置（铁律 6） -----

// Scenario (amend11 B.2): round 1 concluded non-ad → welcome + pending; a
// LATER retry round of the same judgment concludes AD (e.g. the vote was
// unresolved and the re-run flipped) → our own chain disposes the user →
// pending clears IMMEDIATELY and the 300s deadline must NOT dispose again.
func TestAmend11OwnDisposalClearsPendingImmediately(t *testing.T) {
	cfg := a11Cfg()
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := verifyBot(t, tg, fa, cfg)
	ctx := context.Background()
	a11Join(b, tg, 9019, "lateU", "阿未", "正常简介")
	waitFor(t, func() bool { return len(tg.sendsSnapshot()) == 1 }, "欢迎")
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 1 {
		t.Fatalf("应已登记: %d", pending)
	}
	// A later retry round of the SAME join judgment concludes AD.
	fa.verdict = fakeVerdict(true, "广告", 0.95)
	ok, _ := b.RetryRound(ctx, a11RetryItem(9019, "正常简介", 1))
	if !ok {
		t.Fatal("the ad retry round should conclude")
	}
	// Disposed once; pending cleared immediately (cleared BEFORE the deadline
	// could ever see it).
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("本方处置应发生: %v", got)
	}
	if _, pending, _, _ := b.JoinVerifyStats(); pending != 0 {
		t.Fatalf("处置后立刻清 pending: %d", pending)
	}
	// The 300s deadline must NOT dispose them a second time.
	expireNow(b)
	if got := tg.banList(); len(got) != 1 {
		t.Fatalf("到期不得重复处置: %v", got)
	}
}
