// Concurrent flash-vote verification (TASK_antispam_amend3.md A.4 / amend2 §5).
//
// When the primary chain says is_ad=true but confidence < BOT_AI_MIN_CONFIDENCE,
// BOT_AI_VERIFY_VOTES requests fan out concurrently on the verify models (each
// with its own BOT_AI_VERIFY_TIMEOUT). A failed/timed-out lane abstains (it is
// not a "no" vote). votes_ad >= BOT_AI_VERIFY_MAJORITY commits the punishment;
// anything else (short of majority, tie, too many abstentions) leaves the item
// unresolved so the caller re-enqueues it for another round after the fixed
// retry interval.
package ai

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// VerifyConfig drives one vote round.
type VerifyConfig struct {
	Votes    int           // lanes (default 3)
	Models   []string      // lane models (lane i takes model[i % len])
	Majority int           // ad votes needed to punish (default 2)
	Timeout  time.Duration // per-lane timeout (default 45s)
}

// VoteLane is one lane's outcome.
type VoteLane struct {
	Model      string
	IsAd       bool
	Confidence float64
	MS         int64
	Err        string // non-empty = abstention
}

// VerifyResult is one round's tally.
type VerifyResult struct {
	Lanes       []VoteLane
	VotesTotal  int
	VotesAd     int
	VotesNo     int
	Abstentions int
	Final       string // "ad" | "not_ad" | "unresolved"
	TotalMS     int64
}

// Verify runs one concurrent vote round. lanes model round trips are recorded
// on stats. Content is the same input as the primary classification.
func Verify(ctx context.Context, primary *Client, cfg VerifyConfig, stats *Stats, content Content) VerifyResult {
	if cfg.Votes <= 0 {
		cfg.Votes = 3
	}
	if cfg.Majority <= 0 {
		cfg.Majority = 2
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"mimo-v2.6-flash"}
	}

	start := time.Now()
	lanes := make([]VoteLane, cfg.Votes)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Votes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := cfg.Models[i%len(cfg.Models)]
			lane := VoteLane{Model: model}
			t0 := time.Now()
			ctxT, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
			// A lane is pinned to ONE model (Classify on a single-model chain):
			// lanes must not walk the primary chain, or a failing lane would
			// silently succeed on some other model.
			res, ok := primary.ClassifyWithModel(ctxT, model, Content{Text: content.Text, ImageURL: content.ImageURL}, "message")
			lane.MS = time.Since(t0).Milliseconds()
			if stats != nil {
				stats.NoteAttempt(model, ok)
			}
			if !ok {
				// Failed or timed-out lane: abstention, never a "no" vote.
				lane.Err = "undecided"
			} else {
				lane.IsAd = res.Verdict.IsAd
				lane.Confidence = res.Verdict.Confidence
			}
			lanes[i] = lane
		}(i)
	}
	wg.Wait()

	result := VerifyResult{Lanes: lanes, TotalMS: time.Since(start).Milliseconds()}
	result.VotesTotal = len(lanes)
	for _, lane := range lanes {
		if lane.Err != "" {
			result.Abstentions++
		} else if lane.IsAd {
			result.VotesAd++
		} else {
			result.VotesNo++
		}
	}
	if result.VotesAd >= cfg.Majority {
		result.Final = "ad"
	} else if result.VotesNo+result.Abstentions == result.VotesTotal && result.VotesNo >= cfg.Majority {
		// Unanimous-enough "not ad" among the non-abstaining lanes.
		result.Final = "not_ad"
	} else {
		// Short of majority, tie, or too many abstentions to decide: the item
		// stays unresolved (caller re-enqueues for another round).
		result.Final = "unresolved"
	}
	return result
}

// Format renders a vote round for logs and /check.
func (r VerifyResult) Format() string {
	var parts []string
	for _, lane := range r.Lanes {
		if lane.Err != "" {
			parts = append(parts, fmt.Sprintf("%s:abstain(%dms)", lane.Model, lane.MS))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%v/%.2f(%dms)", lane.Model, lane.IsAd, lane.Confidence, lane.MS))
	}
	return fmt.Sprintf("votes=%d ad=%d abstain=%d final=%s [%s] %dms",
		r.VotesTotal, r.VotesAd, r.Abstentions, r.Final, joinComma(parts), r.TotalMS)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
