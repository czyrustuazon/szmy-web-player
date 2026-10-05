package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustNets(t *testing.T, spec string) []string {
	t.Helper()
	nets, err := ParseNets(spec)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range nets {
		out = append(out, n.String())
	}
	return out
}

func policy(t *testing.T, spec string, devices []string, whois WhoisFunc) (*Policy, *[]string) {
	t.Helper()
	nets, err := ParseNets(spec)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	p := New(nets, devices, whois, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	return p, &logs
}

func allowed(p *Policy, remote string) (bool, string) { return p.Allowed(context.Background(), remote) }

// ---------------------------------------------------------------- parsing

func TestParseNetsGroupsCIDRsAndAddresses(t *testing.T) {
	if got := mustNets(t, "tailscale"); len(got) != 2 || got[0] != "100.64.0.0/10" {
		t.Errorf("tailscale: %v", got)
	}
	lan := strings.Join(mustNets(t, "LAN"), " ") // group names are case-insensitive
	for _, want := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"} {
		if !strings.Contains(lan, want) {
			t.Errorf("lan misses %s: %s", want, lan)
		}
	}
	if got := mustNets(t, "any"); len(got) != 2 {
		t.Errorf("any: %v", got)
	}
	if got := mustNets(t, "loopback"); len(got) != 2 {
		t.Errorf("loopback: %v", got)
	}
	got := mustNets(t, "192.168.1.77/24, 203.0.113.9;  2001:db8::1 10.1.2.3")
	want := []string{"192.168.1.0/24", "203.0.113.9/32", "2001:db8::1/128", "10.1.2.3/32"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
	if got := mustNets(t, "::ffff:192.168.1.5"); got[0] != "192.168.1.5/32" {
		t.Errorf("an IPv4-mapped address is treated as IPv4: %v", got)
	}
}

func TestParseNetsRejectsNonsense(t *testing.T) {
	for _, spec := range []string{"", "   ", ",;", "wifi", "10.0.0.0/33", "300.1.1.1", "tailscale, banana"} {
		if _, err := ParseNets(spec); err == nil {
			t.Errorf("%q should be rejected", spec)
		}
	}
}

func TestParseDevices(t *testing.T) {
	got := ParseDevices(" MainServer, 4090;iPhone182  minisforum ,, ")
	if strings.Join(got, "|") != "mainserver|4090|iphone182|minisforum" {
		t.Errorf("got %v", got)
	}
	if ParseDevices("") != nil || ParseDevices(" , ") != nil {
		t.Error("nothing listed means no devices")
	}
}

// ---------------------------------------------------------------- the network layer

func TestDefaultPolicyAllowsTailnetAndHomeButNotThePublicInternet(t *testing.T) {
	p, _ := policy(t, "tailscale,lan", nil, nil)
	cases := map[string]bool{
		"100.114.200.30:5555":       true,  // Tailscale
		"100.64.0.1:1":              true,  // bottom of 100.64.0.0/10
		"100.127.255.254:1":         true,  // top of it
		"100.128.0.1:1":             false, // just outside (public)
		"100.63.255.255:1":          false,
		"192.168.1.20:4444":         true, // home
		"10.0.0.5:1":                true,
		"172.17.0.1:1":              true, // the Docker gateway (host-side connections)
		"172.32.0.1:1":              false,
		"169.254.1.1:1":             true,
		"[fd7a:115c:a1e0::1]:80":    true, // Tailscale IPv6
		"[fd12:3456::1]:80":         true, // IPv6 ULA (home)
		"[fe80::1%eth0]:80":         true, // link-local with a zone
		"[::ffff:192.168.1.5]:80":   true, // IPv4 seen as IPv6
		"[::ffff:203.0.113.7]:80":   false,
		"203.0.113.7:51199":         false, // public internet
		"8.8.8.8:1":                 false,
		"[2001:db8::1]:80":          false,
		"127.0.0.1:1":               true, // loopback is always allowed
		"[::1]:1":                   true,
		"192.168.1.20":              true, // no port at all
		"not-an-address":            false,
		"":                          false,
		"999.1.1.1:80":              false,
	}
	for remote, want := range cases {
		if got, why := allowed(p, remote); got != want {
			t.Errorf("%q: allowed=%v (%s), want %v", remote, got, why, want)
		}
	}
}

func TestTailnetOnlyAndCustomNetworks(t *testing.T) {
	p, _ := policy(t, "tailscale", nil, nil)
	if ok, _ := allowed(p, "192.168.1.20:1"); ok {
		t.Error("home network must be refused when only tailscale is allowed")
	}
	if ok, _ := allowed(p, "100.114.200.30:1"); !ok {
		t.Error("tailnet must be allowed")
	}
	if ok, _ := allowed(p, "127.0.0.1:1"); !ok {
		t.Error("loopback stays allowed (health checks)")
	}

	one, _ := policy(t, "192.168.50.0/24", nil, nil)
	if ok, _ := allowed(one, "192.168.50.9:1"); !ok {
		t.Error("the one configured subnet")
	}
	if ok, _ := allowed(one, "192.168.51.9:1"); ok {
		t.Error("another subnet")
	}
	every, _ := policy(t, "any", nil, nil)
	if ok, _ := allowed(every, "8.8.8.8:1"); !ok {
		t.Error("any allows everything")
	}
}

func TestRefusalReasons(t *testing.T) {
	p, _ := policy(t, "tailscale,lan", nil, nil)
	if _, why := allowed(p, "203.0.113.7:1"); !strings.Contains(why, "outside") {
		t.Errorf("why: %q", why)
	}
	if _, why := allowed(p, "garbage"); !strings.Contains(why, "unreadable") {
		t.Errorf("why: %q", why)
	}
}

// ---------------------------------------------------------------- the device layer

func TestKnownDevicesGateTailscalePeersOnly(t *testing.T) {
	calls := 0
	whois := func(_ context.Context, remote string) ([]string, error) {
		calls++
		switch {
		case strings.HasPrefix(remote, "100.89.219.7"):
			return []string{"minisforum", "minisforum-win"}, nil
		case strings.HasPrefix(remote, "100.99.99.99"):
			return []string{"strangers-laptop"}, nil
		}
		return nil, errors.New("unknown peer")
	}
	p, _ := policy(t, "tailscale,lan", []string{"MiniSforum", "4090"}, whois)

	if ok, why := allowed(p, "100.89.219.7:1234"); !ok {
		t.Errorf("a known device must pass (names are case-insensitive): %s", why)
	}
	ok, why := allowed(p, "100.99.99.99:1")
	if ok || !strings.Contains(why, "strangers-laptop") {
		t.Errorf("an unknown tailnet device (say, from a shared tailnet) must be refused: %v %s", ok, why)
	}
	// A device the daemon cannot identify is refused (fail closed).
	if ok, why := allowed(p, "100.100.100.100:1"); ok || !strings.Contains(why, "could not identify") {
		t.Errorf("unidentifiable: %v %s", ok, why)
	}
	// Home-network peers are not Tailscale peers: no identity check, the network rule applies.
	before := calls
	if ok, _ := allowed(p, "192.168.1.20:1"); !ok || calls != before {
		t.Errorf("LAN peers skip the device check (calls %d -> %d)", before, calls)
	}
	// And it still refuses addresses outside the networks, whatever they claim to be.
	if ok, _ := allowed(p, "203.0.113.7:1"); ok {
		t.Error("public address")
	}
}

func TestDeviceIdentitiesAreCachedBriefly(t *testing.T) {
	calls := 0
	whois := func(context.Context, string) ([]string, error) { calls++; return []string{"4090"}, nil }
	p, _ := policy(t, "tailscale", []string{"4090"}, whois)
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if ok, _ := allowed(p, "100.103.95.26:"+string(rune('1'+i))); !ok { // new source port each time
			t.Fatal("must be allowed")
		}
	}
	if calls != 1 {
		t.Errorf("a page load makes many requests: whois must be asked once, got %d", calls)
	}
	now = now.Add(2 * time.Minute)
	allowed(p, "100.103.95.26:9")
	if calls != 2 {
		t.Errorf("the answer expires after a minute: %d", calls)
	}
}

func TestKnownDevicesWithoutADaemonConnectionFailClosed(t *testing.T) {
	p, _ := policy(t, "tailscale,lan", []string{"4090"}, nil)
	ok, why := allowed(p, "100.103.95.26:1")
	if ok || !strings.Contains(why, "no tailscaled") {
		t.Errorf("%v %s", ok, why)
	}
	if ok, _ := allowed(p, "192.168.1.5:1"); !ok {
		t.Error("home peers do not need tailscaled")
	}
}

func TestDescribe(t *testing.T) {
	p, _ := policy(t, "tailscale,lan", nil, nil)
	if d := p.Describe(); !strings.Contains(d, "8 allowed network") || strings.Contains(d, "known device") {
		t.Errorf("%q", d)
	}
	p2, _ := policy(t, "tailscale", []string{"a", "b"}, nil)
	if d := p2.Describe(); !strings.Contains(d, "2 known device") {
		t.Errorf("%q", d)
	}
}

// ---------------------------------------------------------------- middleware

func TestMiddlewareRefusesWith403AndLogsOncePerAddress(t *testing.T) {
	p, logs := policy(t, "tailscale,lan", nil, nil)
	now := time.Unix(5_000_000, 0)
	p.now = func() time.Time { return now }
	var served int
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++; w.Write([]byte("hello")) }))

	do := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "100.114.200.30") // forged: must be ignored
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("100.114.200.30:1"); rec.Code != 200 || rec.Body.String() != "hello" {
		t.Errorf("allowed: %d", rec.Code)
	}
	for i := 0; i < 5; i++ {
		if rec := do("203.0.113.7:4000"); rec.Code != 403 || strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("refused: %d %q", rec.Code, rec.Body.String())
		}
	}
	if served != 1 {
		t.Errorf("a forged X-Forwarded-For must not open the door (served %d)", served)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "203.0.113.7") {
		t.Errorf("one log line for five refusals from the same address: %v", *logs)
	}
	do("198.51.100.1:1") // a different address is logged on its own
	if len(*logs) != 2 {
		t.Errorf("logs: %v", *logs)
	}
	now = now.Add(11 * time.Minute)
	do("203.0.113.7:4001")
	if len(*logs) != 3 {
		t.Errorf("logged again after ten minutes: %v", *logs)
	}
}

// ---------------------------------------------------------------- talking to tailscaled

func fakeTailscaled(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	sock := filepath.Join(t.TempDir(), "ts.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("cannot listen on a unix socket here:", err)
	}
	srv := &http.Server{Handler: handler}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.Serve(l) }()
	t.Cleanup(func() { srv.Close(); wg.Wait() })
	return sock
}

func TestTailscaleWhoisReadsTheDeviceNameFromTailscaled(t *testing.T) {
	var gotPath, gotAddr, gotHost string
	sock := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAddr, gotHost = r.URL.Path, r.URL.Query().Get("addr"), r.Host
		json.NewEncoder(w).Encode(map[string]any{"Node": map[string]any{
			"Name": "Minisforum.tail0303c3.ts.net.", "ComputedName": "MiniSForum",
			"Hostinfo": map[string]any{"Hostname": "MINISFORUM-PC"},
		}})
	})
	names, err := TailscaleWhois(sock)(context.Background(), "100.89.219.7:5050")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "minisforum,minisforum-pc,minisforum" {
		t.Errorf("names: %v", names)
	}
	if gotPath != "/localapi/v0/whois" || gotAddr != "100.89.219.7:5050" || gotHost != "local-tailscaled.sock" {
		t.Errorf("request: %s %s %s", gotPath, gotAddr, gotHost)
	}
	// And it works end to end through a Policy.
	p, _ := policy(t, "tailscale", []string{"minisforum"}, TailscaleWhois(sock))
	if ok, why := allowed(p, "100.89.219.7:5050"); !ok {
		t.Errorf("%s", why)
	}
}

func TestTailscaleWhoisSkipsMissingFields(t *testing.T) {
	sock := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Node":{"Name":"","ComputedName":"  ","Hostinfo":{"Hostname":"Phone"}}}`))
	})
	names, err := TailscaleWhois(sock)(context.Background(), "100.1.2.3:1")
	if err != nil || strings.Join(names, ",") != "phone" {
		t.Errorf("%v %v", names, err)
	}
}

func TestTailscaleWhoisErrors(t *testing.T) {
	notFound := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no match", 404) })
	if _, err := TailscaleWhois(notFound)(context.Background(), "100.1.2.3:1"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("non-200: %v", err)
	}
	garbage := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) })
	if _, err := TailscaleWhois(garbage)(context.Background(), "100.1.2.3:1"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("bad JSON: %v", err)
	}
	if _, err := TailscaleWhois(filepath.Join(t.TempDir(), "missing.sock"))(context.Background(), "100.1.2.3:1"); err == nil {
		t.Error("no socket")
	}
	// A request that cannot even be built (an invalid context) is reported too.
	//lint:ignore SA1012 deliberately passing a nil context
	if _, err := TailscaleWhois(notFound)(nil, "100.1.2.3:1"); err == nil { //nolint:staticcheck
		t.Error("nil context")
	}
}
