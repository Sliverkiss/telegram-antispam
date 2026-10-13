package ai

import (
	"sort"
	"sync"
)

// Stats aggregates AI call counters for /status and /statusz. All fields are
// lock-guarded (the poll loop, verify voters, and the retry worker hit them
// concurrently).
type Stats struct {
	mu sync.Mutex

	modelCalls    map[string]int64 // per-model attempts (every round trip)
	modelOKs      map[string]int64 // per-model successful verdicts
	fallbacks     int64            // chain advanced to a later model
	verifyRounds  int64            // confidence-triggered vote rounds
	verifyFlipped int64            // vote rounds that overrode the primary verdict
	reports       int64            // /report AI reviews
	localFallback int64            // local rule engine final decisions
}

// NewStats returns a zeroed Stats.
func NewStats() *Stats {
	return &Stats{modelCalls: map[string]int64{}, modelOKs: map[string]int64{}}
}

// NoteAttempt records one model round trip.
func (s *Stats) NoteAttempt(model string, ok bool) {
	s.mu.Lock()
	s.modelCalls[model]++
	if ok {
		s.modelOKs[model]++
	}
	s.mu.Unlock()
}

// NoteFallback records one chain fallback (a model was skipped for the next).
func (s *Stats) NoteFallback() { s.inc(&s.fallbacks, 1) }

// NoteVerify records one vote round; flipped marks whether the vote overrode
// the primary verdict.
func (s *Stats) NoteVerify(flipped bool) {
	s.mu.Lock()
	s.verifyRounds++
	if flipped {
		s.verifyFlipped++
	}
	s.mu.Unlock()
}

// NoteReport records one /report AI review.
func (s *Stats) NoteReport() { s.inc(&s.reports, 1) }

// NoteLocalFallback records one local-rule-engine terminal decision.
func (s *Stats) NoteLocalFallback() { s.inc(&s.localFallback, 1) }

func (s *Stats) inc(dst *int64, n int64) {
	s.mu.Lock()
	*dst += n
	s.mu.Unlock()
}

// Snapshot is a consistent copy for rendering.
type StatsSnapshot struct {
	ModelCalls    map[string]int64
	ModelOKs      map[string]int64
	Fallbacks     int64
	VerifyRounds  int64
	VerifyFlipped int64
	Reports       int64
	LocalFallback int64
}

// Snapshot copies the current counters.
func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StatsSnapshot{
		ModelCalls:    copyCounts(s.modelCalls),
		ModelOKs:      copyCounts(s.modelOKs),
		Fallbacks:     s.fallbacks,
		VerifyRounds:  s.verifyRounds,
		VerifyFlipped: s.verifyFlipped,
		Reports:       s.reports,
		LocalFallback: s.localFallback,
	}
}

func copyCounts(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SortedModels returns the model names in stable sort order (for rendering).
func (sn StatsSnapshot) SortedModels() []string {
	names := make([]string, 0, len(sn.ModelCalls))
	for k := range sn.ModelCalls {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
