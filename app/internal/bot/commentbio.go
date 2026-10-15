// The amend12 channel-post comment bio screen (关联频道帖子评论 → 审简介).
//
// Scenario (user's verbatim requirement): a group linked to a channel; when a
// message is a reply under a CHANNEL POST (i.e. it replies to the post's
// automatic forward in the linked discussion group), the replier's bio is
// inspected — a bio carrying a link or @ goes to the AI review (mode="bio",
// the SAME prompt channel the join path uses; prompts/ untouched), an
// AI-condemned bio is punished via the standard b.punishAndClear() chain
// (notification-channel push + in-group AI announcement + bio signature
// write), and the condemned bio enters the flash cache below.
//
// 防刷 (anti-abuse cache, the user's exact wording): ONLY an AI-condemned
// ("审核没过", judged ad) bio enters the cache; a REPEAT of that bio within
// BOT_COMMENT_BIO_FLASH_TTL (default 30m) is kicked DIRECTLY with ZERO AI
// calls. This is NOT rate limiting (the user explicitly rejected that
// framing): the cache exists to skip re-judging a bio the AI already
// condemned, never to save calls at the cost of correctness. "重复" is
// EXACT sha1-of-normalized-bio match — near-duplicate detection stays with
// the bio signature library, which screens every bio anyway (it is checked
// BEFORE this screen, at zero AI cost).
//
// Contract:
//   - Trigger: message in an authorized group whose ReplyToMessage != nil AND
//     ReplyToMessage.IsAutomaticForward == true (the replied-to message is
//     the automatic forward of a linked-channel post — the canonical shape of
//     a channel-post comment). Non-replies, replies to plain messages, and
//     channel automatic forwards themselves (sender_chat posts, From==nil or
//     SenderChat set) never trigger.
//   - Gate: only authorized groups; bot / group-admin / BOT_ADMIN_USERS
//     exempt (inherited from HandleMessage before this screen); a pending
//     join-verification user is consumed by the gate earlier (never both).
//   - Bio fetch: getChat. Failure → WARN + fall through to the ordinary
//     message flow (never punish on a failed lookup).
//   - Bio gate: empty bio, or no link AND no @ → fall through to the
//     ordinary message flow (zero behavior change for those messages).
//   - Flash cache hit (window active) → direct punishAndClear with
//     source="flash_cache", score=100, category from the cached verdict,
//     reason "防刷缓存命中（30 分钟内重复的判广告简介）", ZERO AI calls. The
//     entry stays (fixed window from the write, NOT sliding — the user's
//     wording is "30 分钟内", a fixed lifetime).
//   - Miss → submitJudge(kind="comment", mode="bio", punish=true,
//     writeBio=true): the judge pool runs it per-uid serially ("连续消息进
//     队列一个个处理" — same uid strictly ordered, different uids concurrent,
//     same pool as every other judgment). An ad conclusion runs the standard
//     commitAd chain AND writes the flash cache entry; a non-ad conclusion is
//     zero-disposal and does NOT consume the watchlist slot (bio is an
//     independent screen, amend5 D.2 semantics, same as the join-bio path).
//   - dry-run: judgments run, disposal is the dry-run branch of
//     punishAndClear; the flash cache is NOT written (zero writes).
//   - Restart: cache lost (in-memory, same semantics as join-verify pending;
//     documented as a known limitation).
package bot

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"adbot/internal/normalize"
	"adbot/internal/telegram"
)

// DefaultCommentBioFlashTTL is the fallback 防刷 window
// (BOT_COMMENT_BIO_FLASH_TTL; the user's "30 分钟").
const DefaultCommentBioFlashTTL = 30 * time.Minute

// commentBioFlashEntry is one AI-condemned bio awaiting its repeats.
type commentBioFlashEntry struct {
	category string
	ts       time.Time
}

// commentBioFlash is the bounded 防刷 cache: sha1(normalized bio) → condemned
// verdict metadata + write timestamp. Lazy TTL (expired entries are dropped
// on read/write, never by a background sweeper — the window is short and the
// capacity guard bounds memory on its own). Oldest-first eviction past the
// cap. Safe for concurrent use.
type commentBioFlash struct {
	mu    sync.Mutex
	m     map[string]commentBioFlashEntry
	order []string // insertion order (oldest first) for eviction
	max   int
	ttl   time.Duration

	// clock is injectable so tests time-travel the 30m window (same pattern
	// as joinVerify.scanNow; atomic.Value for -race safety).
	clock atomic.Value // func() time.Time
}

// newCommentBioFlash builds the cache (defensive defaults for ttl/max).
func newCommentBioFlash(ttl time.Duration, max int) *commentBioFlash {
	if ttl <= 0 {
		ttl = DefaultCommentBioFlashTTL
	}
	if max < 1 {
		max = 5000
	}
	c := &commentBioFlash{m: map[string]commentBioFlashEntry{}, order: []string{}, max: max, ttl: ttl}
	c.clock.Store(func() time.Time { return time.Now() })
	return c
}

// now reads the (possibly stubbed) clock.
func (c *commentBioFlash) now() time.Time {
	if f, ok := c.clock.Load().(func() time.Time); ok && f != nil {
		return f()
	}
	return time.Now()
}

// setClock stubs the window clock (tests only).
func (c *commentBioFlash) setClock(f func() time.Time) { c.clock.Store(f) }

// Len returns the live entry count (after a TTL prune of the queried view).
func (c *commentBioFlash) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	return len(c.m)
}

// put records one AI-condemned bio (normalized sha1 key). Fixed window: the
// entry expires ttl after THIS write, never sliding-refreshed. Overwrite of
// an existing key refreshes the window (a fresh AI condemnation restarts the
// clock — the newest judgment owns the entry).
func (c *commentBioFlash) put(key, category string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[key]; !exists {
		c.order = append(c.order, key)
	}
	c.m[key] = commentBioFlashEntry{category: category, ts: c.now()}
	for len(c.order) > c.max {
		drop := c.order[0]
		c.order = c.order[1:]
		delete(c.m, drop)
	}
}

// hit reports whether the condemned bio repeats inside the live window,
// returning the cached category ("" when no live entry).
func (c *commentBioFlash) hit(key string) (category string, ok bool) {
	if key == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, exists := c.m[key]
	if !exists {
		return "", false
	}
	if c.ttl > 0 && c.now().Sub(e.ts) >= c.ttl {
		// Window expired: lazy-drop, the bio goes back to the AI review.
		delete(c.m, key)
		c.removeOrder(key)
		return "", false
	}
	return e.category, true
}

// removeOrder drops key from the eviction order slice (caller holds mu).
func (c *commentBioFlash) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// pruneLocked drops every expired entry (caller holds mu).
func (c *commentBioFlash) pruneLocked() {
	if c.ttl <= 0 {
		return
	}
	now := c.now()
	kept := c.order[:0]
	for _, k := range c.order {
		if e, ok := c.m[k]; ok && now.Sub(e.ts) >= c.ttl {
			delete(c.m, k)
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
}

// ---------------------------------------------------------------------------
// Trigger + bio predicates (pure; unit-testable without a TG fake)
// ---------------------------------------------------------------------------

// isChannelPostComment reports whether m is a comment under a channel post:
// a reply whose replied-to message is the automatic forward of a linked
// channel's post into the discussion group (ReplyToMessage.IsAutomaticForward
// — the canonical Bot API shape for "这条消息在频道帖子下面回复").
func isChannelPostComment(m telegram.Message) bool {
	return m.ReplyToMessage != nil && m.ReplyToMessage.IsAutomaticForward
}

// bioCarriesContact reports whether the bio contains a link or an @
// (username/mention handle): the user's gate for sending the bio to the AI
// review. URL shapes (http/https scheme or a bare domain with a dot) and any
// '@' both count; a plain bio without either falls through untouched.
func bioCarriesContact(bio string) bool {
	if strings.TrimSpace(bio) == "" {
		return false
	}
	if strings.Contains(bio, "@") {
		return true
	}
	return bioHasLink(bio)
}

// bioHasLink detects http(s):// URLs and bare domain tokens ("t.me/xx",
// "example.com", "www.x.com/y") case-insensitively.
func bioHasLink(bio string) bool {
	fields := strings.Fields(bio)
	for _, f := range fields {
		low := strings.ToLower(f)
		if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
			return true
		}
		trimmed := low
		for _, prefix := range []string{"www.", "http://", "https://"} {
			trimmed = strings.TrimPrefix(trimmed, prefix)
		}
		if i := strings.IndexByte(trimmed, '.'); i > 0 && i < len(trimmed)-1 {
			// Require the token to look like host[.tld][/path]: the dot must
			// be followed by more than nothing and the token must not be a
			// plain number ("3.5" style floats are not links).
			rest := trimmed[i+1:]
			if rest == "" {
				continue
			}
			// First path/segment char after the dot must be a letter or digit
			// and the dot must sit inside alphanumeric surroundings.
			if isHostByte(trimmed[i-1]) && isHostByte(rest[0]) {
				// Exclude "数字.数字" (numeric pairs like 3.14 — versions).
				if allDigits(trimmed[:i]) && allDigits(rest) {
					continue
				}
				return true
			}
		}
	}
	return false
}

func isHostByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-'
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// flashKeyOf derives the 防刷 cache key: sha1 of the NORMALIZED bio (the same
// canonical form the signature library uses, so fullwidth/zero-width/emoji
// obfuscation cannot dodge the "重复" match).
func flashKeyOf(bio string) string {
	return normalize.Signature(bio)
}

// ---------------------------------------------------------------------------
// The screen entry point (called from HandleMessage)
// ---------------------------------------------------------------------------

// commentBioReason is the disposition-notice reason for a flash-cache hit.
const commentBioReason = "防刷缓存命中：30 分钟内重复的判广告简介，直接踢出（零 AI）"

// screenChannelPostComment runs the amend12 screen on one group message. It
// returns true when the message was CONSUMED by this screen (the caller must
// NOT continue toward the ordinary message flow) and false when the message
// is out of scope (not a channel-post comment / no bio / bio without link
// or @ / switch off) — in which case the ordinary flow proceeds unchanged.
func (b *Bot) screenChannelPostComment(ctx context.Context, m telegram.Message) bool {
	if !b.Cfg.CommentBioScreen {
		return false
	}
	if !isChannelPostComment(m) {
		return false
	}
	// The channel's own automatic forward carries no human From (or a
	// sender_chat) — the earlier From/SenderChat guards in HandleMessage
	// already return before here; nothing else to exclude.
	chatID, uid := m.Chat.ID, m.From.ID
	b.commentTriggered.Add(1)
	b.Log.Info("comment_bio_triggered",
		"chat", chatID,
		"uid", uid,
		"message_id", m.MessageID,
		"summary", "频道帖子评论：检查评论者简介")

	bio, err := b.bioOf(ctx, uid)
	if err != nil {
		// Failed lookup → fall through to the ordinary flow (never punish on
		// a failed getChat; the message itself may still be screened there).
		b.Log.Warn("comment_bio_getchat_error",
			"chat", chatID, "uid", uid, "err", err.Error(),
			"summary", "简介查询失败：该消息回落普通判定流程")
		return false
	}
	if !bioCarriesContact(bio) {
		// 空 / 无链接无 @：普通成员零变化，回落原流程。
		b.Log.Debug("comment_bio_skipped",
			"chat", chatID, "uid", uid,
			"has_bio", strings.TrimSpace(bio) != "",
			"summary", "简介为空或无链接/无@：回落普通流程")
		return false
	}

	// Flash cache (防刷): a repeat inside the window is kicked directly.
	key := flashKeyOf(bio)
	if b.commentFlash != nil {
		if category, ok := b.commentFlash.hit(key); ok {
			b.commentFlashHits.Add(1)
			b.Log.Info("comment_bio_flash_hit",
				"chat", chatID,
				"uid", uid,
				"message_id", m.MessageID,
				"category", category,
				"summary", "防刷命中：窗口内重复的判广告简介，直接踢、零 AI")
			b.punishAndClear(ctx, punishInfo{
				chatID:    chatID,
				uid:       uid,
				username:  m.From.Username,
				firstName: m.From.FirstName,
				msgID:     m.MessageID,
				text:      bio,
			}, b.Cfg.Action, category, commentBioReason,
				notifyDetails{source: "flash_cache", score: 100})
			return true
		}
	}

	// Fresh condemned-bio candidate → the AI bio review (mode="bio", same
	// prompt channel as the join path; prompts/ untouched). Async: per-uid
	// serial via the judge pool ("连续消息进队列一个个处理"). bio-resolve
	// (用户原话"简介有@的…提取对应简介喂给bot") enriches the judged text with
	// the @ targets' own bios as 【相关简介】 blocks; the flash-cache key and
	// signature text stay the ORIGINAL bio (sigText) — the 防刷 "重复" match
	// must never depend on a third party's channel bio.
	b.submitJudge(ctx, judgmentInput{
		key:        commentKey(uid, m.MessageID),
		kind:       "comment",
		chatID:     chatID,
		messageID:  m.MessageID,
		uid:        uid,
		username:   m.From.Username,
		firstName:  m.From.FirstName,
		text:       b.resolveBioText(ctx, bio),
		sigText:    bio,
		mode:       "bio",
		punish:     true,
		writeBio:   true,
		enqueuedAt: b.now(),
	})
	return true
}

// bioOf fetches one user's bio via getChat (Bot API exposes bio only there).
func (b *Bot) bioOf(ctx context.Context, uid int64) (string, error) {
	info, err := b.TG.GetChat(ctx, uid)
	if err != nil {
		return "", err
	}
	return info.Bio, nil
}

// commentKey serializes one user's comment judgments per message.
func commentKey(uid, messageID int64) string {
	return "comment:" + strconv.FormatInt(uid, 10) + ":" + strconv.FormatInt(messageID, 10)
}

// commentBioFlashStore is the write hook from the ad-verdict lanes: an
// AI-condemned bio enters the 防刷 cache (dry-run excluded — zero writes).
func (b *Bot) commentBioFlashStore(bio, category string) {
	if b.Cfg.DryRun || b.commentFlash == nil {
		return
	}
	key := flashKeyOf(bio)
	if key == "" {
		return
	}
	b.commentFlash.put(key, category)
}
