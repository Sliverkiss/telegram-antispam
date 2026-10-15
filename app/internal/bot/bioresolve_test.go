package bot

// bio-resolve（简介 @ 解析）验收测试。全离线：fakeBioResolver 注入，零真实
// 网络、零真实 token。覆盖：
//   - scanHandles：@ 提取、去重、邮箱/URL 排除、边界、4 字符旧账号不解析
//   - bioResolveText：纯合并器（空解析 = 原文返回；顺序保持）
//   - relatedBioText：user bio 优先，channel description 兜底
//   - handleFromLink：t.me/xxx 扩展点（+hash / joinchat / query / 非用户名排除）
//   - bioResolve：上限跳过、miss 不判失败、空简介跳过、整扇超时
//   - 三链路：join-bio 与 comment-bio 送审文本被富化，签名库 / 防刷缓存 /
//     通知证据仍是 ORIGINAL bio（sigText 分离）；解析失败按原简介判定

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"adbot/internal/config"
	"adbot/internal/normalize"
	"adbot/internal/telegram"
)

// fakeBioResolver is the injectable seam: per-handle intro text, per-handle
// error, call recording, optional per-call delay (timeout tests).
type fakeBioResolver struct {
	texts   map[string]string
	errs    map[string]error
	calls   []string
	delay   time.Duration
	blockCh chan struct{} // when non-nil, lookups park until closed (timeout test)
}

func (f *fakeBioResolver) GetChatByUsername(ctx context.Context, username string) (telegram.ChatInfo, error) {
	f.calls = append(f.calls, username)
	if f.blockCh != nil {
		// Park like a hung transport, but honor the fan timeout (the real
		// client aborts on ctx.Done).
		select {
		case <-f.blockCh:
		case <-ctx.Done():
			return telegram.ChatInfo{}, ctx.Err()
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if err, ok := f.errs[username]; ok {
		return telegram.ChatInfo{}, err
	}
	if txt, ok := f.texts[username]; ok {
		if strings.HasPrefix(txt, "#desc:") { // channel shape: Description only
			return telegram.ChatInfo{ID: 1, Description: strings.TrimPrefix(txt, "#desc:")}, nil
		}
		return telegram.ChatInfo{ID: 1, Bio: txt}, nil
	}
	return telegram.ChatInfo{}, nil // found but empty
}

// resolveCfg arms bio-resolve on the legacy test config.
func resolveCfg() config.Config {
	cfg := defaultCfg()
	cfg.BioResolve = true
	cfg.BioResolveMax = 3
	return cfg
}

// --- scanHandles --------------------------------------------------------------

func TestScanHandles(t *testing.T) {
	got := scanHandles("私聊 @alice_bot 个人频道 @bobby")
	want := []string{"alice_bot", "bobby"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scanHandles = %v, want %v", got, want)
	}
}

func TestScanHandlesExcludesEmailAndURL(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"联系 a@b.com 滴我", nil},                                 // email local part
		{"微信xx@163.com 资源", nil},                               // CJK + email
		{"看 https://t.me/@spam_channel", nil},                  // URL userinfo
		{"看 t.me/@spam_channel 频道", nil},                       // URL path @
		{"加 @real_handle 联系 a@b.com", []string{"real_handle"}}, // both
	}
	for _, c := range cases {
		got := scanHandles(c.text)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("scanHandles(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestScanHandlesBoundaryAndDedup(t *testing.T) {
	// Trailing boundary: "@abc.def" is one token and must not resolve.
	if got := scanHandles("加 @abc.def 滴"); len(got) != 0 {
		t.Errorf("glued @abc.def must not resolve: %v", got)
	}
	if got := scanHandles("@aaa@bbb"); len(got) != 0 {
		t.Errorf("@aaa@bbb must not resolve: %v", got)
	}
	// 4-char legacy handles are below the 5-char floor.
	if got := scanHandles("@abcd 滴"); len(got) != 0 {
		t.Errorf("4-char handle must not resolve: %v", got)
	}
	// Dedup, order preserved.
	if got := scanHandles("@first @second @first"); strings.Join(got, ",") != "first,second" {
		t.Errorf("dedup = %v", got)
	}
}

// --- bioResolveText (pure combiner) -------------------------------------------

func TestBioResolveTextEmptyReturnsBio(t *testing.T) {
	if got := bioResolveText("原始简介", nil); got != "原始简介" {
		t.Fatalf("no resolutions must return bio unchanged: %q", got)
	}
	if got := bioResolveText("原始简介", map[string]string{}); got != "原始简介" {
		t.Fatalf("empty map must return bio unchanged: %q", got)
	}
}

func TestBioResolveTextAppendsRelatedBioBlock(t *testing.T) {
	bio := "私聊机器人：@helper_bot 个人频道：@my_channel"
	res := map[string]string{"helper_bot": "普通助手机器人", "my_channel": "日常分享"}
	got := bioResolveText(bio, res)
	// Bio first, then one 【相关简介】 line per handle in bio order.
	want := "私聊机器人：@helper_bot 个人频道：@my_channel" +
		"\n【相关简介】@helper_bot：普通助手机器人" +
		"\n【相关简介】@my_channel：日常分享"
	if got != want {
		t.Fatalf("enriched text:\n got  %q\n want %q", got, want)
	}
}

// --- relatedBioText -------------------------------------------------------------

func TestRelatedBioText(t *testing.T) {
	if got := relatedBioText(telegram.ChatInfo{Bio: " 用户简介 ", Description: "频道简介"}); got != "用户简介" {
		t.Errorf("bio wins: %q", got)
	}
	if got := relatedBioText(telegram.ChatInfo{Description: " 频道简介 "}); got != "频道简介" {
		t.Errorf("description fallback: %q", got)
	}
	if got := relatedBioText(telegram.ChatInfo{}); got != "" {
		t.Errorf("empty: %q", got)
	}
}

// --- handleFromLink (extension point) -------------------------------------------

func TestHandleFromLink(t *testing.T) {
	cases := []struct {
		link string
		want string
	}{
		{"https://t.me/good_channel", "good_channel"},
		{"t.me/good_channel", "good_channel"},
		{"https://t.me/+AbCdEf", ""},
		{"https://t.me/joinchat/AbCdEf", ""},
		// Deep links drop the query and keep the bare username (resolvable
		// shape; the extension point is not invoked this round anyway).
		{"https://t.me/somebot?start=xxx", "somebot"},
		{"https://example.com/whatever", ""},
		{"https://t.me/abcd", ""}, // too short
		{"", ""},
	}
	for _, c := range cases {
		if got := handleFromLink(c.link); got != c.want {
			t.Errorf("handleFromLink(%q) = %q, want %q", c.link, got, c.want)
		}
	}
}

// --- bioResolve (I/O behavior) ---------------------------------------------------

func TestBioResolveCapSkipsExtraHandles(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	cfg := resolveCfg()
	cfg.BioResolveMax = 1
	b := testBot(t, tg, fa, cfg)
	res := &fakeBioResolver{texts: map[string]string{
		"first_handle": "一", "second_handle": "二",
	}}
	b.BioTarget = res
	b.bioResolve(context.Background(), "加 @first_handle @second_handle 滴")
	if len(res.calls) != 1 || res.calls[0] != "first_handle" {
		t.Fatalf("cap must stop after BioResolveMax: calls=%v", res.calls)
	}
}

func TestBioResolveMissIsNotFailure(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	b := testBot(t, tg, fa, resolveCfg())
	res := &fakeBioResolver{
		texts: map[string]string{"ok_handle": "简介命中"},
		errs:  map[string]error{"dead_handle": errors.New("telegram api getChat: chat not found (code 400)")},
	}
	b.BioTarget = res
	got := b.bioResolve(context.Background(), "加 @dead_handle 和 @ok_handle")
	if _, ok := got["ok_handle"]; !ok {
		t.Fatalf("miss must not kill the other handles: %v", got)
	}
	if _, ok := got["dead_handle"]; ok {
		t.Fatalf("dead handle must be absent: %v", got)
	}
}

func TestBioResolveEmptyTargetSkipped(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	b := testBot(t, tg, fa, resolveCfg())
	res := &fakeBioResolver{} // everything resolves to empty ChatInfo
	b.BioTarget = res
	if got := b.bioResolve(context.Background(), "加 @quiet_handle 滴"); len(got) != 0 {
		t.Fatalf("empty target text must be skipped: %v", got)
	}
}

func TestBioResolveDisabledOrNoSeam(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	cfg := resolveCfg()
	cfg.BioResolve = false
	b := testBot(t, tg, fa, cfg)
	res := &fakeBioResolver{}
	b.BioTarget = res
	if got := b.bioResolve(context.Background(), "加 @some_handle 滴"); got != nil {
		t.Fatalf("disabled flag must resolve nothing: %v", got)
	}
	if len(res.calls) != 0 {
		t.Fatalf("disabled flag must call zero lookups: %v", res.calls)
	}
	// Flag on, seam unwired (nil BioTarget) → no lookups, no panic.
	cfg2 := resolveCfg()
	b2 := testBot(t, tg, fa, cfg2)
	if got := b2.bioResolve(context.Background(), "加 @some_handle 滴"); got != nil {
		t.Fatalf("nil seam must resolve nothing: %v", got)
	}
}

func TestBioResolveTimeoutBoundsFan(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{}
	cfg := resolveCfg()
	cfg.BioResolveTimeout = 20 * time.Millisecond
	b := testBot(t, tg, fa, cfg)
	res := &fakeBioResolver{blockCh: make(chan struct{})}
	b.BioTarget = res
	defer close(res.blockCh)
	done := make(chan struct{})
	var got map[string]string
	go func() {
		got = b.bioResolve(context.Background(), "加 @stuck_handle 滴")
		close(done)
	}()
	select {
	case <-done:
		// The whole fan must come back (bounded), with nothing resolved.
		if len(got) != 0 {
			t.Fatalf("timeout must resolve nothing: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bioResolve hung past the fan timeout")
	}
}

// --- 三链路 1: join-bio ---------------------------------------------------------

// Join-bio with a @ whose target is grey-industry → enriched AI text carries
// the 【相关简介】 evidence; the signature library keeps the ORIGINAL bio.
func TestJoinBioResolveEnrichesJudgmentKeepsSignature(t *testing.T) {
	tg := newFakeTG()
	bio := "频道 @gray_channel"
	tg.chat[300] = telegram.ChatInfo{ID: 300, Bio: bio}
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, resolveCfg())
	b.BioTarget = &fakeBioResolver{texts: map[string]string{
		"gray_channel": "棋牌资源每日更新 上分下分通道",
	}}
	b.HandleNewMembers(context.Background(), -300, []telegram.User{u(300, false, "spam", "S")}, 0)

	if len(tg.bans) != 1 {
		t.Fatalf("ad verdict must ban: %v", tg.bans)
	}
	// The AI judged the ENRICHED text (bio + target intro).
	want := "频道 @gray_channel\n【相关简介】@gray_channel：棋牌资源每日更新 上分下分通道"
	if fa.lastContent.Text != want {
		t.Fatalf("AI text:\n got  %q\n want %q", fa.lastContent.Text, want)
	}
	// The signature library keeps the ORIGINAL bio, not the enriched text.
	if ok, _ := b.Store.HasBioSignature(context.Background(), normalize.Signature(bio)); !ok {
		t.Fatal("signature must be the original bio")
	}
	if ok, _ := b.Store.HasBioSignature(context.Background(), normalize.Signature(want)); ok {
		t.Fatal("enriched text must NOT enter the signature library")
	}
}

// Join-bio whose @ resolves to an innocuous target → judged with the block,
// non-ad verdict, zero disposal (the misfire's counter-example).
func TestJoinBioResolveInnocuousTargetPasses(t *testing.T) {
	tg := newFakeTG()
	bio := "我推: @Xxiongxiong 京东车: @jd_0721_bot"
	tg.chat[301] = telegram.ChatInfo{ID: 301, Bio: bio}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.1)}
	b := testBot(t, tg, fa, resolveCfg())
	b.BioTarget = &fakeBioResolver{texts: map[string]string{
		"Xxiongxiong": "个人推车号 京东百亿补贴", "jd_0721_bot": "京东自动回复机器人",
	}}
	b.HandleNewMembers(context.Background(), -300, []telegram.User{u(301, false, "push", "P")}, 0)
	if len(tg.bans) != 0 {
		t.Fatalf("innocuous resolution must pass: %v", tg.bans)
	}
	if !strings.Contains(fa.lastContent.Text, "【相关简介】@jd_0721_bot：京东自动回复机器人") {
		t.Fatalf("AI text missing related block: %q", fa.lastContent.Text)
	}
}

// --- 三链路 2: comment-bio -------------------------------------------------------

// Comment-bio with a @ → enriched AI text + flash cache keyed on the ORIGINAL
// bio (a second identical bio within the window = flash hit, zero AI).
func TestCommentBioResolveEnrichesAndFlashUsesOriginalBio(t *testing.T) {
	tg := newFakeTG()
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, commentCfg())
	// commentCfg does not arm BioResolve; arm it on top.
	b.Cfg.BioResolve = true
	b.Cfg.BioResolveMax = 3
	b.BioTarget = &fakeBioResolver{texts: map[string]string{
		"gray_channel": "棋牌资源每日更新 上分下分通道",
	}}
	bio := "频道 @gray_channel"
	commentDrive(b, tg, 13, bio, commentMsg(-500, 13, 40, "评论"))
	if len(tg.bans) != 1 {
		t.Fatalf("ad verdict must ban: %v", tg.bans)
	}
	want := "频道 @gray_channel\n【相关简介】@gray_channel：棋牌资源每日更新 上分下分通道"
	if fa.lastContent.Text != want {
		t.Fatalf("AI text:\n got  %q\n want %q", fa.lastContent.Text, want)
	}

	// A SECOND joiner with the same ORIGINAL bio hits the flash cache: direct
	// kick, zero AI (the enriched text never became the cache key).
	fa2 := &fakeAI{}
	commentDrive(b, tg, 14, bio, commentMsg(-500, 14, 41, "评论二"))
	if len(tg.bans) != 2 {
		t.Fatalf("flash hit must kick the second carrier: %v", tg.bans)
	}
	_ = fa2
}

// --- 三链路 3: resolution failure -------------------------------------------------

// Join-bio whose @ lookup fails → judged as-is (no enrichment, no retry, no
// AI-failure accounting): the pre-round behavior.
func TestJoinBioResolveFailureJudgesAsIs(t *testing.T) {
	tg := newFakeTG()
	bio := "资源 @dead_channel"
	tg.chat[302] = telegram.ChatInfo{ID: 302, Bio: bio}
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.1)}
	b := testBot(t, tg, fa, resolveCfg())
	b.BioTarget = &fakeBioResolver{errs: map[string]error{
		"dead_channel": errors.New("telegram api getChat: chat not found (code 400)"),
	}}
	b.HandleNewMembers(context.Background(), -300, []telegram.User{u(302, false, "miss", "M")}, 0)

	// Judged as-is (original bio, no block), non-ad verdict, zero disposal.
	if fa.lastContent.Text != bio {
		t.Fatalf("failed lookup must judge the original bio: %q", fa.lastContent.Text)
	}
	if len(tg.bans) != 0 {
		t.Fatalf("lookup failure is a miss, not a judgment failure: %v", tg.bans)
	}
}
