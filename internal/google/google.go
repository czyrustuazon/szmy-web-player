// Package google implements "Sign in with Google" (OpenID Connect, authorization code flow
// with PKCE) for a short list of allowed Gmail / Google accounts.
//
// It lets the player be reached through a public tunnel (Cloudflare Tunnel and the like): the
// network allowlist cannot tell visitors apart there, so who you are is proved by Google
// instead. Only addresses on the allowlist get a session; everyone else is refused, however
// valid their Google account is.
//
// The ID token is received straight from Google's token endpoint over TLS, in exchange for a
// one-time code and the PKCE verifier, so its signature is not checked separately (OpenID
// Connect Core 3.1.3.7, rule 6 allows this). Every claim that matters is checked: issuer,
// audience (our client id), expiry, nonce, a verified email and the allowlist.
package google

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrState means the sign-in attempt is unknown or too old (or was already used).
	ErrState = errors.New("sign-in expired or invalid; please try again")
	// ErrDenied means Google vouched for the user, but the address is not on the allowlist.
	ErrDenied = errors.New("this Google account is not allowed")
	// ErrBusy means too many sign-in attempts are waiting to be completed.
	ErrBusy = errors.New("too many sign-ins in progress; please try again in a few minutes")
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	pendingTTL    = 10 * time.Minute
	maxPending    = 1000
)

// randRead is a seam so tests can simulate a failing random source.
var randRead = rand.Read

type pending struct {
	verifier string
	nonce    string
	expires  time.Time
}

// Client runs the sign-in flow. It is safe for concurrent use.
type Client struct {
	clientID     string
	clientSecret string
	redirectURL  string
	emails       map[string]bool

	authURL  string
	tokenURL string
	http     *http.Client
	now      func() time.Time

	mu      sync.Mutex
	pending map[string]pending
}

// New builds a Client. redirectURL is the exact callback registered in the Google Cloud
// console; emails is the allowlist.
func New(clientID, clientSecret, redirectURL string, emails []string) *Client {
	c := &Client{
		clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL,
		emails:   map[string]bool{},
		authURL:  authEndpoint,
		tokenURL: tokenEndpoint,
		http:     &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		pending:  map[string]pending{},
	}
	for _, e := range emails {
		c.emails[strings.ToLower(strings.TrimSpace(e))] = true
	}
	return c
}

// ParseEmails splits a list of addresses (comma, space or semicolon separated), lower-cased.
func ParseEmails(spec string) []string {
	var out []string
	for _, e := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		out = append(out, strings.ToLower(e))
	}
	return out
}

// Start begins a sign-in. It returns the state to bind to the browser (a cookie) and the Google
// URL to send the browser to.
func (c *Client) Start() (state, redirect string, err error) {
	b := make([]byte, 96)
	if _, err := randRead(b); err != nil {
		return "", "", err
	}
	state, verifier, nonce := hex.EncodeToString(b[:32]), hex.EncodeToString(b[32:64]), hex.EncodeToString(b[64:])

	now := c.now()
	c.mu.Lock()
	for s, p := range c.pending {
		if now.After(p.expires) {
			delete(c.pending, s)
		}
	}
	if len(c.pending) >= maxPending {
		c.mu.Unlock()
		return "", "", ErrBusy
	}
	c.pending[state] = pending{verifier: verifier, nonce: nonce, expires: now.Add(pendingTTL)}
	c.mu.Unlock()

	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {c.clientID},
		"redirect_uri":          {c.redirectURL},
		"response_type":         {"code"},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return state, c.authURL + "?" + q.Encode(), nil
}

// Finish completes a sign-in: it trades the code for an ID token and checks it. On success it
// returns the verified email address. If the account is valid but not allowed it returns the
// address together with ErrDenied, so the caller can log who was refused.
func (c *Client) Finish(ctx context.Context, state, code string) (string, error) {
	c.mu.Lock()
	p, ok := c.pending[state]
	delete(c.pending, state) // a state works once
	c.mu.Unlock()
	if !ok || c.now().After(p.expires) {
		return "", ErrState
	}

	form := url.Values{
		"code":          {code},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"redirect_uri":  {c.redirectURL},
		"grant_type":    {"authorization_code"},
		"code_verifier": {p.verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach Google: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Google refused the sign-in code (%s)", resp.Status)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("unreadable answer from Google: %w", err)
	}
	return c.verify(tok.IDToken, p.nonce)
}

// verify checks the claims of an ID token received from Google's token endpoint.
func (c *Client) verify(idToken, nonce string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("Google sent no usable ID token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("Google sent an unreadable ID token")
	}
	var cl struct {
		Iss           string `json:"iss"`
		Aud           string `json:"aud"`
		Exp           int64  `json:"exp"`
		Nonce         string `json:"nonce"`
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"` // a bool, or the string "true"
	}
	if err := json.Unmarshal(raw, &cl); err != nil {
		return "", errors.New("Google sent an unreadable ID token")
	}
	switch {
	case cl.Iss != "https://accounts.google.com" && cl.Iss != "accounts.google.com":
		return "", errors.New("ID token has the wrong issuer")
	case cl.Aud != c.clientID:
		return "", errors.New("ID token is for a different client")
	case c.now().Unix() >= cl.Exp:
		return "", errors.New("ID token has expired")
	case cl.Nonce != nonce:
		return "", errors.New("ID token does not match this sign-in")
	case cl.EmailVerified != true && cl.EmailVerified != "true":
		return "", errors.New("Google has not verified this email address")
	}
	email := strings.ToLower(strings.TrimSpace(cl.Email))
	if email == "" || !c.emails[email] {
		return email, ErrDenied
	}
	return email, nil
}

// SetEndpoints points the client at other authorization and token endpoints: a stand-in for
// Google in tests, or another OpenID Connect provider that issues the same claims.
func (c *Client) SetEndpoints(authURL, tokenURL string) {
	c.authURL, c.tokenURL = authURL, tokenURL
}
