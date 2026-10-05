package google

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testClient = "client-123.apps.googleusercontent.com"
	testSecret = "s3cret"
	testRedir  = "https://music.example.org/auth/google/callback"
)

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "hdr." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func goodClaims(nonce string) map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": testClient, "exp": time.Now().Add(time.Hour).Unix(),
		"nonce": nonce, "email": "Me@Gmail.com", "email_verified": true,
	}
}

// fake is a stand-in for Google's token endpoint. It remembers the last request and answers
// with whatever id_token build returns for the nonce of the sign-in under test.
type fake struct {
	srv    *httptest.Server
	form   url.Values
	status int
	body   func(nonce string) string
	nonce  string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.form = r.PostForm
		w.WriteHeader(f.status)
		if f.body != nil {
			_, _ = w.Write([]byte(f.body(f.nonce)))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newClient(f *fake, emails ...string) *Client {
	if len(emails) == 0 {
		emails = []string{"me@gmail.com"}
	}
	c := New(testClient, testSecret, testRedir, emails)
	c.tokenURL = f.srv.URL
	return c
}

// begin starts a sign-in and returns the state, the browser's cookie and the parsed Google URL.
func begin(t *testing.T, c *Client) (string, string, url.Values) {
	t.Helper()
	state, cookie, redirect, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	return state, cookie, u.Query()
}

func tokenBody(claims func(nonce string) map[string]any) func(string) string {
	return func(nonce string) string {
		b, _ := json.Marshal(map[string]string{"id_token": jwt(claims(nonce))})
		return string(b)
	}
}

func TestParseEmails(t *testing.T) {
	got := ParseEmails(" Me@Gmail.com, you@gmail.com;  x@y.z ,,")
	if strings.Join(got, "|") != "me@gmail.com|you@gmail.com|x@y.z" {
		t.Fatalf("got %v", got)
	}
	if len(ParseEmails("")) != 0 {
		t.Fatal("blank means no addresses")
	}
}

func TestStartBuildsGoogleURL(t *testing.T) {
	c := New(testClient, testSecret, testRedir, nil)
	state, cookie, q := begin(t, c)
	if len(state) != 64 || q.Get("state") != state {
		t.Fatalf("state %q / %q", state, q.Get("state"))
	}
	if !strings.HasPrefix(cookie, state+".") || strings.Contains(q.Encode(), strings.Split(cookie, ".")[1]) {
		t.Errorf("the cookie carries the state and a verifier that never goes to Google: %q", cookie)
	}
	for k, want := range map[string]string{
		"client_id": testClient, "redirect_uri": testRedir, "response_type": "code",
		"scope": "openid email", "code_challenge_method": "S256", "prompt": "select_account",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("nonce") == "" || len(q.Get("code_challenge")) != 43 {
		t.Errorf("nonce / challenge missing: %v", q)
	}
	state2, _, _ := begin(t, c)
	if state2 == state {
		t.Fatal("every sign-in needs its own state")
	}
}

func TestStartFailsWithoutRandomness(t *testing.T) {
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = old })
	if _, _, _, err := New("a", "b", "c", nil).Start(); err == nil {
		t.Fatal("must not start a sign-in without randomness (signing key)")
	}
	c := New("a", "b", "c", nil)
	randRead = old
	if _, _, _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if _, _, _, err := c.Start(); err == nil {
		t.Fatal("must not start a sign-in without randomness (state)")
	}
}

func TestStartKeepsNothingPerAttempt(t *testing.T) {
	// Anyone on the internet can call Start; a flood of unfinished sign-ins must neither use
	// memory nor lock the owner out.
	f := newFake(t)
	f.body = tokenBody(goodClaims)
	c := newClient(f)
	for i := 0; i < 5000; i++ {
		if _, _, _, err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.used) != 0 {
		t.Fatalf("unfinished sign-ins are not stored: %d", len(c.used))
	}
	state, cookie, q := begin(t, c)
	f.nonce = q.Get("nonce")
	if _, err := c.Finish(context.Background(), cookie, state, "x"); err != nil {
		t.Fatalf("the owner still gets in: %v", err)
	}
}

func TestUsedStatesAreForgottenAfterTheyExpire(t *testing.T) {
	f := newFake(t)
	f.body = tokenBody(goodClaims)
	c := newClient(f)
	now := time.Now()
	c.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		state, cookie, q := begin(t, c)
		f.nonce = q.Get("nonce")
		if _, err := c.Finish(context.Background(), cookie, state, "x"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(pendingTTL + time.Second)
	}
	if len(c.used) != 1 {
		t.Fatalf("expired entries are swept: %d", len(c.used))
	}
}

func TestFinishSuccess(t *testing.T) {
	f := newFake(t)
	f.body = tokenBody(goodClaims)
	c := newClient(f)
	state, cookie, q := begin(t, c)
	f.nonce = q.Get("nonce")

	email, err := c.Finish(context.Background(), cookie, state, "the-code")
	if err != nil || email != "me@gmail.com" {
		t.Fatalf("got %q, %v", email, err)
	}
	for k, want := range map[string]string{
		"code": "the-code", "client_id": testClient, "client_secret": testSecret,
		"redirect_uri": testRedir, "grant_type": "authorization_code",
	} {
		if f.form.Get(k) != want {
			t.Errorf("token request %s = %q, want %q", k, f.form.Get(k), want)
		}
	}
	// PKCE: the verifier sent to Google must hash to the challenge sent earlier.
	sum := sha256.Sum256([]byte(f.form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Error("code_verifier does not match the code_challenge")
	}
	// A state is single use.
	if _, err := c.Finish(context.Background(), cookie, state, "the-code"); !errors.Is(err, ErrState) {
		t.Fatalf("replayed state: %v", err)
	}
}

func TestFinishAcceptsStringEmailVerified(t *testing.T) {
	f := newFake(t)
	f.body = tokenBody(func(n string) map[string]any {
		c := goodClaims(n)
		c["email_verified"] = "true"
		c["iss"] = "accounts.google.com"
		return c
	})
	c := newClient(f)
	state, cookie, q := begin(t, c)
	f.nonce = q.Get("nonce")
	if _, err := c.Finish(context.Background(), cookie, state, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestFinishRefusesBadCookies(t *testing.T) {
	f := newFake(t)
	f.body = tokenBody(goodClaims)
	c := newClient(f)
	if _, err := c.Finish(context.Background(), "a.b.c.d.e", "a", "x"); !errors.Is(err, ErrState) {
		t.Fatalf("before any Start there is no key: %v", err)
	}
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	state, cookie, _ := begin(t, c)
	parts := strings.Split(cookie, ".")
	resign := func(p ...string) string {
		b := strings.Join(p, ".")
		return b + "." + mac(c.key, b)
	}
	_, foreign, _, _ := New(testClient, testSecret, testRedir, nil).Start() // signed with another key
	cases := map[string][2]string{
		"no cookie":      {"", state},
		"too few parts":  {"a.b.c", state},
		"forged mac":     {strings.Join(parts[:4], ".") + "." + strings.Repeat("0", 64), state},
		"other state":    {cookie, strings.Repeat("a", 64)},
		"bad expiry":     {resign(parts[0], parts[1], parts[2], "soon"), state},
		"another client": {foreign, state},
	}
	for name, cs := range cases {
		if _, err := c.Finish(context.Background(), cs[0], cs[1], "x"); !errors.Is(err, ErrState) {
			t.Errorf("%s: %v", name, err)
		}
	}
	now = now.Add(pendingTTL + time.Second)
	if _, err := c.Finish(context.Background(), cookie, state, "x"); !errors.Is(err, ErrState) {
		t.Fatalf("expired: %v", err)
	}
}

func TestFinishRejectsBadTokens(t *testing.T) {
	mutate := func(key string, val any) func(string) map[string]any {
		return func(n string) map[string]any {
			c := goodClaims(n)
			if val == nil {
				delete(c, key)
			} else {
				c[key] = val
			}
			return c
		}
	}
	cases := map[string]func(string) map[string]any{
		"wrong issuer":   mutate("iss", "https://evil.example"),
		"wrong audience": mutate("aud", "someone-else"),
		"expired":        mutate("exp", time.Now().Add(-time.Minute).Unix()),
		"wrong nonce":    mutate("nonce", "other"),
		"unverified":     mutate("email_verified", false),
		"no verified":    mutate("email_verified", nil),
	}
	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.body = tokenBody(claims)
			c := newClient(f)
			state, cookie, q := begin(t, c)
			f.nonce = q.Get("nonce")
			email, err := c.Finish(context.Background(), cookie, state, "x")
			if err == nil || errors.Is(err, ErrDenied) || email != "" {
				t.Fatalf("must be refused as invalid, got %q, %v", email, err)
			}
		})
	}
}

func TestFinishDeniedAddressIsReported(t *testing.T) {
	for _, email := range []string{"stranger@gmail.com", ""} {
		f := newFake(t)
		f.body = tokenBody(func(n string) map[string]any {
			c := goodClaims(n)
			c["email"] = email
			return c
		})
		c := newClient(f)
		state, cookie, q := begin(t, c)
		f.nonce = q.Get("nonce")
		got, err := c.Finish(context.Background(), cookie, state, "x")
		if !errors.Is(err, ErrDenied) || got != email {
			t.Fatalf("%q: got %q, %v", email, got, err)
		}
	}
}

func TestFinishEmailAllowlistIgnoresCaseAndSpaces(t *testing.T) {
	f := newFake(t)
	f.body = tokenBody(goodClaims) // token says Me@Gmail.com
	c := newClient(f, "  ME@gmail.COM ")
	state, cookie, q := begin(t, c)
	f.nonce = q.Get("nonce")
	if _, err := c.Finish(context.Background(), cookie, state, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestFinishTransportAndPayloadErrors(t *testing.T) {
	run := func(name string, setup func(f *fake, c *Client)) {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c := newClient(f)
			setup(f, c)
			state, cookie, q := begin(t, c)
			f.nonce = q.Get("nonce")
			email, err := c.Finish(context.Background(), cookie, state, "x")
			if err == nil || errors.Is(err, ErrDenied) || errors.Is(err, ErrState) || email != "" {
				t.Fatalf("got %q, %v", email, err)
			}
		})
	}
	run("google says no", func(f *fake, _ *Client) { f.status = 400 })
	run("not json", func(f *fake, _ *Client) { f.body = func(string) string { return "<html>" } })
	run("no id token", func(f *fake, _ *Client) { f.body = func(string) string { return "{}" } })
	run("token not base64", func(f *fake, _ *Client) {
		f.body = func(string) string { return `{"id_token":"a.!!!.c"}` }
	})
	run("token payload not json", func(f *fake, _ *Client) {
		f.body = func(string) string {
			return `{"id_token":"a.` + base64.RawURLEncoding.EncodeToString([]byte("nope")) + `.c"}`
		}
	})
	run("unreachable", func(f *fake, c *Client) { f.srv.Close() })
	run("bad token url", func(f *fake, c *Client) { c.tokenURL = "http://[::1" })
}

func TestSetEndpoints(t *testing.T) {
	c := New(testClient, testSecret, testRedir, nil)
	c.SetEndpoints("https://idp.example/auth", "https://idp.example/token")
	_, _, q := begin(t, c)
	_, _, redirect, _ := c.Start()
	if !strings.HasPrefix(redirect, "https://idp.example/auth?") || c.tokenURL != "https://idp.example/token" || q.Get("client_id") != testClient {
		t.Fatalf("endpoints not applied: %s %s", redirect, c.tokenURL)
	}
}
