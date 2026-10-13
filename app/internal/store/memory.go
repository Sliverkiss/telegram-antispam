package store

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Memory is an in-process Store for development and tests. Its data dies with
// the process — the startup warn log makes that explicit. Every recorded entry
// carries a write timestamp (ts) so the janitor can evict entries past their
// TTL and enforce hard capacity caps; the clock is injectable for tests.
type Memory struct {
	mu       sync.RWMutex
	suspects map[int64]entry[Suspect]

	bioSigs  map[string]entry[string] // sha1 → original
	msgSigs  map[string]entry[string]
	tracked  map[trackKey][]int64
	punished int64
	closed   bool

	// simhash near-duplicate indexes for the bio/msg signature libraries and
	// per-day /report usage counters (amend3 round).
	bioSim  *SimhashIndex
	msgSim  *SimhashIndex
	reports map[string]entry[int] // "day:uid" → count
	simDist int

	now     func() time.Time
	sliding bool // bump ts on hit (BOT_RETENTION_SLIDING)
}

// entry wraps a stored value with its write timestamp (for TTL sweeping and
// old-age eviction).
type entry[T any] struct {
	val T
	ts  time.Time
}

type trackKey struct {
	chatID int64
	uid    int64
}

// NewMemory returns an empty Memory store using the real clock.
func NewMemory() *Memory {
	return NewMemoryWithClock(time.Now)
}

// NewMemoryWithClock returns an empty Memory store using the provided clock.
// Tests inject a fake clock to exercise TTL/sliding-expiry deterministically.
func NewMemoryWithClock(now func() time.Time) *Memory {
	return &Memory{
		suspects: map[int64]entry[Suspect]{},
		bioSigs:  map[string]entry[string]{},
		msgSigs:  map[string]entry[string]{},
		tracked:  map[trackKey][]int64{},
		reports:  map[string]entry[int]{},
		bioSim:   NewSimhashIndex(3, 50000),
		msgSim:   NewSimhashIndex(3, 50000),
		simDist:  3,
		now:      now,
	}
}

// SetSimhashDistance configures the near-match hamming distance
// (BOT_SIMHASH_DISTANCE) on both simhash indexes.
func (m *Memory) SetSimhashDistance(dist int) {
	if dist < 0 {
		dist = 3
	}
	m.mu.Lock()
	m.simDist = dist
	m.bioSim.mu.Lock()
	m.bioSim.maxDist = dist
	m.bioSim.mu.Unlock()
	m.msgSim.mu.Lock()
	m.msgSim.maxDist = dist
	m.msgSim.mu.Unlock()
	m.mu.Unlock()
}

// SetSliding toggles sliding expiration: when true (default), a hit refreshes
// the entry's write timestamp so hot signatures are not repeatedly re-judged.
func (m *Memory) SetSliding(on bool) {
	m.mu.Lock()
	m.sliding = on
	m.mu.Unlock()
}

func (m *Memory) clock() time.Time {
	m.mu.RLock()
	now := m.now
	m.mu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

func (m *Memory) AddSuspect(_ context.Context, s Suspect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.JoinedAt.IsZero() {
		s.JoinedAt = m.clockLocked()
	}
	m.suspects[s.UID] = entry[Suspect]{val: s, ts: m.clockLocked()}
	return nil
}

func (m *Memory) RemoveSuspect(_ context.Context, uid int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.suspects, uid)
	return nil
}

func (m *Memory) IsSuspect(_ context.Context, uid int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.suspects[uid]
	if ok {
		m.touchLocked(uid, 0, "")
	}
	return ok, nil
}

func (m *Memory) GetSuspect(_ context.Context, uid int64) (*Suspect, error) {
	m.mu.RLock()
	e, ok := m.suspects[uid]
	m.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	cp := e.val
	return &cp, nil
}

func (m *Memory) SuspectCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.suspects)), nil
}

func (m *Memory) AddBioSignature(_ context.Context, sig, original string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bioSigs[sig] = entry[string]{val: original, ts: m.clockLocked()}
	m.bioSim.Add(sig, "bio", simhashOf(original))
	return nil
}

func (m *Memory) HasBioSignature(_ context.Context, sig string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.bioSigs[sig]
	if ok {
		m.touchLocked(0, 0, sig)
	}
	return ok, nil
}

func (m *Memory) AddMessageSignature(_ context.Context, sig, original string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgSigs[sig] = entry[string]{val: original, ts: m.clockLocked()}
	m.msgSim.Add(sig, "msg", simhashOf(original))
	return nil
}

func (m *Memory) HasMessageSignature(_ context.Context, sig string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.msgSigs[sig]
	if ok {
		m.touchLocked(0, 0, sig)
	}
	return ok, nil
}

func (m *Memory) BioSigCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.bioSigs)), nil
}

func (m *Memory) MsgSigCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.msgSigs)), nil
}

func (m *Memory) BioSignatures(_ context.Context) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.bioSigs))
	for k, e := range m.bioSigs {
		out[k] = e.val
	}
	return out, nil
}

func (m *Memory) MessageSignatures(_ context.Context) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.msgSigs))
	for k, e := range m.msgSigs {
		out[k] = e.val
	}
	return out, nil
}

func (m *Memory) RemoveSignature(_ context.Context, kind, sig string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var src map[string]entry[string]
	if kind == "bio" {
		src = m.bioSigs
	} else {
		src = m.msgSigs
	}
	if _, ok := src[sig]; !ok {
		return false, nil
	}
	delete(src, sig)
	return true, nil
}

func (m *Memory) TrackMessage(_ context.Context, chatID, uid, messageID int64, max int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := trackKey{chatID: chatID, uid: uid}
	list := m.tracked[k]
	list = append(list, messageID)
	if max > 0 && len(list) > max {
		list = append([]int64(nil), list[len(list)-max:]...)
	}
	m.tracked[k] = list
	return nil
}

func (m *Memory) TrackedMessages(_ context.Context, chatID, uid int64) ([]int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.tracked[trackKey{chatID: chatID, uid: uid}]
	out := make([]int64, len(list))
	copy(out, list)
	return out, nil
}

func (m *Memory) ClearTrackedMessages(_ context.Context, chatID, uid int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tracked, trackKey{chatID: chatID, uid: uid})
	return nil
}

func (m *Memory) RecordPunishment(_ context.Context, p Punishment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.At.IsZero() {
		p.At = m.clockLocked()
	}
	m.punished++
	return nil
}

func (m *Memory) Punished(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.punished, nil
}

func (m *Memory) Ping(_ context.Context) error { return nil }

func (m *Memory) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

// Sweep evicts TTL-expired records and enforces the suspect/signature capacity
// caps, returning per-category drop counts. It is the janitor's entry point.
// TTL fields are already resolved (0 = never expire that category).
func (m *Memory) Sweep(_ context.Context, so SweepOptions) (SweepResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res SweepResult
	now := so.Now
	if now.IsZero() {
		now = m.clockLocked()
	}

	for uid, e := range m.suspects {
		if so.SuspectTTL > 0 && now.Sub(e.ts) >= so.SuspectTTL {
			delete(m.suspects, uid)
			res.DroppedSuspects++
		}
	}
	if so.MaxSuspects > 0 && len(m.suspects) > so.MaxSuspects {
		evicted := m.evictSuspectsLocked(so.MaxSuspects)
		res.EvictedSuspects = evicted
		res.DroppedSuspects += evicted
	}

	bioDropped := sweepSigMap(m.bioSigs, so.SignatureTTL, now)
	msgDropped := sweepSigMap(m.msgSigs, so.SignatureTTL, now)
	res.DroppedSignatures = bioDropped + msgDropped

	// simhash indexes and report keys share the signature/suspect TTLs.
	// (SimhashIndex.Sweep takes its own lock — do NOT pre-lock here.)
	simDropped := m.bioSim.Sweep(so.SignatureTTL, now)
	simDropped += m.msgSim.Sweep(so.SignatureTTL, now)
	res.DroppedSignatures += simDropped
	res.DroppedSuspects += m.sweepReports(so.SuspectTTL, now)

	if so.MaxSignatures > 0 {
		total := len(m.bioSigs) + len(m.msgSigs)
		if total > so.MaxSignatures {
			over := total - so.MaxSignatures
			evicted := m.evictSignaturesLocked(over)
			res.EvictedSignatures = evicted
			res.DroppedSignatures += evicted
		}
	}

	res.RemainingTotal = int64(len(m.suspects)) + int64(len(m.bioSigs)) + int64(len(m.msgSigs))
	return res, nil
}

func (m *Memory) evictSuspectsLocked(max int) int64 {
	type kv struct {
		uid int64
		ts  time.Time
	}
	list := make([]kv, 0, len(m.suspects))
	for uid, e := range m.suspects {
		list = append(list, kv{uid, e.ts})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ts.Before(list[j].ts) })
	over := len(list) - max
	for _, item := range list[:over] {
		delete(m.suspects, item.uid)
	}
	return int64(over)
}

func (m *Memory) evictSignaturesLocked(over int) int64 {
	// Evict oldest across both maps until `over` entries are removed.
	type kv struct {
		sig string
		ts  time.Time
	}
	var list []kv
	for sig, e := range m.bioSigs {
		list = append(list, kv{sig, e.ts})
	}
	for sig, e := range m.msgSigs {
		list = append(list, kv{sig, e.ts})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ts.Before(list[j].ts) })
	if over > len(list) {
		over = len(list)
	}
	for _, item := range list[:over] {
		if _, ok := m.bioSigs[item.sig]; ok {
			delete(m.bioSigs, item.sig)
		} else {
			delete(m.msgSigs, item.sig)
		}
	}
	return int64(over)
}

func sweepSigMap(m map[string]entry[string], ttl time.Duration, now time.Time) int64 {
	if ttl <= 0 {
		return 0
	}
	var dropped int64
	for sig, e := range m {
		if now.Sub(e.ts) >= ttl {
			delete(m, sig)
			dropped++
		}
	}
	return dropped
}

// touchLocked refreshes the write timestamp of a hit entry when sliding
// expiration is on. uid>0 targets a suspect; sig!="" targets a signature (the
// two are mutually exclusive in practice).
func (m *Memory) touchLocked(uid int64, _ int64, sig string) {
	if !m.sliding {
		return
	}
	now := m.clockLocked()
	if uid != 0 {
		if e, ok := m.suspects[uid]; ok {
			e.ts = now
			m.suspects[uid] = e
		}
		return
	}
	if sig != "" {
		if e, ok := m.bioSigs[sig]; ok {
			e.ts = now
			m.bioSigs[sig] = e
			return
		}
		if e, ok := m.msgSigs[sig]; ok {
			e.ts = now
			m.msgSigs[sig] = e
		}
	}
}

// clockLocked returns the current time under the write lock (caller holds m.mu).
func (m *Memory) clockLocked() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// ---------------------------------------------------------------------------
// Simhash near-duplicate search (Signaturer implementation)
// ---------------------------------------------------------------------------

// simhashOf computes the 64-bit simhash of a stored original; normalize
// (applied inside) puts it in the same domain as the search side.
func simhashOf(s string) uint64 {
	return storeSimhash(s)
}

// SearchBioSignature returns the exact-or-near (simhash ≤ maxDist) bio
// signature for normalized text. A hit refreshes sliding TTLs.
func (m *Memory) SearchBioSignature(_ context.Context, normalized string, maxDist int) (string, bool) {
	if normalized == "" {
		return "", false
	}
	m.mu.RLock()
	sig := SignatureOf(normalized)
	_, exact := m.bioSigs[sig]
	m.mu.RUnlock()
	if exact {
		m.HasBioSignature(context.Background(), sig) // refresh sliding ts
		return sig, true
	}
	h := storeSimhash(normalized)
	m.mu.RLock()
	m.bioSim.mu.Lock()
	matchSig := ""
	for i := range m.bioSim.entries {
		if HammingDistanceU64(m.bioSim.entries[i].h, h) <= maxDist {
			m.bioSim.entries[i].ts = m.clockLocked()
			matchSig = m.bioSim.entries[i].sig
			break
		}
	}
	m.bioSim.mu.Unlock()
	m.mu.RUnlock()
	if matchSig != "" {
		m.HasBioSignature(context.Background(), matchSig)
		return matchSig, true
	}
	return "", false
}

// SearchMessageSignature is the message-library equivalent.
func (m *Memory) SearchMessageSignature(_ context.Context, normalized string, maxDist int) (string, bool) {
	if normalized == "" {
		return "", false
	}
	m.mu.RLock()
	sig := SignatureOf(normalized)
	_, exact := m.msgSigs[sig]
	m.mu.RUnlock()
	if exact {
		m.HasMessageSignature(context.Background(), sig)
		return sig, true
	}
	h := storeSimhash(normalized)
	m.mu.RLock()
	m.msgSim.mu.Lock()
	matchSig := ""
	for i := range m.msgSim.entries {
		if HammingDistanceU64(m.msgSim.entries[i].h, h) <= maxDist {
			m.msgSim.entries[i].ts = m.clockLocked()
			matchSig = m.msgSim.entries[i].sig
			break
		}
	}
	m.msgSim.mu.Unlock()
	m.mu.RUnlock()
	if matchSig != "" {
		m.HasMessageSignature(context.Background(), matchSig)
		return matchSig, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// /report usage counters (per-day per-user + message dedup)
// ---------------------------------------------------------------------------

// ReportKey identifies one report usage window: day (YYYY-MM-DD) + reporter.
func ReportKey(day string, reporter int64) string {
	return day + ":" + strconvInt(reporter)
}

// ReportKeyForMessage identifies a reported message for dedup.
func ReportKeyForMessage(chatID, messageID int64) string {
	return "msg:" + strconvInt(chatID) + ":" + strconvInt(messageID)
}

// BumpReportUsage increments the per-day counter for a reporter and returns
// the new count. Keys expire with the suspect TTL sweep.
func (m *Memory) BumpReportUsage(day string, reporter int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := ReportKey(day, reporter)
	e, ok := m.reports[k]
	if !ok {
		e = entry[int]{val: 0, ts: m.clockLocked()}
	}
	e.val++
	e.ts = m.clockLocked()
	m.reports[k] = e
	return e.val
}

// MarkReported records that (chatID, messageID) was already handled so
// duplicate reports are deduped; returns true the first time only.
func (m *Memory) MarkReported(chatID, messageID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := ReportKeyForMessage(chatID, messageID)
	if _, ok := m.reports[k]; ok {
		return false
	}
	m.reports[k] = entry[int]{val: 1, ts: m.clockLocked()}
	return true
}

// sweepReports drops expired report keys (shares the suspect TTL).
func (m *Memory) sweepReports(ttl time.Duration, now time.Time) int64 {
	if ttl <= 0 {
		return 0
	}
	var dropped int64
	for k, e := range m.reports {
		if now.Sub(e.ts) >= ttl {
			delete(m.reports, k)
			dropped++
		}
	}
	return dropped
}

func strconvInt(n int64) string { return strconv.FormatInt(n, 10) }

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
