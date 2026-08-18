package gitprovider

import (
	"bytes"
	"io"
)

const redactedPlaceholder = "***REDACTED***"

// Redactor wraps an io.Writer and strips every configured secret value from bytes written through
// it before they reach the underlying writer — the mandatory choke point (plan §4.5) for every
// git/log output path a token could otherwise leak through (subprocess stdout/stderr, structured
// logs, error strings). Empty secret values are ignored (an empty needle would match everywhere).
//
// Write is stateful: os/exec streams a child's stdout/stderr to Writers in chunks (commonly
// 32KiB), and a secret can straddle a chunk boundary so that neither individual Write call
// contains it whole. Redactor holds back a short tail of unflushed bytes between calls — enough
// to catch a secret split across the boundary — and only forwards output once it can no longer
// be part of a straddling match. Callers MUST call Flush (or Close) once no more data is coming,
// or the held-back tail is never emitted.
type Redactor struct {
	w        io.Writer
	secrets  [][]byte
	maxLen   int
	pending  []byte
	writeErr error
}

// NewRedactor builds a Redactor over w that strips each of secrets from every Write call. Callers
// should build one Redactor per job/run from every credential value in play (askpass credentials,
// env-var fallback tokens, ...) and route ALL git/log output for that run through it.
func NewRedactor(w io.Writer, secrets ...string) *Redactor {
	r := &Redactor{w: w}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		b := []byte(s)
		r.secrets = append(r.secrets, b)
		if len(b) > r.maxLen {
			r.maxLen = len(b)
		}
	}
	return r
}

// AddSecrets adds additional secret values to an existing Redactor, recomputing maxLen. It exists
// so a caller-supplied Redactor can be defensively extended with the secret values actually in
// play for a given call (see RunGit) rather than relying on every caller to have built the
// Redactor from exactly the right set up front — a mismatch there is a silent token leak, not an
// error. Safe to call before any Write (adding secrets mid-stream after bytes have already been
// flushed unredacted would not retroactively redact them).
func (r *Redactor) AddSecrets(secrets ...string) {
	if r == nil {
		return
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		b := []byte(s)
		r.secrets = append(r.secrets, b)
		if len(b) > r.maxLen {
			r.maxLen = len(b)
		}
	}
}

// Redact returns p with every configured secret value replaced by a fixed placeholder, without
// writing anything or touching any streaming state. Used to sanitize bytes that are about to be
// embedded in an error message or other string rather than streamed through Write.
func (r *Redactor) Redact(p []byte) []byte {
	if r == nil {
		return p
	}
	out := p
	for _, s := range r.secrets {
		if len(s) == 0 {
			continue
		}
		out = bytes.ReplaceAll(out, s, []byte(redactedPlaceholder))
	}
	return out
}

// Write implements io.Writer, redacting p before forwarding to the underlying writer. Per the
// io.Writer contract it reports the length of the ORIGINAL input p (not the redacted/resized
// output) on success, so callers relying on n == len(p) are not misled by a transformed write.
//
// To catch a secret split across two Write calls, Write holds back the trailing (maxLen-1) bytes
// of the combined (pending+p) buffer rather than flushing everything immediately: those bytes
// might be the prefix of a secret whose suffix arrives in the next call. The held-back tail is
// prepended to the next Write, or emitted by Flush/Close when the stream ends.
func (r *Redactor) Write(p []byte) (int, error) {
	if r == nil || r.w == nil {
		return len(p), nil
	}
	if r.writeErr != nil {
		// A previous write to the underlying writer already failed: report it again rather than
		// silently pretending the (now potentially unredacted, un-flushable) held-back tail is
		// fine. Once a fallible writer has failed, further redaction bookkeeping cannot be trusted.
		return 0, r.writeErr
	}
	n := len(p)
	if r.maxLen == 0 {
		// No secrets configured: nothing can straddle, so no need to hold anything back.
		if _, err := r.w.Write(p); err != nil {
			r.writeErr = err
			return 0, err
		}
		return n, nil
	}

	combined := append(r.pending, p...)
	r.pending = nil

	hold := r.maxLen - 1
	// A naive "flush everything except the last `hold` raw bytes" is not safe on its own: a
	// complete match can START before that boundary and END after it (this Write call's data
	// simply completed a secret whose prefix arrived last call), and slicing combined at the
	// boundary before redacting would split that match in two, leaving its still-literal prefix
	// in the flushed portion. So the boundary must never fall inside a match: it is pushed out to
	// the end of the furthest-reaching complete match found anywhere in combined.
	holdStart := len(combined) - hold
	if lastEnd := r.lastMatchEnd(combined); lastEnd > holdStart {
		holdStart = lastEnd
	}
	if holdStart > len(combined) {
		holdStart = len(combined)
	}
	if holdStart < 0 {
		holdStart = 0
	}

	if holdStart > 0 {
		redacted := r.Redact(combined[:holdStart])
		if _, err := r.w.Write(redacted); err != nil {
			r.writeErr = err
			return 0, err
		}
	}
	r.pending = append(r.pending, combined[holdStart:]...)
	return n, nil
}

// lastMatchEnd scans combined for every configured secret and returns the furthest end offset
// (exclusive) reached by any complete, non-overlapping match. It is used to push the
// flush/hold-back boundary out past any match that would otherwise be split by a naive
// fixed-width tail, which is what lets a secret straddling two Write calls stay intact until it
// is fully redacted.
func (r *Redactor) lastMatchEnd(combined []byte) int {
	end := 0
	for _, s := range r.secrets {
		if len(s) == 0 {
			continue
		}
		pos := 0
		for {
			i := bytes.Index(combined[pos:], s)
			if i < 0 {
				break
			}
			matchEnd := pos + i + len(s)
			if matchEnd > end {
				end = matchEnd
			}
			pos = matchEnd
		}
	}
	return end
}

// Flush redacts and emits any held-back tail bytes to the underlying writer. Callers MUST call
// this once no more data will be written (e.g. after cmd.Run() returns in RunGit) — otherwise up
// to maxLen-1 bytes of legitimate output are silently dropped rather than ever reaching the sink.
//
// If an earlier Write already failed against the underlying writer, that error is latched
// (writeErr) and returned again here even if there is no pending tail to emit: a caller that
// only checks Flush's return value (ignoring Write's) would otherwise observe Flush succeed after
// a fallible writer had already dropped output on the floor mid-stream.
func (r *Redactor) Flush() error {
	if r == nil || r.w == nil {
		return nil
	}
	if len(r.pending) == 0 {
		return r.writeErr
	}
	redacted := r.Redact(r.pending)
	r.pending = nil
	if _, err := r.w.Write(redacted); err != nil {
		r.writeErr = err
		return err
	}
	return r.writeErr
}

// Close flushes any pending held-back bytes. It does not close the underlying writer.
func (r *Redactor) Close() error {
	return r.Flush()
}
