// SimHash similarity matching (TASK_antispam_amend3.md A.9 / amend2 §6.2):
// a 64-bit simhash over normalized text so "改字/插 emoji/加空格" variants of a
// known ad signature still hit. Exact sha1 matches win first; simhash hamming
// distance <= BOT_SIMHASH_DISTANCE (default 3) counts as a hit otherwise.
package normalize

import (
	"strings"
)

// SimHash computes the 64-bit simhash of normalized text using character
// 3-grams of the normalized form (CJK-friendly; word segmentation is not
// needed for near-duplicate detection at distance 3).
func SimHash(s string) uint64 {
	n := Normalize(s)
	grams := trigrams(n)
	if len(grams) == 0 {
		return 0
	}
	var counts [64]int
	for _, g := range grams {
		h := fnv1a(g)
		for i := 0; i < 64; i++ {
			if h&(1<<uint(i)) != 0 {
				counts[i]++
			} else {
				counts[i]--
			}
		}
	}
	var sim uint64
	for i := 0; i < 64; i++ {
		if counts[i] > 0 {
			sim |= 1 << uint(i)
		}
	}
	return sim
}

// HammingDistance returns the number of differing bits of two 64-bit hashes.
func HammingDistance(a, b uint64) int {
	x := a ^ b
	dist := 0
	for x != 0 {
		x &= x - 1
		dist++
	}
	return dist
}

// SimHashEqual reports whether two texts' simhashes are within maxDist bits.
func SimHashEqual(a, b string, maxDist int) bool {
	ha, hb := SimHash(a), SimHash(b)
	if ha == 0 || hb == 0 {
		return false // empty normalized text never matches
	}
	return HammingDistance(ha, hb) <= maxDist
}

// trigrams returns the character 3-grams of s (lowercased caller input
// already expected; we operate on runes to stay CJK-correct).
func trigrams(s string) []string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) < 3 {
		if len(runes) == 0 {
			return nil
		}
		return []string{string(runes)}
	}
	out := make([]string, 0, len(runes)-2)
	for i := 0; i+2 < len(runes); i++ {
		out = append(out, string(runes[i:i+3]))
	}
	return out
}

// fnv1a is the 64-bit FNV-1a hash of a gram.
func fnv1a(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for _, r := range s {
		h ^= uint64(r)
		h *= prime64
	}
	return h
}
