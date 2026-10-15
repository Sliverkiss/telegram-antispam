package bot

// amend13「85 分以上才踢出去」——独立验收测试。
//
// 门限语义（唯一权威口径）：
//   - 通知分数 = notify.ConfidenceScore(confidence)（0–100 四舍五入）。
//   - score ≥ BOT_AI_MIN_PUNISH_SCORE（默认 85）→ 处置（复用 punishAndClear）。
//   - score < 门限 → 放过（不踢不删不签名不留记录），日志
//     amend13_below_score_release + /statusz 计数 ai_below_score_releases_total。
//   - 边界：85 整 = 达标（“85分以上”按 ≥85 处置）。
//   - 复核投出 ad：最终分 = 判广告复核路的平均置信度（非原始低分），
//     同样受门限约束（复核既非死代码、也不无视门限）。
//   - 非受影响通道（签名库 / 上游敏感 / 裸号必杀 / 入群验证超时 /
//     防刷缓存 / 本地规则兜底）行为零变化。
//
// 本文件另含两条线上误杀样本的回归：
//   80 分「联系请👉@xfwwl_bot」与 72 分「https://bugeshan.com」必须不再被踢。

import (
	"context"
	"testing"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/retryq"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// floorCfg arms the disposal floor like production (85).
func floorCfg() config.Config {
	cfg := defaultCfg()
	cfg.MinPunishScore = 85
	return cfg
}

// --- 线上误杀样本回归 -----------------------------------------------------------

// TestAm13ProductionSample80NotKicked: the exact production sample that was
// kicked at 80 分 must now be RELEASED (below the 85 floor).
func TestAm13ProductionSample80NotKicked(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.80)} // → 80 分
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 801, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 801, 41, "联系请👉@xfwwl_bot"))

	if len(tg.bans) != 0 {
		t.Fatalf("80 分样本必须不踢：bans=%v", tg.bans)
	}
	if len(tg.deletes) != 0 {
		t.Fatalf("80 分样本必须不删消息：deletes=%v", tg.deletes)
	}
	if _, releases := b.Amend13Stats(); releases != 1 {
		t.Fatalf("放过计数 = %d, want 1", releases)
	}
}

// TestAm13ProductionSample72NotKicked: the second production sample (72 分).
func TestAm13ProductionSample72NotKicked(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.72)} // → 72 分
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 802, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 802, 42, "https://bugeshan.com"))

	if len(tg.bans) != 0 {
		t.Fatalf("72 分样本必须不踢：bans=%v", tg.bans)
	}
}

// --- 门限边界 -------------------------------------------------------------------

// TestAm13Score87Kicks: 87 分 ≥ 85 → 处置照旧（复用 punishAndClear 链）。
func TestAm13Score87Kicks(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.87)}
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 803, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 803, 43, "加V 下注"))

	if len(tg.bans) != 1 {
		t.Fatalf("87 分必须踢：bans=%v", tg.bans)
	}
	if _, releases := b.Amend13Stats(); releases != 0 {
		t.Fatalf("处置不该计放过：releases=%d", releases)
	}
}

// TestAm13Boundary85Kicks: 0.85 → score exactly 85 = 达标处置。
func TestAm13Boundary85Kicks(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.85)}
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 804, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 804, 44, "加V 下注"))

	if len(tg.bans) != 1 {
		t.Fatalf("85 整边界必须处置（≥85）：bans=%v", tg.bans)
	}
}

// TestAm13Boundary84Releases: 0.84 → 84 分 = 低于门限放过。
func TestAm13Boundary84Releases(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.84)}
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 805, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 805, 45, "加V 下注"))

	if len(tg.bans) != 0 {
		t.Fatalf("84 分必须放过：bans=%v", tg.bans)
	}
}

// TestAm13RoundingBoundary: ConfidenceScore 取整口径——门限比较用的是取整后
// 的分数，与通知展示口径一致（0.845 → 84.5 → 85 = 达标；0.844 → 84.4 → 84 = 放过）。
func TestAm13RoundingBoundary(t *testing.T) {
	cases := []struct {
		conf float64
		want bool // want disposal?
	}{
		{0.845, true},  // 84.5 → 85（int(v+0.5) 四舍五入）
		{0.844, false}, // 84.4 → 84
	}
	for i, tc := range cases {
		tg := newFakeTG()
		fa := &fakeAI{verdict: fakeVerdict(true, "广告", tc.conf)}
		b := voteBot(t, tg, fa, nil, floorCfg())
		ctx := context.Background()
		uid := int64(810 + i)
		b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -400})
		b.HandleMessage(ctx, adMsg(-400, uid, int64(46+i), "加V 下注"))
		got := len(tg.bans) == 1
		if got != tc.want {
			t.Errorf("conf=%v 处置=%v, want %v", tc.conf, got, tc.want)
		}
	}
}

// TestAm13BelowFloorConsumesWatchlist: the below-floor outcome is FINAL —
// the watchlist slot is consumed (amend5 D "one judgment = one consumption"),
// a later message from the same user does not hit the AI again.
func TestAm13BelowFloorConsumesWatchlist(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.80)}
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 820, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 820, 47, "联系请👉@xfwwl_bot"))
	if sus, _ := b.Store.IsSuspect(ctx, 820); sus {
		t.Fatal("低于门限放过也必须消费观察名额（判定已终局）")
	}
	b.HandleMessage(ctx, adMsg(-400, 820, 48, "再发一条"))
	if fa.callCount() != 1 {
		t.Fatalf("消费后不该再送 AI：calls=%d", fa.callCount())
	}
}

// --- 复核投票与门限的关系 --------------------------------------------------------

// TestAm13VoteAdConsensusScorePunishes: 低置信度（0.5）→ 复核投出 ad，复核路
// 平均置信度 0.90 → 最终分 90 ≥ 85 → 处置。复核不是死代码：共识够高仍踢。
func TestAm13VoteAdConsensusScorePunishes(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)} // 触发复核
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{
			VotesTotal: 3, VotesAd: 2,
			Lanes: []ai.VoteLane{
				{Model: "m", IsAd: true, Confidence: 0.95},
				{Model: "m", IsAd: true, Confidence: 0.85},
				{Model: "m", Err: "undecided"},
			},
			Final: "ad",
		}
	}
	b := voteBot(t, tg, fa, verify, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 830, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 830, 50, "加V 下注"))

	if len(tg.bans) != 1 {
		t.Fatalf("复核共识 90 分必须处置：bans=%v", tg.bans)
	}
}

// TestAm13VoteAdConsensusBelowFloorReleases: 复核投出 ad 但共识平均 0.60 →
// 60 分 < 85 → 放过。复核通过≠无视门限；也不再沿用原始 0.5 低分去踢。
func TestAm13VoteAdConsensusBelowFloorReleases(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{
			VotesTotal: 3, VotesAd: 2,
			Lanes: []ai.VoteLane{
				{Model: "m", IsAd: true, Confidence: 0.65},
				{Model: "m", IsAd: true, Confidence: 0.55},
				{Model: "m", Err: "undecided"},
			},
			Final: "ad",
		}
	}
	b := voteBot(t, tg, fa, verify, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 831, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 831, 51, "加V 下注"))

	if len(tg.bans) != 0 {
		t.Fatalf("复核共识 60 分必须放过：bans=%v", tg.bans)
	}
	if _, releases := b.Amend13Stats(); releases != 1 {
		t.Fatalf("放过计数 = %d, want 1", releases)
	}
}

// TestAm13VoteNotAdUnchanged: 复核投 not_ad → 行为零变化（不处置、消费名额）。
func TestAm13VoteNotAdUnchanged(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{VotesTotal: 3, VotesNo: 3, Final: "not_ad"}
	}
	b := voteBot(t, tg, fa, verify, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 832, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 832, 52, "聊聊而已"))

	if len(tg.bans) != 0 {
		t.Fatalf("复核 not_ad 不处置：bans=%v", tg.bans)
	}
	if sus, _ := b.Store.IsSuspect(ctx, 832); sus {
		t.Fatal("复核 not_ad 消费观察名额")
	}
	if _, releases := b.Amend13Stats(); releases != 0 {
		t.Fatalf("not_ad 不计放过：releases=%d", releases)
	}
}

// TestAm13VoteConsensusDegenerateLanesFallback: 复核判 ad 但 lanes 无置信度
// （0）→ 最终分回落主判定置信度（0.5→50 分）→ 低于门限放过。
func TestAm13VoteConsensusDegenerateLanesFallback(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	verify := func(ctx context.Context, content ai.Content) ai.VerifyResult {
		return ai.VerifyResult{VotesTotal: 2, VotesAd: 2, Final: "ad"}
	}
	b := voteBot(t, tg, fa, verify, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 833, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 833, 53, "加V 下注"))

	if len(tg.bans) != 0 {
		t.Fatalf("无形状 lanes 回落 50 分必须放过：bans=%v", tg.bans)
	}
}

// --- 配置 -----------------------------------------------------------------------

// TestAm13FloorDisabledByZero: 显式 0 = 关闭门限（旧行为：判广告即处置）。
func TestAm13FloorDisabledByZero(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "赌博", 0.5)}
	cfg := floorCfg()
	cfg.MinPunishScore = 0
	b := voteBot(t, tg, fa, nil, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 840, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 840, 54, "加V 下注"))

	if len(tg.bans) != 1 {
		// Verify == nil → commitAd；门限 0 = 关闭 → 处置（amend13 之前的旧行为）。
		t.Fatalf("门限关闭 = 旧行为（判广告即处置）：bans=%v", tg.bans)
	}
	if _, releases := b.Amend13Stats(); releases != 0 {
		t.Fatalf("门限关闭不计数：releases=%d", releases)
	}
}

// --- 非受影响通道零变化 ----------------------------------------------------------

// TestAm13SignatureHitUnaffected: 签名库命中（score 100）不受门限影响。
func TestAm13SignatureHitUnaffected(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	if err := b.Store.AddMessageSignature(ctx, "sig-801", "联系请👉@xfwwl_bot"); err != nil {
		t.Fatal(err)
	}
	b.Store.AddSuspect(ctx, store.Suspect{UID: 850, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 850, 55, "联系请👉@xfwwl_bot"))

	if len(tg.bans) != 1 {
		t.Fatalf("签名命中必须处置（score 100 豁免门限）：bans=%v", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("签名命中零 AI：calls=%d", fa.callCount())
	}
}

// TestAm13LocalFallbackUnaffected: 本地规则兜底不受门限影响（既有阈值合同）。
func TestAm13LocalFallbackUnaffected(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{fail: true}
	cfg := floorCfg()
	b := voteBot(t, tg, fa, nil, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 851, ChatID: -500})
	b.HandleMessage(ctx, adMsg(-500, 851, 56, "专业办证 加V信 kfw123"))

	item := retryqItem(cfg, "msg:-500:56", -500, 56, 851, "专业办证 加V信 kfw123")
	for round := 0; round < cfg.RetryMax; round++ {
		ok, code := b.RetryRound(ctx, item)
		item.Attempts = round + 1
		if ok && code == "local_fallback" {
			if len(tg.bans) != 1 {
				t.Fatalf("兜底命中必须处置（豁免门限）：bans=%v", tg.bans)
			}
			return
		}
	}
	t.Fatalf("兜底未触发")
}

// retryqItem builds the retryq item for the fallback-lane test.
func retryqItem(cfg config.Config, key string, chatID, msgID, uid int64, text string) retryq.Item {
	return retryq.Item{Key: key, Kind: "message", ChatID: chatID, MessageID: msgID, UID: uid,
		Text: text, Mode: "message", EnqueuedAt: time.Now()}
}

// --- /statusz 视图 ---------------------------------------------------------------

// TestAm13StatuszView: Amend13View 携带门限值与放过计数。
func TestAm13StatuszView(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.80)}
	b := voteBot(t, tg, fa, nil, floorCfg())
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 860, ChatID: -400})
	b.HandleMessage(ctx, adMsg(-400, 860, 57, "联系请👉@xfwwl_bot"))

	v := b.Amend13View()
	if v.MinPunishScore != 85 {
		t.Errorf("MinPunishScore = %d, want 85", v.MinPunishScore)
	}
	if v.BelowScoreReleasesTotal != 1 {
		t.Errorf("BelowScoreReleasesTotal = %d, want 1", v.BelowScoreReleasesTotal)
	}
}

// TestAm13JoinBelowFloorWelcomes: 入群简介判广告但低于门限 → 放过 + 走欢迎出口
// （onJoinVerdictNonAd：门限放过的 join 结论 = 非处置终局）。
func TestAm13JoinBelowFloorWelcomes(t *testing.T) {
	tg := newFakeTG()
	tg.chat[870] = telegram.ChatInfo{ID: 870, Bio: "个人主页 https://me.example"}
	tg.member[fkey(-500, 870)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 870}}
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.80)}
	cfg := floorCfg()
	cfg.JoinVerify = true
	cfg.JoinVerifyTimeout = 300 * time.Second
	cfg.JoinVerifyScan = 5 * time.Second
	b := voteBot(t, tg, fa, nil, cfg)

	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(870, false, "p80", "P80")}, 0)

	if len(tg.bans) != 0 {
		t.Fatalf("低于门限的入群判定必须不处置：bans=%v", tg.bans)
	}
	if n := welcomeCount(tg); n != 1 {
		t.Fatalf("低于门限放过 = 非处置终局 → 应发欢迎：%d", n)
	}
}
