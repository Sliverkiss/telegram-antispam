// The join-episode screen gate (one physical join = ONE screening).
//
// Root cause this closes (the "清理广告后频道推送两条" incident): Telegram
// delivers ONE physical join as TWO updates the moment a bot subscribes to
// chat_member (amend14 put it into allowed_updates):
//
//	a message update carrying new_chat_members (the join service message), and
//	a chat_member update with old=left/"" → new=member.
//
// Both funneled into HandleNewMembers → screenNewMember with no per-join
// idempotency, so every join-triggered disposal ran its whole chain TWICE:
// two AI screenings (the second usually re-hitting via the bio signature the
// first disposal had just written — "已封禁 · 100分 · 签名命中" minutes after
// the real verdict), two ban calls, two notification-channel pushes, two
// kick announcements (the Announcer dedupe keys differ: the service message
// carries messageID, the chat_member twin 0).
//
// Contract:
//   - A join EPISODE (chat+uid) is screened at most once. A second join
//     screening for the same chat+uid is a twin/replay and is dropped
//     (join_screen_dedup debug log).
//   - A genuine re-join re-screens: a chat_member left/kicked/restricted
//     transition observed AFTER the last screening re-arms the gate
//     (noteLeft), and a passed verification ends the episode (clear). Every
//     real leave+rejoin therefore screens again.
//   - TTL backstop (joinGateTTL): if the leave transition is never observed
//     (update lost), the record still expires so a later rejoin screens.
//   - The gate is input-level event collapsing, NOT output dedupe: the
//     disposal chain itself stays untouched.
//   - Known tradeoff: when the chat_member twin wins the dispatch race (the
//     twins arrive in one batch on concurrent workers), the blocked service
//     message twin drops its joinMsgID, so BOT_DELETE_JOIN_MSG may miss the
//     join service message for that join. The knob defaults to off.
package bot

import (
	"fmt"
	"sync"
	"time"
)

// joinGateTTL bounds how long one episode's screening record blocks
// re-screening when no leave transition was ever observed (lost update).
const joinGateTTL = 5 * time.Minute

// joinGateCap bounds the gate map (prune-on-insert of expired entries).
const joinGateCap = 8192

// joinGateEntry is one chat+uid episode record.
type joinGateEntry struct {
	screenedAt time.Time
	leftAt     time.Time // set by noteLeft; after screenedAt → re-join
}

// joinScreenGate collapses the twin join updates into one screening.
// Safe for concurrent use (update dispatch runs on BOT_UPDATE_WORKERS
// goroutines).
type joinScreenGate struct {
	mu      sync.Mutex
	entries map[string]joinGateEntry
}

func newJoinScreenGate() *joinScreenGate {
	return &joinScreenGate{entries: map[string]joinGateEntry{}}
}

func joinGateKey(chatID, uid int64) string {
	return fmt.Sprintf("%d:%d", chatID, uid)
}

// admit reports whether this chat+uid starts a fresh join episode. True →
// the caller screens and the record is stamped. False → the same physical
// join is already screened (twin update / replay / no observed leave inside
// the TTL).
func (g *joinScreenGate) admit(chatID, uid int64) bool {
	key := joinGateKey(chatID, uid)
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[key]
	if ok && !e.leftAt.After(e.screenedAt) && now.Sub(e.screenedAt) < joinGateTTL {
		return false // the same join, already screened
	}
	if len(g.entries) >= joinGateCap {
		g.pruneLocked(now)
	}
	g.entries[key] = joinGateEntry{screenedAt: now}
	return true
}

// noteLeft records a chat_member left/kicked/restricted transition: a
// subsequent join of this chat+uid is a fresh episode and re-screens.
func (g *joinScreenGate) noteLeft(chatID, uid int64) {
	key := joinGateKey(chatID, uid)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries[key] = joinGateEntry{leftAt: time.Now()}
}

// clear drops the episode record entirely (verification passed — the episode
// ended; any later join screening is a fresh episode).
func (g *joinScreenGate) clear(chatID, uid int64) {
	key := joinGateKey(chatID, uid)
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, key)
}

// pruneLocked drops stale records (caller holds mu).
func (g *joinScreenGate) pruneLocked(now time.Time) {
	for k, e := range g.entries {
		if now.Sub(e.screenedAt) >= joinGateTTL && now.Sub(e.leftAt) >= joinGateTTL {
			delete(g.entries, k)
		}
	}
}
