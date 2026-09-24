package unit

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/geoip/refresh"
)

// sentinel-event-enrichment §8.2 / §10.2 TASK-05 "zero MaxMind downloads from pods", cases (b)-(f): the
// refresh job over a counting fake MaxMind (client.WithEndpoint) and a fake S3 reached through the
// REAL minio-go client.

const refreshLicenseKey = "lic-SECRET-do-not-log-9f8e7d"

// fakeMaxMind serves the metadata and download endpoints geoipupdate/v8's client calls.
type fakeMaxMind struct {
	mu        sync.Mutex
	md5       string // what the metadata endpoint claims
	db        []byte // what the download endpoint serves
	metadata  int
	downloads int
}

func (f *fakeMaxMind) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		user, pass, ok := r.BasicAuth()
		if !ok || user != "123456" || pass != refreshLicenseKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/geoip/updates/metadata":
			f.metadata++
			assert.Equal(t, "GeoLite2-City", r.URL.Query().Get("edition_id"))
			fmt.Fprintf(w, `{"databases":[{"date":"2026-02-04","edition_id":"GeoLite2-City","md5":%q}]}`, f.md5)
		case r.URL.Path == "/geoip/databases/GeoLite2-City/download":
			f.downloads++
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: "GeoLite2-City_20260204/GeoLite2-City.mmdb", Mode: 0o644, Size: int64(len(f.db))}))
			_, _ = tw.Write(f.db)
			require.NoError(t, tw.Close())
			require.NoError(t, gz.Close())
			w.Header().Set("Last-Modified", "Wed, 04 Feb 2026 22:49:29 GMT")
			_, _ = w.Write(buf.Bytes())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeMaxMind) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metadata, f.downloads
}

// fakeRefreshS3 holds at most one object. Its stat answers the stored metadata as X-Amz-Meta-* headers,
// which minio-go turns into UserMetadata keys like "Maxmind-Md5".
type fakeRefreshS3 struct {
	mu      sync.Mutex
	obj     []byte
	meta    map[string]string // header name -> value, e.g. "X-Amz-Meta-Maxmind-Md5"
	statErr bool
	puts    int
	putMeta http.Header
}

func (f *fakeRefreshS3) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Equal(t, "/sentinel-staging/geoip/GeoLite2-City.mmdb", r.URL.Path)
		switch r.Method {
		case http.MethodHead:
			if f.statErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if f.obj == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			for k, v := range f.meta {
				w.Header().Set(k, v)
			}
			w.Header().Set("ETag", `"stored"`)
			w.Header().Set("Last-Modified", "Wed, 04 Feb 2026 22:49:29 GMT")
			w.Header().Set("Content-Length", strconv.Itoa(len(f.obj)))
		case http.MethodPut:
			f.puts++
			f.putMeta = r.Header.Clone()
			body, err := readS3Body(r)
			require.NoError(t, err)
			f.obj = body
			w.Header().Set("ETag", `"new"`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// readS3Body decodes an aws-chunked (streaming SigV4) body when minio-go sends one over plain HTTP.
func readS3Body(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	var out bytes.Buffer
	br := bufio.NewReader(r.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, err
		}
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

type refreshRig struct {
	mm  *fakeMaxMind
	s3  *fakeRefreshS3
	env map[string]string
	log *geoLog
	mmU string
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()
	rig := &refreshRig{mm: &fakeMaxMind{}, s3: &fakeRefreshS3{}, log: &geoLog{}}
	mmSrv := httptest.NewServer(rig.mm.handler(t))
	t.Cleanup(mmSrv.Close)
	s3Srv := httptest.NewServer(rig.s3.handler(t))
	t.Cleanup(s3Srv.Close)
	rig.mmU = mmSrv.URL
	rig.env = map[string]string{
		"MAXMIND_ACCOUNT_ID": "123456", "MAXMIND_LICENSE_KEY": refreshLicenseKey,
		"GEOIP_S3_ENDPOINT": s3Srv.URL, "GEOIP_S3_BUCKET": "sentinel-staging", "GEOIP_S3_PREFIX": "geoip/",
		"GEOIP_S3_ACCESS_KEY": "ak", "GEOIP_S3_SECRET_KEY": "sk",
	}
	return rig
}

func (r *refreshRig) run(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := refresh.Run(ctx, slog.New(slog.NewJSONHandler(r.log, nil)), refresh.Options{
		Lookup:          func(k string) (string, bool) { v, ok := r.env[k]; return v, ok },
		MaxMindEndpoint: r.mmU,
	})
	r.log.mu.Lock()
	logged := r.log.buf.String()
	r.log.mu.Unlock()
	assert.NotContains(t, logged, refreshLicenseKey, "the licence key is never logged")
	return code
}

func md5Hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }

// (b) The stored object's MD5 matches MaxMind's: one metadata request, zero downloads, no PutObject.
// The fake returns it as X-Amz-Meta-Maxmind-Md5, which minio-go keys as "Maxmind-Md5", so a
// case-sensitive lookup of "maxmind-md5" would miss it and download every run.
func TestGeoIPRefreshUpToDate(t *testing.T) {
	rig := newRefreshRig(t)
	db := geoFixture(t, "GeoLite2-City-Test.mmdb")
	rig.mm.md5, rig.mm.db = md5Hex(db), db
	rig.s3.obj = db
	rig.s3.meta = map[string]string{"X-Amz-Meta-Maxmind-Md5": md5Hex(db)}

	assert.Equal(t, 0, rig.run(t))
	meta, downloads := rig.mm.counts()
	assert.Equal(t, 1, meta)
	assert.Equal(t, 0, downloads)
	assert.Equal(t, 0, rig.s3.puts)
	assert.Contains(t, rig.log.buf.String(), "geoip-refresh: up to date")
}

// (c) A newer file: exactly one download and one PutObject, with the metadata the next run compares.
func TestGeoIPRefreshNewerFileUploadsOnce(t *testing.T) {
	for _, stored := range []string{"none", "older"} {
		t.Run(stored, func(t *testing.T) {
			rig := newRefreshRig(t)
			db := geoFixture(t, "GeoLite2-City-Test.mmdb")
			rig.mm.md5, rig.mm.db = md5Hex(db), db
			if stored == "older" {
				rig.s3.obj = []byte("old")
				rig.s3.meta = map[string]string{"X-Amz-Meta-Maxmind-Md5": "0123456789abcdef0123456789abcdef"}
			}

			assert.Equal(t, 0, rig.run(t))
			meta, downloads := rig.mm.counts()
			assert.Equal(t, 1, meta)
			assert.Equal(t, 1, downloads)
			require.Equal(t, 1, rig.s3.puts)
			assert.Equal(t, db, rig.s3.obj, "the uploaded bytes are the download")
			assert.Equal(t, md5Hex(db), rig.s3.putMeta.Get("X-Amz-Meta-Maxmind-Md5"))
			assert.Equal(t, strconv.FormatInt(geoBuildEpoch.Unix(), 10), rig.s3.putMeta.Get("X-Amz-Meta-Maxmind-Build-Epoch"))
			assert.Equal(t, "2026-02-04T22:49:29Z", rig.s3.putMeta.Get("X-Amz-Meta-Maxmind-Last-Modified"))
			assert.Equal(t, "application/octet-stream", rig.s3.putMeta.Get("Content-Type"))
			assert.Contains(t, rig.log.buf.String(), "geoip-refresh: uploaded")

			// The next run finds the MD5 it stored and downloads nothing.
			rig.s3.meta = map[string]string{"X-Amz-Meta-Maxmind-Md5": rig.s3.putMeta.Get("X-Amz-Meta-Maxmind-Md5")}
			assert.Equal(t, 0, rig.run(t))
			_, downloads = rig.mm.counts()
			assert.Equal(t, 1, downloads, "a second run is an update check only")
			assert.Equal(t, 1, rig.s3.puts)
		})
	}
}

// (d) No credentials: MaxMind is never contacted, exit 1 (contract decision 6).
func TestGeoIPRefreshNoCredentials(t *testing.T) {
	for _, c := range []struct{ name, account, key string }{
		{"no account", "", refreshLicenseKey}, {"no key", "123456", ""}, {"neither", "", ""},
		{"account not a positive integer", "-4", refreshLicenseKey}, {"account not a number", "abc", refreshLicenseKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newRefreshRig(t)
			rig.env["MAXMIND_ACCOUNT_ID"], rig.env["MAXMIND_LICENSE_KEY"] = c.account, c.key
			assert.Equal(t, 1, rig.run(t))
			meta, downloads := rig.mm.counts()
			assert.Equal(t, 0, meta+downloads, "0 MaxMind requests")
			assert.Equal(t, 0, rig.s3.puts)
			assert.Contains(t, rig.log.buf.String(), "MaxMind not contacted")
		})
	}
	rig := newRefreshRig(t)
	delete(rig.env, "MAXMIND_ACCOUNT_ID")
	delete(rig.env, "MAXMIND_LICENSE_KEY")
	assert.Equal(t, 1, rig.run(t))
	assert.Contains(t, rig.log.buf.String(), "geoip-refresh: MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY not set; MaxMind not contacted")
}

// (e) A stat error (not a not-found): 0 MaxMind requests, exit 1, so a download that could not be
// stored is never spent. An invalid S3 configuration is the same.
func TestGeoIPRefreshStatErrorContactsNothing(t *testing.T) {
	rig := newRefreshRig(t)
	rig.s3.statErr = true
	assert.Equal(t, 1, rig.run(t))
	meta, downloads := rig.mm.counts()
	assert.Equal(t, 0, meta+downloads)

	rig = newRefreshRig(t)
	rig.env["GEOIP_S3_FORCE_PATH_STYLE"] = "sometimes"
	assert.Equal(t, 1, rig.run(t))
	meta, downloads = rig.mm.counts()
	assert.Equal(t, 0, meta+downloads)
}

// (f) The download's MD5 differs from MaxMind's claim: nothing is uploaded. So is a download that is
// not a GeoLite2-City database.
func TestGeoIPRefreshValidatesBeforeUpload(t *testing.T) {
	db := geoFixture(t, "GeoLite2-City-Test.mmdb")
	for _, c := range []struct {
		name  string
		claim string
		body  []byte
	}{
		{"md5 mismatch", "ffffffffffffffffffffffffffffffff", db},
		{"corrupt", "", db[:len(db)/2]},
		{"wrong type", "", geoFixture(t, "GeoIP2-City-Test.mmdb")},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newRefreshRig(t)
			rig.mm.db = c.body
			rig.mm.md5 = c.claim
			if c.claim == "" {
				rig.mm.md5 = md5Hex(c.body)
			}
			assert.Equal(t, 1, rig.run(t))
			_, downloads := rig.mm.counts()
			assert.Equal(t, 1, downloads)
			assert.Equal(t, 0, rig.s3.puts, "nothing uploaded; the stored object stays the last good one")
		})
	}
}
