// Package clientip resolves the caller's IP address for one ingest request (sentinel-event-enrichment
// spec §3). It believes X-Forwarded-For only through a configured trusted-proxy CIDR list, walking the
// chain right to left from the connection's peer, and it returns an address only when that address is
// public (§3.5): a private, loopback, link-local, carrier-grade-NAT or shared-ELB address is never a
// derived caller IP. A request carrying the tunnel's no-derive marker gets no caller IP at all (§3.8).
//
// The result is computed once per HTTP request by Middleware and carried on the request context with the
// request's own User-Agent, through a private key type (CLAUDE.md: never a bare string context key).
package clientip

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// DeriveIPHeader is the no-derive marker (spec §3.8). The sdk-js tunnel sends it as `off` on every
// forward. Its presence alone, with any value including an empty one, means "derive no caller IP from
// this connection": the value is never parsed, so a typo can never turn derivation back on.
const DeriveIPHeader = "X-Sentinel-Derive-IP"

// maxHops is the walk's cost cap (spec §3.4 step 5): passing this many entries returns no result.
const maxHops = 32

// ParseTrusted parses TRUSTED_PROXY_CIDRS (spec §3.3): a comma-separated list of CIDR prefixes, where a
// bare address is a single-address prefix. Whitespace around entries and empty entries are ignored, so
// "" is the empty list (trust nothing). Every prefix is masked. An unparseable entry, or an IPv4-mapped
// IPv6 one, is an error whose text main logs verbatim before exiting 1: a typo in a trust boundary must
// never become silent.
func ParseTrusted(s string) ([]netip.Prefix, error) {
	return parseTrustedList("TRUSTED_PROXY_CIDRS", s)
}

// parseTrustedList is ParseTrusted for a list read from the environment variable envName, whose name prefixes
// every error so a typo in one list never exits naming another.
func parseTrustedList(envName, s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range strings.Split(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(entry, "/") {
			parsed, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a CIDR or IP address", envName, entry)
			}
			p = parsed
		} else {
			a, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a CIDR or IP address", envName, entry)
			}
			a = a.WithZone("")
			p = netip.PrefixFrom(a, a.BitLen())
		}
		// netip.Prefix.Contains never matches an IPv4 address against an IPv6 prefix, and every address
		// is unmapped before the check, so a mapped entry would trust nothing, in silence (§3.3).
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf("%s: %q is an IPv4-mapped IPv6 prefix; write it in IPv4 form", envName, entry)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Resolver applies the trusted-proxy walk (spec §3.4) and the public-caller rule (§3.5).
type Resolver struct {
	trusted []netip.Prefix
	// peerOnly, when peerScoped, is the prefix list PeerTrusted tests instead of trusted. The compat resolver
	// walks X-Forwarded-For over the union of both lists but believes the relay identity headers (which
	// PeerTrusted gates) only from SENTRY_COMPAT_TRUSTED_PROXY_CIDRS: the global list never grants identity.
	peerOnly   []netip.Prefix
	peerScoped bool
}

// NewResolver returns a Resolver over the given trusted prefixes (ParseTrusted's output). An empty list
// trusts nothing: the caller is always the connection's peer, which must still be public.
func NewResolver(trusted []netip.Prefix) *Resolver {
	return &Resolver{trusted: append([]netip.Prefix(nil), trusted...)}
}

// Resolvers parses TRUSTED_PROXY_CIDRS (globalSpec) and SENTRY_COMPAT_TRUSTED_PROXY_CIDRS (compatSpec) and returns
// the two resolvers main.go uses: ingest over the global prefixes ALONE (for /ingest and /ingest/batch, exactly
// today's resolver), compat over the global prefixes plus the compat ones for the X-Forwarded-For walk, whose
// PeerTrusted (the relay identity headers' gate) is the compat prefixes ALONE. Either spec invalid is an error that
// names the variable it came from, which main.go treats exactly as today's invalid TRUSTED_PROXY_CIDRS: log
// clientip.config_invalid and exit 1.
func Resolvers(globalSpec, compatSpec string) (ingest, compat *Resolver, err error) {
	global, err := parseTrustedList("TRUSTED_PROXY_CIDRS", globalSpec)
	if err != nil {
		return nil, nil, err
	}
	extra, err := parseTrustedList("SENTRY_COMPAT_TRUSTED_PROXY_CIDRS", compatSpec)
	if err != nil {
		return nil, nil, err
	}
	union := append(append([]netip.Prefix(nil), global...), extra...)
	compat = NewResolver(union)
	compat.peerOnly, compat.peerScoped = append([]netip.Prefix(nil), extra...), true
	return NewResolver(global), compat, nil
}

// PeerTrusted reports whether the connection's immediate peer (RemoteAddr) is inside one of the resolver's
// trusted prefixes (for the compat resolver: inside the compat-only prefixes, see Resolvers). An address inside a
// header never counts.
func (r *Resolver) PeerTrusted(req *http.Request) bool {
	peer, ok := parseHop(req.RemoteAddr)
	if !ok {
		return false
	}
	if r.peerScoped {
		return containsAddr(r.peerOnly, peer)
	}
	return r.isTrusted(peer)
}

// Trusted returns the configured prefixes, for the startup log line.
func (r *Resolver) Trusted() []netip.Prefix {
	return append([]netip.Prefix(nil), r.trusted...)
}

// Resolve returns the request's caller IP, and true, only when the walk finds one and it is public.
//
//   - A request carrying DeriveIPHeader gets no caller IP, before RemoteAddr or any header is read (§3.8).
//   - The walk starts at the connection's peer and moves left through X-Forwarded-For. The first entry
//     outside every trusted prefix is the caller; an entry inside one is our own proxy and is passed
//     over. An empty or unreadable entry, running off the left end, or passing maxHops entries gives no
//     result (§3.4).
//   - The returned address is unmapped and zone-free, so its String() is what the ingestor stores.
func (r *Resolver) Resolve(req *http.Request) (netip.Addr, bool) {
	if len(req.Header.Values(DeriveIPHeader)) > 0 {
		return netip.Addr{}, false
	}

	peer, ok := parseHop(req.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	seen := 1
	if !r.isTrusted(peer) {
		return publicOnly(peer)
	}

	// The peer is one of our proxies: only now is X-Forwarded-For read. All header lines, in order,
	// joined and split on commas (§3.4). No header at all splits to one empty entry, which stops the walk.
	entries := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(entries) - 1; i >= 0; i-- {
		seen++
		if seen > maxHops {
			return netip.Addr{}, false
		}
		hop, ok := parseHop(strings.Trim(entries[i], " \t"))
		if !ok {
			return netip.Addr{}, false
		}
		if !r.isTrusted(hop) {
			return publicOnly(hop)
		}
	}
	return netip.Addr{}, false
}

// RelayedClientIP is the caller address a relay-scoped key asserts: the rightmost X-Forwarded-For entry, which is
// the one core's relay sets (sentinel-relay-key-trust K3). It is used only if it parses and is public; nothing
// else is consulted, no network is trusted, and a request carrying DeriveIPHeader gets none (as in Resolve). The
// caller must have authenticated a relay key: this function does not check.
func RelayedClientIP(req *http.Request) (netip.Addr, bool) {
	if len(req.Header.Values(DeriveIPHeader)) > 0 {
		return netip.Addr{}, false
	}
	entries := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	hop, ok := parseHop(strings.Trim(entries[len(entries)-1], " \t"))
	if !ok {
		return netip.Addr{}, false
	}
	return publicOnly(hop)
}

func (r *Resolver) isTrusted(a netip.Addr) bool { return containsAddr(r.trusted, a) }

func containsAddr(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func publicOnly(a netip.Addr) (netip.Addr, bool) {
	if !IsPublic(a) {
		return netip.Addr{}, false
	}
	return a, true
}

// parseHop reads one chain entry: a bare address, or ip:port / [v6]:port. The result is unmapped and
// its zone dropped. An empty or unparseable entry is not a hop.
func parseHop(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		ap, perr := netip.ParseAddrPort(s)
		if perr != nil {
			return netip.Addr{}, false
		}
		a = ap.Addr()
	}
	return a.Unmap().WithZone(""), true
}

// notPublic is §3.5's fixed prefix list, subtracted after IsGlobalUnicast and IsPrivate. Every entry was
// measured to pass both of those on go1.26.4, which is why each must be explicit. N7 (Huawei's shared-ELB
// backend range) lies inside N6 and is listed on its own so that narrowing N6 can never re-admit it.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // N8 "this network" (RFC 1122 §3.2.1.3)
	netip.MustParsePrefix("100.64.0.0/10"),  // N6 carrier-grade NAT (RFC 6598)
	netip.MustParsePrefix("100.125.0.0/16"), // N7 Huawei shared-ELB backend range
	netip.MustParsePrefix("240.0.0.0/4"),    // N9 reserved (RFC 1112 §4)
	netip.MustParsePrefix("fec0::/10"),      // N10 deprecated site-local (RFC 3879)
	netip.MustParsePrefix("::/96"),          // N11 IPv4-compatible (RFC 4291 §2.5.5.1)
	netip.MustParsePrefix("64:ff9b::/96"),   // N11 NAT64 (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // N11 local-use NAT64 (RFC 8215)
	netip.MustParsePrefix("2002::/16"),      // N11 6to4 (RFC 3056)
}

// IsPublic is the one definition of "a real caller's address" (spec §3.5), shared by the walk and the geo
// lookup. The address is unmapped and zone-free first. IsGlobalUnicast already excludes invalid,
// unspecified, loopback, link-local, multicast and IPv4 broadcast (N1, N2, N4, N5); IsPrivate covers N3;
// notPublic covers N6-N11. Everything else, including the documentation ranges, is public.
func IsPublic(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Info is the per-request data enrichment reads (spec §7.2): the resolved caller IP, when there is a
// public one, and the ingest request's own User-Agent.
type Info struct {
	ClientIP    netip.Addr
	HasClientIP bool
	UserAgent   string
	// PeerTrusted is Resolver.PeerTrusted for this request. Nothing reads it to grant relay identity any more: that
	// is decided by the authenticated key's scope (sentinel-relay-key-trust §4.2). It stays as an observation.
	PeerTrusted bool
}

// requestInfoKey is private so no other package can read or forge the value by a string literal.
type requestInfoKey struct{}

// WithRequestInfo returns ctx carrying info.
func WithRequestInfo(ctx context.Context, info Info) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// RequestInfoFromContext returns the Info Middleware stored, and false when there is none (a unit test,
// or service.Ingest with no request): no caller IP and no connection User-Agent.
func RequestInfoFromContext(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(requestInfoKey{}).(Info)
	return info, ok
}

// Middleware resolves the caller once per HTTP request and stores it, with the request's User-Agent, on
// the context before next runs. It never rejects a request.
func Middleware(r *Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ip, ok := r.Resolve(req)
		info := Info{ClientIP: ip, HasClientIP: ok, UserAgent: req.UserAgent(), PeerTrusted: r.PeerTrusted(req)}
		next.ServeHTTP(w, req.WithContext(WithRequestInfo(req.Context(), info)))
	})
}
