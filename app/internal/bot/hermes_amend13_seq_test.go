package bot

// Hermes：amend13 两条链路的真实日志序列（给肉眼核对处置分支）。
// 跑法：go test ./internal/bot/ -run TestH13ZSequences -v -count=1

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/store"
)

func TestH13ZSequences(t *testing.T) {
	// ---------- (a) 80 分「联系请👉@xfwwl_bot」→ 低于门限放过 ----------
	var logs strings.Builder
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "广告", 0.80)}
	b := a13LogBot(t, tg, fa, &logs)
	b.Store.AddSuspect(context.Background(), store.Suspect{UID: 9401, ChatID: -400})
	b.HandleMessage(context.Background(), adMsg(-400, 9401, 1001, "联系请👉@xfwwl_bot"))
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (a) 80 分样本 → 低于门限放过（ban=%d, 删消息=%d, 放过计数=%d）=====\n%s",
		len(tg.banList()), len(tg.deletes), mustReleases(b), logs.String())

	// ---------- (b) 87 分 → 门限之上处置 ----------
	logs.Reset()
	tg2 := newFakeTG()
	fa2 := &fakeAI{verdict: fakeVerdict(true, "广告", 0.87)}
	b2 := a13LogBot(t, tg2, fa2, &logs)
	b2.Store.AddSuspect(context.Background(), store.Suspect{UID: 9402, ChatID: -400})
	b2.HandleMessage(context.Background(), adMsg(-400, 9402, 1002, "加V 下注"))
	waitFor(t, func() bool { return len(tg2.banList()) >= 1 }, "处置")
	time.Sleep(100 * time.Millisecond)
	t.Logf("\n===== (b) 87 分样本 → 门限之上处置（ban=%d, AI 调用=%d）=====\n%s",
		len(tg2.banList()), fa2.callCount(), logs.String())
}

// a13LogBot builds a floor-armed bot whose log drains into logs.
func a13LogBot(t *testing.T, tg *fakeTG, fa AIClassifier, logs *strings.Builder) *Bot {
	t.Helper()
	st := store.NewMemory()
	lg := logx.New(logs, logx.Info, "text", "bot")
	return New(st, tg, fa, nil, floorCfg(), lg)
}

func mustReleases(b *Bot) int64 {
	_, r := b.Amend13Stats()
	return r
}
