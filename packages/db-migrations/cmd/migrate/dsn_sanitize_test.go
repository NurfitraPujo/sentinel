package main

import "testing"

func TestSanitizeDSNRedactsBothForms(t *testing.T) {
	cases := []struct{ name, in, mustNotContain string }{
		{"url form", "postgres://sentinel:changeme@postgres:5432/sentinel?sslmode=disable", "changeme"},
		{"keyword form", "host=db user=sentinel password=changeme dbname=sentinel", "changeme"},
		{"url with special chars", "postgres://u:p%40ss%3Aword@h:5432/d", "p%40ss%3Aword"},
	}
	for _, c := range cases {
		got := sanitizeDSN(c.in)
		if contains(got, c.mustNotContain) {
			t.Errorf("%s: sanitizeDSN(%q) = %q — still leaks %q", c.name, c.in, got, c.mustNotContain)
		}
		t.Logf("%s -> %s", c.name, got)
	}
	// A DSN with no credential at all must survive intact, or error messages become useless.
	if got := sanitizeDSN("postgres://localhost:5432/sentinel"); got != "postgres://localhost:5432/sentinel" {
		t.Errorf("credential-free DSN was altered: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
