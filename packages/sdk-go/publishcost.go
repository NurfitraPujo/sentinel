package sentinel

import (
	"bytes"
	"encoding/json"
	"strings"
)

// maxPublishBytes is NATS's 1,048,576-byte max_payload less a 48,576-byte
// fixed allowance for what the ingestor adds beyond the event as sent (spec
// §3.5, "The allowance").
const maxPublishBytes = 1_000_000

// maxBatchBodyBytes is the ingestor's maxBatchBodyBytes: the batch endpoint's
// body cap, which the SDK now always posts to (spec §3.5 step 4, T38).
const maxBatchBodyBytes = 5_242_880

// maxBatchEvents is the ingestor's maxBatchSize.
const maxBatchEvents = 500

// escOffJSON encodes v with HTML escaping off, the trailing newline
// json.Encoder.Encode writes removed - the same encoder settings publishCost
// uses throughout (spec §3.5 part 1, and the rc14 note's jsonStringExcess).
func escOffJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	out := buf.Bytes()
	return bytes.TrimSuffix(out, []byte("\n"))
}

// needsEscape is jsonStringExcess's fast-path guard: whether s has any byte
// below 0x20, a '"', a '\', or the sequence for U+2028/U+2029 - the only
// characters an escaping-off encoder still escapes beyond what protobuf's
// UTF-8 bytes would cost (spec §3, the rc14 note).
func needsEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			return true
		}
	}
	return strings.ContainsRune(s, ' ') || strings.ContainsRune(s, ' ')
}

// jsonStringExcess is the bytes an escaping-off encoder adds to s beyond its
// quotes: len(escOffJSON(s)) - 2 - len(s). It is exact by construction (it
// runs the same encoder on s alone), and 0 on the fast path (spec §3, the
// rc14 note: sdkgo-rc14-size-model-escape-overcount-sheds-publishable).
func jsonStringExcess(s string) int {
	if !needsEscape(s) {
		return 0
	}
	encoded := escOffJSON(s)
	return len(encoded) - 2 - len(s)
}

// vl is the length, in bytes, of n encoded as a protobuf varint.
func vl(n int) int {
	switch {
	case n < 0:
		return 10
	case n < 1<<7:
		return 1
	case n < 1<<14:
		return 2
	case n < 1<<21:
		return 3
	case n < 1<<28:
		return 4
	case n < 1<<35:
		return 5
	default:
		return 10
	}
}

// ld is a length-delimited protobuf field of n bytes: its tag is counted by
// the caller (every tag in google/protobuf/struct.proto is one byte, spec
// §3.5).
func ld(n int) int { return 1 + vl(n) + n }

// V is the protobuf size of a decoded value v (google.protobuf.Value): 2 for
// null or a bool, 9 for a number (an 8-byte double plus its 1-byte tag),
// ld(len(s)) for a string's UTF-8 bytes, ld of a list's summed elements, and
// ld(S(m)) for an object (spec §3.5).
func V(v interface{}) int {
	switch t := v.(type) {
	case nil:
		return 2
	case bool:
		return 2
	case float64:
		return 9
	case string:
		return ld(len(t))
	case []interface{}:
		sum := 0
		for _, e := range t {
			sum += ld(V(e))
		}
		return ld(sum)
	case map[string]interface{}:
		return ld(S(t))
	default:
		return 0
	}
}

// S is a Struct's size: the sum, over its entries, of a map entry (the key,
// then the Value) as one length-delimited message (spec §3.5).
func S(m map[string]interface{}) int {
	sum := 0
	for k, v := range m {
		sum += ld(ld(len(k)) + ld(V(v)))
	}
	return sum
}

// publishCost is the protobuf size of the event exactly as the transport
// sends it - b's typed fields and m's four Struct-typed fields, which the
// transport's decoded-bounds step (§3.5 step 3) made equal to what b sends -
// estimated from above (spec §3.5). It reads m and decodes nothing, so it
// has no error to return.
func publishCost(b *Event, m *decodeMirror) int {
	return publishCostPart1(b) + publishCostPart2(m) + publishCostPart3(m) + publishCostPart4(m) + publishCostPart5(m)
}

// publishCostPart1 is the typed fields: a copy of b with Metadata, Contexts,
// Request.Data and every breadcrumb's Data nil'd, encoded with escaping off,
// minus each typed string's escape excess (the rc14 fix, part 1 as amended),
// plus 6 bytes for each entry of Tags, Request.Headers and Request.Env.
func publishCostPart1(b *Event) int {
	proj := *b
	proj.Metadata = nil
	proj.Contexts = nil
	if proj.Request != nil {
		req := *proj.Request
		req.Data = nil
		proj.Request = &req
	}
	if len(proj.Breadcrumbs) > 0 {
		crumbs := make([]Breadcrumb, len(proj.Breadcrumbs))
		copy(crumbs, proj.Breadcrumbs)
		for i := range crumbs {
			crumbs[i].Data = nil
		}
		proj.Breadcrumbs = crumbs
	}

	n := len(escOffJSON(proj))

	excess := 0
	excess += jsonStringExcess(proj.Level)
	excess += jsonStringExcess(proj.ServerName)
	excess += jsonStringExcess(proj.Transaction)
	excess += jsonStringExcess(proj.Message)
	excess += jsonStringExcess(proj.ErrorClass)
	excess += jsonStringExcess(proj.Fingerprint)
	excess += jsonStringExcess(proj.ReleaseVersion)
	excess += jsonStringExcess(proj.TraceID)
	excess += jsonStringExcess(proj.SpanID)
	excess += jsonStringExcess(proj.Platform)
	excess += jsonStringExcess(proj.Environment)
	excess += jsonStringExcess(proj.EventID)
	excess += jsonStringExcess(proj.ProjectKey)
	if proj.SDK != nil {
		excess += jsonStringExcess(proj.SDK.Name)
		excess += jsonStringExcess(proj.SDK.Version)
	}
	if proj.User != nil {
		excess += jsonStringExcess(proj.User.ID)
		excess += jsonStringExcess(proj.User.Username)
		excess += jsonStringExcess(proj.User.Email)
		excess += jsonStringExcess(proj.User.IPAddress)
	}
	entries := 0
	if proj.Request != nil {
		excess += jsonStringExcess(proj.Request.Method)
		excess += jsonStringExcess(proj.Request.URL)
		excess += jsonStringExcess(proj.Request.QueryString)
		for k, v := range proj.Request.Headers {
			excess += jsonStringExcess(k) + jsonStringExcess(v)
			entries++
		}
		for k, v := range proj.Request.Env {
			excess += jsonStringExcess(k) + jsonStringExcess(v)
			entries++
		}
	}
	for _, f := range proj.Stacktrace {
		excess += jsonStringExcess(f.File)
		excess += jsonStringExcess(f.Function)
	}
	for _, c := range proj.Breadcrumbs {
		excess += jsonStringExcess(c.Type)
		excess += jsonStringExcess(c.Category)
		excess += jsonStringExcess(c.Level)
		excess += jsonStringExcess(c.Message)
	}
	for k, v := range proj.Tags {
		excess += jsonStringExcess(k) + jsonStringExcess(v)
		entries++
	}

	return n - excess + 6*entries
}

func publishCostPart2(m *decodeMirror) int {
	if m == nil || len(m.Metadata) == 0 {
		return 0
	}
	return ld(S(m.Metadata))
}

func publishCostPart3(m *decodeMirror) int {
	if m == nil || len(m.Contexts) == 0 {
		return 0
	}
	sum := 0
	for name, ctx := range m.Contexts {
		e := ld(len(name)) + ld(S(ctx))
		sum += 2 + vl(e) + e
	}
	return sum
}

func publishCostPart4(m *decodeMirror) int {
	if m == nil || m.Request == nil || m.Request.Data == nil {
		return 0
	}
	return ld(V(m.Request.Data))
}

func publishCostPart5(m *decodeMirror) int {
	if m == nil {
		return 0
	}
	sum := 0
	for _, c := range m.Breadcrumbs {
		if c.Data != nil {
			sum += ld(S(c.Data))
		}
	}
	return sum
}
