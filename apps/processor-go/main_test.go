package main

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/tagfacets"
)

// TestBackfillExitCode kills the mutation tfb-backfill-safety-controls-unproven names: main.go's already-
// running exit code silently becoming 0. The deploy Job's Backoff/Complete semantics (main's own comment
// on the -duty flag set) depend on ErrAlreadyRunning mapping to 2, not 0 or 1.
func TestBackfillExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"already running", tagfacets.ErrAlreadyRunning, 2},
		{"wrapped already running", fmt.Errorf("run: %w", tagfacets.ErrAlreadyRunning), 2},
		{"other error", errors.New("boom"), 1},
		{"success", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := backfillExitCode(c.err); got != c.want {
				t.Fatalf("backfillExitCode(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestBackfillFlagsNames kills the mutation tfb-backfill-safety-controls-unproven names: renaming the
// -duty flag (or -batch, -pause), which would silently break the chart's rendered CronJob args without
// any test failing until deploy.
func TestBackfillFlagsNames(t *testing.T) {
	fs, batch, pause, duty := backfillFlags()
	fs.SetOutput(io.Discard)
	if err := fs.Parse([]string{"-batch=321", "-pause=750ms", "-duty=0.42"}); err != nil {
		t.Fatalf("parsing -batch/-pause/-duty: %v", err)
	}
	if *batch != 321 {
		t.Fatalf("batch = %d, want 321", *batch)
	}
	if pause.String() != "750ms" {
		t.Fatalf("pause = %s, want 750ms", pause)
	}
	if *duty != 0.42 {
		t.Fatalf("duty = %v, want 0.42", *duty)
	}
}

// TestBackfillFlagsDefaults pins the flags' defaults to tagfacets' own exported defaults, so the two
// cannot silently drift apart.
func TestBackfillFlagsDefaults(t *testing.T) {
	fs, batch, pause, duty := backfillFlags()
	fs.SetOutput(io.Discard)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parsing no args: %v", err)
	}
	if *batch != tagfacets.DefaultBatch {
		t.Fatalf("default batch = %d, want %d", *batch, tagfacets.DefaultBatch)
	}
	if *pause != tagfacets.DefaultPause {
		t.Fatalf("default pause = %s, want %s", pause, tagfacets.DefaultPause)
	}
	if *duty != tagfacets.DefaultDuty {
		t.Fatalf("default duty = %v, want %v", *duty, tagfacets.DefaultDuty)
	}
}
