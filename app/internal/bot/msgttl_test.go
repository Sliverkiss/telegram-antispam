package bot

// amend8 bot-chat-message TTL tests: everything offline via fakeTG; the TTL
// is a tiny duration (80ms) so expiry is observable without a clock stub.
// The 8 assertions of TASK_antispam_amend8.md §测试:
//  1. command replies (/start /help /status /check) expire → DeleteMessage
//     with the right chat/message id;
//  2. BOT_MSG_TTL=0 → zero deletes;
//  3. notification-channel pushes NEVER trigger a delete (critical: the
//     notify sender is a separate sink and DeleteMessage is never called
//     with BOT_LOG_CHAT as the chat id);
//  4. delete failure → WARN only, the rest of the flow unaffected;
//  5. the same message scheduled twice → deleted once;
//  6. a slow TTL/delete never blocks the command reply (< 100ms);
//  7. the leave notice and the kick announcement are covered too;
//  8. the variable contract sets (config KnownVars / .env.example /
//     docker-compose.yml / README) are identical.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/notify"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// fakeTTLTG wraps fakeTG with a per-chat send id counter and a deletable
// record of DeleteMessage calls as (chatID, messageID) pairs (fakeTG only
// records ids as strings — the TTL path needs the chat id too).
type fakeTTLTG struct {
	*fakeTG
	mu        sync.Mutex
	sendErr   error
	nextID    int64
	fixedID   int64    // > 0: every send returns this id (duplicate-simulation)
	delPairs  []string // "chat:msg"
	deleteErr error
	blocked   chan struct{} // when non-nil, DeleteMessage parks on it
}

func newFakeTTLTG() *fakeTTLTG {
	return &fakeTTLTG{
		fakeTG: newFakeTG(),
	}
}

func (f *fakeTTLTG) SendMessage(_ context.Context, chatID int64, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	if f.fixedID > 0 {
		f.fakeTG.sends = append(f.fakeTG.sends, text)
		return f.fixedID, nil
	}
	f.nextID++
	f.fakeTG.sends = append(f.fakeTG.sends, text)
	return f.nextID, nil
}

func (f *fakeTTLTG) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	if f.blocked != nil {
		select {
		case <-f.blocked:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fakeTG.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.delPairs = append(f.delPairs, itoa(chatID)+":"+itoa(messageID))
	return nil
}

func (f *fakeTTLTG) deletePairsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.delPairs...)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ttlBot wires a Bot around fakeTTLTG with the given BOT_MSG_TTL.
func ttlBot(t *testing.T, tg *fakeTTLTG, ttl time.Duration) *Bot {
	t.Helper()
	cfg := defaultCfg()
	cfg.MsgTTL = ttl
	return testBot(t, tg, &fakeAI{}, cfg)
}

// waitForMsgTTLDelete polls until n TTL deletions landed in tg.delPairs.
func waitForTTLDeletes(t *testing.T, tg *fakeTTLTG, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(tg.deletePairsSnapshot()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: deletes=%v", what, tg.deletePairsSnapshot())
}

// --- 1. command replies expire → DeleteMessage with the right ids ------------

func TestMsgTTLCommandRepliesDeleted(t *testing.T) {
	for _, cmd := range []string{"/start", "/help", "/status", "/check 文本"} {
		t.Run(cmd, func(t *testing.T) {
			tg := newFakeTTLTG()
			cfg := defaultCfg()
			cfg.MsgTTL = 80 * time.Millisecond
			cfg.AdminUsers = map[int64]bool{42: true} // /status /check admin
			b := testBot(t, tg, &fakeAI{}, cfg)
			ctx := context.Background()

			m := adMsg(-500, 42, 1, cmd)
			b.HandleUpdate(ctx, cmdUpdate(m))

			waitFor(t, func() bool { return len(tg.sends) == 1 }, "command reply sent")
			waitForTTLDeletes(t, tg, 1, cmd+" reply TTL delete")
			pairs := tg.deletePairsSnapshot()
			if len(pairs) != 1 || pairs[0] != "-500:1" {
				t.Fatalf("TTL delete = %v, want [-500:1] (chat/msg ids)", pairs)
			}
			if _, deleted, _ := b.MsgTTLStats(); deleted != 1 {
				t.Fatalf("msg_ttl_deleted_total = %d, want 1", deleted)
			}
		})
	}
}

// --- 2. BOT_MSG_TTL=0 → never delete ------------------------------------------

func TestMsgTTLZeroNeverDeletes(t *testing.T) {
	tg := newFakeTTLTG()
	b := ttlBot(t, tg, 0)
	ctx := context.Background()
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 1, "/start")))

	waitFor(t, func() bool { return len(tg.sends) == 1 }, "reply sent without TTL")
	// Long enough for any stray timer to fire; the assertion is zero deletes.
	time.Sleep(200 * time.Millisecond)
	if got := tg.deletePairsSnapshot(); len(got) != 0 {
		t.Fatalf("BOT_MSG_TTL=0 must never delete: %v", got)
	}
	if got := tg.deleteCalls; got != 0 {
		t.Fatalf("BOT_MSG_TTL=0 must zero DeleteMessage calls: %d", got)
	}
}

// --- 3. notification channel pushes NEVER trigger a delete (key assertion) ----

// The notify path sends to BOT_LOG_CHAT through its OWN Sender (never
// b.sendChat). This test wires the real notifier to the same fakeTTLTG and
// drives a full disposal: pushes land in the log chat and DeleteMessage is
// NEVER called with the log chat id — not by notify (its sender only
// sends), not by the TTL subsystem (notifications never go through it).
func TestMsgTTLNotifyChannelNeverDeleted(t *testing.T) {
	tg := newFakeTTLTG()
	cfg := defaultCfg()
	cfg.MsgTTL = 80 * time.Millisecond
	cfg.LogChat = -999 // the notification channel
	b := testBot(t, tg, &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}, cfg)
	// The notify sender routes through fakeTTLTG.SendMessage so the test can
	// assert "a message WAS delivered to the log chat" while watching every
	// DeleteMessage against that chat.
	rec := &lockedRecorder{}
	n := notify.New(notify.Config{
		LogChat:      cfg.LogChat,
		OnPunish:     true,
		OnError:      true,
		Level:        logx.Error,
		RatePerMin:   10000,
		QueueSize:    cfg.NotifyQueue,
		DedupeWindow: cfg.NotifyDedupeWindow,
		MaxLen:       cfg.NotifyMaxLen,
		TextLen:      cfg.NotifyTextLen,
		IncludeText:  cfg.NotifyIncludeText,
		Drain:        cfg.NotifyDrain,
	}, notifySink{tg: tg, rec: rec}, logx.New(nil, logx.Error, "text", "notify"))
	b.Notifier = n
	n.Start(context.Background())
	t.Cleanup(n.Stop)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 6, ChatID: -600})

	b.HandleMessage(ctx, adMsg(-600, 6, 22, "加QQ赚钱 日入过万"))

	// The disposition push landed in the log chat (via notify's own sender).
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "notify push to log chat")
	// The only TTL-deleted message ids are the ones sent through sendChat:
	// the command replies / leave notices / announcements. The disposal's own
	// delete of the triggering ad message targets chat -600 (not the log
	// chat). Give any misrouted timer plenty of time to expose itself.
	time.Sleep(300 * time.Millisecond)
	for _, pair := range tg.deletePairsSnapshot() {
		if strings.HasPrefix(pair, "-999:") {
			t.Fatalf("notification channel message must NEVER be deleted: %v", pair)
		}
	}
	// And the push itself was never scheduled for deletion.
	if sched, _, _ := b.MsgTTLStats(); sched != 0 {
		t.Fatalf("notify push must not be TTL-scheduled: scheduled=%d", sched)
	}
}

// notifySink is the notify.Sender used by the notify-channel test: delivery
// goes through fakeTTLTG (so any DeleteMessage against the log chat would be
// visible) and the delivered texts land in a locked recorder for the wait.
type notifySink struct {
	tg  *fakeTTLTG
	rec *lockedRecorder
}

func (s notifySink) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	s.rec.add(text)
	return s.tg.SendMessage(ctx, chatID, text)
}

// lockedRecorder is a mutex-guarded text sink (the notifier worker writes
// from its own goroutine; the test polls from the test goroutine).
type lockedRecorder struct {
	mu    sync.Mutex
	sends []string
}

func (r *lockedRecorder) add(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, text)
}

func (r *lockedRecorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sends...)
}

// --- 4. delete failure → WARN only, the rest unaffected -----------------------

func TestMsgTTLDeleteFailureWarnOnly(t *testing.T) {
	tg := newFakeTTLTG()
	tg.deleteErr = errors.New("400 message to delete not found")
	b := ttlBot(t, tg, 60*time.Millisecond)
	ctx := context.Background()
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 1, "/start")))
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 2, "/help")))

	// Two replies sent; both TTL deletions failed (WARN only) — no panic,
	// no retry storm, stats count the errors, sends unaffected.
	waitFor(t, func() bool { return len(tg.sends) == 2 }, "both replies sent")
	waitForTTLDeletesErrs(t, b, 2)
	if tg.deleteCalls != 2 {
		t.Fatalf("exactly one delete attempt per message, got %d", tg.deleteCalls)
	}
	// A later message still flows normally.
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 3, "/status")))
	waitFor(t, func() bool { return len(tg.sends) == 3 }, "third reply unaffected")
}

func waitForTTLDeletesErrs(t *testing.T, b *Bot, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, errs := b.MsgTTLStats(); errs == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, _, errs := b.MsgTTLStats()
	t.Fatalf("timed out waiting for %d TTL errors, got %d", want, errs)
}

// --- 5. the same message registered twice → deleted once ----------------------

func TestMsgTTLDuplicateScheduledDeletesOnce(t *testing.T) {
	tg := newFakeTTLTG()
	// fixedID simulates Telegram handing back the SAME message id for two
	// sends (e.g. a retry racing a re-registration): the dedupe guard must
	// schedule exactly one deletion.
	tg.fixedID = 1
	b := ttlBot(t, tg, 60*time.Millisecond)
	ctx := context.Background()

	if _, err := b.sendChat(ctx, -500, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.sendChat(ctx, -500, "two"); err != nil {
		t.Fatal(err)
	}

	waitForTTLDeletes(t, tg, 1, "single delete despite duplicate registration")
	if sched, deleted, errs := b.MsgTTLStats(); sched != 1 || deleted != 1 || errs != 0 {
		t.Fatalf("stats = (%d,%d,%d), want (1,1,0)", sched, deleted, errs)
	}
	// No second delete after a full grace period.
	time.Sleep(200 * time.Millisecond)
	if got := tg.deleteCalls; got != 1 {
		t.Fatalf("duplicate registration must delete once, calls=%d", got)
	}
}

// --- 6. slow delete/scheduling never blocks the command reply (< 100ms) -------

func TestMsgTTLSlowDeleteDoesNotBlockReply(t *testing.T) {
	tg := newFakeTTLTG()
	// The delete parks for 500ms — far beyond the 100ms budget; the reply
	// itself must return immediately.
	tg.blocked = make(chan struct{})
	time.AfterFunc(500*time.Millisecond, func() { close(tg.blocked) })
	b := ttlBot(t, tg, 30*time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 1, "/start")))
	elapsed := time.Since(start)

	if len(tg.sends) != 1 {
		t.Fatalf("command reply must be sent: %v", tg.sends)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("slow TTL delete blocked the command reply: %s", elapsed)
	}
	// The delete eventually lands (asynchronous).
	waitForTTLDeletes(t, tg, 1, "delayed TTL delete after unblock")
}

// --- 7. leave notice and kick announcement are covered -----------------------

func TestMsgTTLLeaveNoticeDeleted(t *testing.T) {
	tg := newFakeTTLTG()
	cfg := defaultCfg()
	cfg.MsgTTL = 60 * time.Millisecond
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

	waitFor(t, func() bool { return len(tg.sends) == 1 }, "leave notice sent")
	waitForTTLDeletes(t, tg, 1, "leave notice TTL delete")
	pairs := tg.deletePairsSnapshot()
	if len(pairs) != 1 || pairs[0] != "-424242:1" {
		t.Fatalf("leave notice delete = %v, want [-424242:1]", pairs)
	}
}

func TestMsgTTLAnnouncementDeleted(t *testing.T) {
	tg := newFakeTTLTG()
	cfg := defaultCfg()
	cfg.MsgTTL = 60 * time.Millisecond
	cfg.Action = "ban"
	b := testBot(t, tg, &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}, cfg)
	b.Announcer = NewAnnouncer(tg, &fakeAnnouncer{out: "🚪 TTL 已送出门外"},
		true, 2*time.Second, 2*time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil, b)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 78, ChatID: -600})

	b.HandleMessage(ctx, adMsgWithFrom(-600, 78, 98, "加V赚钱", "TTL"))

	// Two deletions land: the triggering ad message (98, by the disposal) and
	// the announcement (id 1, by the TTL).
	waitForTTLDeletes(t, tg, 2, "announcement TTL delete")
	pairs := tg.deletePairsSnapshot()
	found := false
	for _, p := range pairs {
		if p == "-600:1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("announcement message must be TTL-deleted: %v", pairs)
	}
}

// --- Stop drains ----------------------------------------------------------------

func TestMsgTTLStopDrains(t *testing.T) {
	tg := newFakeTTLTG()
	b := ttlBot(t, tg, 120*time.Millisecond)
	b.msgTTL.drainV = 2 * time.Second
	ctx := context.Background()

	start := time.Now()
	b.HandleUpdate(ctx, cmdUpdate(adMsg(-500, 42, 1, "/start")))
	// Stop before expiry: the in-flight timer is released by close(stopped)
	// and the deletion is dropped (shutdown never deletes — the task says so).
	b.StopMsgTTL()
	if time.Since(start) > time.Second {
		t.Fatalf("Stop must not wait the whole TTL: %s", time.Since(start))
	}
	time.Sleep(250 * time.Millisecond)
	if got := tg.deletePairsSnapshot(); len(got) != 0 {
		t.Fatalf("shutdown must NOT delete pending messages: %v", got)
	}
	// Idempotent double-stop.
	b.StopMsgTTL()
}
