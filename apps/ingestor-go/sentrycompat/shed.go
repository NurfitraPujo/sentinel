package sentrycompat

import (
	"sort"
	"strconv"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"google.golang.org/protobuf/proto"
)

// ShedSlack is room for the markers a shed round writes: one round aims at over + ShedSlack.
const ShedSlack = 16 << 10

// wellKnownContexts are never shed (spec §4.7 B3): they are what a reader of one event wants first.
var wellKnownContexts = map[string]bool{
	"device": true, "os": true, "app": true, "runtime": true, "browser": true, "culture": true,
}

// Shed is the service.Shedder both handlers pass (spec §4.7, D13). One call walks B1..B5 in order, removing
// elements until the sum of their estimates reaches over+ShedSlack, and moves to the next step only when a step
// has nothing left. Each estimate is a lower bound of the bytes the removal saves (removing an element also
// saves its tag and length prefix), so one round normally suffices. It reports whether it removed anything.
//
// Never shed, so what the event is and how it groups survive: the top-level stacktrace's frames and their file,
// function, line, in_app, module and abs_path; the titled exception's type, value and mechanism; error_class,
// message, level, environment, release_version, platform, trace_id, span_id, user, tags, request, sdk, the
// well-known contexts, the exempt metadata keys and the markers.
func Shed(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, over int) bool {
	target := over + ShedSlack
	removed := 0
	any := false
	steps := []func(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, need int) (saved int, did bool){
		shedBreadcrumbs, shedMetadata, shedContexts, shedExceptionValues,
	}
	for _, step := range steps {
		saved, did := step(p, ev, target-removed)
		removed += saved
		any = any || did
		if removed >= target {
			return any
		}
	}
	if shedContextLines(p) {
		any = true
	}
	return any
}

// B1: breadcrumbs, oldest first (the bounds step keeps them oldest first, so index 0).
func shedBreadcrumbs(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, need int) (int, bool) {
	saved, n := 0, 0
	for n < len(p.Breadcrumbs) && n < len(ev.Breadcrumbs) && saved < need {
		saved += proto.Size(ev.Breadcrumbs[n])
		n++
	}
	if n == 0 {
		return 0, false
	}
	p.Breadcrumbs = append([]validation.Breadcrumb(nil), p.Breadcrumbs[n:]...)
	if len(p.Breadcrumbs) == 0 {
		p.Breadcrumbs = nil
	}
	validation.MarkServerTruncated(p, obs.TruncatedFieldBreadcrumbs, "breadcrumbs:"+strconv.Itoa(n))
	return saved, true
}

type shedCandidate struct {
	key  string
	size int
}

func sortCandidates(c []shedCandidate) {
	sort.Slice(c, func(i, j int) bool {
		if c[i].size != c[j].size {
			return c[i].size > c[j].size
		}
		return c[i].key < c[j].key
	})
}

// B2: metadata keys (the body's extra), largest estimate first, ties to the lexically smallest key; never the two
// markers or a key the processor routes on.
func shedMetadata(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, need int) (int, bool) {
	if p.Metadata == nil || ev.Metadata == nil {
		return 0, false
	}
	var cands []shedCandidate
	for k := range p.Metadata {
		if k == validation.MetadataKeyServerTruncated || k == validation.MetadataKeyServerTruncatedKeys || validation.IsMetadataExemptKey(k) {
			continue
		}
		v, ok := ev.Metadata.Fields[k]
		if !ok {
			continue
		}
		cands = append(cands, shedCandidate{k, len(k) + proto.Size(v)})
	}
	sortCandidates(cands)
	saved := 0
	var keys []string
	for _, c := range cands {
		if saved >= need {
			break
		}
		delete(p.Metadata, c.key)
		saved += c.size
		keys = append(keys, c.key)
	}
	if len(keys) == 0 {
		return 0, false
	}
	validation.MarkServerTruncated(p, obs.TruncatedFieldMetadata, keys...)
	return saved, true
}

// B3: contexts other than the well-known ones, largest estimate first, ties to the lexically smallest name.
func shedContexts(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, need int) (int, bool) {
	var cands []shedCandidate
	for name := range p.Contexts {
		if wellKnownContexts[name] {
			continue
		}
		v, ok := ev.Contexts[name]
		if !ok {
			continue
		}
		cands = append(cands, shedCandidate{name, len(name) + proto.Size(v)})
	}
	sortCandidates(cands)
	saved := 0
	var keys []string
	for _, c := range cands {
		if saved >= need {
			break
		}
		delete(p.Contexts, c.key)
		saved += c.size
		keys = append(keys, "contexts."+c.key)
	}
	if len(keys) == 0 {
		return 0, false
	}
	if len(p.Contexts) == 0 {
		p.Contexts = nil
	}
	validation.MarkServerTruncated(p, obs.TruncatedFieldContexts, keys...)
	return saved, true
}

// B4: exception values other than the LAST one (the titled one, D3), oldest first.
func shedExceptionValues(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, need int) (int, bool) {
	if p.Exception == nil || ev.Exception == nil || len(p.Exception.Values) < 2 {
		return 0, false
	}
	saved, n := 0, 0
	for n < len(p.Exception.Values)-1 && n < len(ev.Exception.Values) && saved < need {
		saved += proto.Size(ev.Exception.Values[n])
		n++
	}
	if n == 0 {
		return 0, false
	}
	p.Exception.Values = append([]validation.ExceptionValue(nil), p.Exception.Values[n:]...)
	validation.MarkServerTruncated(p, obs.TruncatedFieldException, "exception.values:"+strconv.Itoa(n))
	return saved, true
}

// B5: every frame's context_line, pre_context and post_context, chain and top-level, all at once.
func shedContextLines(p *validation.ErrorPayload) bool {
	did := false
	clear := func(frames []validation.StackFrame) {
		for i := range frames {
			f := &frames[i]
			if f.ContextLine != "" || len(f.PreContext) > 0 || len(f.PostContext) > 0 {
				f.ContextLine, f.PreContext, f.PostContext = "", nil, nil
				did = true
			}
		}
	}
	clear(p.Stacktrace)
	if p.Exception != nil {
		for i := range p.Exception.Values {
			clear(p.Exception.Values[i].Stacktrace)
		}
	}
	if did {
		validation.MarkServerTruncated(p, obs.TruncatedFieldStacktraceContext, "stacktrace.context")
	}
	return did
}
