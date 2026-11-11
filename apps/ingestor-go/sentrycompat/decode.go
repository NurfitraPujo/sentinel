// Package sentrycompat serves the Sentry-compatible ingest routes (POST /api/{project_id}/envelope/ and /store/):
// a bounded body decoder, an envelope reader, a Sentry event -> ErrorPayload mapper and the handlers that feed
// service.IngestAtWithin (sentinel-sentry-compat-ingest spec §3-§5).
package sentrycompat

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	MaxCompressedBodyBytes   = 5 << 20  // the request body as sent: 413
	MaxDecompressedBodyBytes = 20 << 20 // the envelope after decoding: 413
	MaxEventItemBytes        = 1 << 20  // one event item's payload (envelope: dropped; store: 413)
	MaxEnvelopeItems         = 100      // item headers per envelope: 413 on the 101st
	MaxHeaderLineBytes       = 64 << 10 // the envelope header line and each item header line: 400 when longer
)

var (
	ErrUnsupportedEncoding = errors.New("unsupported Content-Encoding")    // 415
	ErrBodyTooLarge        = errors.New("request body too large")          // 413
	ErrCorruptBody         = errors.New("invalid compressed request body") // 400
)

// DecodeBody returns a reader over the decoded body that yields at most max bytes and then fails with
// ErrBodyTooLarge. body must already be wrapped by http.MaxBytesReader(w, r.Body, MaxCompressedBodyBytes).
//
// Every error the decoder or the counting reader can produce is classified here, so ReadEnvelope never needs
// to know which decoder ran: an *http.MaxBytesError passes through unchanged, io.EOF passes through, and every
// other decoder failure (including io.EOF from a gzip or zlib constructor on an empty body) is ErrCorruptBody.
func DecodeBody(body io.Reader, contentEncoding string, max int64) (io.Reader, error) {
	var dec io.Reader
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		dec = body
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, classifyDecodeErr(err)
		}
		dec = zr
	case "deflate":
		br := bufio.NewReader(body)
		head, _ := br.Peek(2)
		if len(head) == 2 && isZlibHeader(head[0], head[1]) {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return nil, classifyDecodeErr(err)
			}
			dec = zr
		} else {
			dec = flate.NewReader(br)
		}
	default:
		return nil, ErrUnsupportedEncoding
	}
	return &capReader{r: io.LimitReader(dec, max+1), max: max}, nil
}

// isZlibHeader is RFC 1950's CMF/FLG check: compression method 8, window size at most 32 KiB, and the 16-bit
// header a multiple of 31. A raw deflate stream passes it only when it opens with a non-final stored block and
// then only one time in 31; that stream is then read as zlib, fails, and is a 400, never a wrong decode.
func isZlibHeader(b0, b1 byte) bool {
	return b0&0x0F == 8 && b0>>4 <= 7 && (uint16(b0)<<8|uint16(b1))%31 == 0
}

func classifyDecodeErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) || errors.Is(err, ErrBodyTooLarge) {
		return err
	}
	return ErrCorruptBody
}

// capReader is the counting reader every decoded body (identity included) is wrapped in. Its source is
// io.LimitReader(decoder, max+1), so the decoder is never asked for more than max+1 bytes: a decompression
// bomb stops after at most max+1 decoded bytes (plus the decoder's window), whatever its ratio.
type capReader struct {
	r   io.Reader
	n   int64
	max int64
	err error
}

// Read returns the decoder's bytes until n reaches max; the call that would take n past max returns the bytes
// up to max and ErrBodyTooLarge, and every later call returns 0, ErrBodyTooLarge.
func (c *capReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := c.r.Read(p)
	if over := c.n + int64(n) - c.max; over > 0 {
		n -= int(over)
		c.n = c.max
		c.err = ErrBodyTooLarge
		return n, c.err
	}
	c.n += int64(n)
	if err != nil && err != io.EOF {
		c.err = classifyDecodeErr(err)
		return n, c.err
	}
	return n, err
}
