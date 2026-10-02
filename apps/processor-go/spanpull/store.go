package spanpull

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// ErrNotFound is a trace the store does not hold (yet, or any more): not a failure — the second fetch tries again.
var ErrNotFound = errors.New("trace not found in the trace store")

// maxResponseBytes bounds one trace-store response so a huge trace cannot exhaust the processor's memory.
const maxResponseBytes = 32 << 20

// ErrTooLarge is a response over maxResponseBytes, or the store's own "trace exceeds max size" refusal: not retried (the next fetch returns the same body), counted
// `too_large`, and the stage is finished.
var ErrTooLarge = errors.New("trace store response over the size bound")

// TraceStore fetches one trace's spans. The two implementations speak the two formats D3 names (Tempo OTLP JSON,
// the Jaeger JSON of VictoriaTraces); everything after the parse is shared. Returned spans carry the trace id
// asked for, a service name and an environment taken from the span's resource, and Source "pulled" is added by the
// caller.
type TraceStore interface {
	Fetch(ctx context.Context, traceID string) ([]spanwire.Span, error)
}

// NewTraceStore builds the adapter for cfg.Kind. A custom client lets tests stub the transport.
func NewTraceStore(cfg Config, client *http.Client) (TraceStore, error) {
	if client == nil {
		client = &http.Client{}
	}
	switch cfg.Kind {
	case KindTempo:
		return &tempoStore{base: cfg.URL, client: client}, nil
	case KindJaeger:
		return &jaegerStore{base: cfg.URL, client: client}, nil
	default:
		return nil, fmt.Errorf("unknown trace store kind %q", cfg.Kind)
	}
}

const (
	errBodyBytes   = 4 << 10
	tooLargeMarker = "trace exceeds max size"
)

// dropControls removes NUL and the other control characters: Postgres text rejects NUL, and the rest have no place
// in a one-line log field.
func dropControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func controlsToSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// excerpt is b as one line of at most 200 characters.
func excerpt(b []byte) string {
	s := strings.Join(strings.Fields(controlsToSpace(strings.ToValidUTF8(string(b), "?"))), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// getJSON GETs path under base and returns the body, ErrNotFound for a 404. The context carries the 5 s budget.
func getJSON(ctx context.Context, client *http.Client, base, path string) ([]byte, error) {
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("trace store URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trace store request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyBytes))
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck
		// Tempo refuses a trace over its max size with HTTP 500: the trace only grows, so this is final, not a failure.
		if strings.Contains(string(prefix), tooLargeMarker) {
			return nil, ErrTooLarge
		}
		if ex := excerpt(prefix); ex != "" {
			return nil, fmt.Errorf("trace store answered HTTP %d: %s", resp.StatusCode, ex)
		}
		return nil, fmt.Errorf("trace store answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("trace store body: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, ErrTooLarge
	}
	return body, nil
}

// normID canonicalises a trace (want 32) or span (want 16) id to lower-case hex. It accepts hex of the right length,
// shorter hex (left-padded: some stores drop leading zeros), and the base64 of the raw bytes, which is how a
// protobuf-JSON bytes field is spelled. Anything else, and the all-zero id, is "".
func normID(s string, want int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var out string
	switch {
	case len(s) == want && isHexDigits(s):
		out = strings.ToLower(s)
	case len(s) == base64Len(want/2):
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			if raw, err = base64.URLEncoding.DecodeString(s); err != nil {
				return ""
			}
		}
		if len(raw) != want/2 {
			return ""
		}
		out = hex.EncodeToString(raw)
	case len(s) < want && isHexDigits(s):
		out = strings.Repeat("0", want-len(s)) + strings.ToLower(s)
	default:
		return ""
	}
	if strings.Trim(out, "0") == "" {
		return ""
	}
	return out
}

func isHexDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func base64Len(n int) int { return (n + 2) / 3 * 4 }
