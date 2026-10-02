package unit

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scReadFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(scFixtureDir), filepath.FromSlash(rel)))
	require.NoError(t, err)
	return b
}

func scDecode(t *testing.T, raw []byte, enc string) []byte {
	t.Helper()
	rd, err := sentrycompat.DecodeBody(bytes.NewReader(raw), enc, sentrycompat.MaxDecompressedBodyBytes)
	require.NoError(t, err)
	out, err := io.ReadAll(rd)
	require.NoError(t, err)
	return out
}

func scReadEnv(t *testing.T, raw []byte) *sentrycompat.Envelope {
	t.Helper()
	env, err := sentrycompat.ReadEnvelope(bytes.NewReader(raw))
	require.NoError(t, err)
	return env
}

func scCompact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, json.Compact(&b, raw))
	return b.Bytes()
}

// P3.1
func TestSentryEnvelopeFixturesParse(t *testing.T) {
	env := scReadEnv(t, scReadFixture(t, "envelopes/flutter-9.6.0.envelope"))
	assert.Equal(t, "11111111111141118111111111111111", env.Header.EventID)
	require.NotNil(t, env.Header.SDK)
	assert.Equal(t, "sentry.dart.flutter", env.Header.SDK.Name)
	assert.Equal(t, "9.6.0", env.Header.SDK.Version)
	require.Len(t, env.Items, 3)
	assert.Equal(t, []string{"event", "transaction", "client_report"}, []string{env.Items[0].Type, env.Items[1].Type, env.Items[2].Type})
	assert.Equal(t, string(scCompact(t, scReadFixture(t, "flutter-typeerror-unhandled.json"))), string(env.Items[0].Payload))
	// sentinel-span-ingest D2: a transaction item is kept like an event so its spans can be parsed; every other type
	// (the client_report below) is still read past unread.
	assert.NotEmpty(t, env.Items[1].Payload, "a transaction payload is kept")
	assert.Nil(t, env.Items[2].Payload, "a non-event, non-transaction payload is read past, not kept")

	env = scReadEnv(t, scReadFixture(t, "envelopes/nextjs-10.envelope"))
	assert.Equal(t, "44444444444444448444444444444444", env.Header.EventID)
	require.Len(t, env.Items, 2)
	assert.Equal(t, "event", env.Items[0].Type)
	assert.Equal(t, "session", env.Items[1].Type)
	var ev map[string]any
	require.NoError(t, json.Unmarshal(env.Items[0].Payload, &ev))
	assert.Equal(t, "javascript", ev["platform"])
}

// P3.2
func TestSentryEnvelopeVariantsMatch(t *testing.T) {
	identity := scReadFixture(t, "envelopes/flutter-9.6.0.envelope")
	for _, c := range []struct{ file, enc string }{
		{"envelopes/flutter-9.6.0.envelope.gz", "gzip"},
		{"envelopes/flutter-9.6.0.envelope.gz", "x-gzip"},
		{"envelopes/flutter-9.6.0.envelope.gz", " GZIP "},
		{"envelopes/flutter-9.6.0.envelope.zlib", "deflate"},
		{"envelopes/flutter-9.6.0.envelope.deflate", "deflate"},
	} {
		got := scDecode(t, scReadFixture(t, c.file), c.enc)
		assert.True(t, bytes.Equal(identity, got), "%s as %s", c.file, c.enc)
		env := scReadEnv(t, got)
		assert.Len(t, env.Items, 3, c.file)
	}
	nx := scReadFixture(t, "envelopes/nextjs-10.envelope")
	assert.True(t, bytes.Equal(nx, scDecode(t, scReadFixture(t, "envelopes/nextjs-10.envelope.gz"), "gzip")))
	assert.True(t, bytes.Equal(identity, scDecode(t, identity, "")))
	assert.True(t, bytes.Equal(identity, scDecode(t, identity, "identity")))

	for _, enc := range []string{"br", "zstd", "compress", "gzip, br", "gzip,deflate"} {
		_, err := sentrycompat.DecodeBody(bytes.NewReader(identity), enc, 1<<20)
		assert.ErrorIs(t, err, sentrycompat.ErrUnsupportedEncoding, enc)
	}
}

// P3.3
func TestSentryEnvelopeFraming(t *testing.T) {
	big := strings.Repeat("a", 65<<10)
	cases := []struct {
		name      string
		in        string
		items     []string // types
		malformed bool
	}{
		{"header only", `{}`, nil, false},
		{"header only with newline", "{}\n", nil, false},
		{"blank lines between items", "{}\n\n{\"type\":\"a\",\"length\":2}\nxx\n\n\n{\"type\":\"b\"}\ny\n", []string{"a", "b"}, false},
		{"crlf line ends", "{}\r\n{\"type\":\"a\",\"length\":2}\r\nxx\r\n{\"type\":\"event\"}\r\n{\"a\":1}\r\n", []string{"a", "event"}, false},
		{"mixed framing", "{}\n{\"type\":\"event\",\"length\":7}\n{\"a\":1}\n{\"type\":\"session\"}\nzzz", []string{"event", "session"}, false},
		{"length zero", "{}\n{\"type\":\"a\",\"length\":0}\n\n{\"type\":\"b\",\"length\":0}", []string{"a", "b"}, false},
		{"unknown type read past", "{}\n{\"type\":\"security\"}\nxx\n{\"type\":\"Event\",\"length\":2}\nxx", []string{"security", "Event"}, false},
		{"length item then garbage", "{}\n{\"type\":\"a\",\"length\":1}\nxx", nil, true},
		{"truncated length", "{}\n{\"type\":\"a\",\"length\":10}\nxx", nil, true},
		{"negative length", "{}\n{\"type\":\"a\",\"length\":-1}\nx", nil, true},
		{"fractional length", "{}\n{\"type\":\"a\",\"length\":1.5}\nx", nil, true},
		{"string length", "{}\n{\"type\":\"a\",\"length\":\"3\"}\nxxx", nil, true},
		{"no type", "{}\n{\"length\":1}\nx", nil, true},
		{"numeric type", "{}\n{\"type\":7}\nx", nil, true},
		{"empty type", "{}\n{\"type\":\"\"}\nx", nil, true},
		{"item header not json", "{}\nnot json\nx", nil, true},
		{"item header not object", "{}\n[1]\nx", nil, true},
		{"envelope header not object", "[1]\n", nil, true},
		{"envelope header not json", "nope\n", nil, true},
		{"empty body", "", nil, true},
		{"65 KiB header line", "{\"x\":\"" + big + "\"}\n", nil, true},
		{"65 KiB item header line", "{}\n{\"type\":\"a\",\"x\":\"" + big + "\"}\nx", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, err := sentrycompat.ReadEnvelope(strings.NewReader(c.in))
			if c.malformed {
				require.ErrorIs(t, err, sentrycompat.ErrMalformedEnvelope)
				return
			}
			require.NoError(t, err)
			var got []string
			for _, it := range env.Items {
				got = append(got, it.Type)
			}
			assert.Equal(t, c.items, got)
		})
	}

	// An unknown-type item is read past whatever its size; an event over the cap is marked, not an error.
	huge := strings.Repeat("x", sentrycompat.MaxEventItemBytes+1)
	for name, in := range map[string]string{
		"length-framed":  "{}\n{\"type\":\"event\",\"length\":" + itoa(len(huge)) + "}\n" + huge + "\n{\"type\":\"event\",\"length\":2}\n{}",
		"newline-framed": "{}\n{\"type\":\"event\"}\n" + huge + "\n{\"type\":\"event\",\"length\":2}\n{}",
	} {
		env, err := sentrycompat.ReadEnvelope(strings.NewReader(in))
		require.NoError(t, err, name)
		require.Len(t, env.Items, 2, name)
		assert.True(t, env.Items[0].Oversized, name)
		assert.Nil(t, env.Items[0].Payload, name)
		assert.False(t, env.Items[1].Oversized, name)
		assert.Equal(t, "{}", string(env.Items[1].Payload), name)
	}

	// 100 items pass; the 101st is ErrTooManyItems.
	build := func(n int) string {
		return "{}\n" + strings.Repeat("{\"type\":\"session\"}\nx\n", n)
	}
	env, err := sentrycompat.ReadEnvelope(strings.NewReader(build(sentrycompat.MaxEnvelopeItems)))
	require.NoError(t, err)
	assert.Len(t, env.Items, 100)
	_, err = sentrycompat.ReadEnvelope(strings.NewReader(build(sentrycompat.MaxEnvelopeItems + 1)))
	assert.ErrorIs(t, err, sentrycompat.ErrTooManyItems)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// P3.4
func TestSentryDecodeBombStopsAtCap(t *testing.T) {
	const bomb = 64 << 20
	zeros := make([]byte, bomb)
	compress := map[string]func() []byte{
		"gzip": func() []byte {
			var b bytes.Buffer
			w, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
			_, _ = w.Write(zeros)
			_ = w.Close()
			return b.Bytes()
		},
		"deflate (zlib)": func() []byte {
			var b bytes.Buffer
			w, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
			_, _ = w.Write(zeros)
			_ = w.Close()
			return b.Bytes()
		},
		"deflate (raw)": func() []byte {
			var b bytes.Buffer
			w, _ := flate.NewWriter(&b, flate.BestCompression)
			_, _ = w.Write(zeros)
			_ = w.Close()
			return b.Bytes()
		},
	}
	for name, mk := range compress {
		t.Run(name, func(t *testing.T) {
			enc := strings.Fields(name)[0]
			body := mk()
			require.Less(t, len(body), sentrycompat.MaxCompressedBodyBytes, "the bomb must fit under the compressed cap")
			rd, err := sentrycompat.DecodeBody(bytes.NewReader(body), enc, sentrycompat.MaxDecompressedBodyBytes)
			require.NoError(t, err)
			n, err := io.Copy(io.Discard, rd)
			assert.ErrorIs(t, err, sentrycompat.ErrBodyTooLarge)
			assert.EqualValues(t, sentrycompat.MaxDecompressedBodyBytes, n)
			// Every later read fails the same way.
			k, err := rd.Read(make([]byte, 8))
			assert.Zero(t, k)
			assert.ErrorIs(t, err, sentrycompat.ErrBodyTooLarge)
		})
	}
}

// scErrClass covers the error classification DecodeBody and the reader promise ReadEnvelope.
func TestSentryDecodeErrorClasses(t *testing.T) {
	// Corrupt and empty compressed bodies are ErrCorruptBody.
	for name, c := range map[string]struct{ enc, body string }{
		"corrupt gzip": {"gzip", "not gzip at all"},
		"empty gzip":   {"gzip", ""},
		"empty zlib":   {"deflate", ""},
		"corrupt zlib": {"deflate", "\x78\x9c\xff\xff"},
		"corrupt raw":  {"deflate", "\xff\xff\xff\xff"},
		"truncated gzip": {"gzip", string(func() []byte {
			var b bytes.Buffer
			w := gzip.NewWriter(&b)
			_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
			_ = w.Close()
			return b.Bytes()[:20]
		}())},
	} {
		rd, err := sentrycompat.DecodeBody(strings.NewReader(c.body), c.enc, 1<<20)
		if err == nil {
			_, err = io.Copy(io.Discard, rd)
		}
		assert.ErrorIs(t, err, sentrycompat.ErrCorruptBody, name)
	}

	// A MaxBytesReader error under a decoder passes through unchanged.
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write(bytes.Repeat([]byte("abcdefgh"), 4096))
	_ = w.Close()
	rec := httptest.NewRecorder()
	body := http.MaxBytesReader(rec, io.NopCloser(bytes.NewReader(b.Bytes())), 64)
	rd, err := sentrycompat.DecodeBody(body, "gzip", 1<<20)
	if err == nil {
		_, err = io.Copy(io.Discard, rd)
	}
	var mbe *http.MaxBytesError
	assert.True(t, errors.As(err, &mbe), "got %v", err)
}
