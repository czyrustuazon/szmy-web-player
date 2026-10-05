package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"masterplayer/internal/google"
)

const gClient = "client-123.apps.googleusercontent.com"

// gfake is the stand-in for Google: its token endpoint vouches for email, echoing the nonce of
// the sign-in under test (which startGoogle reads from the URL the browser is sent to).
type gfake struct {
	mu    sync.Mutex // the token endpoint runs on its own goroutine
	nonce string
	*google.Client
}

// googleEnv is an env whose server offers Sign in with Google against a fake Google.
func googleEnv(t *testing.T, email string) (*env, *gfake) {
	t.Helper()
	e := newEnv(t, false, true)
	g := &gfake{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		nonce := g.nonce
		g.mu.Unlock()
		claims, _ := json.Marshal(map[string]any{
			"iss": "https://accounts.google.com", "aud": gClient, "exp": time.Now().Add(time.Hour).Unix(),
			"nonce": nonce, "email": email, "email_verified": true,
		})
		idToken := "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": idToken})
	}))
	t.Cleanup(fake.Close)
	g.Client = google.New(gClient, "sec", "https://music.example.org/auth/google/callback", []string{"me@gmail.com"})
	g.SetEndpoints("https://accounts.google.test/auth", fake.URL)
	e.srv.Google = g.Client
	e.srv.Cfg.AdminPassword = "pw"
	e.srv.Cfg.CookieSecure = true
	e.h = e.srv.Handler()
	return e, g
}

// startGoogle performs /auth/google/start and returns the state cookie and the state.
func startGoogle(t *testing.T, e *env, g *gfake) (*http.Cookie, string) {
	t.Helper()
	rec := e.do("GET", "/auth/google/start", nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "accounts.google.test" {
		t.Fatalf("start must redirect to Google, got %q", rec.Header().Get("Location"))
	}
	g.mu.Lock()
	g.nonce = loc.Query().Get("nonce")
	g.mu.Unlock()
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthCookie {
			cookie = c
		}
	}
	state := loc.Query().Get("state")
	if cookie == nil || cookie.Value != state || !cookie.HttpOnly || !cookie.Secure ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/auth/google/" {
		t.Fatalf("state cookie must bind the browser to the sign-in: %+v", cookie)
	}
	return cookie, state
}

func callback(e *env, cookie *http.Cookie, query string) *httptest.ResponseRecorder {
	return e.do("GET", "/auth/google/callback?"+query, nil, cookie)
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge > 0 {
			return c
		}
	}
	t.Fatalf("no session cookie in %v", rec.Result().Cookies())
	return nil
}

func TestGoogleSignInEndToEnd(t *testing.T) {
	e, g := googleEnv(t, "Me@Gmail.com")
	wantStatus(t, e.do("GET", "/api/browse", nil, nil), 401)

	cookie, state := startGoogle(t, e, g)
	rec := callback(e, cookie, "state="+state+"&code=abc")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	sc := sessionCookie(t, rec)
	if !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie flags: %+v", sc)
	}
	wantStatus(t, e.do("GET", "/api/browse", nil, sc), 200)

	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, sc), &sess)
	if sess["authenticated"] != true || sess["google"] != true || sess["password"] != true {
		t.Errorf("session: %v", sess)
	}
	// The state is single use: replaying the callback must not sign anyone in.
	wantStatus(t, callback(e, cookie, "state="+state+"&code=abc"), 400)
}

func TestGoogleOnlyHidesPasswordForm(t *testing.T) {
	e, _ := googleEnv(t, "me@gmail.com")
	e.srv.Cfg.AdminPassword = ""
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["google"] != true || sess["password"] != false || sess["authRequired"] != true {
		t.Errorf("session: %v", sess)
	}
}

func TestGoogleRoutesAbsentWhenNotConfigured(t *testing.T) {
	e := newEnv(t, false, true)
	wantStatus(t, e.do("GET", "/auth/google/start", nil, nil), 404)
	wantStatus(t, e.do("GET", "/auth/google/callback?state=x", nil, nil), 404)
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["google"] != false || sess["password"] != true {
		t.Errorf("session: %v", sess)
	}
}

func TestGoogleCallbackRefusals(t *testing.T) {
	e, g := googleEnv(t, "me@gmail.com")
	cookie, state := startGoogle(t, e, g)

	// No cookie, a different cookie, or no state: the browser did not start this sign-in.
	wantStatus(t, callback(e, nil, "state="+state+"&code=abc"), 400)
	wantStatus(t, callback(e, &http.Cookie{Name: oauthCookie, Value: "other"}, "state="+state+"&code=abc"), 400)
	wantStatus(t, callback(e, &http.Cookie{Name: oauthCookie, Value: ""}, "code=abc"), 400)
	// A matching cookie for a state the server never issued.
	wantStatus(t, callback(e, &http.Cookie{Name: oauthCookie, Value: "forged"}, "state=forged&code=abc"), 400)
	// The user pressed Cancel at Google.
	rec := callback(e, cookie, "state="+state+"&error=access_denied")
	wantStatus(t, rec, 401)
	if rec.Header().Get("Set-Cookie") == "" || strings.Contains(rec.Header().Get("Set-Cookie"), "mp_session") {
		t.Errorf("a refusal must clear the state cookie and issue no session: %v", rec.Header())
	}
}

func TestGoogleCallbackRefusesStrangers(t *testing.T) {
	e, g := googleEnv(t, "stranger@gmail.com")
	cookie, state := startGoogle(t, e, g)
	rec := callback(e, cookie, "state="+state+"&code=abc")
	wantStatus(t, rec, 403)
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge > 0 {
			t.Fatal("a refused account must not get a session")
		}
	}
	lines, _ := e.logs.Recent(10)
	if len(lines) != 1 || !strings.Contains(lines[0], "stranger@gmail.com") {
		t.Errorf("the refused address should be logged: %v", lines)
	}
}

func TestGoogleCallbackWhenGoogleFails(t *testing.T) {
	e, g := googleEnv(t, "me@gmail.com")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	t.Cleanup(bad.Close)
	g.SetEndpoints("https://accounts.google.test/auth", bad.URL)
	cookie, state := startGoogle(t, e, g)
	wantStatus(t, callback(e, cookie, "state="+state+"&code=abc"), 502)
	if lines, _ := e.logs.Recent(10); len(lines) != 1 {
		t.Errorf("a failed exchange is logged: %v", lines)
	}
}

func TestGoogleCallbackWithoutSessionRandomness(t *testing.T) {
	e, g := googleEnv(t, "me@gmail.com")
	e.srv.newSession = func() (string, bool) { return "", false }
	cookie, state := startGoogle(t, e, g)
	wantStatus(t, callback(e, cookie, "state="+state+"&code=abc"), 500)
}

func TestGoogleStartRefusesAFlood(t *testing.T) {
	e, _ := googleEnv(t, "me@gmail.com")
	var last int
	for i := 0; i < 1100; i++ {
		last = e.do("GET", "/auth/google/start", nil, nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("unfinished sign-ins must be capped, last status %d", last)
	}
}
