package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/telegram"
)

// runSelftest executes the belmo connectivity acceptance checks in order:
// config → storage → AI → Telegram. Each prints [PASS]/[FAIL]; any failure
// makes the process exit non-zero. It only calls getMe (no polling, no 409
// conflict with a running instance). Timeouts are bounded.
func runSelftest(cfg config.Config) {
	ok := true
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fmt.Println("=== adbot selftest ===")

	// 1. Config (already parsed — render the redacted summary).
	fmt.Println("[PASS] config: parsed")
	keys := sortedKeys(cfg.Redacted())
	for _, k := range keys {
		fmt.Printf("        %s=%s\n", k, cfg.Redacted()[k])
	}

	// TTL configuration (explicit requirement for -selftest).
	fmt.Printf("        effective TTL suspect=%s signature=%s sliding=%v\n",
		cfg.RetentionFor("suspect"), cfg.RetentionFor("signature"), cfg.RetentionSliding)
	fmt.Printf("        prompt_version=%s models=%s\n", ai.PromptVersion, fmt.Sprint(cfg.AIEffectiveModels()))

	// 2. Storage: report the chosen backend and its record counts.
	st, backend, err := openStore(cfg, nil)
	if err != nil {
		fmt.Printf("[FAIL] store: %v\n", err)
		ok = false
	} else if backend == "memory" {
		fmt.Println("[WARN] store: memory — 数据不会持久化，重启丢失")
	} else {
		fmt.Printf("[PASS] store: %s\n", backend)
	}
	if st != nil {
		defer st.Close()
		if err := st.Ping(ctx); err != nil {
			fmt.Printf("[FAIL] store: ping: %v\n", err)
			ok = false
		} else {
			suspects, _ := st.SuspectCount(ctx)
			bioSigs, _ := st.BioSigCount(ctx)
			msgSigs, _ := st.MsgSigCount(ctx)
			fmt.Printf("[PASS] store: ping ok · suspects=%d bio_sigs=%d msg_sigs=%d\n", suspects, bioSigs, msgSigs)
		}
	}

	// 3. AI: canary sentence → JSON verdict + latency (full model chain).
	if cfg.AIKey == "" {
		fmt.Println("[FAIL] ai: BOT_AI_API_KEY is not set")
		ok = false
	} else {
		client := ai.NewClient(ai.ClientConfig{
			BaseURL: cfg.AIBaseURL,
			APIKey:  cfg.AIKey,
			Models:  cfg.AIEffectiveModels(),
			Timeout: cfg.AITimeout,
		})
		start := time.Now()
		res, okAI := client.ClassifyText(ctx, "判断这句话是不是广告：专业办理各种证件 加V信 xxx", "message")
		elapsed := time.Since(start)
		if !okAI {
			fmt.Printf("[FAIL] ai: classify failed (chain exhausted, %.0fms)\n", float64(elapsed.Microseconds())/1000)
			ok = false
		} else {
			v := res.Verdict
			fmt.Printf("[PASS] ai: model=%s fallback=%v is_ad=%v category=%s confidence=%.2f reason=%q (%.0fms)\n",
				res.Model, res.Fallback, v.IsAd, v.ControlledCategory(), v.Confidence, v.Reason, float64(elapsed.Microseconds())/1000)
		}
	}

	// 4. Telegram: getMe (single call — safe next to a running poller).
	if cfg.Token == "" {
		fmt.Println("[FAIL] telegram: BOT_TOKEN is not set")
		ok = false
	} else {
		tg, err := telegram.NewClient(cfg.Token)
		if err != nil {
			fmt.Printf("[FAIL] telegram: init: %v\n", err)
			ok = false
		} else {
			me, err := tg.GetMe(ctx)
			if err != nil {
				fmt.Printf("[FAIL] telegram: getMe: %v\n", err)
				ok = false
			} else {
				fmt.Printf("[PASS] telegram: getMe → @%s (id=%d)\n", me.Username, me.ID)
			}
		}
	}

	fmt.Println("=== selftest done ===")
	if !ok {
		os.Exit(1)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
