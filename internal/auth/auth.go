// Package auth implements the single-password login and session tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	sessions map[string]time.Time
}

// New builds an Auth. With disabled=true every request is allowed.
func New(salt, hash []byte, disabled bool, ttl time.Duration) *Auth {
	return &Auth{disabled: disabled, salt: salt, hash: hash, ttl: ttl, now: time.Now, sessions: map[string]time.Time{}}
}

// Disabled reports whether authentication is switched off.
func (a *Auth) Disabled() bool { return a.disabled }

// Check compares a password in constant time.
func (a *Auth) Check(password string) bool {
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
	a.sessions[token] = now.Add(a.ttl)
	return token, true
}

// Valid reports whether token is a live session.
func (a *Auth) Valid(token string) bool {
	if a.disabled {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	if a.now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	return true
}

// Logout ends a session.
func (a *Auth) Logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
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
