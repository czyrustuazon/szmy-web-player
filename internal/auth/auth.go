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
