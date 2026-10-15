// The bio-resolve round: 简介 @ 解析.
//
// User's verbatim requirement: "简介有@的，我们可以获取一下@的链接，也就是对应
// https://t.me的，提取链接对应东西的简介页喂给bot判断吧，这样更稳一点" —
// before a bio goes to the AI review, every @handle it carries is resolved via
// getChat (the t.me target of the mention) and the target's own bio (user
// target) or description (channel/supergroup target) is appended to the
// judged text as a 【相关简介】 block. The prompt (judge_v3) weighs that block
// with the same category list — a bio that only says "私聊机器人：@xxx_bot"
// resolves to an innocuous target and passes; a bio whose @ resolves to a
// 博彩/卖资源 channel dies with the target's own text as evidence.
//
// Scope (deliberate): resolution is a JUDGMENT-INPUT enrichment only.
//   - The ORIGINAL bio remains the signature-library text, the 防刷 flash-cache
//     key, and the disposition-notice evidence (judgmentInput.sigText): the
//     punishment chain, signature writes and flash cache stay byte-for-byte
//     keyed on what the user actually wrote, never on what someone else's bio
//     said (a third party's channel bio changing must not corrupt our
//     signature library).
//   - Whether a bio is SENT to the AI is unchanged (the 【5】 trigger gate:
//     bio has @ / has link; joining-with-bio, comment bio — same conditions).
//     This module never decides submission, only enriches the text.
//   - A failed lookup is a MISS, never a failure of the judgment: the bio is
//     judged as-is (fallback = pre-round behavior). Retries are NOT consumed.
//   - Non-@ links (http/t.me/bare domains) are not resolved this round: the
//     user's wording targets @ mentions ("简介有@的"); t.me/xxx links ride the
//     same prompt via the text itself. Extension point: handleFromLink.
//
// Contract:
//   - Extract: @handles via scanHandles — "the @ token + [A-Za-z0-9_]{5,}"
//     and must be followed by a boundary. @s inside emails (preceded by
//     word chars) and inside URLs (preceded by '/', ':' or alphanumerics
//     without a whitespace boundary) are excluded — "联系 a@b.com" resolves
//     nothing. 5..32 chars is the Telegram username constraint; 4-char
//     legacy handles are deliberately not resolved (log noise, and the
//     ambiguity cost outweighs the recall).
//   - Dedup + order-preserving; max BOT_BIO_RESOLVE_MAX (default 3) resolved,
//     the rest skipped with a WARN.
//   - Whole fan bounded by BOT_BIO_RESOLVE_TIMEOUT (default 10s; 0 = no
//     dedicated bound — each lookup still rides the caller's ctx).
//   - Each resolved handle contributes one line:
//     【相关简介】@handle：bio 或 description
//     Empty target text → the handle is skipped (nothing to feed the judge).
//   - bioResolveText is pure; bioResolve performs I/O and is injectable
//     through the BioResolver interface (tests use fakes, zero network).
package bot

import (
	"context"
	"regexp"
	"strings"

	"adbot/internal/telegram"
)

// bioRelatePrefix introduces the 【相关简介】 block inside the judged text.
const bioRelatePrefix = "【相关简介】"

// handleTokenRe matches one @handle candidate: "@" followed by 5..32
// [A-Za-z0-9_] (the Telegram username constraint, minus 4-char legacy
// handles). Callers enforce the boundary conditions.
var handleTokenRe = regexp.MustCompile(`@([A-Za-z0-9_]{5,32})`)

// BioResolver resolves one @handle to its target's intro text (bio for a
// user, description for a channel). It is the injectable seam: the real
// telegram.Client in production (GetChatByUsername), fakes in tests
// (零真实网络).
type BioResolver interface {
	GetChatByUsername(ctx context.Context, username string) (telegram.ChatInfo, error)
}

// scanHandles extracts resolvable @handles from a text, in order, deduped:
// the handle must not be part of an email or URL.
//
// Exclusion rules:
//   - "@" preceded by a word char (letter/digit/underscore/.) → email-ish
//     local part ("a@b.com", "微信xx@163.com") — skip;
//   - "@" preceded by '/' or ':' → URL userinfo/path ("t.me/@x",
//     "https://x.com/@y") — skip;
//   - the handle run must END at a boundary: the next char (if any) is not a
//     word char, not '.' and not '@' ("@abc_def" gluing into "@abc_def.ghi"
//     or "@abcdef@x" is one token and skipped).
func scanHandles(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range handleTokenRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		// Leading context: exclude email/URL shapes.
		if start > 0 {
			prev := text[start-1]
			if isWordByte(prev) || prev == '.' || prev == '/' || prev == ':' {
				continue
			}
		}
		// Trailing boundary: the char after the handle run must not extend it.
		if end < len(text) {
			next := text[end]
			if isWordByte(next) || next == '.' || next == '@' {
				continue
			}
		}
		handle := text[start+1 : end]
		if seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, handle)
	}
	return out
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// relatedBioText picks the intro text of a resolved target: a user's bio, or
// a channel/supergroup's description (getChat fills exactly one of them for
// either shape).
func relatedBioText(info telegram.ChatInfo) string {
	if strings.TrimSpace(info.Bio) != "" {
		return strings.TrimSpace(info.Bio)
	}
	return strings.TrimSpace(info.Description)
}

// bioResolveText is the pure combiner: given the source bio and the resolved
// per-handle intro texts (only handles with non-empty text), it renders the
// judgment text — original bio first, then the 【相关简介】 block. Empty
// resolutions return the bio unchanged.
func bioResolveText(bio string, resolved map[string]string) string {
	if len(resolved) == 0 {
		return bio
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(bio))
	for _, h := range scanHandles(bio) {
		if txt, ok := resolved[h]; ok {
			b.WriteString("\n")
			b.WriteString(bioRelatePrefix)
			b.WriteString("@")
			b.WriteString(h)
			b.WriteString("：")
			b.WriteString(txt)
		}
	}
	return b.String()
}

// bioResolve performs the resolution: fetch each resolvable handle's intro
// (bounded by cfg.BioResolveMax and cfg.BioResolveTimeout) and return the
// per-handle map for bioResolveText. Misses are simply absent from the map.
// The returned ok is always true — a lookup failure is a miss, not a judgment
// failure (the bio is then judged as-is).
func (b *Bot) bioResolve(ctx context.Context, bio string) map[string]string {
	if !b.Cfg.BioResolve || strings.TrimSpace(bio) == "" {
		return nil
	}
	if b.BioTarget == nil {
		return nil
	}
	handles := scanHandles(bio)
	if len(handles) == 0 {
		return nil
	}
	max := b.Cfg.BioResolveMax
	if max < 1 {
		max = 3
	}
	if b.Cfg.BioResolveTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.Cfg.BioResolveTimeout)
		defer cancel()
	}
	resolved := map[string]string{}
	for i, h := range handles {
		if i >= max {
			b.Log.Warn("bio_resolve_skipped_cap",
				"handle", "@"+h,
				"cap", max,
				"summary", "简介 @ 数超上限：超出部分不再解析")
			break
		}
		info, err := b.BioTarget.GetChatByUsername(ctx, h)
		if err != nil {
			// Miss: never fail the judgment, never retry — the bio is judged
			// as-is (pre-round behavior for this handle).
			b.Log.Warn("bio_resolve_miss",
				"handle", "@"+h,
				"err", err.Error(),
				"summary", "简介 @ 解析失败：按原简介判定")
			continue
		}
		if txt := relatedBioText(info); txt != "" {
			resolved[h] = txt
			b.Log.Info("bio_resolve_hit",
				"handle", "@"+h,
				"len", len(txt),
				"summary", "已提取 @ 目标简介，随判定文本送审")
		} else {
			b.Log.Debug("bio_resolve_empty",
				"handle", "@"+h,
				"summary", "目标简介为空：不附加")
		}
	}
	return resolved
}

// resolveBioText runs resolve+combine for one bio and returns the judgment
// text (the enriched bio). It is the single entry the three bio lanes call.
func (b *Bot) resolveBioText(ctx context.Context, bio string) string {
	return bioResolveText(bio, b.bioResolve(ctx, bio))
}

// handleFromLink extracts a resolvable handle from a t.me/xxx (or
// https://t.me/xxx) link — the non-@ extension point. This round resolves @
// mentions only (the user's wording targets 简介@); the function exists so a
// future round can widen the scope without re-plumbing the lanes. Invite/join
// links (t.me/+hash, t.me/joinchat) are not usernames and resolve to ""; bot
// deep links (t.me/x?start=...) drop the query and keep the bare username.
func handleFromLink(link string) string {
	low := strings.ToLower(strings.TrimSpace(link))
	for _, prefix := range []string{"https://", "http://"} {
		low = strings.TrimPrefix(low, prefix)
	}
	if !strings.HasPrefix(low, "t.me/") {
		return ""
	}
	rest := low[len("t.me/"):]
	if rest == "" {
		return ""
	}
	if strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "joinchat") {
		return ""
	}
	// Drop query/fragment, then require a bare username shape.
	if i := strings.IndexAny(rest, "?#/"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" || strings.ContainsAny(rest, "/") {
		return ""
	}
	if len(rest) < 5 || len(rest) > 32 {
		return ""
	}
	for i := 0; i < len(rest); i++ {
		if !isWordByte(rest[i]) {
			return ""
		}
	}
	return rest
}
