package gitprovider

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestGitCredential_RedactsOnAllFormatVerbs is the git-security finding-2 test: %v, %+v and %#v
// of a GitCredential (and a bare slog attribute holding one) must never print the plaintext
// username/password, mirroring settings.CredentialSecret's existing discipline. GitCredential's
// fields are unexported so the ONLY way this test can construct a non-empty one is through the
// package's own exported constructors.
func TestGitCredential_RedactsOnAllFormatVerbs(t *testing.T) {
	const secretToken = "ghp_shouldNeverAppearInAnyLog"
	cred := GitHubTokenCredential(secretToken)

	cases := map[string]string{
		"%v":  fmt.Sprintf("%v", cred),
		"%+v": fmt.Sprintf("%+v", cred),
		"%#v": fmt.Sprintf("%#v", cred),
		"%s":  fmt.Sprintf("%s", cred),
	}
	for verb, got := range cases {
		if strings.Contains(got, secretToken) {
			t.Fatalf("SECURITY: %s of a GitCredential leaked the token: %q", verb, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Fatalf("%s of a GitCredential should read as redacted, got: %q", verb, got)
		}
	}

	// slog.LogValuer: a bare attribute holding the credential must also redact.
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("git op", "cred", cred)
	if strings.Contains(buf.String(), secretToken) {
		t.Fatalf("SECURITY: slog output leaked the token: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "REDACTED") {
		t.Fatalf("slog output should show the credential as redacted, got: %q", buf.String())
	}
}
