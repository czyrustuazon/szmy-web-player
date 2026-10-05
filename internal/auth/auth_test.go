package auth

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newAuth(t *testing.T, pw string) *Auth {
	t.Helper()
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	return New(salt, Hash(salt, pw), false, time.Hour)
}

func TestHashIsSaltedAndDeterministic(t *testing.T) {
	s1, s2 := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	if !bytes.Equal(Hash(s1, "pw"), Hash(s1, "pw")) {
		t.Error("same input must hash the same")
	}
	if bytes.Equal(Hash(s1, "pw"), Hash(s2, "pw")) {
		t.Error("salt must change the hash")
	}
	if bytes.Equal(Hash(s1, "pw"), Hash(s1, "pw2")) {
		t.Error("password must change the hash")
	}
	if len(Hash(s1, "pw")) != 32 {
		t.Error("expected a SHA-256 sized hash")
	}
}

func TestNewSaltIsRandom(t *testing.T) {
	a, _ := NewSalt()
	b, _ := NewSalt()
	if len(a) != 16 || bytes.Equal(a, b) {
		t.Fatal("salts should be 16 random bytes")
	}
}

func TestLoginValidLogout(t *testing.T) {
	a := newAuth(t, "secret")
	if _, ok := a.Login("wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	if a.Valid("") || a.Valid("nonsense") {
		t.Fatal("unknown tokens must be invalid")
	}
	tok, ok := a.Login("secret")
	if !ok || len(tok) != 64 {
		t.Fatalf("login: %q %v", tok, ok)
	}
	if !a.Valid(tok) {
		t.Fatal("fresh session should be valid")
	}
	other, _ := a.Login("secret")
	if other == tok {
		t.Fatal("tokens must be unique")
	}
	a.Logout(tok)
	if a.Valid(tok) || !a.Valid(other) {
		t.Fatal("logout must end only that session")
	}
	if a.Disabled() {
		t.Error("not disabled")
	}
}

func TestSessionExpiry(t *testing.T) {
	a := newAuth(t, "pw")
	now := time.Unix(1_000_000, 0)
	a.now = func() time.Time { return now }
	tok, _ := a.Login("pw")
	now = now.Add(59 * time.Minute)
	if !a.Valid(tok) {
		t.Fatal("still inside the TTL")
	}
	now = now.Add(2 * time.Minute)
	if a.Valid(tok) {
		t.Fatal("expired session accepted")
	}
	// Logging in sweeps expired sessions out of memory.
	old, _ := a.Login("pw")
	now = now.Add(2 * time.Hour)
	_, _ = a.Login("pw")
	a.mu.Lock()
	_, stillThere := a.sessions[key(old)]
	a.mu.Unlock()
	if stillThere {
		t.Error("expired session should be swept on login")
	}
}

func TestLoginFailsWhenRandomSourceFails(t *testing.T) {
	a := newAuth(t, "pw")
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = old })
	if tok, ok := a.Login("pw"); ok || tok != "" {
		t.Fatalf("a session must not be issued without randomness: %q %v", tok, ok)
	}
}

func TestDisabledAuthAllowsEverything(t *testing.T) {
	a := New(nil, nil, true, time.Hour)
	if !a.Disabled() || !a.Valid("anything") {
		t.Fatal("disabled auth must allow all requests")
	}
	if tok, ok := a.Login("whatever"); !ok || tok == "" {
		t.Fatal("login always succeeds when disabled")
	}
}

func TestNewSessionAndPasswordlessAuth(t *testing.T) {
	// With only Google sign-in there is no password hash: no password, not even "", may log in.
	a := New(nil, nil, false, time.Hour)
	if _, ok := a.Login(""); ok {
		t.Fatal("a password login must never work when no password is set")
	}
	if _, ok := a.Login("anything"); ok {
		t.Fatal("a password login must never work when no password is set")
	}
	tok, ok := a.NewSession()
	if !ok || len(tok) != 64 || !a.Valid(tok) {
		t.Fatalf("NewSession: %q %v", tok, ok)
	}
}

func TestLimiterBlocksAfterMaxFailuresUntilTheWindowPasses(t *testing.T) {
	l := NewLimiter(3, 15*time.Minute, 100)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allowed("a") {
			t.Fatalf("attempt %d must be allowed", i+1)
		}
		l.Failed("a")
	}
	if l.Allowed("a") {
		t.Fatal("blocked after three failures")
	}
	if !l.Allowed("b") {
		t.Fatal("other clients are unaffected")
	}
	now = now.Add(15 * time.Minute)
	if !l.Allowed("a") {
		t.Fatal("allowed again once the window has passed")
	}
	l.Failed("a")
	l.Failed("a")
	l.Succeeded("a")
	l.Failed("a")
	l.Failed("a")
	if !l.Allowed("a") {
		t.Fatal("a success clears the count")
	}
}

func TestLimiterCapacity(t *testing.T) {
	l := NewLimiter(5, time.Minute, 2)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	l.Failed("a")
	l.Failed("b")
	l.Failed("c") // not tracked: full
	if l.Allowed("c") || l.Allowed("d") {
		t.Fatal("with the table full, new clients are refused")
	}
	if !l.Allowed("a") {
		t.Fatal("tracked clients go on as usual")
	}
	now = now.Add(time.Minute)
	l.Failed("c") // the sweep frees the expired entries
	if len(l.fails) != 1 || !l.Allowed("d") {
		t.Fatalf("expired clients are swept: %v", l.fails)
	}
}

// logs collects what Persist reports.
type logs []string

func (l *logs) f(format string, args ...any) { *l = append(*l, fmt.Sprintf(format, args...)) }

func TestPersistSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	var l logs
	a := newAuth(t, "pw")
	if err := a.Persist(path, l.f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Persist should create the file, proving it can be written:", err)
	}
	keep, _ := a.Login("pw")
	gone, _ := a.Login("pw")
	a.Logout(gone)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), keep) || !strings.Contains(string(data), key(keep)) {
		t.Fatalf("the file must hold token digests, never tokens: %s", data)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("sessions file mode %v, want 0600", fi.Mode().Perm())
		}
	}

	b := newAuth(t, "pw") // the restarted server
	if err := b.Persist(path, l.f); err != nil {
		t.Fatal(err)
	}
	if !b.Valid(keep) || b.Valid(gone) {
		t.Fatal("after a restart only the live session should still work")
	}
	if len(l) != 0 {
		t.Errorf("nothing should be reported: %v", l)
	}
}

func TestPersistDropsExpiredSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Unix(1_000_000, 0)
	os.WriteFile(path, []byte(fmt.Sprintf(`{%q:%d,%q:%d}`, key("live"), now.Unix()+60, key("old"), now.Unix()-60)), 0o600)
	a := newAuth(t, "pw")
	a.now = func() time.Time { return now }
	if err := a.Persist(path, (&logs{}).f); err != nil {
		t.Fatal(err)
	}
	if !a.Valid("live") || a.Valid("old") {
		t.Fatal("expired sessions must not come back")
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), key("old")) {
		t.Error("expired sessions should be dropped from the file")
	}
}

func TestPersistDamagedFileSignsEveryoneOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	var l logs
	a := newAuth(t, "pw")
	if err := a.Persist(path, l.f); err != nil {
		t.Fatal(err)
	}
	if len(l) != 1 || !strings.Contains(l[0], "damaged") {
		t.Fatalf("a damaged file should be reported: %v", l)
	}
	if data, _ := os.ReadFile(path); string(data) != "{}" {
		t.Errorf("the damaged file should be replaced by an empty one: %s", data)
	}
}

func TestPersistErrors(t *testing.T) {
	dir := t.TempDir()
	// Unreadable: the path is a directory.
	if err := newAuth(t, "pw").Persist(dir, (&logs{}).f); err == nil {
		t.Error("an unreadable sessions file must stop the start")
	}
	// Unwritable: the folder does not exist.
	if err := newAuth(t, "pw").Persist(filepath.Join(dir, "missing", "s.json"), (&logs{}).f); err == nil {
		t.Error("an unwritable sessions file must stop the start")
	}
}

func TestSaveFailuresAreReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	var l logs
	a := newAuth(t, "pw")
	if err := a.Persist(filepath.Join(dir, "sessions.json"), l.f); err != nil {
		t.Fatal(err)
	}
	// The temporary file cannot be written.
	a.path = filepath.Join(dir, "missing", "s.json")
	tok, ok := a.Login("pw")
	if !ok || !a.Valid(tok) {
		t.Fatal("a session must still be issued when saving fails")
	}
	// The temporary file is written but cannot replace the target (a non-empty directory).
	target := filepath.Join(dir, "busy")
	os.MkdirAll(filepath.Join(target, "x"), 0o755)
	a.path = target
	a.Logout(tok)
	if len(l) != 2 || !strings.Contains(l[0], "saving sessions") || !strings.Contains(l[1], "saving sessions") {
		t.Fatalf("both failures should be reported: %v", l)
	}
}
