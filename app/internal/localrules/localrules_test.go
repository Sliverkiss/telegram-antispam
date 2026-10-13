package localrules

import "testing"

// Positive samples: each must reach the default threshold (3) and be IsAd.
func TestPositiveSamples(t *testing.T) {
	eng := Engine{}
	for _, s := range []string{
		"专业办理各种证件 加V信 abc123",
		"加vx：kfw123456 有资源看片",
		"百家乐棋牌上分下分 扫码进群",
		"usdt换汇跑分 日结 滴滴12345678901",
		"兼职带你赚钱 扫码进群 二维码",
		"学历办证 包过 四六级 加qq99887766",
		"上门服务资源 扫码看片",
	} {
		res := eng.Evaluate(s, false)
		if !res.IsAd {
			t.Errorf("positive sample must hit threshold: %q → %+v", s, res)
		}
	}
}

// Negative samples from the task book: ordinary chat must stay below
// threshold (no false positives on daily conversation).
func TestNegativeSamples(t *testing.T) {
	eng := Engine{}
	for _, s := range []string{
		"我加你了",
		"发票报销流程问下行政",
		"多少钱",
		"今天天气不错",
		"明天下午开会记得带电脑",
		"谢谢，收到了",
		"这个多少钱能便宜点吗",
	} {
		res := eng.Evaluate(s, false)
		if res.IsAd {
			t.Errorf("negative sample must not hit: %q → %+v", s, res)
		}
	}
}

// The same contact pattern alone (score 2) must stay below the default
// threshold 3 — one signal is not an ad.
func TestSingleSignalBelowThreshold(t *testing.T) {
	eng := Engine{}
	res := eng.Evaluate("加我微信聊", false)
	if res.IsAd {
		t.Errorf("contact-only must be below threshold: %+v", res)
	}
	// contact + QR = 4 ≥ 3 → ad.
	res = eng.Evaluate("加我微信 扫码进群", false)
	if !res.IsAd {
		t.Errorf("contact+QR must reach threshold: %+v", res)
	}
}

func TestExtraKeywords(t *testing.T) {
	eng := Engine{ExtraKeywords: []string{"内部群"}}
	// The extra keyword alone scores +2 (below threshold — one signal is not
	// an ad); combined with a contact pattern it must reach the threshold.
	res := eng.Evaluate("进内部群领福利", false)
	if res.Score < 2 {
		t.Errorf("extra keyword must score: %+v", res)
	}
	res = eng.Evaluate("进内部群 加V信 abc123", false)
	if !res.IsAd {
		t.Errorf("extra keyword + contact must hit threshold: %+v", res)
	}
}

func TestCustomThreshold(t *testing.T) {
	eng := Engine{Threshold: 10}
	res := eng.Evaluate("加V信 abc123 有资源看片", false)
	if res.IsAd {
		t.Errorf("threshold 10 must not fire on 2-domain hit: %+v", res)
	}
}

func TestQRImageFlag(t *testing.T) {
	eng := Engine{}
	res := eng.Evaluate("看这张图", true)
	if res.Score < 2 || len(res.Hits) == 0 {
		t.Errorf("imageQR flag must score the QR domain: %+v", res)
	}
	// QR image + contact pattern must reach the threshold.
	res = eng.Evaluate("看这张图 加V信 abc123", true)
	if !res.IsAd {
		t.Errorf("QR image + contact must hit threshold: %+v", res)
	}
}

func TestEmptyThresholdDefaults(t *testing.T) {
	eng := Engine{Threshold: 0}
	if got := eng.Evaluate("x", false).Threshold; got != 3 {
		t.Errorf("zero threshold must default to 3, got %d", got)
	}
}

func TestDomainDedup(t *testing.T) {
	// Two keywords from the same domain (赌博 domain: 百家乐 + 棋牌) score once.
	eng := Engine{}
	res := eng.Evaluate("百家乐棋牌", false)
	if res.Score >= 4 {
		t.Errorf("same-domain hits must dedup: %+v", res)
	}
	// Two different domains (赌博 + 色情) score twice.
	res = eng.Evaluate("百家乐 看片", false)
	if res.Score < 4 {
		t.Errorf("cross-domain hits must both score: %+v", res)
	}
}
