// Package enrich fills what an SDK left empty, the way Sentry's server does (sentinel-event-enrichment
// spec §3, §5, §6): user.ip_address from the caller's connection, user.geo from a local GeoLite2-City
// database, and contexts.browser / contexts.os from a User-Agent. A value the body carries always wins,
// except the literal "{{auto}}" IP. Nothing here does network I/O.
//
// Apply runs inside service.IngestService.IngestAt after the receipt-time stamp and BEFORE the bounds
// step (§7.1), so every derived value is truncated, dropped and marked exactly as a sent one would be.
package enrich

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"sync"

	lru "github.com/hashicorp/golang-lru"
	"github.com/ua-parser/uap-go/uaparser"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// AutoIP is the sentinel value asking the server to use the caller's IP (spec §3.2). Matched exactly:
// case-sensitive, no trimming.
const AutoIP = "{{auto}}"

// GeoLookup resolves a public address to a user.geo (spec §5.6). *geoip.Loader implements it; tests
// pass a stub. It must never do network I/O.
type GeoLookup interface {
	Lookup(netip.Addr) (validation.GeoInfo, bool)
}

// Enricher carries the in-process lookups Apply may use. A nil *Enricher, or a nil field, means that
// fill is off; the IP rule needs neither and always runs.
type Enricher struct {
	Geo GeoLookup
	// UA parses User-Agents into browser and OS families (spec §6.1): built once at startup with
	// uaparser.New(uaparser.WithCacheSize(1024)) and shared; it is safe for concurrent use.
	UA *uaparser.Parser

	// uaResults caches what a User-Agent resolved to, shared across requests, so the budget below can
	// tell a cache hit (free) from a regex parse (charged). uap-go keeps its own cache too.
	uaOnce    sync.Once
	uaResults *lru.Cache
}

// MaxUserAgentBytes: a longer (or empty) User-Agent is not parsed. Regex cost grows with length, and no
// real User-Agent is that long (spec §6.2). It stays at 1024, not lower, because spec §4.4 has the
// tunnel forward any 1-1024-byte browser User-Agent and §6.2/decision 15 fix this cap to match; the
// per-request cost is bounded by MaxUAParsesPerRequest instead.
const MaxUserAgentBytes = 1024

// MaxUAParsesPerRequest caps the User-Agents one ingest request may send through the regex parser
// (enr-code-ua-parse-cpu-amplification). An uncached parse of a long User-Agent costs milliseconds, so
// without a cap one 500-item batch of distinct User-Agents costs seconds of CPU. Each distinct
// User-Agent is resolved at most once per request, a cached one is free, and past the cap an item gets
// no browser/OS context: never an error, and the event is still accepted.
const MaxUAParsesPerRequest = 8

// Budget is one ingest request's User-Agent allowance. The HTTP handlers put a fresh one on each
// request's context (WithBudget) and every item of a batch shares it. Safe for concurrent use.
type Budget struct {
	mu   sync.Mutex
	left int
	seen map[string]uaResult // every User-Agent this request already resolved
}

// NewBudget returns an allowance of MaxUAParsesPerRequest uncached parses.
func NewBudget() *Budget {
	return &Budget{left: MaxUAParsesPerRequest, seen: map[string]uaResult{}}
}

type budgetKey struct{}

// WithBudget returns ctx carrying b.
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

// BudgetFromContext returns the request's Budget, or nil when there is none.
func BudgetFromContext(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}

// Apply enriches one payload in place from the request's Info (the zero Info when the request carried
// none: no caller IP, no connection User-Agent). Order: IP (§3.2), then geo, then User-Agent (§7.1),
// because geo reads the IP the first step produced. It is ApplyWithBudget with a budget of its own: one
// payload has at most one User-Agent, so a lone call is bounded anyway.
func Apply(p *validation.ErrorPayload, info clientip.Info, e *Enricher) {
	ApplyWithBudget(p, info, e, nil)
}

// ApplyWithBudget is Apply charging User-Agent parses to b, the request's shared Budget (nil: a fresh
// one for this payload alone).
func ApplyWithBudget(p *validation.ErrorPayload, info clientip.Info, e *Enricher, b *Budget) {
	applyIP(p, info)
	if e == nil {
		return
	}
	if e.Geo != nil {
		applyGeo(p, e.Geo)
	}
	if e.UA != nil {
		if b == nil {
			b = NewBudget()
		}
		applyUA(p, info, e, b)
	}
}

// applyGeo is spec §5.3: only when user.geo was not sent (a sent geo, even {}, is never touched), and
// only for an ip_address that parses exactly as stored and is public (§3.5: one definition of "not a
// real caller"). It applies to sent IPs and derived ones alike. A miss fills nothing.
func applyGeo(p *validation.ErrorPayload, geo GeoLookup) {
	if p.User == nil || p.User.Geo != nil || p.User.IPAddress == "" {
		return
	}
	a, err := netip.ParseAddr(p.User.IPAddress)
	if err != nil {
		return
	}
	a = a.Unmap().WithZone("")
	if !clientip.IsPublic(a) {
		return
	}
	if g, ok := geo.Lookup(a); ok {
		p.User.Geo = &g
	}
}

// IsClientPlatform is spec §3.1: platform is one of javascript, browser, cocoa, objc, dart, flutter,
// except javascript whose metadata.runtime is the JSON string "node" or "edge" (sdk-js sends
// "javascript" from every runtime, T8). A server platform's connection is a pod, never a user.
func IsClientPlatform(p *validation.ErrorPayload) bool {
	switch p.Platform {
	case "javascript":
		rt, _ := p.Metadata["runtime"].(string)
		return rt != "node" && rt != "edge"
	case "browser", "cocoa", "objc", "dart", "flutter":
		return true
	default:
		return false
	}
}

// isTunnelOnly is spec §3.9's I3t predicate: a javascript item whose metadata.runtime is the JSON string
// "browser". The browser build of sdk-js can post only to a same-origin tunnel and the ingestor serves no
// CORS, so such an item's connection is always a tunnel's server, never the browser.
func isTunnelOnly(p *validation.ErrorPayload) bool {
	rt, _ := p.Metadata["runtime"].(string)
	return p.Platform == "javascript" && rt == "browser"
}

// applyIP is spec §3.2, rows I1-I6 and I3t. info.HasClientIP is true only for a public caller (the
// resolver applies §3.5), so no infrastructure address is ever written here.
func applyIP(p *validation.ErrorPayload, info clientip.Info) {
	if p.User != nil && p.User.IPAddress == AutoIP {
		if info.HasClientIP {
			p.User.IPAddress = info.ClientIP.String() // I1
		} else {
			p.User.IPAddress = "" // I2: the literal is never stored
		}
		return
	}
	if p.User != nil && p.User.IPAddress != "" {
		return // I6: the body wins, byte for byte
	}
	if !info.HasClientIP || !IsClientPlatform(p) || isTunnelOnly(p) {
		return // I4, I5, I3t
	}
	if p.User == nil {
		p.User = &validation.UserInfo{}
	}
	p.User.IPAddress = info.ClientIP.String() // I3
}

// userAgent is spec §6.2's source: the body's request.headers first (any platform; the first key in
// lexical order that equals "User-Agent" case-insensitively), and only when that gives nothing, the
// ingest request's own User-Agent for a client-platform event. A server SDK's HTTP-client UA is never
// parsed as a browser.
func userAgent(p *validation.ErrorPayload, info clientip.Info) string {
	if p.Request != nil && len(p.Request.Headers) > 0 {
		keys := make([]string, 0, len(p.Request.Headers))
		for k := range p.Request.Headers {
			if strings.EqualFold(k, "User-Agent") {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			if v := p.Request.Headers[keys[0]]; v != "" {
				return v
			}
		}
	}
	if IsClientPlatform(p) {
		return info.UserAgent
	}
	return ""
}

// applyUA is spec §6.3: contexts.browser, and contexts.os (client-platform event) or contexts.client_os
// (any other event, Relay's rule, so a browser's OS is never recorded as a server's), each inserted
// only when that top-level key is absent, the family is known, and the result still fits the contexts
// bounds. A context the SDK sent, even {} or null, and every key inside it, is never touched.
func applyUA(p *validation.ErrorPayload, info clientip.Info, e *Enricher, b *Budget) {
	osKey := "client_os"
	if IsClientPlatform(p) {
		osKey = "os"
	}
	_, haveBrowser := p.Contexts["browser"]
	_, haveOS := p.Contexts[osKey]
	if haveBrowser && haveOS {
		return
	}
	ua := userAgent(p, info)
	if ua == "" || len(ua) > MaxUserAgentBytes {
		return
	}
	r, ok := e.resolveUA(ua, b)
	if !ok {
		return // over this request's budget: derive nothing
	}
	if !haveBrowser {
		insertContext(p, "browser", r.browser, r.browserVersion)
	}
	if !haveOS {
		insertContext(p, osKey, r.os, r.osVersion)
	}
}

// uaResult is what one User-Agent resolves to: both families and versions, the OS version already
// frozen (Relay's rule).
type uaResult struct {
	browser, browserVersion string
	os, osVersion           string
}

// resolveUA answers from this request's earlier answers, then from the shared cache, and only then
// parses, charging b. With b spent it reports false.
func (e *Enricher) resolveUA(ua string, b *Budget) (uaResult, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.seen[ua]; ok {
		return r, true
	}
	e.uaOnce.Do(func() {
		e.uaResults, _ = lru.New(uaResultCacheSize) // fails only for a size <= 0
	})
	if v, ok := e.uaResults.Get(ua); ok {
		r := v.(uaResult)
		b.seen[ua] = r
		return r, true
	}
	if b.left <= 0 {
		return uaResult{}, false
	}
	b.left--
	br := e.UA.ParseUserAgent(ua)
	o := e.UA.ParseOs(ua)
	r := uaResult{
		browser:        br.Family,
		browserVersion: uaVersion(br.Major, br.Minor, br.Patch),
		os:             o.Family,
		osVersion:      frozenOSVersion(o.Family, uaVersion(o.Major, o.Minor, o.Patch)),
	}
	e.uaResults.Add(ua, r)
	b.seen[ua] = r
	return r, true
}

// uaResultCacheSize matches the parser's own cache (spec §6.1).
const uaResultCacheSize = 1024

// insertContext writes {name, version?} under key when the family is known ("Other" means unknown,
// Relay's is_known) and the insert fits (validation.ContextFits). No "type" key: the contract fixes
// {name, version}.
func insertContext(p *validation.ErrorPayload, key, family, version string) {
	if family == "" || family == "Other" {
		return
	}
	ctx := map[string]interface{}{"name": family}
	if version != "" {
		ctx["version"] = version
	}
	if !validation.ContextFits(p, key, ctx) {
		return
	}
	if p.Contexts == nil {
		p.Contexts = map[string]map[string]interface{}{}
	}
	p.Contexts[key] = ctx
}

// uaVersion is Relay's get_version: major, then .minor if present, then .patch if minor is present.
// No major means no version.
func uaVersion(major, minor, patch string) string {
	if major == "" {
		return ""
	}
	v := major
	if minor != "" {
		v += "." + minor
		if patch != "" {
			v += "." + patch
		}
	}
	return v
}

// frozenOSVersion is Relay's frozen-version rule: browsers freeze the OS version they report, so
// Windows "10" (also 11) and macOS "10.15.7" (also every later release) are lower bounds.
func frozenOSVersion(family, version string) string {
	switch {
	case family == "Windows" && version == "10":
		return ">=10"
	case family == "Mac OS X" && version == "10.15.7":
		return ">=10.15.7"
	}
	return version
}
