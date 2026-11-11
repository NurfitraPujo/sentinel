package sentrycompat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// ErrInvalidEvent is MapEvent's only failure: the payload is not a JSON object.
var ErrInvalidEvent = errors.New("invalid event payload")

// Source is the value MapEvent writes to ErrorPayload.Source.
const Source = "sentry-compat"

var (
	platformRe    = regexp.MustCompile(`^[a-z0-9]+$`)
	environmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,49}$`)
)

type raw = json.RawMessage

// MapEvent maps one Sentry event JSON onto a validation.ErrorPayload that IngestAt accepts (spec §5). It fails
// only when payload is not a JSON object. project_key and project_id are left empty: identity comes from the key
// (Config.Scope).
//
// Decoding discipline (D-a..D-d): every field is decoded on its own and a field of the wrong JSON type is
// treated as absent; free-form values come only from encoding/json into interface{}; strings carry no U+0000
// from the moment they are decoded (event_id excepted); every protovalidate rule is satisfied by construction.
func MapEvent(payload []byte, env EnvelopeHeader) (*validation.ErrorPayload, error) {
	var f map[string]raw
	if err := json.Unmarshal(payload, &f); err != nil || f == nil {
		return nil, ErrInvalidEvent
	}
	p := &validation.ErrorPayload{Source: Source}

	// M1
	if id, ok := rawString(f["event_id"]); ok && id != "" {
		p.EventID = id
	} else {
		p.EventID = env.EventID
	}
	// M2
	p.Timestamp = parseSentryTime(f["timestamp"])

	// M14 (needed by M3)
	p.SDK = mapSDK(f["sdk"], env.SDK)

	// M3
	plat := str(f["platform"])
	if !platformRe.MatchString(plat) || len(plat) > 50 {
		plat = "other"
	}
	if plat == "other" && p.SDK != nil && strings.HasPrefix(p.SDK.Name, "sentry.dart") {
		plat = "dart"
	}
	p.Platform = plat

	// M4
	p.Level = mapLevel(f["level"])
	// M5
	p.Logger = str(f["logger"])
	p.ServerName = str(f["server_name"])
	p.Transaction = str(f["transaction"])
	p.Dist = str(f["dist"])
	// M6
	p.ReleaseVersion = truncRunes(str(f["release"]), 100)
	// M7
	e := strings.ToLower(strings.TrimSpace(str(f["environment"])))
	if !environmentRe.MatchString(e) {
		e = "production"
	}
	p.Environment = e

	msg := messageText(f)

	// M9 / E1-E4 / S1-S3 / M10
	values := mapExceptionValues(f["exception"])
	if len(values) > 0 {
		p.Exception = &validation.ExceptionInterface{Values: values}
		last := values[len(values)-1]
		t := strings.TrimSpace(last.Type)
		if t == "" {
			t = firstLine(last.Value)
		}
		if t == "" {
			t = firstLine(msg)
		}
		if t == "" {
			t = "UnknownError"
		}
		p.ErrorClass = truncRunes(t, 255)
		if last.Value != "" {
			p.Message = truncRunes(last.Value, 10000)
		} else {
			p.Message = truncRunes(msg, 10000)
		}
		if len(last.Stacktrace) > 0 {
			p.Stacktrace = append([]validation.StackFrame(nil), last.Stacktrace...)
		}
	} else {
		p.Message = truncRunes(msg, 10000)
		if c := truncRunes(firstLine(msg), 255); c != "" {
			p.ErrorClass = c
		} else {
			p.ErrorClass = "UnknownError"
		}
	}
	if len(p.Stacktrace) == 0 {
		p.Stacktrace = threadFrames(f["threads"])
	}
	if len(p.Stacktrace) > 100 {
		p.Stacktrace = p.Stacktrace[:100]
	}

	// M12
	p.Breadcrumbs = mapBreadcrumbs(f["breadcrumbs"])
	// M13
	p.User = mapUser(f["user"])
	// M15
	p.Tags = mapTags(f["tags"])
	// M16 / M17
	p.Contexts = mapContexts(f["contexts"])
	p.TraceID, p.SpanID = traceIDs(p.Contexts)
	// M18
	p.Request = mapRequest(f["request"])
	// M19
	p.Metadata = mapExtra(f["extra"])
	// M20
	p.Fingerprint = mapFingerprint(f["fingerprint"])
	return p, nil
}

// ---- decoding helpers (D-a..D-c) ----

// rawString decodes a JSON string as is (no U+0000 stripping); ok is false for anything else.
func rawString(r raw) (string, bool) {
	if len(r) == 0 || r[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return "", false
	}
	return s, true
}

func stripNULs(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// str is a JSON string with U+0000 removed, else "".
func str(r raw) string {
	s, _ := rawString(r)
	return stripNULs(s)
}

// num is a JSON number as float64.
func num(r raw) (float64, bool) {
	if len(r) == 0 || !(r[0] == '-' || (r[0] >= '0' && r[0] <= '9')) {
		return 0, false
	}
	var f float64
	if json.Unmarshal(r, &f) != nil {
		return 0, false
	}
	return f, true
}

// stringify is a JSON string as is, a number as its literal JSON text, a bool as "true"/"false"; absent otherwise.
func stringify(r raw) (string, bool) {
	if len(r) == 0 {
		return "", false
	}
	switch {
	case r[0] == '"':
		s, ok := rawString(r)
		return stripNULs(s), ok
	case r[0] == '-' || (r[0] >= '0' && r[0] <= '9'):
		return string(bytes.TrimSpace(r)), true
	case bytes.Equal(r, []byte("true")):
		return "true", true
	case bytes.Equal(r, []byte("false")):
		return "false", true
	}
	return "", false
}

func isObject(r raw) bool { return len(r) > 0 && r[0] == '{' }
func isArray(r raw) bool  { return len(r) > 0 && r[0] == '[' }

// object decodes a JSON object into its members; ok is false for anything else (null included).
func object(r raw) (map[string]raw, bool) {
	if !isObject(r) {
		return nil, false
	}
	var m map[string]raw
	if json.Unmarshal(r, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

func array(r raw) ([]raw, bool) {
	if !isArray(r) {
		return nil, false
	}
	var a []raw
	if json.Unmarshal(r, &a) != nil {
		return nil, false
	}
	return a, true
}

// valuesOrArray reads `{values: [...]}` or a plain array (Sentry sends both for breadcrumbs, exception, threads).
func valuesOrArray(r raw) []raw {
	if a, ok := array(r); ok {
		return a
	}
	if o, ok := object(r); ok {
		if a, ok := array(o["values"]); ok {
			return a
		}
	}
	return nil
}

// cleanKeys resolves the members of an object whose keys may carry U+0000 (D-c). An exact (NUL-free) key wins
// over a NUL-bearing key that cleans to it; among NUL-bearing keys that clean to the same key the first in sorted
// raw-key order wins. It is the rule validation.boundTags applies, so a result never depends on map order.
func cleanKeys[V any](m map[string]V) []struct {
	Key string
	Val V
} {
	type kv = struct {
		Key string
		Val V
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]kv, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if strings.IndexByte(k, 0) < 0 {
			out = append(out, kv{k, m[k]})
			seen[k] = struct{}{}
		}
	}
	for _, k := range keys {
		if strings.IndexByte(k, 0) < 0 {
			continue
		}
		c := stripNULs(k)
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, kv{c, m[k]})
	}
	return out
}

// freeform decodes any JSON value with encoding/json into interface{} (D-b), removing U+0000 from every string
// and key. ok is false when encoding/json cannot hold the value (a number beyond float64, nesting over 10,000).
func freeform(r raw) (interface{}, bool) {
	if len(r) == 0 {
		return nil, false
	}
	var v interface{}
	if json.Unmarshal(r, &v) != nil {
		return nil, false
	}
	return cleanValue(v), true
}

func cleanValue(v interface{}) interface{} {
	switch x := v.(type) {
	case string:
		return stripNULs(x)
	case []interface{}:
		for i := range x {
			x[i] = cleanValue(x[i])
		}
		return x
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for _, e := range cleanKeys(x) {
			out[e.Key] = cleanValue(e.Val)
		}
		return out
	}
	return v
}

func freeformObject(r raw) (map[string]interface{}, bool) {
	if !isObject(r) {
		return nil, false
	}
	v, ok := freeform(r)
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]interface{})
	return m, ok
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for c := 0; c < n; c++ {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return s[:i]
}

func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// firstLine is the first line of s whose TrimSpace is non-empty, trimmed, or "".
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// ---- event-level rows ----

func mapLevel(r raw) string {
	l := strings.ToLower(strings.TrimSpace(str(r)))
	if l == "log" {
		return "info"
	}
	return l
}

func mapSDK(r raw, fromEnv *validation.SDKInfo) *validation.SDKInfo {
	if o, ok := object(r); ok {
		s := &validation.SDKInfo{Name: str(o["name"]), Version: str(o["version"])}
		if s.Name == "" && s.Version == "" {
			return nil
		}
		return s
	}
	if fromEnv != nil {
		c := *fromEnv
		return &c
	}
	return nil
}

// messageText is M8: the first non-empty of logentry.formatted, logentry.message, message (a string),
// message.formatted, message.message.
func messageText(f map[string]raw) string {
	if le, ok := object(f["logentry"]); ok {
		if s := str(le["formatted"]); s != "" {
			return s
		}
		if s := str(le["message"]); s != "" {
			return s
		}
	}
	if s := str(f["message"]); s != "" {
		return s
	}
	if m, ok := object(f["message"]); ok {
		if s := str(m["formatted"]); s != "" {
			return s
		}
		if s := str(m["message"]); s != "" {
			return s
		}
	}
	return ""
}

// ---- exceptions, frames, threads (§5.3, §5.4) ----

func mapExceptionValues(r raw) []validation.ExceptionValue {
	var out []validation.ExceptionValue
	for _, el := range valuesOrArray(r) {
		o, ok := object(el)
		if !ok {
			continue
		}
		v := validation.ExceptionValue{Type: str(o["type"]), Value: str(o["value"])}
		if m, ok := object(o["mechanism"]); ok {
			mech := &validation.Mechanism{Type: str(m["type"])}
			if h := m["handled"]; bytes.Equal(h, []byte("true")) {
				t := true
				mech.Handled = &t
			} else if bytes.Equal(h, []byte("false")) {
				fl := false
				mech.Handled = &fl
			}
			v.Mechanism = mech
		}
		v.Stacktrace = mapFrames(o["stacktrace"])
		out = append(out, v)
	}
	return out
}

// mapFrames maps stacktrace.frames and reverses the list: Sentry sends frames outermost first, and the
// processor groups on the first in-app frames in array order, so the list must be crash-first (§6.1).
func mapFrames(r raw) []validation.StackFrame {
	st, ok := object(r)
	if !ok {
		return nil
	}
	els, _ := array(st["frames"])
	var frames []validation.StackFrame
	for _, el := range els {
		if o, ok := object(el); ok {
			frames = append(frames, mapFrame(o))
		}
	}
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	return frames
}

func mapFrame(o map[string]raw) validation.StackFrame {
	abs := str(o["abs_path"])
	file := abs
	if file == "" {
		file = str(o["filename"])
	}
	file = truncBytes(file, 512)
	fr := validation.StackFrame{
		File:        file,
		AbsPath:     abs,
		Function:    truncBytes(str(o["function"]), 255),
		Module:      str(o["module"]),
		ContextLine: str(o["context_line"]),
		Line:        frameInt(o["lineno"]),
		Column:      frameInt(o["colno"]),
		PreContext:  strList(o["pre_context"]),
		PostContext: strList(o["post_context"]),
		InApp:       bytes.Equal(o["in_app"], []byte("true")) && file != "",
	}
	return fr
}

func frameInt(r raw) int32 {
	n, ok := num(r)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > math.MaxInt32 {
		return 0
	}
	return int32(math.Trunc(n))
}

func strList(r raw) []string {
	els, ok := array(r)
	if !ok {
		return nil
	}
	out := make([]string, len(els))
	for i, el := range els {
		out[i] = str(el)
	}
	return out
}

// threadFrames is S2: the crashed thread's frames, else the current thread's, else the first thread with any.
func threadFrames(r raw) []validation.StackFrame {
	type cand struct {
		frames           []validation.StackFrame
		crashed, current bool
	}
	var cands []cand
	for _, el := range valuesOrArray(r) {
		o, ok := object(el)
		if !ok {
			continue
		}
		fr := mapFrames(o["stacktrace"])
		if len(fr) == 0 {
			continue
		}
		cands = append(cands, cand{fr, bytes.Equal(o["crashed"], []byte("true")), bytes.Equal(o["current"], []byte("true"))})
	}
	for _, c := range cands {
		if c.crashed {
			return c.frames
		}
	}
	for _, c := range cands {
		if c.current {
			return c.frames
		}
	}
	if len(cands) > 0 {
		return cands[0].frames
	}
	return nil
}

// ---- breadcrumbs, user, tags, contexts, request, extra, fingerprint ----

func mapBreadcrumbs(r raw) []validation.Breadcrumb {
	var out []validation.Breadcrumb
	for _, el := range valuesOrArray(r) {
		o, ok := object(el)
		if !ok {
			continue
		}
		b := validation.Breadcrumb{
			Timestamp: parseSentryTime(o["timestamp"]),
			Type:      str(o["type"]),
			Category:  str(o["category"]),
			Level:     mapLevel(o["level"]),
			Message:   str(o["message"]),
		}
		if d, ok := freeformObject(o["data"]); ok {
			b.Data = d
		}
		out = append(out, b)
	}
	return out
}

func mapUser(r raw) *validation.UserInfo {
	o, ok := object(r)
	if !ok {
		return nil
	}
	u := &validation.UserInfo{Username: str(o["username"]), Email: str(o["email"]), IPAddress: str(o["ip_address"])}
	u.ID, _ = stringify(o["id"])
	if g, ok := object(o["geo"]); ok {
		u.Geo = &validation.GeoInfo{City: str(g["city"]), Region: str(g["region"]), CountryCode: str(g["country_code"]), Subdivision: str(g["subdivision"])}
	}
	if u.ID == "" && u.Username == "" && u.Email == "" && u.IPAddress == "" && u.Geo == nil {
		return nil
	}
	return u
}

// mapTags is M15. Keys are cleaned of U+0000 BEFORE relay_auth is deleted (finding d14-nul-tag-key), in the
// object form and in the pairs form, so `relay_auth\u0000` cannot survive the delete and be cleaned to
// `relay_auth` later by the bounds step.
func mapTags(r raw) map[string]string {
	tags := map[string]string{}
	if o, ok := object(r); ok {
		vals := make(map[string]string, len(o))
		for k, v := range o {
			if s, ok := stringify(v); ok {
				vals[k] = s
			}
		}
		for _, e := range cleanKeys(vals) {
			tags[e.Key] = e.Val
		}
	} else if els, ok := array(r); ok {
		for _, el := range els {
			pair, ok := array(el)
			if !ok || len(pair) != 2 {
				continue
			}
			k, ok := rawString(pair[0])
			if !ok {
				continue
			}
			if s, ok := stringify(pair[1]); ok {
				tags[stripNULs(k)] = s
			}
		}
	}
	delete(tags, TagRelayAuth)
	if len(tags) == 0 {
		return nil
	}
	return tags
}

func mapContexts(r raw) map[string]map[string]interface{} {
	o, ok := object(r)
	if !ok {
		return nil
	}
	decoded := make(map[string]map[string]interface{}, len(o))
	for name, v := range o {
		if m, ok := freeformObject(v); ok {
			decoded[name] = m
		}
	}
	out := map[string]map[string]interface{}{}
	for _, e := range cleanKeys(decoded) {
		out[e.Key] = e.Val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func traceIDs(ctx map[string]map[string]interface{}) (traceID, spanID string) {
	t, ok := ctx["trace"]
	if !ok {
		return "", ""
	}
	pick := func(k string) string {
		s, _ := t[k].(string)
		if n := utf8.RuneCountInString(s); n < 1 || n > 64 {
			return ""
		}
		return s
	}
	return pick("trace_id"), pick("span_id")
}

func mapRequest(r raw) *validation.RequestInfo {
	o, ok := object(r)
	if !ok {
		return nil
	}
	q := &validation.RequestInfo{Method: str(o["method"]), URL: str(o["url"])}
	q.QueryString = mapQueryString(o["query_string"])
	q.Headers = mapHeaders(o["headers"])
	if d, ok := freeform(o["data"]); ok {
		q.Data = d
	}
	if e, ok := object(o["env"]); ok {
		env := map[string]string{}
		for _, kv := range cleanKeys(e) {
			if s, ok := stringify(kv.Val); ok {
				env[kv.Key] = s
			}
		}
		if len(env) > 0 {
			q.Env = env
		}
	}
	if q.Method == "" && q.URL == "" && q.QueryString == "" && q.Headers == nil && q.Data == nil && q.Env == nil {
		return nil
	}
	return q
}

func mapQueryString(r raw) string {
	if s, ok := rawString(r); ok {
		return strings.TrimPrefix(stripNULs(s), "?")
	}
	var parts []string
	add := func(k string, v raw) {
		s, _ := stringify(v)
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(s))
	}
	if o, ok := object(r); ok {
		for _, kv := range cleanKeys(o) {
			add(kv.Key, kv.Val)
		}
	} else if els, ok := array(r); ok {
		for _, el := range els {
			if pair, ok := array(el); ok && len(pair) == 2 {
				if k, ok := stringify(pair[0]); ok {
					add(k, pair[1])
				}
			}
		}
	}
	return strings.Join(parts, "&")
}

func mapHeaders(r raw) map[string]string {
	out := map[string]string{}
	if o, ok := object(r); ok {
		for _, kv := range cleanKeys(o) {
			if s, ok := stringify(kv.Val); ok {
				out[kv.Key] = s
			}
		}
	} else if els, ok := array(r); ok {
		for _, el := range els {
			pair, ok := array(el)
			if !ok || len(pair) != 2 {
				continue
			}
			name, ok := rawString(pair[0])
			if !ok {
				continue
			}
			s, ok := stringify(pair[1])
			if !ok {
				continue
			}
			name = stripNULs(name)
			if prev, dup := out[name]; dup {
				s = prev + ", " + s
			}
			out[name] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapExtra(r raw) map[string]interface{} {
	o, ok := object(r)
	if !ok {
		return nil
	}
	decoded := make(map[string]interface{}, len(o))
	for k, v := range o {
		if x, ok := freeform(v); ok {
			decoded[k] = x
		}
	}
	out := map[string]interface{}{}
	for _, e := range cleanKeys(decoded) {
		out[e.Key] = e.Val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapFingerprint is M20 (D5): "{{ default }}" anywhere means the server computes; otherwise the elements hash to
// 32 hex characters.
func mapFingerprint(r raw) string {
	var elems []string
	if els, ok := array(r); ok {
		for _, el := range els {
			if s, ok := stringify(el); ok {
				elems = append(elems, s)
			}
		}
	} else if s, ok := stringify(r); ok && len(r) > 0 && r[0] == '"' {
		elems = []string{s}
	}
	nonEmpty := false
	for _, e := range elems {
		nonEmpty = nonEmpty || e != ""
	}
	if !nonEmpty {
		// no element, or only empty strings: hashing them would put every such event in one issue
		return ""
	}
	for _, e := range elems {
		squashed := strings.Map(func(c rune) rune {
			if unicode.IsSpace(c) {
				return -1
			}
			return c
		}, e)
		if strings.ToLower(squashed) == "{{default}}" {
			return ""
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(elems, "\n")))
	return hex.EncodeToString(sum[:])[:32]
}

// ---- timestamps (§5.5) ----

// parseSentryTime converts a Sentry timestamp (event or breadcrumb) to UTC; the zero time means "none".
func parseSentryTime(r raw) time.Time {
	if len(r) == 0 {
		return time.Time{}
	}
	switch {
	case r[0] == '-' || (r[0] >= '0' && r[0] <= '9'):
		f, ok := num(r)
		if !ok {
			return time.Time{}
		}
		return unixSeconds(f)
	case r[0] == '"':
		s, _ := rawString(r)
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse("2006-01-02T15:04:05.999999999", s); err == nil {
			return t.UTC()
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return unixSeconds(f)
		}
	}
	return time.Time{}
}

func unixSeconds(f float64) time.Time {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f >= 253402300800 {
		return time.Time{}
	}
	return time.UnixMicro(int64(math.Round(f * 1e6))).UTC()
}
