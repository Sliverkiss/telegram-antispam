package bot

// Hermes 独立验收（amend15：简介 @ 解析 → 送审富化）——不复用执行体断言。
//
// H15-1 解析不递归：被解析目标的简介里再出现 @ → 绝不再抓（防扇出爆炸）
// H15-2 解析失败 ≡ 关闭解析：送审文本逐字节相同（原简介），零残块，裁决一致
// H15-3 送审触发门不变：频道评论简介无 @ 无链接 → 零送审零解析；有 @ → 送审一次且解析只发生在这条送审内

import (
	"context"
	"errors"
	"strings"
	"testing"

	"adbot/internal/telegram"
)

func TestH15MyNoRecursiveResolveFanout(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, resolveCfg())
	res := &fakeBioResolver{texts: map[string]string{
		"outer_one": "见 @outer_two 与 @deeper_handle",
		"outer_two": "第二层也带 @another_deep",
	}}
	b.BioTarget = res
	tg.chat[400] = telegram.ChatInfo{ID: 400, Bio: "简介 @outer_one @outer_two"}
	b.HandleNewMembers(context.Background(), -300, []telegram.User{u(400, false, "rec", "R")}, 0)

	if len(res.calls) != 2 {
		t.Fatalf("只应解析原简介里的 2 个 handle，实际 %v", res.calls)
	}
	for _, c := range res.calls {
		if c == "deeper_handle" || c == "another_deep" {
			t.Fatalf("被解析目标的简介里的 @ 不得再抓（扇出）：%v", res.calls)
		}
	}
	if !strings.Contains(fa.lastContent.Text, "deeper_handle") {
		t.Fatalf("深层 @ 应作为文本证据进入送审：%q", fa.lastContent.Text)
	}
}

func TestH15MyResolveFailureEqualsDisabled(t *testing.T) {
	// (a) 解析开启但查不到目标
	tgA := newFakeTG()
	tgA.chat[401] = telegram.ChatInfo{ID: 401, Bio: "资源 @dead_chan"}
	faA := &fakeAI{verdict: fakeVerdict(false, "无", 0.1)}
	bA := testBot(t, tgA, faA, resolveCfg())
	bA.BioTarget = &fakeBioResolver{errs: map[string]error{
		"dead_chan": errors.New("telegram api getChat: chat not found (code 400)"),
	}}
	bA.HandleNewMembers(context.Background(), -300, []telegram.User{u(401, false, "a", "A")}, 0)

	// (b) 解析功能整体关闭
	tgB := newFakeTG()
	tgB.chat[402] = telegram.ChatInfo{ID: 402, Bio: "资源 @dead_chan"}
	faB := &fakeAI{verdict: fakeVerdict(false, "无", 0.1)}
	cfgB := defaultCfg()
	cfgB.BioResolve = false
	bB := testBot(t, tgB, faB, cfgB)
	bB.HandleNewMembers(context.Background(), -300, []telegram.User{u(402, false, "b", "B")}, 0)

	if faA.lastContent.Text != faB.lastContent.Text {
		t.Fatalf("解析失败必须与关闭解析逐字节一致：\n A %q\n B %q",
			faA.lastContent.Text, faB.lastContent.Text)
	}
	if strings.Contains(faA.lastContent.Text, "【相关简介】") {
		t.Fatalf("解析失败不得留残块：%q", faA.lastContent.Text)
	}
	if len(tgA.bans) != 0 || len(tgB.bans) != 0 {
		t.Fatalf("两条都必须零处置：%v %v", tgA.bans, tgB.bans)
	}
}

func TestH15MySubmissionTriggerGateUnchanged(t *testing.T) {
	// 频道帖子评论 + 简介不含 @ 不含链接 → 不送审、不解析（触发门未变）
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.9)}
	b := testBot(t, tg, fa, commentCfg())
	b.Cfg.BioResolve = true
	res := &fakeBioResolver{}
	b.BioTarget = res
	commentDrive(b, tg, 21, "今天天气不错 顺便聊聊", commentMsg(-500, 21, 50, "评论"))

	if fa.calls != 0 {
		t.Fatalf("无 @ 无链接的简介不得送审：AI 调用 %d 次", fa.calls)
	}
	if len(res.calls) != 0 {
		t.Fatalf("未送审就不得解析：%v", res.calls)
	}

	// 含 @ → 送审一次，且解析发生在这条送审里
	fa2 := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.9)}
	b2 := testBot(t, tg, fa2, commentCfg())
	b2.Cfg.BioResolve = true
	res2 := &fakeBioResolver{texts: map[string]string{"some_channel": "棋牌上分下分"}}
	b2.BioTarget = res2
	commentDrive(b2, tg, 22, "我的频道 @some_channel", commentMsg(-500, 22, 51, "评论二"))

	if fa2.calls != 1 {
		t.Fatalf("含 @ 的简介必须送审一次：%d", fa2.calls)
	}
	if len(res2.calls) != 1 || res2.calls[0] != "some_channel" {
		t.Fatalf("解析必须发生在这条送审里：%v", res2.calls)
	}
}
