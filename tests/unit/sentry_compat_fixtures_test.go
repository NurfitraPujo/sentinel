package unit

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/mail"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const scFixtureDir = "../fixtures/sentry"

// scFixtureTexts returns every file under tests/fixtures/sentry as text, the compressed variants decompressed.
func scFixtureTexts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(filepath.FromSlash(scFixtureDir), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch filepath.Ext(p) {
		case ".gz":
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			raw, err = io.ReadAll(zr)
			if err != nil {
				return err
			}
		case ".zlib":
			zr, err := zlib.NewReader(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			raw, err = io.ReadAll(zr)
			if err != nil {
				return err
			}
		case ".deflate":
			var err error
			raw, err = io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
			if err != nil {
				return err
			}
		}
		out[filepath.ToSlash(p)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("found only %d fixture files under %s", len(out), scFixtureDir)
	}
	return out
}

var (
	scIPv4Re    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	scIPv6Re    = regexp.MustCompile(`(?i)[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}`)
	scEmailRe   = regexp.MustCompile(`[^\s"'<>,;()\[\]{}]+@[^\s"'<>,;()\[\]{}]+`)
	scDomainRe  = regexp.MustCompile(`(?i)^([a-z0-9-]+\.)+[a-z]{2,}$`)
	scURLHostRe = regexp.MustCompile(`(?i)https?://([^/\s"'?#\\]+)`)
	scHexRe     = regexp.MustCompile(`\b(?:[0-9a-fA-F]{16}|[0-9a-fA-F]{32}|[0-9a-fA-F]{64})\b|\buser-[0-9a-f]{8}\b`)
	scGeoRe     = regexp.MustCompile(`"geo"\s*:\s*\{[^{}]*\}`)
)

var scAllowedV4 = []string{"127.0.0.0/8", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"}
var scAllowedV6 = []string{"2001:db8::/32", "::1/128"}

func scInPrefixes(a netip.Addr, prefixes []string) bool {
	for _, p := range prefixes {
		if netip.MustParsePrefix(p).Contains(a) {
			return true
		}
	}
	return false
}

func scHostAllowed(host string) bool {
	host = strings.ToLower(host)
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, ok := range []string{"localhost", "example.com"} {
		if host == ok {
			return true
		}
	}
	for _, d := range []string{"example.invalid", "example.test"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func scDenyList(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(scFixtureDir + "/anonymization-denylist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			set[l] = true
		}
	}
	if len(set) < 8 {
		t.Fatalf("deny list has %d hashes, want at least 8", len(set))
	}
	return set
}

// scAnonymizationViolations is the check of spec §10.1, on one file's text.
func scAnonymizationViolations(text string, deny map[string]bool) []string {
	var bad []string
	for _, m := range scIPv4Re.FindAllString(text, -1) {
		a, err := netip.ParseAddr(m)
		if err == nil && !scInPrefixes(a, scAllowedV4) {
			bad = append(bad, "ipv4 "+m)
		}
	}
	for _, m := range scIPv6Re.FindAllString(text, -1) {
		a, err := netip.ParseAddr(m)
		if err == nil && a.Is6() && !scInPrefixes(a, scAllowedV6) {
			bad = append(bad, "ipv6 "+m)
		}
	}
	for _, m := range scEmailRe.FindAllString(text, -1) {
		if m == "sent_live_fixture@ingest.example.invalid" || strings.HasPrefix(m, "sent_live_fixture@ingest.example.invalid/") {
			continue // the envelope header's fabricated dsn
		}
		addr, err := mail.ParseAddress(m)
		if err != nil {
			continue
		}
		if i := strings.LastIndexByte(addr.Address, '@'); i >= 0 && scDomainRe.MatchString(addr.Address[i+1:]) {
			bad = append(bad, "email "+m)
		}
	}
	for _, m := range scURLHostRe.FindAllStringSubmatch(text, -1) {
		if !scHostAllowed(m[1]) {
			bad = append(bad, "url host "+m[1])
		}
	}
	for _, m := range scHexRe.FindAllString(text, -1) {
		sum := sha256.Sum256([]byte(strings.ToLower(m)))
		if deny[hex.EncodeToString(sum[:])] {
			bad = append(bad, "original identifier (hash match)")
		}
	}
	for _, m := range scGeoRe.FindAllString(text, -1) {
		var g struct {
			Geo map[string]any `json:"geo"`
		}
		if json.Unmarshal([]byte("{"+m+"}"), &g) != nil {
			bad = append(bad, "unparseable geo")
			continue
		}
		for k, v := range g.Geo {
			if s, _ := v.(string); s != "" && s != "Example City" && s != "Example Region" && s != "ZZ" {
				bad = append(bad, "user.geo."+k+"="+s)
			}
		}
	}
	return bad
}

func TestSentryFixturesAreAnonymized(t *testing.T) {
	deny := scDenyList(t)
	for name, text := range scFixtureTexts(t) {
		if bad := scAnonymizationViolations(text, deny); len(bad) > 0 {
			t.Errorf("%s is not anonymized: %v", name, bad)
		}
	}
	// The check must bite: each class of leak is caught on a synthetic file.
	sum := sha256.Sum256([]byte("deadbeefdeadbeef"))
	withOriginal := map[string]bool{hex.EncodeToString(sum[:]): true}
	for _, leak := range []string{
		`{"ip":"8.8.8.8"}`, `{"ip":"2606:4700::1111"}`, `{"e":"someone@gmail.com"}`,
		`{"u":"https://github.com/x/y"}`, `{"id":"deadbeefdeadbeef"}`, `{"user":{"geo":{"city":"Jakarta"}}}`,
	} {
		if len(scAnonymizationViolations(leak, withOriginal)) == 0 {
			t.Errorf("leak not caught: %s", leak)
		}
	}
	for _, fine := range []string{`{"r":"zasri.zasri@1.5.28+216"}`, `{"ip":"203.0.113.7"}`, `{"t":"2026-09-25T09:41:53.000Z"}`} {
		if bad := scAnonymizationViolations(fine, withOriginal); len(bad) > 0 {
			t.Errorf("false positive on %s: %v", fine, bad)
		}
	}
}
