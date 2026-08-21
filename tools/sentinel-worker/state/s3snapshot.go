package state

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// S3Config is the plan §2.8 S3 snapshot-backend configuration (`WORKER_SNAPSHOT_BACKEND=s3`).
// Endpoint is a full scheme://host[:port] (works against AWS S3, MinIO, or any S3-compatible
// endpoint -- the compose stack's MinIO in particular). Prefix is optional and is joined with a
// "/" before every object key.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	Region    string
	AccessKey string
	SecretKey string
}

// stateFiles is the exact set of files S3Snapshotter tars/untars -- cursor.json and jobs.journal.
// agent-key.json is DELIBERATELY excluded (plan §2.5/§2.8: restoring an old snapshot must never
// resurrect a rotated-away key). Any other file present in the state dir (e.g. agent-logs/) is
// also excluded -- only these two are the durability contract's concern.
var stateFiles = []string{"cursor.json", "jobs.journal"}

// S3Snapshotter is the plan §2.8 `s3` Snapshotter backend: a hand-rolled AWS SigV4 client (stdlib
// discipline, same as gitprovider/llm's hand-rolled HTTP adapters -- no AWS SDK dependency),
// storing one tarball of the state dir per generation plus a `latest` pointer object written last.
//
// Stale-writer guard: Upload refuses to persist a generation <= the highest generation this
// process has ever uploaded OR restored (baseGeneration), so a late-dying old process (e.g. after
// a rolling update started a replacement before the old pod's context cancelled) can never clobber
// a newer snapshot with a stale one.
type S3Snapshotter struct {
	cfg    S3Config
	client *http.Client

	// mu guards baseGeneration. main.go's runPeriodic loop, journalMaintenanceLoop, and the
	// SIGTERM handler all call Upload against the SAME S3Snapshotter instance from separate
	// goroutines, so baseGeneration's read-check-write in Upload and the read-update in
	// RestoreLatest must be serialized or two concurrent uploads can both pass the
	// generation <= baseGeneration check and each believe it "won" the stale-writer guard.
	mu sync.Mutex

	// baseGeneration is the highest generation known to this process -- set by RestoreLatest (the
	// generation restored) and advanced by every successful Upload. Upload() rejects any
	// generation <= this value. All access must hold mu.
	baseGeneration int64
}

// NewS3Snapshotter constructs an S3Snapshotter against cfg. httpClient may be nil, in which case
// http.DefaultClient is used; tests inject one pointed at an httptest fake S3.
func NewS3Snapshotter(cfg S3Config, httpClient *http.Client) *S3Snapshotter {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &S3Snapshotter{cfg: cfg, client: httpClient}
}

var _ Snapshotter = (*S3Snapshotter)(nil)

// objectKey joins cfg.Prefix with name using a single "/", tolerating an empty prefix or one with
// a trailing slash already.
func (s *S3Snapshotter) objectKey(name string) string {
	if s.cfg.Prefix == "" {
		return name
	}
	return path.Join(strings.TrimSuffix(s.cfg.Prefix, "/"), name)
}

func (s *S3Snapshotter) objectURL(key string) string {
	return strings.TrimSuffix(s.cfg.Endpoint, "/") + "/" + s.cfg.Bucket + "/" + key
}

// Upload implements Snapshotter.Upload: PUTs tarball as `state-<generation>.tar`, then PUTs a
// `latest` pointer object (its body is just the decimal generation number) written LAST -- per
// plan §2.8, so a reader following `latest` never sees a generation whose tarball object isn't
// there yet. Refuses (returns an error, does not upload) any generation <= baseGeneration -- the
// stale-writer guard.
func (s *S3Snapshotter) Upload(ctx context.Context, generation int64, tarball []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation <= s.baseGeneration {
		return fmt.Errorf("state: refusing to upload snapshot generation %d: not greater than the last known generation %d (stale-writer guard)", generation, s.baseGeneration)
	}
	tarKey := s.objectKey(fmt.Sprintf("state-%d.tar", generation))
	if err := s.put(ctx, tarKey, tarball); err != nil {
		return fmt.Errorf("uploading %s: %w", tarKey, err)
	}
	latestKey := s.objectKey("latest")
	if err := s.put(ctx, latestKey, []byte(strconv.FormatInt(generation, 10))); err != nil {
		return fmt.Errorf("uploading %s: %w", latestKey, err)
	}
	s.baseGeneration = generation
	return nil
}

// RestoreLatest implements Snapshotter.RestoreLatest: GETs the `latest` pointer object, parses the
// generation it names, then GETs that generation's tarball. A missing `latest` object (404) is
// reported as (nil, 0, false, nil) -- nothing to restore, not an error (fresh bucket / first boot).
func (s *S3Snapshotter) RestoreLatest(ctx context.Context) ([]byte, int64, bool, error) {
	latestKey := s.objectKey("latest")
	body, ok, err := s.get(ctx, latestKey)
	if err != nil {
		return nil, 0, false, fmt.Errorf("fetching %s: %w", latestKey, err)
	}
	if !ok {
		return nil, 0, false, nil
	}
	generation, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return nil, 0, false, fmt.Errorf("parsing generation from %s: %w", latestKey, err)
	}
	tarKey := s.objectKey(fmt.Sprintf("state-%d.tar", generation))
	tarball, ok, err := s.get(ctx, tarKey)
	if err != nil {
		return nil, 0, false, fmt.Errorf("fetching %s: %w", tarKey, err)
	}
	if !ok {
		return nil, 0, false, fmt.Errorf("state: latest pointer names generation %d but %s does not exist", generation, tarKey)
	}
	s.mu.Lock()
	if generation > s.baseGeneration {
		s.baseGeneration = generation
	}
	s.mu.Unlock()
	return tarball, generation, true, nil
}

// SeedGeneration reports the highest generation this process has observed for a foreign latest
// pointer, without performing a restore. Finding #2 (core-robustness round 3): on startup, a
// process whose local state dir survived a restart (so RestoreLatest for local disk is never
// called, or a non-S3 backend restored local state) must still learn the S3 "latest" generation,
// or its nextGen counter can collide with a generation another writer already has committed to
// S3. Callers should invoke this unconditionally at startup when the snapshot backend is S3,
// regardless of whether local state was restored.
func (s *S3Snapshotter) SeedGeneration(ctx context.Context) (int64, error) {
	latestKey := s.objectKey("latest")
	body, ok, err := s.get(ctx, latestKey)
	if err != nil {
		return 0, fmt.Errorf("fetching %s: %w", latestKey, err)
	}
	if !ok {
		return 0, nil
	}
	generation, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing generation from %s: %w", latestKey, err)
	}
	s.mu.Lock()
	if generation > s.baseGeneration {
		s.baseGeneration = generation
	}
	seeded := s.baseGeneration
	s.mu.Unlock()
	return seeded, nil
}

// put performs a SigV4-signed PUT of body to key.
func (s *S3Snapshotter) put(ctx context.Context, key string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	if err := s.sign(req, body); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PUT %s: status %d: %s", key, resp.StatusCode, string(respBody))
	}
	return nil
}

// get performs a SigV4-signed GET of key. A 404 is reported as (nil, false, nil), not an error.
func (s *S3Snapshotter) get(ctx context.Context, key string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, false, err
	}
	if err := s.sign(req, nil); err != nil {
		return nil, false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, false, fmt.Errorf("GET %s: status %d: %s", key, resp.StatusCode, string(respBody))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// --- AWS SigV4 (hand-rolled, stdlib only; https://docs.aws.amazon.com/general/latest/gr/sigv4-signing-flow.html) ---

const (
	awsAlgorithm = "AWS4-HMAC-SHA256"
	awsService   = "s3"
)

// sign SigV4-signs req in place (adds X-Amz-Date, X-Amz-Content-Sha256, and Authorization
// headers), using body's SHA-256 as the payload hash. Region defaults to "us-east-1" when unset
// (MinIO and most S3-compatible stores accept any region string).
func (s *S3Snapshotter) sign(req *http.Request, body []byte) error {
	region := s.cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	req.Header.Set("Host", req.Host)

	signedHeaders, canonicalHeaders := canonicalHeaderSet(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, region, awsService, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(s.cfg.SecretKey, dateStamp, region, awsService)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	authHeader := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		awsAlgorithm, s.cfg.AccessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
	return nil
}

// canonicalURI URI-encodes each path segment per SigV4 rules while leaving "/" separators intact.
// The tar/latest object keys this package ever generates contain no characters needing encoding
// beyond what net/url's PathEscape already handles per-segment, so this is intentionally simple
// rather than a full byte-perfect SigV4 URI-encoder.
func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = pathEscape(seg)
	}
	return strings.Join(segments, "/")
}

func pathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalHeaderSet returns (signedHeaders, canonicalHeaders) for host + x-amz-date +
// x-amz-content-sha256, the minimal signed-header set this client ever sends.
func canonicalHeaderSet(req *http.Request) (signedHeaders, canonicalHeaders string) {
	type kv struct{ k, v string }
	headers := []kv{
		{"host", req.Host},
		{"x-amz-content-sha256", req.Header.Get("X-Amz-Content-Sha256")},
		{"x-amz-date", req.Header.Get("X-Amz-Date")},
	}
	// Already alphabetically sorted (host < x-amz-content-sha256 < x-amz-date).
	var names []string
	var canon strings.Builder
	for _, h := range headers {
		names = append(names, h.k)
		canon.WriteString(h.k)
		canon.WriteString(":")
		canon.WriteString(strings.TrimSpace(h.v))
		canon.WriteString("\n")
	}
	return strings.Join(names, ";"), canon.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func deriveSigningKey(secretKey, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// --- state-dir tarball build/extract ---

// BuildStateTarball tars stateFiles (cursor.json, jobs.journal) found under stateDir into a single
// uncompressed tar archive. A file in stateFiles that does not exist (e.g. a fresh install with no
// journal yet) is silently skipped, not an error -- Upload should still be able to snapshot
// whatever partial state exists. agent-key.json is never included regardless of what's in
// stateDir (stateFiles simply never names it) -- see this file's package doc for why that matters.
func BuildStateTarball(stateDir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range stateFiles {
		fullPath := filepath.Join(stateDir, name)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", fullPath, err)
		}
		hdr := &tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("writing tar header for %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("writing tar body for %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar writer: %w", err)
	}
	return buf.Bytes(), nil
}

// ExtractStateTarball writes tarball's entries into stateDir, restoring cursor.json/jobs.journal
// from a snapshot. Entry names are restricted to filepath.Base of themselves before joining with
// stateDir, so a maliciously (or corruptly) crafted tarball entry name can never escape stateDir
// via ".." path traversal (defense in depth -- this content is trusted server-side infrastructure,
// not user input, but crossing a network+deserialization boundary warrants the guard anyway).
func ExtractStateTarball(stateDir string, tarball []byte) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir %s: %w", stateDir, err)
	}
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." || name == "" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("reading tar body for %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, name), data, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}
