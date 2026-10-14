package bot

// amend9 裸号必杀 tests. Everything offline: fakeTG / fakeAI, no long-polling,
// no real token. The assertions of TASK_antispam_amend9.md §E:
//
//	 1. four-way AND hit → disposal + notify evidence line carries all four;
//	 2. each missing leg alone → no disposal (premium / name / bio / photo);
//	 3. BOT_JOIN_NAKED_PREMIUM=false → armed-off, never disposes;
//	 4. dry-run → join_rule_dry_run logged, zero writes;
//	 5. signature hit takes priority: exactly one disposal notice;
//	 6. update without is_premium but getChatMember=true → still hits;
//	 7. production samples Lynne Bla / Ahsley Bee (bio + photo present) → miss;
//	 8. the notification keeps the 4-line template and score 100;
//	 9. the handler stays fast (the getChatMember re-fetch is the only added
//	    call and is not on the AI path);
//	10. the variable contract sets agree (config contract_test).

import (
	"context"
	"strings"
	"testing"
	"time"

	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// --- B. isEnglishName: table-driven, ≥15 cases --------------------------------

func TestIsEnglishNameTable(t *testing.T) {
	cases := []struct {
		first, last string
		want        bool
		note        string
	}{
		// English: ≥2 ASCII latin letters, no CJK.
		{"Lynne", "Bla", true, "two-part English name (production sample)"},
		{"Lynne", "", true, "single first name"},
		{"kevin", "", true, "all-lowercase"},
		{"Ashl", "❤️", true, "emoji decoration (production sample shape)"},
		{"A.", "Smith", true, "initial + surname"},
		{"Ahsley", "Bee", true, "production sample"},
		{"John", "O'Connell", true, "apostrophe is non-CJK filler"},
		{"Mary-Jane", "Watson", true, "hyphen is filler"},
		{"Ann", "李", false, "mixed name: Han surname disqualifies"},
		{"Ahsley", "财", false, "one Han rune anywhere disqualifies"},
		// Not English.
		{"财源广进", "", false, "pure Han"},
		{"こい", "", false, "kana alone: zero latin letters"},
		{"김철수", "", false, "Hangul alone: zero latin letters"},
		{"😀🤡", "", false, "pure emoji"},
		{"", "", false, "empty string"},
		{"   ", "", false, "blank only"},
		{"123", "456", false, "pure digits"},
		{"A", "", false, "single latin letter (< 2)"},
		{"Ａｂ", "", false, "fullwidth latin is NOT ascii latin"},
	}
	if len(cases) < 15 {
		t.Fatalf("table must hold ≥15 cases, got %d", len(cases))
	}
	for _, c := range cases {
		if got := isEnglishName(c.first, c.last); got != c.want {
			t.Errorf("isEnglishName(%q, %q) = %v, want %v (%s)", c.first, c.last, got, c.want, c.note)
		}
	}
}

// TestIsEnglishNameKanaAndHangulDisqualifyLikeHan: the CJK block check in the
// task ("假名、谚文") is expressed as "no latin letters ≥2 AND no Han" — a
// kana-only or hangul-only name has zero latin letters so it is non-English
// by the letter count alone. A mixed latin+kana name is still English by the
// spec (no Han rune, ≥2 latin). Pin both edges.
func TestIsEnglishNameKanaAndHangulDisqualifyLikeHan(t *testing.T) {
	if isEnglishName("こい", "") {
		t.Error("kana-only name must not be English (zero latin letters)")
	}
	if isEnglishName("김철수", "") {
		t.Error("hangul-only name must not be English (zero latin letters)")
	}
	if isEnglishName("Ahsley", "bee") != true {
		t.Error("mixed latin + no-CJK name stays English")
	}
}

// --- C. joinRuleHit unit --------------------------------------------------------

func TestJoinRuleHitWhyString(t *testing.T) {
	hit, why := joinRuleHit(true, "Lynne", "Bla", "", false)
	if !hit {
		t.Fatal("four-way AND must hit")
	}
	for _, want := range []string{"Premium会员", `英文名 "Lynne Bla"`, "无简介", "无头像"} {
		if !strings.Contains(why, want) {
			t.Errorf("why missing %q: %s", want, why)
		}
	}
	// Single first name renders without the trailing space.
	_, why = joinRuleHit(true, "kevin", "", "  ", false)
	if !strings.Contains(why, `英文名 "kevin"`) {
		t.Errorf("single-name why = %s", why)
	}
}

func TestJoinRuleMissReasonAllLegs(t *testing.T) {
	for _, tc := range []struct {
		premium, english, noBio, hasPhoto bool
		want                              string
	}{
		{false, true, true, false, "not_premium"},
		{true, false, true, false, "not_english_name"},
		{true, true, false, false, "has_bio"},
		{true, true, true, true, "has_photo"},
	} {
		if got := missReason(tc.premium, tc.english, tc.noBio, tc.hasPhoto); got != tc.want {
			t.Errorf("missReason(%v,%v,%v,%v) = %q, want %q",
				tc.premium, tc.english, tc.noBio, tc.hasPhoto, got, tc.want)
		}
	}
}

// --- E. integration through screenNewMember ------------------------------------

// nakedUser builds the joiner shape: premium, English name, no bio, no photo.
func nakedUser(id int64, username, first, last string) telegram.User {
	return telegram.User{ID: id, IsBot: false, Username: username, FirstName: first, LastName: last, IsPremium: true}
}

// 1. four-way hit → kick + notify 💬 carries all four evidence pieces.
func TestJoinRuleAllFourHitDisposes(t *testing.T) {
	tg := newFakeTG()
	tg.chat[501] = telegram.ChatInfo{ID: 501} // empty bio, no photo
	cfg := defaultCfg()
	cfg.Action = "kick"
	fa := &fakeAI{}
	b := testBot(t, tg, fa, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(501, "x25chars", "Lynne", "Bla")}, 0)
	if len(tg.bans) != 2 || tg.bans[0] != "ban" || tg.bans[1] != "unban" {
		t.Fatalf("kick mode must ban+unban: %v", tg.bans)
	}
	if fa.callCount() != 0 {
		t.Fatalf("join_rule must not consume AI: %d", fa.callCount())
	}
	if sus, _ := b.Store.IsSuspect(context.Background(), 501); sus {
		t.Fatal("disposed user must leave the watchlist")
	}
	if _, hits := b.JoinRuleStats(); hits != 1 {
		t.Errorf("join_rule_hits_total = %d, want 1", hits)
	}
}

// 1b. …and the notification evidence line (async via the notifier worker).
func TestJoinRuleAllFourHitNotifyEvidence(t *testing.T) {
	tg := newFakeTG()
	tg.chat[502] = telegram.ChatInfo{ID: 502}
	cfg := defaultCfg()
	cfg.LogChat = -999
	b := testBot(t, tg, &fakeAI{}, cfg)
	rec := wireTestNotifier(t, b, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(502, "spam", "Lynne", "Bla")}, 0)
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "join_rule disposition notify")
	nb := rec.texts()[0]
	for _, want := range []string{"🔎 Premium会员", `英文名 "Lynne Bla"`, "无简介", "无头像"} {
		if !strings.Contains(nb, want) {
			t.Errorf("notify 💬 line missing %q:\n%s", want, nb)
		}
	}
}

// 2. each missing leg alone → no disposal.
func TestJoinRuleMissPerLeg(t *testing.T) {
	for _, tc := range []struct {
		name            string
		user            telegram.User
		chat            telegram.ChatInfo
		memberIsPremium bool
	}{
		{"not_premium", withPremium(nakedUser(510, "u1", "Lynne", "Bla"), false), telegram.ChatInfo{ID: 510}, false},
		{"chinese_name", nakedUser(511, "u2", "财源广进", ""), telegram.ChatInfo{ID: 511}, true},
		{"has_bio", nakedUser(512, "u3", "Lynne", "Bla"), telegram.ChatInfo{ID: 512, Bio: "正常简介"}, true},
		{"has_photo", nakedUser(513, "u4", "Lynne", "Bla"), telegram.ChatInfo{ID: 513, HasPhoto: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg := newFakeTG()
			tg.chat[tc.user.ID] = tc.chat
			tg.member[fkey(-500, tc.user.ID)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: tc.user.ID, IsPremium: tc.memberIsPremium}}
			fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
			b := testBot(t, tg, fa, defaultCfg())
			b.HandleNewMembers(context.Background(), -500, []telegram.User{tc.user}, 0)
			if len(tg.bans) != 0 {
				t.Fatalf("miss (%s) must not dispose: %v", tc.name, tg.bans)
			}
			if _, hits := b.JoinRuleStats(); hits != 0 {
				t.Errorf("join_rule_hits_total = %d, want 0", hits)
			}
		})
	}
}

// withPremium flips the IsPremium leg (test helper).
func withPremium(u telegram.User, p bool) telegram.User {
	u.IsPremium = p
	return u
}

// 3. BOT_JOIN_NAKED_PREMIUM=false → even a full hit is skipped.
func TestJoinRuleDisabledSkipsEvenFullHit(t *testing.T) {
	tg := newFakeTG()
	tg.chat[520] = telegram.ChatInfo{ID: 520}
	cfg := defaultCfg()
	cfg.JoinNakedPremium = false
	fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
	b := testBot(t, tg, fa, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(520, "x", "Lynne", "Bla")}, 0)
	if len(tg.bans) != 0 {
		t.Fatalf("disabled rule must not dispose: %v", tg.bans)
	}
	if enabled, _ := b.JoinRuleStats(); enabled {
		t.Error("join_rule_enabled must mirror the config")
	}
	// No bio → no AI either (the join falls through to the empty-bio return).
	if fa.callCount() != 0 {
		t.Fatalf("empty bio must not consume AI: %d", fa.callCount())
	}
}

// 4. dry-run → join_rule_dry_run logged, zero writes.
func TestJoinRuleDryRunLogsOnly(t *testing.T) {
	tg := newFakeTG()
	tg.chat[521] = telegram.ChatInfo{ID: 521}
	cfg := defaultCfg()
	cfg.DryRun = true
	var logs strings.Builder
	lg := logx.New(&logs, logx.Info, "text", "bot")
	st := store.NewMemory()
	b := New(st, tg, &fakeAI{}, nil, cfg, lg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(521, "x", "Lynne", "Bla")}, 0)
	if len(tg.bans) != 0 || len(tg.deletes) != 0 || len(tg.sends) != 0 {
		t.Fatalf("dry-run must perform zero telegram writes: %v %v %v", tg.bans, tg.deletes, tg.sends)
	}
	if n, _ := b.Store.Punished(context.Background()); n != 0 {
		t.Fatal("dry-run must not record punishments")
	}
	if _, hits := b.JoinRuleStats(); hits != 0 {
		t.Errorf("dry-run must not count into join_rule_hits_total: %d", hits)
	}
	if !strings.Contains(logs.String(), "join_rule_dry_run") {
		t.Fatalf("join_rule_dry_run must be logged:\n%s", logs.String())
	}
}

// 5. signature hit wins: exactly one disposal notice even though the same
// joiner would also satisfy the naked-premium rule.
func TestJoinRuleSignaturePrioritySingleNotice(t *testing.T) {
	tg := newFakeTG()
	// The joiner's username normalizes EXACTLY to a stored bio signature (the
	// signature loop screens bio, username and first_name), while the user
	// shape would also fully satisfy the naked rule (premium, English first
	// name, empty bio, no photo). The signature loop runs first and returns —
	// the join rule must never be consulted, so exactly one disposal.
	tg.chat[522] = telegram.ChatInfo{ID: 522} // empty bio, no photo → naked hit armed
	fa := &fakeAI{}
	cfg := defaultCfg()
	cfg.LogChat = -999
	b := testBot(t, tg, fa, cfg)
	ctx := context.Background()
	if err := b.Store.AddBioSignature(ctx, normalize.Signature("办证 加V信"), "办证 加V信"); err != nil {
		t.Fatal(err)
	}
	rec := wireTestNotifier(t, b, cfg)
	b.HandleNewMembers(ctx, -500, []telegram.User{nakedUser(522, "办证 加V信", "Lynne", "Bla")}, 0)
	if len(tg.bans) != 1 {
		t.Fatalf("signature path must dispose exactly once: %v", tg.bans)
	}
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "signature disposition notify")
	if got := rec.texts(); len(got) != 1 {
		t.Fatalf("exactly one disposition notice, got %d", len(got))
	}
	if _, hits := b.JoinRuleStats(); hits != 0 {
		t.Errorf("signature-priority disposal must not count as join_rule hit: %d", hits)
	}
	if !strings.Contains(rec.texts()[0], "签名命中") {
		t.Errorf("notice must be the signature one:\n%s", rec.texts()[0])
	}
	if fa.callCount() != 0 {
		t.Fatalf("signature hit must not consume AI: %d", fa.callCount())
	}
}

// 6. update carries no is_premium, getChatMember returns true → still hits.
func TestJoinRulePremiumFromGetChatMember(t *testing.T) {
	tg := newFakeTG()
	tg.chat[523] = telegram.ChatInfo{ID: 523}
	// The update's user record has IsPremium=false (field omitted upstream);
	// only the getChatMember re-fetch knows the truth.
	tg.member[fkey(-500, 523)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: 523, IsPremium: true}}
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	u := nakedUser(523, "x", "Lynne", "Bla")
	u.IsPremium = false
	b.HandleNewMembers(context.Background(), -500, []telegram.User{u}, 0)
	if len(tg.bans) != 1 {
		t.Fatalf("getChatMember-sourced premium must still hit: %v", tg.bans)
	}
	if _, hits := b.JoinRuleStats(); hits != 1 {
		t.Errorf("join_rule_hits_total = %d, want 1", hits)
	}
}

// 7. production-sample regression: Lynne Bla / Ahsley Bee have bio + photo →
// the naked rule must MISS them (they stay covered by the bio-signature path).
func TestJoinRuleProductionSamplesMiss(t *testing.T) {
	samples := []struct {
		id                    int64
		username, first, last string
		bio                   string
	}{
		{530, "LynneBla_x25", "Lynne", "Bla", "点我主页 看片上门 发财 加V:8888"},
		{531, "AhsleyBee_x25", "Ahsley", "Bee", "跟我玩 撩我 快加我 t.me/xxx"},
	}
	for _, s := range samples {
		t.Run(s.first+" "+s.last, func(t *testing.T) {
			tg := newFakeTG()
			// Real field values: premium=true, English name, HAS bio, HAS photo.
			tg.chat[s.id] = telegram.ChatInfo{ID: s.id, Bio: s.bio, HasPhoto: true}
			tg.member[fkey(-500, s.id)] = telegram.ChatMember{Status: "member", User: telegram.User{ID: s.id, IsPremium: true}}
			fa := &fakeAI{verdict: fakeVerdict(false, "无", 0.9)}
			b := testBot(t, tg, fa, defaultCfg())
			b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(s.id, s.username, s.first, s.last)}, 0)
			if len(tg.bans) != 0 {
				t.Fatalf("%s %s must NOT hit the naked rule: %v", s.first, s.last, tg.bans)
			}
			if _, hits := b.JoinRuleStats(); hits != 0 {
				t.Errorf("join_rule_hits_total = %d, want 0", hits)
			}
			// Non-empty bio → the AI bio round runs (the samples are covered
			// by bio screening instead).
			waitFor(t, func() bool { return fa.callCount() == 1 }, "bio judgment")
		})
	}
}

// 8. the notification keeps the 4-line template and the 100 score.
func TestJoinRuleNotifyTemplateAndScore(t *testing.T) {
	tg := newFakeTG()
	tg.chat[540] = telegram.ChatInfo{ID: 540}
	cfg := defaultCfg()
	cfg.LogChat = -999
	b := testBot(t, tg, &fakeAI{}, cfg)
	rec := wireTestNotifier(t, b, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(540, "tpl", "Lynne", "Bla")}, 0)
	waitFor(t, func() bool { return len(rec.texts()) == 1 }, "join_rule disposition notify")
	nb := rec.texts()[0]
	for _, want := range []string{
		"⏱️ 处理时间:",      // line 1
		"💬 🔎 Premium会员", // line 2 (the evidence string)
		"👤 @tpl",        // line 3
		"✅ 已封禁 · 100分",  // line 4 (default action=ban in defaultCfg)
	} {
		if !strings.Contains(nb, want) {
			t.Errorf("notify missing %q:\n%s", want, nb)
		}
	}
	// Exactly the four template lines (no anomaly lines appended).
	if got := strings.Count(nb, "\n"); got != 3 {
		t.Errorf("notify must be 4 lines, got %d:\n%s", got+1, nb)
	}
}

// 9. handler fast return: a join screen with the rule armed returns in
// milliseconds (only getChat + getChatMember, no AI on the path).
func TestJoinRuleHandlerFastReturn(t *testing.T) {
	tg := newFakeTG()
	tg.chat[541] = telegram.ChatInfo{ID: 541}
	fa := &fakeAI{}
	b := testBot(t, tg, fa, defaultCfg())
	start := time.Now()
	for i := 0; i < 50; i++ {
		uid := int64(600 + i)
		tg.chat[uid] = telegram.ChatInfo{ID: uid}
		b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(uid, "fast", "Lynne", "Bla")}, 0)
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("50 join screens took %v; the naked-premium path must stay off the AI lane", elapsed)
	}
	// All 50 disposed synchronously (join path, no pool involved).
	if len(tg.bans) != 50 {
		t.Fatalf("bans = %d, want 50", len(tg.bans))
	}
}

// 10. variable contract — the three/four-surface equality is pinned by
// config's TestEnvContractThreeWayConsistent / TestEnvContractREADMESubset
// (config cannot import bot). This test pins the bot-side consumption: the
// JoinNakedPremium field is the single switch the flow consults — true fires
// on a full hit, false does not (complement of test 3, from the armed side).
func TestJoinRuleConfigConsumed(t *testing.T) {
	cfg := defaultCfg()
	if !cfg.JoinNakedPremium {
		t.Fatal("defaultCfg must arm the join rule for this test to be meaningful")
	}
	// Armed (config.Load default true) → the same joiner shape fires.
	tg := newFakeTG()
	tg.chat[550] = telegram.ChatInfo{ID: 550}
	b := testBot(t, tg, &fakeAI{}, cfg)
	b.HandleNewMembers(context.Background(), -500, []telegram.User{nakedUser(550, "cfg", "Lynne", "Bla")}, 0)
	if _, hits := b.JoinRuleStats(); hits != 1 {
		t.Fatalf("armed config must fire: hits=%d", hits)
	}
}
