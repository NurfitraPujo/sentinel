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
