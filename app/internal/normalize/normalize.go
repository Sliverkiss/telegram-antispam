// Package normalize turns arbitrary user-generated text (bios, messages,
// usernames) into a canonical form and derives a stable sha1 signature from
// it. The same text must always canonicalize to the same sha1, so the
// antispam bot can build signature libraries where one normalized form
// matches many visually-identical variants (fullwidth, zero-width padding,
// emoji decoration, URL scheme variants, separator noise).
package normalize

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"unicode"
)

// Normalize canonicalizes text for signature derivation:
//
//  1. strip zero-width / invisible characters
//  2. fullwidth → halfwidth (ＡＢＣ１２３ → ABC123, fullwidth space → space)
//  3. lowercase
//  4. strip emoji and other symbols (skin tones, variation selectors, pictographs)
//  5. strip URL scheme + www. + www<num>. (keep host + path)
//  6. collapse any separator/punctuation run into single spaces
//  7. trim and collapse whitespace
func Normalize(s string) string {
	s = stripZeroWidth(s)
	s = fullwidthToHalfwidth(s)
	s = strings.ToLower(s)
	s = stripEmojiAndSymbols(s)
	s = stripURLJunk(s)
	s = collapseSeparators(s)
	return strings.Join(strings.Fields(s), " ")
}

// Signature returns the hex sha1 of the normalized string. Empty normalized
// input yields "" (no signature for empty content).
func Signature(s string) string {
	n := Normalize(s)
	if n == "" {
		return ""
	}
	sum := sha1.Sum([]byte(n))
	return hex.EncodeToString(sum[:])
}

// stripZeroWidth removes zero-width and other invisible codepoints that are
// used to obfuscate text (U+200B..U+200F, U+FEFF, U+202A..U+202E, U+2060..U+2064).
func stripZeroWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0x200B && r <= 0x200F,
			r == 0xFEFF,
			r >= 0x202A && r <= 0x202E,
			r >= 0x2060 && r <= 0x2064:
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fullwidthToHalfwidth maps fullwidth Latin/ASCII forms to their halfwidth
// equivalents (excluding the fullwidth space, handled separately).
func fullwidthToHalfwidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '　':
			b.WriteRune(' ')
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripEmojiAndSymbols removes pictographs, emoji, and combining/decorative
// codepoints. It keeps letters, digits, and CJK ideographs/other scripts'
// letters and marks, plus basic ASCII punctuation (later collapsed).
func stripEmojiAndSymbols(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isEmojiOrDecor(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isEmojiOrDecor reports whether r should be dropped as decorative noise.
func isEmojiOrDecor(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoji, symbols
		r >= 0x2600 && r <= 0x27BF,   // misc symbols & dingbats
		r >= 0x2B00 && r <= 0x2BFF,   // misc symbols & arrows
		r == 0x00A9 || r == 0x00AE,   // © ®
		r >= 0xFE00 && r <= 0xFE0F,   // variation selectors
		r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators (flags)
		return true
	}
	return false
}

// stripURLJunk removes "scheme://", "www.", and "www<digits>." prefixes from
// URL-looking tokens so the host+path survive as the comparable signature.
func stripURLJunk(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		fields[i] = stripURLToken(f)
	}
	return strings.Join(fields, " ")
}

func stripURLToken(tok string) string {
	// scheme://
	if i := strings.Index(tok, "://"); i >= 0 {
		tok = tok[i+3:]
	}
	// www. / www1. / www99. prefix
	lower := tok
	for {
		if strings.HasPrefix(lower, "www.") {
			tok = tok[4:]
			break
		}
		if strings.HasPrefix(lower, "www") {
			rest := lower[3:]
			j := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			if j > 0 && j < len(rest) && rest[j] == '.' {
				tok = tok[3+j+1:]
				break
			}
		}
		break
	}
	return tok
}

// collapseSeparators folds any run of separators/punctuation/symbols into a
// single space, keeping alphanumerics and script letters intact.
func collapseSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSep := false
	for _, r := range s {
		if isSeparator(r) {
			if !lastWasSep {
				b.WriteRune(' ')
				lastWasSep = true
			}
			continue
		}
		b.WriteRune(r)
		lastWasSep = false
	}
	return b.String()
}

// isSeparator reports whether r is a separator/noise character: whitespace,
// ASCII punctuation/symbols, general-punctuation block, and the soft hyphen.
func isSeparator(r rune) bool {
	switch {
	case unicode.IsSpace(r),
		r == 0x00AD,                // soft hyphen
		r >= 0x2000 && r <= 0x206F, // general punctuation (em dash etc.)
		(r >= 0x21 && r <= 0x2D),   // ! " # $ % & ' ( ) * + , -
		(r >= 0x3A && r <= 0x40),   // : ; < = > ? @
		(r >= 0x5B && r <= 0x60),   // [ \ ] ^ _ `
		(r >= 0x7B && r <= 0x7E):   // { | } ~
		// '.' and '/' are kept: they are structural in host+path URL
		// signatures (the reason scheme+www is stripped above).
		return true
	}
	return false
}
