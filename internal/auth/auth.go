// Package auth implements the single-password login and session tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

const iterations = 100_000

// randRead is a seam so tests can simulate a failing random source.
var randRead = rand.Read

// NewSalt returns 16 random bytes.
func NewSalt() ([]byte, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return b, err
}

// Hash derives a password hash by iterated, salted SHA-256.
func Hash(salt []byte, password string) []byte {
	h := sha256.Sum256(append(append([]byte{}, salt...), password...))
	for i := 1; i < iterations; i++ {
		h = sha256.Sum256(h[:])
	}
	return h[:]
}

// Auth validates passwords and tracks sessions.
type Auth struct {
	disabled bool
	salt     []byte
	hash     []byte
	ttl      time.Duration
	now      func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time // key(token) -> expiry
	path     string               // sessions file; "" keeps sessions in memory only (see Persist)
	logf     func(string, ...any)
}

// key is how a session is stored, in memory and on disk: a SHA-256 of its token, so a copy of
// the sessions file holds nothing a browser could present.
func key(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// Persist keeps sessions in the file at path, so a restart does not sign everyone out. It loads
// the sessions already there (dropping expired ones), checks that the file can be written, and
// from then on saves it after every sign-in and sign-out. Deleting the file while the server is
// stopped signs everyone out (make logout-all). A damaged file is ignored: everyone signs in
// again. Failures to save later are reported through logf; the session itself still works until
// the next restart.
func (a *Auth) Persist(path string, logf func(string, ...any)) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	loaded := map[string]int64{}
	if len(data) > 0 && json.Unmarshal(data, &loaded) != nil {
		logf("sessions file %s is damaged; everyone has to sign in again", path)
		loaded = map[string]int64{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, exp := range loaded {
		if t := time.Unix(exp, 0); t.After(now) {
			a.sessions[k] = t
		}
	}
	a.path, a.logf = path, logf
	return a.save()
}

// save writes the sessions file (when there is one), replacing it atomically. Call with mu held.
func (a *Auth) save() error {
	if a.path == "" {
		return nil
	}
	out := make(map[string]int64, len(a.sessions))
	for k, exp := range a.sessions {
		out[k] = exp.Unix()
	}
	data, _ := json.Marshal(out) // a map of strings to numbers always encodes
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// saved saves after a change, reporting a failure instead of failing the request. Call with mu held.
func (a *Auth) saved() {
	if err := a.save(); err != nil {
		a.logf("saving sessions: %v", err)
	}
}

// New builds an Auth. With disabled=true every request is allowed.
func New(salt, hash []byte, disabled bool, ttl time.Duration) *Auth {
	return &Auth{disabled: disabled, salt: salt, hash: hash, ttl: ttl, now: time.Now, sessions: map[string]time.Time{}}
}

// Disabled reports whether authentication is switched off.
func (a *Auth) Disabled() bool { return a.disabled }

// Check compares a password in constant time. Without a stored hash (Google sign-in only) no
// password matches, and nothing is hashed, so guesses cost the server no CPU.
func (a *Auth) Check(password string) bool {
	if a.hash == nil {
		return false
	}
	return subtle.ConstantTimeCompare(Hash(a.salt, password), a.hash) == 1
}

// Login returns a new session token when the password is right.
func (a *Auth) Login(password string) (string, bool) {
	if !a.disabled && !a.Check(password) {
		return "", false
	}
	return a.NewSession()
}

// NewSession issues a session token without checking a password, for sign-in methods that
// have already proved who the user is (Google sign-in).
func (a *Auth) NewSession() (string, bool) {
	b := make([]byte, 32)
	if _, err := randRead(b); err != nil {
		return "", false
	}
	token := hex.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for t, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, t)
		}
	}
	a.sessions[key(token)] = now.Add(a.ttl)
	a.saved()
	return token, true
}

// Valid reports whether token is a live session.
func (a *Auth) Valid(token string) bool {
	if a.disabled {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	k := key(token)
	exp, ok := a.sessions[k]
	if !ok {
		return false
	}
	if a.now().After(exp) {
		delete(a.sessions, k) // dropped from the file at the next save
		return false
	}
	return true
}

// Logout ends a session.
func (a *Auth) Logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, key(token))
	a.saved()
}

// Limiter slows down password guessing. After max failures from one client within window,
// that client is refused until the window has passed. It remembers at most capacity clients;
// when that many are being tracked, new ones are refused too (an attack from many addresses
// is slowed down as a whole rather than allowed to grow memory without bound).
type Limiter struct {
	max      int
	window   time.Duration
	capacity int
	now      func() time.Time

	mu    sync.Mutex
	fails map[string]*strikes
}

type strikes struct {
	n     int
	since time.Time
}

// NewLimiter builds a Limiter.
func NewLimiter(max int, window time.Duration, capacity int) *Limiter {
	return &Limiter{max: max, window: window, capacity: capacity, now: time.Now, fails: map[string]*strikes{}}
}

// live returns the client's failures in the current window (nil if none). Call with mu held.
func (l *Limiter) live(key string) *strikes {
	s := l.fails[key]
	if s != nil && l.now().Sub(s.since) >= l.window {
		delete(l.fails, key)
		return nil
	}
	return s
}

// Allowed reports whether the client may try a password now.
func (l *Limiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.live(key); s != nil {
		return s.n < l.max
	}
	return len(l.fails) < l.capacity
}

// Failed records a wrong password.
func (l *Limiter) Failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.live(key); s != nil {
		s.n++
		return
	}
	for k := range l.fails { // sweep, so the capacity counts live clients only
		l.live(k)
	}
	if len(l.fails) < l.capacity {
		l.fails[key] = &strikes{n: 1, since: l.now()}
	}
}

// Succeeded forgets the client's failures.
func (l *Limiter) Succeeded(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
