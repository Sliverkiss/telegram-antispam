package logx

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Debug, "text", "test")
	l.Info("thing", "uid", 42, "ok", true)
	out := buf.String()
	for _, want := range []string{"INFO", "test", "thing", "uid=\"42\"", "ok=\"true\""} {
		if !strings.Contains(out, want) {
			t.Errorf("text log missing %q in %q", want, out)
		}
	}
	// Text format carries the timestamp as its leading RFC3339 field.
	if len(out) < 10 || out[4] != '-' {
		t.Errorf("text log should lead with an RFC3339 timestamp, got %q", out)
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Debug, "json", "test")
	l.Warn("event1", "a", "b")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("json log not parseable: %v (%q)", err, buf.String())
	}
	if m["level"] != "WARN" || m["event"] != "event1" || m["component"] != "test" || m["a"] != "b" {
		t.Errorf("json fields wrong: %v", m)
	}
	if _, ok := m["ts"]; !ok {
		t.Error("json log missing ts")
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Warn, "text", "t")
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	out := buf.String()
	if strings.Contains(out, "d") || strings.Contains(out, " i ") {
		t.Errorf("debug/info leaked through warn filter: %q", out)
	}
	if !strings.Contains(out, "[WARN] t w") || !strings.Contains(out, "[ERROR] t e") {
		t.Errorf("warn/error missing: %q", out)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": Debug, "DEBUG": Debug, "info": Info, "warn": Warn,
		"warning": Warn, "error": Error, "bogus": Info, "": Info,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNopOnNil(t *testing.T) {
	var l *Logger
	l.Debug("x") // must not panic
	l.Info("y")
	l.Warn("z")
	l.Error("q")
}
