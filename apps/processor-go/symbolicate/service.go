// Package symbolicate is sentinel-sourcemaps contract §4.4: the processor stage that rewrites a
// JavaScript event's minified frames to their original positions from the release's uploaded
// sourcemaps, BEFORE the fingerprint is computed and AFTER the project id is resolved.
//
// The package has no wiring of its own (be.md task 5; task 6 calls it from
// service.processEventInternal). It deliberately does not import apps/processor-go/event:
// event.go calls fingerprint.Compute, and task 6 makes Compute call NormalizePath, so an import
// in this direction would cycle. It exposes its own Frame; the service copies the event's frames
// in and out.
//
// Everything a frame can become is one of the eight Outcomes, and nothing here ever fails an
// event (D7): every failure is a pass-through with a reason, a panic in parse or resolve is
// recovered per map (decision 19), and the per-event Deadline (decision 37) bounds the wall clock
// while the fetch + parse that outlived it continues in the background and fills the cache.
package symbolicate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// Outcome is the per-frame result -- the `outcome` label of sentinel_symbolication_frames_total
// (§12), and the reason a negative entry carries. The values are obs.SymbolicationFrame*, never
// re-typed here (B5: one spelling per state, and obs.go is where it lives).
type Outcome string

const (
	// OutcomeApplied: the frame was rewritten (§4.4 step 6).
	OutcomeApplied Outcome = Outcome(obs.SymbolicationFrameApplied)
	// OutcomeNoColumn: column == 0; a (file, line) lookup with no column would be wrong, not
	// merely absent (§4.4 step 2).
	OutcomeNoColumn Outcome = Outcome(obs.SymbolicationFrameNoColumn)
	// OutcomeNoMap: no `sourcemaps` row for (project, release, normalised path). Negatively cached.
	OutcomeNoMap Outcome = Outcome(obs.SymbolicationFrameNoMap)
	// OutcomeNoMapping: the map has no segment at or before the column on that line, or the
	// segment there carries no source (§4.4 step 5). Not cached -- the map is fine.
	OutcomeNoMapping Outcome = Outcome(obs.SymbolicationFrameNoMapping)
	// OutcomeMapError: a parse error, a sha256 mismatch, or a recovered panic. Negatively cached.
	OutcomeMapError Outcome = Outcome(obs.SymbolicationFrameMapError)
	// OutcomeFetchError: the object GET failed (or the Store did). Negatively cached on a GET
	// failure only.
	OutcomeFetchError Outcome = Outcome(obs.SymbolicationFrameFetchError)
	// OutcomeTimeout: the map's fetch + parse had not finished by the event's Deadline; the fill
	// continues in the background. NEVER negatively cached (decision 37).
	OutcomeTimeout Outcome = Outcome(obs.SymbolicationFrameTimeout)
	// OutcomeOversize: the row's size_bytes exceeds MaxParseBytes; never fetched. Negatively
	// cached (decision 38).
	OutcomeOversize Outcome = Outcome(obs.SymbolicationFrameOversize)
)

// negativelyCacheable is the set §4.4 allows a negative entry to carry: the four step-3/4
// failures, never `timeout` (the fill is in flight), never `no_mapping` (the map is fine and the
// next frame may resolve).
func (o Outcome) negativelyCacheable() bool {
	switch o {
	case OutcomeNoMap, OutcomeFetchError, OutcomeMapError, OutcomeOversize:
		return true
	}
	return false
}

// Frame is the stage's own view of a stack frame. The caller copies event.StackFrame in, calls
// Symbolicate, and copies out File/Line/Column/Function plus -- only when Symbolicated -- InApp
// and the `symbolicated: true` and `raw` keys of §4.4 step 6. A frame that passed through has
// Symbolicated false and Raw nil, and the caller writes NO key for it (§4.4 step 7: absent, not
// false, so a pre-feature occurrence and an unsymbolicated one are byte-identical).
type Frame struct {
	File     string
	Line     int32
	Column   int32
	Function string
	// InApp is the SDK's value on the way in. On a frame the stage rewrote it becomes the value
	// inApp derives from the resolved File (sentinel-sourcemaps-turbopack-grouping §4.2; the
	// SDK's value moves to Raw.InApp); on any other frame the stage leaves it exactly as sent.
	InApp bool

	Symbolicated bool
	Raw          *RawFrame
}

// RawFrame is §4.4 step 6's `raw` record: the values the SDK sent, kept beside the rewrite.
// InApp has no omitempty on purpose (turbopack-grouping D4): a `false` the SDK sent over a
// `false` the rule derived is a fact worth one byte, and omitempty on a bool would erase it.
type RawFrame struct {
	File     string `json:"file"`
	Line     int32  `json:"line"`
	Column   int32  `json:"column,omitempty"`
	Function string `json:"function"`
	InApp    bool   `json:"in_app"`
}

// namespaceRoot is turbopack-grouping §4.2 rule 2's root segment: the first segment after a
// `<scheme>:///` prefix, or the leading segment of a bare path, when it is a bracketed name
// (`[project]`, `[turbopack]`, `[next]`, `[externals]`). Decision 18: only the ROOT is tested --
// a `[id]` or `[...unmatched]` later in the path is an ordinary directory.
var namespaceRoot = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*:///)?\[([a-z][a-z0-9-]*)\]/`)

// inApp is turbopack-grouping §4.2's rule over a frame's POST-rewrite file string -- the map's
// `sources` entry joined to `sourceRoot`, never the minified name and never the function. It
// reads, in order:
//  1. false when p contains `/node_modules/` anywhere or starts with `node_modules/` (a bare
//     relative source under an empty sourceRoot -- A22);
//  2. else false when the namespace root is a bracketed name other than `[project]` (A7-A9);
//  3. else true -- `[project]`-rooted whatever directory follows (D6), `webpack:///./src/…`, a
//     bare `src/lib/x.ts`, the Vite fixture's `../../src/main.ts`.
//
// The fixture table A1-A22 (contract §4.2) is the spec; TestSymbolicate_InAppTable drives it
// through Symbolicate.
func inApp(p string) bool {
	if strings.Contains(p, "/node_modules/") || strings.HasPrefix(p, "node_modules/") {
		return false
	}
	if m := namespaceRoot.FindStringSubmatch(p); m != nil && m[1] != "project" {
		return false
	}
	return true
}

// EventOutcome is the per-event derivation of §4.4 -- the `outcome` label of
// sentinel_symbolication_events_total. Values are obs.SymbolicationEvent*.
type EventOutcome string

const (
	EventAppliedAll       EventOutcome = EventOutcome(obs.SymbolicationEventAppliedAll)
	EventAppliedPartial   EventOutcome = EventOutcome(obs.SymbolicationEventAppliedPartial)
	EventNone             EventOutcome = EventOutcome(obs.SymbolicationEventNone)
	EventSkippedPlatform  EventOutcome = EventOutcome(obs.SymbolicationEventSkippedPlatform)
	EventSkippedNoRelease EventOutcome = EventOutcome(obs.SymbolicationEventSkippedNoRelease)
	EventSkippedNoFrames  EventOutcome = EventOutcome(obs.SymbolicationEventSkippedNoFrames)
)

// Gate is §4.4's "which events are symbolicated": all three, else the reason the stage is
// skipped. releaseVersion is the value AFTER Normalize's metadata fallback. platforms is
// Config.Platforms (empty = off, which reads as skipped_platform for every event).
func Gate(platforms []string, platform, releaseVersion string, frameCount int) (ok bool, skipped EventOutcome) {
	inSet := false
	for _, p := range platforms {
		if p == platform {
			inSet = true
			break
		}
	}
	switch {
	case !inSet:
		return false, EventSkippedPlatform
	case releaseVersion == "":
		return false, EventSkippedNoRelease
	case frameCount == 0:
		return false, EventSkippedNoFrames
	}
	return true, ""
}

// Result is what Symbolicate returns: one Outcome per frame, index-aligned with the frames
// passed in (which were rewritten in place).
type Result struct {
	Outcomes []Outcome
}

// EventOutcome derives §4.4's per-occurrence state from the frames.
func (r Result) EventOutcome() EventOutcome {
	applied := 0
	for _, o := range r.Outcomes {
		if o == OutcomeApplied {
			applied++
		}
	}
	switch {
	case len(r.Outcomes) == 0 || applied == 0:
		return EventNone
	case applied == len(r.Outcomes):
		return EventAppliedAll
	}
	return EventAppliedPartial
}

// Observer receives what is only visible inside the stage: cache events, the cache's byte total
// and the cold-path fetch + parse duration (§12's cache_events_total, cache_bytes and
// fetch_duration_seconds). Frame and event outcomes are in Result and Gate's return; the caller
// counts those. NopObserver is the default.
type Observer interface {
	CacheEvent(kind CacheEventKind)
	CacheBytes(current int64)
	FetchDuration(d time.Duration)
}

// NopObserver discards every observation.
type NopObserver struct{}

func (NopObserver) CacheEvent(CacheEventKind)   {}
func (NopObserver) CacheBytes(int64)            {}
func (NopObserver) FetchDuration(time.Duration) {}

// Service is the stage. One per process; safe for concurrent use.
type Service struct {
	cfg      Config
	store    Store
	fetcher  Fetcher
	cache    *cache
	observer Observer
	now      func() time.Time
	logger   *slog.Logger

	mu    sync.Mutex
	fills map[string]*fill // in-flight fetch + parse, keyed by sourcemaps.id
	wg    sync.WaitGroup   // every fill goroutine; Close waits for them
}

// Option configures New.
type Option func(*Service)

// WithObserver wires the metrics seam.
func WithObserver(o Observer) Option { return func(s *Service) { s.observer = o } }

// WithLogger sets the logger the background fill reports failures to (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// withClock injects the clock the cache's TTLs read (tests).
func withClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// New builds the stage. cfg must come from DefaultConfig/ParseConfig -- a zero Deadline is an
// error, not "no deadline", because it would time out every cold map on the first event.
func New(cfg Config, store Store, fetcher Fetcher, opts ...Option) (*Service, error) {
	if cfg.Deadline <= 0 || cfg.CacheBytes <= 0 || cfg.MaxParseBytes <= 0 || cfg.NegativeTTL <= 0 || cfg.PositiveTTL <= 0 {
		return nil, errors.New("symbolicate: every Config bound must be positive; use DefaultConfig or ParseConfig")
	}
	if store == nil || fetcher == nil {
		return nil, errors.New("symbolicate: Store and Fetcher are required")
	}
	s := &Service{
		cfg:      cfg,
		store:    store,
		fetcher:  fetcher,
		observer: NopObserver{},
		now:      time.Now,
		logger:   slog.Default(),
		fills:    map[string]*fill{},
	}
	for _, o := range opts {
		o(s)
	}
	s.cache = newCache(cfg, s.now, s.observer)
	return s, nil
}

// Close waits for every in-flight background fill; the processor calls it on shutdown so a fill
// does not outlive the process's storage client.
func (s *Service) Close() { s.wg.Wait() }

// CacheBytes is the positive cache's current cost (raw bytes).
func (s *Service) CacheBytes() int64 { return s.cache.Bytes() }

// Symbolicate is §4.4 steps 1-7 for one event's frames, rewritten in place. projectID is the
// RESOLVED project (never a frame's), releaseVersion the value after Normalize's fallback. The
// caller has already passed Gate. It never returns an error and never panics on a map's account
// (D7): the returned Result has exactly one Outcome per frame.
//
// The whole call is bounded by cfg.Deadline (decision 37): frames whose map's fetch + parse has
// not landed by then are `timeout`, and the fill continues in the background.
func (s *Service) Symbolicate(ctx context.Context, projectID, releaseVersion string, frames []Frame) Result {
	res := Result{Outcomes: make([]Outcome, len(frames))}
	if len(frames) == 0 {
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Deadline)
	defer cancel()

	// Group the frames by normalised path so each map is looked up, fetched and resolved once per
	// event (§4.4 step 3: the query at most once per triple). Order of first appearance is kept so
	// the deadline is spent on the top frames first.
	type group struct {
		path    string
		indexes []int
	}
	var groups []*group
	byPath := map[string]*group{}
	for i, f := range frames {
		if f.Column <= 0 {
			// 0 is the wire's "unknown" (§4.3); a negative never passes the ingestor, but if one
			// did it is no column either, not a lookup that cannot resolve.
			res.Outcomes[i] = OutcomeNoColumn
			continue
		}
		path := NormalizePath(f.File)
		g, ok := byPath[path]
		if !ok {
			g = &group{path: path}
			byPath[path] = g
			groups = append(groups, g)
		}
		g.indexes = append(g.indexes, i)
	}

	for _, g := range groups {
		row, pm, outcome := s.lookup(ctx, negKey{projectID, releaseVersion, g.path})
		if outcome != OutcomeApplied {
			for _, i := range g.indexes {
				res.Outcomes[i] = outcome
			}
			continue
		}
		if !s.resolveGroup(row, pm, frames, g.indexes, res.Outcomes) {
			// The map's resolve panicked: every frame on it is map_error, the entry is dropped and
			// the triple negatively cached (decision 19).
			s.cache.drop(row.ID)
			s.cache.setNegative(negKey{projectID, releaseVersion, g.path}, OutcomeMapError)
			for _, i := range g.indexes {
				res.Outcomes[i] = OutcomeMapError
			}
		}
	}
	return res
}

// resolveGroup rewrites every frame of one map under a per-map recover; false means the resolve
// panicked (an index past sources/names -- see sourcemap.go) and the caller applies map_error.
func (s *Service) resolveGroup(row *Sourcemap, pm *parsedMap, frames []Frame, indexes []int, outcomes []Outcome) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("sourcemap resolve panicked; frames pass through as map_error",
				slog.String("sourcemap_id", row.ID), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			ok = false
		}
	}()
	// Resolve every frame BEFORE writing any, so a panic on the third frame does not leave the
	// first two rewritten while the map is being reported broken.
	rs := make([]resolved, len(indexes))
	found := make([]bool, len(indexes))
	for k, i := range indexes {
		rs[k], found[k] = pm.resolve(frames[i].Line, frames[i].Column)
	}
	for k, i := range indexes {
		if !found[k] {
			outcomes[i] = OutcomeNoMapping
			continue
		}
		f := &frames[i]
		f.Raw = &RawFrame{File: f.File, Line: f.Line, Column: f.Column, Function: f.Function, InApp: f.InApp}
		f.File = rs[k].file
		f.Line = rs[k].line
		f.Column = rs[k].column
		if rs[k].name != "" {
			f.Function = rs[k].name
		}
		// turbopack-grouping §4.2: the map outranks the SDK on a rewritten frame (A4, flow P3);
		// the SDK's value is already under Raw.InApp. Read AFTER f.File took the resolved source.
		f.InApp = inApp(f.File)
		f.Symbolicated = true
		outcomes[i] = OutcomeApplied
	}
	return true
}

// lookup is §4.4 steps 3-4 through the cache for one triple: the negative cache, the positive
// cache (a hit costs no query), the Store, the parse cap, then the (shared, background-capable)
// fill. It returns the parsed map with OutcomeApplied, or the pass-through reason for every frame
// on the path.
func (s *Service) lookup(ctx context.Context, k negKey) (*Sourcemap, *parsedMap, Outcome) {
	if reason, ok := s.cache.negative(k); ok {
		return nil, nil, reason
	}
	if row, pm, ok := s.cache.get(k); ok {
		return row, pm, OutcomeApplied
	}
	row, err := s.store.Lookup(ctx, k.projectID, k.releaseVersion, k.minifiedPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, OutcomeTimeout
		}
		s.logger.Warn("sourcemap lookup failed; frames pass through as fetch_error", slog.String("error", err.Error()))
		return nil, nil, OutcomeFetchError
	}
	if row == nil {
		s.cache.setNegative(k, OutcomeNoMap)
		return nil, nil, OutcomeNoMap
	}
	if row.SizeBytes > s.cfg.MaxParseBytes {
		s.cache.setNegative(k, OutcomeOversize)
		return nil, nil, OutcomeOversize
	}
	f := s.startFill(ctx, k, row)
	if !f.wait(ctx) {
		return nil, nil, OutcomeTimeout
	}
	if f.outcome != OutcomeApplied {
		return nil, nil, f.outcome
	}
	return row, f.pm, OutcomeApplied
}

// fill is one in-flight fetch + parse of one row. One per sourcemaps.id at a time (decision 37's
// "one in-flight fill per key"): a burst of events on one cold map joins the same fill instead of
// fetching it N times.
type fill struct {
	done    chan struct{}
	pm      *parsedMap
	outcome Outcome
}

// wait blocks until the fill lands or ctx expires; a fill that has landed wins even when both
// are ready, so a map that arrived exactly at the deadline is applied, not timed out.
func (f *fill) wait(ctx context.Context) bool {
	select {
	case <-f.done:
		return true
	default:
	}
	select {
	case <-f.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// fillTimeoutFactor bounds the background fill at this many event deadlines (floor
// fillTimeoutMin): a fill that never returns would otherwise pin its key forever and make every
// later event on the map a `timeout`. Not an operator knob -- it follows SYMBOLICATE_DEADLINE.
const (
	fillTimeoutFactor = 10
	fillTimeoutMin    = 30 * time.Second
)

func (s *Service) startFill(ctx context.Context, k negKey, row *Sourcemap) *fill {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.fills[row.ID]; ok {
		return f
	}
	f := &fill{done: make(chan struct{})}
	s.fills[row.ID] = f
	// The fill outlives the event's deadline on purpose: detached from ctx's cancellation (its
	// values -- trace context -- are kept) under its own generous bound.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), max(fillTimeoutMin, fillTimeoutFactor*s.cfg.Deadline))
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		f.pm, f.outcome = s.fetchAndParse(bg, row)
		if f.outcome == OutcomeApplied {
			s.cache.put(k, row, f.pm)
		} else {
			s.cache.setNegative(k, f.outcome)
		}
		s.mu.Lock()
		delete(s.fills, row.ID)
		s.mu.Unlock()
		close(f.done)
	}()
	return f
}

// fetchAndParse is §4.4 step 4 for one row under the per-map recover of decision 19: GET the
// bytes, verify sha256, parse (dropping sourcesContent). Every failure is a reason; a panic in
// the parser is `map_error`.
func (s *Service) fetchAndParse(ctx context.Context, row *Sourcemap) (pm *parsedMap, outcome Outcome) {
	start := s.now()
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("sourcemap parse panicked; frames pass through as map_error",
				slog.String("sourcemap_id", row.ID), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			pm, outcome = nil, OutcomeMapError
		}
		s.observer.FetchDuration(s.now().Sub(start))
	}()
	raw, err := s.fetcher.Fetch(ctx, row.StorageKey)
	if err != nil {
		s.logger.Warn("sourcemap fetch failed; frames pass through as fetch_error",
			slog.String("sourcemap_id", row.ID), slog.String("storage_key", row.StorageKey), slog.String("error", err.Error()))
		return nil, OutcomeFetchError
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, strings.TrimSpace(row.SHA256)) {
		s.logger.Warn("sourcemap sha256 mismatch; frames pass through as map_error",
			slog.String("sourcemap_id", row.ID), slog.String("expected", row.SHA256), slog.String("got", got))
		return nil, OutcomeMapError
	}
	pm, err = parseSourcemap(raw)
	if err != nil {
		s.logger.Warn("sourcemap parse failed; frames pass through as map_error",
			slog.String("sourcemap_id", row.ID), slog.String("error", err.Error()))
		return nil, OutcomeMapError
	}
	return pm, OutcomeApplied
}
