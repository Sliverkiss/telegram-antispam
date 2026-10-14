package bot

// Hermes 独立验收（amend9 入群「裸号」必杀判据）。不依赖执行体写的测试：
//   1. 四条件真值表：16 种组合里**恰好 1 种**命中
//   2. 英文名边界：混汉字/全角/假名/谚文/纯数字/单字母/emoji 全试一遍
//   3. 端到端：premium 只来自 getChatMember（update 不带该字段）→ 仍要杀
//      且要真的：ban+unban(踢) + 通知证据四条 + **群内 AI 公告** + 删入群消息
//   4. 误杀防线：中文名 / 有头像 / 有简介 各自都不能杀
//   5. 开关关闭 → 不杀，回落到原 AI 流程

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/telegram"
)

// --- 1. 真值表：只有 premium && english && noBio && noPhoto 命中 --------------

func TestHermesJoinRuleTruthTable(t *testing.T) {
	hits, wanted := 0, 0
	for mask := 0; mask < 16; mask++ {
		premium := mask&1 != 0
		english := mask&2 != 0
		hasBio := mask&4 != 0
		hasPhoto := mask&8 != 0

		first := "Lynne"
		if !english {
			first = "财源广进"
		}
		bio := ""
		if hasBio {
			bio = "我是正常用户"
		}
		hit, why := joinRuleHit(premium, first, "Bla", bio, hasPhoto)
		want := premium && english && !hasBio && !hasPhoto
		if want {
			wanted++
		}
		if hit != want {
			t.Errorf("mask=%04b(premium=%v english=%v bio=%v photo=%v): hit=%v want=%v",
				mask, premium, english, hasBio, hasPhoto, hit, want)
		}
		if hit {
			hits++
			for _, frag := range []string{"Premium会员", "英文名", "无简介", "无头像"} {
				if !strings.Contains(why, frag) {
					t.Errorf("证据串缺 %q: %s", frag, why)
				}
			}
		} else if why != "" {
			t.Errorf("未命中时 why 必须为空，got %q", why)
		}
	}
	if hits != 1 || wanted != 1 {
		t.Fatalf("真值表必须恰好 1 种命中，得到 hits=%d wanted=%d", hits, wanted)
	}
}

// --- 2. 英文名边界（我自己的用例）-------------------------------------------

func TestHermesEnglishNameBoundaries(t *testing.T) {
	cases := []struct {
		first, last string
		want        bool
	}{
		{"Lynne", "Bla", true},
		{"kevin", "", true},
		{"A.", "", false},        // 只有 1 个字母
		{"", "", false},          // 空
		{"   ", "", false},       // 空白
		{"财源广进", "", false},      // 纯中文
		{"Ahsley财", "", false},   // 英文混汉字
		{"Александр", "", false}, // 西里尔（非 ASCII 拉丁）
		{"こい", "", false},        // 平假名
		{"한글", "", false},        // 谚文
		{"ＡＢＣ", "", false},       // 全角拉丁
		{"123456", "", false},    // 纯数字
		{"🎉🎉", "", false},        // 纯 emoji
		{"Ashl❤️", "", true},     // 字母 + emoji → 算英文
		{"🐔王", "Wang", false},    // 汉字混英文
		{"John", "Smith", true},  // 标准英文全名
		{"Bob123", "", true},     // 字母 + 数字
		{"𝕷𝖞𝖓𝖓𝖊", "", false},     // 花体字母（非 ASCII）
	}
	for _, c := range cases {
		if got := isEnglishName(c.first, c.last); got != c.want {
			t.Errorf("isEnglishName(%q,%q) = %v, want %v", c.first, c.last, got, c.want)
		}
	}
}

// --- 3. 端到端：premium 只来自 getChatMember，仍要杀，且公告/通知/踢都要真发生 --

func TestHermesJoinRuleE2EFromGetChatMember(t *testing.T) {
	const uid, chatID, joinMsg = int64(7001), int64(-500), int64(77)

	tg := newFakeTG()
	tg.chat[uid] = telegram.ChatInfo{ID: uid} // 空简介 + 无头像
	// update 里【不带】is_premium（模拟 Telegram 不在 new_chat_members 里填充）
	// 只有 getChatMember 能告诉我们他是会员 —— 这正是容易漏判的场景
	tg.member[fkey(chatID, uid)] = telegram.ChatMember{
		User: telegram.User{ID: uid, FirstName: "Lynne", LastName: "Bla", IsPremium: true},
	}

	cfg := defaultCfg()
	cfg.Action = "kick"
	cfg.LogChat = -999
	cfg.DeleteJoinMsg = true
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	rec := wireTestNotifier(t, b, cfg)

	ann := &fakeAnnouncer{out: "🚪 Lynne Bla 已被请出门外，广告请去别处发～"}
	b.Announcer = NewAnnouncer(tg, ann, true, time.Second, time.Second,
		logx.New(nil, logx.Error, "text", "announce"), nil, b)

	ctx := context.Background()
	b.HandleNewMembers(ctx, chatID, []telegram.User{u(uid, false, "naked25chars", "Lynne")}, joinMsg)

	// (a) 踢出 = ban + unban
	waitFor(t, func() bool { return len(tg.banList()) == 2 }, "kick = ban+unban")
	if got := tg.banList(); got[0] != "ban" || got[1] != "unban" {
		t.Fatalf("kick 必须先 ban 再 unban，got %v", got)
	}

	// (b) 通知频道：证据串四条 + 100 分
	waitFor(t, func() bool { return len(rec.texts()) >= 1 }, "通知推送")
	nb := rec.texts()[0]
	for _, frag := range []string{"💬 🔎 Premium会员", "英文名", "无简介", "无头像", "100分", "已踢出"} {
		if !strings.Contains(nb, frag) {
			t.Errorf("通知缺 %q:\n%s", frag, nb)
		}
	}
	if n := strings.Count(nb, "\n"); n != 3 {
		t.Errorf("通知必须是 4 行模板，got %d 行:\n%s", n+1, nb)
	}

	// (c) 群内 AI 公告真的发出去了
	waitFor(t, func() bool {
		for _, s := range tg.sendsSnapshot() {
			if strings.Contains(s, "Lynne Bla") {
				return true
			}
		}
		return false
	}, "群内 AI 公告")
	if ann.callCount() == 0 {
		t.Error("公告器一次都没被调用")
	}

	// (d) 入群消息被删
	waitFor(t, func() bool { return len(tg.deleteList()) == 1 }, "删入群消息")
	if got := tg.deleteList(); got[0] != fmt.Sprint(joinMsg) {
		t.Errorf("删的不是入群消息: %v", got)
	}

	// (e) 命中就不该再走 AI
	if fa.callCount() != 0 {
		t.Errorf("直接杀路径不该调 AI，却被调了 %d 次", fa.callCount())
	}
	if enabled, hits := b.JoinRuleStats(); !enabled || hits != 1 {
		t.Errorf("JoinRuleStats = (%v,%d), want (true,1)", enabled, hits)
	}
}

// --- 4. 误杀防线 ------------------------------------------------------------

func TestHermesJoinRuleFalsePositiveGuards(t *testing.T) {
	cases := []struct {
		name    string
		first   string
		last    string
		bio     string
		photo   bool
		wantBan bool
	}{
		{"全中_该杀", "Lynne", "Bla", "", false, true},
		{"中文名_不杀", "财源广进", "", "", false, false},
		{"有名有姓的中文_不杀", "王", "小明", "", false, false},
		{"有头像_不杀", "Lynne", "Bla", "", true, false},
		{"有简介_不杀", "Lynne", "Bla", "养鸡专业户", false, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid := int64(7100 + i)
			tg := newFakeTG()
			tg.chat[uid] = telegram.ChatInfo{ID: uid, Bio: c.bio, HasPhoto: c.photo}
			cfg := defaultCfg()
			cfg.Action = "kick"
			fa := &fakeAI{}
			b := testBot(t, tg, fa, cfg)

			// premium 从 getChatMember 来
			tg.member[fkey(-500, uid)] = telegram.ChatMember{
				User: telegram.User{ID: uid, FirstName: c.first, LastName: c.last, IsPremium: true},
			}
			b.HandleNewMembers(context.Background(), -500,
				[]telegram.User{u(uid, false, "someone", c.first)}, 0)

			if c.wantBan {
				waitFor(t, func() bool { return len(tg.banList()) == 2 }, "该杀却没杀")
				return
			}
			time.Sleep(120 * time.Millisecond)
			if got := tg.banList(); len(got) != 0 {
				t.Fatalf("误杀！%s 不该被踢，却执行了 %v", c.name, got)
			}
			if c.bio != "" && fa.callCount() == 0 {
				t.Errorf("有简介的正常用户应继续走 AI 审核，AI 却没被调用")
			}
		})
	}
}

// --- 5. 开关关闭 → 不杀，回落原流程 ------------------------------------------

func TestHermesJoinRuleDisabledSwitch(t *testing.T) {
	tg := newFakeTG()
	tg.chat[int64(7200)] = telegram.ChatInfo{ID: 7200}
	cfg := defaultCfg()
	cfg.JoinNakedPremium = false
	cfg.Action = "kick"
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	tg.member[fkey(-500, 7200)] = telegram.ChatMember{
		User: telegram.User{ID: 7200, FirstName: "Lynne", LastName: "Bla", IsPremium: true},
	}
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u(7200, false, "x", "Lynne")}, 0)
	time.Sleep(120 * time.Millisecond)
	if got := tg.banList(); len(got) != 0 {
		t.Fatalf("开关关闭时不该杀，却执行了 %v", got)
	}
	if _, hits := b.JoinRuleStats(); hits != 0 {
		t.Errorf("开关关闭时命中计数必须为 0，got %d", hits)
	}
}
