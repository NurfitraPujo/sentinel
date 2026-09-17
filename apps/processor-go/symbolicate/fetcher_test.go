package symbolicate

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Addressing-style guard for the read side. Both clients hard-coded path-style until the first
// production sourcemap upload failed 4/4 against Huawei OBS ("VirtualHostDomainRequired",
// sentinel-production, 2026-09-17); the dashboard's put failed first, and this fetcher would have
// failed the GET identically. The style is now decided from the endpoint host, with
// S3_FORCE_PATH_STYLE as an explicit override -- the same rule as storage.ts's usePathStyle.

func TestUsePathStyle_HostRule(t *testing.T) {
	cases := []struct {
		host string
		path bool
	}{
		// MinIO in compose/dev/tests: bare hostname, localhost, IP literal -> path-style.
		{"minio:9000", true},
		{"minio", true},
		{"localhost:9000", true},
		{"127.0.0.1:9000", true},
		{"[::1]:9000", true},
		{"[::1]", true},
		// Hosted stores: dotted hostname -> virtual-hosted (OBS requires it; S3/R2 accept it).
		{"obs.ap-southeast-3.myhuaweicloud.com", false},
		{"obs.ap-southeast-3.myhuaweicloud.com:443", false},
		{"s3.us-east-1.amazonaws.com", false},
		{"uploads.example.test:9000", false},
	}
	for _, c := range cases {
		got, err := UsePathStyle(c.host, "")
		if err != nil {
			t.Fatalf("UsePathStyle(%q): %v", c.host, err)
		}
		if got != c.path {
			t.Errorf("UsePathStyle(%q) = %v, want %v", c.host, got, c.path)
		}
	}
}

func TestUsePathStyle_OverrideWins(t *testing.T) {
	for _, v := range []string{"true", "1", " TRUE "} {
		if got, err := UsePathStyle("obs.ap-southeast-3.myhuaweicloud.com", v); err != nil || !got {
			t.Errorf("override %q on a dotted host: got (%v, %v), want (true, nil)", v, got, err)
		}
	}
	for _, v := range []string{"false", "0", "False"} {
		if got, err := UsePathStyle("minio:9000", v); err != nil || got {
			t.Errorf("override %q on a bare host: got (%v, %v), want (false, nil)", v, got, err)
		}
	}
	_, err := UsePathStyle("minio:9000", "yes")
	if err == nil || !strings.Contains(err.Error(), EnvS3ForcePathStyle) {
		t.Fatalf("a non-boolean override must fail naming %s, got %v", EnvS3ForcePathStyle, err)
	}
	if _, err := UsePathStyle("", ""); err == nil {
		t.Fatal("an empty host must be an error")
	}
}

// TestNewS3Fetcher_AddressingStyle pins the decision THROUGH the request the client sends: the
// pure rule being right is worthless if NewS3Fetcher still passes BucketLookupPath, and a mirror
// field on the fetcher would stay green in exactly that case. An httptest.Server stands in for
// the store; a dialer routes every host (including `bucket.obs.example.test`) to it, so the
// dotted virtual-hosted case resolves without DNS. Virtual-hosted shows up as Host
// `<bucket>.<endpoint host>`; path-style as a request path under `/<bucket>/`.
func TestNewS3Fetcher_AddressingStyle(t *testing.T) {
	const bucket = "sentinel-attachments"
	var mu sync.Mutex
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost, gotPath = r.Host, r.URL.Path
		mu.Unlock()
		// The minimum the minio client parses out of a GET response.
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"0"`)
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	srvAddr := srv.Listener.Addr().String()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srvAddr)
		},
	}

	cases := []struct {
		name     string
		host     string // endpoint host as the deployment would set it
		override string
		wantHost string // "" = path-style: Host is the endpoint, path starts with /<bucket>/
	}{
		{"minio is path-style", srvAddr, "", ""},
		{"obs is virtual-hosted", "obs.example.test", "", bucket + ".obs.example.test"},
		{"override forces path-style on obs", "obs.example.test", "true", ""},
		{"override forces virtual-hosted on minio", srvAddr, "false", bucket + "." + srvAddr},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := S3Config{Endpoint: "http://" + c.host, Bucket: bucket, AccessKey: "k", SecretKey: "s",
				ForcePathStyle: c.override, transport: transport}
			f, err := NewS3Fetcher(cfg, DefaultMaxParseBytes)
			if err != nil {
				t.Fatalf("NewS3Fetcher: %v", err)
			}
			if _, err := f.Fetch(context.Background(), "maps/a.map"); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			mu.Lock()
			host, path := gotHost, gotPath
			mu.Unlock()
			if c.wantHost != "" {
				if host != c.wantHost {
					t.Errorf("virtual-hosted: Host = %q, want %q (path %q)", host, c.wantHost, path)
				}
				if strings.HasPrefix(path, "/"+bucket+"/") {
					t.Errorf("virtual-hosted: path %q must not carry the bucket", path)
				}
			} else {
				if host != c.host {
					t.Errorf("path-style: Host = %q, want the bare endpoint %q", host, c.host)
				}
				if !strings.HasPrefix(path, "/"+bucket+"/") {
					t.Errorf("path-style: path = %q, want a /%s/ prefix", path, bucket)
				}
			}
		})
	}

	cfg := S3Config{Endpoint: "http://minio:9000", Bucket: bucket, AccessKey: "k", SecretKey: "s", ForcePathStyle: "maybe"}
	if _, err := NewS3Fetcher(cfg, DefaultMaxParseBytes); err == nil || !strings.Contains(err.Error(), EnvS3ForcePathStyle) {
		t.Fatalf("a bad override must be a constructor (startup) error naming the variable, got %v", err)
	}
}

func TestS3ConfigFromEnv_ReadsForcePathStyle(t *testing.T) {
	env := map[string]string{"S3_ENDPOINT": "http://minio:9000", "S3_FORCE_PATH_STYLE": " false "}
	cfg := S3ConfigFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if cfg.ForcePathStyle != "false" {
		t.Fatalf("ForcePathStyle = %q, want %q (trimmed)", cfg.ForcePathStyle, "false")
	}
}
