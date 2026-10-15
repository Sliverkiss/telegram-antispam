// The amend10/amend11 join verification: the 300-second sticker gate.
//
// ORDER (amend11, user-mandated, strict): 观察名单 → 签名库 → 四条件裸号必杀
// (amend9) → AI 简介判定 → 【THIS】入群验证 →（无简介）等发言. The welcome
// message and the pending registration hang off the FINAL non-ad conclusion of
// the join-bio judgment (first AI round, a retry round, or the local-rule
// fallback — all funnel through Bot.onJoinVerdictNonAd): an AD conclusion
// disposes the joiner with NO welcome and NO registration; a failing/timed-out
// judgment goes to the existing retry queue with NO welcome and NO
// registration while it is pending. A joiner with NO bio has nothing to judge:
// the welcome is sent directly at join time.
//
// Hard contracts (TASK_antispam_amend11.md):
//   - Idempotent welcome: one welcome / one registration per user. The
//     joinVerify woken map pins this across multiple retry rounds and the
//     local-rule fallback.
//   - Pre-registration health check (登记前体检): before welcoming, a
//     getChatMember confirms the joiner is still IN the group. left/kicked →
//     no welcome, no registration (join_verify_welcome_skipped reason=gone);
//     already disposed by us → skip. Over-cap / welcome-send failure → the
//     gate is skipped for that joiner (never a silent trap).
//   - Passing shape: message.sticker != nil, same chat, NOTHING else (no
//     relaxation knob; a regression test pins the pure-emoji-text case).
//     While a user is pending, their messages never reach the AI.
//   - The welcome message does NOT ride the amend8 60s TTL (sendChatNoTTL);
//     it is deleted exactly when the verification ends.
//   - Timeout robustness (amend11 C — Telegram notifications DO get lost):
//     BEFORE the timeout disposal, a getChatMember re-check decides —
//     left/kicked → clear only, never dispose (join_verify_timeout_skipped
//     reason=gone); restricted and unable to send → clear only, WARN
//     (reason=unable_to_verify); normal member → dispose; query failing
//     after 2 retries → still dispose (fail-open, WARN).
//   - Clear paths are idempotent and NEVER dispose: chat_member
//     left/kicked/restricted (any kicker, including other bots), OUR OWN
//     punishment chain disposing a pending user (clearDisposedUid — hook in
//     the punish chains), pass, and the timeout outcome itself.
//   - Timeout disposal: the standard punish() chain with
//     action=BOT_JOIN_VERIFY_ACTION (default "ban"), source="join_verify",
//     category="其他", score=100, evidence "🔎 入群 300 秒未验证（未发贴纸）".
//     A disposal API error (USER_NOT_PARTICIPANT / not found / permission) is
//     non-fatal: WARN + pending still cleared (no retry storm, no crash).
//   - dry-run: join_verify_dry_run log only, no kick, counters excluded.
//   - The deadline is enforced by ONE background scan ticker
//     (BOT_JOIN_VERIFY_SCAN), never a time.AfterFunc per joiner; the ticker
//     dies with Stop. The scan reads a stubbed clock so tests time-travel.
//   - Pending state is in-memory with a mutex and a hard cap
//     (BOT_JOIN_VERIFY_MAX_PENDING). A restart loses pending (README 已知限制).
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

// pendingVerify is one joiner awaiting their sticker. amend11 removed the
// amend10 bio/joinMsgID parking: the gate now sits AFTER the join-bio
// judgment, so there is nothing left to suspend — a pending joiner has
// already cleared the AI screen.
type pendingVerify struct {
	uid          int64
	chatID       int64
	deadline     time.Time
	welcomeMsgID int64
	firstName    string
	username     string
	registeredAt time.Time
}

// joinVerifier holds the pending set, the woken map, the scan ticker and the
// counters.
type joinVerifier struct {
	b *Bot

	maxPending int

	mu      sync.Mutex
	pending map[int64]*pendingVerify // uid → entry (one group at a time)
	// expiring (amend14): entries whose deadline has crossed and whose
	// disposal chain is IN FLIGHT. scanDeadlines moves them here instead of
	// dropping them on the floor: until expireOne claims the entry, a sticker
	// from the joiner still rescues them (join_verify_late_pass). This closes
	// the amend14 race — a sticker landing between the scan tick and the
	// ban used to be silently ignored ("发了贴纸仍被踢").
	expiring map[int64]*pendingVerify // uid → entry (disposal in flight)
	// woken pins the idempotent welcome: uid → the gate already got its
	// final non-ad conclusion (welcome sent, refused, or skipped). Set the
	// FIRST time onJoinVerdictNonAd fires for the user; every later round
	// (retry re-entry, local-rule fallback) is a no-op. Cleared when the
	// episode ends (pass / left / disposed / expired), so a FUTURE re-join
	// after this episode is welcomed fresh.
	woken map[int64]bool

	// scanNow is the clock the deadline scan and register deadline stamping
	// read. Tests stub it via setScanClock; production is time.Now. Held in
	// an atomic.Value so the ticker goroutine and a stubbing test never race.
	scanNow atomic.Value // func() time.Time

	stopped chan struct{}
	start   sync.Once

	// stats (atomic, /statusz)
	passedTotal  atomic.Int64
	timeoutTotal atomic.Int64
	// latePassedTotal counts the amend14 race rescues: stickers that arrived
	// past the deadline but BEFORE the disposal chain took the entry
	// (join_verify_late_pass_total on /statusz — the direct "发了贴纸仍被踢"
	// observability probe; a post-deploy nonzero value there = the bug was
	// live on the old build).
	latePassedTotal atomic.Int64
	// skippedTotal counts every timeout that did NOT dispose (gone /
	// unable_to_verify) plus welcome-time skips (join_verify_skipped_total).
	skippedTotal atomic.Int64
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
		expiring:   map[int64]*pendingVerify{},
		woken:      map[int64]bool{},
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
// total, skipped total. Timeout counts real disposals only — dry-run and
// skipped outcomes are excluded. expiring entries still count into pending
// (join_verify_pending is "awaiting a verdict": the disposal has NOT yet
// landed, and a late sticker can still rescue them).
func (v *joinVerifier) Stats() (pending, passed, timeouts, skipped int64) {
	v.mu.Lock()
	n := int64(len(v.pending) + len(v.expiring))
	v.mu.Unlock()
	return n, v.passedTotal.Load(), v.timeoutTotal.Load(), v.skippedTotal.Load()
}

// latePassed returns join_verify_late_pass_total (amend14): stickers that
// arrived past the deadline but before the disposal claimed the entry.
func (v *joinVerifier) latePassed() int64 { return v.latePassedTotal.Load() }

// isPending reports whether uid is awaiting verification — including entries
// past their deadline whose disposal is still in flight (amend14: the
// message-gate must keep holding the user's messages until the timeout
// outcome actually lands, so a sticker in the race window is judged instead
// of silently falling through).
func (v *joinVerifier) isPending(uid int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.pending[uid]
	if ok {
		return true
	}
	_, ok = v.expiring[uid]
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
//
// amend14: due entries move pending → expiring (NOT a hard delete). The
// entries stay rescuable until expireOne claims them: a sticker that lands
// in the window between this scan tick and the disposal chain's ban call
// still passes (handleStickerMessage consults expiring too) — the old code
// dropped the entry on the floor here, so the sticker hit
// isPending()==false, fell through the ordinary flow (a sticker is not
// judgeable → ignored), and the user was banned anyway: exactly the
// observed "发了贴纸仍被踢".
func (v *joinVerifier) scanDeadlines() {
	now := v.now()
	var due []*pendingVerify
	v.mu.Lock()
	for uid, p := range v.pending {
		if now.After(p.deadline) {
			due = append(due, p)
			delete(v.pending, uid)
			delete(v.woken, uid)
			v.expiring[uid] = p
		}
	}
	v.mu.Unlock()
	for _, p := range due {
		v.expireOne(p)
	}
}

// expireOne runs the amend11 C timeout decision for one timed-out joiner:
// re-check getChatMember first (notifications get lost — never trust only
// chat_member), then dispose / clear-only accordingly. The entry moved to
// the expiring map by scanDeadlines — every path below FIRST tries to claim
// it back (claimExpiring; a late sticker may have rescued the joiner while
// the getChatMember re-check was retrying), then only cleans up the welcome
// message and the counters. A disposal API error (USER_NOT_PARTICIPANT /
// not found / permission) is non-fatal: WARN + the entry still leaves the
// expiring map (no retry storm, no crash).
func (v *joinVerifier) expireOne(p *pendingVerify) {
	if !v.claimExpiring(p) {
		// Rescued between the scan tick and here (join_verify_late_pass in
		// handleStickerMessage): the sticker already passed the joiner,
		// deleted the welcome and cleared the maps. Do NOTHING — the ban
		// must not fire.
		v.b.Log.Info("join_verify_late_pass_ack",
			"chat", p.chatID,
			"uid", p.uid,
			"summary", "到期处置前贴纸已通过：放弃本次处置（amend14 竞态修复）")
		return
	}
	v.expireOneClaimed(p)
}

// claimExpiring atomically takes the entry out of the expiring map. It
// returns false when the entry is already gone — i.e. a sticker passed it
// after scanDeadlines marked it expiring but before expireOne got here (the
// handleStickerMessage rescue deleted it from both maps). True means the
// timeout outcome now owns the entry and may dispose.
func (v *joinVerifier) claimExpiring(p *pendingVerify) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	cur, ok := v.expiring[p.uid]
	if !ok || cur != p {
		return false
	}
	delete(v.expiring, p.uid)
	return true
}

// expireOneClaimed is expireOne after the entry has been claimed for the
// timeout outcome (no late sticker can rescue it from here on).
func (v *joinVerifier) expireOneClaimed(p *pendingVerify) {
	if v.b.Cfg.DryRun {
		v.b.Log.Info("join_verify_dry_run",
			"chat", p.chatID,
			"uid", p.uid,
			"action", v.b.Cfg.JoinVerifyAction,
			"summary", "干跑：只打日志不踢，计数不计入")
		v.deleteWelcome(p)
		return
	}
	switch v.timeoutMembership(context.Background(), p) {
	case "dispose":
	case "gone":
		v.skippedTotal.Add(1)
		v.b.Log.Info("join_verify_timeout_skipped",
			"chat", p.chatID,
			"uid", p.uid,
			"reason", "gone",
			"summary", "到期时人已不在群：只清理欢迎消息，绝不处置")
		v.deleteWelcome(p)
		return
	case "unable_to_verify":
		v.skippedTotal.Add(1)
		v.b.Log.Warn("join_verify_timeout_skipped",
			"chat", p.chatID,
			"uid", p.uid,
			"reason", "unable_to_verify",
			"summary", "到期时人被限制无法发言：无法完成验证，只清理不处置")
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

// timeoutMembership performs the pre-disposal re-check (amend11 C): initial
// query + up to 2 retries at 1s intervals; still failing → fail-open. The
// result is the expireOne decision lane:
//
//	dispose          normal member (or the query failed after retries —
//	                 the user's principle: 不能不处置)
//	gone             left/kicked (or restricted with is_member=false)
//	unable_to_verify restricted and cannot send messages or stickers
func (v *joinVerifier) timeoutMembership(ctx context.Context, p *pendingVerify) string {
	var m telegram.ChatMember
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "dispose"
			case <-time.After(time.Second):
			}
		}
		m, err = v.b.TG.GetChatMember(ctx, p.chatID, p.uid)
		if err == nil {
			return classifyTimeoutMembership(m)
		}
		v.b.Log.Warn("join_verify_member_check_error",
			"chat", p.chatID,
			"uid", p.uid,
			"attempt", attempt+1,
			"err", err.Error())
	}
	// Fail-open (amend11 C: 查询失败 → 仍按超时处置 + WARN — the user's
	// explicit "不能不处置" overrides lost-signal caution here).
	v.b.Log.Warn("join_verify_member_check_failed",
		"chat", p.chatID,
		"uid", p.uid,
		"summary", "成员状态查询连续失败：fail-open，仍按超时处置")
	return "dispose"
}

// classifyTimeoutMembership maps one getChatMember result onto the expireOne
// decision lane (pure; unit-testable without a TG fake).
func classifyTimeoutMembership(m telegram.ChatMember) string {
	switch m.Status {
	case "left", "kicked":
		return "gone"
	case "restricted":
		if !m.IsMember {
			return "gone"
		}
		if !m.CanSendMessages || !m.CanSendOtherMessages {
			return "unable_to_verify"
		}
	}
	return "dispose"
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
// It returns false when over the cap (the joiner falls through — nothing was
// pending for them, the gate simply does not exist) or when the welcome send
// fails hard (no welcome → no gate: the joiner would be stuck for 300s with
// no way to know why).
func (v *joinVerifier) register(ctx context.Context, chatID, uid int64, username, firstName string) bool {
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
		delete(v.expiring, uid) // amend14: a stale expiring twin must not linger
		v.mu.Unlock()
		v.deleteWelcome(old)
	} else if old, ok := v.expiring[uid]; ok {
		// Re-join while the OLD episode's disposal is in flight: the re-join
		// proves the user is back, so the in-flight timeout outcome would
		// ban a member who just re-joined. Drop the expiring entry (the
		// claim in expireOne will fail → no ban) and re-register fresh.
		delete(v.expiring, uid)
		v.mu.Unlock()
		v.deleteWelcome(old)
	} else {
		v.mu.Unlock()
	}

	deadline := v.now().Add(v.b.verifyTimeout())
	msgID, err := v.b.sendChatNoTTL(ctx, chatID, v.welcomeText(username, firstName))
	if err != nil {
		v.b.Log.Warn("join_verify_welcome_error",
			"chat", chatID,
			"uid", uid, "err", err.Error(),
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
//
// amend14: a pending entry AND an expiring entry both match. The expiring
// case is the race rescue — the deadline scan already moved the joiner to
// expiring but expireOne has not claimed them yet (the getChatMember
// re-check retries 2×1s, and the delete+ban round-trips follow: a multi-
// second window per joiner). A sticker arriving in that window PASSES: the
// entry leaves both maps, so claimExpiring in expireOne returns false and
// the ban never fires. The rescue is counted in latePassedTotal
// (join_verify_late_pass_total) — the direct probe for the reported
// "发了贴纸仍被踢" incident.
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
	if !ok {
		p, ok = v.expiring[uid]
	}
	if ok && p.chatID != m.Chat.ID {
		// Cross-group sticker: does NOT verify (chatID guard). The pending/
		// expiring entry stays; the message is not consumed.
		v.mu.Unlock()
		return false
	}
	if ok {
		delete(v.pending, uid)
		delete(v.expiring, uid)
		delete(v.woken, uid)
	}
	v.mu.Unlock()
	if !ok {
		return false
	}
	v.passedTotal.Add(1)
	if p.deadline.Before(v.now()) {
		v.latePassedTotal.Add(1)
		v.b.Log.Info("join_verify_late_pass",
			"chat", p.chatID,
			"uid", uid,
			"deadline", p.deadline.Format(time.RFC3339),
			"summary", "贴纸在到期处置落地前赶到：按通过处理，绝不处置（amend14）")
	} else {
		v.b.Log.Info("join_verify_passed",
			"chat", p.chatID,
			"uid", uid,
			"ms", v.now().Sub(p.registeredAt).Milliseconds())
	}
	v.deleteWelcome(p)
	return true
}

// clearOnLeft removes a pending entry when the user left / was kicked / got
// restricted by anyone (含别的 bot — the update carries no reliable author),
// and deletes the residual welcome message: zero disposal (不踢空气),
// idempotent, and the 300s deadline can no longer reach the cleared entry.
func (v *joinVerifier) clearOnLeft(uid int64) {
	v.mu.Lock()
	p, ok := v.pending[uid]
	if !ok {
		// amend14: also clear an expiring twin — the user left/got removed
		// while OUR timeout chain was walking. Dropping it here fails the
		// expireOne claim, so the in-flight disposal stops short of the ban
		// (the 300s deadline must never ban someone who is already out; the
		// 零处置 principle wins over the already-started chain).
		p, ok = v.expiring[uid]
		if !ok {
			v.mu.Unlock()
			return
		}
		delete(v.expiring, uid)
	} else {
		delete(v.pending, uid)
		delete(v.expiring, uid)
	}
	delete(v.woken, uid)
	v.mu.Unlock()
	v.b.Log.Info("join_verify_cleared",
		"uid", uid,
		"chat", p.chatID,
		"reason", "kicked_by_other",
		"summary", "被他人/别的方式移出：清 pending、删欢迎，零处置")
	v.deleteWelcome(p)
}

// clearDisposedUid clears a pending entry after OUR OWN punishment chain
// disposed the user (amend11 B.2 — e.g. the join-bio verdict came back ad
// after the registration, or a signature hit raced ahead). The punish chain
// already deleted the welcome message via its explicit msgIDs when the
// disposal came from the verifier itself; on other chains the message would
// have been created only by this subsystem, so deleting here too is safe
// (deleteOne tolerates an already-deleted id). Idempotent no-op for an
// absent uid; NEVER a disposal.
func (v *joinVerifier) clearDisposedUid(uid int64) {
	v.mu.Lock()
	p, ok := v.pending[uid]
	if !ok {
		// amend14: also clear an expiring twin — OUR OWN chain just disposed
		// the user (e.g. the join-bio verdict concluded ad after the deadline
		// scan had already marked them expiring). Dropping it here fails the
		// expireOne claim → no double disposal from the timeout chain.
		p, ok = v.expiring[uid]
		if !ok {
			v.mu.Unlock()
			return
		}
		delete(v.expiring, uid)
	} else {
		delete(v.pending, uid)
		delete(v.expiring, uid)
	}
	delete(v.woken, uid)
	v.mu.Unlock()
	v.b.Log.Info("join_verify_cleared",
		"uid", uid,
		"chat", p.chatID,
		"reason", "disposed_by_us",
		"summary", "本方处置链已处置该用户：立刻清 pending，到期不再处置")
	v.deleteWelcome(p)
}
