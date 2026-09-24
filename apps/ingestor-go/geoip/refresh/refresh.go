// Package refresh is the `ingestor geoip-refresh` subcommand (sentinel-event-enrichment spec §8.2): the
// ONLY code that contacts MaxMind. A twice-weekly CronJob runs it; it downloads GeoLite2-City only when
// MaxMind has a newer file than the one in object storage (the update check does not count against the
// free account's download limit), validates it, and uploads it through the S3 API for the pods'
// geoip.Loader to pick up.
//
// Nothing on the serving path imports this package: only the ingestor's main does, in the subcommand
// branch (tests/unit/ingestor_geoip_imports_test.go holds that). The licence key is never logged.
package refresh

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/maxmind/geoipupdate/v8/client"
	"github.com/minio/minio-go/v7"
	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/geoip"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

const (
	EnvAccountID  = "MAXMIND_ACCOUNT_ID"
	EnvLicenseKey = "MAXMIND_LICENSE_KEY"

	// EditionID is the one database this job keeps current.
	EditionID = "GeoLite2-City"

	// MetaMD5 is the user-metadata key the job stores MaxMind's MD5 under. minio-go returns user
	// metadata keyed by the canonical header name with X-Amz-Meta- trimmed ("Maxmind-Md5"), so it is
	// always read case-insensitively (spec §8.2 step 3).
	MetaMD5          = "maxmind-md5"
	MetaBuildEpoch   = "maxmind-build-epoch"
	MetaLastModified = "maxmind-last-modified"

	runTimeout = 10 * time.Minute
)

// Options are the test seams. Main uses the zero value with os.LookupEnv.
type Options struct {
	// Lookup reads the environment; nil means os.LookupEnv.
	Lookup func(string) (string, bool)
	// MaxMindEndpoint replaces https://updates.maxmind.com (client.WithEndpoint); "" keeps it.
	MaxMindEndpoint string
	// HTTPClient is used for MaxMind; nil means the client's default.
	HTTPClient *http.Client
	// S3Transport replaces the S3 client's transport (geoip.Config.Transport).
	S3Transport http.RoundTripper
}

// Main is the subcommand entry point: main() calls os.Exit(refresh.Main(ctx, logger)).
func Main(ctx context.Context, logger *slog.Logger) int {
	return Run(ctx, logger, Options{})
}

// Run performs one refresh and returns the process exit code: 0 when the stored database is current
// (up to date, or uploaded), 1 on any failure. Every failure leaves the stored object as it was.
func Run(ctx context.Context, logger *slog.Logger, opts Options) int {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	lookup := opts.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	fail := func(msg string, attrs ...any) int {
		logger.Error(msg, append(attrs, slog.String(obs.LogKeyEvent, "geoip.refresh_failed"))...)
		return 1
	}

	// 1. The MaxMind credentials (contract decision 6: with no key, the job fails, logs it, and geo
	//    stays empty). The key is never logged.
	accountRaw, _ := lookup(EnvAccountID)
	licenseKey, _ := lookup(EnvLicenseKey)
	accountRaw = strings.TrimSpace(accountRaw)
	if accountRaw == "" || licenseKey == "" {
		return fail("geoip-refresh: MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY not set; MaxMind not contacted")
	}
	accountID, err := strconv.Atoi(accountRaw)
	if err != nil || accountID <= 0 {
		return fail("geoip-refresh: MAXMIND_ACCOUNT_ID is not a positive integer; MaxMind not contacted")
	}

	// 2. The S3 client, from the same GEOIP_S3_* configuration as the pods.
	cfg := geoip.ConfigFromEnv(lookup, logger)
	cfg.Transport = opts.S3Transport
	if cfg.Bucket == "" {
		return fail("geoip-refresh: " + geoip.EnvS3Bucket + " not set; MaxMind not contacted")
	}
	s3, err := geoip.NewS3Client(cfg)
	if err != nil {
		return fail("geoip-refresh: invalid S3 configuration; MaxMind not contacted", slog.String("error", err.Error()))
	}
	key := cfg.ObjectKey()
	object := cfg.ObjectPath()

	// 3. The MD5 of the stored object, if any. Any error other than not-found stops BEFORE MaxMind is
	//    contacted, so a download that could not be stored is never spent.
	storedMD5 := ""
	info, err := s3.StatObject(ctx, cfg.Bucket, key, minio.StatObjectOptions{})
	switch {
	case err == nil:
		storedMD5 = userMetadata(info.UserMetadata, MetaMD5)
	case isNotFound(err):
	default:
		return fail("geoip-refresh: cannot stat the stored database; MaxMind not contacted",
			slog.String("object", object), slog.String("error", err.Error()))
	}

	// 4. One metadata check; a download only when MaxMind's MD5 differs.
	var copts []client.Option
	if opts.MaxMindEndpoint != "" {
		copts = append(copts, client.WithEndpoint(opts.MaxMindEndpoint))
	}
	if opts.HTTPClient != nil {
		copts = append(copts, client.WithHTTPClient(opts.HTTPClient))
	}
	mm, err := client.New(accountID, licenseKey, copts...)
	if err != nil {
		return fail("geoip-refresh: cannot build the MaxMind client")
	}
	resp, err := mm.Download(ctx, EditionID, storedMD5)
	if err != nil {
		return fail("geoip-refresh: MaxMind request failed", slog.String("error", err.Error()))
	}
	defer resp.Reader.Close()
	if !resp.UpdateAvailable {
		logger.Info("geoip-refresh: up to date", slog.String("md5", storedMD5), slog.String("object", object),
			slog.String(obs.LogKeyEvent, "geoip.refresh_up_to_date"))
		return 0
	}

	// 5. Read (capped) and validate before anything is uploaded.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(resp.Reader, geoip.MaxObjectBytes+1))
	if err != nil {
		return fail("geoip-refresh: reading the download failed; nothing uploaded", slog.String("error", err.Error()))
	}
	if n > geoip.MaxObjectBytes {
		return fail(fmt.Sprintf("geoip-refresh: the download is larger than %d bytes; nothing uploaded", geoip.MaxObjectBytes))
	}
	sum := md5.Sum(buf.Bytes())
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, resp.MD5) {
		return fail("geoip-refresh: MD5 mismatch; nothing uploaded", slog.String("want", resp.MD5), slog.String("got", got))
	}
	reader, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		return fail("geoip-refresh: the download is not a MaxMind database; nothing uploaded", slog.String("error", err.Error()))
	}
	if err := reader.Verify(); err != nil {
		return fail("geoip-refresh: the download failed verification; nothing uploaded", slog.String("error", err.Error()))
	}
	if dbType := reader.Metadata.DatabaseType; dbType != EditionID {
		return fail("geoip-refresh: unexpected database type; nothing uploaded", slog.String("database_type", dbType))
	}
	buildTime := time.Unix(int64(reader.Metadata.BuildEpoch), 0).UTC()

	// 6. One atomic object replacement: pods read the old file or the new one, never a mix.
	_, err = s3.PutObject(ctx, cfg.Bucket, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		UserMetadata: map[string]string{
			MetaMD5:          strings.ToLower(resp.MD5),
			MetaBuildEpoch:   strconv.FormatUint(uint64(reader.Metadata.BuildEpoch), 10),
			MetaLastModified: resp.LastModified.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return fail("geoip-refresh: upload failed", slog.String("object", object), slog.String("error", err.Error()))
	}
	logger.Info("geoip-refresh: uploaded",
		slog.String("md5", strings.ToLower(resp.MD5)),
		slog.String("build_time", buildTime.Format(time.RFC3339)),
		slog.Int("bytes", buf.Len()),
		slog.String("object", object),
		slog.String(obs.LogKeyEvent, "geoip.refresh_uploaded"))
	return 0
}

// userMetadata returns the value whose key equals name case-insensitively (spec §8.2 step 3).
func userMetadata(m map[string]string, name string) string {
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == http.StatusNotFound || resp.Code == minio.NoSuchKey
	}
	r := minio.ToErrorResponse(err)
	return r.StatusCode == http.StatusNotFound || r.Code == minio.NoSuchKey
}
