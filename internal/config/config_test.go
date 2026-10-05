package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8080 || c.MusicDir != "./music" || c.DataDir != "./data" || c.UploadSubdir != "uploads" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.MaxUploadMB != 61440 || c.MinFreeMB != 1024 || c.UploadTTLHours != 48 {
		t.Fatalf("unexpected upload defaults: %+v", c)
	}
	if c.TranscodeWorkers != 2 || c.TrashMinutes != 10 || c.VgmstreamBin != "vgmstream-cli" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if !c.AuthDisabled || c.CookieSecure || c.ReadOnly || c.AdminPassword != "" {
		t.Fatalf("no password set must mean open access: %+v", c)
	}
}

func TestAccessSettings(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowedNets != "tailscale,lan" || len(c.KnownDevices) != 0 || c.TailscaleSocket != "/var/run/tailscale/tailscaled.sock" {
		t.Fatalf("secure by default: %+v", c)
	}
	c, err = Load(env(map[string]string{
		"MP_ALLOWED_NETS": "tailscale, 192.168.1.0/24", "MP_KNOWN_DEVICES": "MainServer, 4090;iphone182",
		"MP_TAILSCALE_SOCKET": "/tmp/ts.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowedNets != "tailscale, 192.168.1.0/24" || strings.Join(c.KnownDevices, ",") != "mainserver,4090,iphone182" || c.TailscaleSocket != "/tmp/ts.sock" {
		t.Fatalf("overrides: %+v", c)
	}
	if _, err := Load(env(map[string]string{"MP_ALLOWED_NETS": "wifi"})); err == nil || !strings.Contains(err.Error(), "MP_ALLOWED_NETS") {
		t.Errorf("a typo in the network list must stop startup, not silently open or close the door: %v", err)
	}
}

func TestBlankPasswordMeansOpenAccess(t *testing.T) {
	for _, pw := range []string{"", " ", "\t \n"} {
		c, err := Load(env(map[string]string{"MP_ADMIN_PASSWORD": pw}))
		if err != nil || !c.AuthDisabled {
			t.Errorf("password %q: auth should be disabled, got %+v %v", pw, c, err)
		}
	}
	c, err := Load(env(map[string]string{"MP_ADMIN_PASSWORD": "hunter2"}))
	if err != nil || c.AuthDisabled {
		t.Errorf("a password must turn authentication on: %+v %v", c, err)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"MP_PORT": "9000", "MP_MUSIC_DIR": "/music", "MP_DATA_DIR": "/data",
		"MP_UPLOAD_SUBDIR": "inbox/new", "MP_ADMIN_PASSWORD": " keep spaces ",
		"MP_COOKIE_SECURE": "1", "MP_READ_ONLY": "yes", "MP_MAX_UPLOAD_MB": "10",
		"MP_TRANSCODE_WORKERS": "4", "MP_TRASH_MINUTES": "3", "MP_CACHE_MB": "100",
		"MP_VGMSTREAM_BIN": "/opt/vgm/vgmstream-cli",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.UploadSubdir != "inbox/new" || c.AuthDisabled || !c.CookieSecure || !c.ReadOnly {
		t.Fatalf("overrides not applied: %+v", c)
	}
	if c.AdminPassword != " keep spaces " {
		t.Fatalf("password must not be trimmed, got %q", c.AdminPassword)
	}
	if c.MaxUploadMB != 10 || c.TranscodeWorkers != 4 || c.TrashMinutes != 3 || c.CacheMB != 100 {
		t.Fatalf("numeric overrides not applied: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"not a number":    {"MP_PORT": "abc"},
		"port range":      {"MP_PORT": "70000"},
		"port zero":       {"MP_PORT": "0"},
		"upload size":     {"MP_MAX_UPLOAD_MB": "0"},
		"min free":        {"MP_MIN_FREE_MB": "-1"},
		"upload ttl":      {"MP_UPLOAD_TTL_HOURS": "0"},
		"workers":         {"MP_TRANSCODE_WORKERS": "0"},
		"trash minutes":   {"MP_TRASH_MINUTES": "0"},
		"subdir parent":   {"MP_UPLOAD_SUBDIR": "../x"},
		"subdir dot":      {"MP_UPLOAD_SUBDIR": "."},
		"subdir absolute": {"MP_UPLOAD_SUBDIR": "/etc"},
		"subdir hidden":   {"MP_UPLOAD_SUBDIR": "a/.b"},
	}
	for name, m := range cases {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPaths(t *testing.T) {
	c := Config{DataDir: "/d"}
	for _, p := range []string{c.CacheDir(), c.StatePath(), c.LogPath()} {
		if !strings.Contains(p, "d") {
			t.Fatalf("bad path %q", p)
		}
	}
	if !strings.HasSuffix(c.StatePath(), "state.json") || !strings.HasSuffix(c.LogPath(), "error.log") || !strings.HasSuffix(c.CacheDir(), "cache") {
		t.Fatal("unexpected file names")
	}
}

func googleEnv(extra map[string]string) func(string) string {
	m := map[string]string{
		"MP_GOOGLE_CLIENT_ID": "id.apps.googleusercontent.com", "MP_GOOGLE_CLIENT_SECRET": "sec",
		"MP_ALLOWED_EMAILS": "Me@Gmail.com, you@gmail.com", "MP_PUBLIC_URL": "https://music.example.org/",
	}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

func TestGoogleSignIn(t *testing.T) {
	c, err := Load(googleEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !c.GoogleEnabled || c.AuthDisabled {
		t.Fatalf("Google sign-in must turn the login on even without a password: %+v", c)
	}
	if strings.Join(c.AllowedEmails, ",") != "me@gmail.com,you@gmail.com" || c.PublicURL != "https://music.example.org" {
		t.Fatalf("allowlist / public url: %+v", c)
	}
	if !c.CookieSecure {
		t.Error("an https public address means a Secure cookie")
	}
	if c.GoogleRedirectURL() != "https://music.example.org/auth/google/callback" {
		t.Errorf("redirect: %s", c.GoogleRedirectURL())
	}
	c, err = Load(googleEnv(map[string]string{"MP_PUBLIC_URL": "http://localhost:8787"}))
	if err != nil || c.CookieSecure || c.GoogleRedirectURL() != "http://localhost:8787/auth/google/callback" {
		t.Errorf("http public address: %+v %v", c, err)
	}
	c, _ = Load(env(nil))
	if c.GoogleEnabled || len(c.AllowedEmails) != 0 {
		t.Errorf("off by default: %+v", c)
	}
}

func TestGoogleSignInMisconfiguration(t *testing.T) {
	cases := map[string]map[string]string{
		"no secret":         {"MP_GOOGLE_CLIENT_SECRET": ""},
		"no client id":      {"MP_GOOGLE_CLIENT_ID": ""},
		"no allowlist":      {"MP_ALLOWED_EMAILS": " "},
		"no public url":     {"MP_PUBLIC_URL": ""},
		"public url scheme": {"MP_PUBLIC_URL": "music.example.org"},
		"public url ftp":    {"MP_PUBLIC_URL": "ftp://music.example.org"},
		"public url path":   {"MP_PUBLIC_URL": "https://example.org/music"},
		"public url junk":   {"MP_PUBLIC_URL": "https://exa mple.org"},
	}
	for name, m := range cases {
		if _, err := Load(googleEnv(m)); err == nil {
			t.Errorf("%s: a half-configured Google sign-in must stop startup, never let everyone in", name)
		}
	}
}
