package spanwire

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// MaxAttributeBytes is D7's cap on one span's stored attributes (marshaled JSON).
const MaxAttributeBytes = 8 << 10

// TruncatedKey is the marker CapAttributes adds when it had to cut: its value lists the keys it shortened or dropped,
// so a reader can tell a value that ended there from one that was cut.
const TruncatedKey = "_truncated"

// CleanString removes NUL bytes, which Postgres rejects in text and jsonb, and invalid UTF-8, then cuts the result
// to at most max bytes on a rune boundary (max <= 0 means no cut).
func CleanString(s string, max int) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if max > 0 && len(s) > max {
		s = s[:max]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// CleanValue applies CleanString to every string inside a decoded JSON value (including map keys).
func CleanValue(v any) any {
	switch x := v.(type) {
	case string:
		return CleanString(x, 0)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[CleanString(k, 0)] = CleanValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CleanValue(e)
		}
		return out
	default:
		return v
	}
}

// CapAttributes returns attrs bounded to max marshaled bytes (D7: "largest values truncated first, marked"). It never
// mutates attrs. Each round it finds the key whose marshaled value is largest and shortens it — a string to half its
// length, any other shape to the string "[truncated]" — until the whole map fits; when only tiny values remain it
// drops keys, largest first. Every key it touched is listed under TruncatedKey.
func CapAttributes(attrs map[string]any, max int) map[string]any {
	if len(attrs) == 0 {
		return attrs
	}
	if size(attrs) <= max {
		return attrs
	}
	out := make(map[string]any, len(attrs)+1)
	for k, v := range attrs {
		out[k] = v
	}
	cut := map[string]struct{}{}
	// Leave room for the marker itself.
	budget := max - 256
	for size(out) > budget {
		keys := make([]string, 0, len(out))
		for k := range out {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic tie-break
		big, bigSize := "", -1
		for _, k := range keys {
			if s := valueSize(out[k]); s > bigSize {
				big, bigSize = k, s
			}
		}
		if big == "" {
			break
		}
		cut[big] = struct{}{}
		switch v := out[big].(type) {
		case string:
			if len(v) > 64 {
				out[big] = CleanString(v[:len(v)/2], 0) + "…"
				continue
			}
			delete(out, big)
		default:
			if bigSize > 16 {
				out[big] = "[truncated]"
				continue
			}
			delete(out, big)
		}
	}
	marked := make([]string, 0, len(cut))
	for k := range cut {
		marked = append(marked, k)
	}
	sort.Strings(marked)
	if len(marked) > 16 {
		marked = marked[:16]
	}
	out[TruncatedKey] = marked
	return out
}

func size(m map[string]any) int {
	b, err := json.Marshal(m)
	if err != nil {
		return 1 << 30
	}
	return len(b)
}

func valueSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 1 << 30
	}
	return len(b)
}
