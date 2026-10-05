package auth

import (
	"bytes"
	"errors"
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
	_, stillThere := a.sessions[old]
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
