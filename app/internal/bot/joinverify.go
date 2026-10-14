// The amend10 join verification: the 300-second sticker gate.
//
// Flow position (user-mandated order, strict): 观察名单 → 签名库 →
// 四条件裸号必杀 (amend9) → 【THIS】入群验证 → AI. When armed, every joiner
// that survives the earlier layers gets a welcome message and must post A
// STICKER before BOT_JOIN_VERIFY_TIMEOUT expires. Until they do, their
// messages NEVER reach the AI; the deadline expiry disposes them through the
// STANDARD punishment chain (notify + in-group announcement ride it).
//
// Hard contracts (TASK_antispam_amend10.md):
//   - Passing shape: message.sticker != nil, NOTHING else. Pure-emoji TEXT,
//     photos, words, voice, video, forwards do not pass — and there is NO
//     relaxation knob, by explicit user demand ("必须要贴纸"). A regression
//     test pins the pure-emoji-text case so a future edit cannot widen it.
//   - Passing requires the uid in pending AND the chatID matching (a sticker
//     in ANOTHER group does not verify).
//   - The welcome message does NOT ride the amend8 60s TTL: it must stay
//     readable for the whole window. It is deleted exactly when the
//     verification ends (pass / timeout disposal / user left). It goes
//     through sendChatNoTTL — the ONE send path that skips the TTL registry.
//   - Timeout disposal: the standard punish() chain with
//     action=BOT_JOIN_VERIFY_ACTION (default "ban" — a real ban, not
//     kick-and-readmit), source="join_verify", category="其他", score=100,
//     evidence line "🔎 入群 300 秒未验证（未发贴纸）"; the welcome message
//     deletion rides the chain's explicit msgIDs.
//   - dry-run: join_verify_dry_run log only, no kick, counter excluded.
//   - The deadline is enforced by ONE background scan ticker
//     (BOT_JOIN_VERIFY_SCAN), never a time.AfterFunc per joiner; the ticker
//     dies with Stop (tied to bot shutdown). The scan reads a now() hook so
//     tests can time-travel without sleeping.
//   - Pending state is in-memory with a mutex and a hard cap
//     (BOT_JOIN_VERIFY_MAX_PENDING; over-cap registration is refused with a
//     WARN — the joiner then falls through to the normal flow, never dropped
//     silently). A restart loses pending: welcome residues are not
//     compensated (README 已知限制).
package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"adbot/internal/telegram"
)

// DefaultJoinVerifyTimeout is the fallback deadline (BOT_JOIN_VERIFY_TIMEOUT).
const DefaultJoinVerifyTimeout = 300 * time.Second

// DefaultJoinVerifyScan is the fallback scan interval (BOT_JOIN_VERIFY_SCAN).
const DefaultJoinVerifyScan = 5 * time.Second

// DefaultJoinVerifyMsg is the fixed welcome wording (BOT_JOIN_VERIFY_MSG); %s
// is replaced with @username (or the display name when there is no username).
const DefaultJoinVerifyMsg = "👋 欢迎 %s！请在 300 秒内发一个贴纸，证明你不是机器人，否则会被移出本群～"

// DefaultJoinVerifyAction is the default timeout disposal (a REAL ban — the
// user's explicit wording, not kick-and-readmit).
const DefaultJoinVerifyAction = "ban"

// joinVerifyEvidence is the 💬 evidence line for the timeout disposition
// notice.
func joinVerifyEvidence() string {
	return fmt.Sprintf("🔎 入群 %d 秒未验证（未发贴纸）", int(DefaultJoinVerifyTimeout/time.Second))
}

// pendingVerify is one joiner awaiting their sticker. bio/joinMsgID park the
// join-bio AI round that the gate suspended (F.2: on pass, the flow resumes
// exactly as it would have run without the gate — 有简介 → AI 判简介；
// 无简介 → the watchlist record from screenNewMember waits for the first
// message).
type pendingVerify struct {
	uid          int64
	chatID       int64
	deadline     time.Time
	welcomeMsgID int64
	firstName    string
	username     string
	registeredAt time.Time
	bio          string
	joinMsgID    int64
}

// joinVerifier holds the pending set, the scan ticker and the counters.
type joinVerifier struct {
	b *Bot

	maxPending int

	mu      sync.Mutex
	pending map[int64]*pendingVerify // uid → entry (one group at a time)

	// scanNow is the clock the deadline scan and register deadline stamping
	// read. Tests stub it via setScanClock; production is time.Now. Held in
	// an atomic.Value so the ticker goroutine and a stubbing test never race.
	scanNow atomic.Value // func() time.Time

	stopped chan struct{}
	start   sync.Once

	// stats (atomic, /statusz)
	passedTotal  atomic.Int64
	timeoutTotal atomic.Int64
}

// joinVerifyConfig carries the BOT_JOIN_VERIFY_MAX_PENDING knob (extracted
// so newJoinVerifier stays a pure constructor with defensive defaults).
type joinVerifyConfig struct {
	maxPending int
}

// newJoinVerifier builds the subsystem (not yet started).
func newJoinVerifier(b *Bot, cfg joinVerifyConfig) *joinVerifier {
	max := cfg.maxPending
	if max < 1 {
		max = 5000
	}
	v := &joinVerifier{
		b:          b,
		maxPending: max,
		pending:    map[int64]*pendingVerify{},
		stopped:    make(chan struct{}),
	}
	v.scanNow.Store(func() time.Time { return time.Now() })
	return v
}

// now returns the scan clock (stubbed in tests).
func (v *joinVerifier) now() time.Time {
	if f, ok := v.scanNow.Load().(func() time.Time); ok && f != nil {
		return f()
	}
	return time.Now()
}

// setScanClock stubs the deadline clock (tests only; -race safe).
func (v *joinVerifier) setScanClock(f func() time.Time) { v.scanNow.Store(f) }

// Stats returns the /statusz gauges: pending count, passed total, timeout
// total. Timeout counts real disposals only — dry-run is excluded.
func (v *joinVerifier) Stats() (pending, passed, timeouts int64) {
	v.mu.Lock()
	n := int64(len(v.pending))
	v.mu.Unlock()
	return n, v.passedTotal.Load(), v.timeoutTotal.Load()
}

// isPending reports whether uid is awaiting verification.
func (v *joinVerifier) isPending(uid int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.pending[uid]
	return ok
}

// Start launches the single scan ticker. No-op when already started (the
// ticker loop must never double-start).
func (v *joinVerifier) Start() {
	if v == nil {
		return
	}
	v.start.Do(func() {
		go v.scanLoop()
	})
}

// Stop closes the ticker loop (idempotent; tied to bot shutdown).
func (v *joinVerifier) Stop() {
	if v == nil {
		return
	}
	select {
	case <-v.stopped:
		return
	default:
		close(v.stopped)
	}
}

// scanLoop is the single deadline scan goroutine (amend10 B: NOT
// time.AfterFunc per joiner — one ticker for the whole pending set).
func (v *joinVerifier) scanLoop() {
	interval := v.b.Cfg.JoinVerifyScan
	if interval <= 0 {
		interval = DefaultJoinVerifyScan
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-v.stopped:
			return
		case <-ticker.C:
			v.scanDeadlines()
		}
	}
}

// scanDeadlines disposes every entry past its deadline. It runs on the
// ticker goroutine. The chain is local (never an AI round), so there is no
// pool-parallelism to preserve and no handler to unblock.
func (v *joinVerifier) scanDeadlines() {
	now := v.now()
	var due []*pendingVerify
	v.mu.Lock()
	for uid, p := range v.pending {
		if now.After(p.deadline) {
			due = append(due, p)
			delete(v.pending, uid)
		}
	}
	v.mu.Unlock()
	for _, p := range due {
		v.expireOne(p)
	}
}

// expireOne disposes one timed-out joiner through the standard chain. The
// welcome message is deleted via the chain's explicit msgIDs (join path
// semantics: punish takes the id, no tracked list).
func (v *joinVerifier) expireOne(p *pendingVerify) {
	if v.b.Cfg.DryRun {
		v.b.Log.Info("join_verify_dry_run",
			"chat", p.chatID,
			"uid", p.uid,
			"action", v.b.Cfg.JoinVerifyAction,
			"summary", "干跑：只打日志不踢，计数不计入")
		v.deleteWelcome(p)
		return
	}
	v.timeoutTotal.Add(1)
	v.b.Log.Info("join_verify_timeout",
		"chat", p.chatID,
		"uid", p.uid,
		"deadline", p.deadline.Format(time.RFC3339))
	v.b.punish(context.Background(), punishInfo{
		chatID:    p.chatID,
		uid:       p.uid,
		username:  p.username,
		firstName: p.firstName,
		text:      joinVerifyEvidence(),
	}, v.b.Cfg.JoinVerifyAction, "join_verify", "其他",
		notifyDetails{source: "join_verify", score: 100}, []int64{p.welcomeMsgID})
}

// deleteWelcome deletes the welcome message of a pending entry (pass /
// dry-run / leave cleanup path; deleteOne is a no-op for msgID ≤ 0).
func (v *joinVerifier) deleteWelcome(p *pendingVerify) {
	if p.welcomeMsgID <= 0 {
		return
	}
	v.b.deleteOne(context.Background(), p.chatID, p.welcomeMsgID)
}

// register adds a joiner to the pending set and sends the welcome message.
// bio/joinMsgID park the suspended join-bio AI round (submitted on pass).
// It returns false when over the cap (the joiner falls through to the normal
// flow — still watchlisted and screened, never silently dropped) or when the
// welcome send fails hard (no welcome → no gate: the joiner would be stuck
// for 300s with no way to know why, so verification is skipped for them).
func (v *joinVerifier) register(ctx context.Context, chatID, uid int64, username, firstName, bio string, joinMsgID int64) bool {
	v.mu.Lock()
	if len(v.pending) >= v.maxPending {
		v.mu.Unlock()
		v.b.Log.Warn("join_verify_pending_full",
			"chat", chatID,
			"uid", uid,
			"cap", v.maxPending,
			"summary", "入群验证登记超上限：该用户走原流程")
		return false
	}
	// Re-join while a pending entry exists: the old entry (old group / old
	// welcome) is superseded — delete its welcome and re-register fresh.
	if old, ok := v.pending[uid]; ok {
		delete(v.pending, uid)
		v.mu.Unlock()
		v.deleteWelcome(old)
	} else {
		v.mu.Unlock()
	}

	deadline := v.now().Add(v.b.verifyTimeout())
	msgID, err := v.b.sendChatNoTTL(ctx, chatID, v.welcomeText(username, firstName))
	if err != nil {
		v.b.Log.Warn("join_verify_welcome_error",
			"chat", chatID, "uid", uid, "err", err.Error(),
			"summary", "欢迎消息发送失败：跳过验证，走原流程")
		return false
	}
	v.mu.Lock()
	v.pending[uid] = &pendingVerify{
		uid:          uid,
		chatID:       chatID,
		deadline:     deadline,
		welcomeMsgID: msgID,
		firstName:    firstName,
		username:     username,
		registeredAt: v.now(),
		bio:          bio,
		joinMsgID:    joinMsgID,
	}
	v.mu.Unlock()
	v.b.Log.Info("join_verify_pending_added",
		"chat", chatID,
		"uid", uid,
		"deadline", deadline.Format(time.RFC3339),
		"welcome_msg_id", msgID)
	return true
}

// welcomeText renders the fixed wording with the @username (or display name).
func (v *joinVerifier) welcomeText(username, firstName string) string {
	tpl := v.b.Cfg.JoinVerifyMsg
	if tpl == "" {
		tpl = DefaultJoinVerifyMsg
	}
	return fmt.Sprintf(tpl, joinVerifyLabel(username, firstName))
}

// joinVerifyLabel picks the @username, else the display name (amend10 C:
// 无用户名时用名字).
func joinVerifyLabel(username, firstName string) string {
	if s := strings.TrimSpace(username); s != "" {
		return "@" + s
	}
	if s := strings.TrimSpace(firstName); s != "" {
		return s
	}
	return "新朋友"
}

// handleStickerMessage checks one incoming message: a sticker from a
// pending joiner in the SAME chat passes the verification. It returns true
// when the message was CONSUMED by the verification (the caller must NOT
// continue toward the AI screening) and false when the message is irrelevant
// (no sticker, unknown uid, or cross-group).
func (v *joinVerifier) handleStickerMessage(m telegram.Message) bool {
	if m.Sticker == nil {
		return false
	}
	if m.From == nil {
		return false
	}
	uid := m.From.ID
	v.mu.Lock()
	p, ok := v.pending[uid]
	if ok && p.chatID != m.Chat.ID {
		// Cross-group sticker: does NOT verify (chatID guard). The pending
		// entry stays; the message is not consumed.
		v.mu.Unlock()
		return false
	}
	if ok {
		delete(v.pending, uid)
	}
	v.mu.Unlock()
	if !ok {
		return false
	}
	v.passedTotal.Add(1)
	v.b.Log.Info("join_verify_passed",
		"chat", p.chatID,
		"uid", uid,
		"ms", v.now().Sub(p.registeredAt).Milliseconds())
	v.deleteWelcome(p)
	// F.2: the gate ends here — resume the join flow exactly as it would have
	// run without the gate. 有简介 → the parked bio round goes to the AI
	// (async, join-key serialized ahead of the first message); 无简介 → nothing
	// to submit: the watchlist record from screenNewMember simply waits for
	// the first judgeable message.
	v.b.submitJoinBio(context.Background(), p.chatID, p.joinMsgID,
		telegram.User{ID: p.uid, Username: p.username, FirstName: p.firstName}, p.bio)
	return true
}

// clearOnLeft removes a pending entry when the user left/was kicked, and
// deletes the residual welcome message (退群清理：不再处置，不踢空气).
// Idempotent: clearing an absent uid is a silent no-op.
func (v *joinVerifier) clearOnLeft(uid int64) {
	v.mu.Lock()
	p, ok := v.pending[uid]
	if !ok {
		v.mu.Unlock()
		return
	}
	delete(v.pending, uid)
	v.mu.Unlock()
	v.b.Log.Info("join_verify_cleared",
		"uid", uid,
		"chat", p.chatID,
		"summary", "退群清理：不再处置，欢迎消息已删")
	v.deleteWelcome(p)
}
