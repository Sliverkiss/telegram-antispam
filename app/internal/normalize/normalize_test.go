package normalize

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase", "Hello World", "hello world"},
		{"fullwidth letters", "\uff21\uff22\uff23123", "abc123"},
		{"fullwidth space", "\u4f60\u597d\u3000\u4e16\u754c", "\u4f60\u597d \u4e16\u754c"},
		{"zero width joiner", "abc\u200bdef\u200cg\u2060hi", "abcdefghi"},
		{"zero width no break", "\ufeffhello", "hello"},
		{"zero width rtl marks", "a\u200eb\u200fc", "abc"},
		{"emoji stripped", "\u52a0\u5fae\u4fe1 \U0001f60a \u514d\u8d39", "\u52a0\u5fae\u4fe1 \u514d\u8d39"},
		{"emoji only removed", "\U0001f680\u200d\U0001f389", ""},
		{"skin tone", "hi\U0001f44d\U0001f3fb there", "hi there"},
		{"url scheme", "https://www.example.com/path", "example.com/path"},
		{"url www", "www.google.com", "google.com"},
		{"url www digits", "www3.baidu.com", "baidu.com"},
		{"url no scheme no www", "example.com/a?b=1", "example.com/a b 1"},
		{"collapse punctuation", "\u4ee3\u529e!!!\u8bc1\u4ef6\u2014\u2014\u529e\u7406", "\u4ee3\u529e \u8bc1\u4ef6 \u529e\u7406"},
		{"mixed noise", "\u52a0\uff36\u4fe1\U0001f4a5\uff1a\uff36\uff38\uff11\uff12\uff13", "\u52a0v\u4fe1 vx123"},
		{"empty", "", ""},
		{"whitespace only", " \t\n ", ""},
		{"dash and dot", "a.b-c_d", "a.b c d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Normalize(c.in); got != c.want {
				t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSignature(t *testing.T) {
	if Signature("") != "" {
		t.Error("empty signature must be empty")
	}
	if Signature("x") == "" {
		t.Error("non-empty text must produce a signature")
	}
	if got, want := Normalize("\uff41\uff22\uff23"), "abc"; got != want {
		t.Fatalf("fullwidth normalize = %q, want %q", got, want)
	}
	if len(Signature("x")) != 40 {
		t.Errorf("signature length = %d, want 40", len(Signature("x")))
	}
}

func TestSignatureCaseFold(t *testing.T) {
	if Signature("HELLO") != Signature("hello") {
		t.Error("case must be folded before hashing")
	}
}

func TestSignatureURLVariant(t *testing.T) {
	a := Signature("https://www.example.com/path")
	b := Signature("http://example.com/path")
	c := Signature("EXAMPLE.COM/path")
	if a == "" || a != b || b != c {
		t.Errorf("url variants must share one signature: %q %q %q", a, b, c)
	}
}
