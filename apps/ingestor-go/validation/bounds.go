package validation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// The live-path bounds (sentinel-event-schema-v2 spec §4.2). Every string bound is in CODE POINTS,
// the unit protovalidate's size() and a Postgres VARCHAR both count in, unless its name ends in
// Bytes or Size (serialized bytes, as Go's json.Marshal produces them). docs/sdk-specification.md §3
// carries one table row per constant, and tests/unit/sdk_spec_limits_test.go (V3b) fails when the two
// disagree.
const (
	// ---- existing fields: enforced by protovalidate (reject 400), listed here so the docs table has
	// one constant per row. None of them is checked in this package any more (spec §4.3).
	MaxProjectKeyLength     = 64
	MaxPlatformLength       = 50
	MaxEnvironmentLength    = 50
	MaxMessageLength        = 10000
	MaxErrorClassLength     = 255
	MaxTraceIDLength        = 64
	MaxSpanIDLength         = 64
	MaxReleaseVersionLength = 100
	// MaxFingerprintLength matches issues.fingerprint VARCHAR(64) and the "error_event.fingerprint" CEL
	// rule. A client fingerprint is stored unhashed, and a shortened grouping key would merge issues,
	// so an oversized one is rejected by protovalidate, never truncated.
	MaxFingerprintLength = 64
	// MaxEventIDLength: an event_id over it is REPLACED with a minted UUIDv4 by service.IngestAt.
	MaxEventIDLength    = 64
	MaxStacktraceFrames = 100
	// MaxFilePathLength and MaxFunctionNameLength are SDK-side MUST-truncate rules for top-level
	// frames, which the server does NOT enforce: both are fingerprint inputs, and cutting one here
	// would split existing issues (spec §4.2, S4). Chain frames are not fingerprint inputs, so the
	// server truncates theirs to the same numbers.
	MaxFilePathLength     = 512
	MaxFunctionNameLength = 255
	// MaxMetadataSize is enforced HERE, by shrinking (spec §4.2 `metadata` row, S3).
	MaxMetadataSize = 64 * 1024

	// ---- v2 fields: enforced here, never a 400.
	MaxLoggerLength             = 64
	MaxServerNameLength         = 256
	MaxTransactionLength        = 200
	MaxDistLength               = 64
	MaxSourceLength             = 64
	MaxSDKNameLength            = 128
	MaxSDKVersionLength         = 64
	MaxUserIDBytes              = 255 // = the processor's maxAffectedIDBytes (coordinator ruling 6)
	MaxUsernameLength           = 128
	MaxUserEmailLength          = 254
	MaxUserIPAddressLength      = 45 // the textual IPv6 maximum; the format is not validated
	MaxGeoTextLength            = 128
	MaxGeoCountryCodeLength     = 8
	MaxTags                     = 50
	MaxTagKeyLength             = 32
	MaxTagValueLength           = 200
	MaxContexts                 = 32
	MaxContextKeyLength         = 64
	MaxContextsSize             = 64 * 1024
	MaxRequestMethodLength      = 16
	MaxRequestURLLength         = 2048
	MaxRequestQueryStringLength = 4096
	MaxRequestHeaders           = 64
	MaxRequestHeaderKeyLength   = 256
	MaxRequestHeaderValueLength = 4096
	MaxRequestEnv               = 64
	MaxRequestEnvKeyLength      = 256
	MaxRequestEnvValueLength    = 1024
	MaxRequestDataSize          = 16 * 1024
	MaxBreadcrumbs              = 100
	MaxBreadcrumbsSize          = 128 * 1024
	MaxBreadcrumbTypeLength     = 64
	MaxBreadcrumbCategoryLength = 128
	MaxBreadcrumbMessageLength  = 8192
	MaxBreadcrumbDataSize       = 4096
	MaxExceptionValues          = 20
	MaxExceptionTypeLength      = 255
	MaxExceptionValueLength     = 10000
	MaxMechanismTypeLength      = 64
	MaxExceptionFrames          = 100
	MaxFrameModuleLength        = 255
	MaxFrameAbsPathLength       = 1024
	MaxFrameContextLineLength   = 1024
	MaxFrameContextLines        = 5

	// ---- the server's own marker (review finding esv2-rev-markers-unbounded-evict-exempt). The shrink
	// never removes the markers, so they must be bounded for "the markers always fit" (spec §4.2) to
	// hold: server_truncated_keys lists at most MaxServerTruncatedKeys removed keys, each cut to
	// MaxServerTruncatedKeyBytes on a UTF-8 boundary, then one "+N more" entry for the rest. Even fully
	// JSON-escaped that is under 40 KB, so the markers can never push an exempt key out on their own.
	MaxServerTruncatedKeys     = 50
	MaxServerTruncatedKeyBytes = 128
)

// The two server-owned metadata keys (spec §4.2 "Marker"). Distinct from the JS SDK's client-side
// `truncated` / `truncated_keys`, so a reader can always tell who cut what. A client value under
// either key is overwritten or removed.
const (
	MetadataKeyServerTruncated     = "server_truncated"
	MetadataKeyServerTruncatedKeys = "server_truncated_keys"
)

// metadataExemptKeys are the metadata keys the processor routes on (the affected-id reads, the search
// index reads, the release_version fallback) plus the JS SDK's own markers. The shrink removes them
// only after everything else is gone.
var metadataExemptKeys = map[string]bool{
	"user_id": true, "userId": true, "user": true,
	"tenant_id": true, "tenantId": true, "organization_id": true,
	"trace_id": true, "traceId": true, "trace-id": true,
	"span_id": true, "spanId": true, "span-id": true,
	"request_id": true, "requestId": true, "request-id": true,
	"release_version": true, "truncated": true, "truncated_keys": true,
}

// IsMetadataExemptKey reports whether k is a metadata key the processor routes on. The bounds step's shrink
// removes such a key only after everything else; a caller that sheds metadata itself (the Sentry-compatible
// routes' publish-budget shedder) must exempt the same keys, so both read this one list.
func IsMetadataExemptKey(k string) bool { return metadataExemptKeys[k] }

// wellKnownContexts are removed last when contexts is over its bound.
var wellKnownContexts = map[string]bool{
	"device": true, "os": true, "app": true, "runtime": true, "browser": true, "culture": true,
}

var tagKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,32}$`)

var validLevels = map[string]bool{"": true, "fatal": true, "error": true, "warning": true, "info": true, "debug": true}

// cuts accumulates what the bounds step changed, for the metadata markers.
type cuts struct {
	fields map[string]bool
	keys   removedKeys
}

// removedKeys is server_truncated_keys: the first MaxServerTruncatedKeys removed keys, each cut to
// MaxServerTruncatedKeyBytes, then one "+N more" entry counting the rest. It keeps its own serialized
// size so the metadata shrink can measure the marker per removal in O(1).
type removedKeys struct {
	list     []interface{} // strings; []interface{} is the shape structpb.NewStruct accepts
	overflow int
	bytes    int // json.Marshal size of list's elements and the commas between them
}

func (r *removedKeys) empty() bool { return len(r.list) == 0 }

func (r *removedKeys) add(k string) {
	if len(r.list) == MaxServerTruncatedKeys {
		r.overflow++
		return
	}
	k = truncateBytes(k, MaxServerTruncatedKeyBytes)
	if len(r.list) > 0 {
		r.bytes++
	}
	r.bytes += jsonSize(k)
	r.list = append(r.list, k)
}

func (r *removedKeys) overflowEntry() string { return fmt.Sprintf("+%d more", r.overflow) }

// jsonSize is the json.Marshal size of value().
func (r *removedKeys) jsonSize() int {
	n := 2 + r.bytes
	if r.overflow > 0 {
		n += 1 + jsonSize(r.overflowEntry())
	}
	return n
}

func (r *removedKeys) value() []interface{} {
	out := make([]interface{}, len(r.list), len(r.list)+1)
	copy(out, r.list)
	if r.overflow > 0 {
		out = append(out, r.overflowEntry())
	}
	return out
}

func (c *cuts) mark(field string) { c.fields[field] = true }

// addKey records one removed map key for server_truncated_keys (capped, see removedKeys). The entry
// never carries a U+0000: the markers are stored in error_occurrences.metadata JSONB, which
// rejects it with 22P05, a Permanent error that dead-letters the event (esv2-rev-nul-tag-key-dlq).
func (c *cuts) addKey(k string) {
	c.keys.add(strings.ReplaceAll(k, "\x00", ""))
}

// str applies the NUL strip and then a code-point truncation to one v2 string, marking field for
// either change. Stripping runs first so the bound is measured on what will be stored.
func (c *cuts) str(field string, s string, max int) string {
	s = c.nul(field, s)
	if utf8.RuneCountInString(s) > max {
		c.mark(field)
		s = truncateRunes(s, max)
	}
	return s
}

// ident applies the NUL strip and then DROPS an identifier over its bound: a truncated identifier is a
// different identifier (the argument at apps/processor-go/event/event.go's BoundAffectedID).
func (c *cuts) ident(field string, s string, over func(string) bool) string {
	s = c.nul(field, s)
	if over(s) {
		c.mark(field)
		return ""
	}
	return s
}

// nul removes every U+0000 (Postgres rejects it in text with 22021 and in jsonb with 22P05, both
// class 22, both dead-lettered as Permanent).
func (c *cuts) nul(field string, s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	c.mark(field)
	return strings.ReplaceAll(s, "\x00", "")
}

// nulDeep strips U+0000 from every key and string value of a decoded JSON value, marking field once.
func (c *cuts) nulDeep(field string, v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return c.nul(field, t)
	case map[string]interface{}:
		return c.nulMap(field, t)
	case []interface{}:
		for i := range t {
			t[i] = c.nulDeep(field, t[i])
		}
		return t
	default:
		return v
	}
}

func (c *cuts) nulMap(field string, m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	var renamed []string
	for k, v := range m {
		m[k] = c.nulDeep(field, v)
		if strings.IndexByte(k, 0) >= 0 {
			renamed = append(renamed, k)
		}
	}
	// Deterministic on a collision: keys are re-inserted in sorted order and the first one wins.
	sort.Strings(renamed)
	for _, k := range renamed {
		c.mark(field)
		v := m[k]
		delete(m, k)
		clean := strings.ReplaceAll(k, "\x00", "")
		if _, taken := m[clean]; !taken && clean != "" {
			m[clean] = v
		}
	}
	return m
}

// truncateBytes cuts s to at most max bytes on a UTF-8 boundary.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func truncateRunes(s string, max int) string {
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

func overRunes(max int) func(string) bool {
	return func(s string) bool { return utf8.RuneCountInString(s) > max }
}

func jsonSize(v interface{}) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// entrySize is the byte size of `"key":value` inside a json.Marshal'd object.
func entrySize(k string, v interface{}) int {
	return jsonSize(k) + 1 + jsonSize(v)
}

// applyBounds is spec §4.2 in the order §4.2 fixes: the v2 field cuts first, which write the markers
// into metadata, then the metadata shrink, which measures the map WITH the markers and never removes
// them.
func applyBounds(p *ErrorPayload) {
	c := &cuts{fields: map[string]bool{}}

	if !validLevels[p.Level] {
		c.mark(obs.TruncatedFieldLevel)
		p.Level = ""
	}
	p.Logger = c.str(obs.TruncatedFieldLogger, p.Logger, MaxLoggerLength)
	p.ServerName = c.str(obs.TruncatedFieldServerName, p.ServerName, MaxServerNameLength)
	p.Transaction = c.str(obs.TruncatedFieldTransaction, p.Transaction, MaxTransactionLength)
	p.Dist = c.str(obs.TruncatedFieldDist, p.Dist, MaxDistLength)
	p.Source = c.str(obs.TruncatedFieldSource, p.Source, MaxSourceLength)

	if p.SDK != nil {
		p.SDK.Name = c.str(obs.TruncatedFieldSDK, p.SDK.Name, MaxSDKNameLength)
		p.SDK.Version = c.str(obs.TruncatedFieldSDK, p.SDK.Version, MaxSDKVersionLength)
	}
	boundUser(c, p.User)
	boundTags(c, p)
	boundContexts(c, p)
	boundRequest(c, p.Request)
	boundBreadcrumbs(c, p)
	boundException(c, p.Exception)
	for i := range p.Stacktrace {
		boundFrameV2(c, &p.Stacktrace[i])
	}

	writeMarkers(c, p)
	shrinkMetadata(c, p)
}

func boundUser(c *cuts, u *UserInfo) {
	if u == nil {
		return
	}
	u.ID = c.ident(obs.TruncatedFieldUserID, u.ID, func(s string) bool { return len(s) > MaxUserIDBytes })
	u.Username = c.str(obs.TruncatedFieldUserUsername, u.Username, MaxUsernameLength)
	u.Email = c.ident(obs.TruncatedFieldUserEmail, u.Email, overRunes(MaxUserEmailLength))
	u.IPAddress = c.ident(obs.TruncatedFieldUserIPAddress, u.IPAddress, overRunes(MaxUserIPAddressLength))
	if g := u.Geo; g != nil {
		g.City = c.str(obs.TruncatedFieldUserGeo, g.City, MaxGeoTextLength)
		g.Region = c.str(obs.TruncatedFieldUserGeo, g.Region, MaxGeoTextLength)
		g.Subdivision = c.str(obs.TruncatedFieldUserGeo, g.Subdivision, MaxGeoTextLength)
		g.CountryCode = c.ident(obs.TruncatedFieldUserGeo, g.CountryCode, overRunes(MaxGeoCountryCodeLength))
	}
}

// boundTags strips U+0000 from every key and value, drops every entry still invalid, then keeps the
// MaxTags lexically smallest keys (Go map order is random, so the choice has to be deterministic). A key
// is stripped to its valid form ("env\x00x" -> "envx"), not dropped; when the stripped form collides
// with a key the client sent as-is, the sent one wins, and between two stripped forms the lexically first
// original key. Every key missing from the result is recorded once as `tags.<key>`, by its stripped form
// (review findings esv2-rev-nul-tag-key-dlq, esv2-rev2-nul-collision-markers).
func boundTags(c *cuts, p *ErrorPayload) {
	if len(p.Tags) == 0 {
		return
	}
	var removed, collided []string
	out := make(map[string]string, len(p.Tags))
	for _, k := range sortedKeys(p.Tags) {
		clean := c.nul(obs.TruncatedFieldTags, k)
		v := c.nul(obs.TruncatedFieldTags, p.Tags[k])
		if clean != k {
			_, sent := p.Tags[clean]
			_, taken := out[clean]
			if sent || taken {
				collided = append(collided, clean)
				continue
			}
		}
		if !tagKeyPattern.MatchString(clean) || v == "" || utf8.RuneCountInString(v) > MaxTagValueLength || strings.Contains(v, "\n") {
			removed = append(removed, clean)
			continue
		}
		out[clean] = v
	}
	if keys := sortedKeys(out); len(keys) > MaxTags {
		for _, k := range keys[MaxTags:] {
			delete(out, k)
			removed = append(removed, k)
		}
	}
	removed = appendLost(removed, collided, func(k string) bool { _, kept := out[k]; return kept })
	p.Tags = out
	if len(removed) > 0 {
		c.mark(obs.TruncatedFieldTags)
		for _, k := range removed {
			c.addKey("tags." + k)
		}
	}
}

// appendLost finishes a NUL-collision map's removed list: a stripped key that lost to an entry of the
// same name is recorded only when no entry of that name was kept, since the key the client sent (or the
// first stripped form) is otherwise what the name describes. The result is sorted and has no
// duplicates, so `{"":..,"\x00":..}` is recorded once (review finding esv2-rev2-nul-collision-markers).
func appendLost(removed, collided []string, kept func(string) bool) []string {
	for _, k := range collided {
		if !kept(k) {
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	return slices.Compact(removed)
}

// boundContexts drops every context with an invalid name, then, while over the count or the byte
// total, removes one context at a time: non-well-known before well-known, largest serialized first,
// ties to the lexically smallest name. A NUL-stripped name collides the way a tag key does (see
// boundTags), and every name missing from the result is recorded once as `contexts.<name>`.
func boundContexts(c *cuts, p *ErrorPayload) {
	if len(p.Contexts) == 0 {
		return
	}
	var removed, collided []string
	for _, k := range sortedKeys(p.Contexts) {
		v := p.Contexts[k]
		if v != nil {
			v = c.nulMap(obs.TruncatedFieldContexts, v)
		}
		clean := c.nul(obs.TruncatedFieldContexts, k)
		if clean != k {
			delete(p.Contexts, k)
			if _, taken := p.Contexts[clean]; taken {
				collided = append(collided, clean)
				continue
			}
		}
		if n := utf8.RuneCountInString(clean); n < 1 || n > MaxContextKeyLength {
			delete(p.Contexts, clean)
			removed = append(removed, clean)
			continue
		}
		p.Contexts[clean] = v
	}
	// One sort and a running size (spec §4.2): an entry's size never changes, so the victim order is
	// fixed up front. Re-sorting and re-marshalling per removal was O(n^2 log n) in the context count,
	// a CPU DoS from one request (review finding esv2-rev-bounds-quadratic-dos).
	candidates := make([]sizedKey, 0, len(p.Contexts))
	body := 0
	for k, v := range p.Contexts {
		s := entrySize(k, v)
		candidates = append(candidates, sizedKey{key: k, size: s, last: wellKnownContexts[k]})
		body += s
	}
	sortVictims(candidates)
	for _, v := range candidates {
		if len(p.Contexts) <= MaxContexts && objectSize(body, len(p.Contexts)) <= MaxContextsSize {
			break
		}
		delete(p.Contexts, v.key)
		body -= v.size
		removed = append(removed, v.key)
	}
	removed = appendLost(removed, collided, func(k string) bool { _, kept := p.Contexts[k]; return kept })
	if len(removed) > 0 {
		c.mark(obs.TruncatedFieldContexts)
		for _, k := range removed {
			c.addKey("contexts." + k)
		}
	}
}

func boundRequest(c *cuts, r *RequestInfo) {
	if r == nil {
		return
	}
	r.Method = c.str(obs.TruncatedFieldRequestMethod, r.Method, MaxRequestMethodLength)
	r.URL = c.str(obs.TruncatedFieldRequestURL, r.URL, MaxRequestURLLength)
	r.QueryString = c.str(obs.TruncatedFieldRequestQueryString, r.QueryString, MaxRequestQueryStringLength)
	r.Headers = boundStringMap(c, obs.TruncatedFieldRequestHeaders, r.Headers, MaxRequestHeaders, MaxRequestHeaderKeyLength, MaxRequestHeaderValueLength)
	r.Env = boundStringMap(c, obs.TruncatedFieldRequestEnv, r.Env, MaxRequestEnv, MaxRequestEnvKeyLength, MaxRequestEnvValueLength)
	if r.Data != nil {
		r.Data = c.nulDeep(obs.TruncatedFieldRequestData, r.Data)
		if jsonSize(r.Data) > MaxRequestDataSize {
			c.mark(obs.TruncatedFieldRequestData)
			r.Data = nil
		}
	}
}

// boundStringMap is the headers/env rule: an invalid key is dropped, a value is truncated, and entries
// past maxEntries are dropped lexically largest first. A NUL-stripped key collides the way a tag key
// does: the key the client sent wins, then the lexically first original key (see boundTags).
func boundStringMap(c *cuts, field string, m map[string]string, maxEntries, maxKey, maxValue int) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for _, k := range sortedKeys(m) {
		clean := c.nul(field, k)
		if n := utf8.RuneCountInString(clean); n < 1 || n > maxKey {
			c.mark(field)
			continue
		}
		if clean != k {
			if _, sent := m[clean]; sent {
				continue
			}
		}
		if _, taken := out[clean]; taken {
			continue
		}
		out[clean] = c.str(field, m[k], maxValue)
	}
	if keys := sortedKeys(out); len(keys) > maxEntries {
		c.mark(field)
		for _, k := range keys[maxEntries:] {
			delete(out, k)
		}
	}
	return out
}

// boundBreadcrumbs keeps the LAST MaxBreadcrumbs (the most recent: Sentry's list is oldest to newest),
// bounds each crumb, then drops the oldest while the list is over its serialized byte total.
func boundBreadcrumbs(c *cuts, p *ErrorPayload) {
	if len(p.Breadcrumbs) == 0 {
		return
	}
	if len(p.Breadcrumbs) > MaxBreadcrumbs {
		c.mark(obs.TruncatedFieldBreadcrumbs)
		p.Breadcrumbs = p.Breadcrumbs[len(p.Breadcrumbs)-MaxBreadcrumbs:]
	}
	for i := range p.Breadcrumbs {
		b := &p.Breadcrumbs[i]
		b.Type = c.str(obs.TruncatedFieldBreadcrumbsType, b.Type, MaxBreadcrumbTypeLength)
		b.Category = c.str(obs.TruncatedFieldBreadcrumbsCategory, b.Category, MaxBreadcrumbCategoryLength)
		b.Message = c.str(obs.TruncatedFieldBreadcrumbsMessage, b.Message, MaxBreadcrumbMessageLength)
		if !validLevels[b.Level] {
			c.mark(obs.TruncatedFieldBreadcrumbsLevel)
			b.Level = ""
		}
		if b.Data != nil {
			b.Data = c.nulMap(obs.TruncatedFieldBreadcrumbsData, b.Data)
			if jsonSize(b.Data) > MaxBreadcrumbDataSize {
				c.mark(obs.TruncatedFieldBreadcrumbsData)
				b.Data = nil
			}
		}
	}
	// Each crumb's size once and a running total, as the contexts and metadata shrinks do: re-marshalling
	// the whole list per dropped crumb cost 300-440 ms on one 822 KB body (review finding
	// esv2-rev2-breadcrumbs-byte-loop-quadratic).
	sizes := make([]int, len(p.Breadcrumbs))
	body := 0
	for i := range p.Breadcrumbs {
		sizes[i] = jsonSize(p.Breadcrumbs[i])
		body += sizes[i]
	}
	for len(p.Breadcrumbs) > 0 && arraySize(body, len(p.Breadcrumbs)) > MaxBreadcrumbsSize {
		c.mark(obs.TruncatedFieldBreadcrumbs)
		body -= sizes[0]
		sizes = sizes[1:]
		p.Breadcrumbs = p.Breadcrumbs[1:]
	}
}

// boundException keeps the LAST MaxExceptionValues (the raised exception is last) and bounds each value
// and its chain frames. A chain frame is repaired, never rejected: it is not a fingerprint input, and
// a v2 field never costs the event (spec S18).
func boundException(c *cuts, e *ExceptionInterface) {
	if e == nil {
		return
	}
	if len(e.Values) > MaxExceptionValues {
		c.mark(obs.TruncatedFieldException)
		e.Values = e.Values[len(e.Values)-MaxExceptionValues:]
	}
	for i := range e.Values {
		v := &e.Values[i]
		v.Type = c.str(obs.TruncatedFieldExceptionType, v.Type, MaxExceptionTypeLength)
		v.Value = c.str(obs.TruncatedFieldExceptionValue, v.Value, MaxExceptionValueLength)
		if v.Mechanism != nil {
			v.Mechanism.Type = c.str(obs.TruncatedFieldExceptionMechanism, v.Mechanism.Type, MaxMechanismTypeLength)
		}
		if len(v.Stacktrace) > MaxExceptionFrames {
			// The FIRST frames are kept: the crashing end, as the JS SDK keeps the innermost frames.
			c.mark(obs.TruncatedFieldExceptionStacktrace)
			v.Stacktrace = v.Stacktrace[:MaxExceptionFrames]
		}
		for j := range v.Stacktrace {
			f := &v.Stacktrace[j]
			f.File = c.str(obs.TruncatedFieldExceptionStacktrace, f.File, MaxFilePathLength)
			f.Function = c.str(obs.TruncatedFieldExceptionStacktrace, f.Function, MaxFunctionNameLength)
			if f.InApp && f.File == "" {
				// Would fail stack_frame.file_required; the frame and its module/abs_path are kept.
				c.mark(obs.TruncatedFieldExceptionStacktrace)
				f.InApp = false
			}
			if f.Column < 0 {
				c.mark(obs.TruncatedFieldExceptionStacktrace)
				f.Column = 0
			}
			boundFrameV2(c, f)
		}
	}
}

// boundFrameV2 bounds the five v2 frame fields, on top-level and chain frames alike. File and function
// of a top-level frame are NOT touched: they are fingerprint inputs (spec S4).
func boundFrameV2(c *cuts, f *StackFrame) {
	f.Module = c.str(obs.TruncatedFieldStacktraceModule, f.Module, MaxFrameModuleLength)
	f.AbsPath = c.str(obs.TruncatedFieldStacktraceAbsPath, f.AbsPath, MaxFrameAbsPathLength)
	f.ContextLine = c.str(obs.TruncatedFieldStacktraceContext, f.ContextLine, MaxFrameContextLineLength)
	if len(f.PreContext) > MaxFrameContextLines {
		// The lines nearest the context line are kept: the last of pre, the first of post.
		c.mark(obs.TruncatedFieldStacktraceContext)
		f.PreContext = f.PreContext[len(f.PreContext)-MaxFrameContextLines:]
	}
	if len(f.PostContext) > MaxFrameContextLines {
		c.mark(obs.TruncatedFieldStacktraceContext)
		f.PostContext = f.PostContext[:MaxFrameContextLines]
	}
	for i := range f.PreContext {
		f.PreContext[i] = c.str(obs.TruncatedFieldStacktraceContext, f.PreContext[i], MaxFrameContextLineLength)
	}
	for i := range f.PostContext {
		f.PostContext[i] = c.str(obs.TruncatedFieldStacktraceContext, f.PostContext[i], MaxFrameContextLineLength)
	}
}

// fieldList is server_truncated's value: the marked fields in vocabulary order. []interface{}, not
// []string: structpb.NewStruct (mapping.MapPayloadToEvent) accepts only the shapes encoding/json decodes
// into.
func (c *cuts) fieldList() []interface{} {
	fields := []interface{}{}
	for _, f := range obs.TruncatedFieldVocabulary() {
		if c.fields[f] {
			fields = append(fields, f)
		}
	}
	return fields
}

// writeMarkers (re)writes both server-owned keys from c, and removes a key with nothing to say, so a
// client value under either key never survives the step.
func writeMarkers(c *cuts, p *ErrorPayload) {
	if len(c.fields) == 0 {
		if p.Metadata != nil {
			delete(p.Metadata, MetadataKeyServerTruncated)
			delete(p.Metadata, MetadataKeyServerTruncatedKeys)
		}
		return
	}
	if p.Metadata == nil {
		p.Metadata = map[string]interface{}{}
	}
	p.Metadata[MetadataKeyServerTruncated] = c.fieldList()
	if c.keys.empty() {
		delete(p.Metadata, MetadataKeyServerTruncatedKeys)
		return
	}
	p.Metadata[MetadataKeyServerTruncatedKeys] = c.keys.value()
}

// shrinkMetadata enforces MaxMetadataSize by removing whole top-level keys: while over, the key whose
// `"key":value` is largest goes first (ties to the lexically smallest key), everything non-exempt
// before any exempt key, and the two markers never. `metadata` is added to server_truncated BEFORE the
// final measurement, so the markers always fit.
//
// The removal order is fixed up front (an entry's size never changes), and the map's size is kept as a
// running total: one sort, no re-marshal per removal (spec §4.2; review finding
// esv2-rev-bounds-quadratic-dos).
func shrinkMetadata(c *cuts, p *ErrorPayload) {
	if p.Metadata == nil || jsonSize(p.Metadata) <= MaxMetadataSize {
		return
	}
	c.mark(obs.TruncatedFieldMetadata)
	candidates := make([]sizedKey, 0, len(p.Metadata))
	body := 0
	for k, v := range p.Metadata {
		if k != MetadataKeyServerTruncated && k != MetadataKeyServerTruncatedKeys {
			s := entrySize(k, v)
			candidates = append(candidates, sizedKey{key: k, size: s, last: metadataExemptKeys[k]})
			body += s
		}
	}
	sortVictims(candidates)
	// Only the server_truncated_keys entry grows during the loop, and removedKeys measures itself.
	fieldsSize := entrySize(MetadataKeyServerTruncated, c.fieldList())
	keysNameSize := jsonSize(MetadataKeyServerTruncatedKeys) + 1
	remaining := len(candidates)
	// The size writeMarkers will leave: the remaining entries, server_truncated (always written, since
	// `metadata` is now marked) and server_truncated_keys once it has an entry. Both markers count
	// inside the bound, and being capped they can never force an exempt key out on their own.
	size := func() int {
		if c.keys.empty() {
			return objectSize(body+fieldsSize, remaining+1)
		}
		return objectSize(body+fieldsSize+keysNameSize+c.keys.jsonSize(), remaining+2)
	}
	for _, v := range candidates {
		if size() <= MaxMetadataSize {
			break
		}
		delete(p.Metadata, v.key)
		body -= v.size
		remaining--
		c.addKey(v.key)
	}
	writeMarkers(c, p)
}

// sizedKey is one removal candidate of a shrink: its `"key":value` byte size, and whether it belongs to
// the set removed only after everything else (well-known contexts, exempt metadata keys).
type sizedKey struct {
	key  string
	size int
	last bool
}

// sortVictims orders the candidates the way the shrinks remove them: `last` ones after the others, the
// largest first, ties to the lexically smallest key.
func sortVictims(v []sizedKey) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].last != v[j].last {
			return !v[i].last
		}
		if v[i].size != v[j].size {
			return v[i].size > v[j].size
		}
		return v[i].key < v[j].key
	})
}

// ContextFits reports whether p.Contexts, with v inserted under name, still fits MaxContexts and
// MaxContextsSize, measured exactly as boundContexts measures it (entrySize / objectSize), so the two
// cannot drift. Enrichment inserts a derived context only when this holds, so a derived context can
// never make the bounds step evict a context the SDK sent (sentinel-event-enrichment spec §6.3).
func ContextFits(p *ErrorPayload, name string, v map[string]interface{}) bool {
	body, n := entrySize(name, v), 1
	for k, c := range p.Contexts {
		if k == name {
			continue
		}
		body += entrySize(k, c)
		n++
	}
	return n <= MaxContexts && objectSize(body, n) <= MaxContextsSize
}

// objectSize is the json.Marshal size of an object with n entries whose `"key":value` sizes sum to
// body: the braces and one comma between entries.
func objectSize(body, n int) int {
	if n > 1 {
		return body + 2 + n - 1
	}
	return body + 2
}

// arraySize is the json.Marshal size of an array of n elements whose sizes sum to body; the
// punctuation is the same as an object's.
func arraySize(body, n int) int { return objectSize(body, n) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var overflowEntryRe = regexp.MustCompile(`^\+(\d+) more$`)

// MarkServerTruncated records a removal made AFTER the bounds step (the publish-budget shedder, spec §4.7): it
// adds field to metadata.server_truncated (vocabulary order, no duplicates) and appends keys to
// metadata.server_truncated_keys under the bounds step's own cap (MaxServerTruncatedKeys entries of at most
// MaxServerTruncatedKeyBytes, then one "+N more", whose N is read back from an existing list). It never removes a
// key from either marker.
func MarkServerTruncated(p *ErrorPayload, field string, keys ...string) {
	if p.Metadata == nil {
		p.Metadata = map[string]interface{}{}
	}
	have := map[string]bool{field: true}
	for _, f := range markerStrings(p.Metadata[MetadataKeyServerTruncated]) {
		have[f] = true
	}
	fields := []interface{}{}
	for _, f := range obs.TruncatedFieldVocabulary() {
		if have[f] {
			fields = append(fields, f)
		}
	}
	p.Metadata[MetadataKeyServerTruncated] = fields
	if len(keys) == 0 {
		return
	}
	var rk removedKeys
	existing := markerStrings(p.Metadata[MetadataKeyServerTruncatedKeys])
	for i, e := range existing {
		if m := overflowEntryRe.FindStringSubmatch(e); m != nil && i == len(existing)-1 {
			rk.overflow, _ = strconv.Atoi(m[1])
			continue
		}
		rk.add(e)
	}
	for _, k := range keys {
		rk.add(strings.ReplaceAll(k, "\x00", ""))
	}
	p.Metadata[MetadataKeyServerTruncatedKeys] = rk.value()
}

// markerStrings reads a marker value back: the bounds step writes []interface{} of strings.
func markerStrings(v interface{}) []string {
	var out []string
	switch t := v.(type) {
	case []interface{}:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, t...)
	}
	return out
}
