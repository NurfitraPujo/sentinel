package sentinel

import (
	"os"
	"regexp"
	"testing"
)

// versionHeadingRE matches a CHANGELOG "## vX.Y.Z — ..." heading.
var versionHeadingRE = regexp.MustCompile(`(?m)^## v(\d+\.\d+\.\d+)\b`)

// TestChangelogMatchesSDKVersion is P4.1: the CHANGELOG's newest heading
// equals SDKVersion, and v0.3.1, v0.3.2 and v0.4.0 are all present (spec
// §7.1).
func TestChangelogMatchesSDKVersion(t *testing.T) {
	raw, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}

	matches := versionHeadingRE.FindStringSubmatch(string(raw))
	if matches == nil {
		t.Fatal("no version heading found in CHANGELOG.md")
	}
	if matches[1] != SDKVersion {
		t.Errorf("newest CHANGELOG heading = %q, want SDKVersion %q", matches[1], SDKVersion)
	}

	for _, want := range []string{"v0.3.1", "v0.3.2", "v0.4.0"} {
		if !regexp.MustCompile(`(?m)^## ` + regexp.QuoteMeta(want) + `\b`).Match(raw) {
			t.Errorf("CHANGELOG.md missing a heading for %s", want)
		}
	}
}
