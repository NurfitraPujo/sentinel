package unit

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ua-parser/uap-go/uaparser"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// sentinel-event-enrichment spec §6 / §10.2 TASK-04 "source and fill rules": User-Agent -> contexts.

const (
	uaChromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	uaSafariMac     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15"
	uaFirefoxLinux  = "Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0"
	uaUnknown       = "sentinel-probe"
)

var sharedUAParser = sync.OnceValue(func() *uaparser.Parser {
	p, err := uaparser.New(uaparser.WithCacheSize(1024))
	if err != nil {
		panic(err)
	}
	return p
})

func uaEnricher() *enrich.Enricher { return &enrich.Enricher{UA: sharedUAParser()} }

type ctxs = map[string]map[string]interface{}

func uaApply(p *validation.ErrorPayload, connUA string) *validation.ErrorPayload {
	enrich.Apply(p, clientip.Info{UserAgent: connUA}, uaEnricher())
	return p
}

func withHeaders(p *validation.ErrorPayload, h map[string]string) *validation.ErrorPayload {
	p.Request = &validation.RequestInfo{Headers: h}
	return p
}

func jsBrowser() *validation.ErrorPayload {
	return &validation.ErrorPayload{Platform: "javascript", Metadata: map[string]interface{}{"runtime": "browser"}}
}

func TestUAFillFromConnectionForClientPlatform(t *testing.T) {
	p := uaApply(jsBrowser(), uaChromeWindows)
	assert.Equal(t, ctxs{
		"browser": {"name": "Chrome", "version": "128.0.0"},
		"os":      {"name": "Windows", "version": ">=10"},
	}, p.Contexts, "tunnel-clean's UA (spec §10.2 TASK-04): Relay's version and frozen-OS rules, no type key")
}

func TestUAVersionRules(t *testing.T) {
	p := uaApply(jsBrowser(), uaSafariMac)
	assert.Equal(t, map[string]interface{}{"name": "Safari", "version": "17.5"}, p.Contexts["browser"], "no patch: major.minor")
	assert.Equal(t, map[string]interface{}{"name": "Mac OS X", "version": ">=10.15.7"}, p.Contexts["os"], "frozen macOS")

	p = uaApply(jsBrowser(), uaFirefoxLinux)
	assert.Equal(t, map[string]interface{}{"name": "Firefox", "version": "129.0"}, p.Contexts["browser"])
	assert.Equal(t, map[string]interface{}{"name": "Linux"}, p.Contexts["os"], "no major means no version key")
}

func TestUABodyHeaderWinsAndMatchesCaseInsensitively(t *testing.T) {
	// The body's request.headers wins over the connection UA, on any platform.
	p := uaApply(withHeaders(jsBrowser(), map[string]string{"user-agent": uaFirefoxLinux}), uaChromeWindows)
	assert.Equal(t, "Firefox", p.Contexts["browser"]["name"])

	// Several case variants: the first key in lexical order wins ("USER-AGENT" < "User-Agent" < "user-agent").
	p = uaApply(withHeaders(jsBrowser(), map[string]string{
		"user-agent": uaChromeWindows, "User-Agent": uaSafariMac, "USER-AGENT": uaFirefoxLinux,
	}), "")
	assert.Equal(t, "Firefox", p.Contexts["browser"]["name"])

	// An empty body value gives nothing, so the connection UA is used for a client-platform event.
	p = uaApply(withHeaders(jsBrowser(), map[string]string{"User-Agent": ""}), uaChromeWindows)
	assert.Equal(t, "Chrome", p.Contexts["browser"]["name"])
}

func TestUAConnectionIgnoredForServerPlatforms(t *testing.T) {
	for _, p := range []*validation.ErrorPayload{
		{Platform: "go"},
		{Platform: "javascript", Metadata: map[string]interface{}{"runtime": "node"}},
		{Platform: "javascript", Metadata: map[string]interface{}{"runtime": "edge"}},
	} {
		uaApply(p, uaChromeWindows)
		assert.Nilf(t, p.Contexts, "%s %v: a server SDK's HTTP-client UA is never parsed as a browser", p.Platform, p.Metadata)
	}
}

func TestUAServerEventWritesClientOS(t *testing.T) {
	p := uaApply(withHeaders(&validation.ErrorPayload{Platform: "go"}, map[string]string{"User-Agent": uaChromeWindows}), "")
	assert.Equal(t, ctxs{
		"browser":   {"name": "Chrome", "version": "128.0.0"},
		"client_os": {"name": "Windows", "version": ">=10"},
	}, p.Contexts, "Relay: client_os for a non-client platform, never os")

	// A server event's own os context stays; the end user's OS goes beside it.
	p = withHeaders(&validation.ErrorPayload{Platform: "go"}, map[string]string{"User-Agent": uaChromeWindows})
	p.Contexts = ctxs{"os": {"name": "Linux", "version": "6.1"}}
	uaApply(p, "")
	assert.Equal(t, map[string]interface{}{"name": "Linux", "version": "6.1"}, p.Contexts["os"])
	assert.Equal(t, "Windows", p.Contexts["client_os"]["name"])
}

func TestUASentContextsUntouched(t *testing.T) {
	for _, sent := range []ctxs{
		{"browser": {}, "os": {}},
		{"browser": nil, "os": nil},
		{"browser": {"version": "1"}, "os": {"name": "Plan 9"}},
	} {
		p := jsBrowser()
		p.Contexts = ctxs{}
		for k, v := range sent {
			p.Contexts[k] = v
		}
		uaApply(p, uaChromeWindows)
		assert.Equal(t, sent, p.Contexts, "a sent context, even {} or null, and every key inside it, is never touched")
	}

	// Only the absent one is filled.
	p := jsBrowser()
	p.Contexts = ctxs{"browser": {"name": "Custom"}}
	uaApply(p, uaChromeWindows)
	assert.Equal(t, map[string]interface{}{"name": "Custom"}, p.Contexts["browser"])
	assert.Equal(t, "Windows", p.Contexts["os"]["name"])
}

func TestUAOtherAndOversizedFillNothing(t *testing.T) {
	p := uaApply(jsBrowser(), uaUnknown)
	assert.Nil(t, p.Contexts, "family Other is unknown and fills nothing")

	long := uaChromeWindows + " " + strings.Repeat("x", 1025-len(uaChromeWindows)-1)
	require.Len(t, long, 1025)
	p = uaApply(jsBrowser(), long)
	assert.Nil(t, p.Contexts, "a 1025-byte UA is not parsed")

	exact := uaChromeWindows + " " + strings.Repeat("x", 1024-len(uaChromeWindows)-1)
	require.Len(t, exact, 1024)
	p = uaApply(jsBrowser(), exact)
	assert.Equal(t, "Chrome", p.Contexts["browser"]["name"], "1024 bytes is still parsed")
}

// TestUAFitRule: a derived context is inserted only if the contexts object still fits, so it can never
// make the bounds step evict a context the SDK sent.
func TestUAFitRule(t *testing.T) {
	// 32 sent contexts: nothing derived, and after the bounds step all 32 survive.
	p := jsBrowser()
	p.ProjectKey, p.Environment, p.ErrorClass = "proj", "test", "E"
	p.Contexts = ctxs{}
	for i := 0; i < validation.MaxContexts; i++ {
		p.Contexts[fmt.Sprintf("c%02d", i)] = map[string]interface{}{"v": i}
	}
	uaApply(p, uaChromeWindows)
	assert.Len(t, p.Contexts, validation.MaxContexts)
	assert.NotContains(t, p.Contexts, "browser")
	validation.ValidatePayload(p)
	assert.Len(t, p.Contexts, validation.MaxContexts, "no sent context evicted")

	// Near the byte cap: one sent context large enough that adding browser would cross 65536.
	p = jsBrowser()
	pad := strings.Repeat("a", validation.MaxContextsSize-40)
	p.Contexts = ctxs{"big": {"p": pad}}
	require.True(t, validation.ContextFits(p, "big", p.Contexts["big"]), "the sent context alone fits")
	uaApply(p, uaChromeWindows)
	assert.NotContains(t, p.Contexts, "browser", "the derived context does not fit")
	validation.ValidatePayload(p)
	assert.Contains(t, p.Contexts, "big", "the sent context is never evicted")
}

func TestContextFitsMeasuresLikeTheBoundsStep(t *testing.T) {
	p := &validation.ErrorPayload{}
	v := map[string]interface{}{"name": "Chrome"}
	assert.True(t, validation.ContextFits(p, "browser", v))
	p.Contexts = ctxs{}
	for i := 0; i < validation.MaxContexts-1; i++ {
		p.Contexts[fmt.Sprintf("c%02d", i)] = map[string]interface{}{}
	}
	assert.True(t, validation.ContextFits(p, "browser", v), "the 32nd fits")
	p.Contexts["c31"] = map[string]interface{}{}
	assert.False(t, validation.ContextFits(p, "browser", v), "a 33rd does not")
	assert.True(t, validation.ContextFits(p, "c31", v), "replacing an existing name does not add one")
}

// enr-code-ua-parse-cpu-amplification: one request's User-Agent cost is bounded. Items of a batch share
// one enrich.Budget; each distinct User-Agent is resolved at most once, a cached one is free, and past
// enrich.MaxUAParsesPerRequest uncached parses an item simply gets no browser/OS context.
func uaDistinct(tag string, i int) string {
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0 %s-%d", tag, i)
}

func uaBatch(e *enrich.Enricher, b *enrich.Budget, uas []string) (enriched int) {
	for _, ua := range uas {
		p := withHeaders(&validation.ErrorPayload{Platform: "go"}, map[string]string{"User-Agent": ua})
		enrich.ApplyWithBudget(p, clientip.Info{}, e, b)
		if _, ok := p.Contexts["browser"]; ok {
			enriched++
		}
	}
	return enriched
}

func TestUABudgetCapsUncachedParsesPerRequest(t *testing.T) {
	require.Equal(t, 8, enrich.MaxUAParsesPerRequest)
	e := uaEnricher()
	uas := make([]string, 500)
	for i := range uas {
		uas[i] = uaDistinct("cap", i)
	}
	assert.Equal(t, enrich.MaxUAParsesPerRequest, uaBatch(e, enrich.NewBudget(), uas),
		"500 distinct uncached User-Agents in one request: only the budget is parsed, the rest derive nothing")
}

func TestUABudgetParsesEachDistinctUserAgentOnce(t *testing.T) {
	e := uaEnricher()
	b := enrich.NewBudget()
	same := make([]string, 500)
	for i := range same {
		same[i] = uaDistinct("same", 0)
	}
	assert.Equal(t, 500, uaBatch(e, b, same), "one User-Agent 500 times is one parse, and every item is enriched")

	var fresh []string
	for i := 1; i <= enrich.MaxUAParsesPerRequest; i++ {
		fresh = append(fresh, uaDistinct("same", i))
	}
	assert.Equal(t, enrich.MaxUAParsesPerRequest-1, uaBatch(e, b, fresh),
		"the repeated User-Agent was charged once, so exactly budget-1 new ones still fit")
}

func TestUABudgetCachedUserAgentsAreFree(t *testing.T) {
	e := uaEnricher()
	var first []string
	for i := 0; i < enrich.MaxUAParsesPerRequest; i++ {
		first = append(first, uaDistinct("cached", i))
	}
	require.Equal(t, len(first), uaBatch(e, enrich.NewBudget(), first))

	// A later request: the same User-Agents are cache hits and cost nothing, so a full budget of new
	// ones still fits beside them.
	second := append([]string(nil), first...)
	for i := 0; i < enrich.MaxUAParsesPerRequest; i++ {
		second = append(second, uaDistinct("cached-new", i))
	}
	assert.Equal(t, len(second), uaBatch(e, enrich.NewBudget(), second))
}

func TestUABudgetFromContext(t *testing.T) {
	b := enrich.NewBudget()
	assert.Same(t, b, enrich.BudgetFromContext(enrich.WithBudget(t.Context(), b)))
	assert.Nil(t, enrich.BudgetFromContext(t.Context()))
}
