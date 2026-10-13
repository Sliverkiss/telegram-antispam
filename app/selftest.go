package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// runSelftest executes the belmo connectivity acceptance checks in order:
// config → Redis → AI → Telegram. Each prints [PASS]/[FAIL]; any failure
// makes the process exit non-zero. Timeouts are bounded.
func runSelftest(cfg config.Config) {
	ok := true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Println("=== adbot selftest ===")

	// 1. Config (already parsed — render the redacted summary).
	fmt.Println("[PASS] config: parsed")
	for _, k := range []string{"BOT_ADDR", "BOT_ACTION", "BOT_REDIS_URL", "BOT_AI_BASE_URL", "BOT_AI_MODEL", "BOT_LOG_CHAT"} {
		fmt.Printf("        %s=%s\n", k, cfg.Redacted()[k])
	}

	// 2. Redis: PING → SET/GET/DEL roundtrip.
	if cfg.RedisURL == "" {
		fmt.Println("[PASS] redis: not configured — in-memory store (dev/test)")
	} else {
		rs, err := store.OpenRedis(cfg.RedisURL, cfg.SuspectTTL, cfg.RedisInsecureTLS, nil)
		if err != nil {
			fmt.Printf("[FAIL] redis: connect: %v\n", err)
			ok = false
		} else {
			defer rs.Close()
			if err := rs.Ping(ctx); err != nil {
				fmt.Printf("[FAIL] redis: PING: %v\n", err)
				ok = false
			} else {
				fmt.Println("[PASS] redis: PING")
				probe := "adbot:selftest:probe"
				if _, err := rs.Do("SET", probe, "1"); err != nil {
					fmt.Printf("[FAIL] redis: SET: %v\n", err)
					ok = false
				} else if _, err := rs.Do("GET", probe); err != nil {
					fmt.Printf("[FAIL] redis: GET: %v\n", err)
					ok = false
				} else {
					fmt.Println("[PASS] redis: SET/GET roundtrip")
				}
				rs.Do("DEL", probe)
			}
		}
	}

	// 3. AI: canary sentence → JSON verdict + latency.
	if cfg.AIKey == "" {
		fmt.Println("[FAIL] ai: BOT_AI_API_KEY is not set")
		ok = false
	} else {
		aicfg := ai.New(ai.Config{
			BaseURL: cfg.AIBaseURL,
			APIKey:  cfg.AIKey,
			Model:   cfg.AIModel,
			Timeout: cfg.AITimeout,
			Retries: 2,
		})
		start := time.Now()
		v, okAI := ai.Classify(ctx, aicfg, "判断这句话是不是广告：专业办理各种证件 加V信 xxx")
		elapsed := time.Since(start)
		if !okAI {
			fmt.Printf("[FAIL] ai: classify failed or undecided (%v)\n", elapsed.Round(time.Millisecond))
			ok = false
		} else {
			fmt.Printf("[PASS] ai: is_ad=%v category=%s confidence=%.2f reason=%q (%.0fms)\n",
				v.IsAd, v.Category, v.Confidence, v.Reason, float64(elapsed.Microseconds())/1000)
		}
	}

	// 4. Telegram: getMe.
	if cfg.Token == "" {
		fmt.Println("[FAIL] telegram: BOT_TOKEN is not set")
		ok = false
	} else {
		tg := telegram.NewClient(cfg.Token)
		me, err := tg.GetMe(ctx)
		if err != nil {
			fmt.Printf("[FAIL] telegram: getMe: %v\n", err)
			ok = false
		} else {
			fmt.Printf("[PASS] telegram: getMe → @%s (id=%d)\n", me.Username, me.ID)
		}
	}

	fmt.Println("=== selftest done ===")
	if !ok {
		os.Exit(1)
	}
}
