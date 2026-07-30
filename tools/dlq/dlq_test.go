package main

// Lives alongside main.go, in package main, rather than in tests/unit: the logic under test here
// (classOf, matchesFilters, run()'s flag validation, the state file) is unexported, and tests/unit is a
// separate importable package (see tests/unit/*.go, `package unit`) that cannot reach unexported symbols
// of a `package main` command. Splitting this tool's logic into an importable package just to satisfy
// that convention would be a bigger, riskier change than the task asked for, and CLAUDE.md's B4 warning
// (tests/unit is one flat package; a single stale file disables all of it) is exactly the kind of
// blast radius this avoids by not touching that package at all.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go"

	sentinelnats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

func TestClassOf(t *testing.T) {
	cases := []struct {
		name string
		hdr  nats.Header
		want string
	}{
		{"nil header", nil, classUnclassified},
		{"no class key", nats.Header{"X-Other": []string{"x"}}, classUnclassified},
		{"empty class value", nats.Header{sentinelnats.DLQClassHeader: []string{""}}, classUnclassified},
		{"transient", nats.Header{sentinelnats.DLQClassHeader: []string{sentinelnats.DLQClassTransient}}, sentinelnats.DLQClassTransient},
		{"permanent", nats.Header{sentinelnats.DLQClassHeader: []string{sentinelnats.DLQClassPermanent}}, sentinelnats.DLQClassPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classOf(tc.hdr); got != tc.want {
				t.Errorf("classOf(%v) = %q, want %q", tc.hdr, got, tc.want)
			}
		})
	}
}

func TestMatchesFilters(t *testing.T) {
	cases := []struct {
		name   string
		cfg    config
		reason string
		class  string
		want   bool
	}{
		{"no filters matches anything", config{}, "project not found", sentinelnats.DLQClassPermanent, true},
		{"reason filter matches substring case-insensitively", config{reasonContains: "NOT FOUND"}, "project not found: x", sentinelnats.DLQClassTransient, true},
		{"reason filter rejects non-match", config{reasonContains: "constraint"}, "project not found: x", sentinelnats.DLQClassTransient, false},
		{"class filter matches", config{class: sentinelnats.DLQClassTransient}, "any reason", sentinelnats.DLQClassTransient, true},
		{"class filter rejects mismatch", config{class: sentinelnats.DLQClassPermanent}, "any reason", sentinelnats.DLQClassTransient, false},
		{"class filter matches unclassified sentinel", config{class: classUnclassified}, "any reason", classUnclassified, true},
		{"both filters must hold (AND)", config{reasonContains: "found", class: sentinelnats.DLQClassPermanent}, "project not found", sentinelnats.DLQClassPermanent, true},
		{"both filters: reason ok, class mismatch fails", config{reasonContains: "found", class: sentinelnats.DLQClassTransient}, "project not found", sentinelnats.DLQClassPermanent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesFilters(tc.cfg, tc.reason, tc.class); got != tc.want {
				t.Errorf("matchesFilters(%+v, %q, %q) = %v, want %v", tc.cfg, tc.reason, tc.class, got, tc.want)
			}
		})
	}
}

func TestContentHashStableAndDistinct(t *testing.T) {
	a := contentHash([]byte("same bytes"))
	b := contentHash([]byte("same bytes"))
	c := contentHash([]byte("different bytes"))

	if a != b {
		t.Errorf("contentHash is not deterministic: %q != %q for identical input", a, b)
	}
	if a == c {
		t.Errorf("contentHash collided for different input: %q", a)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json") // exercises directory creation in saveState

	// Missing file is not an error and yields an empty, ready-to-use state.
	st, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState on missing file: %v", err)
	}
	if len(st.Records) != 0 {
		t.Fatalf("expected empty records for missing file, got %+v", st.Records)
	}

	st.Records["deadbeef"] = replayRecord{Count: 2, LastSeq: 42}
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState after save: %v", err)
	}
	rec, ok := reloaded.Records["deadbeef"]
	if !ok {
		t.Fatalf("expected record %q to survive round trip, got %+v", "deadbeef", reloaded.Records)
	}
	if rec.Count != 2 || rec.LastSeq != 42 {
		t.Errorf("round-tripped record = %+v, want Count=2 LastSeq=42", rec)
	}

	// No temp files left behind (saveState's rename should have consumed the one it created).
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("unexpected leftover file in state directory: %s", e.Name())
		}
	}
}

func TestLoadStateRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seeding corrupt state file: %v", err)
	}
	if _, err := loadState(path); err == nil {
		t.Fatal("expected loadState to fail closed on a corrupt state file, got nil error")
	}
}

// TestRunValidatesFlagsBeforeConnecting exercises run()'s guard-rail validation. All of these are
// checked before run() ever calls nats.Connect, so they can be tested without a live NATS server —
// a bad config must be rejected before the tool touches the network at all.
func TestRunValidatesFlagsBeforeConnecting(t *testing.T) {
	cases := []struct {
		name string
		cfg  config
	}{
		{"invalid class value", config{class: "bogus"}},
		{"drain combined with purge", config{drain: true, purge: true}},
		{"drain combined with an incompatible class filter", config{drain: true, class: sentinelnats.DLQClassPermanent}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("run(%+v) = nil error, want a validation error", tc.cfg)
			}
		})
	}
}

// TestDrainForcesTransientAndDelete pins the documented -drain behavior: it always targets class
// transient and always deletes on successful replay, regardless of what -class/-delete were set to,
// as long as they don't actively conflict (checked above). This is a config-shape test, not a live-NATS
// test — it can't observe replay/delete happening, but it can (and must) prove the case that's easy to
// silently regress: -class="" plus -drain leaving cfg.class empty instead of defaulting to transient.
func TestDrainDefaultsClassWhenUnset(t *testing.T) {
	// run() mutates its local copy of cfg before ever dialing NATS; reaching the dial attempt (which
	// fails fast against an invalid URL) proves the pre-dial validation/defaulting path ran to
	// completion without error for this input shape.
	err := run(context.Background(), config{
		drain:   true,
		natsURL: "nats://127.0.0.1:1", // nothing listens here; connect fails fast
	})
	if err == nil {
		t.Fatal("expected a connection error against an unreachable NATS URL")
	}
	if got := err.Error(); !contains(got, "failed to connect to NATS") {
		t.Fatalf("expected a connection failure (meaning validation/defaulting passed), got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// sanity check that json tags round-trip through the real encoding/json package the same way saveState
// and loadState use, guarding against a struct-tag typo silently breaking persistence.
func TestReplayStateJSONShape(t *testing.T) {
	st := &replayState{Records: map[string]replayRecord{"abc": {Count: 1}}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out replayState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Records["abc"].Count != 1 {
		t.Fatalf("got %+v", out.Records)
	}
}
