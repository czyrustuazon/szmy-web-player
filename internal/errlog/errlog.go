// Package errlog writes one structured line per error, in the same shape as
// szmy's error log: code, message, site and path.
package errlog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Code identifies a class of error.
type Code int

const (
	CodeUnsupported Code = 1
	CodeDecode      Code = 2
	CodeIO          Code = 3
	CodeMeta        Code = 4
	CodeUpload      Code = 5
	CodeTranscode   Code = 6
)

var messages = map[Code]string{
	CodeUnsupported: "unsupported or non-audio file",
	CodeDecode:      "decode failed",
	CodeIO:          "file system error",
	CodeMeta:        "metadata read failed",
	CodeUpload:      "upload failed",
	CodeTranscode:   "transcode failed",
}

// Message is the human-readable text for a code.
func (c Code) Message() string {
	if m, ok := messages[c]; ok {
		return m
	}
	return "unknown error"
}

// Logger appends lines to a size-capped log file.
type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	out      io.Writer
	now      func() time.Time
}

// New creates a logger. path may be empty (no file); out may be nil (no echo).
func New(path string, maxBytes int64, out io.Writer) *Logger {
	return &Logger{path: path, maxBytes: maxBytes, out: out, now: time.Now}
}

// Append records an error. detail is optional extra context.
func (l *Logger) Append(code Code, site, path, detail string) {
	msg := code.Message()
	if detail != "" {
		msg += ": " + detail
	}
	line := fmt.Sprintf("%s code=%d msg=%s site=%s path=%s",
		l.now().UTC().Format(time.RFC3339), int(code), field(msg), field(site), field(path))

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		fmt.Fprintln(l.out, line)
	}
	if l.path != "" {
		if err := l.write(line); err != nil && l.out != nil {
			fmt.Fprintln(l.out, "errlog: cannot write log file:", err)
		}
	}
}

func field(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
	if s == "" {
		return "-"
	}
	return s
}

func (l *Logger) write(line string) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(l.path); err == nil && l.maxBytes > 0 && st.Size()+int64(len(line))+1 > l.maxBytes {
		old := l.path + ".1"
		_ = os.Remove(old)
		if err := os.Rename(l.path, old); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

// Recent returns the last n lines of the current log file.
func (l *Logger) Recent(n int) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
