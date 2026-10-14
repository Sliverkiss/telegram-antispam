// The amend9 join rule: 入群「裸号」必杀.
//
// A Telegram Premium subscriber that joins with an English-looking display
// name, an empty bio and no profile photo is a mass-produced ad account with
// near certainty (the exact shape of the production samples that motivated
// this rule). The rule is a strict four-way AND:
//
//  1. is_premium == true  (Bot API omits the field when false — measured)
//  2. English name        (≥2 ASCII latin letters, zero CJK characters)
//  3. empty bio           (getChat)
//  4. no profile photo    (getChat photo)
//
// All four → direct disposal (source=join_rule, category=广告, score=100),
// skipping the AI round entirely. Any miss → the normal screening flow.
// The production samples "Lynne Bla" / "Ahsley Bee" (premium + English name
// but WITH bio and photo) must NOT hit — they stay covered by the
// bio-with-link signature path.
//
// is_premium sourcing: the update's new_chat_members is NOT trusted alone
// (whether the library populates it there is unverified); screenNewMember
// unconditionally re-fetches getChatMember and ORs the two.
package bot

import (
	"fmt"
	"strings"
	"unicode"
)

// isEnglishName reports whether the display name looks Latin/English:
// at least two ASCII letters [A-Za-z] after trimming, and no CJK characters
// (Han, Kana, Hangul). Fullwidth latin letters (Ａ) count as non-latin, as
// does everything outside the ASCII range — only [A-Za-z] accumulates the
// letter count, and one CJK rune disqualifies the whole name.
func isEnglishName(first, last string) bool {
	name := strings.TrimSpace(first + " " + last)
	letters := 0
	for _, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			letters++
			continue
		}
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return false
		}
	}
	return letters >= 2
}

// joinRuleHit evaluates the four-way AND. The returned why is the evidence
// string for the disposition notice's 💬 line, e.g.:
//
//	🔎 Premium会员 · 英文名 "Lynne Bla" · 无简介 · 无头像
func joinRuleHit(premium bool, first, last, bio string, hasPhoto bool) (hit bool, why string) {
	english := isEnglishName(first, last)
	noBio := strings.TrimSpace(bio) == ""
	if premium && english && noBio && !hasPhoto {
		name := strings.TrimSpace(first + " " + last)
		return true, fmt.Sprintf("🔎 Premium会员 · 英文名 %q · 无简介 · 无头像", name)
	}
	return false, ""
}

// missReason names the first failed leg (join_rule_miss_reason debug log).
// Empty string cannot happen: the caller only logs when the rule is armed and
// the user was worth evaluating, so at least one leg must differ.
func missReason(premium, english, noBio, hasPhoto bool) string {
	switch {
	case !premium:
		return "not_premium"
	case !english:
		return "not_english_name"
	case !noBio:
		return "has_bio"
	case hasPhoto:
		return "has_photo"
	}
	return ""
}
