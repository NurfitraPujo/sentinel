package sentinel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Client-side bounds (spec §3.4): the SDK applies the server's own rules to
// every field it emits so the server's bounds step has nothing left to cut
// and writes no marker, and protovalidate's rules to the pre-v2 fields it
// does not look at, so nothing the SDK emits is ever rejected with 400.
//
// These constants are a deliberate copy of apps/ingestor-go/validation/bounds.go's
// (B5: sdk-go is a separate module with no dependency on the ingestor). If
// the server's rules change, this file must change in the same PR.
const (
	sdkMaxServerNameLength  = 256
	sdkMaxTransactionLength = 200
	sdkMaxMessageLength     = 10000
	sdkMaxErrorClassLength  = 255
	sdkMaxFingerprintLength = 64
	sdkMaxReleaseVersion    = 100
	sdkMaxTraceIDLength     = 64
	sdkMaxSpanIDLength      = 64
	sdkMaxStacktraceFrames  = 100
	sdkMaxSDKNameLength     = 128
	sdkMaxSDKVersionLength  = 64
	sdkMaxUserIDBytes       = 255
	sdkMaxUsernameLength    = 128
	sdkMaxUserEmailLength   = 254
	sdkMaxUserIPLength      = 45
	sdkMaxTags              = 50
	sdkMaxTagKeyLength      = 32
	sdkMaxTagValueLength    = 200
	sdkMaxContexts          = 32
	sdkMaxContextKeyLength  = 64
	// sdkMaxContextsSize and sdkMaxMetadataSize are the server's 65,536-byte
	// bounds less a 4,096-byte margin (spec §3.5, "The margin"): the SDK keeps
	// up to 4 KiB less of each than the server would, so the server's own
	// shrink never runs on an SDK-bounded event.
	sdkMaxContextsSize          = 65536 - 4096
	sdkMaxMetadataSize          = 65536 - 4096
	sdkMaxRequestMethodLength   = 16
	sdkMaxRequestURLLength      = 2048
	sdkMaxRequestQueryLength    = 4096
	sdkMaxRequestHeaders        = 64
	sdkMaxRequestHeaderKeyLen   = 256
	sdkMaxRequestHeaderValueLen = 4096
	sdkMaxRequestEnv            = 64
	sdkMaxRequestEnvKeyLen      = 256
	sdkMaxRequestEnvValueLen    = 1024
	sdkMaxRequestDataSize       = 16 * 1024
	sdkMaxBreadcrumbsSize       = 128 * 1024
	sdkMaxBreadcrumbTypeLength  = 64
	sdkMaxBreadcrumbCategoryLen = 128
	sdkMaxBreadcrumbMessageLen  = 8192
	sdkMaxBreadcrumbDataSize    = 4096

	sdkMetadataKeyTruncated     = "truncated"
	sdkMetadataKeyTruncatedKeys = "truncated_keys"
	sdkMaxTruncatedKeys         = 50
	sdkMaxTruncatedKeyBytes     = 128
)

// sdkMetadataExemptKeys mirrors apps/ingestor-go/validation/bounds.go's
// metadataExemptKeys: every metadata key any SDK or server code reads by
// name (spec §4.2), plus this SDK's own truncated/truncated_keys markers.
var sdkMetadataExemptKeys = map[string]bool{
	"user_id": true, "userId": true, "user": true,
	"tenant_id": true, "tenantId": true, "organization_id": true,
	"trace_id": true, "traceId": true, "trace-id": true,
	"span_id": true, "spanId": true, "span-id": true,
	"request_id": true, "requestId": true, "request-id": true,
	"release_version": true, sdkMetadataKeyTruncated: true, sdkMetadataKeyTruncatedKeys: true,
}

// sdkWellKnownContexts mirrors the ingestor's wellKnownContexts: removed last
// when contexts is over its bound.
var sdkWellKnownContexts = map[string]bool{
	"device": true, "os": true, "app": true, "runtime": true, "browser": true, "culture": true,
}

var sdkTagKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,32}$`)

var sdkValidLevels = map[string]bool{"": true, LevelFatal: true, LevelError: true, LevelWarning: true, LevelInfo: true, LevelDebug: true}

// validUTF8 makes s valid UTF-8 exactly as json.Marshal makes it: every
// invalid byte becomes one U+FFFD (json.Marshal / encoding/json's
// appendString writes one replacement per invalid byte, unlike
// strings.ToValidUTF8, which collapses a whole invalid run into one). A
// valid U+FFFD already in s is left as is.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
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

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func overRunes(s string, max int) bool { return utf8.RuneCountInString(s) > max }

func jsonSize(v interface{}) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func entrySize(k string, v interface{}) int {
	return jsonSize(k) + 1 + jsonSize(v)
}

func objectSize(body, n int) int {
	if n > 1 {
		return body + 2 + n - 1
	}
	return body + 2
}

func arraySize(body, n int) int { return objectSize(body, n) }

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedContextKeys(m map[string]map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sizedKey is one removal candidate: its "key":value byte size, and whether
// it belongs to the set removed only after everything else (well-known
// contexts, exempt metadata keys).
type sizedKey struct {
	key  string
	size int
	last bool
}

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

// validTag reports whether key and value could be a tags entry: key matches
// ^[a-zA-Z0-9_.:-]{1,32}$ and value (with U+0000 already removed by the
// caller) is non-empty, at most 200 code points, and holds no "\n" (spec
// §3.4 tags row; §4.2 rule 4). Shared between WithTag's routing and the
// bounds step's own drop rule.
func validTag(key, value string) bool {
	if !sdkTagKeyPattern.MatchString(key) {
		return false
	}
	if value == "" || strings.Contains(value, "\n") {
		return false
	}
	return utf8.RuneCountInString(value) <= sdkMaxTagValueLength
}

// sdkMarker accumulates what the transport's bounds step removed, for the
// SDK's own truncated / truncated_keys metadata marker (spec §3.4, "The
// SDK's marker").
type sdkMarker struct {
	contexts bool
	metadata bool
	keys     []string
	overflow int
}

func (m *sdkMarker) addKey(k string) {
	if len(m.keys) == sdkMaxTruncatedKeys {
		m.overflow++
		return
	}
	m.keys = append(m.keys, truncateBytes(strings.ReplaceAll(k, "\x00", ""), sdkMaxTruncatedKeyBytes))
}

func (m *sdkMarker) empty() bool { return !m.contexts && !m.metadata && len(m.keys) == 0 }

func (m *sdkMarker) truncatedValue() []interface{} {
	var fields []interface{}
	if m.contexts {
		fields = append(fields, "contexts")
	}
	if m.metadata {
		fields = append(fields, "metadata")
	}
	return fields
}

func (m *sdkMarker) truncatedKeysValue() []interface{} {
	if len(m.keys) == 0 && m.overflow == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(m.keys)+1)
	for _, k := range m.keys {
		out = append(out, k)
	}
	if m.overflow > 0 {
		out = append(out, fmt.Sprintf("+%d more", m.overflow))
	}
	return out
}

// applyMarker writes m's two keys into metadata (replacing whatever a caller
// put under either), or removes a caller's value under either key when m is
// empty (spec §3.4).
func (m *sdkMarker) applyMarker(metadata map[string]interface{}) map[string]interface{} {
	if m.empty() {
		if metadata == nil {
			return metadata
		}
		if _, ok := metadata[sdkMetadataKeyTruncated]; !ok {
			if _, ok2 := metadata[sdkMetadataKeyTruncatedKeys]; !ok2 {
				return metadata
			}
		}
		out := make(map[string]interface{}, len(metadata))
		for k, v := range metadata {
			if k == sdkMetadataKeyTruncated || k == sdkMetadataKeyTruncatedKeys {
				continue
			}
			out[k] = v
		}
		return out
	}
	out := make(map[string]interface{}, len(metadata)+2)
	for k, v := range metadata {
		out[k] = v
	}
	out[sdkMetadataKeyTruncated] = m.truncatedValue()
	if v := m.truncatedKeysValue(); v != nil {
		out[sdkMetadataKeyTruncatedKeys] = v
	} else {
		delete(out, sdkMetadataKeyTruncatedKeys)
	}
	return out
}

// nulString removes every U+0000 from s.
func nulString(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// nulDeep strips U+0000 from every key and string value of a decoded JSON
// value, at any depth (mirrors the ingestor's cuts.nulDeep/nulMap).
func nulDeep(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return nulString(t)
	case map[string]interface{}:
		return nulMap(t)
	case []interface{}:
		for i := range t {
			t[i] = nulDeep(t[i])
		}
		return t
	default:
		return v
	}
}

func nulMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	var renamed []string
	for k, v := range m {
		m[k] = nulDeep(v)
		if strings.IndexByte(k, 0) >= 0 {
			renamed = append(renamed, k)
		}
	}
	sort.Strings(renamed)
	for _, k := range renamed {
		v := m[k]
		delete(m, k)
		clean := strings.ReplaceAll(k, "\x00", "")
		if _, taken := m[clean]; !taken && clean != "" {
			m[clean] = v
		}
	}
	return m
}

// appendLost mirrors the ingestor's appendLost: a NUL-stripped key that lost
// a collision is recorded only when no entry of that name was kept.
func appendLost(removed, collided []string, kept func(string) bool) []string {
	for _, k := range collided {
		if !kept(k) {
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	out := removed[:0:0]
	var last string
	for i, k := range removed {
		if i == 0 || k != last {
			out = append(out, k)
			last = k
		}
	}
	return out
}

// boundTimestamp is spec §3.4's timestamp row: UTC (removing an offset of 24h
// or more, which time.Time.MarshalJSON refuses), then a year outside
// [0,9999] (which also fails MarshalJSON) becomes "now".
func boundTimestamp(t time.Time) time.Time {
	t = t.UTC()
	if t.Year() < 0 || t.Year() > 9999 {
		return time.Now().UTC()
	}
	return t
}

// boundEventID is spec §3.4's event_id row: empty, over 64 code points, or
// holding a control character becomes a new UUIDv4 - exactly the replacement
// the server makes (service.go: resolveEventID).
func boundEventID(id string) string {
	if id == "" || utf8.RuneCountInString(id) > 64 {
		return newEventID()
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return newEventID()
		}
	}
	return id
}

// boundEnvironment is spec §3.4's environment row.
var environmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,49}$`)
var environmentInvalidRun = regexp.MustCompile(`[^a-z0-9._-]+`)
var environmentLeadingInvalid = regexp.MustCompile(`^[^a-z0-9]+`)

func boundEnvironment(env string) string {
	if environmentPattern.MatchString(env) {
		return env
	}
	e := strings.ToLower(env)
	e = environmentInvalidRun.ReplaceAllString(e, "-")
	e = environmentLeadingInvalid.ReplaceAllString(e, "")
	if len(e) > 50 {
		e = e[:50]
	}
	if e == "" {
		return "development"
	}
	return e
}

var platformPattern = regexp.MustCompile(`^[a-z0-9]+$`)

func boundPlatform(p string) string {
	if platformPattern.MatchString(p) && len(p) <= 50 {
		return p
	}
	return "go"
}

// boundEvent applies every §3.4 row not marked "decoded" to a bounded
// shallow copy of ev, and returns it. The pushed *Event is never written:
// every pointer whose target changes, and every map or slice whose contents
// change, is copied first (spec §3.5 step 1, §8 guarantee 1).
func boundEvent(ev *Event, maxBreadcrumbs int) *Event {
	b := *ev

	b.Level = NormalizeLevel(b.Level)
	b.ServerName = truncateRunes(validUTF8(b.ServerName), sdkMaxServerNameLength)
	b.Transaction = truncateRunes(validUTF8(b.Transaction), sdkMaxTransactionLength)
	b.Message = truncateRunes(validUTF8(b.Message), sdkMaxMessageLength)

	b.ErrorClass = truncateRunes(validUTF8(b.ErrorClass), sdkMaxErrorClassLength)
	if b.ErrorClass == "" {
		b.ErrorClass = "Error"
	}

	if fp := validUTF8(b.Fingerprint); overRunes(fp, sdkMaxFingerprintLength) {
		b.Fingerprint = ""
	} else {
		b.Fingerprint = fp
	}

	b.ReleaseVersion = truncateRunes(validUTF8(b.ReleaseVersion), sdkMaxReleaseVersion)

	if tid := validUTF8(b.TraceID); overRunes(tid, sdkMaxTraceIDLength) {
		b.TraceID = ""
	} else {
		b.TraceID = tid
	}
	if sid := validUTF8(b.SpanID); overRunes(sid, sdkMaxSpanIDLength) {
		b.SpanID = ""
	} else {
		b.SpanID = sid
	}

	b.Platform = boundPlatform(validUTF8(b.Platform))
	b.Environment = boundEnvironment(validUTF8(b.Environment))

	if len(b.Stacktrace) > 0 {
		frames := make([]Frame, len(b.Stacktrace))
		copy(frames, b.Stacktrace)
		if len(frames) > sdkMaxStacktraceFrames {
			frames = frames[:sdkMaxStacktraceFrames]
		}
		for i := range frames {
			frames[i].Function = validUTF8(frames[i].Function)
			frames[i].File = validUTF8(frames[i].File)
			if frames[i].InApp && frames[i].File == "" {
				frames[i].InApp = false
			}
			if frames[i].Line < -(1<<31) || frames[i].Line > (1<<31-1) {
				frames[i].Line = 0
			}
		}
		b.Stacktrace = frames
	}

	b.EventID = boundEventID(validUTF8(b.EventID))
	b.Timestamp = boundTimestamp(b.Timestamp)

	if b.SDK != nil {
		sdk := *b.SDK
		sdk.Name = truncateRunes(validUTF8(sdk.Name), sdkMaxSDKNameLength)
		sdk.Version = truncateRunes(validUTF8(sdk.Version), sdkMaxSDKVersionLength)
		if sdk.Name == "" && sdk.Version == "" {
			b.SDK = nil
		} else {
			b.SDK = &sdk
		}
	}

	if b.User != nil {
		u := *b.User
		if id := validUTF8(u.ID); len(id) > sdkMaxUserIDBytes {
			u.ID = ""
		} else {
			u.ID = id
		}
		u.Username = truncateRunes(validUTF8(u.Username), sdkMaxUsernameLength)
		if e := validUTF8(u.Email); overRunes(e, sdkMaxUserEmailLength) {
			u.Email = ""
		} else {
			u.Email = e
		}
		if ip := validUTF8(u.IPAddress); overRunes(ip, sdkMaxUserIPLength) {
			u.IPAddress = ""
		} else {
			u.IPAddress = ip
		}
		if u == (UserInfo{}) {
			b.User = nil
		} else {
			b.User = &u
		}
	}

	b.Tags = boundTags(b.Tags)

	if b.Request != nil {
		req := *b.Request
		req.Method = truncateRunes(validUTF8(req.Method), sdkMaxRequestMethodLength)
		req.URL = truncateRunes(validUTF8(req.URL), sdkMaxRequestURLLength)
		req.QueryString = truncateRunes(validUTF8(req.QueryString), sdkMaxRequestQueryLength)
		req.Headers = boundStringMapTyped(req.Headers, sdkMaxRequestHeaders, sdkMaxRequestHeaderKeyLen, sdkMaxRequestHeaderValueLen)
		req.Env = boundStringMapTyped(req.Env, sdkMaxRequestEnv, sdkMaxRequestEnvKeyLen, sdkMaxRequestEnvValueLen)
		b.Request = &req
	}

	if len(b.Breadcrumbs) > 0 {
		n := maxBreadcrumbs
		if n <= 0 {
			n = DefaultMaxBreadcrumbs
		} else if n > MaxBreadcrumbsLimit {
			n = MaxBreadcrumbsLimit
		}
		crumbs := b.Breadcrumbs
		if len(crumbs) > n {
			crumbs = crumbs[len(crumbs)-n:]
		}
		out := make([]Breadcrumb, len(crumbs))
		copy(out, crumbs)
		for i := range out {
			out[i].Type = truncateRunes(validUTF8(out[i].Type), sdkMaxBreadcrumbTypeLength)
			out[i].Category = truncateRunes(validUTF8(out[i].Category), sdkMaxBreadcrumbCategoryLen)
			out[i].Message = truncateRunes(validUTF8(out[i].Message), sdkMaxBreadcrumbMessageLen)
			out[i].Level = NormalizeLevel(out[i].Level)
			if out[i].Timestamp.IsZero() || out[i].Timestamp.UTC().Year() < 0 || out[i].Timestamp.UTC().Year() > 9999 {
				out[i].Timestamp = b.Timestamp
			} else {
				out[i].Timestamp = out[i].Timestamp.UTC()
			}
		}
		b.Breadcrumbs = out
	}

	return &b
}

// boundTags is spec §3.4's tags row: U+0000 removed with the server's
// collision rule, PII-filtered, invalid entries dropped, then over 50 keys,
// tenant_id (when present) plus the 49 lexically smallest others are kept.
func boundTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return tags
	}
	var collided []string
	out := make(map[string]string, len(tags))
	for _, k := range sortedStringKeys(tags) {
		clean := nulString(k)
		v := nulString(tags[k])
		if clean != k {
			_, sent := tags[clean]
			_, taken := out[clean]
			if sent || taken {
				collided = append(collided, clean)
				continue
			}
		}
		if isSensitiveKey(clean) {
			if !sdkTagKeyPattern.MatchString(clean) {
				continue
			}
			out[clean] = "[FILTERED]"
			continue
		}
		if !validTag(clean, v) {
			continue
		}
		out[clean] = v
	}
	if len(out) > sdkMaxTags {
		tenant, hasTenant := out["tenant_id"]
		keys := sortedStringKeys(out)
		kept := make(map[string]string, sdkMaxTags)
		if hasTenant {
			kept["tenant_id"] = tenant
		}
		for _, k := range keys {
			if k == "tenant_id" {
				continue
			}
			if len(kept) >= sdkMaxTags {
				break
			}
			kept[k] = out[k]
		}
		out = kept
	}
	return out
}

// decodeMirrorCrumb and decodeMirror mirror the ingestor's decoded shapes for
// the four Struct-typed fields (spec §3.5 step 2): map[string]interface{}
// for metadata and a crumb's data, map[string]map[string]interface{} for
// contexts, interface{} for request.data. Every other field is left
// undeclared: json.Unmarshal skips it, exactly as decoding is not needed for
// fields step 1 already gave the ingestor's own Go type.
type decodeMirrorCrumb struct {
	Data map[string]interface{} `json:"data,omitempty"`
}

type decodeMirrorRequest struct {
	Data interface{} `json:"data,omitempty"`
}

type decodeMirror struct {
	Metadata    map[string]interface{}            `json:"metadata,omitempty"`
	Contexts    map[string]map[string]interface{} `json:"contexts,omitempty"`
	Request     *decodeMirrorRequest              `json:"request,omitempty"`
	Breadcrumbs []decodeMirrorCrumb               `json:"breadcrumbs,omitempty"`
}

// ingestorBreadcrumbShape mirrors the ingestor's Breadcrumb wire shape for
// the 128 KiB list bound: no field is omitempty (spec §3.4 "breadcrumbs"
// row, T28), so an empty crumb still carries every key and a nil-or-empty
// Data marshals as "data":null, exactly as the ingestor re-marshals it.
type ingestorBreadcrumbShape struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	Category  string                 `json:"category"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data"`
}

// boundContextsDecoded is spec §3.4's "contexts" row, run on the decoded
// mirror: U+0000 removed at every depth with the server's collision rule,
// invalid names dropped, then contexts removed - non-well-known first,
// largest first, ties to the smallest name - while over 32 entries or
// 61,440 bytes. Returns the names removed, for the SDK's marker.
func boundContextsDecoded(contexts map[string]map[string]interface{}) (map[string]map[string]interface{}, []string) {
	if len(contexts) == 0 {
		return contexts, nil
	}
	var removed, collided []string
	for _, k := range sortedContextKeys(contexts) {
		v := contexts[k]
		if v != nil {
			v = nulMap(v)
		}
		clean := nulString(k)
		if clean != k {
			delete(contexts, k)
			if _, taken := contexts[clean]; taken {
				collided = append(collided, clean)
				continue
			}
		}
		if n := utf8.RuneCountInString(clean); n < 1 || n > sdkMaxContextKeyLength {
			delete(contexts, clean)
			removed = append(removed, clean)
			continue
		}
		contexts[clean] = v
	}
	candidates := make([]sizedKey, 0, len(contexts))
	body := 0
	for k, v := range contexts {
		s := entrySize(k, v)
		candidates = append(candidates, sizedKey{key: k, size: s, last: sdkWellKnownContexts[k]})
		body += s
	}
	sortVictims(candidates)
	for _, v := range candidates {
		if len(contexts) <= sdkMaxContexts && objectSize(body, len(contexts)) <= sdkMaxContextsSize {
			break
		}
		delete(contexts, v.key)
		body -= v.size
		removed = append(removed, v.key)
	}
	removed = appendLost(removed, collided, func(k string) bool { _, kept := contexts[k]; return kept })
	return contexts, removed
}

// boundMetadataDecoded is spec §3.4's "metadata" row: the server-owned
// markers removed (the SDK writes its own below), then, over 61,440 bytes,
// keys shrunk in the server's order - non-exempt first, largest first, ties
// to the lexically smallest key - one at a time while the object WITH the
// SDK's marker is over the bound. Returns the metadata (with the marker
// applied) and nothing else: the caller supplies the marker built from this
// and the contexts row.
func boundMetadataDecoded(metadata map[string]interface{}, marker *sdkMarker) map[string]interface{} {
	if metadata != nil {
		delete(metadata, sdkMetadataKeyTruncated)
		delete(metadata, sdkMetadataKeyTruncatedKeys)
	}
	// Measured WITH the marker applied, not the raw metadata: a caller
	// arriving here after boundContextsDecoded already set marker.contexts
	// (and added "contexts.*" keys) carries a marker footprint of its own,
	// and that footprint is exactly what the shrink loop below accounts for
	// via fieldsSize/sizeOf. Measuring metadata alone here could take this
	// fast path while the metadata WITH the marker's truncated/truncated_keys
	// fields written in is actually over the bound.
	if metadata == nil || jsonSize(marker.applyMarker(metadata)) <= sdkMaxMetadataSize {
		return marker.applyMarker(metadata)
	}
	marker.metadata = true
	candidates := make([]sizedKey, 0, len(metadata))
	body := 0
	for k, v := range metadata {
		s := entrySize(k, v)
		candidates = append(candidates, sizedKey{key: k, size: s, last: sdkMetadataExemptKeys[k]})
		body += s
	}
	sortVictims(candidates)
	fieldsSize := entrySize(sdkMetadataKeyTruncated, marker.truncatedValue())
	remaining := len(candidates)
	sizeOf := func() int {
		kv := marker.truncatedKeysValue()
		if kv == nil {
			return objectSize(body+fieldsSize, remaining+1)
		}
		return objectSize(body+fieldsSize+entrySize(sdkMetadataKeyTruncatedKeys, kv), remaining+2)
	}
	for _, v := range candidates {
		if sizeOf() <= sdkMaxMetadataSize {
			break
		}
		delete(metadata, v.key)
		body -= v.size
		remaining--
		marker.addKey(v.key)
	}
	return marker.applyMarker(metadata)
}

// boundRequestDataDecoded is spec §3.4's "request.data" row: U+0000 removed
// at every depth, then dropped whole over 16 KiB.
func boundRequestDataDecoded(data interface{}) interface{} {
	if data == nil {
		return nil
	}
	data = nulDeep(data)
	if jsonSize(data) > sdkMaxRequestDataSize {
		return nil
	}
	return data
}

// boundBreadcrumbDataDecoded is spec §3.4's crumb "data" clause: U+0000
// removed at every depth, then dropped whole over 4 KiB.
func boundBreadcrumbDataDecoded(data map[string]interface{}) map[string]interface{} {
	if data == nil {
		return nil
	}
	data = nulMap(data)
	if jsonSize(data) > sdkMaxBreadcrumbDataSize {
		return nil
	}
	return data
}

// boundBreadcrumbListSize trims the oldest crumbs while the list, measured
// on the ingestor's own wire shape (no omitempty, T28), is over 131,072
// bytes. crumbs and data must already be individually bounded.
func boundBreadcrumbListSize(crumbs []Breadcrumb, data []map[string]interface{}) ([]Breadcrumb, []map[string]interface{}) {
	if len(crumbs) == 0 {
		return crumbs, data
	}
	sizes := make([]int, len(crumbs))
	body := 0
	for i := range crumbs {
		shape := ingestorBreadcrumbShape{
			Timestamp: crumbs[i].Timestamp,
			Type:      crumbs[i].Type,
			Category:  crumbs[i].Category,
			Level:     crumbs[i].Level,
			Message:   crumbs[i].Message,
			Data:      data[i],
		}
		sizes[i] = jsonSize(shape)
		body += sizes[i]
	}
	start := 0
	for len(crumbs)-start > 0 && arraySize(body, len(crumbs)-start) > sdkMaxBreadcrumbsSize {
		body -= sizes[start]
		start++
	}
	return crumbs[start:], data[start:]
}

func boundStringMapTyped(m map[string]string, maxEntries, maxKey, maxValue int) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for _, k := range sortedStringKeys(m) {
		clean := nulString(k)
		if n := utf8.RuneCountInString(clean); n < 1 || n > maxKey {
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
		out[clean] = truncateRunes(nulString(m[k]), maxValue)
	}
	if keys := sortedStringKeys(out); len(keys) > maxEntries {
		for _, k := range keys[maxEntries:] {
			delete(out, k)
		}
	}
	return out
}
