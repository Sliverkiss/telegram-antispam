// Tolerant-but-strict verdict parsing (TASK_antispam_amend3.md C.3).
//
// Tolerant: ```json fences, preamble/trailing chatter, confidence delivered as
// a string or an integer, out-of-range confidence clamped to [0,1],
// case-insensitive field names.
//
// Strict: the decision fields must exist and be parseable — no JSON object, a
// missing is_ad, or an is_ad that is neither boolean nor a boolean-looking
// string is a CONTRACT VIOLATION: ParseVerdict returns an error and callers
// must route the call into the retry chain. A parse failure may NEVER be
// interpreted as "not an ad".
//
// category is free Chinese text by design (the model may return words outside
// the controlled vocabulary); no enum validation happens here.
package ai

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParseVerdict turns model text into a Verdict. Tolerant on shape, strict on
// contract: any failure returns an error that callers treat as a failed call.
func ParseVerdict(text string) (Verdict, error) {
	raw := extractJSONObject(text)
	if raw == "" {
		return Verdict{}, fmt.Errorf("no JSON object in %q", truncateForLog(text, 120))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return Verdict{}, fmt.Errorf("decode verdict: %w", err)
	}

	var v Verdict

	// is_ad: strict contract field.
	isAd, err := parseBoolField(m, "is_ad")
	if err != nil {
		return Verdict{}, fmt.Errorf("contract violation: %w", err)
	}
	v.IsAd = isAd

	// category: free text, no enum validation (recorded verbatim).
	v.Category = parseStringField(m, "category")
	if v.Category == "" {
		v.Category = "其他"
	}

	// confidence: tolerant number handling (string / int / float / percent),
	// clamped into [0,1].
	v.Confidence = parseConfidence(m)

	// reason: free text.
	v.Reason = parseStringField(m, "reason")
	return v, nil
}

// extractJSONObject strips ```json fences and any chatter around the first
// complete top-level JSON object in the text.
func extractJSONObject(text string) string {
	text = stripFences(text)
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return ""
	}
	// Walk the object, tracking nesting and string state, to find its end —
	// robust against trailing junk and braces inside strings.
	depth := 0
	inStr := false
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	// Unterminated object: try the naive "first { to last }" salvage.
	if end := strings.LastIndexByte(text, '}'); end > start {
		return text[start : end+1]
	}
	return ""
}

// stripFences removes a leading ```json fence (with optional trailing fence).
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			rest := s[i+1:]
			if j := strings.LastIndex(rest, "```"); j >= 0 {
				rest = rest[:j]
			}
			s = rest
		} else if len(s) >= 6 {
			s = s[3:]
		}
	}
	return strings.TrimSpace(s)
}

// parseBoolField reads a field that must be a JSON boolean (or a boolean-
// looking string like "true"/"True"). Anything else is a contract violation.
func parseBoolField(m map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := lookupField(m, key)
	if !ok {
		return false, fmt.Errorf("missing field %q", key)
	}
	val := strings.TrimSpace(string(raw))
	if val == "null" {
		return false, fmt.Errorf("field %q is null", key)
	}
	switch val {
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
	}
	return false, fmt.Errorf("field %q is not a boolean: %s", key, truncateForLog(val, 40))
}

// parseStringField reads a string field tolerantly (numbers are stringified).
func parseStringField(m map[string]json.RawMessage, key string) string {
	raw, ok := lookupField(m, key)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return strings.Trim(string(raw), `"`)
}

// parseConfidence reads confidence from the "confidence" field (falling back
// to common misspellings), tolerating string/integer/percent forms, and clamps
// the result into [0,1].
func parseConfidence(m map[string]json.RawMessage) float64 {
	raw, ok := lookupField(m, "confidence")
	if !ok {
		return 0
	}
	f := parseNumberTolerant(raw)
	if f > 1 && f <= 100 {
		// A percentage (e.g. 85 meaning 0.85).
		f = f / 100
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// parseNumberTolerant decodes a JSON number, a numeric string, or an integer
// delivered as a bare word.
func parseNumberTolerant(raw json.RawMessage) float64 {
	val := strings.TrimSpace(string(raw))
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSuffix(strings.TrimSpace(s), "%")
		if parsed, err := strconv.ParseFloat(s, 64); err == nil {
			return parsed
		}
	}
	if parsed, err := strconv.ParseFloat(val, 64); err == nil {
		return parsed
	}
	return 0
}

// lookupField finds a key case-insensitively (models occasionally title-case).
func lookupField(m map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if raw, ok := m[key]; ok {
		return raw, true
	}
	lower := strings.ToLower(key)
	for k, raw := range m {
		if strings.ToLower(k) == lower {
			return raw, true
		}
	}
	return nil, false
}

// truncateForLog bounds what we echo into an error message.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
