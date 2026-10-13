// Package localrules is the last-resort rule engine that takes over only when
// the AI has been unreachable for BOT_AI_RETRY_MAX_ATTEMPTS consecutive rounds
// (TASK_antispam_amend3.md A.6 / amend2 §6.1). It is a weighted keyword +
// contact-pattern scorer over normalized text: score >= threshold means the
// item is treated as an ad (full punishment flow); below threshold the user
// stays on the watchlist (observed, not punished). The negative sample table
// in localrules_test.go guards against false hits on ordinary chat ("我加你了",
// "发票报销流程").
package localrules

import (
	"regexp"
	"strings"
)

// Scoring rules (weights per amend2 §6.1):
//
//	contact patterns +2 each kind, keyword hit +2 per distinct domain,
//	QR/扫码 +2, @all / short-link +1 each.
const (
	wContact   = 2
	wKeyword   = 2
	wQRCode    = 2
	wMentions  = 1
	wShortLink = 1
)

// Domain keywords: each entry is one scoring domain; multiple hits inside the
// same domain score once (dedup by domain).
var domains = [][]string{
	{"色情", "看片", "资源", "约炮", "楼凤", "上门服务", "嫖"},
	{"儿童性内容", "萝莉", "幼女", "小学生视频", "幼儿园视频"},
	{"赌博", "百家乐", "棋牌", "彩票带单", "上分", "下分", "包赢", "稳赚不赔", "下注"},
	{"办证", "证件", "驾照", "学历", "四六级", "印章", "发票"},
	{"贷款", "征信修复", "套现", "洗白"},
	{"刷单", "跑分", "代收", "接码", "日结"},
	{"兼职带单", "兼职", "带你赚钱"},
	{"usdt", "换汇", "泰达"},
	{"毒品", "笑气", "违禁药", "枪支", "弹药", "管制刀具", "迷药"},
	{"外挂", "作弊器"},
	{"查档", "定位手机", "黑客接单"},
}

// contactRe matches WeChat/QQ/telegram handle patterns with an inviting verb.
var contactRe = regexp.MustCompile(
	`(加|私|详聊|滴滴|滴我).{0,6}(v|v信|vx|微|微信|薇|威信|q|qq|扣|tg|telegram|飞机)` +
		`|(v|vx|微信|威信|薇|qq|扣扣|tg).{0,4}(号|群)\s*[:：]?\s*[a-z0-9_-]{4,}` +
		`|@?[a-z0-9_]{5,}\s*(加我|私我|咨询)`)

// phoneRe matches bare 11-digit CN mobile numbers (spam drops these a lot).
var phoneRe = regexp.MustCompile(`1[3-9]\d{9}`)

// qrRe matches 二维码 / 扫码 phrases.
var qrRe = regexp.MustCompile(`二维码|扫码|扫一扫|扫码进群`)

// shortLinkRe matches common shortener hosts.
var shortLinkRe = regexp.MustCompile(`\b(t\.me|bit\.ly|tinyurl|is\.gd|suo\.im|dwz\.cn|urlc|htl\.li)`)

// atAllRe matches @全体成员 mentions.
var atAllRe = regexp.MustCompile(`@全体成员|@all`)

// Engine is a configured rule engine. Threshold defaults to 3.
type Engine struct {
	Threshold int
	// ExtraKeywords are appended to the domain keyword sets (BOT_LOCAL_KEYWORDS);
	// each extra hit adds its own +2.
	ExtraKeywords []string
}

// Result is one scoring outcome.
type Result struct {
	Score     int // total weighted score
	Threshold int
	IsAd      bool     // score >= threshold
	Hits      []string // the distinct rules/domains that fired
}

// Evaluate scores normalized text (and whether an attached image carried a
// QR code, from the AI-free vision path: not available here, so callers pass
// imageQR when they know it).
func (e Engine) Evaluate(text string, imageQR bool) Result {
	res := Result{Threshold: e.Threshold}
	if e.Threshold <= 0 {
		e.Threshold = 3
		res.Threshold = 3
	}
	t := strings.ToLower(text)

	if contactRe.MatchString(t) {
		res.Score += wContact
		res.Hits = append(res.Hits, "联系方式")
	}
	if phoneRe.MatchString(t) {
		res.Score += wContact
		res.Hits = append(res.Hits, "手机号")
	}

	for i, domain := range domains {
		for _, kw := range domain {
			if strings.Contains(t, kw) {
				res.Score += wKeyword
				res.Hits = append(res.Hits, "词表:"+kw)
				_ = i
				break // one hit scores the domain once
			}
		}
	}
	for _, kw := range e.ExtraKeywords {
		if kw != "" && strings.Contains(t, strings.ToLower(kw)) {
			res.Score += wKeyword
			res.Hits = append(res.Hits, "自定义:"+kw)
		}
	}

	if imageQR || qrRe.MatchString(t) {
		res.Score += wQRCode
		res.Hits = append(res.Hits, "二维码")
	}
	if atAllRe.MatchString(t) {
		res.Score += wMentions
		res.Hits = append(res.Hits, "@全体")
	}
	if shortLinkRe.MatchString(t) {
		res.Score += wShortLink
		res.Hits = append(res.Hits, "短链")
	}

	res.IsAd = res.Score >= e.Threshold
	return res
}
