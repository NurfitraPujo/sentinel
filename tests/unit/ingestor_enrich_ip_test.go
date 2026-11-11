package unit

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// sentinel-event-enrichment spec §3.1, §3.2 and §3.9 (§10.2 TASK-01 "{{auto}} / client-platform rules"):
// the IP rule enrich.Apply runs per item, over the platforms Sentinel receives.

type enrPlatform struct {
	name     string
	platform string
	runtime  any // metadata.runtime; nil = absent
	client   bool
	tunnel   bool // I3t: javascript + runtime "browser"
}

var enrPlatforms = []enrPlatform{
	{"javascript+browser", "javascript", "browser", true, true},
	{"javascript, no runtime", "javascript", nil, true, false},
	{"javascript+node", "javascript", "node", false, false},
	{"javascript+edge", "javascript", "edge", false, false},
	{"go", "go", nil, false, false},
	{"cocoa", "cocoa", nil, true, false},
	{"dart", "dart", nil, true, false},
}

func enrPayload(pl enrPlatform, user *validation.UserInfo) *validation.ErrorPayload {
	p := &validation.ErrorPayload{Platform: pl.platform, User: user}
	if pl.runtime != nil {
		p.Metadata = map[string]interface{}{"runtime": pl.runtime}
	}
	return p
}

var (
	enrPublicCaller = clientip.Info{ClientIP: netip.MustParseAddr("198.51.100.23"), HasClientIP: true}
	enrNoCaller     = clientip.Info{}
)

func TestEnrichIPRules(t *testing.T) {
	for _, pl := range enrPlatforms {
		t.Run(pl.name, func(t *testing.T) {
			// I1: {{auto}} with a public caller is the caller, on every platform (I3t does not apply).
			p := enrPayload(pl, &validation.UserInfo{IPAddress: "{{auto}}", ID: "u1"})
			enrich.Apply(p, enrPublicCaller, nil)
			assert.Equal(t, "198.51.100.23", p.User.IPAddress, "I1")
			assert.Equal(t, "u1", p.User.ID)

			// I2: {{auto}} with no public caller is "", never the literal.
			p = enrPayload(pl, &validation.UserInfo{IPAddress: "{{auto}}"})
			enrich.Apply(p, enrNoCaller, nil)
			assert.Equal(t, "", p.User.IPAddress, "I2")

			// I3 / I3t / I5: absent IP (user null, and the key empty) with a public caller.
			for _, u := range []*validation.UserInfo{nil, {ID: "u1"}} {
				p = enrPayload(pl, u)
				enrich.Apply(p, enrPublicCaller, nil)
				switch {
				case pl.client && !pl.tunnel:
					require.NotNil(t, p.User, "I3: user is created")
					assert.Equal(t, "198.51.100.23", p.User.IPAddress, "I3")
				case pl.tunnel:
					if u == nil {
						assert.Nil(t, p.User, "I3t: a tunnel-only item gets no connection-derived IP")
					} else {
						assert.Equal(t, "", p.User.IPAddress, "I3t")
					}
				default:
					if u == nil {
						assert.Nil(t, p.User, "I5: a server event is unchanged")
					} else {
						assert.Equal(t, "", p.User.IPAddress, "I5")
					}
				}
			}

			// I4: absent IP, no caller: unchanged.
			p = enrPayload(pl, nil)
			enrich.Apply(p, enrNoCaller, nil)
			assert.Nil(t, p.User, "I4")

			// I6: any other value is byte-equal, public or not, valid or not, whatever the caller.
			for _, v := range []string{"10.1.2.3", "2001:db8::1", "not-an-ip", "{{AUTO}}", " {{auto}}"} {
				p = enrPayload(pl, &validation.UserInfo{IPAddress: v})
				enrich.Apply(p, enrPublicCaller, nil)
				assert.Equal(t, v, p.User.IPAddress, "I6: the body wins")
			}
		})
	}
}

// TestEnrichIPClientPlatforms pins §3.1's predicate, including the runtime split and non-string runtimes.
func TestEnrichIPClientPlatforms(t *testing.T) {
	for _, c := range []struct {
		platform string
		runtime  any
		want     bool
	}{
		{"javascript", "browser", true}, {"javascript", nil, true}, {"javascript", "deno", true},
		{"javascript", "node", false}, {"javascript", "edge", false}, {"javascript", 7, true},
		{"browser", "node", true}, {"cocoa", nil, true}, {"objc", nil, true}, {"dart", nil, true},
		{"flutter", nil, true}, {"go", nil, false}, {"java", nil, false}, {"python", nil, false}, {"", nil, false},
	} {
		p := &validation.ErrorPayload{Platform: c.platform}
		if c.runtime != nil {
			p.Metadata = map[string]interface{}{"runtime": c.runtime}
		}
		assert.Equalf(t, c.want, enrich.IsClientPlatform(p), "%s runtime=%v", c.platform, c.runtime)
	}
}

// TestEnrichIPPrivateBodyValueUntouched is the §3.2 I6 / §3.5 "never to a body value" case on its own:
// a private body IP is stored as sent even though a derived one would be refused.
func TestEnrichIPPrivateBodyValueUntouched(t *testing.T) {
	p := &validation.ErrorPayload{Platform: "javascript", User: &validation.UserInfo{IPAddress: "10.1.2.3"}}
	enrich.Apply(p, enrPublicCaller, nil)
	assert.Equal(t, "10.1.2.3", p.User.IPAddress)
}
