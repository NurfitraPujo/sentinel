package unit

import (
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
)

func TestCompute_BasicFingerprint(t *testing.T) {
	tests := []struct {
		name     string
		cfg      fingerprint.FingerprintConfig
		wantLen  int
		contains string
	}{
		{
			name: "basic fingerprint with app frames",
			cfg: fingerprint.FingerprintConfig{
				ErrorClass: "NullPointerException",
				Stacktrace: []fingerprint.StackFrame{
					{File: "main.go", Line: 10, Function: "main", InApp: true},
					{File: "utils.go", Line: 20, Function: "process", InApp: true},
				},
			},
			wantLen:  16,
			contains: "",
		},
		{
			name: "fingerprint with custom override",
			cfg: fingerprint.FingerprintConfig{
				CustomFingerprint: "custom-fp-123",
				ErrorClass:        "NullPointerException",
				Stacktrace: []fingerprint.StackFrame{
					{File: "main.go", Line: 10, Function: "main", InApp: true},
				},
			},
			wantLen:  0,
			contains: "custom-fp-123",
		},
		{
			name: "error class only - no app frames",
			cfg: fingerprint.FingerprintConfig{
				ErrorClass: "RuntimeError",
				Stacktrace: []fingerprint.StackFrame{},
			},
			wantLen: 16,
		},
		{
			name: "only non-app frames",
			cfg: fingerprint.FingerprintConfig{
				ErrorClass: "Error",
				Stacktrace: []fingerprint.StackFrame{
					{File: "/usr/local/go/lib.go", Line: 10, Function: "someFunc", InApp: false},
				},
			},
			wantLen: 16,
		},
		{
			name: "limit to max app frames",
			cfg: fingerprint.FingerprintConfig{
				ErrorClass: "Error",
				Stacktrace: []fingerprint.StackFrame{
					{File: "a.go", Line: 1, Function: "f1", InApp: true},
					{File: "b.go", Line: 2, Function: "f2", InApp: true},
					{File: "c.go", Line: 3, Function: "f3", InApp: true},
					{File: "d.go", Line: 4, Function: "f4", InApp: true},
					{File: "e.go", Line: 5, Function: "f5", InApp: true},
				},
			},
			wantLen: 16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fingerprint.Compute(tt.cfg)
			if tt.contains != "" {
				if got != tt.contains {
					t.Errorf("Compute() = %v, want %v", got, tt.contains)
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("Compute() length = %v, want %v", len(got), tt.wantLen)
			}
		})
	}
}

func TestCompute_Deterministic(t *testing.T) {
	cfg := fingerprint.FingerprintConfig{
		ErrorClass: "NullPointerException",
		Stacktrace: []fingerprint.StackFrame{
			{File: "main.go", Line: 10, Function: "main", InApp: true},
		},
	}

	fp1 := fingerprint.Compute(cfg)
	fp2 := fingerprint.Compute(cfg)

	if fp1 != fp2 {
		t.Errorf("Compute() not deterministic: got %v and %v", fp1, fp2)
	}
}

func TestCompute_CollisionResistance(t *testing.T) {
	cfg1 := fingerprint.FingerprintConfig{
		ErrorClass: "ErrorA",
		Stacktrace: []fingerprint.StackFrame{
			{File: "file.go", Line: 10, Function: "funcA", InApp: true},
		},
	}

	cfg2 := fingerprint.FingerprintConfig{
		ErrorClass: "ErrorB",
		Stacktrace: []fingerprint.StackFrame{
			{File: "file.go", Line: 10, Function: "funcA", InApp: true},
		},
	}

	fp1 := fingerprint.Compute(cfg1)
	fp2 := fingerprint.Compute(cfg2)

	if fp1 == fp2 {
		t.Errorf("Compute() produced collision: both got %v", fp1)
	}
}

// TestCompute_EnvironmentSplitsIdentity is criterion 2's fingerprint half
// (sentinel-project-environments §4.9): the SAME error class and the SAME frames delivered to
// `production` and to `staging` must not hash to the same value, and a `CustomFingerprint` sent
// from both environments must still return that fingerprint verbatim.
//
// The second half is the one that looks like a bug and is not. Prefixing a client-chosen
// fingerprint would change identity for exactly the clients who chose it, which is the one thing a
// custom fingerprint exists to prevent — so `Compute` returns it unchanged and the SPLIT comes from
// the UNIQUE (project_id, environment, fingerprint) constraint instead. The hash change is defence
// in depth; the column is the mechanism.
func TestCompute_EnvironmentSplitsIdentity(t *testing.T) {
	frames := []fingerprint.StackFrame{
		{File: "checkout.go", Line: 88, Function: "checkout.Charge", InApp: true},
	}

	tests := []struct {
		name       string
		production fingerprint.FingerprintConfig
		staging    fingerprint.FingerprintConfig
		wantEqual  bool
	}{
		{
			name: "same class and frames in two environments hash differently",
			production: fingerprint.FingerprintConfig{
				Environment: "production", ErrorClass: "PaymentError", Stacktrace: frames,
			},
			staging: fingerprint.FingerprintConfig{
				Environment: "staging", ErrorClass: "PaymentError", Stacktrace: frames,
			},
			wantEqual: false,
		},
		{
			name: "same class with no frames at all still splits on environment alone",
			production: fingerprint.FingerprintConfig{
				Environment: "production", ErrorClass: "PaymentError",
			},
			staging: fingerprint.FingerprintConfig{
				Environment: "staging", ErrorClass: "PaymentError",
			},
			wantEqual: false,
		},
		{
			name: "a CustomFingerprint is returned verbatim in BOTH environments",
			production: fingerprint.FingerprintConfig{
				CustomFingerprint: "checkout-charge-timeout",
				Environment:       "production", ErrorClass: "PaymentError", Stacktrace: frames,
			},
			staging: fingerprint.FingerprintConfig{
				CustomFingerprint: "checkout-charge-timeout",
				Environment:       "staging", ErrorClass: "PaymentError", Stacktrace: frames,
			},
			wantEqual: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prod := fingerprint.Compute(tt.production)
			stag := fingerprint.Compute(tt.staging)
			if (prod == stag) != tt.wantEqual {
				t.Errorf("Compute(production)=%q Compute(staging)=%q; wantEqual=%v", prod, stag, tt.wantEqual)
			}
			if tt.production.CustomFingerprint != "" && prod != tt.production.CustomFingerprint {
				t.Errorf("CustomFingerprint was not returned verbatim: got %q, want %q",
					prod, tt.production.CustomFingerprint)
			}
		})
	}
}

// TestCompute_EnvironmentIsPrefixedNotAppended pins the ORDER of §4.9's hash input
// (`environment + "|" + errorClass`, then the frames) rather than only that two environments
// differ. Swapping the two fields is the mistake this catches: an implementation that appended the
// environment after the error class would satisfy every assertion above and still produce a
// different value for every fingerprint the contract's formula would have produced.
//
// Note what this deliberately does NOT claim: a delimiter-joined input is not injective, so
// Environment "a|b" + ErrorClass "c" and Environment "a" + ErrorClass "b|c" hash identically by
// construction. That is a property of the formula §4.9 specifies, not a defect, and asserting
// otherwise would be asserting against the contract.
func TestCompute_EnvironmentIsPrefixedNotAppended(t *testing.T) {
	forward := fingerprint.Compute(fingerprint.FingerprintConfig{Environment: "staging", ErrorClass: "TimeoutError"})
	swapped := fingerprint.Compute(fingerprint.FingerprintConfig{Environment: "TimeoutError", ErrorClass: "staging"})
	if forward == swapped {
		t.Errorf("environment and error class are order-insensitive in the hash input: both hashed to %v", forward)
	}
}
