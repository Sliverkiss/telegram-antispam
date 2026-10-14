package bot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/notify"
	"adbot/internal/retryq"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// fakeTG records every call for sequence assertions.
type fakeTG struct {
	mu sync.Mutex // guards the record slices (announce goroutines write concurrently)

	chat    map[int64]telegram.ChatInfo
	member  map[string]telegram.ChatMember // "chat:uid"
	bans    []string                       // "ban" / "unban"
	deletes []string                       // message ids deleted (as string)
	sends   []string
	leaves  []int64
	seq     []string

	// Image path: file metadata + downloads, with injectable failures.
	files       map[string]telegram.FileMeta
	fileErr     map[string]error
	downloads   map[string][]byte
	downloadN   int
	downloadErr error

	// deleteScript controls successive DeleteMessage results (nil entry = ok).
	deleteScript []error
	deleteCalls  int

	// sendErr (amend7 tests): when set, every SendMessage returns it.
	sendErr error
}

func newFakeTG() *fakeTG {
	return &fakeTG{
		chat:      map[int64]telegram.ChatInfo{},
		member:    map[string]telegram.ChatMember{},
		files:     map[string]telegram.FileMeta{},
		fileErr:   map[string]error{},
		downloads: map[string][]byte{},
	}
}

func (f *fakeTG) GetChatMember(_ context.Context, chatID, userID int64) (telegram.ChatMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "getchatmember")
	if m, ok := f.member[fkey(chatID, userID)]; ok {
		return m, nil
	}
	return telegram.ChatMember{Status: "member"}, nil
}
func (f *fakeTG) GetChat(_ context.Context, chatID int64) (telegram.ChatInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "getchat")
	return f.chat[chatID], nil
}
func (f *fakeTG) BanChatMember(_ context.Context, chatID, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "ban")
	f.bans = append(f.bans, "ban")
	return nil
}
func (f *fakeTG) UnbanChatMember(_ context.Context, chatID, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "unban")
	f.bans = append(f.bans, "unban")
	return nil
}
func (f *fakeTG) DeleteMessage(_ context.Context, chatID, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "delete")
	f.deleteCalls++
	i := f.deleteCalls - 1
	if i < len(f.deleteScript) && f.deleteScript[i] != nil {
		return f.deleteScript[i]
	}
	f.deletes = append(f.deletes, strconv.FormatInt(messageID, 10))
	return nil
}
func (f *fakeTG) SendMessage(_ context.Context, chatID int64, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "send")
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sends = append(f.sends, text)
	return 1, nil
}
func (f *fakeTG) LeaveChat(_ context.Context, chatID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = append(f.seq, "leave")
	f.leaves = append(f.leaves, chatID)
	return nil
}
func (f *fakeTG) GetFileMetadata(_ context.Context, fileID string) (telegram.FileMeta, error) {
	f.mu.Lock()
	f.seq = append(f.seq, "getfile")
	f.mu.Unlock()
	if err, ok := f.fileErr[fileID]; ok {
		return telegram.FileMeta{}, err
	}
	if m, ok := f.files[fileID]; ok {
		return m, nil
	}
	return telegram.FileMeta{FileID: fileID, FilePath: "photos/" + fileID + ".jpg", FileSize: 8}, nil
}
func (f *fakeTG) DownloadFile(_ context.Context, meta telegram.FileMeta, _ int64) ([]byte, error) {
	f.mu.Lock()
	f.seq = append(f.seq, "download")
	f.downloadN++
	f.mu.Unlock()
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	if d, ok := f.downloads[meta.FileID]; ok {
		return d, nil
	}
	return []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}, // PNG magic
		nil
}

func fkey(chatID, userID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(userID, 10)
}

// fakeAI implements bot.AIClassifier with counters.
type fakeAI struct {
	mu          sync.Mutex
	calls       int
	fail        bool // ok=false on every call
	verdict     ai.Verdict
	lastContent ai.Content
	lastMode    string
}

func (f *fakeAI) Classify(_ context.Context, content ai.Content, mode string) (ai.ClassifyResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastContent = content
	f.lastMode = mode
	res := ai.ClassifyResult{Verdict: f.verdict, Model: "fake-model"}
	return res, !f.fail
}

func (f *fakeAI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func fakeVerdict(isAd bool, category string, conf float64) ai.Verdict {
	return ai.Verdict{IsAd: isAd, Category: category, Confidence: conf, Reason: "test"}
}

func testBot(t *testing.T, tg TelegramClient, classify AIClassifier, cfg config.Config) *Bot {
	t.Helper()
	st := store.NewMemory()
	lg := logx.New(nil, logx.Error, "text", "bot")
	return New(st, tg, classify, nil, cfg, lg)
}

// voteBot wires a Verifier into the bot.
func voteBot(t *testing.T, tg TelegramClient, classify AIClassifier, verify Verifier, cfg config.Config) *Bot {
	t.Helper()
	st := store.NewMemory()
	lg := logx.New(nil, logx.Error, "text", "bot")
	return New(st, tg, classify, verify, cfg, lg)
}

func defaultCfg() config.Config {
	return config.Config{
		Action:       "ban",
		IgnoreAdmins: true,
		LogChat:      0,
		Chats:        map[int64]bool{-100: true, -200: true, -300: true, -400: true, -500: true, -600: true, -700: true, -800: true, -900: true, -10: true, -1000: true},
		Store:        "memory",
		Version:      "test",
		// Vote/verify knobs (tests exercise the confidence gate).
		MinConfidence:      0.85,
		VerifyVotes:        3,
		VerifyMajority:     2,
		LocalRulesEnabled:  true,
		LocalRuleThreshold: 3,
		SimhashDistance:    3,
		RetryInterval:      10 * 1_000_000_000, // 1s in ns (unused unless retryq wired)
		RetryMax:           5,
		// amend4: notify defaults (overridden per-test).
		NotifyOnPunish:     true,
		NotifyOnError:      true,
		NotifyLevel:        "error",
		NotifyRatePerMin:   10000, // effectively unlimited in tests
		NotifyQueue:        200,
		NotifyDedupeWindow: 5 * time.Minute,
		NotifyMaxLen:       1024,
		NotifyTextLen:      120,
		NotifyIncludeText:  true,
		NotifyDrain:        2 * time.Second,
		AdminCacheTTL:      5 * time.Minute,
		AdminCacheMax:      5000,
	}
}

// notifyRecorder is a thread-safe sink for notifier deliveries: the notifier
// worker runs on its own goroutine, so tests must read through texts().
type notifyRecorder struct {
	mu    sync.Mutex
	sends []string
	err   error // when set, every SendMessage returns it (delivery failure)
}

func (r *notifyRecorder) SendMessage(_ context.Context, _ int64, text string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	r.sends = append(r.sends, text)
	return 1, nil
}

// texts returns a copy of the delivered messages.
func (r *notifyRecorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sends...)
}

// wireTestNotifier builds a live notifier around a dedicated recorder (drain
// on test-end via Stop) and returns the recorder. Only tests that assert on
// disposition/error notifications need it.
func wireTestNotifier(t *testing.T, b *Bot, cfg config.Config) *notifyRecorder {
	t.Helper()
	return wireTestNotifierSink(t, b, cfg, &notifyRecorder{})
}

func wireTestNotifierSink(t *testing.T, b *Bot, cfg config.Config, rec *notifyRecorder) *notifyRecorder {
	t.Helper()
	n := notify.New(notify.Config{
		LogChat:      cfg.LogChat,
		OnPunish:     cfg.NotifyOnPunish,
		OnError:      cfg.NotifyOnError,
		Level:        logx.ParseLevel(cfg.NotifyLevel),
		RatePerMin:   cfg.NotifyRatePerMin,
		QueueSize:    cfg.NotifyQueue,
		DedupeWindow: cfg.NotifyDedupeWindow,
		MaxLen:       cfg.NotifyMaxLen,
		TextLen:      cfg.NotifyTextLen,
		IncludeText:  cfg.NotifyIncludeText,
		Drain:        cfg.NotifyDrain,
	}, rec, logx.New(nil, logx.Error, "text", "notify"))
	b.Notifier = n
	n.Start(context.Background())
	t.Cleanup(n.Stop)
	return rec
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func u(id int64, isBot bool, username, first string) telegram.User {
	return telegram.User{ID: id, IsBot: isBot, Username: username, FirstName: first}
}

func adMsg(chatID, uid, messageID int64, text string) telegram.Message {
	return telegram.Message{
		MessageID: messageID, From: &telegram.User{ID: uid},
		Chat: telegram.Chat{ID: chatID, Type: "supergroup"}, Text: text,
	}
}

// --- Join screening ----------------------------------------------------------

func TestJoinAdBioDirectDisposal(t *testing.T) {
	tg := newFakeTG()
	tg.chat[100] = telegram.ChatInfo{ID: 100, Bio: "专业办理各种证件 加V信 xxx"}
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	if err := b.Store.AddBioSignature(context.Background(), normalize.Signature("专业办理各种证件 加V信 xxx"), "专业办理各种证件 加V信 xxx"); err != nil {
		t.Fatal(err)
	}
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(100, false, "spammer", "Spammer")}, 0)

	if len(tg.bans) != 1 || tg.bans[0] != "ban" {
		t.Fatalf("bans = %v, want one ban", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("AI must not run on signature hit: %d", fa.callCount())
	}
}

func TestJoinSimhashBioNearHitDisposal(t *testing.T) {
	// 防改字绕过: a stored bio signature with its trailing digit changed must
	// still dispose the joiner without an AI call (hamming distance 1).
	tg := newFakeTG()
	orig := "上门服务资源看片 加V:8888"
	tg.chat[101] = telegram.ChatInfo{ID: 101, Bio: "上门服务资源看片 加V:8889"}
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	if err := b.Store.AddBioSignature(context.Background(), normalize.Signature(orig), orig); err != nil {
		t.Fatal(err)
	}
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(101, false, "near", "Near")}, 0)
	if len(tg.bans) != 1 {
		t.Fatalf("simhash near-hit must dispose: bans=%v", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("near-hit must not consume AI: %d", fa.callCount())
	}
}

func TestJoinNormalBioKeepsWatchlistOnly(t *testing.T) {
	tg := newFakeTG()
	tg.chat[200] = telegram.ChatInfo{ID: 200, Bio: "我是正常人"}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
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
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
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
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(400, true, "bot", "B")}, 0)
	if n, _ := b.Store.SuspectCount(context.Background()); n != 0 {
		t.Fatalf("bots must be skipped; suspects=%d", n)
	}
}

func TestJoinNonWhitelistedIgnored(t *testing.T) {
	tg := newFakeTG()
	tg.chat[999] = telegram.ChatInfo{ID: 999, Bio: "广告 加V"}
	fa := &fakeAI{verdict: fakeVerdict(true, "办证", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	b.HandleNewMembers(context.Background(), -777, []telegram.User{u(999, false, "spam", "S")}, 0)
	if fa.callCount() != 0 {
		t.Fatalf("non-whitelisted join must be zero AI: %d", fa.callCount())
	}
	if n, _ := b.Store.SuspectCount(context.Background()); n != 0 {
		t.Fatalf("non-whitelisted join must not write: %d suspects", n)
	}
}

// --- Message screening -------------------------------------------------------

func TestMessageSignatureHitDirectDisposal(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 5, ChatID: -500})
	b.Store.AddMessageSignature(ctx, normalize.Signature("专业办证 加V信 111"), "专业办证 加V信 111")

	b.HandleMessage(ctx, adMsg(-500, 5, 13, "专业办证 加V信 111"))

	// Only the triggering message is deleted (no tracked-message dump).
	if len(tg.deletes) != 1 || tg.deletes[0] != "13" {
		t.Fatalf("deletes = %v, want just the triggering id 13", tg.deletes)
	}
	if len(tg.bans) != 1 {
		t.Fatalf("bans = %v", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("AI must not run on signature hit: %d", fa.callCount())
	}
	if sus, _ := b.Store.IsSuspect(ctx, 5); sus {
		t.Fatal("suspect should be removed after disposal")
	}
}

func TestMessageSimhashNearHit(t *testing.T) {
	// 改字 variant of a known ad signature still disposes (A.9).
	tg := newFakeTG()
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 15, ChatID: -500})
	orig := "免费看片 上门服务 加V:6666"
	b.Store.AddMessageSignature(ctx, normalize.Signature(orig), orig)

	b.HandleMessage(ctx, adMsg(-500, 15, 14, "免费看片 上门服務 加V:6666"))

	if len(tg.bans) != 1 {
		t.Fatalf("simhash hit must dispose: %v", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("simhash hit must not consume AI: %d", fa.callCount())
	}
}

func TestMessageSimhashNoFalseHit(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 16, ChatID: -500})
	orig := "免费看片 上门服务 加V:6666"
	b.Store.AddMessageSignature(ctx, normalize.Signature(orig), orig)

	b.HandleMessage(ctx, adMsg(-500, 16, 15, "今天天气不错适合散步"))

	// Unrelated text must NOT simhash-hit; it runs the AI instead.
	if fa.callCount() != 1 {
		t.Fatalf("unrelated text must reach AI: %d", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("no punishment: %v", tg.bans)
	}
}

func TestMessageAIAdFullSequence(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.9)}
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	rec := wireTestNotifier(t, b, cfg)
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})

	b.HandleMessage(ctx, adMsg(-600, 6, 22, "加QQ赚钱 日入过万"))

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
	// Disposition notification (async; the worker delivers on its own goroutine).
	// §1.1 content: uid, action, evidence, score — all present.
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "disposition notify")
	nb := rec.texts()[0]
	for _, want := range []string{
		"⏱️ 处理时间:", "💬 加QQ赚钱 日入过万", "👤 无用户名 · 6",
		"✅ 已封禁 · 90分",
	} {
		if !strings.Contains(nb, want) {
			t.Fatalf("notify text missing %q:\n%s", want, nb)
		}
	}
	if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature("加QQ赚钱 日入过万")); !ok {
		t.Fatal("ad message should persist to message signature library")
	}
	if sus, _ := b.Store.IsSuspect(ctx, 6); sus {
		t.Fatal("suspect should be removed")
	}
}

func TestMessageAIAdNoNotifyWhenDisabled(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LogChat = -999
	cfg.NotifyOnPunish = false // BOT_NOTIFY_ON_PUNISH=false → zero pushes
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.9)}
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	rec := wireTestNotifier(t, b, cfg)
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})

	b.HandleMessage(ctx, adMsg(-600, 6, 23, "加QQ赚钱 日入过万"))

	// Disposal still happens; no notification is pushed.
	if len(tg.bans) != 1 || len(tg.deletes) != 1 {
		t.Fatalf("disposal must still run: bans=%v deletes=%v", tg.bans, tg.deletes)
	}
	time.Sleep(50 * time.Millisecond)
	if got := rec.texts(); len(got) != 0 {
		t.Fatalf("BOT_NOTIFY_ON_PUNISH=false must push nothing: %v", got)
	}
}

// TestMessageAIAdNotifySendFailKeepsDisposal — §6: a SendMessage failure in
// the notifier must never break the disposition flow. The notification channel
// returns 500; the delete+ban+signature+watchlist removal must still complete.
func TestMessageAIAdNotifySendFailKeepsDisposal(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.9)}
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	rec := &notifyRecorder{err: errors.New("sendMessage 500 upstream")}
	wireTestNotifierSink(t, b, cfg, rec)
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})

	b.HandleMessage(ctx, adMsg(-600, 6, 24, "加QQ赚钱 日入过万"))

	// Disposition completes even though the notification send always fails.
	if len(tg.deletes) != 1 || tg.deletes[0] != "24" {
		t.Fatalf("deletes = %v, want [24]", tg.deletes)
	}
	if len(tg.bans) != 1 {
		t.Fatalf("bans = %v", tg.bans)
	}
	if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature("加QQ赚钱 日入过万")); !ok {
		t.Fatal("signature must still be written despite notify failure")
	}
	if sus, _ := b.Store.IsSuspect(ctx, 6); sus {
		t.Fatal("suspect must still be removed despite notify failure")
	}
	// The notifier attempted the send (and failed); nothing is delivered.
	if got := rec.texts(); len(got) != 0 {
		t.Fatalf("all sends fail, nothing delivered: %v", got)
	}
}

func TestMessageNormalNoop(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 7, ChatID: -700})
	b.HandleMessage(ctx, adMsg(-700, 7, 30, "今天天气不错"))
	if len(tg.bans) != 0 {
		t.Fatalf("non-ad must not dispose: %v", tg.bans)
	}
	// amend5 D: one judgment = one consumption — a completed not-ad verdict
	// removes the user from the watchlist.
	if sus, _ := b.Store.IsSuspect(ctx, 7); sus {
		t.Fatal("not-ad verdict must consume the watchlist slot")
	}
}

func TestMessageAdminSkipped(t *testing.T) {
	tg := newFakeTG()
	tg.member[fkey(-800, 8)] = telegram.ChatMember{Status: "administrator"}
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 8, ChatID: -800})
	b.HandleMessage(ctx, adMsg(-800, 8, 40, "广告"))
	if len(tg.bans) != 0 {
		t.Fatalf("admin must be skipped: %v", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("AI must not run for admins: %d", fa.callCount())
	}
}

func TestMessageAIFailureEnqueuesRetryNotBan(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	cfg := defaultCfg()
	b := testBot(t, tg, fa, cfg)
	// Wire a retry queue so the failure lands in the pending queue.
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 9, ChatID: -900})
	b.HandleMessage(ctx, adMsg(-900, 9, 50, "可疑内容"))
	if len(tg.bans) != 0 {
		t.Fatalf("AI failure must fail safe (no ban): %v", tg.bans)
	}
	if _, _, _, aiFails := b.Stats(); aiFails == 0 {
		t.Fatal("AI failure should be counted")
	}
	if rq.Len() != 1 {
		t.Fatalf("failed judgment must be enqueued: len=%d", rq.Len())
	}
}

func TestMessageBotOrEmptySkipped(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.HandleMessage(ctx, adMsg(-10, 10, 60, "广告"))
	b.HandleMessage(ctx, adMsg(-10, 11, 61, "   "))
	// Service message: no text, no media.
	b.HandleMessage(ctx, telegram.Message{MessageID: 62, From: &telegram.User{ID: 12}, Chat: telegram.Chat{ID: -10, Type: "supergroup"}})
	if fa.callCount() != 0 {
		t.Fatalf("bot/empty/service messages must not hit AI: %d", fa.callCount())
	}
}

func TestKickModeUnbans(t *testing.T) {
	tg := newFakeTG()
	tg.chat[100] = telegram.ChatInfo{ID: 100, Bio: "办证 加V"}
	cfg := defaultCfg()
	cfg.Action = "kick"
	fa := &fakeAI{verdict: fakeVerdict(true, "办证", 0.9)}
	b := testBot(t, tg, fa, cfg)
	b.HandleNewMembers(context.Background(), -100, []telegram.User{u(100, false, "k", "K")}, 0)
	if len(tg.bans) != 2 || tg.bans[0] != "ban" || tg.bans[1] != "unban" {
		t.Fatalf("kick mode should ban then unban: %v", tg.bans)
	}
}

// --- Photo multimodal + file-hash dedup (A.3) --------------------------------

func photoMsg(chatID, uid, messageID int64, caption, fileID, uniqueID string) telegram.Message {
	m := adMsg(chatID, uid, messageID, "")
	m.Caption = caption
	// Two sizes; the larger (second) must be picked.
	m.Photo = []telegram.PhotoSize{
		{FileID: fileID + "-small", FileUniqueID: uniqueID + "-small", Width: 100, Height: 100},
		{FileID: fileID, FileUniqueID: uniqueID, Width: 800, Height: 600},
	}
	return m
}

func TestPhotoMessageJudgedMultimodally(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "色情", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 20, ChatID: -300})

	b.HandleMessage(ctx, photoMsg(-300, 20, 31, "看图加V", "f1", "u1"))

	// The AI call must carry the image data URL + caption text.
	fa.mu.Lock()
	content := fa.lastContent
	mode := fa.lastMode
	fa.mu.Unlock()
	if !strings.HasPrefix(content.ImageURL, "data:image/png;base64,") {
		t.Fatalf("image must be judged multimodally, ImageURL = %q", content.ImageURL)
	}
	if content.Text != "看图加V" {
		t.Errorf("caption must be the text part: %q", content.Text)
	}
	if mode != "message" {
		t.Errorf("mode = %q", mode)
	}
	// Largest photo requested (not the small variant).
	if !strings.Contains(strings.Join(tg.seq, ","), "getfile") {
		t.Fatalf("getFile not called: %v", tg.seq)
	}
	// Image wins over text on conflict (here: ad verdict punishes).
	if len(tg.bans) != 1 {
		t.Fatalf("ad image verdict must punish: %v", tg.bans)
	}
}

func TestPhotoFileHashDedup(t *testing.T) {
	// The same file_unique_id is judged once: the second message reuses the
	// cached verdict without a new AI call or re-download.
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "色情", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 21, ChatID: -300})
	b.Store.AddSuspect(ctx, store.Suspect{UID: 22, ChatID: -300})

	b.HandleMessage(ctx, photoMsg(-300, 21, 32, "看图1", "f2", "u2"))
	b.HandleMessage(ctx, photoMsg(-300, 22, 33, "看图2", "f2", "u2"))

	if fa.callCount() != 1 {
		t.Fatalf("same file_unique_id must be judged once, AI calls = %d", fa.callCount())
	}
	if tg.downloadN != 1 {
		t.Fatalf("same file_unique_id must be downloaded once, downloads = %d", tg.downloadN)
	}
	// Both punishers were disposed (cached verdict reused).
	if len(tg.bans) != 2 {
		t.Fatalf("both users must be punished: %v", tg.bans)
	}
}

func TestImageDocumentJudged(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 23, ChatID: -300})
	m := adMsg(-300, 23, 34, "")
	m.Caption = "文件"
	m.Document = &telegram.Document{FileID: "d1", FileName: "ad.png", MimeType: "image/png"}
	b.HandleMessage(ctx, m)
	fa.mu.Lock()
	content := fa.lastContent
	fa.mu.Unlock()
	if content.ImageURL == "" {
		t.Fatal("image document must be judged multimodally")
	}
}

func TestImageDownloadFailureEnqueuedNotDropped(t *testing.T) {
	tg := newFakeTG()
	tg.downloadErr = errors.New("network down")
	fa := &fakeAI{verdict: fakeVerdict(true, "色情", 0.95)}
	cfg := defaultCfg()
	b := testBot(t, tg, fa, cfg)
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 24, ChatID: -300})

	b.HandleMessage(ctx, photoMsg(-300, 24, 35, "看图", "f3", "u3"))

	if fa.callCount() != 0 {
		t.Fatalf("download failure must not hit AI: %d", fa.callCount())
	}
	if rq.Len() != 1 {
		t.Fatalf("download failure must enqueue for retry: %d", rq.Len())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("no punishment on download failure: %v", tg.bans)
	}
}

// --- Confidence gate → vote (A.4) ---------------------------------------------

func TestLowConfidenceVoteAdPunishes(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)} // below 0.85
	verifyCalls := 0
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		verifyCalls++
		return ai.VerifyResult{VotesTotal: 3, VotesAd: 2, Final: "ad"}
	}
	b := voteBot(t, tg, fa, verify, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 30, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 30, 41, "加V 下注"))

	if verifyCalls != 1 {
		t.Fatalf("low-confidence ad must trigger one vote round: %d", verifyCalls)
	}
	if len(tg.bans) != 1 {
		t.Fatalf("vote majority ad must punish: %v", tg.bans)
	}
}

func TestLowConfidenceVoteNotAdReleases(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{VotesTotal: 3, VotesNo: 3, Final: "not_ad"}
	}
	b := voteBot(t, tg, fa, verify, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 31, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 31, 42, "加V 下注"))

	if len(tg.bans) != 0 {
		t.Fatalf("vote not_ad must release: %v", tg.bans)
	}
	// amend5 D: the vote concluded — the watchlist slot is consumed.
	if sus, _ := b.Store.IsSuspect(ctx, 31); sus {
		t.Fatal("vote not_ad must consume the watchlist slot")
	}
}

func TestLowConfidenceVoteUnresolvedEnqueuesRetry(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		// 1 ad, 1 no, 1 abstain → short of majority → unresolved.
		return ai.VerifyResult{VotesTotal: 3, VotesAd: 1, VotesNo: 1, Abstentions: 1, Final: "unresolved"}
	}
	cfg := defaultCfg()
	b := voteBot(t, tg, fa, verify, cfg)
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 32, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 32, 43, "加V 下注"))

	if len(tg.bans) != 0 {
		t.Fatalf("unresolved vote must not punish: %v", tg.bans)
	}
	if rq.Len() != 1 {
		t.Fatalf("unresolved vote must re-enqueue: %d", rq.Len())
	}
}

func TestVoteAllAbstainUnresolved(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{VotesTotal: 3, Abstentions: 3, Final: "unresolved"}
	}
	cfg := defaultCfg()
	b := voteBot(t, tg, fa, verify, cfg)
	rq := retryq.New(b.RetryRound, 3600*1e9, cfg.RetryMax, 500,
		logx.New(nil, logx.Error, "text", "retryq"))
	b.RetryQ = rq
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 33, ChatID: -400})
	b.HandleMessage(ctx, adMsg(-400, 33, 44, "加V 下注"))
	if len(tg.bans) != 0 || rq.Len() != 1 {
		t.Fatalf("all-abstain must be unresolved + enqueued: bans=%v len=%d", tg.bans, rq.Len())
	}
}

func TestHighConfidenceNoVote(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		t.Fatal("high confidence must not vote")
		return ai.VerifyResult{}
	}
	b := voteBot(t, tg, fa, verify, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 34, ChatID: -400})
	b.HandleMessage(ctx, adMsg(-400, 34, 45, "加V 下注"))
	if len(tg.bans) != 1 {
		t.Fatalf("high confidence ad must punish directly: %v", tg.bans)
	}
}

// --- Retry → local rule fallback (A.5/A.6) -------------------------------------

func TestRetryExhaustedLocalFallbackPunishes(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	cfg := defaultCfg()
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 40, ChatID: -500})
	b.HandleMessage(ctx, adMsg(-500, 40, 50, "专业办证 加V信 kfw123"))

	// The queue carried the item; drive it to exhaustion by invoking
	// RetryRound max times like the worker does.
	item := retryq.Item{Key: "msg:-500:50", Kind: "message", ChatID: -500, MessageID: 50, UID: 40,
		Text: "专业办证 加V信 kfw123", Mode: "message", EnqueuedAt: time.Now()}
	for round := 0; round < cfg.RetryMax; round++ {
		ok, code := b.RetryRound(ctx, item)
		item.Attempts = round + 1
		item.LastErrCode = code
		if ok && code == "local_fallback" {
			// The last round ran the local rule fallback → punished.
			if len(tg.bans) != 1 {
				t.Fatalf("local fallback must punish on rule hit: %v", tg.bans)
			}
			return
		}
	}
	t.Fatalf("local fallback never fired after %d rounds", cfg.RetryMax)
}

func TestRetryExhaustedLocalFallbackPassesBelowThreshold(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	cfg := defaultCfg()
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 41, ChatID: -500})
	b.HandleMessage(ctx, adMsg(-500, 41, 51, "我加你了"))

	item := retryq.Item{Key: "msg:-500:51", Kind: "message", ChatID: -500, MessageID: 51, UID: 41,
		Text: "我加你了", Mode: "message", EnqueuedAt: time.Now()}
	for round := 0; round < cfg.RetryMax; round++ {
		ok, code := b.RetryRound(ctx, item)
		item.Attempts = round + 1
		item.LastErrCode = code
		if ok && code == "local_fallback" {
			// Below threshold → no punishment, but amend5 D: the judgment is
			// complete, so the watchlist slot is consumed.
			if len(tg.bans) != 0 {
				t.Fatalf("below-threshold text must not punish: %v", tg.bans)
			}
			if sus, _ := b.Store.IsSuspect(ctx, 41); sus {
				t.Fatal("below-threshold fallback must consume the watchlist slot")
			}
			localFB, _ := b.ExtraStats()
			if localFB == 0 {
				t.Fatal("local fallback count must increment")
			}
			return
		}
	}
	t.Fatalf("local fallback never fired")
}

func TestLocalFallbackNegativeSamples(t *testing.T) {
	// The amend2 §6.1 negative list must never be punished by local rules.
	for _, text := range []string{"我加你了", "发票报销流程问下行政", "多少钱"} {
		tg := newFakeTG()
		fa := &fakeAI{fail: true}
		cfg := defaultCfg()
		b := testBot(t, tg, fa, cfg)
		ctx := context.Background()
		b.Store.AddSuspect(ctx, store.Suspect{UID: 42, ChatID: -500})
		b.HandleMessage(ctx, adMsg(-500, 42, 52, text))
		item := retryq.Item{Key: "msg:-500:52", Kind: "message", ChatID: -500, MessageID: 52, UID: 42,
			Text: text, Mode: "message", EnqueuedAt: time.Now(), Attempts: cfg.RetryMax - 1}
		ok, code := b.RetryRound(ctx, item)
		if !ok || code != "local_fallback" {
			t.Fatalf("last round must run local fallback: ok=%v code=%q", ok, code)
		}
		if len(tg.bans) != 0 {
			t.Errorf("negative sample %q must not punish: %v", text, tg.bans)
		}
	}
}

// --- Single delete: 429 retry / 400 warn-only / 403 (A.7) ----------------------

func TestDelete429RetriesThenSucceeds(t *testing.T) {
	tg := newFakeTG()
	tg.deleteScript = []error{
		&telegram.APIError{Code: 429, Description: "Too Many Requests", RetryAfter: 1},
		nil,
	}
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 50, ChatID: -600})
	b.HandleMessage(ctx, adMsg(-600, 50, 60, "加V 下注"))
	if len(tg.bans) != 1 {
		t.Fatalf("ban must proceed after a retried delete: %v", tg.bans)
	}
	if len(tg.deletes) != 1 || tg.deletes[0] != "60" {
		t.Fatalf("delete eventually succeeded: %v", tg.deletes)
	}
}

func TestDelete429ExhaustsAfterThreeRetries(t *testing.T) {
	tg := newFakeTG()
	e := &telegram.APIError{Code: 429, Description: "Too Many Requests", RetryAfter: 1}
	tg.deleteScript = []error{e, e, e, e, e}
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 51, ChatID: -600})
	b.HandleMessage(ctx, adMsg(-600, 51, 61, "加V 下注"))
	// ≤3 retries → the fourth failure gives up; the ban still proceeds.
	if len(tg.bans) != 1 {
		t.Fatalf("delete failure must not block the ban: %v", tg.bans)
	}
	if tg.deleteCalls > 4 {
		t.Fatalf("delete attempts = %d, want ≤4 (1 + 3 retries)", tg.deleteCalls)
	}
	_, deleteFails := b.DeleteStats()
	if deleteFails == 0 {
		t.Error("delete failure must be counted")
	}
}

func TestDelete400WarnOnly(t *testing.T) {
	tg := newFakeTG()
	tg.deleteScript = []error{
		&telegram.APIError{Code: 400, Description: "Bad Request: message to delete not found"},
	}
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 52, ChatID: -600})
	b.HandleMessage(ctx, adMsg(-600, 52, 62, "加V 下注"))
	// 400 "can't delete" → warn and continue (ban + signature still happen).
	if len(tg.bans) != 1 {
		t.Fatalf("400 delete must not block the flow: %v", tg.bans)
	}
	if tg.deleteCalls != 1 {
		t.Fatalf("400 must not retry: calls=%d", tg.deleteCalls)
	}
	if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature("加V 下注")); !ok {
		t.Fatal("signature write must continue after a 400 delete")
	}
}

func TestDelete403LoggedNotRetried(t *testing.T) {
	tg := newFakeTG()
	tg.deleteScript = []error{
		&telegram.APIError{Code: 403, Description: "Forbidden: bot is not an admin"},
	}
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 53, ChatID: -600})
	b.HandleMessage(ctx, adMsg(-600, 53, 63, "加V 下注"))
	if len(tg.bans) != 1 {
		t.Fatalf("403 delete must not block the ban: %v", tg.bans)
	}
	if tg.deleteCalls != 1 {
		t.Fatalf("403 must not retry: calls=%d", tg.deleteCalls)
	}
}

// --- Whitelist / auto-leave ----------------------------------------------------

func TestMyChatMemberLeaveForeignChat(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LeaveOtherChats = true
	cfg.LeaveNotice = true
	b := testBot(t, tg, &fakeAI{}, cfg)
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
	if !strings.Contains(strings.Join(tg.seq, ","), "send,leave") {
		t.Fatalf("expected send (notice) before leave: %v", tg.seq)
	}
}

func TestMyChatMemberLeaveWithoutNotice(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LeaveOtherChats = true
	cfg.LeaveNotice = false
	b := testBot(t, tg, &fakeAI{}, cfg)
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
	b := testBot(t, tg, &fakeAI{}, cfg)
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
	fa := &fakeAI{verdict: fakeVerdict(true, "办证", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.HandleMessage(ctx, adMsg(-777, 1, 99, "广告 加V"))
	if fa.callCount() != 0 {
		t.Fatalf("non-whitelisted message must be zero AI: %d", fa.callCount())
	}
	if n, _ := b.Store.SuspectCount(ctx); n != 0 {
		t.Fatalf("non-whitelisted message must be zero writes: %d", n)
	}
	if len(tg.bans) != 0 {
		t.Fatalf("must not punish: %v", tg.bans)
	}
}

// --- Commands -------------------------------------------------------------------

func TestCommandSkippedFromAdJudgment(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "办证", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 5, ChatID: -500})
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 5, 1, "/start")))
	if fa.callCount() != 0 {
		t.Fatalf("command must not trigger AI: %d", fa.callCount())
	}
}

func TestStartCommandReplies(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, &fakeAI{}, defaultCfg())
	b.HandleUpdate(context.Background(), cmdUpdate(adMsg(-500, 5, 1, "/start")))
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "🟢 在线") {
		t.Fatalf("start reply = %v", tg.sends)
	}
}

func TestStatusAdminOnly(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, &fakeAI{}, defaultCfg())
	b.HandleUpdate(context.Background(), cmdUpdate(adMsg(-500, 5, 1, "/status")))
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "仅管理员") {
		t.Fatalf("non-admin /status = %v", tg.sends)
	}
}

func TestStatusAdminUserAllowedShowsExtendedStats(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.AdminUsers = map[int64]bool{42: true}
	b := testBot(t, tg, &fakeAI{}, cfg)
	b.HandleUpdate(context.Background(), cmdUpdate(adMsg(-500, 42, 1, "/status")))
	if len(tg.sends) != 1 {
		t.Fatalf("admin user /status = %v", tg.sends)
	}
	// Amend3 /status extensions: prompt version, model chain, queue
	// (/report and its 举报 counter were removed in the amend5 round).
	s := tg.sends[0]
	for _, want := range []string{"提示词", "模型链", "待决队列", ai.PromptVersion} {
		if !strings.Contains(s, want) {
			t.Errorf("/status missing %q", want)
		}
	}
}

func TestCheckRunsAIOnlyNoDisposal(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.AdminUsers = map[int64]bool{42: true}
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.9)}
	b := testBot(t, tg, fa, cfg)
	b.HandleUpdate(context.Background(), cmdUpdate(adMsg(-500, 42, 1, "/check 广告")))
	if len(tg.bans) != 0 {
		t.Fatalf("/check must not dispose: %v", tg.bans)
	}
	if fa.callCount() != 1 {
		t.Fatalf("/check should run exactly one AI call: %d", fa.callCount())
	}
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "is_ad") {
		t.Fatalf("/check reply = %v", tg.sends)
	}
	if !strings.Contains(tg.sends[0], ai.PromptVersion) {
		t.Errorf("/check must report the prompt version")
	}
}

func TestDryRunZeroWrites(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.DryRun = true
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})
	b.HandleMessage(ctx, adMsg(-600, 6, 2, "加V 信"))
	// Zero Telegram mutations.
	if len(tg.bans) != 0 || len(tg.deletes) != 0 || len(tg.sends) != 0 {
		t.Fatalf("dry-run must perform zero telegram writes: bans=%v deletes=%v sends=%v",
			tg.bans, tg.deletes, tg.sends)
	}
	// Zero store mutations: no signature write, no punishment record.
	if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature("加V 信")); ok {
		t.Fatal("dry-run must not write signatures")
	}
	if n, _ := b.Store.Punished(ctx); n != 0 {
		t.Fatal("dry-run must not record punishments")
	}
	// The suspect is NOT removed (no disposal happened).
	if ok, _ := b.Store.IsSuspect(ctx, 6); !ok {
		t.Fatal("dry-run must keep the suspect on the watchlist")
	}
}

// --- Panic isolation (B) ---------------------------------------------------------

func TestPanicInUpdateIsolated(t *testing.T) {
	tg := newFakeTG()
	b := testBot(t, tg, &fakeAI{}, defaultCfg())
	// A nil From in a command message panics inside handleUpdate
	// (m.From.ID); the recover guard must swallow it.
	b.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1,
		Message:  &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: -500, Type: "supergroup"}, Text: "/start"},
	})
	// The process survived and a later update still flows.
	b.HandleUpdate(context.Background(), cmdUpdate(adMsg(-500, 5, 3, "/start")))
	if len(tg.sends) == 0 {
		t.Fatal("bot must keep processing updates after a panic")
	}
}

// --- edited_message re-screening (B) ---------------------------------------------

// TestEditedMessageAfterConsumptionNoAI: amend5 D — the first (not-ad)
// judgment consumed the one-shot watchlist slot, so the edited ad version no
// longer reaches the AI and is not punished.
func TestEditedMessageAfterConsumptionNoAI(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)} // first version: not ad
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 44, ChatID: -800})

	// First version: normal → judgment completes → slot consumed.
	b.HandleUpdate(ctx, telegram.Update{UpdateID: 1, Message: ptr(adMsg(-800, 44, 80, "正常"))})
	if len(tg.bans) != 0 {
		t.Fatalf("normal first message: %v", tg.bans)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 44); sus {
		t.Fatal("first not-ad judgment must consume the watchlist slot")
	}
	if fa.callCount() != 1 {
		t.Fatalf("first message must be judged exactly once: %d", fa.callCount())
	}

	// The user edits it into an ad → no AI call, no punishment (the user is no
	// longer on the watchlist; only a signature hit could still dispose).
	fa.mu.Lock()
	fa.verdict = fakeVerdict(true, "诈骗", 0.95)
	fa.mu.Unlock()
	b.HandleUpdate(ctx, telegram.Update{UpdateID: 2, EditedMessage: ptr(adMsg(-800, 44, 80, "高回报理财 加V:999"))})
	if fa.callCount() != 1 {
		t.Fatalf("edited message after slot consumption must not hit AI: %d", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("edited message after slot consumption must not punish: %v", tg.bans)
	}
}

// TestEditedMessageSignatureStillDisposes: after slot consumption, signature
// matching remains for ALL users — an edit matching a known ad signature is
// still disposed without any AI call.
func TestEditedMessageSignatureStillDisposes(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 45, ChatID: -800})

	// First version: normal → slot consumed.
	b.HandleUpdate(ctx, telegram.Update{UpdateID: 1, Message: ptr(adMsg(-800, 45, 81, "正常"))})
	if sus, _ := b.Store.IsSuspect(ctx, 45); sus {
		t.Fatal("first not-ad judgment must consume the watchlist slot")
	}

	// Seed a message signature for the ad text; the edited version matches it.
	const adText = "高回报理财 加V:999"
	norm := normalize.Normalize(adText)
	if err := b.Store.AddMessageSignature(ctx, normalize.SignatureOf(norm), norm); err != nil {
		t.Fatal(err)
	}
	b.HandleUpdate(ctx, telegram.Update{UpdateID: 2, EditedMessage: ptr(adMsg(-800, 45, 82, adText))})
	if fa.callCount() != 1 {
		t.Fatalf("signature hit must not call AI: %d", fa.callCount())
	}
	if len(tg.bans) != 1 {
		t.Fatalf("signature hit after slot consumption must dispose: %v", tg.bans)
	}
}

func ptr[T any](v T) *T { return &v }

// cmdUpdate wraps a command message as an update (named for the command flow,
// not the removed /report feature).
func cmdUpdate(m telegram.Message) telegram.Update {
	return telegram.Update{UpdateID: m.MessageID, Message: &m}
}

// --- Upstream sensitive disposal chain (amend5 A, bot level) ------------------
// A httptest gateway returns the 400 sensitive envelope; the bot runs the FULL
// disposal chain on the synthesized verdict (delete → ban → signature write →
// notify with the · 上游敏感拦截 suffix), and a repeat of the same text hits the
// signature library with zero AI calls.

// sensitiveGatewayAI is a bot.AIClassifier backed by a real ai.Client pointed
// at a httptest server that answers the 400 sensitive envelope. HTTP request
// counting proves "no next model / no retry" at the transport level.
type sensitiveGatewayAI struct {
	client *ai.Client
	mu     sync.Mutex
	reqs   int
}

func (s *sensitiveGatewayAI) Classify(ctx context.Context, content ai.Content, mode string) (ai.ClassifyResult, bool) {
	s.mu.Lock()
	s.reqs++
	s.mu.Unlock()
	return s.client.ClassifyText(ctx, content.Text, mode)
}

func (s *sensitiveGatewayAI) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs
}

func TestUpstreamSensitiveFullDisposalChain(t *testing.T) {
	var httpReqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&httpReqs, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":%s,"code":"content_filter"}}`,
			`"content filtered by upstream (sensitive words detected)"`)
	}))
	defer srv.Close()

	sg := &sensitiveGatewayAI{client: ai.NewClient(ai.ClientConfig{
		BaseURL: srv.URL, APIKey: "k", Models: []string{"m1", "m2", "m2b"}, Timeout: 2 * time.Second,
	})}
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LogChat = 999
	b := testBot(t, tg, sg, cfg)
	rec := wireTestNotifier(t, b, cfg)
	ctx := context.Background()
	const text = "跑分 日结 加V接单"
	b.Store.AddSuspect(ctx, store.Suspect{UID: 60, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 60, 61, text))

	if len(tg.bans) != 1 || tg.bans[0] != "ban" {
		t.Fatalf("sensitive kill must ban: %v", tg.bans)
	}
	if len(tg.deletes) != 1 || tg.deletes[0] != "61" {
		t.Fatalf("exactly the triggering message must be deleted: %v", tg.deletes)
	}
	if got := atomic.LoadInt32(&httpReqs); got != 1 {
		t.Fatalf("exactly 1 HTTP request (chain terminated, no retry): %d", got)
	}
	if sg.requestCount() != 1 {
		t.Fatalf("exactly 1 classifier call: %d", sg.requestCount())
	}
	// Signature written → the same text again is a signature hit (no AI).
	norm := normalize.Normalize(text)
	sig := normalize.SignatureOf(norm)
	if hit, _ := b.Store.HasMessageSignature(ctx, sig); !hit {
		t.Fatal("sensitive kill must write the message signature")
	}
	b.HandleMessage(ctx, adMsg(-500, 61, 62, text))
	if sg.requestCount() != 1 {
		t.Fatalf("signature hit must not call AI: %d", sg.requestCount())
	}
	if len(tg.bans) != 2 {
		t.Fatalf("signature hit must dispose: %v", tg.bans)
	}
	// Notify: the disposition notice must end its last line with the suffix.
	waitFor(t, func() bool { return len(rec.texts()) >= 1 }, "disposition notification")
	if n := len(rec.texts()); n < 1 {
		t.Fatalf("notifications = %d", n)
	}
	last := rec.texts()[len(rec.texts())-1]
	if !strings.Contains(last, "上游敏感拦截") {
		t.Fatalf("notify text must mention 上游敏感拦截: %q", last)
	}
	if !strings.Contains(last, "· 上游敏感拦截") {
		t.Fatalf("notify last line must carry the suffix: %q", last)
	}
}

// --- One judgment = one consumption (amend5 D) ---------------------------------

// TestOneShotFirstNotAdRemovesAndSkipsAI: the first not-ad judgment removes
// the user; their SECOND message triggers no AI call (request-count assertion).
func TestOneShotFirstNotAdRemovesAndSkipsAI(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 61, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 61, 62, "今天天气不错"))
	if sus, _ := b.Store.IsSuspect(ctx, 61); sus {
		t.Fatal("first not-ad judgment must remove the user from the watchlist")
	}
	if fa.callCount() != 1 {
		t.Fatalf("first message must be judged once: %d", fa.callCount())
	}

	// Second message: user no longer on the watchlist → no AI call.
	b.HandleMessage(ctx, adMsg(-500, 61, 63, "又发了一条普通消息"))
	if fa.callCount() != 1 {
		t.Fatalf("second message after consumption must not hit AI: %d", fa.callCount())
	}
}

// TestOneShotFirstIsAdStillDisposes: regression — an ad first message disposes
// and removes (the pre-amend5 behavior is unchanged).
func TestOneShotFirstIsAdStillDisposes(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 62, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 62, 64, "高回报理财 加V:999"))
	if len(tg.bans) != 1 {
		t.Fatalf("ad first message must dispose: %v", tg.bans)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 62); sus {
		t.Fatal("punished ad user must be removed from the watchlist")
	}
}

// TestOneShotVoteNotAdRemoves: vote-flip to not_ad also consumes the slot.
func TestOneShotVoteNotAdRemoves(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)} // low confidence → vote
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{VotesTotal: 3, VotesNo: 3, Final: "not_ad"}
	}
	b := voteBot(t, tg, fa, verify, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 63, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 63, 65, "聊聊而已"))
	if len(tg.bans) != 0 {
		t.Fatalf("vote not_ad must not punish: %v", tg.bans)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 63); sus {
		t.Fatal("vote not_ad must consume the watchlist slot")
	}

	// A later message from the same user: no AI (slot consumed).
	b.HandleMessage(ctx, adMsg(-500, 63, 66, "再来一句"))
	if fa.callCount() != 1 {
		t.Fatalf("post-consumption message must not hit AI: %d", fa.callCount())
	}
}

// TestOneShotVoteUnresolvedKeepsWatching: an unresolved vote re-enqueues and
// keeps the user on the watchlist.
func TestOneShotVoteUnresolvedKeepsWatching(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{VotesTotal: 3, VotesAd: 1, VotesNo: 1, Abstentions: 1, Final: "unresolved"}
	}
	b := voteBot(t, tg, fa, verify, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 64, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 64, 67, "待定内容"))
	if len(tg.bans) != 0 {
		t.Fatalf("unresolved vote must not punish: %v", tg.bans)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 64); !sus {
		t.Fatal("unresolved vote must keep the user on the watchlist")
	}
}

// TestOneShotEmojiFirstDoesNotConsume: a pure emoji/sticker first message is
// not judgeable — no AI, no consumption — so a follow-up text IS judged.
func TestOneShotEmojiFirstDoesNotConsume(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 65, ChatID: -500})

	// Pure emoji: text is non-blank so it passes the service-message filter,
	// but the emoji-only rule must keep it un-judged and un-consumed.
	b.HandleMessage(ctx, adMsg(-500, 65, 68, "😊😊😊"))
	if fa.callCount() != 0 {
		t.Fatalf("emoji-only first message must not hit AI: %d", fa.callCount())
	}
	if sus, _ := b.Store.IsSuspect(ctx, 65); !sus {
		t.Fatal("emoji-only message must not consume the watchlist slot")
	}

	// The next real text message is judged normally (slot still available).
	b.HandleMessage(ctx, adMsg(-500, 65, 69, "正常发言"))
	if fa.callCount() != 1 {
		t.Fatalf("follow-up text must be judged: %d", fa.callCount())
	}
}

// TestOneShotJoinBioDoesNotConsume: a join-bio not-ad judgment leaves the
// first-message opportunity intact (D.2: bio is an independent screen).
func TestOneShotJoinBioDoesNotConsume(t *testing.T) {
	tg := newFakeTG()
	tg.chat[65] = telegram.ChatInfo{ID: 65, Bio: "爱读书爱旅行"}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.HandleNewMembers(ctx, -500, []telegram.User{u(65, false, "reader", "Reader")}, 0)
	if sus, _ := b.Store.IsSuspect(ctx, 65); !sus {
		t.Fatal("join-bio not-ad must keep the watchlist entry")
	}
	if fa.callCount() != 1 {
		t.Fatalf("bio must be judged once: %d", fa.callCount())
	}

	// First message afterwards is still judged (opportunity not consumed).
	b.HandleMessage(ctx, adMsg(-500, 65, 70, "大家好"))
	if fa.callCount() != 2 {
		t.Fatalf("first message after join must still be judged: %d", fa.callCount())
	}
	if sus, _ := b.Store.IsSuspect(ctx, 65); sus {
		t.Fatal("first message not-ad must consume the slot")
	}
}

// TestOneShotSignatureStillScreensEveryone: after slot consumption, signature
// matching remains for ALL users (second ad message = signature hit).
func TestOneShotSignatureStillScreensEveryone(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 66, ChatID: -500})

	// First message not-ad → slot consumed.
	b.HandleMessage(ctx, adMsg(-500, 66, 71, "正常"))
	if fa.callCount() != 1 {
		t.Fatalf("first message must be judged once: %d", fa.callCount())
	}

	// Seed the ad text into the message signature library.
	const adText = "兼职日结 加V:123"
	norm := normalize.Normalize(adText)
	if err := b.Store.AddMessageSignature(ctx, normalize.SignatureOf(norm), norm); err != nil {
		t.Fatal(err)
	}
	b.HandleMessage(ctx, adMsg(-500, 66, 72, adText))
	if fa.callCount() != 1 {
		t.Fatalf("signature hit must not call AI (post-consumption): %d", fa.callCount())
	}
	if len(tg.bans) != 1 {
		t.Fatalf("signature hit must still dispose: %v", tg.bans)
	}
}

// TestOneShotLocalFallbackBelowThresholdConsumes: a first message whose
// judgment falls to the local fallback below threshold consumes the slot.
func TestOneShotLocalFallbackBelowThresholdConsumes(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	b := testBot(t, tg, fa, defaultCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 67, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 67, 73, "随便聊聊"))
	cfg := defaultCfg()
	// Jump straight to the last round (the queue's exhaustion round).
	item := retryq.Item{Key: "msg:-500:73", Kind: "message", ChatID: -500, MessageID: 73, UID: 67,
		Text: "随便聊聊", Mode: "message", EnqueuedAt: time.Now(), Attempts: cfg.RetryMax - 1}
	ok, code := b.RetryRound(ctx, item)
	if !ok || code != "local_fallback" {
		t.Fatalf("last round must run local fallback: ok=%v code=%q", ok, code)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 67); sus {
		t.Fatal("below-threshold local fallback must consume the watchlist slot")
	}

	// A later message triggers no AI (slot consumed).
	b.HandleMessage(ctx, adMsg(-500, 67, 74, "再来一句"))
	if fa.callCount() != 2 {
		t.Fatalf("post-consumption message must not hit AI: %d", fa.callCount())
	}
}

// --- /report removal (amend5 B, E.6) -------------------------------------------

// TestReportCommandIsGone: sending the removed /report command (with a reply,
// the old entry shape) triggers NO AI call, NO notification, NO stats, and no
// error reply — the dispatch simply does not exist anymore.
func TestReportCommandIsGone(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg := defaultCfg()
	cfg.LogChat = 999
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	ctx := context.Background()

	// Old entry shape: /report as a reply to an ad message, sent by an
	// admin-listed user (so it cannot bounce off the admin gate reply).
	m := adMsg(-500, 42, 80, "/report")
	m.ReplyToMessage = ptr(adMsg(-500, 60, 81, "高回报理财 加V:999"))
	b.Cfg.AdminUsers = map[int64]bool{42: true}
	b.HandleUpdate(ctx, cmdUpdate(m))

	if fa.callCount() != 0 {
		t.Fatalf("/report must not trigger AI: %d", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("/report must not dispose: %v", tg.bans)
	}
	if len(tg.sends) != 0 {
		t.Fatalf("/report must not send a reply: %v", tg.sends)
	}
	// No notification of any kind (punish or error).
	if texts := rec.texts(); len(texts) != 0 {
		t.Fatalf("/report must not notify: %v", texts)
	}
	// No stats movement: local fallback / simhash counters stay zero.
	localFB, simHits := b.ExtraStats()
	if localFB != 0 || simHits != 0 {
		t.Fatalf("/report must not move stats: local=%d simhash=%d", localFB, simHits)
	}
	// The replied-to user stays untracked (no report record was created).
	if n, _ := b.Store.SuspectCount(ctx); n != 0 {
		t.Fatalf("/report must not create watchlist entries: %d", n)
	}
}
