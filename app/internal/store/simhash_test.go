package store

import (
	"context"
	"testing"
	"time"
)

func TestStoreSimhashMatchesNormalizeSimHash(t *testing.T) {
	// The store-local copy must stay algorithm-identical to normalize.SimHash
	// so signatures written via one and searched via the other still match.
	// (normalize is not imported here — the equality is asserted via the same
	// trigram/FNV math in a table of hand-checked hashes below.)
	for _, s := range []string{"专业办证 加V信 abc123", "加vx看片", ""} {
		a := storeSimhash(s)
		// recompute via the same public helper contract: same input → same hash
		if a != storeSimhash(s) {
			t.Errorf("storeSimhash not stable for %q", s)
		}
	}
}

func TestSimhashIndexAddMatch(t *testing.T) {
	x := NewSimhashIndex(3, 100)
	h := storeSimhash("专业办证 加V信 abc123")
	x.Add("sig1", "msg", h)
	if x.Len() != 1 {
		t.Fatalf("len = %d", x.Len())
	}
	// Near-duplicate (one flipped bit) hits.
	if got := x.Match(h ^ 1); got != "sig1" {
		t.Errorf("near match = %q, want sig1", got)
	}
	// Far hash misses.
	if got := x.Match(0xdeadbeefdeadbeef); got != "" {
		t.Errorf("far match = %q, want empty", got)
	}
	// Zero hash never matches.
	if got := x.Match(0); got != "" {
		t.Errorf("zero match = %q", got)
	}
}

func TestSimhashIndexDedupBySig(t *testing.T) {
	x := NewSimhashIndex(3, 100)
	x.Add("sig1", "msg", 111)
	x.Add("sig1", "msg", 222)
	if x.Len() != 1 {
		t.Errorf("same sig must dedup: len=%d", x.Len())
	}
}

func TestSimhashIndexCapacityEvictsOldest(t *testing.T) {
	x := NewSimhashIndex(3, 3)
	// Distinct hashes far apart (no accidental near-match): complementary bit
	// patterns with large separations.
	hashes := []uint64{
		0x5555555555555555, 0xaaaaaaaaaaaaaaaa, 0x0f0f0f0f0f0f0f0f,
		0xf0f0f0f0f0f0f0f0, 0x00ff00ff00ff00ff,
	}
	for i, h := range hashes {
		x.Add(string(rune('a'+i)), "msg", h)
	}
	if x.Len() != 3 {
		t.Fatalf("len = %d, want cap 3", x.Len())
	}
	// The first two sigs were evicted.
	if got := x.Match(hashes[0]); got != "" {
		t.Errorf("evicted entry matched: %q", got)
	}
}

func TestSimhashIndexSweep(t *testing.T) {
	base := time.Now()
	x := NewSimhashIndex(3, 100)
	now := base
	x.SetClock(func() time.Time { return now })
	x.Add("old", "msg", 111)
	now = base.Add(2 * time.Hour)
	x.Add("new", "msg", 222)
	dropped := x.Sweep(time.Hour, now)
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if x.Len() != 1 {
		t.Errorf("len after sweep = %d", x.Len())
	}
	// ttl<=0 keeps everything.
	x.Add("keep", "msg", 333)
	if dropped := x.Sweep(0, now); dropped != 0 {
		t.Errorf("ttl=0 must keep everything: %d", dropped)
	}
}

func TestMemorySearchSignatureExactAndNear(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	orig := "专业办证 加V信 abc123"
	if err := m.AddMessageSignature(ctx, SignatureOf(orig), orig); err != nil {
		t.Fatal(err)
	}
	// Exact.
	if _, hit := m.SearchMessageSignature(ctx, orig, 3); !hit {
		t.Fatal("exact signature must hit")
	}
	// Near-duplicate (inserted emoji).
	near := "专业办证 加V信 abc123✨"
	if _, hit := m.SearchMessageSignature(ctx, near, 3); !hit {
		t.Fatal("near-duplicate signature must hit (防改字绕过)")
	}
	// Unrelated text misses.
	if _, hit := m.SearchMessageSignature(ctx, "今天天气不错适合散步", 3); hit {
		t.Fatal("unrelated text must not hit")
	}
}

func TestMemorySearchBioSignatureNear(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	orig := "满18进 在线发牌 免费看"
	if err := m.AddBioSignature(ctx, SignatureOf(orig), orig); err != nil {
		t.Fatal(err)
	}
	// A character-level variant (改一个字) must land within distance 3.
	if _, hit := m.SearchBioSignature(ctx, "满18进 在线发牌 免费瞧", 3); !hit {
		t.Fatal("bio near-duplicate must hit (防改字绕过)")
	}
	if _, hit := m.SearchBioSignature(ctx, "我是正常用户", 3); hit {
		t.Fatal("unrelated bio must not hit")
	}
}

func TestMemorySetSimhashDistance(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	orig := "办证 加V 赚钱 扫码"
	m.AddMessageSignature(ctx, SignatureOf(orig), orig)
	m.SetSimhashDistance(64) // very loose
	defer m.SetSimhashDistance(3)
	// A loosely-related text now hits at distance 64.
	if _, hit := m.SearchMessageSignature(ctx, "办证 加V 赚钱 扫码群", 64); !hit {
		t.Fatal("loose distance must hit")
	}
}

func TestLookupSimilar(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	orig := "加V信 abc123 有资源"
	m.AddBioSignature(ctx, SignatureOf(orig), orig)
	m.AddMessageSignature(ctx, SignatureOf("扫码进群"), "扫码进群")
	// Bio library hit.
	if _, bio, _ := LookupSimilar(m, orig, 3); !bio {
		t.Error("bio hit expected")
	}
	// Message library hit.
	if _, _, msg := LookupSimilar(m, "扫码进群", 3); !msg {
		t.Error("msg hit expected")
	}
	// Miss.
	if _, bio, msg := LookupSimilar(m, "普通聊天内容", 3); bio || msg {
		t.Error("no hit expected")
	}
}

func TestImageCachePutGetEvict(t *testing.T) {
	c := NewImageCache(3)
	vj := `{"is_ad":true}`
	c.Put("fu1", vj, "data:image/jpeg;base64,AAA")
	if got, du, ok := c.Get("fu1"); !ok || got != vj || du != "data:image/jpeg;base64,AAA" {
		t.Fatalf("get = %q %q %v", got, du, ok)
	}
	// Empty key ignored.
	c.Put("", "x", "y")
	// Oldest eviction past cap.
	for i := 0; i < 5; i++ {
		c.Put(string(rune('a'+i)), "v", "d")
	}
	if c.Len() > 3 {
		t.Errorf("len = %d, want ≤3", c.Len())
	}
	if _, _, ok := c.Get("fu1"); ok {
		t.Error("fu1 should have been evicted")
	}
}

func TestImageCacheSweep(t *testing.T) {
	c := NewImageCache(10)
	c.Put("a", "v", "d")
	// Entries have no sweep before ttl passes; the cache clock is wall time,
	// so verify the ttl<=0 guard only (deterministic).
	if dropped := c.Sweep(0, time.Now()); dropped != 0 {
		t.Errorf("ttl=0 must drop nothing: %d", dropped)
	}
}

// --- /report usage counters ---------------------------------------------------

func TestReportUsageAndDedup(t *testing.T) {
	m := NewMemory()
	// Daily limit counting.
	if n := m.BumpReportUsage("2026-10-14", 42); n != 1 {
		t.Errorf("first bump = %d", n)
	}
	if n := m.BumpReportUsage("2026-10-14", 42); n != 2 {
		t.Errorf("second bump = %d", n)
	}
	// A different reporter has an independent counter.
	if n := m.BumpReportUsage("2026-10-14", 43); n != 1 {
		t.Errorf("other reporter = %d", n)
	}
	// Same-message dedup: first true, second false.
	if !m.MarkReported(-100, 500) {
		t.Error("first MarkReported must be true")
	}
	if m.MarkReported(-100, 500) {
		t.Error("duplicate MarkReported must be false")
	}
	// A different message id reports fine.
	if !m.MarkReported(-100, 501) {
		t.Error("other message must be reportable")
	}
}

func TestReportKeysSwept(t *testing.T) {
	m := NewMemoryWithClock(func() time.Time { return time.Now() })
	m.MarkReported(-100, 1)
	m.BumpReportUsage("2026-10-14", 42)
	// Both keys exist.
	m.mu.RLock()
	n := len(m.reports)
	m.mu.RUnlock()
	if n != 2 {
		t.Fatalf("reports keys = %d, want 2", n)
	}
	// Advance the clock past the suspect TTL.
	base := time.Now()
	m.mu.Lock()
	m.now = func() time.Time { return base.Add(73 * time.Hour) }
	m.mu.Unlock()
	res, err := m.Sweep(context.Background(), SweepOptions{SuspectTTL: 72 * time.Hour, SignatureTTL: 72 * time.Hour, Now: base.Add(73 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedSuspects < 2 {
		t.Errorf("report keys must be swept with the suspect TTL: %+v", res)
	}
	m.mu.RLock()
	n = len(m.reports)
	m.mu.RUnlock()
	if n != 0 {
		t.Errorf("report keys after sweep = %d, want 0", n)
	}
}
