package symbolicate

import (
	"strings"
	"testing"
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

// TestNewS3Fetcher_AddressingStyle pins the decision THROUGH the constructor, on the fields
// S3ConfigFromEnv fills: the pure rule being right is worthless if NewS3Fetcher still passes
// BucketLookupPath.
func TestNewS3Fetcher_AddressingStyle(t *testing.T) {
	base := S3Config{Bucket: "sentinel-attachments", AccessKey: "k", SecretKey: "s"}
	cases := []struct {
		name     string
		endpoint string
		override string
		path     bool
	}{
		{"minio is path-style", "http://minio:9000", "", true},
		{"obs is virtual-hosted", "https://obs.ap-southeast-3.myhuaweicloud.com", "", false},
		{"override forces path-style on obs", "https://obs.ap-southeast-3.myhuaweicloud.com", "true", true},
		{"override forces virtual-hosted on minio", "http://minio:9000", "false", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			cfg.Endpoint, cfg.ForcePathStyle = c.endpoint, c.override
			f, err := NewS3Fetcher(cfg, DefaultMaxParseBytes)
			if err != nil {
				t.Fatalf("NewS3Fetcher: %v", err)
			}
			if f.PathStyle() != c.path {
				t.Errorf("PathStyle() = %v, want %v", f.PathStyle(), c.path)
			}
		})
	}

	cfg := base
	cfg.Endpoint, cfg.ForcePathStyle = "http://minio:9000", "maybe"
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
