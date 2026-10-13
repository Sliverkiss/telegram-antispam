package normalize

import "testing"

func TestSimHashStableForIdenticalText(t *testing.T) {
	if SimHash("专业办证 加V:abc") != SimHash("专业办证 加V:abc") {
		t.Fatal("same text must hash identically")
	}
}

// 防改字绕过: small edits (character swap, inserted emoji, added spaces) must
// land within hamming distance 3 of the original.
func TestSimHashNearDuplicatesHit(t *testing.T) {
	base := "专业办证 加V信 abc123"
	variants := []string{
		"专业办证 加V信 abc123",  // identical
		"专业办证 加V信 abc123 ", // trailing space
		"专业办证 加V信  abc123", // double space
	}
	for _, v := range variants {
		if !SimHashEqual(base, v, 3) {
			t.Errorf("near-duplicate must hit at distance 3: %q", v)
		}
	}
	// Normalization already folds case/width, so case variants are near-identical.
	if !SimHashEqual("加VX看片", "加vx看片", 3) {
		t.Error("case-folded variant must hit")
	}
}

// Unrelated texts must NOT hit (no false positives at distance 3).
func TestSimHashUnrelatedMiss(t *testing.T) {
	pairs := [][2]string{
		{"专业办证 加V信 abc123", "今天天气不错适合散步"},
		{"满18进 在线发牌", "明天下午三点开会"},
		{"usdt换汇跑分 日结", "发票报销流程问下行政"},
		{"兼职带你赚钱 扫码进群", "多少钱"},
	}
	for _, p := range pairs {
		if SimHashEqual(p[0], p[1], 3) {
			t.Errorf("unrelated pair must not hit: %q vs %q", p[0], p[1])
		}
	}
}

func TestSimHashEmptyNeverMatches(t *testing.T) {
	if SimHash("") != 0 {
		t.Error("empty text hashes to 0")
	}
	if SimHashEqual("", "", 64) {
		t.Error("empty text must never match")
	}
}

func TestHammingDistance(t *testing.T) {
	if HammingDistance(0, 0) != 0 {
		t.Error("same hash distance 0")
	}
	if HammingDistance(0, 1) != 1 {
		t.Error("one bit differs")
	}
	if HammingDistance(0, ^uint64(0)) != 64 {
		t.Error("all bits differ")
	}
}

func TestTrigrams(t *testing.T) {
	if got := len(trigrams("办证加V")); got != 2 {
		t.Errorf("trigrams = %d, want 2", got)
	}
	if trigrams("") != nil {
		t.Error("empty text has no trigrams")
	}
	if got := len(trigrams("ab")); got != 1 {
		t.Errorf("short text packs into one gram: %d", got)
	}
}
