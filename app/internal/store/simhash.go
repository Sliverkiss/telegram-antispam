package store

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"sync"
	"time"

	"adbot/internal/normalize"
)

// storeSimhash hashes into the SAME domain as the search side: normalize
// (which normalize.SimHash applies internally) is run on the stored original
// too, so a raw-text write and a normalized-text search always align. (The
// earlier forked copy hashed raw text at write time and missed every
// near-match searched with normalized text — see the audit report.)
func storeSimhash(s string) uint64 {
	return normalize.SimHash(s)
}

// SignatureOf returns the sha1 hex of already-normalized text (the caller
// normalized before searching; we hash it once more for the exact-match key).
func SignatureOf(normalized string) string {
	sum := sha1.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// SimhashIndex is a bounded in-memory simhash index for near-duplicate
// signature matching (BOT_SIMHASH_DISTANCE). It is embedded by the Memory
// store; the Redis store keeps its own SADD/SMEMBERS flow (see redis.go) and
// does not use this type. The index carries a maxDist and its own capacity
// cap with oldest-first eviction (no unbounded growth).
type SimhashIndex struct {
	mu      sync.Mutex
	entries []simhashEntry // ordered by insertion time (oldest first)
	maxDist int
	maxLen  int
	now     func() time.Time
}

type simhashEntry struct {
	sig  string // sha1 signature key this hash belongs to (bio or msg)
	h    uint64
	kind string // "bio" or "msg"
	ts   time.Time
}

// NewSimhashIndex builds an index with the given hamming distance and cap.
func NewSimhashIndex(maxDist, maxLen int) *SimhashIndex {
	if maxDist < 0 {
		maxDist = 3
	}
	if maxLen <= 0 {
		maxLen = 50000
	}
	return &SimhashIndex{maxDist: maxDist, maxLen: maxLen, now: time.Now}
}

// SetClock injects a fake clock (tests).
func (x *SimhashIndex) SetClock(now func() time.Time) {
	x.mu.Lock()
	x.now = now
	x.mu.Unlock()
}

// Add records one hash under its signature key and kind, evicting the oldest
// entries past the cap.
func (x *SimhashIndex) Add(sig, kind string, h uint64) {
	if h == 0 || sig == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	// Dedup by sig: replace an existing entry's hash/ts.
	for i := range x.entries {
		if x.entries[i].sig == sig {
			x.entries[i].h = h
			x.entries[i].ts = x.now()
			return
		}
	}
	x.entries = append(x.entries, simhashEntry{sig: sig, h: h, kind: kind, ts: x.now()})
	if len(x.entries) > x.maxLen {
		x.entries = append(x.entries[:0], x.entries[len(x.entries)-x.maxLen:]...)
	}
}

// Match returns the signature key of a stored hash within maxDist bits of h
// ("" when no near-duplicate exists). A hit refreshes the entry's ts (sliding).
func (x *SimhashIndex) Match(h uint64) string {
	if h == 0 {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := range x.entries {
		if HammingDistanceU64(x.entries[i].h, h) <= x.maxDist {
			x.entries[i].ts = x.now() // sliding refresh
			return x.entries[i].sig
		}
	}
	return ""
}

// Len returns the number of indexed hashes.
func (x *SimhashIndex) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.entries)
}

// Sweep drops entries older than ttl (ttl <= 0 = keep everything) and returns
// how many were dropped; it is called from Memory.Sweep.
func (x *SimhashIndex) Sweep(ttl time.Duration, now time.Time) int64 {
	if ttl <= 0 {
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	kept := x.entries[:0]
	var dropped int64
	for _, e := range x.entries {
		if now.Sub(e.ts) >= ttl {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	x.entries = kept
	return dropped
}

// HammingDistanceU64 is the shared bit-distance helper; delegated to
// normalize so the two can never drift apart.
func HammingDistanceU64(a, b uint64) int {
	return normalize.HammingDistance(a, b)
}

// ---------------------------------------------------------------------------
// Image-verdict cache: same file_unique_id is judged once (file hash dedup).
// ---------------------------------------------------------------------------

// ImageCache caches one verdict per file_unique_id with its own TTL/cap.
type ImageCache struct {
	mu    sync.Mutex
	m     map[string]imageEntry
	max   int
	order []string // insertion order for oldest-first eviction
}

type imageEntry struct {
	fileUniqueID string
	verdictJSON  string // serialized ai.Verdict
	dataURL      string // cached base64 data URL (avoid re-download)
	ts           time.Time
}

// NewImageCache builds a cache with the given capacity.
func NewImageCache(max int) *ImageCache {
	if max <= 0 {
		max = 5000
	}
	return &ImageCache{m: map[string]imageEntry{}, max: max}
}

// Get returns the cached verdict JSON and data URL ("" on miss).
func (c *ImageCache) Get(fileUniqueID string) (verdictJSON, dataURL string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.m[fileUniqueID]
	if !hit {
		return "", "", false
	}
	return e.verdictJSON, e.dataURL, true
}

// Put stores the verdict and data URL for a file, evicting the oldest past cap.
func (c *ImageCache) Put(fileUniqueID, verdictJSON, dataURL string) {
	if fileUniqueID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[fileUniqueID]; !exists {
		c.order = append(c.order, fileUniqueID)
	}
	c.m[fileUniqueID] = imageEntry{fileUniqueID: fileUniqueID, verdictJSON: verdictJSON, dataURL: dataURL, ts: time.Now()}
	if len(c.order) > c.max {
		drop := c.order[:len(c.order)-c.max]
		for _, k := range drop {
			delete(c.m, k)
		}
		c.order = append(c.order[:0], c.order[len(c.order)-c.max:]...)
	}
}

// Sweep drops entries older than ttl; returns the drop count.
func (c *ImageCache) Sweep(ttl time.Duration, now time.Time) int64 {
	if ttl <= 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var dropped int64
	kept := c.order[:0]
	for _, k := range c.order {
		if e, ok := c.m[k]; ok && now.Sub(e.ts) >= ttl {
			delete(c.m, k)
			dropped++
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
	return dropped
}

// Len returns the cache size.
func (c *ImageCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// Store helpers: the extended signature-search contract used by the bot
// (exact sha1 first, then simhash). Implemented as methods on Store through
// the Signaturer interface below so Memory gains them while Redis keeps
// working through its own SMEMBERS-based implementation.

// Signaturer is the extended similarity-search interface (optional upgrade on
// top of Store for stores that support simhash near-lookup).
type Signaturer interface {
	// SearchMessageSignature returns (sig, true) for an exact OR near
	// (simhash <= maxDist) match of the normalized text.
	SearchMessageSignature(ctx context.Context, normalized string, maxDist int) (string, bool)
	// SearchBioSignature is the bio-library equivalent.
	SearchBioSignature(ctx context.Context, normalized string, maxDist int) (string, bool)
}

// LookupSimilar finds a stored signature for normalized text via exact match
// first, then simhash near-match, on any store implementing Signaturer.
// Stores that do not implement it fall back to exact-only (callers keep their
// HasXSignature path).
func LookupSimilar(st Store, normalized string, maxDist int) (sig string, bio, msg bool) {
	if sr, ok := st.(Signaturer); ok && normalized != "" {
		if s, hit := sr.SearchBioSignature(context.Background(), normalized, maxDist); hit {
			return s, true, false
		}
		if s, hit := sr.SearchMessageSignature(context.Background(), normalized, maxDist); hit {
			return s, false, true
		}
	}
	return "", false, false
}
