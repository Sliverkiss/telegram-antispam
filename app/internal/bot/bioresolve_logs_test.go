package bot

// Temporary log-capture driver: runs the three bio-resolve chains at Debug
// level with a real logx.Logger writing to a buffer, so the final report can
// quote the ACTUAL log sequences (报告要求：三条链路真实日志序列). Not part of
// the acceptance surface — run with:
//   go test ./internal/bot/ -run TestBioResolveLogSequences -count=1 -v
import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

func TestBioResolveLogSequences(t *testing.T) {
	ctx := context.Background()

	// ---- chain 1: join-bio, @ resolves to grey target, punished ----
	var buf bytes.Buffer
	lg := logx.New(&buf, logx.Debug, "text", "bot")
	tg := newFakeTG()
	bio := "频道 @gray_channel 每日上新"
	tg.chat[300] = telegram.ChatInfo{ID: 300, Bio: bio}
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg := resolveCfg()
	cfg.LogChat = -999
	st := store.NewMemory()
	b := New(st, tg, fa, nil, cfg, lg)
	b.BioTarget = &fakeBioResolver{texts: map[string]string{
		"gray_channel": "棋牌资源每日更新 上分下分通道",
	}}
	b.HandleNewMembers(ctx, -300, []telegram.User{u(300, false, "spam", "S")}, 0)
	t.Logf("---- chain 1: join-bio @hit → punish ----\n%s", buf.String())
	if ok, _ := b.Store.HasBioSignature(ctx, normalize.Signature(bio)); !ok {
		t.Fatal("chain 1: signature must persist")
	}

	// ---- chain 2: comment-bio, @ resolves, flash hit on the original bio ----
	buf.Reset()
	lg2 := logx.New(&buf, logx.Debug, "text", "bot")
	tg2 := newFakeTG()
	fa2 := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	cfg2 := commentCfg()
	cfg2.LogChat = -999
	b2 := New(store.NewMemory(), tg2, fa2, nil, cfg2, lg2)
	b2.Cfg.BioResolve = true
	b2.Cfg.BioResolveMax = 3
	b2.BioTarget = &fakeBioResolver{texts: map[string]string{
		"gray_channel": "棋牌资源每日更新 上分下分通道",
	}}
	commentDrive(b2, tg2, 13, "频道 @gray_channel", commentMsg(-500, 13, 40, "评论"))
	t.Logf("---- chain 2: comment-bio @hit → punish ----\n%s", buf.String())

	// ---- chain 3: join-bio, @ lookup misses, judged as-is, released ----
	buf.Reset()
	lg3 := logx.New(&buf, logx.Debug, "text", "bot")
	tg3 := newFakeTG()
	bio3 := "资源 @dead_channel"
	tg3.chat[302] = telegram.ChatInfo{ID: 302, Bio: bio3}
	fa3 := &fakeAI{verdict: fakeVerdict(false, "无", 0.1)}
	b3 := New(store.NewMemory(), tg3, fa3, nil, resolveCfg(), lg3)
	b3.BioTarget = &fakeBioResolver{errs: map[string]error{
		"dead_channel": errors.New("telegram api getChat: chat not found (code 400)"),
	}}
	b3.HandleNewMembers(ctx, -300, []telegram.User{u(302, false, "miss", "M")}, 0)
	t.Logf("---- chain 3: join-bio @miss → judged as-is ----\n%s", buf.String())
	if len(tg3.bans) != 0 {
		t.Fatal("chain 3: miss must not dispose")
	}
	_ = strings.TrimSpace
}
