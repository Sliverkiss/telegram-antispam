package ai

import (
	"strings"
	"testing"
)

// The bio-resolve round ground truth: the five rows the stakeholder answered
// one by one (Hermes 逐条问、需求方逐条答——"真值，你的提示词必须能复现这 5
// 条"). The prompt is text, not code — the enforceable contract here is that
// judge_v3 (a) carries the inverted definition style (不放过清单, not a
// 放行 definition), (b) downgrades 引流动作 from a conviction judgment to a
// corroborating clue, (c) embeds BOTH pass-listed examples verbatim so the
// model has seen the exact shapes, and (d) keeps the kill-list rows that the
// three kick-out truth samples must ride (薅羊毛群发 / 小店卖货 / 二手交易).
// Reproducing the five verdicts themselves end-to-end is the -prompteval
// data loop's job (testdata/prompt_eval.jsonl rows b01..b05, run against the
// real gateway — never in unit tests).
func TestJudgeV3EmbedsGroundTruth(t *testing.T) {
	p := BuildSystemPrompt("message")
	for _, want := range []string{
		"唯一定罪标准：品类命中",
		"不放过",
		"私聊机器人：@xxx_bot 个人频道：@xxx",
		"我推: @Xxiongxiong 京东车: @jd_0721_bot",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("judge_v3 must embed %q (stakeholder ground truth)", want)
		}
	}
	// The two pass rows carry their verdicts inline; the three kill rows are
	// represented by their category list (群发商业广告 sub-rows).
	for _, want := range []string{
		"→ is_ad=false",
		"福利群", "我自己小店", "出二手",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("judge_v3 must cover kill-row cue %q", want)
		}
	}
}

// TestJudgeV3InvertedDefinition: the v2 crime "存在引流动作 → is_ad=true" must
// be gone — that judgment-b) line is exactly what killed the misjudged
// 京东车 notification (an ordinary @ annotation read as 导流).
func TestJudgeV3InvertedDefinition(t *testing.T) {
	p := BuildSystemPrompt("message")
	if strings.Contains(p, "任一命中即 is_ad=true") {
		t.Error("the v2 multi-criteria conviction list must be rewritten")
	}
	// 引流动作 must be demoted to a corroborating clue.
	if !strings.Contains(p, "引流动作不是罪名，只是佐证") {
		t.Error("the demotion clause must be present")
	}
	// The clause line-wraps in the prompt text ("…仅因为带联系方式/@/\n链接 →…"),
	// so assert both halves of the acquittal rule.
	if !strings.Contains(p, "仅因为带联系方式/@/") || !strings.Contains(p, "链接 → is_ad=false") {
		t.Error("the contact-only acquittal rule must be present")
	}
}

// TestJudgeV3CarriesRelatedBioContract: the prompt must know the 【相关简介】
// block shape bio-resolve appends, so the model treats it as evidence
// rather than foreign text.
func TestJudgeV3CarriesRelatedBioContract(t *testing.T) {
	p := BuildSystemPrompt("message")
	if !strings.Contains(p, "【相关简介】") {
		t.Error("judge_v3 must document the 【相关简介】 evidence block")
	}
}

// TestJudgeV3OutputContractUnchanged: the v3 rewrite must keep the output
// contract byte-compatible with the parse layer (fields, controlled vocab,
// JSON-only) — a contract drift would turn every call into a parse failure.
func TestJudgeV3OutputContractUnchanged(t *testing.T) {
	p := BuildSystemPrompt("message")
	for _, want := range []string{
		`"is_ad"`, `"category"`, `"confidence"`, `"reason"`,
		`{"is_ad":true,"category":"赌博","confidence":0.97,"reason":"百家乐带单并引流加V"}`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("output contract drift: %q missing", want)
		}
	}
	for _, c := range controlledCategories {
		if !strings.Contains(p, c) {
			t.Errorf("controlled vocab entry %q missing from the prompt", c)
		}
	}
}
