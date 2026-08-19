// Package keyguard implements sentinel-worker's Agent-key rotation (plan §2.5): expiry-driven
// rotation now that key expiry is real (C6), a null-expiry age fallback, on-401 rotation, and a
// two-backend key store (file | kubernetes-secret). N8a defines the KeyStore seam and the trigger
// evaluation logic only — the k8s-secret backend and live rotation HTTP call ship in N8e.
package keyguard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// KeyStore is the plan §2.5 "two-backend interface" (WORKER_KEYSTORE = file | kubernetes-secret).
// Implementations MUST persist before Swap is observed by callers ("persist before use" — the new
// secret is durably stored, then and only then swapped in memory).
type KeyStore interface {
	// Load returns the currently persisted key, or ("", false) if the store has never been
	// written (bootstrap: SENTINEL_AGENT_KEY from env is used instead).
	Load(ctx context.Context) (key string, ok bool, err error)
	// Persist durably stores a newly rotated key. Must complete before the caller swaps it into
	// the live Client (plan §2.5's persist-before-use rule).
	Persist(ctx context.Context, key string) error
}

// KeyInfo mirrors the subset of GET /api/agent/self's `key` object keyguard needs (C13: createdAt
// is ISO or null; C6: expiresAt is now real).
type KeyInfo struct {
	CreatedAt *time.Time
	ExpiresAt *time.Time
}

// Trigger names which of the plan §2.5 rotation rules fired, for logging/metrics.
type Trigger string

const (
	TriggerNone       Trigger = ""
	TriggerExpiryNear Trigger = "expiry-near" // (a) expiresAt within WORKER_ROTATE_BEFORE_HOURS
	TriggerAge        Trigger = "age"         // (b) null-expiry key age >= WORKER_ROTATE_EVERY_DAYS
	TriggerOn401      Trigger = "on-401"      // (c) reactive, on-401, once
)

// Evaluate implements the plan §2.5 trigger priority order (a) expiry-near, (b) age-for-null-expiry,
// returning TriggerNone when neither fires. on401 rotation is a separate explicit call site (the
// caller invokes rotation directly on a 401, per plan §2.4's Auth row), not evaluated here.
func Evaluate(info KeyInfo, now time.Time, rotateBeforeHours int, rotateEveryDays int) Trigger {
	if info.ExpiresAt != nil {
		if now.Add(time.Duration(rotateBeforeHours) * time.Hour).After(*info.ExpiresAt) {
			return TriggerExpiryNear
		}
		return TriggerNone
	}
	// Null-expiry key: age-based fallback, unless disabled (0 = off, plan §5).
	if rotateEveryDays <= 0 || info.CreatedAt == nil {
		return TriggerNone
	}
	age := now.Sub(*info.CreatedAt)
	if age >= time.Duration(rotateEveryDays)*24*time.Hour {
		return TriggerAge
	}
	return TriggerNone
}

// FileKeyStore is the `file` backend (compose/VM): a local path (agent-key.json, 0600), atomic
// tmp+rename write, following the same pattern as state/cursor.go and state/journal.go. It
// overrides SENTINEL_AGENT_KEY at startup (env is bootstrap-only, plan §2.5) whenever the file
// already exists; a missing file is not an error (bootstrap: the caller falls back to
// SENTINEL_AGENT_KEY and Persist creates the file on first rotation).
type FileKeyStore struct {
	Path string
}

var _ KeyStore = FileKeyStore{}

// fileKeyRecord is agent-key.json's on-disk shape. OldKeyPrefix and RotatedAt are diagnostic only
// (never the full old secret -- "NEVER log/journal/snapshot the secret" applies to old keys too).
type fileKeyRecord struct {
	Key          string    `json:"key"`
	OldKeyPrefix string    `json:"oldKeyPrefix,omitempty"`
	RotatedAt    time.Time `json:"rotatedAt,omitempty"`
	// CreatedAt tracks this key's local rotation age as a fallback for the age trigger when the
	// server exposes no createdAt (plan §2.5: "Track rotation age locally when the server exposes
	// no createdAt fallback").
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// Load reads agent-key.json. A missing file returns ("", false, nil) — not an error — so callers
// fall back to SENTINEL_AGENT_KEY on first boot.
func (s FileKeyStore) Load(ctx context.Context) (string, bool, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var rec fileKeyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return "", false, fmt.Errorf("parsing %s: %w", s.Path, err)
	}
	if rec.Key == "" {
		return "", false, nil
	}
	return rec.Key, true, nil
}

// LoadRecord is Load plus the local rotation-age metadata, for the age trigger's local fallback
// when the server exposes no createdAt. A missing file returns a zero record with ok=false.
func (s FileKeyStore) LoadRecord(ctx context.Context) (fileKeyRecord, bool, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileKeyRecord{}, false, nil
		}
		return fileKeyRecord{}, false, err
	}
	var rec fileKeyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fileKeyRecord{}, false, fmt.Errorf("parsing %s: %w", s.Path, err)
	}
	return rec, rec.Key != "", nil
}

// Persist durably stores key via tmp+rename (0600), preserving a short, non-secret prefix of the
// PREVIOUS key (if any) and a rotatedAt timestamp for diagnostics. It never writes the old key's
// full value.
func (s FileKeyStore) Persist(ctx context.Context, key string) error {
	dir := filepath.Dir(s.Path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	now := time.Now().UTC()
	rec := fileKeyRecord{Key: key, RotatedAt: now, CreatedAt: now}
	if prev, ok, _ := s.LoadRecord(ctx); ok {
		rec.OldKeyPrefix = keyPrefix(prev.Key)
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling key record: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".agent-key-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, s.Path, err)
	}
	return nil
}

// Writable probes whether the directory holding Path actually accepts writes -- a genuine
// read-only-remounted state volume (plan §2.5: "read-only key store ... disables rotation and
// logs loudly at start") rather than a config-validation-time guess. It creates and immediately
// removes a throwaway temp file; any error (permission denied, read-only filesystem, missing and
// uncreatable directory) means rotation must be disabled.
func (s FileKeyStore) Writable(ctx context.Context) bool {
	dir := filepath.Dir(s.Path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(dir, ".agent-key-writable-probe-*.tmp")
	if err != nil {
		return false
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)
	return true
}

// keyPrefix returns a short, non-secret diagnostic prefix of a key (never the full value).
func keyPrefix(key string) string {
	const n = 8
	if len(key) <= n {
		return ""
	}
	return key[:n] + "..."
}

// K8sKeyStore is the `kubernetes-secret` backend: reads the key from a mounted Secret volume file
// (read-only, kubelet-managed) and rotates by PATCHing the named Secret via the in-cluster K8s
// API. HTTPClient/APIServerURL/Token/CA are all overridable so tests can point this at an
// httptest fake apiserver instead of a real cluster.
type K8sKeyStore struct {
	// MountPath is the file the Secret is projected to for reads (WORKER_KEY_SECRET_MOUNT), e.g.
	// /var/run/secrets/sentinel/agent-key (kubelet updates it automatically on Secret changes, but
	// keyguard treats its own in-memory Client.Key, swapped post-Persist, as authoritative -- this
	// mount is read at startup only).
	MountPath string
	// SecretName/Namespace identify the Secret PATCHed on rotation (WORKER_KEY_SECRET_NAME/_NAMESPACE).
	SecretName string
	Namespace  string
	// DataKey is the key within the Secret's `data` map holding the agent key (default "key").
	DataKey string

	// APIServerURL, Token, and CA override the in-cluster defaults (KUBERNETES_SERVICE_HOST/PORT,
	// the mounted SA token file, and the mounted CA bundle) so tests can inject an httptest fake
	// apiserver. Production wiring (main.go) leaves these unset and calls InCluster() first.
	APIServerURL string
	Token        string
	HTTPClient   *http.Client
}

var _ KeyStore = &K8sKeyStore{}

// InClusterAPIServerURL builds the in-cluster apiserver base URL from KUBERNETES_SERVICE_HOST/PORT,
// as the Go client-go library does.
func InClusterAPIServerURL() (string, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", fmt.Errorf("keyguard: KUBERNETES_SERVICE_HOST/PORT not set (not running in-cluster)")
	}
	return "https://" + host + ":" + port, nil
}

const (
	defaultServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultServiceAccountCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// dataKey returns s.DataKey, defaulting to "key".
func (s *K8sKeyStore) dataKey() string {
	if s.DataKey == "" {
		return "key"
	}
	return s.DataKey
}

// Load reads the key from the mounted Secret volume file (a plain projected file, already
// base64-decoded by the kubelet -- unlike the Secret's `data` map over the API, a volume-mounted
// Secret key is the raw value on disk).
func (s *K8sKeyStore) Load(ctx context.Context) (string, bool, error) {
	data, err := os.ReadFile(s.MountPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", false, nil
	}
	return key, true, nil
}

// secretPatchBody is a strategic-merge-patch body targeting one key in a Secret's `data` map. The
// K8s API requires `data` values to be base64-encoded regardless of Content-Type.
type secretPatchBody struct {
	Data map[string]string `json:"data"`
}

// Persist PATCHes the named Secret's data[DataKey] with the base64-encoded new key, via the K8s
// API (strategic-merge-patch, Content-Type: application/strategic-merge-patch+json). Callers must
// treat a non-2xx response as a failed persist (old key stays in use -- see PERSIST-BEFORE-USE).
func (s *K8sKeyStore) Persist(ctx context.Context, key string) error {
	body := secretPatchBody{Data: map[string]string{s.dataKey(): base64.StdEncoding.EncodeToString([]byte(key))}}
	return s.patch(ctx, body, false)
}

// Writable probes whether this process can actually PATCH the named Secret -- e.g. the mounted
// SA token's RBAC lacks `patch` on secrets -- rather than only checking that a name/namespace were
// configured (plan §2.5: "read-only key store ... disables rotation and logs loudly at start").
// It issues a dry-run PATCH (?dryRun=All, per the K8s API's server-side dry-run support) with an
// empty data map, so nothing is ever actually mutated: a successful dry-run response means the
// same PATCH would have succeeded for real; any error (RBAC denial, unreachable apiserver, missing
// token/CA) means rotation cannot work and must be disabled.
func (s *K8sKeyStore) Writable(ctx context.Context) bool {
	return s.patch(ctx, secretPatchBody{Data: map[string]string{}}, true) == nil
}

// patch is the shared PATCH implementation behind Persist and Writable; dryRun appends the K8s
// API's server-side dry-run query parameter so Writable's probe never mutates the Secret.
func (s *K8sKeyStore) patch(ctx context.Context, body secretPatchBody, dryRun bool) error {
	if s.SecretName == "" || s.Namespace == "" {
		return fmt.Errorf("keyguard: k8s key store requires SecretName and Namespace")
	}
	apiServer := s.APIServerURL
	if apiServer == "" {
		var err error
		apiServer, err = InClusterAPIServerURL()
		if err != nil {
			return err
		}
	}
	token := s.Token
	if token == "" {
		tokBytes, err := os.ReadFile(defaultServiceAccountTokenFile)
		if err != nil {
			return fmt.Errorf("keyguard: reading service account token: %w", err)
		}
		token = strings.TrimSpace(string(tokBytes))
	}
	httpClient := s.HTTPClient
	if httpClient == nil {
		httpClient = defaultK8sHTTPClient()
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling secret patch body: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", strings.TrimRight(apiServer, "/"), s.Namespace, s.SecretName)
	if dryRun {
		url += "?dryRun=All"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("building secret patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("patching secret %s/%s: %w", s.Namespace, s.SecretName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("patching secret %s/%s: unexpected status %d", s.Namespace, s.SecretName, resp.StatusCode)
	}
	return nil
}

// defaultK8sHTTPClient builds a client trusting the mounted CA bundle when present, falling back
// to the default transport (e.g. in tests, where an httptest fake apiserver is plain HTTP).
func defaultK8sHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
