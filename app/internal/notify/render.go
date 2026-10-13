// Rendering of the amend4 notification texts (§1). Everything here is pure:
// no I/O, no state — tests can pin the exact output. All outputs are plain
// text by construction; the delivery layer never sets parse_mode.
package notify

import (
	"fmt"
	"strings"
	"time"

	"adbot/internal/logx"
)

// FormatOptions bounds rendering.
type FormatOptions struct {
	IncludeText bool // BOT_NOTIFY_INCLUDE_TEXT: 💬 evidence line
	MaxLen      int  // BOT_NOTIFY_MAX_LEN / 4096 hard clip
}

// ---------------------------------------------------------------------------
// Disposition notification (§1.1, user-approved template — copy exactly)
// ---------------------------------------------------------------------------

// PunishDetails is one disposition to announce.
type PunishDetails struct {
	ChatID    int64
	UID       int64
	Username  string
	FirstName string
	// Text is the triggering message (or bio) — the 💬 evidence line.
	Text string
	// Action: "ban" or "kick".
	Action string
	// Category as delivered by the verdict layer (already mapped to the
	// controlled vocabulary; "其他" for local-rule fallback).
	Category string
	// Source: "signature" | "ai" | "vote" | "local".
	Source string
	// Score is the confidence score 0–100 (ai: round(conf*100); vote:
	// rounded average; signature: 100; local: the rule score).
	Score int
	// VoteTally renders "3/3"-style vote support (votes received / total
	// lanes, abstentions excluded from the total when they occurred).
	VoteTotal int
	VoteFor   int
	// Retries: consecutive AI retry rounds consumed before the verdict.
	Retries int
	// DeleteFailed: the triggering message could not be deleted.
	DeleteFailed bool
	// Elapsed: total processing time from message receipt to disposal.
	Elapsed time.Duration
}

// actionVerb maps BOT_ACTION to the template's action word.
func actionVerb(action string) string {
	if strings.EqualFold(action, "kick") {
		return "已踢出"
	}
	return "已封禁"
}

// sourceSuffix implements the "末行后缀" rules: non-AI sources append their
// marker; the high-risk category (儿童性内容) always appends.
func sourceSuffix(d PunishDetails) string {
	switch d.Source {
	case "signature":
		return " · 签名命中"
	case "vote":
		if d.VoteTotal > 0 {
			return fmt.Sprintf(" · 投票 %d/%d", d.VoteFor, d.VoteTotal)
		}
		return " · 投票裁决"
	case "local":
		return ""
	default:
		return "" // ai: keep the line clean
	}
}

// RenderPunish produces the disposition notification. The base is exactly the
// user-approved 4-line template; anomaly lines are appended only when set.
func RenderPunish(d PunishDetails, opts FormatOptions) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("⏱️ 处理时间: %.2f s\n", d.Elapsed.Seconds()))
	if opts.IncludeText {
		b.WriteString("💬 " + excerpt(d.Text, 80) + "\n")
	}
	b.WriteString("👤 " + UserLabel(d) + "\n")

	// Line 4: local fallback has its own form; everything else is
	// action · score (+ source suffix / high-risk tag).
	if d.Source == "local" {
		b.WriteString(fmt.Sprintf("🧩 %s · 兜底 %d分", actionVerb(d.Action), d.Score))
	} else {
		b.WriteString(fmt.Sprintf("✅ %s · %d分", actionVerb(d.Action), d.Score))
		b.WriteString(sourceSuffix(d))
	}
	if d.Category == "儿童性内容" {
		b.WriteString(" · 🚨 儿童性内容")
	}

	// Anomaly lines — appended only when they actually happened.
	if d.DeleteFailed {
		b.WriteString("\n⚠️ 消息删除失败 · 超 48 小时")
	}
	if d.Retries > 1 {
		b.WriteString(fmt.Sprintf("\n🔁 重试 %d 次", d.Retries))
	}
	return clipHard(b.String(), opts.MaxLen)
}

// UserLabel picks the @username, else display name, else uid fallback.
func UserLabel(d PunishDetails) string {
	if u := strings.TrimSpace(d.Username); u != "" {
		return "@" + u
	}
	if f := strings.TrimSpace(d.FirstName); f != "" {
		return f
	}
	return fmt.Sprintf("无用户名 · %d", d.UID)
}

// ConfidenceScore converts an AI confidence into the 0–100 integer score.
func ConfidenceScore(conf float64) int {
	v := conf * 100
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return int(v + 0.5)
}

// ---------------------------------------------------------------------------
// Error notification (§1.2, 2–3 lines: event / detail / location)
// ---------------------------------------------------------------------------

// ErrorEvent is one operational event to announce. Fingerprint isolates the
// dedupe key (event type + chat/uid + error code); the renderers only format.
type ErrorEvent struct {
	// Kind: "telegram_write", "ai_chain", "queue_overflow", "janitor_evict",
	// "selftest".
	Kind string
	// Level: logx.Warn or logx.Error (compared to BOT_NOTIFY_LEVEL).
	Level       logx.Level
	Fingerprint string
	ChatID      int64
	UID         int64
	MessageID   int64
	// Attempts: retry rounds consumed (ai_chain headline "连续 N 次失败").
	Attempts int
	// Method: the failed Telegram write ("deleteMessage", ...) or a short
	// context label (pool error code, "notify queue", ...).
	Method string
	// Detail: the error code / description line (line 2).
	Detail string
	// LastSeen: free-form short note for the dedupe aggregate ("最近：…").
	LastSeen string
}

// fingerprint returns the explicit fingerprint or builds one from the event.
func (e ErrorEvent) fingerprint() string {
	if e.Fingerprint != "" {
		return e.Fingerprint
	}
	var b strings.Builder
	b.WriteString(e.Kind)
	b.WriteString("|")
	b.WriteString(e.Method)
	b.WriteString("|")
	b.WriteString(fmt.Sprint(e.ChatID))
	b.WriteString("|")
	b.WriteString(fmt.Sprint(e.UID))
	return b.String()
}

// lastSeen is what the end-of-window aggregate shows.
func (e ErrorEvent) lastSeen() string {
	if e.LastSeen != "" {
		return e.LastSeen
	}
	return e.Method + " " + e.Detail
}

// poolEmoji maps the AI pool error codes to their emoji (§1.2 table).
func poolEmoji(code string) string {
	switch strings.TrimSpace(code) {
	case "pool_parked":
		return "🧊"
	case "no_accounts":
		return "🕳️"
	case "upstream_busy":
		return "🚦"
	default:
		return ""
	}
}

// headline picks the emoji + event label for the first line. For the AI chain
// it follows the §1.2 template "⚠️ AI 连续 N 次失败 · <emoji> <code>".
func (e ErrorEvent) headline() string {
	switch e.Kind {
	case "telegram_write":
		return "📵 Telegram 写入失败"
	case "ai_chain":
		n := ""
		if e.Attempts > 0 {
			n = fmt.Sprintf(" %d 次", e.Attempts)
		}
		suffix := strings.TrimSpace(e.Detail)
		if em := poolEmoji(e.Detail); em != "" {
			suffix = em + " " + suffix
		}
		return fmt.Sprintf("⚠️ AI 连续%s失败 · %s", n, suffix)
	case "pool":
		if em := poolEmoji(e.Detail); em != "" {
			return "⚠️ AI 池级错误 · " + em + " " + e.Detail
		}
		return "⚠️ AI 池级错误 · " + e.Detail
	case "queue_overflow":
		return "🗑️ 通知队列溢出"
	case "janitor_evict":
		return "📦 容量淘汰触发上限"
	case "selftest":
		return "⚠️ 启动自检失败 · " + e.Detail
	default:
		return "⚠️ " + e.Kind
	}
}

// RenderError produces the 2–3 line error notification.
func RenderError(e ErrorEvent, opts FormatOptions) string {
	var lines []string
	lines = append(lines, e.headline())
	if e.Kind != "ai_chain" && e.Kind != "pool" && e.Kind != "selftest" {
		// line 2: the detail (method + code/description), trimmed of the
		// parts already shown in the headline.
		d := strings.TrimSpace(e.Method + " " + e.Detail)
		if d != "" {
			lines = append(lines, d)
		}
	} else if e.Method != "" {
		// AI/pool/selftest: the detail rode the headline; the second line is
		// the context label (retry summary, pool state, ...).
		lines = append(lines, e.Method)
	}
	// Location line (omitted when nothing to locate).
	var loc []string
	if e.ChatID != 0 {
		loc = append(loc, fmt.Sprintf("%d", e.ChatID))
	}
	if e.MessageID != 0 {
		loc = append(loc, fmt.Sprintf("msg %d", e.MessageID))
	}
	if e.UID != 0 {
		loc = append(loc, fmt.Sprintf("uid %d", e.UID))
	}
	if len(loc) > 0 {
		lines = append(lines, "📍 "+strings.Join(loc, " · "))
	}
	return clipHard(strings.Join(lines, "\n"), opts.MaxLen)
}
