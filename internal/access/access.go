// Package access decides who may reach the app, in the spirit of animedb.haruhi.one and
// anime-db-stream: only your own devices, on your Tailscale network or at home.
//
// Two layers, both checked against the address the connection actually came from (never
// against headers such as X-Forwarded-For, which anyone can forge; see TrustProxies for the
// one exception, a tunnel or reverse proxy running on your own machine or network):
//
//  1. A network allowlist (MP_ALLOWED_NETS). The default, "tailscale,lan", allows Tailscale
//     addresses (100.64.0.0/10) and the private ranges of a home network, and refuses
//     everything else, so a router port-forward or a stray public address cannot expose the
//     player. Loopback is always allowed (health checks, local use).
//  2. Optionally, a device allowlist (MP_KNOWN_DEVICES). A Tailscale peer must then be one of
//     your named devices. The name is the device's MagicDNS name as the host's own tailscaled
//     knows it (its local API, over the unix socket), not the IP and not the hostname the
//     device claims, so another person's node (shared into your tailnet, or renamed to look
//     like yours) cannot get in. Peers on the home network are not Tailscale peers and are
//     allowed by layer 1.
package access

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Named groups usable in MP_ALLOWED_NETS.
var groups = map[string][]string{
	"tailscale": {"100.64.0.0/10", "fd7a:115c:a1e0::/48"},
	"lan":       {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "fc00::/7", "fe80::/10"},
	"loopback":  {"127.0.0.0/8", "::1/128"},
	"any":       {"0.0.0.0/0", "::/0"},
}

var tailscaleNets = mustPrefixes(groups["tailscale"])

func mustPrefixes(specs []string) []netip.Prefix {
	out := make([]netip.Prefix, len(specs))
	for i, s := range specs {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// ParseNets turns a comma or space separated list into prefixes. Each item is a group name
// (tailscale, lan, loopback, any), a CIDR (192.168.1.0/24) or a single address.
func ParseNets(spec string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if g, ok := groups[strings.ToLower(item)]; ok {
			out = append(out, mustPrefixes(g)...)
			continue
		}
		if strings.Contains(item, "/") {
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid network", item)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("%q is not a network, an address or one of tailscale, lan, loopback, any", item)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no networks given")
	}
	return out, nil
}

// ParseDevices splits a list of device names (comma, space or semicolon separated),
// lower-cased, blanks dropped.
func ParseDevices(spec string) []string {
	var out []string
	for _, d := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		out = append(out, strings.ToLower(d))
	}
	return out
}

// WhoisFunc names the device behind a Tailscale connection (remote is "ip:port").
type WhoisFunc func(ctx context.Context, remote string) (names []string, err error)

const (
	whoisCacheTTL = time.Minute
	logEvery      = 10 * time.Minute
)

type cached struct {
	names   []string
	expires time.Time
}

// Policy is the access decision. It is safe for concurrent use.
type Policy struct {
	nets    []netip.Prefix
	devices map[string]bool
	whois   WhoisFunc
	now     func() time.Time
	logf    func(format string, args ...any)

	// Set by TrustProxies: connections from proxies carry the visitor's address in a header,
	// and that address must be in visitors.
	proxies  []netip.Prefix
	visitors []netip.Prefix

	mu      sync.Mutex
	names   map[netip.Addr]cached
	lastLog map[netip.Addr]time.Time
}

// New builds a Policy. Loopback is always allowed. With devices set, whois must be non-nil
// to identify Tailscale peers (if it fails, those peers are refused).
func New(nets []netip.Prefix, devices []string, whois WhoisFunc, logf func(string, ...any)) *Policy {
	p := &Policy{
		nets:    append(append([]netip.Prefix{}, nets...), mustPrefixes(groups["loopback"])...),
		devices: map[string]bool{},
		whois:   whois,
		now:     time.Now,
		logf:    logf,
		names:   map[netip.Addr]cached{},
		lastLog: map[netip.Addr]time.Time{},
	}
	for _, d := range devices {
		p.devices[strings.ToLower(d)] = true
	}
	return p
}

// Describe summarises the policy for the startup log.
func (p *Policy) Describe() string {
	s := fmt.Sprintf("access: %d allowed network range(s) plus loopback", len(p.nets)-len(groups["loopback"]))
	if len(p.devices) > 0 {
		s += fmt.Sprintf("; Tailscale peers must be one of %d known device(s)", len(p.devices))
	}
	if len(p.proxies) > 0 {
		s += fmt.Sprintf("; visitors through a proxy or tunnel are checked by their own address against %d range(s)", len(p.visitors))
	}
	return s
}

// TrustProxies makes the policy proxy-aware. Behind a tunnel such as cloudflared every visitor
// arrives from the tunnel's own (local) address, so the network check alone would wave the
// whole internet through. A request that comes from one of proxies and carries a forwarding
// header is judged instead by the visitor address in that header, which must be in visitors.
// Headers from any other address are ignored, as before: only a proxy you run can vouch for a
// visitor. Call it before serving; with no proxies, forwarding headers are never read.
func (p *Policy) TrustProxies(proxies, visitors []netip.Prefix) {
	p.proxies = append([]netip.Prefix{}, proxies...)
	p.visitors = append([]netip.Prefix{}, visitors...)
}

// forwardHeaders are the headers a proxy uses to pass on the visitor's address.
var forwardHeaders = []string{"Cf-Connecting-Ip", "X-Forwarded-For", "X-Real-Ip", "Forwarded"}

// Forwarded reports whether a request says it came through a proxy or tunnel. It is only a
// claim (anyone can send these headers), so use it to refuse, never to allow.
func Forwarded(h http.Header) bool {
	for _, k := range forwardHeaders {
		if h.Get(k) != "" {
			return true
		}
	}
	return false
}

// visitor reads the visitor's address that a trusted proxy passed on: Cloudflare's
// CF-Connecting-IP, else the last X-Forwarded-For entry (the one the nearest proxy added),
// else X-Real-IP.
func visitor(h http.Header) (netip.Addr, bool) {
	if v := strings.TrimSpace(h.Get("Cf-Connecting-Ip")); v != "" {
		return parseRemote(v)
	}
	if xs := h.Values("X-Forwarded-For"); len(xs) > 0 {
		parts := strings.Split(xs[len(xs)-1], ",")
		return parseRemote(strings.TrimSpace(parts[len(parts)-1]))
	}
	return parseRemote(strings.TrimSpace(h.Get("X-Real-Ip")))
}

// Check decides a request: by its proxy-reported visitor when it comes through a trusted
// proxy, otherwise by Allowed. It also returns the address the decision was about, for the log.
func (p *Policy) Check(r *http.Request) (ok bool, why, who string) {
	if ip, readable := parseRemote(r.RemoteAddr); readable && contains(p.proxies, ip) && Forwarded(r.Header) {
		v, readable := visitor(r.Header)
		if !readable {
			return false, "proxied request without a readable visitor address", r.RemoteAddr
		}
		if !contains(p.visitors, v) {
			return false, "visitor through the proxy is outside the allowed tunnel networks", v.String()
		}
		return true, "", v.String()
	}
	ok, why = p.Allowed(r.Context(), r.RemoteAddr)
	return ok, why, r.RemoteAddr
}

func parseRemote(remote string) (netip.Addr, bool) {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // IPv6 zone
	}
	a, err := netip.ParseAddr(host)
	return a.Unmap(), err == nil
}

func contains(nets []netip.Prefix, a netip.Addr) bool {
	for _, n := range nets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// Allowed reports whether a connection from remote ("ip:port") may proceed, and why not.
func (p *Policy) Allowed(ctx context.Context, remote string) (bool, string) {
	ip, ok := parseRemote(remote)
	if !ok {
		return false, "unreadable address"
	}
	if !contains(p.nets, ip) {
		return false, "outside the allowed networks"
	}
	if len(p.devices) > 0 && contains(tailscaleNets, ip) {
		names, err := p.identify(ctx, ip, remote)
		if err != nil {
			return false, "could not identify the Tailscale device: " + err.Error()
		}
		for _, n := range names {
			if p.devices[n] {
				return true, ""
			}
		}
		return false, fmt.Sprintf("Tailscale device %v is not in the known devices", names)
	}
	return true, ""
}

// identify names the Tailscale peer, remembering the answer for a minute.
func (p *Policy) identify(ctx context.Context, ip netip.Addr, remote string) ([]string, error) {
	p.mu.Lock()
	c, ok := p.names[ip]
	p.mu.Unlock()
	if ok && p.now().Before(c.expires) {
		return c.names, nil
	}
	if p.whois == nil {
		return nil, fmt.Errorf("no tailscaled connection configured")
	}
	names, err := p.whois(ctx, remote)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.names[ip] = cached{names: names, expires: p.now().Add(whoisCacheTTL)}
	p.mu.Unlock()
	return names, nil
}

// Middleware refuses connections that are not allowed with a plain 403. Allowed requests
// carry the address they were judged by (the tunnel visitor, or the peer), for ClientKey.
func (p *Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, why, who := p.Check(r)
		if !ok {
			p.logRefusal(who, why)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		a, _ := parseRemote(who) // always readable once allowed
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), visitorKey{}, a)))
	})
}

type visitorKey struct{}

// ClientKey names who is at the other end of r, for rate limits: the visitor Middleware
// judged (behind a tunnel, the real visitor rather than the tunnel), else the peer address.
// IPv6 visitors are grouped by /64, since one household or phone usually holds a whole /64.
func ClientKey(r *http.Request) string {
	a, ok := r.Context().Value(visitorKey{}).(netip.Addr)
	if !ok {
		if a, ok = parseRemote(r.RemoteAddr); !ok {
			return "unknown"
		}
	}
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

// logRefusal logs a refusal at most once per address every ten minutes, so a scanner cannot
// flood the log.
func (p *Policy) logRefusal(remote, why string) {
	ip, _ := parseRemote(remote)
	p.mu.Lock()
	last, seen := p.lastLog[ip]
	due := !seen || p.now().Sub(last) >= logEvery
	if due {
		p.lastLog[ip] = p.now()
	}
	p.mu.Unlock()
	if due {
		p.logf("access: refused %s (%s)", remote, why)
	}
}

// TailscaleWhois asks the host's tailscaled which device owns a connection, through its local
// API on the unix socket (bind-mount /var/run/tailscale/tailscaled.sock into the container).
//
// Only the device's MagicDNS name counts, never the hostname the device reports about itself
// (anyone can rename their own machine "minisforum"). A device of your own tailnet answers to
// its short name ("minisforum") and its full name ("minisforum.tail0303c3.ts.net"); a device
// shared in from another tailnet only to its full name, so it can never pass for one of yours.
func TailscaleWhois(socket string) WhoisFunc {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
	get := func(ctx context.Context, path string, v any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock"+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("tailscaled answered %s", resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return fmt.Errorf("unreadable answer from tailscaled: %w", err)
		}
		return nil
	}

	var mu sync.Mutex
	var suffix string // this tailnet's MagicDNS suffix, once known
	ownSuffix := func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if suffix != "" {
			return suffix, nil
		}
		var st struct {
			MagicDNSSuffix string
			CurrentTailnet struct{ MagicDNSSuffix string }
		}
		if err := get(ctx, "/localapi/v0/status?peers=false", &st); err != nil {
			return "", err
		}
		s := st.MagicDNSSuffix
		if s == "" {
			s = st.CurrentTailnet.MagicDNSSuffix
		}
		s = strings.ToLower(strings.Trim(s, "."))
		if s == "" {
			return "", fmt.Errorf("tailscaled did not say which tailnet this is (is MagicDNS on?)")
		}
		suffix = s
		return s, nil
	}

	return func(ctx context.Context, remote string) ([]string, error) {
		own, err := ownSuffix(ctx)
		if err != nil {
			return nil, err
		}
		var body struct{ Node struct{ Name string } }
		if err := get(ctx, "/localapi/v0/whois?addr="+url.QueryEscape(remote), &body); err != nil {
			return nil, err
		}
		full := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(body.Node.Name), "."))
		if full == "" {
			return nil, fmt.Errorf("tailscaled gave the device no name")
		}
		if short, ok := strings.CutSuffix(full, "."+own); ok && !strings.Contains(short, ".") {
			return []string{short, full}, nil
		}
		return []string{full}, nil
	}
}
