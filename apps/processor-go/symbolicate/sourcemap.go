package symbolicate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file is the sourcemap v3 decoder of sentinel-sourcemaps decision 33: the second of the two
// options the contract offers (a fork of github.com/go-sourcemap/sourcemap, or "a small v3 decoder
// -- VLQ + a line/segment index -- exposing raw sources, sourceRoot and the matched generated
// line"). It is written here rather than forked because the library's two behaviours the contract
// forbids (decision 21: `absSource` resolving every source against a base URL, and a lookup that
// walks back past the frame's line) are its whole lookup path; what would remain of a fork is the
// VLQ reader and the segment index, which is what this file is.
//
// What the decoder does NOT do, on purpose:
//
//   - it never resolves `sources` against anything -- the string in the map is the string the frame
//     gets (§4.4 step 6, decision 8), joined to a non-empty `sourceRoot` with exactly one `/`
//     between them and nothing else (JoinSourceRoot);
//   - it keeps no `sourcesContent`: the field is not in the struct json.Unmarshal fills, so the
//     bytes are scanned and dropped in the same call (decision 35 -- nothing in this feature reads
//     it, and it is what makes a parsed map several times its raw size);
//   - it validates the VLQ stream (a malformed run is a parse error) but does NOT bounds-check
//     source and name indexes at parse time. A segment whose index points past `sources` or
//     `names` -- "inputs the upload check does not decode" (decision 19) -- panics at resolve,
//     inside the per-map recover the service wraps around parse AND resolve, and is counted
//     `map_error` exactly like a parse error. That recover is the contractual guard (D7, decision
//     19) and the panicking input is the case §11 feeds it; checking here as well would leave the
//     recover untested by any input the decoder can produce.
//
// Index maps (a top-level `sections` array, sentinel-sourcemaps-index-maps §4.3) are the v3
// spec's second form and are decoded here too: each section's `map` is a flat map decoded by the
// SAME decodeMappings with its OWN `sources` / `sourceRoot` / `names`, and a frame resolves
// through the section whose `offset` is the greatest at-or-before its generated position (the
// column offset applies only to the section's first line -- ECMA-426 §5). The form is decided
// from the BYTES at parse time (the row says nothing -- D12) and cached with the map. What is
// still a parse error: a `url` section (D1 -- nothing fetches it), a nested index map (D6), an
// offset that is negative or not strictly after the previous section's (D5), a section without a
// v3 flat `map`, and a map carrying both `mappings` and `sections` (D5). Zero sections is a
// valid parse that resolves nothing (every frame `no_section`, flow P9). `sourcesContent` and
// `ignoreList` are dropped per section exactly as for a flat map.

// parsedMap is one decoded sourcemap: the raw `sources` / `sourceRoot` / `names` strings and,
// per generated line, the segments in generated-column order. Costed by the ROW's size_bytes in
// the cache, never by its own footprint (§4.4, cache paragraph). For an index map (index true)
// sources/names/lines are empty and `sections` carries one parsedMap per section.
type parsedMap struct {
	sources    []string
	sourceRoot string
	names      []string
	// lines[i] holds generated line i (0-based) in ascending generated-column order, as the
	// mappings string lists them. Lines with no segments are empty slices.
	lines [][]segment

	// index is set when the bytes were the `sections` form; sections is then every section in
	// the file's order, strictly ascending by (line, column) -- the route guarantees it and
	// parseSourcemap re-checks, so section() can binary-search.
	index    bool
	sections []parsedSection
}

// parsedSection is one entry of an index map's `sections`: its 0-based generated offset and the
// flat map decoded from its `map`, with that map's own sources / sourceRoot / names.
type parsedSection struct {
	line, column int32
	pm           *parsedMap
}

// segment is one decoded mappings entry. Fields after genCol are only meaningful when hasSource
// is set (a one-field segment is a generated range with no origin, §4.4 step 5). origLine and
// origCol are the map's 0-based values; the 1-based wire conversion happens in resolve.
type segment struct {
	genCol    int32
	source    int32
	origLine  int32
	origCol   int32
	name      int32 // -1 when the segment carries no name
	hasSource bool
}

// rawMap is exactly the fields of a v3 map this feature reads. `sourcesContent`, `ignoreList`,
// `file` and `debugId` are deliberately absent (decision 35; D4) -- for the index form the same
// struct decodes each section's `map`, so they are dropped per section in the same Unmarshal.
type rawMap struct {
	Version    int          `json:"version"`
	Sources    []string     `json:"sources"`
	SourceRoot string       `json:"sourceRoot"`
	Names      []string     `json:"names"`
	Mappings   *string      `json:"mappings"`
	Sections   []rawSection `json:"sections"`
}

// rawSection is one entry of an index map's `sections` (index-maps §4.3): the 0-based generated
// offset the section starts at, and EITHER an inline `map` OR a `url` -- the latter is refused.
type rawSection struct {
	Offset struct{ Line, Column int32 } `json:"offset"`
	URL    *string                      `json:"url"`
	Map    *rawMap                      `json:"map"`
}

var errNotV3 = errors.New("sourcemap: not a v3 map")

// parseSourcemap decodes raw bytes into a parsedMap, dispatching on the bytes (index-maps §4.3):
// version 3 with a string `mappings` is the flat form; version 3 with a `sections` array is the
// index form; both at once, neither, or another version is errNotV3. Every structural fault --
// not JSON, a malformed VLQ run, a bad section -- is an error; the caller counts it `map_error`.
func parseSourcemap(raw []byte) (*parsedMap, error) {
	var m rawMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("sourcemap: %w", err)
	}
	switch {
	case m.Version != 3:
		return nil, errNotV3
	case m.Mappings != nil && m.Sections != nil:
		return nil, errNotV3 // D5: the two forms are exclusive (the route's R14)
	case m.Mappings != nil:
		return parseFlat(&m)
	case m.Sections != nil:
		return parseIndex(&m)
	}
	return nil, errNotV3
}

// parseFlat is the flat form: `mappings` decoded once, sources / sourceRoot / names kept verbatim.
func parseFlat(m *rawMap) (*parsedMap, error) {
	lines, err := decodeMappings(*m.Mappings)
	if err != nil {
		return nil, err
	}
	pm := &parsedMap{
		sources:    m.Sources,
		sourceRoot: m.SourceRoot,
		names:      m.Names,
		lines:      lines,
	}
	if pm.sources == nil {
		pm.sources = []string{}
	}
	if pm.names == nil {
		pm.names = []string{}
	}
	return pm, nil
}

// parseIndex is the index form: each section is checked (§4.3 step 4) and its `map` decoded by
// parseFlat into a parsedMap of its own. Zero sections is a valid parse -- the route never stores
// one (§4.1 R1-R3), so the case exists only so the parser has no undefined input.
func parseIndex(m *rawMap) (*parsedMap, error) {
	pm := &parsedMap{
		sources:  []string{},
		names:    []string{},
		index:    true,
		sections: make([]parsedSection, 0, len(m.Sections)),
	}
	for i, s := range m.Sections {
		switch {
		case s.URL != nil:
			return nil, fmt.Errorf("sourcemap: section %d: url sections are not supported", i)
		case s.Map == nil:
			return nil, fmt.Errorf("sourcemap: section %d: no map", i)
		case s.Map.Sections != nil:
			return nil, fmt.Errorf("sourcemap: section %d: nested index maps are not supported", i)
		case s.Map.Version != 3 || s.Map.Mappings == nil:
			return nil, fmt.Errorf("sourcemap: section %d: map is not a v3 flat map", i)
		case s.Offset.Line < 0 || s.Offset.Column < 0:
			return nil, fmt.Errorf("sourcemap: section %d: negative offset (%d,%d)", i, s.Offset.Line, s.Offset.Column)
		}
		if i > 0 {
			prev := pm.sections[i-1]
			if !lessPos(prev.line, prev.column, s.Offset.Line, s.Offset.Column) {
				return nil, fmt.Errorf("sourcemap: section %d: offset (%d,%d) is not after section %d's (%d,%d)",
					i, s.Offset.Line, s.Offset.Column, i-1, prev.line, prev.column)
			}
		}
		inner, err := parseFlat(s.Map)
		if err != nil {
			return nil, fmt.Errorf("sourcemap: section %d: %w", i, err)
		}
		pm.sections = append(pm.sections, parsedSection{line: s.Offset.Line, column: s.Offset.Column, pm: inner})
	}
	return pm, nil
}

// lessPos is the lexicographic order on 0-based generated positions: (l1,c1) < (l2,c2).
func lessPos(l1, c1, l2, c2 int32) bool {
	return l1 < l2 || (l1 == l2 && c1 < c2)
}

// decodeMappings splits the mappings string into lines (`;`) and segments (`,`) and decodes each
// segment's VLQ fields. The first field of every segment is relative to the previous segment on
// the SAME line; the other four are relative to the previous segment that carried them, across
// lines -- the v3 spec's running state.
func decodeMappings(mappings string) ([][]segment, error) {
	var (
		lines    [][]segment
		source   int32
		origLine int32
		origCol  int32
		name     int32
		fields   [5]int32
	)
	for lineIdx, line := range strings.Split(mappings, ";") {
		var segs []segment
		var genCol int32
		if line != "" {
			for segIdx, enc := range strings.Split(line, ",") {
				if enc == "" {
					continue
				}
				n, err := decodeVLQ(enc, fields[:])
				if err != nil {
					return nil, fmt.Errorf("sourcemap: line %d segment %d: %w", lineIdx+1, segIdx+1, err)
				}
				switch n {
				case 1, 4, 5:
				default:
					return nil, fmt.Errorf("sourcemap: line %d segment %d: %d fields", lineIdx+1, segIdx+1, n)
				}
				genCol += fields[0]
				seg := segment{genCol: genCol, name: -1}
				if n >= 4 {
					source += fields[1]
					origLine += fields[2]
					origCol += fields[3]
					seg.hasSource = true
					seg.source, seg.origLine, seg.origCol = source, origLine, origCol
					if n == 5 {
						name += fields[4]
						seg.name = name
					}
				}
				segs = append(segs, seg)
			}
		}
		lines = append(lines, segs)
	}
	return lines, nil
}

// base64VLQ is the v3 alphabet; -1 marks a byte that is not in it.
var base64VLQ = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return t
}()

// decodeVLQ decodes one segment's base64 VLQ run into out and returns how many values it held.
// A byte outside the alphabet, a run that ends with the continuation bit set, more values than
// out can hold, or a value that overflows int32 is an error -- the "malformed VLQ" input of
// decision 19, caught here rather than left to panic because it is cheap to detect and the
// error path is the same outcome.
func decodeVLQ(enc string, out []int32) (int, error) {
	n := 0
	var value uint64
	var shift uint
	inRun := false
	for i := 0; i < len(enc); i++ {
		d := base64VLQ[enc[i]]
		if d < 0 {
			return 0, fmt.Errorf("byte %q is not base64", enc[i])
		}
		if shift > 35 {
			return 0, errors.New("vlq value overflows")
		}
		value |= uint64(d&31) << shift
		shift += 5
		inRun = true
		if d&32 != 0 {
			continue
		}
		if n >= len(out) {
			return 0, errors.New("too many fields")
		}
		neg := value&1 == 1
		v := value >> 1
		if v > 1<<31-1 {
			return 0, errors.New("vlq value overflows int32")
		}
		if neg {
			out[n] = -int32(v)
		} else {
			out[n] = int32(v)
		}
		n++
		value, shift, inRun = 0, 0, false
	}
	if inRun {
		return 0, errors.New("vlq run ends mid-value")
	}
	if n == 0 {
		return 0, errors.New("empty segment")
	}
	return n, nil
}

// resolved is one frame's original position, 1-based as the wire is (§4.4 step 6).
type resolved struct {
	file   string
	name   string // "" when the segment carries no name -- the caller keeps the frame's function
	line   int32
	column int32
}

// resolve is §4.4 step 5 under decision 21, for a WIRE position (1-based line and column):
//
//   - on the frame's generated line only -- never the line before it -- the segment with the
//     greatest generated column <= the frame's column (a real browser column almost always sits
//     between segments; an exact match would `no_mapping` most real frames);
//   - a one-field segment at that position is no mapping, and the search does NOT continue
//     backwards to an earlier segment that has a source;
//   - no segment at or before the column, or a line the map does not have, is no mapping.
//
// The 1-based wire is converted here and nowhere else: generated line-1 indexes lines, column-1
// is compared against the map's 0-based generated columns, and the origin's 0-based line/column
// come back +1. The fixture test pins this with positions computed by hand from the map.
//
// It indexes pm.sources / pm.names without a bounds check by design (see the file comment): the
// service's per-map recover is what turns a past-the-end index into `map_error`.
func (pm *parsedMap) resolve(line, column int32) (resolved, bool) {
	if line < 1 || column < 1 || int(line) > len(pm.lines) {
		return resolved{}, false
	}
	segs := pm.lines[line-1]
	genCol := column - 1
	// First segment with genCol > the frame's column; the one before it is the nearest at-or-before.
	lo, hi := 0, len(segs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if segs[mid].genCol <= genCol {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return resolved{}, false
	}
	seg := segs[lo-1]
	if !seg.hasSource {
		return resolved{}, false
	}
	r := resolved{
		file:   JoinSourceRoot(pm.sourceRoot, pm.sources[seg.source]),
		line:   seg.origLine + 1,
		column: seg.origCol + 1,
	}
	if seg.name >= 0 {
		r.name = pm.names[seg.name]
	}
	return r, true
}

// resolveStatus is resolveFrame's three-valued answer, so the service can tell §4.4's
// `no_section` (no section reaches the frame's position -- an index-map fact) from `no_mapping`
// (the map, or the section's map, has nothing for the position) without re-deriving either.
type resolveStatus uint8

const (
	resolveFound resolveStatus = iota
	resolveNoMapping
	resolveNoSection
)

// resolveFrame is resolve for either form of map, on a WIRE position. A flat map is resolve as
// above; an index map (index-maps §4.3 "resolve on an index map") is:
//
//  1. line < 1 or column < 1 → no mapping, as for a flat map (the service never sends column 0;
//     it is `no_column` before any lookup);
//  2. the section whose offset is the greatest <= (line-1, column-1) lexicographically -- none
//     (the frame precedes the first section, or the map has no sections) → no section;
//  3. local line gl - s.line; local column gc - s.column when the frame is ON the offset's line,
//     else gc unchanged (the column offset applies only to the section's first line);
//  4. the section's own resolve on that local position, 1-based again -- the flat rule unchanged,
//     with the section's sources / sourceRoot / names.
//
// A section is never bounded by the NEXT section's offset: a map whose mappings run past it is
// a producer bug the decoder does not police, and the frame resolves through the earlier
// section as every other consumer does.
func (pm *parsedMap) resolveFrame(line, column int32) (resolved, resolveStatus) {
	if !pm.index {
		if r, ok := pm.resolve(line, column); ok {
			return r, resolveFound
		}
		return resolved{}, resolveNoMapping
	}
	if line < 1 || column < 1 {
		return resolved{}, resolveNoMapping
	}
	gl, gc := line-1, column-1
	i, ok := pm.section(gl, gc)
	if !ok {
		return resolved{}, resolveNoSection
	}
	s := pm.sections[i]
	ll := gl - s.line
	lc := gc
	if gl == s.line {
		lc = gc - s.column
	}
	if r, ok := s.pm.resolve(ll+1, lc+1); ok {
		return r, resolveFound
	}
	return resolved{}, resolveNoMapping
}

// section picks the index of the section whose offset is the greatest at-or-before the 0-based
// generated position (gl, gc); false when no section starts at or before it. Binary search over
// the strictly ascending offsets parseIndex verified.
func (pm *parsedMap) section(gl, gc int32) (int, bool) {
	// First section with offset > (gl, gc); the one before it is the nearest at-or-before.
	lo, hi := 0, len(pm.sections)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		s := pm.sections[mid]
		if !lessPos(gl, gc, s.line, s.column) { // s.offset <= (gl, gc)
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return 0, false
	}
	return lo - 1, true
}

// JoinSourceRoot is §4.4 step 6's identity rule (decision 21): an empty sourceRoot leaves source
// verbatim; a non-empty one is joined to it AS STRINGS with exactly one `/` between them -- no
// `..` collapsing, no URL resolution, no base. `webpack:///` + `./src/app/page.tsx` is
// `webpack:///./src/app/page.tsx`; `src` + `../lib/x.ts` is `src/../lib/x.ts`. The result is the
// fingerprint's identity string, so the same map must yield the same string on every pod.
func JoinSourceRoot(sourceRoot, source string) string {
	if sourceRoot == "" {
		return source
	}
	return strings.TrimSuffix(sourceRoot, "/") + "/" + strings.TrimPrefix(source, "/")
}
