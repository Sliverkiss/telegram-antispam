// TTL cache for getChatMember results (TASK_antispam_amend4.md §4.5).
//
// Without it every group message cost one Telegram API call (378-person group
// at normal traffic overruns the ~30 req/s budget and the calls are pure
// waste — admin status barely changes). Cache rules:
//   - only DETERMINISTIC results are cached (creator/administrator/member/...);
//     API errors are never cached (a network blip must not freeze a true
//     admin as a plain member, or vice versa);
//   - TTL (BOT_ADMIN_CACHE_TTL, default 5m) and capacity (BOT_ADMIN_CACHE_MAX,
//     default 5000) are enforced; capacity evicts the least-recently-used
//     entry first, ties broken by oldest timestamp (approximate LRU).
package bot

import (
	"context"
	"sync"
	"time"

	"adbot/internal/telegram"
)

// adminCache is a TTL + capacity cache of getChatMember outcomes.
type adminCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[cacheKey]*adminEntry
}

type cacheKey struct {
	chat int64
	uid  int64
}

type adminEntry struct {
	status  string
	expires time.Time
	lastHit time.Time
}

func newAdminCache(ttl time.Duration, max int) *adminCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if max <= 0 {
		max = 5000
	}
	return &adminCache{
		ttl:     ttl,
		max:     max,
		entries: make(map[cacheKey]*adminEntry, 64),
	}
}

// get returns a cached member status ("" on miss or expiry).
func (c *adminCache) get(chat, uid int64) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey{chat, uid}]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(c.entries, cacheKey{chat, uid})
		}
		return ""
	}
	e.lastHit = time.Now()
	return e.status
}

// put stores one DETERMINISTIC status. Erroneous results must never reach
// here (the caller only calls put after a successful API response).
func (c *adminCache) put(chat, uid int64, status string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey{chat, uid}
	if e, ok := c.entries[k]; ok {
		e.status = status
		e.expires = time.Now().Add(c.ttl)
		e.lastHit = time.Now()
		return
	}
	if len(c.entries) >= c.max {
		c.evictOne()
	}
	c.entries[k] = &adminEntry{status: status, expires: time.Now().Add(c.ttl), lastHit: time.Now()}
}

// evictOne drops the least-recently-used entry (mu held).
func (c *adminCache) evictOne() {
	var victim cacheKey
	var oldest time.Time
	first := true
	for k, e := range c.entries {
		if first || e.lastHit.Before(oldest) {
			victim, oldest, first = k, e.lastHit, false
		}
	}
	if !first {
		delete(c.entries, victim)
	}
}

// len is the entry count (for tests).
func (c *adminCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// getChatMember fetches the member status via one API call and caches the
// deterministic result. An API error bypasses the cache entirely (returns the
// error; nothing is written).
func (c *adminCache) getChatMember(ctx context.Context, tg TelegramClient, chatID, userID int64) (telegram.ChatMember, error) {
	m, err := tg.GetChatMember(ctx, chatID, userID)
	if err != nil {
		return telegram.ChatMember{}, err
	}
	c.put(chatID, userID, m.Status)
	return m, nil
}
