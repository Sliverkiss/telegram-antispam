// Package logx is a tiny stdlib-only structured logger with text and JSON
// output, leveled filtering, and a component/trace (update_id) convention.
// Every entry carries ts, level, component, and event; additional key/value
// fields ride along as ordered pairs.
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level is a log severity.
type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

// ParseLevel maps a config string to a Level; unrecognized values fall back
// to Info.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return Debug
	case "info":
		return Info
	case "warn", "warning":
		return Warn
	case "error":
		return Error
	default:
		return Info
	}
}

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Logger is a leveled structured logger. The zero value is not usable; use New.
type Logger struct {
	mu        sync.Mutex
	out       io.Writer
	level     Level
	format    string // "text" or "json"
	component string
}

// New builds a Logger writing to out (defaults to stderr when nil).
func New(out io.Writer, level Level, format, component string) *Logger {
	if out == nil {
		out = os.Stderr
	}
	if format != "json" {
		format = "text"
	}
	return &Logger{out: out, level: level, format: format, component: component}
}

// WithComponent returns a child logger bound to a different component.
func (l *Logger) WithComponent(component string) *Logger {
	if l == nil {
		return New(nil, Info, "text", component)
	}
	return &Logger{out: l.out, level: l.level, format: l.format, component: component}
}

// enabled reports whether a level passes the configured threshold.
func (l *Logger) enabled(lv Level) bool {
	return l != nil && lv >= l.level
}

func (l *Logger) log(lv Level, event string, kv ...any) {
	if !l.enabled(lv) {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == "json" {
		l.writeJSON(now, lv, event, kv)
		return
	}
	l.writeText(now, lv, event, kv)
}

func (l *Logger) writeText(now time.Time, lv Level, event string, kv []any) {
	var b strings.Builder
	b.WriteString(now.Format(time.RFC3339))
	b.WriteString(" [")
	b.WriteString(lv.String())
	b.WriteString("] ")
	if l.component != "" {
		b.WriteString(l.component)
		b.WriteString(" ")
	}
	b.WriteString(event)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(" ")
		b.WriteString(fmt.Sprint(kv[i]))
		b.WriteString("=")
		b.WriteString(strconv.Quote(fmt.Sprint(kv[i+1])))
	}
	b.WriteString("\n")
	io.WriteString(l.out, b.String())
}

func (l *Logger) writeJSON(now time.Time, lv Level, event string, kv []any) {
	m := make(map[string]any, 4+len(kv)/2)
	m["ts"] = now.Format(time.RFC3339)
	m["level"] = lv.String()
	if l.component != "" {
		m["component"] = l.component
	}
	m["event"] = event
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			key = fmt.Sprint(kv[i])
		}
		m[key] = kv[i+1]
	}
	// Stable key order for readable, deterministic output.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		b.Write(kb)
		b.WriteString(":")
		b.Write(vb)
	}
	b.WriteString("}\n")
	io.WriteString(l.out, b.String())
}

// Debug logs at debug level.
func (l *Logger) Debug(event string, kv ...any) { l.log(Debug, event, kv...) }

// Info logs at info level.
func (l *Logger) Info(event string, kv ...any) { l.log(Info, event, kv...) }

// Warn logs at warn level.
func (l *Logger) Warn(event string, kv ...any) { l.log(Warn, event, kv...) }

// Error logs at error level.
func (l *Logger) Error(event string, kv ...any) { l.log(Error, event, kv...) }

// LevelFor returns the configured level.
func (l *Logger) LevelFor() Level {
	if l == nil {
		return Info
	}
	return l.level
}
