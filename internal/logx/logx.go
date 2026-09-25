// Package logx is a minimal structured logger (drop-in for the small subset
// of log/slog that tinydb uses).
//
// log/slog costs ~120 KiB of code pages; on a 10 MiB RSS budget every page
// counts, so tinydb ships its own ~2 KiB equivalent with the same
// key=value text format.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is a log severity.
type Level int

// Levels.
const (
	LevelDebug Level = -4
	LevelInfo  Level = 0
	LevelWarn  Level = 4
	LevelError Level = 8
)

// Logger writes levelled key=value records to a writer.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
}

// New builds a logger writing to w at the given minimum level.
func New(w io.Writer, level Level) *Logger {
	return &Logger{w: w, level: level}
}

var defaultLogger = New(os.Stderr, LevelInfo)

// Default returns the process-wide logger.
func Default() *Logger { return defaultLogger }

// SetDefault replaces the process-wide logger.
func SetDefault(l *Logger) { defaultLogger = l }

// Enabled reports whether level would be emitted.
func (l *Logger) Enabled(level Level) bool { return l != nil && level >= l.level }

// Log emits a record with alternating key/value attributes.
func (l *Logger) Log(level Level, msg string, kv ...any) {
	if !l.Enabled(level) {
		return
	}
	var b strings.Builder
	b.WriteString("time=")
	b.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteString(" level=")
	b.WriteString(levelName(level))
	b.WriteString(" msg=")
	b.WriteString(quote(msg))
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		b.WriteString(fmt.Sprint(kv[i]))
		b.WriteByte('=')
		b.WriteString(quote(fmt.Sprint(kv[i+1])))
	}
	if len(kv)%2 == 1 {
		b.WriteString(" !EXTRA=")
		b.WriteString(quote(fmt.Sprint(kv[len(kv)-1])))
	}
	b.WriteByte('\n')
	l.mu.Lock()
	_, _ = io.WriteString(l.w, b.String())
	l.mu.Unlock()
}

// Debug logs at LevelDebug.
func (l *Logger) Debug(msg string, kv ...any) { l.Log(LevelDebug, msg, kv...) }

// Info logs at LevelInfo.
func (l *Logger) Info(msg string, kv ...any) { l.Log(LevelInfo, msg, kv...) }

// Warn logs at LevelWarn.
func (l *Logger) Warn(msg string, kv ...any) { l.Log(LevelWarn, msg, kv...) }

// Error logs at LevelError.
func (l *Logger) Error(msg string, kv ...any) { l.Log(LevelError, msg, kv...) }

// Package-level helpers for the default logger.

// Debug logs to the default logger.
func Debug(msg string, kv ...any) { defaultLogger.Log(LevelDebug, msg, kv...) }

// Info logs to the default logger.
func Info(msg string, kv ...any) { defaultLogger.Log(LevelInfo, msg, kv...) }

// Warn logs to the default logger.
func Warn(msg string, kv ...any) { defaultLogger.Log(LevelWarn, msg, kv...) }

// Error logs to the default logger.
func Error(msg string, kv ...any) { defaultLogger.Log(LevelError, msg, kv...) }

func levelName(l Level) string {
	switch {
	case l >= LevelError:
		return "ERROR"
	case l >= LevelWarn:
		return "WARN"
	case l >= LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// quote mimics slog.TextHandler quoting: bare when safe, quoted otherwise.
func quote(s string) string {
	if s == "" {
		return `""`
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '=' || c == '"' || c == '\\' || c >= 0x7f {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(s[i])
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}
