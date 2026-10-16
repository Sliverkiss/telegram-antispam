// The amend8 bot-chat-message TTL subsystem.
//
// Every message the bot sends INTO A CHAT (the /start command reply, the
// leave notice for non-whitelisted groups, and the amend7 kick announcement)
// is auto-deleted BOT_MSG_TTL
// (default 60s, 0 = never) after it was sent. It absorbs the amend7
// BOT_ANNOUNCE_TTL and the legacy BOT_CMD_AUTODELETE_SEC knobs.
//
// Hard contract (TASK_antispam_amend8.md):
//   - The notification channel (BOT_LOG_CHAT / internal/notify) is NOT
//     covered: its pushes go through notify's own Sender and stay forever.
//     This file never touches that path.
//   - One entry point for all deletable sends: SendChat. Scheduling happens
//     only AFTER a successful send, in a goroutine — never on the caller's
//     path (command replies and the punishment chain must stay millisecond-
//     fast; tests assert < 100ms).
//   - Idempotent: one message is scheduled at most once (dedupe by
//     chat+message_id under a mutex).
//   - Delete failures only WARN (a manually deleted message or one older
//     than 48h returns 400 — normal tolerance), log event msg_ttl_delete_error.
//   - In-memory mode: a restart loses pending deletions — those messages
//     simply are not deleted (documented cost, no code compensation).
//   - Shutdown: Stop drains in-flight deletions up to the BOT_JUDGE_DRAIN
//     budget; already-sent messages are NOT deleted on shutdown (keep it
//     simple — the task explicitly says so).
package bot

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"adbot/internal/logx"
)

// chatSender is the sending surface the TTL helper wraps (the subset of
// TelegramClient SendChat needs).
type chatSender interface {
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
	DeleteMessage(ctx context.Context, chatID, messageID int64) error
}

// msgDeleter schedules delayed deletions of bot-sent chat messages.
// ttl <= 0 disables deletion entirely (BOT_MSG_TTL=0 → never delete).
type msgDeleter struct {
	tg      chatSender
	log     *logx.Logger
	ttl     time.Duration
	drainV  time.Duration // graceful-shutdown budget (BOT_JUDGE_DRAIN magnitude)
	enabled bool

	wg      sync.WaitGroup
	stopped chan struct{}
	once    sync.Once

	mu        sync.Mutex
	scheduled map[string]bool // "chat:msg" → already scheduled (idempotency)

	// stats (atomic, /statusz)
	scheduledTotal atomic.Int64
	deletedTotal   atomic.Int64
	errorTotal     atomic.Int64
}

// newMsgDeleter builds the subsystem; ttl <= 0 or nil tg → deletion disabled
// (SendChat still sends, it just never schedules a delete).
func newMsgDeleter(tg chatSender, ttl, drain time.Duration, log *logx.Logger) *msgDeleter {
	if log == nil {
		log = logx.New(nil, logx.Error, "text", "msgttl")
	}
	if drain <= 0 {
		drain = 10 * time.Second
	}
	return &msgDeleter{
		tg:        tg,
		log:       log,
		ttl:       ttl,
		drainV:    drain,
		enabled:   ttl > 0 && tg != nil,
		stopped:   make(chan struct{}),
		scheduled: map[string]bool{},
	}
}

// Enabled reports whether delayed deletion is active.
func (m *msgDeleter) Enabled() bool { return m != nil && m.enabled }

// Stats returns the /statusz counters.
func (m *msgDeleter) Stats() (scheduled, deleted, errs int64) {
	if m == nil {
		return 0, 0, 0
	}
	return m.scheduledTotal.Load(), m.deletedTotal.Load(), m.errorTotal.Load()
}

// SendChat is the ONE entry point for bot messages that carry the TTL: send,
// then (async) schedule the delayed delete. It returns the sent message id
// (0 on send failure). Scheduling never blocks the caller: it runs in its own
// goroutine; a TTL of 0 disables deletion while the send still happens.
func (m *msgDeleter) SendChat(ctx context.Context, chatID int64, text string) (int64, error) {
	msgID, err := m.tg.SendMessage(ctx, chatID, text)
	if err != nil {
		return 0, err
	}
	return m.schedule(ctx, chatID, msgID)
}

// SendChatNoTTL is the amend10 escape hatch for messages that must NOT ride
// the global TTL: it sends exactly like SendChat but never registers a
// delayed delete. The ONE consumer is the join-verification welcome message,
// whose lifetime is bound to the verification (deleted on pass / timeout /
// leave), not to BOT_MSG_TTL — a 60s TTL would delete the instruction before
// the joiner ever reads it. Tests assert the message is NOT in the TTL
// registry. Nothing else about the global default behavior changes.
func (m *msgDeleter) SendChatNoTTL(ctx context.Context, chatID int64, text string) (int64, error) {
	return m.tg.SendMessage(ctx, chatID, text)
}

// schedule registers the delayed deletion for a successfully sent message
// (async, idempotent). Split from SendChat so the no-TTL path (SendChatNoTTL)
// shares nothing with it beyond the send itself.
func (m *msgDeleter) schedule(_ context.Context, chatID int64, msgID int64) (int64, error) {
	if !m.Enabled() || msgID <= 0 {
		return msgID, nil
	}
	// Idempotency + stats off the caller's path.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		key := fmt.Sprintf("%d:%d", chatID, msgID)
		m.mu.Lock()
		if m.scheduled[key] {
			m.mu.Unlock()
			return // same message registered twice → delete once
		}
		m.scheduled[key] = true
		m.mu.Unlock()
		m.scheduledTotal.Add(1)
		m.log.Info("msg_ttl_scheduled",
			"chat", chatID,
			"msg_id", msgID,
			"ttl", m.ttl.String())

		select {
		case <-time.After(m.ttl):
		case <-m.stopped:
			return // shutting down: do not delete (task: keep it simple)
		}
		ctxD, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.tg.DeleteMessage(ctxD, chatID, msgID); err != nil {
			m.errorTotal.Add(1)
			// 400 (already deleted / >48h) and friends are normal tolerance:
			// WARN only, no retry, never blocks anything else.
			m.log.Warn("msg_ttl_delete_error",
				"chat", chatID,
				"msg_id", msgID,
				"err", err.Error())
			return
		}
		m.deletedTotal.Add(1)
		m.log.Info("msg_ttl_deleted",
			"chat", chatID,
			"msg_id", msgID)
	}()
	return msgID, nil
}

// Stop drains in-flight deletions up to the drain budget (idempotent).
// Pending-not-yet-expired deletions are dropped by design (restart semantics
// documented in the README; the task says do NOT delete on shutdown).
func (m *msgDeleter) Stop() {
	if m == nil {
		return
	}
	m.once.Do(func() {
		close(m.stopped)
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(m.drainV):
		}
	})
}
