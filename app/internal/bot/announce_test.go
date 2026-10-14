package bot

// amend7 announce subsystem tests: everything offline — the AI generator is
// a fake (or an httptest-backed real ai.Client in the full-chain test), the
// telegram client is fakeTG. The 10 assertions of TASK_antispam_amend7.md F:
//  1. AI ok → one group message containing the kicked name;
//  2. AI fail/timeout/non-compliant → template pool AND the disposal
//     completed FIRST (sequence assertion);
//  3. generated text without the name → name auto-prefixed;
//  4. > 80 runes / multiline → one line + truncated;
//  5. foreign @mention / URL / phone → template fallback;
//  6. dry-run → nothing sent;
//  7. same disposal twice → exactly one announcement;
//  8. BOT_ANNOUNCE_KICK=false → nothing;
//  9. BOT_ANNOUNCE_TTL>0 → deleted after expiry;
// 10. slow AI (blocking) → the punishment chain latency < 100ms.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"adbot/internal/ai"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// fakeAnnouncer implements AnnounceGenerator with injectable behavior.
type fakeAnnouncer struct {
	mu      sync.Mutex
	calls   int
	subject string
	delay   time.Duration
	out     string
	err     error
}

func (f *fakeAnnouncer) Announce(ctx context.Context, subject, category string) (string, error) {
	f.mu.Lock()
	f.calls++
	f.subject = subject
	out, err, delay := f.out, f.err, f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

func (f *fakeAnnouncer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// announceBot wires a bot with the Announcer attached to the fake TG.
func announceBot(t *testing.T, tg *fakeTG, gen AnnounceGenerator, kickEnabled bool, ttl time.Duration) (*Bot, *fakeAnnouncer) {
	t.Helper()
	cfg := defaultCfg()
	cfg.Action = "ban"
	b := testBot(t, tg, &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}, cfg)
	fa, _ := gen.(*fakeAnnouncer)
	b.Announcer = NewAnnouncer(tg, gen, kickEnabled, 2*time.Second, ttl, 2*time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil)
	return b, fa
}

// runKickDisposal drives one full AI-ad message through the bot and waits
// for the announcement goroutine to finish (disposal completes synchronously
// inline in tests — the pool is not started).
func runKickDisposal(t *testing.T, b *Bot, tg *fakeTG, uid, msgID int64, name string) {
	t.Helper()
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -600})
	b.HandleMessage(ctx, adMsgWithFrom(-600, uid, msgID, "加V赚钱", name))
	if tg.sendCount() == 0 && len(tg.banList()) == 0 {
		t.Fatalf("disposal must happen: bans=%v", tg.banList())
	}
	waitFor(t, func() bool { return tg.sendCount() > 0 || announceFinished(b) }, "announcement goroutine")
}

// sendCount / sendsSnapshot / banList / deleteList are lock-guarded readers
// over fakeTG's record slices (the announce goroutine writes concurrently).
func (f *fakeTG) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeTG) sendsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...)
}

func (f *fakeTG) banList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bans...)
}

func (f *fakeTG) deleteList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

// adMsgWithFrom builds an ad message with a first name set.
func adMsgWithFrom(chatID, uid, msgID int64, text, firstName string) telegram.Message {
	m := adMsg(chatID, uid, msgID, text)
	m.From.FirstName = firstName
	return m
}

// announceFinished reports whether the announcer processed everything
// submitted so far (best-effort: fallback path also counts via error stats).
func announceFinished(b *Bot) bool {
	_, _, fallback, errs := b.AnnounceStats()
	return fallback > 0 || errs > 0
}

// --- F.1: AI success → one group message with the kicked name ----------------

func TestAnnounceAIOneGroupMessage(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 spammer 已送出门外，广告请去别处发～"}
	b, _ := announceBot(t, tg, gen, true, 0)
	runKickDisposal(t, b, tg, 70, 90, "Spammer")

	sends := tg.sendsSnapshot()
	if len(sends) != 1 {
		t.Fatalf("exactly one announcement expected, sends = %v", sends)
	}
	if !strings.Contains(sends[0], "Spammer") {
		t.Fatalf("announcement must contain the kicked name: %q", sends[0])
	}
	if !strings.Contains(sends[0], "🚪") {
		t.Fatalf("AI text must be used verbatim: %q", sends[0])
	}
	enabled, sent, fallback, errs := b.AnnounceStats()
	if !enabled || sent != 1 || fallback != 0 || errs != 0 {
		t.Fatalf("stats = (%v,%d,%d,%d), want (true,1,0,0)", enabled, sent, fallback, errs)
	}
}

// --- F.2: AI failure → template; disposal completes FIRST ---------------------

func TestAnnounceAIFailureTemplateAndDisposalFirst(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{err: errors.New("timeout")}
	b, _ := announceBot(t, tg, gen, true, 0)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 71, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 71, 91, "加V赚钱", "Bad"))

	// Disposal completed synchronously BEFORE any announcement could exist.
	if len(tg.banList()) != 1 {
		t.Fatalf("disposal must complete first: bans=%v", tg.banList())
	}
	// Fallback announcement arrives (template), with the name in it.
	waitFor(t, func() bool { return tg.sendCount() == 1 }, "template announcement")
	sends := tg.sendsSnapshot()
	if !strings.Contains(sends[0], "Bad") {
		t.Fatalf("template must carry the name: %q", sends[0])
	}
	if isFromTemplates(sends[0], "Bad") == "" {
		t.Fatalf("output is not one of the fallback templates: %q", sends[0])
	}
	_, sent, fallback, _ := b.AnnounceStats()
	if sent != 1 || fallback != 1 {
		t.Fatalf("stats sent=%d fallback=%d, want 1/1", sent, fallback)
	}
}

// isFromTemplates returns the matching template if text is a filled template.
func isFromTemplates(text, subject string) string {
	for _, tpl := range announceTemplates {
		if text == fmt.Sprintf(tpl, subject) {
			return tpl
		}
	}
	return ""
}

// --- F.3: generated text missing the name → auto-prefix ----------------------

func TestAnnounceMissingSubjectPrefixed(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "已被送出门外，广告请去别处发～"}
	b, _ := announceBot(t, tg, gen, true, 0)
	runKickDisposal(t, b, tg, 72, 92, "Missing")
	sends := tg.sendsSnapshot()
	if len(sends) != 1 || !strings.HasPrefix(sends[0], "Missing ") {
		t.Fatalf("subject must be auto-prefixed: %q", sends)
	}
}

// --- F.4: multiline / > 80 runes → collapsed + truncated ---------------------

func TestAnnounceMultilineAndOverlongCollapsed(t *testing.T) {
	tg := newFakeTG()
	long := "Long 一二三"
	gen := &fakeAnnouncer{out: long + strings.Repeat("字", 100) + "\n第二行文字"}
	b, _ := announceBot(t, tg, gen, true, 0)
	runKickDisposal(t, b, tg, 73, 93, "Long")
	sends := tg.sendsSnapshot()
	if len(sends) != 1 {
		t.Fatalf("one announcement expected: %v", sends)
	}
	out := sends[0]
	if strings.Contains(out, "\n") {
		t.Fatalf("multiline must be collapsed: %q", out)
	}
	if n := len([]rune(out)); n != 80 {
		t.Fatalf("length = %d runes, want 80: %q", n, out)
	}
	if !strings.Contains(out, "Long") {
		t.Fatalf("truncated output must keep the name: %q", out)
	}
}

// --- F.5: URL / foreign mention / phone → template fallback ------------------

func TestAnnounceNoncompliantGoesTemplate(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"url", "Bad1 被踢，详见 https://t.me/joinchat"},
		{"mention", "Bad1 被踢，加 @contact_me"},
		{"phone", "Bad1 被踢，联系 13800138000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg := newFakeTG()
			gen := &fakeAnnouncer{out: tc.out}
			b, _ := announceBot(t, tg, gen, true, 0)
			runKickDisposal(t, b, tg, 74, 94, "Bad1")
			sends := tg.sendsSnapshot()
			if len(sends) != 1 {
				t.Fatalf("template fallback must send: %v", sends)
			}
			if got := isFromTemplates(sends[0], "Bad1"); got == "" {
				t.Fatalf("non-compliant AI output must be replaced by a template: %q", sends[0])
			}
			_, _, fallback, _ := b.AnnounceStats()
			if fallback != 1 {
				t.Fatalf("fallback counter = %d, want 1", fallback)
			}
		})
	}
}

// --- F.6: dry-run → nothing sent ---------------------------------------------

func TestAnnounceDryRunSendsNothing(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 Dry 已送出门外"}
	cfg := defaultCfg()
	cfg.DryRun = true
	b := testBot(t, tg, &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}, cfg)
	b.Announcer = NewAnnouncer(tg, gen, true, 2*time.Second, 0, 2*time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 75, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 75, 95, "加V赚钱", "Dry"))

	// Dry-run: zero telegram mutations of any kind (announce included).
	time.Sleep(100 * time.Millisecond)
	if tg.sendCount() != 0 {
		t.Fatalf("dry-run must not send announcements: %v", tg.sendsSnapshot())
	}
	if len(tg.banList()) != 0 || len(tg.deleteList()) != 0 {
		t.Fatalf("dry-run must perform zero writes: bans=%v deletes=%v", tg.banList(), tg.deleteList())
	}
	_, sent, _, _ := b.AnnounceStats()
	if sent != 0 {
		t.Fatalf("dry-run must not count sends: %d", sent)
	}
	if gen.callCount() != 0 {
		t.Fatalf("dry-run must not even generate: %d", gen.callCount())
	}
}

// --- F.7: same disposal twice → exactly one announcement ---------------------

func TestAnnounceIdempotentPerDisposal(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 Twice 已送出门外"}
	b, _ := announceBot(t, tg, gen, true, 0)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 76, ChatID: -600})

	// First disposal.
	b.HandleMessage(ctx, adMsgWithFrom(-600, 76, 96, "加V赚钱", "Twice"))
	waitFor(t, func() bool { return tg.sendCount() == 1 }, "first announcement")

	// The SAME disposal retriggers (e.g. a second path reporting the same
	// chat+uid+messageID): still exactly one announcement.
	b.Announcer.Announce(ctx, -600, 76, 96, "Twice", "Twice", "诈骗", false)
	time.Sleep(100 * time.Millisecond)
	if tg.sendCount() != 1 {
		t.Fatalf("duplicate disposal must not re-announce: %v", tg.sendsSnapshot())
	}
	if gen.callCount() != 1 {
		t.Fatalf("generator must run once, got %d", gen.callCount())
	}
}

// --- F.8: BOT_ANNOUNCE_KICK=false → nothing ----------------------------------

func TestAnnounceDisabledSwitch(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 Off 已送出门外"}
	b, _ := announceBot(t, tg, gen, false, 0)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 77, ChatID: -600})
	b.HandleMessage(ctx, adMsgWithFrom(-600, 77, 97, "加V赚钱", "Off"))
	if len(tg.banList()) != 1 {
		t.Fatalf("disposal still happens: %v", tg.banList())
	}
	time.Sleep(100 * time.Millisecond)
	if tg.sendCount() != 0 {
		t.Fatalf("BOT_ANNOUNCE_KICK=false must send nothing: %v", tg.sendsSnapshot())
	}
	if gen.callCount() != 0 {
		t.Fatalf("generator must not run when disabled: %d", gen.callCount())
	}
	enabled, _, _, _ := b.AnnounceStats()
	if enabled {
		t.Fatal("stats must report disabled")
	}
}

// --- F.9: BOT_ANNOUNCE_TTL>0 → deleted after expiry --------------------------

func TestAnnounceTTLDeletesMessage(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 TTL 已送出门外"}
	b, _ := announceBot(t, tg, gen, true, 80*time.Millisecond)
	runKickDisposal(t, b, tg, 78, 98, "TTL")

	// The announcement send is recorded (delete of the ad message itself
	// happened during disposal; the TTL deletion targets the announcement).
	waitFor(t, func() bool { return tg.sendCount() == 1 }, "announcement with ttl")
	// fakeTG.DeleteMessage records ids as strings; the TTL goroutine deletes
	// the announcement message id (1). Wait for a delete beyond the disposal.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(tg.deleteList()) >= 2 { // ad message + announcement
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	deletes := tg.deleteList()
	if len(deletes) < 2 {
		t.Fatalf("TTL must delete the announcement message: deletes=%v", deletes)
	}
	if deletes[len(deletes)-1] != "1" {
		t.Fatalf("the announcement message id must be deleted: %v", deletes)
	}
}

// --- F.10: slow AI must not affect the disposal chain (< 100ms) --------------

func TestAnnounceSlowAIDoesNotBlockDisposal(t *testing.T) {
	tg := newFakeTG()
	// The generator blocks for 800ms — far beyond the 100ms budget.
	gen := &fakeAnnouncer{out: "🚪 Slow 已送出门外", delay: 800 * time.Millisecond}
	b, _ := announceBot(t, tg, gen, true, 0)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 79, ChatID: -600})

	start := time.Now()
	b.HandleMessage(ctx, adMsgWithFrom(-600, 79, 99, "加V赚钱", "Slow"))
	elapsed := time.Since(start)

	// Disposal (delete + ban) completed while the announcement was still
	// generating: the punishment path must return in < 100ms.
	if len(tg.banList()) != 1 {
		t.Fatalf("disposal must complete: %v", tg.banList())
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("slow announce AI blocked the disposal chain: %s", elapsed)
	}
	// ... and the announcement eventually lands (asynchronous).
	waitFor(t, func() bool { return tg.sendCount() == 1 }, "delayed announcement")
}

// --- full chain: real ai.Client against httptest (offline) -------------------

// TestAnnounceFullChainRealClient drives a disposal whose announcement goes
// through a REAL ai.Client → httptest gateway, proving the wiring end-to-end
// (model chain, request shape, post-processing) without any network access.
func TestAnnounceFullChainRealClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%s}}]}`,
			`"🚪 全链 已送出门外，广告请去别处发～"`)
	}))
	defer srv.Close()

	client := ai.NewClient(ai.ClientConfig{
		BaseURL: srv.URL, APIKey: "k",
		Models: []string{"m1"}, Timeout: 2 * time.Second,
	})
	client.SetAnnounceTimeout(2 * time.Second)

	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.Action = "ban"
	b := testBot(t, tg, &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}, cfg)
	b.Announcer = NewAnnouncer(tg, client, true, 2*time.Second, 0, 2*time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 80, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 80, 100, "加V赚钱", "全链"))

	waitFor(t, func() bool { return tg.sendCount() == 1 }, "real-client announcement")
	sends := tg.sendsSnapshot()
	if !strings.Contains(sends[0], "全链") {
		t.Fatalf("announcement must carry the name: %q", sends[0])
	}
	_, sent, fallback, errs := b.AnnounceStats()
	if sent != 1 || fallback != 0 || errs != 0 {
		t.Fatalf("stats = (1,%d,%d), want (1,0,0)", fallback, errs)
	}
}

// --- send failure → announce_error + notifier error push, no retry storm -----

func TestAnnounceSendFailureErrorPushNoRetry(t *testing.T) {
	tg := newFakeTG()
	tg.sendErr = errors.New("sendMessage 500")
	gen := &fakeAnnouncer{out: "🚪 Err 已送出门外"}
	cfg := defaultCfg()
	cfg.LogChat = 999
	b := testBot(t, tg, &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}, cfg)
	rec := wireTestNotifier(t, b, cfg)
	b.Announcer = NewAnnouncer(tg, gen, true, 2*time.Second, 0, 2*time.Second,
		logx.New(nil, logx.Error, "text", "announce"), b.Notifier)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 81, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 81, 101, "加V赚钱", "Err"))

	// The send fails: exactly one error push, one error counter tick, no
	// re-send loop (sends stay empty, generator ran once).
	waitFor(t, func() bool { return len(rec.texts()) >= 1 }, "announce error push")
	_, sent, _, errs := b.AnnounceStats()
	if sent != 0 || errs != 1 {
		t.Fatalf("stats sent=%d errs=%d, want 0/1", sent, errs)
	}
	if gen.callCount() != 1 {
		t.Fatalf("no retry storm: generator calls = %d", gen.callCount())
	}
	// The disposal itself was unaffected.
	if len(tg.banList()) != 1 {
		t.Fatalf("disposal must complete: %v", tg.banList())
	}
	if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature("加V赚钱")); !ok {
		t.Fatal("signature write must complete despite announce send failure")
	}
}

// --- Stop drains: shutdown does not leak the in-flight announcement ----------

func TestAnnounceStopDrains(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 Drain 已送出门外", delay: 150 * time.Millisecond}
	b, _ := announceBot(t, tg, gen, true, 0)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 82, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 82, 102, "加V赚钱", "Drain"))
	// Stop must wait for the in-flight announcement (drain budget 2s > delay).
	b.StopAnnouncer()
	if tg.sendCount() != 1 {
		t.Fatalf("drain must deliver the in-flight announcement: %v", tg.sendsSnapshot())
	}
}

// --- subject preference: username > first name > uid --------------------------

func TestAnnounceSubjectPreference(t *testing.T) {
	cases := []struct {
		username, first string
		uid             int64
		want            string
	}{
		{"spammer", "Spammer", 90, "spammer"},
		{"", "FirstOnly", 91, "FirstOnly"},
		{"", "", 92, "92"},
	}
	for _, tc := range cases {
		if got := announceSubject(tc.username, tc.first, tc.uid); got != tc.want {
			t.Errorf("announceSubject(%q,%q,%d) = %q, want %q",
				tc.username, tc.first, tc.uid, got, tc.want)
		}
	}
}

// --- template pool sanity: ≥ 8 templates, each renders the subject -----------

func TestAnnounceTemplatePool(t *testing.T) {
	if len(announceTemplates) < 8 {
		t.Fatalf("template pool must hold ≥ 8 entries, has %d", len(announceTemplates))
	}
	seen := map[string]bool{}
	for _, tpl := range announceTemplates {
		out := fmt.Sprintf(tpl, "someone")
		if !strings.Contains(out, "someone") {
			t.Fatalf("template %q must render the subject", tpl)
		}
		seen[out] = true
		// Templates must be contact-free (name only, no ad content).
		if _, err := ai.CleanAnnounce(out, "someone"); err != nil {
			t.Fatalf("template %q fails its own compliance filter: %v", tpl, err)
		}
	}
	if len(seen) != len(announceTemplates) {
		t.Fatalf("templates must be distinct: %d/%d", len(seen), len(announceTemplates))
	}
}

// --- dedupe: distinct disposals each announce ----------------------------------

func TestAnnounceDistinctDisposalsEachAnnounce(t *testing.T) {
	tg := newFakeTG()
	gen := &fakeAnnouncer{out: "🚪 人群 已送出门外"}
	b, _ := announceBot(t, tg, gen, true, 0)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 83, ChatID: -600})
	b.Store.AddSuspect(ctx, store.Suspect{UID: 84, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 83, 103, "加V赚钱", "甲"))
	b.HandleMessage(ctx, adMsgWithFrom(-600, 84, 104, "加V赚钱", "乙"))
	waitFor(t, func() bool { return tg.sendCount() == 2 }, "two announcements")

	for _, s := range tg.sendsSnapshot() {
		if !strings.Contains(s, "人群") {
			t.Fatalf("announcement text wrong: %q", s)
		}
	}
}
