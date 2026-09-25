package integration

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// sentinel-issue-tag-facets spec §3.2: the derivation rules K1-K3 and V1-V3, table-driven over the
// migration's issue_tag_pairs on a migrated database. The oracle every other proof uses calls the same
// function, so these rows are what prove DERIVATION; the check query proves bookkeeping.
func TestTagFacets_PairsDerivationRules(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	long1024 := strings.Repeat("a", 1024)
	long1025 := strings.Repeat("a", 1025)

	cases := []struct {
		name                 string
		tags, contexts, user string // "" is SQL NULL
		release, environment string
		want                 []string
	}{
		{
			name:     "K1 each derived key from its source",
			contexts: `{"os":{"name":"Android"},"client_os":{"name":"Linux"},"device":{"model":"SM-A115F"},"browser":{"name":"Chrome"}}`,
			user:     `{"id":"u-1"}`,
			release:  "1.2", environment: "production",
			want: []string{"browser.name=Chrome", "client_os.name=Linux", "device.model=SM-A115F", "environment=production",
				"os.name=Android", "release=1.2", "user.id=u-1"},
		},
		{
			name: "K2 a custom tag named like a derived key is not counted", tags: `{"release":"x","environment":"y","tenant":"acme"}`,
			release: "1.2", environment: "production",
			want: []string{"environment=production", "release=1.2", "tenant=acme"},
		},
		{
			name: "K3 an invalid custom key is not counted", tags: `{"a b":"v","ok:key.1-x_y":"v","":"v"}`,
			environment: "production",
			want:        []string{"environment=production", "ok:key.1-x_y=v"},
		},
		{
			name: "K3 a 33-character key is not counted", tags: `{"` + strings.Repeat("k", 33) + `":"v","` + strings.Repeat("k", 32) + `":"v"}`,
			environment: "production",
			want:        []string{"environment=production", strings.Repeat("k", 32) + "=v"},
		},
		{
			name: "V1 masked values are counted verbatim, user.id unmasked",
			tags: `{"card":"[REDACTED_PAN]","pw":"***REDACTED***","ssn":"XXX-XX-XXXX"}`, user: `{"id":"alice@example.com"}`,
			environment: "production",
			want: []string{"card=[REDACTED_PAN]", "environment=production", "pw=***REDACTED***", "ssn=XXX-XX-XXXX",
				"user.id=alice@example.com"},
		},
		{
			name:     "V2 empty, null, number, boolean and object values are skipped",
			tags:     `{"e":"","n":12,"b":true,"o":{"x":"y"},"z":null}`,
			contexts: `{"os":{"name":12},"client_os":{"name":null},"device":{"model":{"x":"y"}},"browser":{"name":""}}`,
			user:     `{"id":42}`,
			release:  "", environment: "production",
			want: []string{"environment=production"},
		},
		{
			name: "V2 tags that are not an object are skipped", tags: `["a","b"]`, user: `"u"`, environment: "staging",
			want: []string{"environment=staging"},
		},
		{
			name:     "V3 a 1024-byte value is counted, 1025 is not",
			contexts: `{"os":{"name":"` + long1024 + `"},"browser":{"name":"` + long1025 + `"}}`, environment: "production",
			want: []string{"environment=production", "os.name=" + long1024},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, err := env.Pool.Query(context.Background(),
				`SELECT key || '=' || value FROM issue_tag_pairs($1::jsonb, $2::jsonb, $3::jsonb, $4, $5)`,
				tfNullJSON(c.tags), tfNullJSON(c.contexts), tfNullJSON(c.user), c.release, c.environment)
			if err != nil {
				t.Fatalf("issue_tag_pairs: %v", err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, s)
			}
			sort.Strings(got)
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("pairs:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// The derived-key list and its display order live in ONE place, issue_tag_derived (spec §3.2, F1).
func TestTagFacets_DerivedKeyOrder(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	rows, err := env.Pool.Query(context.Background(),
		`SELECT ord::text || ':' || key FROM issue_tag_derived(NULL, NULL, NULL, NULL) ORDER BY ord`)
	if err != nil {
		t.Fatalf("issue_tag_derived: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	want := "1:release,2:environment,3:os.name,4:client_os.name,5:device.model,6:browser.name,7:user.id"
	if strings.Join(got, ",") != want {
		t.Fatalf("derived keys %q, want %q", strings.Join(got, ","), want)
	}
}
