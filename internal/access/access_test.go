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

// tailscaled fakes the daemon: status names this tailnet's suffix, whois answers with node.
func tailscaled(t *testing.T, suffix string, node map[string]any) (sock string, whoisAddr *string) {
	t.Helper()
	var addr string
	sock = fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "local-tailscaled.sock" {
			t.Errorf("host %q", r.Host)
		}
		switch r.URL.Path {
		case "/localapi/v0/status":
			json.NewEncoder(w).Encode(map[string]any{"MagicDNSSuffix": suffix})
		case "/localapi/v0/whois":
			addr = r.URL.Query().Get("addr")
			json.NewEncoder(w).Encode(map[string]any{"Node": node})
		default:
			http.NotFound(w, r)
		}
	})
	return sock, &addr
}

func TestTailscaleWhoisNamesYourOwnDevicesByMagicDNSName(t *testing.T) {
	sock, addr := tailscaled(t, "tail0303c3.ts.net", map[string]any{
		"Name": "Minisforum.tail0303c3.ts.net.", "ComputedName": "4090",
		"Hostinfo": map[string]any{"Hostname": "4090"}, // self-reported: must not count
	})
	names, err := TailscaleWhois(sock)(context.Background(), "100.89.219.7:5050")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "minisforum,minisforum.tail0303c3.ts.net" {
		t.Errorf("names: %v", names)
	}
	if *addr != "100.89.219.7:5050" {
		t.Errorf("whois addr %q", *addr)
	}
	// And it works end to end through a Policy.
	p, _ := policy(t, "tailscale", []string{"minisforum"}, TailscaleWhois(sock))
	if ok, why := allowed(p, "100.89.219.7:5050"); !ok {
		t.Errorf("%s", why)
	}
	// A device calling itself "4090" is still minisforum.
	p2, _ := policy(t, "tailscale", []string{"4090"}, TailscaleWhois(sock))
	if ok, _ := allowed(p2, "100.89.219.7:5050"); ok {
		t.Error("the self-reported hostname must not open the door")
	}
}

func TestTailscaleWhoisSharedInDevicesOnlyMatchTheirFullName(t *testing.T) {
	sock, _ := tailscaled(t, "tail0303c3.ts.net.", map[string]any{"Name": "minisforum.strangers-net.ts.net."})
	names, err := TailscaleWhois(sock)(context.Background(), "100.99.99.99:1")
	if err != nil || strings.Join(names, ",") != "minisforum.strangers-net.ts.net" {
		t.Fatalf("%v %v", names, err)
	}
	p, _ := policy(t, "tailscale", []string{"minisforum"}, TailscaleWhois(sock))
	if ok, _ := allowed(p, "100.99.99.99:1"); ok {
		t.Error("someone else's minisforum is not yours")
	}
	// More labels in front of your suffix is not a device name of yours either.
	sock2, _ := tailscaled(t, "tail0303c3.ts.net", map[string]any{"Name": "a.b.tail0303c3.ts.net"})
	if names, _ := TailscaleWhois(sock2)(context.Background(), "100.1.2.3:1"); strings.Join(names, ",") != "a.b.tail0303c3.ts.net" {
		t.Errorf("%v", names)
	}
}

func TestTailscaleWhoisReadsTheSuffixOnceAndFallsBack(t *testing.T) {
	statusCalls := 0
	sock := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/localapi/v0/status" {
			statusCalls++
			w.Write([]byte(`{"MagicDNSSuffix":"","CurrentTailnet":{"MagicDNSSuffix":"Tail0303c3.ts.net"}}`))
			return
		}
		w.Write([]byte(`{"Node":{"Name":"phone.tail0303c3.ts.net."}}`))
	})
	who := TailscaleWhois(sock)
	for i := 0; i < 3; i++ {
		if names, err := who(context.Background(), "100.1.2.3:1"); err != nil || names[0] != "phone" {
			t.Fatalf("%v %v", names, err)
		}
	}
	if statusCalls != 1 {
		t.Errorf("the suffix is asked for once: %d", statusCalls)
	}
}

func TestTailscaleWhoisErrors(t *testing.T) {
	fails := func(name, sock, want string) {
		t.Helper()
		if _, err := TailscaleWhois(sock)(context.Background(), "100.1.2.3:1"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	notFound := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no match", 404) })
	fails("non-200", notFound, "404")
	garbage := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) })
	fails("bad JSON", garbage, "unreadable")
	noSuffix, _ := tailscaled(t, "", map[string]any{"Name": "x.y.ts.net"})
	fails("no MagicDNS", noSuffix, "which tailnet")
	noName, _ := tailscaled(t, "tail0303c3.ts.net", map[string]any{"Name": " "})
	fails("no name", noName, "no name")
	whoisDown := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/localapi/v0/status" {
			w.Write([]byte(`{"MagicDNSSuffix":"t.ts.net"}`))
			return
		}
		http.Error(w, "boom", 500)
	})
	fails("whois fails", whoisDown, "500")
	if _, err := TailscaleWhois(filepath.Join(t.TempDir(), "missing.sock"))(context.Background(), "100.1.2.3:1"); err == nil {
		t.Error("no socket")
	}
	// A request that cannot even be built (an invalid context) is reported too.
	//lint:ignore SA1012 deliberately passing a nil context
	if _, err := TailscaleWhois(notFound)(nil, "100.1.2.3:1"); err == nil { //nolint:staticcheck
		t.Error("nil context")
	}
}

// ---------------------------------------------------------------- behind a proxy or tunnel

func tunnelPolicy(t *testing.T, proxies, visitors string) (*Policy, *[]string) {
	t.Helper()
	p, logs := policy(t, "tailscale,lan", nil, nil)
	pn, err := ParseNets(proxies)
	if err != nil {
		t.Fatal(err)
	}
	vn, err := ParseNets(visitors)
	if err != nil {
		t.Fatal(err)
	}
	p.TrustProxies(pn, vn)
	return p, logs
}

func request(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Add(k, v)
	}
	return r
}

func TestForwarded(t *testing.T) {
	for _, k := range []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		if !Forwarded(http.Header{http.CanonicalHeaderKey(k): {"203.0.113.7"}}) {
			t.Errorf("%s marks a proxied request", k)
		}
	}
	if Forwarded(http.Header{"User-Agent": {"x"}}) {
		t.Error("a plain request is not proxied")
	}
}

func TestTunnelVisitorsAreJudgedByTheirOwnAddress(t *testing.T) {
	// cloudflared on this host: every visitor arrives as loopback (or the Docker gateway).
	p, _ := tunnelPolicy(t, "loopback,lan", "tailscale,lan,198.51.100.0/24")
	cases := []struct {
		remote  string
		headers map[string]string
		want    bool
		who     string
	}{
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.7"}, false, "203.0.113.7"}, // the internet, via the tunnel
		{"172.17.0.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.7"}, false, "203.0.113.7"}, // via the Docker gateway
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "198.51.100.20"}, true, "198.51.100.20"},
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "2001:db8::1"}, false, "2001:db8::1"},
		// The last X-Forwarded-For entry is the one the proxy added; earlier ones are the visitor's own claims.
		{"127.0.0.1:5000", map[string]string{"X-Forwarded-For": "192.168.1.5, 203.0.113.7"}, false, "203.0.113.7"},
		{"127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.7, 198.51.100.9"}, true, "198.51.100.9"},
		{"127.0.0.1:5000", map[string]string{"X-Real-IP": "198.51.100.9"}, true, "198.51.100.9"},
		// Fail closed when the proxy says nothing readable about the visitor.
		{"127.0.0.1:5000", map[string]string{"Forwarded": "for=unknown"}, false, "127.0.0.1:5000"},
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "garbage"}, false, "127.0.0.1:5000"},
		// No forwarding header: a local health check or local use, judged as before.
		{"127.0.0.1:5000", nil, true, "127.0.0.1:5000"},
		// Headers from an address that is not a trusted proxy are ignored, as before.
		{"203.0.113.7:1", map[string]string{"CF-Connecting-IP": "192.168.1.5"}, false, "203.0.113.7:1"},
		{"100.114.200.30:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"}, true, "100.114.200.30:1"},
	}
	for _, c := range cases {
		ok, why, who := p.Check(request(c.remote, c.headers))
		if ok != c.want || who != c.who {
			t.Errorf("%s %v: ok=%v who=%q (%s), want %v %q", c.remote, c.headers, ok, who, why, c.want, c.who)
		}
	}
	if _, why, _ := p.Check(request("127.0.0.1:1", map[string]string{"X-Real-IP": "203.0.113.7"})); !strings.Contains(why, "tunnel networks") {
		t.Errorf("why: %q", why)
	}
	if _, why, _ := p.Check(request("127.0.0.1:1", map[string]string{"Forwarded": "for=x"})); !strings.Contains(why, "readable") {
		t.Errorf("why: %q", why)
	}
}

func TestTunnelVisitorsDefaultToAnywhere(t *testing.T) {
	// The default (MP_TUNNEL_NETS=any) keeps a Google-sign-in tunnel reachable from anywhere.
	p, _ := tunnelPolicy(t, "loopback,lan", "any")
	if ok, why, _ := p.Check(request("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})); !ok {
		t.Error(why)
	}
}

func TestWithoutTrustedProxiesHeadersAreNeverRead(t *testing.T) {
	p, _ := policy(t, "tailscale,lan", nil, nil)
	if ok, _, who := p.Check(request("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})); !ok || who != "127.0.0.1:1" {
		t.Errorf("no proxies configured: judged by the peer address (%v %s)", ok, who)
	}
}

func TestMiddlewareLogsTheTunnelVisitor(t *testing.T) {
	p, logs := tunnelPolicy(t, "loopback", "tailscale")
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"}))
	if rec.Code != 403 {
		t.Errorf("code %d", rec.Code)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "203.0.113.7") {
		t.Errorf("the refusal names the visitor, not the tunnel: %v", *logs)
	}
	if d := p.Describe(); !strings.Contains(d, "proxy or tunnel") {
		t.Errorf("%q", d)
	}
}

func TestClientKey(t *testing.T) {
	p, _ := tunnelPolicy(t, "loopback", "any")
	var got string
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientKey(r) }))
	h.ServeHTTP(httptest.NewRecorder(), request("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"}))
	if got != "203.0.113.7" {
		t.Errorf("behind the tunnel the key is the visitor: %q", got)
	}
	h.ServeHTTP(httptest.NewRecorder(), request("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "2001:db8:1:2:aaaa::1"}))
	if got != "2001:db8:1:2::/64" {
		t.Errorf("IPv6 visitors are grouped by /64: %q", got)
	}
	h.ServeHTTP(httptest.NewRecorder(), request("[::ffff:192.168.1.5]:9", nil))
	if got != "192.168.1.5" {
		t.Errorf("peer: %q", got)
	}
	// Without the middleware (no access policy), the peer address.
	if k := ClientKey(request("192.168.1.9:1", nil)); k != "192.168.1.9" {
		t.Errorf("no middleware: %q", k)
	}
	if k := ClientKey(request("garbage", nil)); k != "unknown" {
		t.Errorf("unreadable: %q", k)
	}
}
