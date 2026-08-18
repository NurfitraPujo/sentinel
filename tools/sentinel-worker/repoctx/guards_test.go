package repoctx

import "testing"

// Direct unit coverage of the validation guards, independent of git's own behavior (git's "--"
// positional handling makes several of these defense-in-depth rather than the only backstop; these
// tests pin the guard's own behavior so a regression is caught even if git's behavior ever changes).

func TestValidateRef_RejectsBadInput(t *testing.T) {
	bad := []string{"-x", "--upload-pack=/bin/sh", "refs/heads/main; rm -rf /", "a b", "", "with space"}
	for _, r := range bad {
		if err := validateRef(r); err == nil {
			t.Errorf("validateRef(%q): expected error", r)
		}
	}
}

// TestValidateRef_RejectsDotsOnly pins the isDotsOnly guard added to validateRef: refPattern's
// charset admits ".", "..", "..." etc, and confinement must not rely on git itself rejecting a
// ".." revision downstream. Without the guard, ".." passes refPattern (it's 1-100 chars of the
// allowed charset and doesn't start with '-').
//
// MUTATION-TEST NOTE: temporarily changing `if isDotsOnly(ref)` to `if false && isDotsOnly(ref)`
// in validateRef (repoctx.go) turns this test RED (".." incorrectly accepted).
func TestValidateRef_RejectsDotsOnly(t *testing.T) {
	bad := []string{"..", ".", "...", "...."}
	for _, r := range bad {
		if err := validateRef(r); err == nil {
			t.Errorf("validateRef(%q): expected error (dots-only)", r)
		}
	}
}

func TestValidateRef_AcceptsGoodInput(t *testing.T) {
	good := []string{"main", "v1.0.0", "refs/tags/v2", "abcdef1234567890"}
	for _, r := range good {
		if err := validateRef(r); err != nil {
			t.Errorf("validateRef(%q): unexpected error: %v", r, err)
		}
	}
}

func TestValidateComponent_RejectsTraversalAndBadChars(t *testing.T) {
	bad := []string{"", "..", "...", "../etc", "a/b", "a b", "a;b"}
	for _, v := range bad {
		if err := validateComponent("owner", v); err == nil {
			t.Errorf("validateComponent(%q): expected error", v)
		}
	}
	if err := validateComponent("owner", "acme-org.1"); err != nil {
		t.Errorf("validateComponent(good): unexpected error: %v", err)
	}
}

func TestValidateGlob_RejectsInjectionShapes(t *testing.T) {
	bad := []string{"-x", "/etc/passwd", "../secret", "a/../b"}
	for _, g := range bad {
		if err := validateGlob(g); err == nil {
			t.Errorf("validateGlob(%q): expected error", g)
		}
	}
	if err := validateGlob(""); err != nil {
		t.Errorf("validateGlob(empty): unexpected error: %v", err)
	}
	if err := validateGlob("src/*.go"); err != nil {
		t.Errorf("validateGlob(good): unexpected error: %v", err)
	}
}
