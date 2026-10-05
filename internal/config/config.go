// Package config loads runtime settings from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"masterplayer/internal/access"
	"masterplayer/internal/google"
)

// Config is the full runtime configuration.
type Config struct {
	Port             int
	MusicDir         string
	DataDir          string
	UploadSubdir     string // relative to MusicDir, slash separated
	AdminPassword    string
	AuthDisabled     bool // derived: true when AdminPassword is blank
	MaxUploadMB      int64 // largest single upload (and extracted archive), in MB
	MinFreeMB        int64 // free space that must remain after an upload, in MB (0 = no check)
	UploadTTLHours   int   // abandoned upload sessions are purged after this many hours
	CacheMB          int64
	VgmstreamBin     string
	TranscodeWorkers int
	TrashMinutes     int
	CookieSecure     bool
	ReadOnly         bool // force read-only: no delete, no upload

	AllowedNets     string   // who may connect, by network: see package access (default "tailscale,lan")
	KnownDevices    []string // if set, Tailscale peers must be one of these device names
	TailscaleSocket string   // tailscaled's local API socket, used to name Tailscale peers
	TrustedProxies  string   // proxies/tunnels whose forwarding headers are believed ("none" = never), default "loopback,lan"
	TunnelNets      string   // where a visitor arriving through such a proxy may come from, default "any"

	// Sign in with Google, for reaching the player through a public tunnel. Only the listed
	// addresses get in. Needs MP_PUBLIC_URL, the address people type (https://music.example.org).
	GoogleClientID     string
	GoogleClientSecret string
	AllowedEmails      []string
	PublicURL          string // no trailing slash
	GoogleEnabled      bool   // derived: a client id is set
}

// Load reads configuration through getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	var firstErr error
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int64) int64 {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %q is not a number", key, v)
		}
		return n
	}
	boolean := func(key string) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(key))) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}

	c := Config{
		Port:             int(num("MP_PORT", 8080)),
		MusicDir:         str("MP_MUSIC_DIR", "./music"),
		DataDir:          str("MP_DATA_DIR", "./data"),
		UploadSubdir:     str("MP_UPLOAD_SUBDIR", "uploads"),
		AdminPassword:    getenv("MP_ADMIN_PASSWORD"),
		MaxUploadMB:     num("MP_MAX_UPLOAD_MB", 61440), // 60 GiB, like anime-db-stream
		MinFreeMB:       num("MP_MIN_FREE_MB", 1024),
		UploadTTLHours:  int(num("MP_UPLOAD_TTL_HOURS", 48)),
		CacheMB:          num("MP_CACHE_MB", 2048),
		VgmstreamBin:     str("MP_VGMSTREAM_BIN", "vgmstream-cli"),
		TranscodeWorkers: int(num("MP_TRANSCODE_WORKERS", 2)),
		TrashMinutes:     int(num("MP_TRASH_MINUTES", 10)),
		CookieSecure:     boolean("MP_COOKIE_SECURE"),
		ReadOnly:         boolean("MP_READ_ONLY"),

		AllowedNets:     str("MP_ALLOWED_NETS", "tailscale,lan"),
		KnownDevices:    access.ParseDevices(getenv("MP_KNOWN_DEVICES")),
		TailscaleSocket: str("MP_TAILSCALE_SOCKET", "/var/run/tailscale/tailscaled.sock"),
		TrustedProxies:  str("MP_TRUSTED_PROXIES", "loopback,lan"),
		TunnelNets:      str("MP_TUNNEL_NETS", "any"),

		GoogleClientID:     str("MP_GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: str("MP_GOOGLE_CLIENT_SECRET", ""),
		AllowedEmails:      google.ParseEmails(getenv("MP_ALLOWED_EMAILS")),
		PublicURL:          strings.TrimRight(str("MP_PUBLIC_URL", ""), "/"),
	}
	if firstErr != nil {
		return Config{}, firstErr
	}
	c.GoogleEnabled = c.GoogleClientID != "" || c.GoogleClientSecret != ""
	if c.GoogleEnabled {
		if err := c.checkGoogle(); err != nil {
			return Config{}, err
		}
		// Signing in over https: the session cookie must never travel over plain http.
		if strings.HasPrefix(c.PublicURL, "https://") {
			c.CookieSecure = true
		}
	}
	// A blank (or whitespace-only) password and no Google sign-in means open access: no login at all.
	c.AuthDisabled = strings.TrimSpace(c.AdminPassword) == "" && !c.GoogleEnabled

	if c.Port < 1 || c.Port > 65535 {
		return Config{}, fmt.Errorf("MP_PORT: %d is out of range", c.Port)
	}
	if c.MaxUploadMB < 1 {
		return Config{}, fmt.Errorf("MP_MAX_UPLOAD_MB must be at least 1")
	}
	if c.MinFreeMB < 0 {
		return Config{}, fmt.Errorf("MP_MIN_FREE_MB must not be negative")
	}
	if c.UploadTTLHours < 1 {
		return Config{}, fmt.Errorf("MP_UPLOAD_TTL_HOURS must be at least 1")
	}
	if c.TranscodeWorkers < 1 {
		return Config{}, fmt.Errorf("MP_TRANSCODE_WORKERS must be at least 1")
	}
	if _, err := access.ParseNets(c.AllowedNets); err != nil {
		return Config{}, fmt.Errorf("MP_ALLOWED_NETS: %w", err)
	}
	if _, err := c.ProxyNets(); err != nil {
		return Config{}, fmt.Errorf("MP_TRUSTED_PROXIES: %w", err)
	}
	if _, err := access.ParseNets(c.TunnelNets); err != nil {
		return Config{}, fmt.Errorf("MP_TUNNEL_NETS: %w", err)
	}
	if c.TrashMinutes < 1 {
		return Config{}, fmt.Errorf("MP_TRASH_MINUTES must be at least 1")
	}
	sub, err := cleanSubdir(c.UploadSubdir)
	if err != nil {
		return Config{}, err
	}
	c.UploadSubdir = sub
	return c, nil
}

// checkGoogle makes sure Google sign-in cannot be switched on half-configured: without an
// allowlist every Google account in the world would be let in.
func (c Config) checkGoogle() error {
	if c.GoogleClientID == "" || c.GoogleClientSecret == "" {
		return fmt.Errorf("MP_GOOGLE_CLIENT_ID and MP_GOOGLE_CLIENT_SECRET must both be set")
	}
	if len(c.AllowedEmails) == 0 {
		return fmt.Errorf("MP_ALLOWED_EMAILS is required with Google sign-in: list the addresses that may get in")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("MP_PUBLIC_URL must be the address people type, like https://music.example.org (got %q)", c.PublicURL)
	}
	return nil
}

// ProxyNets parses MP_TRUSTED_PROXIES; "none" means no proxy is trusted (nil).
func (c Config) ProxyNets() ([]netip.Prefix, error) {
	if strings.EqualFold(c.TrustedProxies, "none") {
		return nil, nil
	}
	return access.ParseNets(c.TrustedProxies)
}

// GoogleRedirectURL is the callback address to register in the Google Cloud console.
func (c Config) GoogleRedirectURL() string { return c.PublicURL + "/auth/google/callback" }

func cleanSubdir(v string) (string, error) {
	s := path.Clean(filepath.ToSlash(v))
	if s == "." || s == "/" || path.IsAbs(s) || s == ".." || strings.HasPrefix(s, "../") {
		return "", fmt.Errorf("MP_UPLOAD_SUBDIR %q must be a relative folder inside the music directory", v)
	}
	for _, seg := range strings.Split(s, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("MP_UPLOAD_SUBDIR %q must not contain hidden folders", v)
		}
	}
	return s, nil
}

// CacheDir is where transcoded audio is cached.
func (c Config) CacheDir() string { return filepath.Join(c.DataDir, "cache") }

// StatePath is the JSON file holding favorites, settings and resume state.
func (c Config) StatePath() string { return filepath.Join(c.DataDir, "state.json") }

// SessionsPath holds the signed-in sessions (token digests only), so a restart keeps them.
func (c Config) SessionsPath() string { return filepath.Join(c.DataDir, "sessions.json") }

// LogPath is the structured error log.
func (c Config) LogPath() string { return filepath.Join(c.DataDir, "error.log") }
