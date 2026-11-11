package sentrycompat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

var (
	ErrMalformedEnvelope = errors.New("malformed envelope") // wrapped with a reason: fmt.Errorf("%w: …", …)
	ErrTooManyItems      = errors.New("too many envelope items")
)

// EnvelopeHeader is the part of the envelope header the mapper uses. dsn, sent_at, trace and every other key
// are ignored and never logged.
type EnvelopeHeader struct {
	EventID string              // header "event_id" when it is a JSON string, else ""
	SDK     *validation.SDKInfo // header "sdk" {name, version} when an object with a string name or version, else nil
}

// Item is one envelope item as the reader returns it.
type Item struct {
	Type      string // the item header's "type", verbatim
	Payload   []byte // set only for a kept event or transaction item
	Oversized bool   // an event or transaction item over MaxEventItemBytes: read past, not kept
}

type Envelope struct {
	Header EnvelopeHeader
	Items  []Item
}

func malformed(reason string) error { return fmt.Errorf("%w: %s", ErrMalformedEnvelope, reason) }

// ReadEnvelope reads a whole envelope (spec §4.3). Errors from r (the size and corruption errors of §4.1)
// are returned unchanged from every read.
func ReadEnvelope(r io.Reader) (*Envelope, error) {
	br := bufio.NewReader(r)

	line, eof, err := readHeaderLine(br, MaxHeaderLineBytes)
	if err != nil {
		return nil, err
	}
	if eof && len(line) == 0 {
		return nil, malformed("empty envelope")
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(line, &hdr); err != nil || hdr == nil {
		return nil, malformed("invalid envelope header")
	}
	env := &Envelope{Header: parseEnvelopeHeader(hdr)}

	// The header line's own newline was consumed; when it was the last byte, eof is false and the next
	// header read sees EOF with no bytes.
	if eof {
		return env, nil
	}
	count := 0
	for {
		line, eof, err := readHeaderLine(br, MaxHeaderLineBytes)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if eof {
				return env, nil
			}
			continue
		}
		count++
		if count > MaxEnvelopeItems {
			return nil, ErrTooManyItems
		}
		typ, length, hasLength, err := parseItemHeader(line)
		if err != nil {
			return nil, err
		}
		it := Item{Type: typ}
		// A transaction item is kept like an event (sentinel-span-ingest D2) under the same 1 MiB per-item bound;
		// every other type is read past unread.
		isEvent := typ == "event" || typ == "transaction"

		if eof {
			// The header line ended at EOF: a length item has no payload; a newline item has an empty one.
			if hasLength && length > 0 {
				return nil, malformed("truncated item")
			}
			env.Items = append(env.Items, it)
			return env, nil
		}

		if hasLength {
			if isEvent && length <= MaxEventItemBytes {
				buf := make([]byte, length)
				if _, err := io.ReadFull(br, buf); err != nil {
					return nil, truncatedOrErr(err)
				}
				it.Payload = buf
			} else {
				if _, err := io.CopyN(io.Discard, br, length); err != nil {
					return nil, truncatedOrErr(err)
				}
				it.Oversized = isEvent
			}
			env.Items = append(env.Items, it)
			// One byte after the payload: EOF ends the envelope, \n continues, \r\n continues.
			b, err := br.ReadByte()
			switch {
			case err == io.EOF:
				return env, nil
			case err != nil:
				return nil, err
			case b == '\n':
			case b == '\r':
				b2, err := br.ReadByte()
				if err == io.EOF || (err == nil && b2 != '\n') {
					return nil, malformed("missing newline after item")
				}
				if err != nil {
					return nil, err
				}
			default:
				return nil, malformed("missing newline after item")
			}
			continue
		}

		// Without length: the payload runs to the next \n or EOF.
		if isEvent {
			payload, over, peof, err := readPayloadLine(br, MaxEventItemBytes)
			if err != nil {
				return nil, err
			}
			if over {
				it.Oversized = true
			} else {
				it.Payload = payload
			}
			env.Items = append(env.Items, it)
			if peof {
				return env, nil
			}
			continue
		}
		peof, err := discardLine(br)
		if err != nil {
			return nil, err
		}
		env.Items = append(env.Items, it)
		if peof {
			return env, nil
		}
	}
}

func truncatedOrErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed("truncated item")
	}
	return err
}

func parseEnvelopeHeader(hdr map[string]json.RawMessage) EnvelopeHeader {
	var h EnvelopeHeader
	if raw, ok := hdr["event_id"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			h.EventID = s // passed raw: IngestAt re-mints an id holding a control character (T8)
		}
	}
	if raw, ok := hdr["sdk"]; ok {
		var o struct {
			Name    json.RawMessage `json:"name"`
			Version json.RawMessage `json:"version"`
		}
		if json.Unmarshal(raw, &o) == nil {
			var name, version string
			_ = json.Unmarshal(o.Name, &name)
			_ = json.Unmarshal(o.Version, &version)
			name, version = stripNUL(name), stripNUL(version)
			if name != "" || version != "" {
				h.SDK = &validation.SDKInfo{Name: name, Version: version}
			}
		}
	}
	return h
}

func parseItemHeader(line []byte) (typ string, length int64, hasLength bool, err error) {
	var h map[string]json.RawMessage
	if json.Unmarshal(line, &h) != nil || h == nil {
		return "", 0, false, malformed("invalid item header")
	}
	rawType, ok := h["type"]
	if !ok || json.Unmarshal(rawType, &typ) != nil || typ == "" {
		return "", 0, false, malformed("invalid item header")
	}
	if rawLen, ok := h["length"]; ok {
		var n json.Number
		dec := json.NewDecoder(bytes.NewReader(rawLen))
		dec.UseNumber()
		if trimmed := bytes.TrimSpace(rawLen); len(trimmed) == 0 || trimmed[0] == '"' || dec.Decode(&n) != nil {
			return "", 0, false, malformed("invalid item length")
		}
		v, perr := n.Int64()
		if perr != nil || v < 0 || v > MaxDecompressedBodyBytes {
			return "", 0, false, malformed("invalid item length")
		}
		return typ, v, true, nil
	}
	return typ, 0, false, nil
}

// readHeaderLine returns the bytes up to (not including) the next \n or EOF, with one trailing \r stripped. eof
// reports that the line ended at EOF rather than at a \n. A line over max bytes is ErrMalformedEnvelope.
func readHeaderLine(br *bufio.Reader, max int) (line []byte, eof bool, err error) {
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > max+2 {
			return nil, false, malformed("line too long")
		}
		switch {
		case rerr == nil:
			buf = trimEOL(buf)
			if len(buf) > max {
				return nil, false, malformed("line too long")
			}
			return buf, false, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case rerr == io.EOF:
			buf = bytes.TrimSuffix(buf, []byte("\r"))
			if len(buf) > max {
				return nil, false, malformed("line too long")
			}
			return buf, true, nil
		default:
			return nil, false, rerr
		}
	}
}

// readPayloadLine is readHeaderLine for a newline-framed event payload: it never fails for length. Past max it
// stops buffering, discards the rest of the line, and returns over = true.
func readPayloadLine(br *bufio.Reader, max int) (line []byte, over, eof bool, err error) {
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if !over {
			buf = append(buf, chunk...)
			if len(buf) > max+2 {
				over, buf = true, nil
			}
		}
		switch {
		case rerr == nil:
			if over {
				return nil, true, false, nil
			}
			buf = trimEOL(buf)
			return buf, len(buf) > max, false, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case rerr == io.EOF:
			if over {
				return nil, true, true, nil
			}
			buf = bytes.TrimSuffix(buf, []byte("\r"))
			return buf, len(buf) > max, true, nil
		default:
			return nil, false, false, rerr
		}
	}
}

// discardLine reads and drops the rest of the current line without buffering it.
func discardLine(br *bufio.Reader) (eof bool, err error) {
	for {
		_, rerr := br.ReadSlice('\n')
		switch {
		case rerr == nil:
			return false, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case rerr == io.EOF:
			return true, nil
		default:
			return false, rerr
		}
	}
}

// trimEOL strips the \n ReadSlice returned and one \r before it.
func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

func stripNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}
