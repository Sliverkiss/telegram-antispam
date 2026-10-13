package bot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"adbot/internal/telegram"
)

// countingTG wraps fakeTG to count getChatMember calls and script errors.
// It is safe for concurrent use (memberCalls is mutex-guarded); it does NOT
// funnel through fakeTG.GetChatMember so the fake's seq slice stays untouched.
type countingTG struct {
	fakeTG
	mu          sync.Mutex
	memberCalls int
	memberErr   error // returned once when set (then cleared)
}

func newCountingTG() *countingTG {
	return &countingTG{fakeTG: *newFakeTG()}
}

func (c *countingTG) GetChatMember(ctx context.Context, chatID, userID int64) (telegram.ChatMember, error) {
	c.mu.Lock()
	c.memberCalls++
	var err error
	if c.memberErr != nil {
		err = c.memberErr
		c.memberErr = nil
	}
	c.mu.Unlock()
	if err != nil {
		return telegram.ChatMember{}, err
	}
	return c.fakeTG.member[fkey(chatID, userID)], nil
}

// --- §4.5 acceptance: same uid asked 3× → 1 API call --------------------------

func TestAdminCacheSameUIDOneAPICall(t *testing.T) {
	tg := newCountingTG()
	tg.member[fkey(-100, 7)] = telegram.ChatMember{Status: "administrator"}
	b := testBot(t, tg, &fakeAI{}, defaultCfg())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if !b.isAdmin(ctx, -100, 7) {
			t.Fatalf("attempt %d: uid 7 must be admin", i)
		}
	}
	if tg.memberCalls != 1 {
		t.Fatalf("3 asks must hit the API once: calls=%d", tg.memberCalls)
	}
}

// --- TTL expiry forces a re-query ----------------------------------------------

func TestAdminCacheTTLExpiryRequeries(t *testing.T) {
	tg := newCountingTG()
	tg.member[fkey(-100, 7)] = telegram.ChatMember{Status: "administrator"}
	cfg := defaultCfg()
	cfg.AdminCacheTTL = 40 * time.Millisecond
	b := testBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()
	b.isAdmin(ctx, -100, 7)
	b.isAdmin(ctx, -100, 7)
	time.Sleep(60 * time.Millisecond)
	b.isAdmin(ctx, -100, 7)
	if tg.memberCalls != 2 {
		t.Fatalf("expired entry must be re-queried: calls=%d", tg.memberCalls)
	}
}

// --- API errors must never be cached -------------------------------------------

func TestAdminCacheAPIErrorNotCached(t *testing.T) {
	tg := newCountingTG()
	tg.memberErr = errors.New("network down")
	tg.member[fkey(-100, 7)] = telegram.ChatMember{Status: "administrator"}
	b := testBot(t, tg, &fakeAI{}, defaultCfg())
	ctx := context.Background()
	// First ask: API error → false, nothing cached.
	if b.isAdmin(ctx, -100, 7) {
		t.Fatal("API error must degrade to non-admin (fail-safe)")
	}
	// Second ask must re-hit the API (no cached error).
	if !b.isAdmin(ctx, -100, 7) {
		t.Fatal("recovered API must return the real admin status")
	}
	if tg.memberCalls != 2 {
		t.Fatalf("error must not be cached: calls=%d", tg.memberCalls)
	}
}

// --- Capacity cap evicts (approximate LRU: least recently hit) ------------------

func TestAdminCacheCapacityEvicts(t *testing.T) {
	c := newAdminCache(time.Minute, 3)
	c.put(1, 10, "member")
	c.put(1, 11, "member")
	c.put(1, 12, "member")
	// Touch 10 so 11 becomes the least-recently-used entry.
	time.Sleep(2 * time.Millisecond)
	c.get(1, 10)
	c.put(1, 13, "member") // evicts (1,11)
	if c.len() != 3 {
		t.Fatalf("len = %d, want 3", c.len())
	}
	if got := c.get(1, 11); got != "" {
		t.Fatalf("evicted entry must be gone, got %q", got)
	}
	for _, uid := range []int64{10, 12, 13} {
		if got := c.get(1, uid); got != "member" {
			t.Fatalf("uid %d evicted unexpectedly", uid)
		}
	}
}

// --- Concurrent safety under -race ----------------------------------------------

func TestAdminCacheConcurrent(t *testing.T) {
	tg := newCountingTG()
	cfg := defaultCfg()
	cfg.AdminCacheTTL = time.Millisecond
	cfg.AdminCacheMax = 8
	b := testBot(t, tg, &fakeAI{}, cfg)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				uid := int64(g*50 + i%50)
				b.isAdmin(ctx, -100, uid)
			}
		}(g)
	}
	wg.Wait()
	// No deadlock, no panic; entry count stays within the cap.
	if b.adminCache.len() > 8 {
		t.Fatalf("cache exceeded cap: %d", b.adminCache.len())
	}
}
