package bot

// Hermes 独立验收（amend13「85 分以上才踢」）——不复用执行体断言，目标是证伪。
//
// H13-1 两条线上误杀样本（80/72）必须：不踢、不删、不通知、不写签名、放过计数+1、名额消耗
// H13-2 门限按【显示分数】判定（四舍五入后 ≥85 才算达标）：0.846→85分踢，0.844→84分放过
// H13-3 门限设 0 = 关闭 → 旧行为（0.72 照踢）
// H13-4 【关键绕过检查】低于门限的评论简介绝不能进防刷缓存：放过后再来一次必须重新走 AI
// H13-5 本地规则兜底不受门限影响（哪怕门限设 100）
// H13-6 签名库通道不受门限影响（门限 100 也照踢）；AI 敏感拦截(0.99)在门限 100 下会被放过（说明门限确实只作用于 AI 判定路）

import (
	"context"
	"testing"
	"time"

	"adbot/internal/normalize"
	"adbot/internal/store"
)

// h13Punished reports whether uid was disposed of.
func h13Kicked(tg *fakeTG) int { return len(tg.banList()) }

// --- H13-1: 线上误杀样本 ---------------------------------------------------------

func TestH13MyProductionSamplesAreReleasedSilently(t *testing.T) {
	cases := []struct {
		name string
		uid  int64
		text string
		conf float64
	}{
		{"80分样本 联系请@xfwwl_bot", 9001, "联系请👉@xfwwl_bot", 0.80},
		{"72分样本 https://bugeshan.com", 9002, "https://bugeshan.com", 0.72},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := newFakeTG()
			cfg := floorCfg()
			cfg.LogChat = -999
			fa := &fakeAI{verdict: fakeVerdict(true, "广告", tc.conf)}
			b := voteBot(t, tg, fa, nil, cfg)
			rec := wireTestNotifier(t, b, cfg)
			ctx := context.Background()
			b.Store.AddSuspect(ctx, store.Suspect{UID: tc.uid, ChatID: -400})

			b.HandleMessage(ctx, adMsg(-400, tc.uid, 9000+tc.uid, tc.text))
			time.Sleep(250 * time.Millisecond) // 给异步通知/公告留出发送窗口

			if n := h13Kicked(tg); n != 0 {
				t.Fatalf("必须不踢：%d 次处置", n)
			}
			if len(tg.deleteList()) != 0 {
				t.Fatalf("必须不删消息：%v", tg.deleteList())
			}
			if got := len(rec.texts()); got != 0 {
				t.Fatalf("放过不该推通知频道：%d 条\n%s", got, rec.texts()[0])
			}
			if len(tg.sendsSnapshot()) != 0 {
				t.Fatalf("放过不该在群里发公告：%v", tg.sendsSnapshot())
			}
			if ok, _ := b.Store.HasMessageSignature(ctx, normalize.Signature(tc.text)); ok {
				t.Fatal("放过不该写签名库")
			}
			if _, releases := b.Amend13Stats(); releases != 1 {
				t.Fatalf("放过计数：%d，want 1", releases)
			}
			// 放过是终局（一次判定一次消费）：名额被消耗
			if ok, _ := b.Store.IsSuspect(ctx, tc.uid); ok {
				t.Fatal("放过也应消耗观察名额（一次判定一次消费）")
			}
		})
	}
}

// --- H13-2: 门限按显示分数（四舍五入后）判定 --------------------------------------

func TestH13MyFloorFollowsDisplayedScore(t *testing.T) {
	cases := []struct {
		conf  float64
		score int
		kick  bool
	}{
		{0.844, 84, false}, // 显示 84 分 → 放过
		{0.846, 85, true},  // 显示 85 分 → 踢（“85分以上”按 ≥85）
		{0.85, 85, true},   // 边界整 85 → 踢
		{0.8499, 85, true}, // 显示仍是 85 → 踢（跟用户看到的数字一致）
		{0.83, 83, false},
	}
	for _, tc := range cases {
		tg := newFakeTG()
		cfg := floorCfg()
		cfg.LogChat = -999
		fa := &fakeAI{verdict: fakeVerdict(true, "广告", tc.conf)}
		b := voteBot(t, tg, fa, nil, cfg)
		ctx := context.Background()
		uid := int64(9100 + tc.score)
		b.Store.AddSuspect(ctx, store.Suspect{UID: uid, ChatID: -400})

		b.HandleMessage(ctx, adMsg(-400, uid, uid, "加V 下注"))

		got := h13Kicked(tg) == 1
		if got != tc.kick {
			t.Fatalf("conf=%.4f（显示 %d 分）应 kick=%v，实际 %v", tc.conf, tc.score, tc.kick, got)
		}
	}
}

// --- H13-3: 门限 0 = 关闭（旧行为） ----------------------------------------------

func TestH13MyZeroFloorRestoresLegacy(t *testing.T) {
	tg := newFakeTG()
	cfg := defaultCfg()
	cfg.LogChat = -999
	cfg.MinPunishScore = 0 // 关闭
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.72)}
	b := voteBot(t, tg, fa, nil, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 9200, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 9200, 9200, "https://bugeshan.com"))

	if n := h13Kicked(tg); n != 1 {
		t.Fatalf("门限关闭时应保持旧行为（照踢）：%d", n)
	}
	if _, releases := b.Amend13Stats(); releases != 0 {
		t.Fatalf("门限关闭不该有放过计数：%d", releases)
	}
}

// --- H13-4: 关键绕过检查：放过的评论简介不能进防刷缓存 ----------------------------

func TestH13MyReleasedCommentBioNeverEntersFlashCache(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	cfg.MinPunishScore = 85
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.60)} // 60 分 → 低于门限
	b := voteBot(t, tg, fa, nil, cfg)

	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 9300, bio, commentMsg(-500, 9300, 9300, "评论一"))
	if n := h13Kicked(tg); n != 0 {
		t.Fatalf("60 分的评论简介必须放过：%d", n)
	}
	if _, _, _, cached := commentStats(b); cached != 0 {
		t.Fatalf("放过的简介绝不能进防刷缓存（否则重复出现会零 AI 直接踢，绕过门限）：%d", cached)
	}
	if ok, _ := b.Store.HasBioSignature(context.Background(), normalize.Signature(bio)); ok {
		t.Fatal("放过的简介不该写 bio 签名库")
	}

	// 同简介再来（另一个号）→ 必须重新走 AI，而不是被防刷秒杀
	commentDrive(b, tg, 9301, bio, commentMsg(-500, 9301, 9301, "评论二"))
	if fa.callCount() != 2 {
		t.Fatalf("放过后的重复简介必须重新送 AI：AI 调用 %d 次", fa.callCount())
	}
	if n := h13Kicked(tg); n != 0 {
		t.Fatalf("仍然低于门限 → 继续放过：%d", n)
	}
	// 门限之上的评论简介才允许进缓存（对照组）
	fa2 := &fakeAI{verdict: fakeVerdict(true, "广告", 0.95)}
	tg2 := newFakeTG()
	b2 := voteBot(t, tg2, fa2, nil, cfg)
	commentDrive(b2, tg2, 9302, "办证 加V t.me/yyy", commentMsg(-500, 9302, 9302, "评论"))
	if _, _, _, cached := commentStats(b2); cached != 1 {
		t.Fatalf("95 分（≥门限）应进防刷缓存：%d", cached)
	}
}

// --- H13-5: 本地规则兜底不受门限影响（门限设 100） --------------------------------

func TestH13MyLocalFallbackIgnoresFloor(t *testing.T) {
	tg := newFakeTG()
	cfg := floorCfg()
	cfg.LogChat = -999
	cfg.MinPunishScore = 100  // 极端门限：AI 判定一律不可能达标
	fa := &fakeAI{fail: true} // AI 全失败 → 走兜底
	b := voteBot(t, tg, fa, nil, cfg)
	ctx := context.Background()
	b.Store.AddSuspect(ctx, store.Suspect{UID: 9400, ChatID: -500})

	b.HandleMessage(ctx, adMsg(-500, 9400, 9400, "专业办证 加V信 kfw123"))
	item := retryqItem(cfg, "msg:-500:9400", -500, 9400, 9400, "专业办证 加V信 kfw123")
	for round := 0; round < cfg.RetryMax; round++ {
		ok, code := b.RetryRound(ctx, item)
		item.Attempts = round + 1
		if ok && code == "local_fallback" {
			if n := h13Kicked(tg); n != 1 {
				t.Fatalf("本地规则兜底必须处置（豁免门限）：%d", n)
			}
			return
		}
	}
	t.Fatal("兜底未触发")
}

// --- H13-6: 签名库通道不受门限影响（门限 100）；门限只约束 AI 判定路 --------------

func TestH13MySignatureLaneIgnoresFloor(t *testing.T) {
	tg := newFakeTG()
	cfg := floorCfg()
	cfg.LogChat = -999
	cfg.MinPunishScore = 100
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.5)}
	b := voteBot(t, tg, fa, nil, cfg)
	ctx := context.Background()
	text := "专业办证 加V信 kfw123"
	b.Store.AddMessageSignature(ctx, normalize.Signature(text), text)
	b.Store.AddSuspect(ctx, store.Suspect{UID: 9500, ChatID: -400})

	b.HandleMessage(ctx, adMsg(-400, 9500, 9500, text))

	if n := h13Kicked(tg); n != 1 {
		t.Fatalf("签名库命中必须照踢（不受门限影响）：%d", n)
	}
	if fa.callCount() != 0 {
		t.Fatalf("签名命中不该走 AI：%d", fa.callCount())
	}
}
