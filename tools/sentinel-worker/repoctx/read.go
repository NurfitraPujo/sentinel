package repoctx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxReadBytes caps read_file output (plan §4.5: "byte-capped output").
const maxReadBytes = 64 << 10

const truncationMarker = "\n[...truncated]"

// ReadFile reads path (relative to repo.Root) with plan §4.5's confinement guarantees:
//
//   - path must not be absolute
//   - the path, resolved with filepath.EvalSymlinks against repo.Root (also resolved), must
//     remain strictly under the resolved root — this defeats both `../` traversal and a symlink
//     inside the clone that points outside it
//   - no path component may be ".git" — the clone's own git metadata is never readable through
//     this tool even though it lives under Root
//
// startLine/endLine are 1-indexed and inclusive; both zero means "whole file" (still subject to
// the byte cap).
func ReadFile(repo *Repo, path string, startLine, endLine int) (string, error) {
	if repo == nil {
		return "", fmt.Errorf("repoctx: nil repo")
	}
	if path == "" {
		return "", fmt.Errorf("repoctx: empty path")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("repoctx: absolute paths are rejected: %q", path)
	}
	// Cheap pre-check before touching the filesystem: a cleaned path that still starts with ".."
	// (or is exactly "..") can only be trying to leave Root.
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repoctx: path escapes repo root: %q", path)
	}
	for _, seg := range strings.Split(cleaned, string(filepath.Separator)) {
		if seg == ".git" {
			return "", fmt.Errorf("repoctx: refusing to read .git path: %q", path)
		}
	}

	rootResolved, err := filepath.EvalSymlinks(repo.Root)
	if err != nil {
		return "", fmt.Errorf("repoctx: resolve repo root: %w", err)
	}
	joined := filepath.Join(repo.Root, cleaned)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("repoctx: resolve path %q: %w", path, err)
	}
	if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", fmt.Errorf("repoctx: path escapes repo root: %q", path)
	}
	// Re-check the .git guard against the RESOLVED path too: a symlink could legitimately resolve
	// to somewhere under Root that nonetheless traverses through a differently-named component
	// that IS a symlink to .git contents. This is NOT merely defense-in-depth: it is the SOLE
	// guard against a symlink whose OWN name is not ".git" but that resolves TO .git under root
	// (e.g. root/peek -> root/.git) — the cleaned-path pre-check above never fires (the requested
	// path is "peek/config", no ".git" segment in the literal text) and the root-escape check
	// below never fires (.git IS under root). See repoctx_test.go
	// TestReadFile_RejectsSymlinkToGitViaAlias.
	//
	// MUTATION-TEST NOTE: temporarily changing `if seg == ".git"` below to `if seg == ".git" &&
	// false` turns TestReadFile_RejectsSymlinkToGitViaAlias RED.
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil {
		return "", fmt.Errorf("repoctx: path escapes repo root: %q", path)
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == ".git" {
			return "", fmt.Errorf("repoctx: refusing to read .git path: %q", path)
		}
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("repoctx: stat %q: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("repoctx: %q is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("repoctx: %q is not a regular file", path)
	}

	f, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("repoctx: open %q: %w", path, err)
	}
	defer f.Close()

	if startLine <= 0 && endLine <= 0 {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(io.LimitReader(f, maxReadBytes+1)); err != nil {
			return "", fmt.Errorf("repoctx: read %q: %w", path, err)
		}
		out := buf.Bytes()
		if len(out) > maxReadBytes {
			out = out[:maxReadBytes]
			return string(out) + truncationMarker, nil
		}
		return string(out), nil
	}

	if startLine <= 0 {
		startLine = 1
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out bytes.Buffer
	lineNo := 0
	truncated := false
	for scanner.Scan() {
		lineNo++
		if lineNo < startLine {
			continue
		}
		if endLine > 0 && lineNo > endLine {
			break
		}
		if out.Len() >= maxReadBytes {
			truncated = true
			break
		}
		out.WriteString(scanner.Text())
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("repoctx: scan %q: %w", path, err)
	}
	res := out.Bytes()
	if len(res) > maxReadBytes {
		res = res[:maxReadBytes]
		truncated = true
	}
	if truncated {
		return string(res) + truncationMarker, nil
	}
	return string(res), nil
}
