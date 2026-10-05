package errlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendFormatsLine(t *testing.T) {
	dir := t.TempDir()
	var echo bytes.Buffer
	l := New(filepath.Join(dir, "sub", "error.log"), 0, &echo)
	l.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

	l.Append(CodeDecode, "meta", "a/b.mp3", "bad\nframe")

	want := "2026-01-02T03:04:05Z code=2 msg=decode failed: bad frame site=meta path=a/b.mp3"
	lines, err := l.Recent(10)
	if err != nil || len(lines) != 1 || lines[0] != want {
		t.Fatalf("got %v, %v; want [%s]", lines, err, want)
	}
	if !strings.Contains(echo.String(), want) {
		t.Fatalf("echo missing line: %q", echo.String())
	}
}

func TestEmptyFieldsBecomeDash(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "e.log"), 0, nil)
	l.Append(Code(99), "", "", "")
	lines, _ := l.Recent(1)
	if len(lines) != 1 || !strings.Contains(lines[0], "msg=unknown error site=- path=-") {
		t.Fatalf("unexpected line: %v", lines)
	}
}

func TestRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.log")
	l := New(p, 200, nil)
	for i := 0; i < 10; i++ {
		l.Append(CodeIO, "site", "path", strings.Repeat("x", 30))
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("expected rotated file: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() > 260 {
		t.Fatalf("current log should stay small, got %v %v", st, err)
	}
}

func TestRecentLimitsAndMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.log")
	l := New(p, 0, nil)
	if lines, err := l.Recent(5); lines != nil || err != nil {
		t.Fatalf("missing file should be empty, got %v %v", lines, err)
	}
	for i := 0; i < 5; i++ {
		l.Append(CodeMeta, "s", "p", "")
	}
	if lines, _ := l.Recent(2); len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines, _ := l.Recent(0); len(lines) != 5 {
		t.Fatalf("n=0 means all, got %d", len(lines))
	}
}

func TestNoFileLogger(t *testing.T) {
	l := New("", 0, nil)
	l.Append(CodeIO, "s", "p", "")
	if lines, err := l.Recent(1); lines != nil || err != nil {
		t.Fatalf("got %v %v", lines, err)
	}
}

func TestUnwritableLogReportsToEcho(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var echo bytes.Buffer
	l := New(filepath.Join(blocker, "e.log"), 0, &echo) // parent is a file, so MkdirAll fails
	l.Append(CodeIO, "s", "p", "")
	if !strings.Contains(echo.String(), "cannot write log file") {
		t.Fatalf("expected failure note, got %q", echo.String())
	}
}

func TestCodeMessages(t *testing.T) {
	for _, c := range []Code{CodeUnsupported, CodeDecode, CodeIO, CodeMeta, CodeUpload, CodeTranscode} {
		if c.Message() == "unknown error" || c.Message() == "" {
			t.Errorf("code %d has no message", c)
		}
	}
	if Code(0).Message() != "unknown error" {
		t.Error("code 0 should be unknown")
	}
}
