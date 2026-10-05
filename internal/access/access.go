// Package access decides who may reach the app, in the spirit of animedb.haruhi.one and
// anime-db-stream: only your own devices, on your Tailscale network or at home.
//
// Two layers, both checked against the address the connection actually came from (never
// against headers such as X-Forwarded-For, which anyone can forge):
//
//  1. A network allowlist (MP_ALLOWED_NETS). The default, "tailscale,lan", allows Tailscale
//     addresses (100.64.0.0/10) and the private ranges of a home network, and refuses
//     everything else, so a router port-forward or a stray public address cannot expose the
//     player. Loopback is always allowed (health checks, local use).
//  2. Optionally, a device allowlist (MP_KNOWN_DEVICES). A Tailscale peer must then be one of
//     your named devices. The name comes from the host's own tailscaled (its local API, over
//     the unix socket), not from the IP, so another person's node in a shared tailnet cannot
//     get in. Peers on the home network are not Tailscale peers and are allowed by layer 1.
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
	return s
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

// Middleware refuses connections that are not allowed with a plain 403.
func (p *Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, why := p.Allowed(r.Context(), r.RemoteAddr); !ok {
			p.logRefusal(r.RemoteAddr, why)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
func TailscaleWhois(socket string) WhoisFunc {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
	return func(ctx context.Context, remote string) ([]string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://local-tailscaled.sock/localapi/v0/whois?addr="+url.QueryEscape(remote), nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tailscaled answered %s", resp.Status)
		}
		var body struct {
			Node struct {
				Name         string
				ComputedName string
				Hostinfo     struct{ Hostname string }
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("unreadable answer from tailscaled: %w", err)
		}
		first, _, _ := strings.Cut(strings.TrimSuffix(body.Node.Name, "."), ".")
		var names []string
		for _, n := range []string{body.Node.ComputedName, body.Node.Hostinfo.Hostname, first} {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				names = append(names, n)
			}
		}
		return names, nil
	}
}
