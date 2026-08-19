package keyguard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileKeyStore_LoadMissingIsNotError(t *testing.T) {
	dir := t.TempDir()
	store := FileKeyStore{Path: filepath.Join(dir, "agent-key.json")}
	key, ok, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || key != "" {
		t.Fatalf("expected (\"\", false) for missing file, got (%q, %v)", key, ok)
	}
}

func TestFileKeyStore_PersistThenLoadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-key.json")
	store := FileKeyStore{Path: path}

	if err := store.Persist(context.Background(), "sk-live-first-secret-value"); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	key, ok, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok || key != "sk-live-first-secret-value" {
		t.Fatalf("expected round-tripped key, got (%q, %v)", key, ok)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}
}

// TestFileKeyStore_PersistIsAtomic proves the tmp+rename pattern: no partial file is ever visible
// at the final path -- the rename target either has the old content or the fully-written new
// content, never a half-write.
func TestFileKeyStore_PersistIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-key.json")
	store := FileKeyStore{Path: path}

	if err := store.Persist(context.Background(), "first-key"); err != nil {
		t.Fatalf("Persist 1: %v", err)
	}
	if err := store.Persist(context.Background(), "second-key"); err != nil {
		t.Fatalf("Persist 2: %v", err)
	}

	// No leftover .tmp files after a successful Persist.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover tmp file after successful Persist: %s", e.Name())
		}
	}

	key, ok, err := store.Load(context.Background())
	if err != nil || !ok || key != "second-key" {
		t.Fatalf("expected second-key after two persists, got (%q, %v, %v)", key, ok, err)
	}
}

// TestFileKeyStore_PersistRecordsOldKeyPrefixNotFullValue proves the on-disk record never carries
// the full previous secret -- only a short diagnostic prefix.
func TestFileKeyStore_PersistRecordsOldKeyPrefixNotFullValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-key.json")
	store := FileKeyStore{Path: path}

	oldKey := "sk-live-abcdefghijklmnopqrstuvwxyz0123456789"
	if err := store.Persist(context.Background(), oldKey); err != nil {
		t.Fatalf("Persist 1: %v", err)
	}
	if err := store.Persist(context.Background(), "sk-live-newnewnewnewnewnewnew"); err != nil {
		t.Fatalf("Persist 2: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), oldKey) {
		t.Fatalf("on-disk record must never contain the full old secret; got: %s", raw)
	}
	var rec fileKeyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.OldKeyPrefix == "" || !strings.HasPrefix(oldKey, strings.TrimSuffix(rec.OldKeyPrefix, "...")) {
		t.Fatalf("expected a diagnostic prefix of the old key, got %q", rec.OldKeyPrefix)
	}
}

func TestK8sKeyStore_LoadFromMountedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-key")
	if err := os.WriteFile(path, []byte("mounted-secret-value\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := &K8sKeyStore{MountPath: path}
	key, ok, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok || key != "mounted-secret-value" {
		t.Fatalf("expected trimmed mounted value, got (%q, %v)", key, ok)
	}
}

func TestK8sKeyStore_LoadMissingMountIsNotError(t *testing.T) {
	store := &K8sKeyStore{MountPath: filepath.Join(t.TempDir(), "missing")}
	key, ok, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || key != "" {
		t.Fatalf("expected (\"\", false) for missing mount, got (%q, %v)", key, ok)
	}
}

// TestK8sKeyStore_PersistPATCHesWithBase64AndAuthHeader proves the k8s backend's PATCH body
// base64-encodes the new secret and carries a bearer auth header, against an httptest fake
// apiserver (plan §2.5's explicit test requirement).
func TestK8sKeyStore_PersistPATCHesWithBase64AndAuthHeader(t *testing.T) {
	const newKey = "sk-live-rotated-secret-value"
	const token = "fake-service-account-token"

	var gotMethod, gotPath, gotAuth string
	var gotBody secretPatchBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding patch body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	store := &K8sKeyStore{
		SecretName:   "agent-key",
		Namespace:    "sentinel",
		APIServerURL: srv.URL,
		Token:        token,
		HTTPClient:   srv.Client(),
	}
	if err := store.Persist(context.Background(), newKey); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Fatalf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/api/v1/namespaces/sentinel/secrets/agent-key" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("expected bearer auth header, got %q", gotAuth)
	}
	got, ok := gotBody.Data["key"]
	if !ok {
		t.Fatalf("expected data[\"key\"] in patch body, got %+v", gotBody.Data)
	}
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decoding base64 patch value: %v", err)
	}
	if string(decoded) != newKey {
		t.Fatalf("expected base64(%q), got decoded %q", newKey, decoded)
	}
}

func TestK8sKeyStore_PersistFailsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := &K8sKeyStore{
		SecretName:   "agent-key",
		Namespace:    "sentinel",
		APIServerURL: srv.URL,
		Token:        "tok",
		HTTPClient:   srv.Client(),
	}
	if err := store.Persist(context.Background(), "whatever"); err == nil {
		t.Fatal("expected error on 403 response")
	}
}

func TestK8sKeyStore_PersistUsesCustomDataKey(t *testing.T) {
	var gotBody secretPatchBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &K8sKeyStore{
		SecretName:   "agent-key",
		Namespace:    "sentinel",
		DataKey:      "agentKey",
		APIServerURL: srv.URL,
		Token:        "tok",
		HTTPClient:   srv.Client(),
	}
	if err := store.Persist(context.Background(), "value"); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if _, ok := gotBody.Data["agentKey"]; !ok {
		t.Fatalf("expected custom data key \"agentKey\" in body, got %+v", gotBody.Data)
	}
}

// TestFileKeyStore_WritableTrueForNormalDir proves a normal, writable state directory reports
// writable (the common case, so ReadOnly stays false and rotation proceeds).
func TestFileKeyStore_WritableTrueForNormalDir(t *testing.T) {
	dir := t.TempDir()
	store := FileKeyStore{Path: filepath.Join(dir, "agent-key.json")}
	if !store.Writable(context.Background()) {
		t.Fatal("expected a normal temp dir to be writable")
	}
}

// TestFileKeyStore_WritableFalseForReadOnlyDir proves a genuinely read-only-remounted state
// directory is detected (plan §2.5: "read-only key store ... disables rotation and logs loudly at
// start" must be reachable for a real read-only mount, not just an unreachable code path).
func TestFileKeyStore_WritableFalseForReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore so t.TempDir()'s cleanup can remove it

	store := FileKeyStore{Path: filepath.Join(dir, "agent-key.json")}
	if store.Writable(context.Background()) {
		t.Fatal("expected a read-only directory to report NOT writable")
	}
}

// TestK8sKeyStore_WritableSendsDryRunPATCHAndDoesNotMutate proves Writable probes with a
// server-side dry-run PATCH (so nothing is ever actually written) and reports true/false from the
// response status, mirroring what a real RBAC-denied PATCH would return.
func TestK8sKeyStore_WritableSendsDryRunPATCHAndDoesNotMutate(t *testing.T) {
	var gotQuery string
	var gotBody secretPatchBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &K8sKeyStore{
		SecretName:   "agent-key",
		Namespace:    "sentinel",
		APIServerURL: srv.URL,
		Token:        "tok",
		HTTPClient:   srv.Client(),
	}
	if !store.Writable(context.Background()) {
		t.Fatal("expected Writable to report true on a 200 dry-run response")
	}
	if !strings.Contains(gotQuery, "dryRun=All") {
		t.Fatalf("expected the dry-run query parameter, got query %q", gotQuery)
	}
	if len(gotBody.Data) != 0 {
		t.Fatalf("expected an empty data map on the writability probe (never mutate a real value), got %+v", gotBody.Data)
	}
}

// TestK8sKeyStore_WritableFalseOnRBACDenial proves an RBAC-denied dry-run PATCH (403) reports
// not-writable, so keyguard can disable rotation before ever attempting a real one.
func TestK8sKeyStore_WritableFalseOnRBACDenial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := &K8sKeyStore{
		SecretName:   "agent-key",
		Namespace:    "sentinel",
		APIServerURL: srv.URL,
		Token:        "tok",
		HTTPClient:   srv.Client(),
	}
	if store.Writable(context.Background()) {
		t.Fatal("expected Writable to report false on a 403 dry-run response")
	}
}
