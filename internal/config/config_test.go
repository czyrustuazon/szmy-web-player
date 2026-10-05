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
	if c.MaxUploadMB != 512 || c.TranscodeWorkers != 2 || c.TrashMinutes != 10 || c.VgmstreamBin != "vgmstream-cli" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.AuthDisabled || c.CookieSecure || c.AdminPassword != "" {
		t.Fatalf("flags should default to off: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"MP_PORT": "9000", "MP_MUSIC_DIR": "/music", "MP_DATA_DIR": "/data",
		"MP_UPLOAD_SUBDIR": "inbox/new", "MP_ADMIN_PASSWORD": " keep spaces ",
		"MP_AUTH_DISABLED": "TRUE", "MP_COOKIE_SECURE": "1", "MP_MAX_UPLOAD_MB": "10",
		"MP_TRANSCODE_WORKERS": "4", "MP_TRASH_MINUTES": "3", "MP_CACHE_MB": "100",
		"MP_VGMSTREAM_BIN": "/opt/vgm/vgmstream-cli",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.UploadSubdir != "inbox/new" || !c.AuthDisabled || !c.CookieSecure {
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
