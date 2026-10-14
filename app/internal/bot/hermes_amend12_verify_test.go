package bot

// Hermes 独立验收（amend12：关联频道帖子评论 → 审简介 + 防刷缓存）。
//
// 本文件不复用执行体的断言，目标是"证伪"执行体自述的四条：
//   H12-A 防刷 key 不能误伤：不同简介（含同模板不同链接、正常简介）必须互不命中
//   H12-B 非触发场景零成本：普通消息 / 回复普通消息 / 回复别人的评论 / 开关关闭
//         → getChat 0 次、AI 0 次、处置 0 次（回复别人的评论=当前覆盖缺口，显式钉住）
//   H12-C 防刷命中真省：AI 不变、仍查一次简介拿 key、处置照旧、群公告+频道推送带「防刷命中」
//   H12-D 同一用户连发 3 条同简介：AI 只花 1 次（防刷在真实刷屏场景的直接收益）
//   H12-E 判非广告的简介不去重：2 条评论 = 2 次 AI，缓存 0（用户明确的口径）
//   H12-F 窗口语义：29m59s 内命中、30m 整过期；容量上限淘汰最旧
//   H12-G 缓存的是简介、不是评论正文；判广告同时落 bio 签名库 + 防刷缓存

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/normalize"
	"adbot/internal/telegram"
)

// h12GetChatCalls counts GetChat invocations (the screen must fetch the bio).
func h12GetChatCalls(tg *fakeTG) int {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	n := 0
	for _, s := range tg.seq {
		if s == "getchat" {
			n++
		}
	}
	return n
}

// h12CountSends counts group sends containing needle.
func h12CountSends(tg *fakeTG, needle string) int {
	n := 0
	for _, s := range tg.sendsSnapshot() {
		if strings.Contains(s, needle) {
			n++
		}
	}
	return n
}

// h12NestedReply replies to ANOTHER person's comment, not to the channel post.
func h12NestedReply(chatID, uid, messageID int64) telegram.Message {
	m := adMsg(chatID, uid, messageID, "回复别人的评论")
	m.ReplyToMessage = &telegram.Message{
		MessageID: messageID - 1,
		Chat:      telegram.Chat{ID: chatID, Type: "supergroup"},
		Text:      "别人的评论",
	}
	return m
}

// --- H12-A: 防刷缓存不得误伤不同简介 ---------------------------------------------

func TestH12AFlashKeysNeverCollide(t *testing.T) {
	pairs := [][2]string{
		{"办证 加V t.me/xxx", "办证 加V t.me/yyy"},     // 同模板不同链接
		{"联系我 @alice", "联系我 @bob"},               // 同模板不同用户名
		{"我的频道 t.me/mychan", "我的博客 example.com"}, // 两个正常简介
		{"办证 加V t.me/xxx", "加V t.me/xxx"},        // 少一个词
	}
	for _, p := range pairs {
		k1, k2 := flashKeyOf(p[0]), flashKeyOf(p[1])
		if k1 == "" || k2 == "" {
			t.Fatalf("key 不能为空: %q/%q", p[0], p[1])
		}
		if k1 == k2 {
			t.Fatalf("误伤：%q 与 %q 归一化后同 key %s", p[0], p[1], k1[:12])
		}
	}
	// 全量互不碰撞（同一个简介在多对里重复出现是合法的，按内容去重后再比 key）
	byKey := map[string]string{}
	for _, bio := range []string{
		"办证 加V t.me/xxx", "办证 加V t.me/yyy", "联系我 @alice", "联系我 @bob",
		"我的频道 t.me/mychan", "我的博客 example.com", "加V t.me/xxx", "微信 kfw123 一天6k",
	} {
		k := flashKeyOf(bio)
		if prev, dup := byKey[k]; dup && prev != bio {
			t.Fatalf("误伤：%q 与 %q 归一化后同 key", prev, bio)
		}
		byKey[k] = bio
	}

	// 行为面：缓存了一条广告简介后，另一条"像但不同"的简介仍必须走 AI。
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, cfg)

	commentDrive(b, tg, 60, "办证 加V t.me/xxx", commentMsg(-500, 60, 90, "评论"))
	if fa.callCount() != 1 {
		t.Fatalf("第一条必须走 AI：%d", fa.callCount())
	}
	commentDrive(b, tg, 61, "办证 加V t.me/yyy", commentMsg(-500, 61, 91, "评论"))
	if fa.callCount() != 2 {
		t.Fatalf("不同简介被防刷缓存误伤（少了一次 AI 调用）：%d", fa.callCount())
	}
	commentDrive(b, tg, 62, "我的频道 t.me/mychan", commentMsg(-500, 62, 92, "评论"))
	if fa.callCount() != 3 {
		t.Fatalf("正常简介被误伤：%d", fa.callCount())
	}
	if _, _, hits, _ := commentStats(b); hits != 0 {
		t.Fatalf("不该有防刷命中：%d", hits)
	}
}

// --- H12-B: 非触发场景必须零成本 --------------------------------------------------

func TestH12BNonScopeMessagesCostNothing(t *testing.T) {
	cases := []struct {
		name  string
		build func(chatID, uid, messageID int64) telegram.Message
		on    bool
	}{
		{"普通消息(非回复)", func(c, u, m int64) telegram.Message { return adMsg(c, u, m, "大家好 abc.com") }, true},
		{"回复普通群消息", func(c, u, m int64) telegram.Message { return plainReplyMsg(c, u, m, "回复 abc.com") }, true},
		{"回复别人的评论(非直接回复帖子)", h12NestedReply, true},
		{"开关关闭+频道帖子评论", func(c, u, m int64) telegram.Message { return commentMsg(c, u, m, "评论 abc.com") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := newFakeTG()
			cfg := commentCfg()
			cfg.LogChat = -999
			cfg.CommentBioScreen = tc.on
			fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
			b := testBot(t, tg, fa, cfg)

			commentDrive(b, tg, 70, "办证 加V t.me/xxx", tc.build(-500, 70, 95))

			if n := h12GetChatCalls(tg); n != 0 {
				t.Fatalf("不该查简介：getChat %d 次", n)
			}
			if n := fa.callCount(); n != 0 {
				t.Fatalf("不该送 AI：%d 次", n)
			}
			if len(tg.bans) != 0 {
				t.Fatalf("不该处置：%v", tg.bans)
			}
			if len(tg.deletes) != 0 {
				t.Fatalf("不该删消息：%v", tg.deletes)
			}
			if _, triggered, hits, cached := commentStats(b); triggered != 0 || hits != 0 || cached != 0 {
				t.Fatalf("统计不该动：triggered=%d hits=%d cached=%d", triggered, hits, cached)
			}
		})
	}
}

// --- H12-C: 防刷命中真省（AI 零调用）且处置链完整 ---------------------------------

func TestH12CFlashHitZeroAIWithFullDisposalChain(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)
	h11ArmAnnouncer(t, b, tg, &h11AnnGen{text: "🧹 广告简介送客"})
	ctx := context.Background()

	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 80, bio, commentMsg(-500, 80, 100, "评论一"))
	if fa.callCount() != 1 || len(tg.bans) != 1 {
		t.Fatalf("第一条：AI=%d bans=%v", fa.callCount(), tg.bans)
	}
	if ok, _ := b.Store.HasBioSignature(ctx, normalize.Signature(bio)); !ok {
		t.Fatal("判广告的简介必须落 bio 签名库")
	}
	waitFor(t, func() bool { return h12CountSends(tg, "🧹 广告简介送客") >= 1 }, "第一条群内公告")

	// 第二条：另一个 uid、同一简介 → 防刷命中
	before := h12GetChatCalls(tg)
	commentDrive(b, tg, 81, bio, commentMsg(-500, 81, 101, "评论二"))

	if fa.callCount() != 1 {
		t.Fatalf("防刷命中必须零 AI 调用：%d", fa.callCount())
	}
	if got := h12GetChatCalls(tg) - before; got != 1 {
		t.Fatalf("命中仍需查一次简介拿 key：getChat %d 次", got)
	}
	if len(tg.bans) != 2 {
		t.Fatalf("命中必须直接踢：%v", tg.bans)
	}
	if len(tg.deletes) != 2 || tg.deletes[1] != "101" {
		t.Fatalf("命中必须删掉触发评论：%v", tg.deletes)
	}
	waitFor(t, func() bool { return h12CountSends(tg, "🧹 广告简介送客") >= 2 }, "第二条群内公告")
	waitFor(t, func() bool { return len(rec.texts()) >= 2 }, "第二条频道推送")
	if nb := rec.texts()[1]; !strings.Contains(nb, "防刷命中") || !strings.Contains(nb, "💬 "+bio) {
		t.Fatalf("频道推送缺「防刷命中」或证据行：\n%s", nb)
	}
	if _, triggered, hits, cached := commentStats(b); triggered != 2 || hits != 1 || cached != 1 {
		t.Fatalf("统计：triggered=%d hits=%d cached=%d，want 2/1/1", triggered, hits, cached)
	}
}

// --- H12-D: 同一用户连发 3 条同简介 = 只花 1 次 AI -------------------------------

func TestH12DSameUserBurstCostsOneAICall(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "微信收钱", 0.95)}
	b := testBot(t, tg, fa, cfg)

	bio := "微信 kfw123 t.me/xxx 一天6k"
	for i, id := range []int64{110, 111, 112} {
		commentDrive(b, tg, 90, bio, commentMsg(-500, 90, id, "刷屏"+string(rune('A'+i))))
	}
	if fa.callCount() != 1 {
		t.Fatalf("同一简介连发 3 条只该花 1 次 AI：%d", fa.callCount())
	}
	if len(tg.bans) != 3 {
		t.Fatalf("三条都要处置：%d", len(tg.bans))
	}
	if _, _, hits, _ := commentStats(b); hits != 2 {
		t.Fatalf("后两条应命中防刷：%d", hits)
	}
}

// --- H12-E: 判非广告的简介不去重（用户口径） -------------------------------------

func TestH12ENonAdBioIsNeverDeduped(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, cfg)

	bio := "我的频道 t.me/mychan"
	for _, id := range []int64{120, 121} {
		commentDrive(b, tg, 95, bio, commentMsg(-500, 95, id, "评论"))
	}
	if fa.callCount() != 2 {
		t.Fatalf("非广告简介不去重，两次都该走 AI：%d", fa.callCount())
	}
	if len(tg.bans) != 0 {
		t.Fatalf("非广告零处置：%v", tg.bans)
	}
	if _, _, hits, cached := commentStats(b); hits != 0 || cached != 0 {
		t.Fatalf("非广告不入缓存：hits=%d cached=%d", hits, cached)
	}
	if ok, _ := b.Store.HasBioSignature(context.Background(), normalize.Signature(bio)); ok {
		t.Fatal("非广告简介不得写入签名库")
	}
}

// --- H12-F: 窗口语义（30m 固定窗口）+ 容量上限 ------------------------------------

func TestH12FWindowAndCapacitySemantics(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newCommentBioFlash(30*time.Minute, 3)
	c.setClock(func() time.Time { return now })
	c.put("k1", "诈骗")

	now = now.Add(29*time.Minute + 59*time.Second)
	if _, ok := c.hit("k1"); !ok {
		t.Fatal("29m59s 内应命中")
	}
	now = now.Add(1 * time.Second) // 正好 30m
	if _, ok := c.hit("k1"); ok {
		t.Fatal("30m 整应过期（重新送 AI）")
	}
	if c.Len() != 0 {
		t.Fatalf("过期条目应被惰性清理：%d", c.Len())
	}

	// 固定窗口：重复出现不续命（不是滑动窗口）
	c2 := newCommentBioFlash(30*time.Minute, 10)
	c2.setClock(func() time.Time { return now })
	c2.put("kx", "诈骗")
	now = now.Add(29 * time.Minute)
	if _, ok := c2.hit("kx"); !ok {
		t.Fatal("29m 应命中")
	}
	now = now.Add(2 * time.Minute) // 距写入 31m，若滑动窗口则仍在窗口内
	if _, ok := c2.hit("kx"); ok {
		t.Fatal("固定窗口：命中不应续命，31m 后必须过期")
	}

	// 容量：超出上限淘汰最旧
	c3 := newCommentBioFlash(time.Hour, 2)
	c3.setClock(func() time.Time { return now })
	c3.put("a", "诈骗")
	c3.put("b", "诈骗")
	c3.put("c", "诈骗")
	if c3.Len() != 2 {
		t.Fatalf("容量上限 2：%d", c3.Len())
	}
	if _, ok := c3.hit("a"); ok {
		t.Fatal("最旧的应被淘汰")
	}
	if _, ok := c3.hit("b"); !ok {
		t.Fatal("较新的应保留")
	}
}

// --- H12-G: 缓存的是简介（不是评论正文），且默认窗口 30m -------------------------

func TestH12GFlashStoresBioNotMessageText(t *testing.T) {
	tg := newFakeTG()
	cfg := commentCfg()
	cfg.LogChat = -999
	fa := &fakeAI{verdict: fakeVerdict(true, "诈骗", 0.95)}
	b := testBot(t, tg, fa, cfg)

	bio := "办证 加V t.me/xxx"
	commentDrive(b, tg, 100, bio, commentMsg(-500, 100, 130, "这条评论正文本身不带链接"))

	if _, ok := b.commentFlash.hit(flashKeyOf(bio)); !ok {
		t.Fatal("缓存里必须有【简介】的 key")
	}
	if _, ok := b.commentFlash.hit(flashKeyOf("这条评论正文本身不带链接")); ok {
		t.Fatal("绝不能把评论正文当成简介缓存（会误杀拿同句话评论的人）")
	}
	if d := newCommentBioFlash(0, 0).ttl; d != 30*time.Minute {
		t.Fatalf("未配置时默认窗口应为 30m：%s", d)
	}
	if _, _, _, cached := commentStats(b); cached != 1 {
		t.Fatalf("缓存条目数：%d", cached)
	}
}
