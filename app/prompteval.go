package main

// -prompteval: runs the labeled sample set (testdata/prompt_eval.jsonl)
// against the real gateway — the ONLY code path allowed to make outbound AI
// calls, and only when the flag is passed explicitly. It reports per-model
// accuracy / FN / FP / parse-failure rate / median latency plus a per-sample
// misjudgment table, so prompt tuning becomes a data loop:
// edit prompt → bump PromptVersion → rerun → compare FN.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
)

// promptEvalSample is one labeled row of testdata/prompt_eval.jsonl.
type promptEvalSample struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Want bool   `json:"want"`
	Tag  string `json:"tag"`
}

type promptEvalRow struct {
	sample    promptEvalSample
	model     string
	gotAd     bool
	ok        bool // call + parse succeeded
	errCode   string
	verdict   ai.Verdict
	latencyMS int64
}

func runPromptEval(cfg config.Config, models []string, samplePath string) int {
	if cfg.AIKey == "" {
		fmt.Println("[FAIL] prompteval: BOT_AI_API_KEY is not set")
		return 1
	}
	samples, err := loadPromptEvalSamples(samplePath)
	if err != nil {
		fmt.Printf("[FAIL] prompteval: load samples: %v\n", err)
		return 1
	}
	fmt.Printf("=== prompteval: %d samples, %d models, prompt %s ===\n",
		len(samples), len(models), ai.PromptVersion)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	exit := 0
	for _, model := range models {
		client := ai.NewClient(ai.ClientConfig{
			BaseURL: cfg.AIBaseURL,
			APIKey:  cfg.AIKey,
			Models:  []string{model}, // eval one model at a time — no chain fallback
			Timeout: cfg.AITimeout,
		})
		rows := make([]promptEvalRow, 0, len(samples))
		for _, s := range samples {
			row := promptEvalRow{sample: s, model: model}
			t0 := time.Now()
			res, ok := client.ClassifyText(ctx, s.Text, "message")
			row.latencyMS = time.Since(t0).Milliseconds()
			row.ok = ok
			if !ok {
				row.errCode = "chain_failed"
			} else {
				row.verdict = res.Verdict
				row.gotAd = res.Verdict.IsAd
			}
			rows = append(rows, row)
		}
		if !printPromptEvalModel(model, rows) {
			exit = 1
		}
	}
	fmt.Println("=== prompteval done ===")
	return exit
}

func printPromptEvalModel(model string, rows []promptEvalRow) bool {
	var tp, tn, fp, fn, parseFail int
	latencies := make([]int64, 0, len(rows))
	var misjudged []promptEvalRow
	for _, r := range rows {
		if !r.ok {
			parseFail++
			misjudged = append(misjudged, r)
			continue
		}
		latencies = append(latencies, r.latencyMS)
		switch {
		case r.sample.Want && r.gotAd:
			tp++
		case !r.sample.Want && !r.gotAd:
			tn++
		case !r.sample.Want && r.gotAd:
			fp++
			misjudged = append(misjudged, r)
		case r.sample.Want && !r.gotAd:
			fn++
			misjudged = append(misjudged, r)
		}
	}
	decided := tp + tn + fp + fn
	accuracy := 0.0
	if decided > 0 {
		accuracy = float64(tp+tn) / float64(decided)
	}
	fmt.Printf("\n--- model %s (prompt %s) ---\n", model, ai.PromptVersion)
	fmt.Printf("samples=%d decided=%d parse_failed=%d\n", len(rows), decided, parseFail)
	fmt.Printf("accuracy=%.2f  TP=%d TN=%d FP=%d FN=%d\n", accuracy, tp, tn, fp, fn)
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		fmt.Printf("median_latency_ms=%d max_latency_ms=%d\n",
			latencies[len(latencies)/2], latencies[len(latencies)-1])
	}
	if fn > 0 {
		fmt.Printf("!! FN>0: 漏杀是最严重的失败（调提示词必杀清单，bump 版本后重跑）\n")
	}
	for _, r := range misjudged {
		outcome := "FP(误伤)"
		if !r.ok {
			outcome = "PARSE_FAIL(违反输出契约)"
		} else if r.sample.Want {
			outcome = "FN(漏杀)"
		}
		fmt.Printf("  %-8s id=%s tag=%s want=%v got=%v conf=%.2f cat=%q reason=%q err=%s\n",
			outcome, r.sample.ID, r.sample.Tag, r.sample.Want, r.gotAd,
			r.verdict.Confidence, r.verdict.ControlledCategory(), r.verdict.Reason, r.errCode)
	}
	return fn == 0 && fp == 0 && parseFail == 0
}

func loadPromptEvalSamples(path string) ([]promptEvalSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []promptEvalSample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		t := sc.Text()
		if t == "" {
			continue
		}
		var s promptEvalSample
		if err := json.Unmarshal([]byte(t), &s); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if s.ID == "" || s.Text == "" {
			return nil, fmt.Errorf("line %d: id/text missing", line)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// promptEvalFlag registers the -prompteval flag family and returns whether it
// was requested (main handles the "explicit outbound" semantics).
type promptEvalFlags struct {
	enabled bool
	models  string
	samples string
}

func registerPromptEvalFlags(fs *flag.FlagSet) *promptEvalFlags {
	p := &promptEvalFlags{}
	fs.BoolVar(&p.enabled, "prompteval", false, "run the labeled prompt evaluation against the REAL gateway (outbound calls!)")
	fs.StringVar(&p.models, "prompteval.models", "", "comma-separated models to evaluate (default: all three chain models)")
	fs.StringVar(&p.samples, "prompteval.samples", "testdata/prompt_eval.jsonl", "labeled sample file (jsonl)")
	return p
}
