package gitprovider

import (
	"bytes"
	"errors"
	"testing"
)

func TestRedactor_WriteStripsSecrets(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, "supersecrettoken", "another-secret")

	n, err := r.Write([]byte("remote: https://x:supersecrettoken@github.com/acme/widgets.git error, another-secret leaked too"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// io.Writer contract: n reports len(original p), not the redacted/resized output.
	if want := len("remote: https://x:supersecrettoken@github.com/acme/widgets.git error, another-secret leaked too"); n != want {
		t.Errorf("n = %d, want %d", n, want)
	}

	got := buf.String()
	if bytes.Contains([]byte(got), []byte("supersecrettoken")) {
		t.Errorf("secret leaked into output: %q", got)
	}
	if bytes.Contains([]byte(got), []byte("another-secret")) {
		t.Errorf("second secret leaked into output: %q", got)
	}
	if !bytes.Contains([]byte(got), []byte(redactedPlaceholder)) {
		t.Errorf("expected placeholder in output: %q", got)
	}
}

func TestRedactor_EmptySecretIgnored(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, "", "real-secret")
	if _, err := r.Write([]byte("hello world, real-secret here")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if bytes.Contains([]byte(got), []byte("real-secret")) {
		t.Errorf("secret leaked: %q", got)
	}
	// Sanity: an empty secret must not turn into a needle that redacts everything.
	if bytes.Contains([]byte(got), []byte(redactedPlaceholder+redactedPlaceholder+redactedPlaceholder)) {
		t.Errorf("empty secret over-redacted: %q", got)
	}
}

func TestRedactor_RedactWithoutWriting(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, "shh")
	out := r.Redact([]byte("shh, it's a shh secret"))
	if bytes.Contains(out, []byte("shh")) {
		t.Errorf("Redact did not strip secret: %q", out)
	}
	if buf.Len() != 0 {
		t.Errorf("Redact must not write to the underlying writer, got %q", buf.String())
	}
}

// MUTATION-TEST NOTE (repo convention: every security guard must be proven to fail red before it
// passes): comment out the bytes.ReplaceAll loop body in Redactor.Redact (or return p unchanged)
// and re-run this file — TestRedactor_WriteStripsSecrets must go red, asserting the guard is load
// bearing rather than decorative.

// TestRedactor_SplitWriteAcrossBoundary proves a secret split across two Write calls (the shape
// os/exec produces when copying a child's stdout/stderr in fixed-size chunks) never reaches the
// sink whole. Without carry-over state between Writes, the two halves land in the sink
// unredacted, reassembled by nothing more than being adjacent bytes.
func TestRedactor_SplitWriteAcrossBoundary(t *testing.T) {
	var buf bytes.Buffer
	secret := "ghp_straddleLeakToken"
	r := NewRedactor(&buf, secret)

	first := "remote: rejected ghp_strad"
	second := "dleLeakToken and stopped"
	if _, err := r.Write([]byte(first)); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := r.Write([]byte(second)); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got := buf.String()
	if bytes.Contains([]byte(got), []byte(secret)) {
		t.Fatalf("LEAK: secret reassembled across write boundary: %q", got)
	}
	if !bytes.Contains([]byte(got), []byte(redactedPlaceholder)) {
		t.Errorf("expected placeholder in output: %q", got)
	}
	// Flush must not drop legitimate output either side of the secret.
	if !bytes.Contains([]byte(got), []byte("remote: rejected")) || !bytes.Contains([]byte(got), []byte("and stopped")) {
		t.Errorf("Flush dropped surrounding output: %q", got)
	}
}

// TestRedactor_FlushEmitsHeldBackTail proves that bytes held back to guard against a straddling
// secret are not silently lost when the stream ends with no further Write.
func TestRedactor_FlushEmitsHeldBackTail(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, "supersecrettoken")

	if _, err := r.Write([]byte("trailing output with no secret at all")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("expected some output flushed eagerly before Flush")
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("no secret at all")) {
		t.Errorf("Flush did not emit held-back tail: %q", buf.String())
	}
}

// failingWriter fails every Write with errWrite once armed.
type failingWriter struct {
	err error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	return 0, f.err
}

var errWrite = errors.New("boom: sink is gone")

// TestRedactor_WriteErrorIsLatchedAndSurfacedByFlush is finding 5: a Write that fails against the
// underlying writer must be remembered (writeErr) and surfaced again by Flush, even when Flush has
// no pending tail of its own to emit — a caller that checks only Flush's return value must still
// observe the failure, not a false "success".
//
// MUTATION-TEST NOTE: temporarily deleting the `r.writeErr = err` assignments in Write (both the
// maxLen==0 fast path and the general path) turns this test RED — Flush then returns nil even
// though the underlying writer failed.
func TestRedactor_WriteErrorIsLatchedAndSurfacedByFlush(t *testing.T) {
	fw := &failingWriter{err: errWrite}
	r := NewRedactor(fw, "supersecrettoken")

	if _, err := r.Write([]byte("some output long enough to flush past the hold-back window!!")); err == nil {
		t.Fatal("expected Write to report the underlying writer's error")
	} else if !errors.Is(err, errWrite) {
		t.Fatalf("expected errWrite, got %v", err)
	}

	// A second Write must also fail fast rather than attempting further (now-unreliable)
	// redaction bookkeeping against a writer already known to be broken.
	if _, err := r.Write([]byte("more")); err == nil {
		t.Fatal("expected subsequent Write to keep reporting the latched error")
	}

	if err := r.Flush(); err == nil {
		t.Fatal("expected Flush to surface the latched write error")
	} else if !errors.Is(err, errWrite) {
		t.Fatalf("expected errWrite from Flush, got %v", err)
	}
}

// TestRedactor_NoSecretsFastPathLatchesWriteError covers the maxLen==0 ("no secrets configured")
// fast path specifically, since it has its own separate r.w.Write call in Write.
func TestRedactor_NoSecretsFastPathLatchesWriteError(t *testing.T) {
	fw := &failingWriter{err: errWrite}
	r := NewRedactor(fw) // no secrets: maxLen stays 0

	if _, err := r.Write([]byte("anything")); err == nil {
		t.Fatal("expected Write to report the underlying writer's error")
	}
	if err := r.Flush(); err == nil {
		t.Fatal("expected Flush to surface the latched write error")
	}
}
