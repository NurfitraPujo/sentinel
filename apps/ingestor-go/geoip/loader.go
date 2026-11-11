package geoip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// Loader states and unavailable reasons (spec §5.5).
const (
	StateDisabled    = "disabled"
	StateLoading     = "loading"
	StateReady       = "ready"
	StateUnavailable = "unavailable"
	StateStale       = "stale"

	ReasonConfig     = "config"
	ReasonNotFound   = "not_found"
	ReasonFetchError = "fetch_error"
	ReasonCorrupt    = "corrupt"
	ReasonWrongType  = "wrong_type"
)

// Log event keys (obs.LogKeyEvent values).
const (
	EventState    = "geoip.state"
	EventReloaded = "geoip.reloaded"
	// EventFetchFailed: a refresh failed while a verified database kept serving (state stays ready).
	EventFetchFailed = "geoip.fetch_failed"
)

// db is one opened database. It is never Closed: it was opened from bytes, so there is no mapping to
// release, and Close could race an in-flight lookup (spec §5.1, §5.4). The GC frees a dropped one.
type db struct {
	reader       *maxminddb.Reader
	etag         string
	buildTime    time.Time
	databaseType string
}

// Loader holds the current database behind an atomic pointer, so Lookup never blocks, and polls object
// storage in one background goroutine. The zero value is not usable; build one with NewLoader.
type Loader struct {
	cfg    Config
	src    ObjectSource
	cfgErr error // non-nil: configuration incomplete or invalid (unavailable/config)
	logger *slog.Logger

	serving atomic.Pointer[db] // what Lookup answers from; nil unless state is ready

	mu         sync.Mutex // serialises polls (Start's goroutine, or a test calling Poll)
	loaded     *db        // last database that opened and verified, whatever its age
	failedETag string     // an object whose CONTENT failed (corrupt, wrong type); not re-fetched until its ETag changes
	failedWhy  string     // failedETag's reason, reported again on every poll that skips it
	problem    string     // the reason of the current failure streak while a database keeps serving ("" = none)
	state      string
	reason     string
}

// fetchAttempts bounds one poll's tries after a TRANSPORT failure (a Stat, Get or read error): the first try plus two retries, backed off by Config.RetryBackoff, then 2x that. Well inside
// the 1m minimum poll interval. A transport failure is never a verdict on the object, so the next poll
// tries again whatever happened here.
const fetchAttempts = 3

// NewLoader builds a Loader over S3 from cfg. An empty bucket is "disabled"; an incomplete or invalid
// configuration is "unavailable/config". Neither is an error: geo is a feature, not a dependency.
func NewLoader(cfg Config, logger *slog.Logger) *Loader {
	l := &Loader{cfg: cfg, logger: logger}
	if cfg.Bucket == "" {
		return l
	}
	src, err := NewS3Source(cfg)
	if err != nil {
		l.cfgErr = err
		return l
	}
	l.src = src
	return l
}

// NewLoaderWithSource builds a Loader over any ObjectSource (tests use a fake).
func NewLoaderWithSource(cfg Config, src ObjectSource, logger *slog.Logger) *Loader {
	return &Loader{cfg: cfg, src: src, logger: logger}
}

// Start returns immediately. One goroutine does the first fetch and then polls every PollInterval
// until ctx ends. Readiness never waits for it; until the first successful load, Lookup answers nothing.
func (l *Loader) Start(ctx context.Context) {
	l.mu.Lock()
	if l.src != nil {
		l.transition(StateLoading, "", nil, nil)
	}
	l.mu.Unlock()
	interval := l.cfg.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	go func() {
		l.Poll(ctx)
		if l.src == nil {
			return // disabled or misconfigured: nothing will ever change without a restart
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.Poll(ctx)
			}
		}
	}()
}

// Poll runs one poll tick (spec §5.4): stat the object, fetch and swap only when its ETag changed, and
// re-evaluate staleness every time. Exported so tests drive ticks deterministically.
func (l *Loader) Poll(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch {
	case l.cfg.Bucket == "":
		l.fail(StateDisabled, "", nil)
		return
	case l.cfgErr != nil:
		l.fail(StateUnavailable, ReasonConfig, l.cfgErr)
		return
	case l.src == nil:
		l.fail(StateUnavailable, ReasonConfig, errors.New("no object source"))
		return
	}

	key := l.cfg.ObjectKey()
	// Each pass is one Stat and, when the ETag changed, one Get pinned to that ETag. A Get that finds a
	// different object (the refresh job uploaded between the two) starts a new pass, bounded by
	// fetchAttempts; it is a transport-class failure, never a verdict on either object.
	for pass := 1; ; pass++ {
		info, err := l.statWithRetry(ctx, key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				l.fail(StateUnavailable, ReasonNotFound, nil)
			} else {
				// A transport failure: a verified database keeps serving (degrade).
				l.degrade(ReasonFetchError, err)
			}
			return
		}

		switch {
		case l.loaded != nil && info.ETag == l.loaded.etag:
			// Unchanged object: only the staleness check runs.
		case info.ETag != "" && info.ETag == l.failedETag:
			// The same broken object as last time: fail the same way, silently, without re-downloading.
			l.degrade(l.failedWhy, nil)
			return
		default:
			next, reason, err := l.fetchWithRetry(ctx, key, info.ETag)
			if errors.Is(err, ErrETagMismatch) && pass < fetchAttempts {
				continue
			}
			if err != nil {
				if reason == ReasonCorrupt || reason == ReasonWrongType {
					// Only a verdict on the bytes blacklists the ETag. A transport failure (fetch_error,
					// including an ETag mismatch) or an object that vanished (not_found) is fetched again
					// on the next poll.
					l.failedETag, l.failedWhy = info.ETag, reason
				}
				if reason == ReasonNotFound {
					l.fail(StateUnavailable, reason, err)
					return
				}
				l.degrade(reason, err)
				return
			}
			l.failedETag, l.failedWhy, l.problem = "", "", ""
			prev := l.loaded
			l.loaded = next
			if prev != nil && l.state == StateReady && !l.isStale(next) {
				// A newer object replaced a serving one: the state does not change, so say so explicitly
				// (spec §5.5); P4 reads the newest build_time from this line.
				l.serving.Store(next)
				l.logger.Info("geoip database reloaded",
					slog.String(obs.LogKeyEvent, EventReloaded),
					slog.String("build_time", next.buildTime.UTC().Format(time.RFC3339)),
					slog.String("database_type", next.databaseType),
					slog.String("etag", next.etag),
					slog.String("object", l.cfg.ObjectPath()))
				return
			}
		}
		break
	}

	l.problem = ""
	if l.isStale(l.loaded) {
		l.serving.Store(nil)
		l.transition(StateStale, "", l.loaded, nil)
		return
	}
	l.serving.Store(l.loaded)
	l.transition(StateReady, "", l.loaded, nil)
}

// statWithRetry is Stat, tried again after a transport failure up to fetchAttempts times with backoff.
// ErrNotFound, or ctx ending, returns at once.
func (l *Loader) statWithRetry(ctx context.Context, key string) (ObjectInfo, error) {
	backoff := l.retryBackoff()
	for attempt := 1; ; attempt++ {
		info, err := l.src.Stat(ctx, key)
		if err == nil || errors.Is(err, ErrNotFound) || attempt == fetchAttempts {
			return info, err
		}
		if !sleepCtx(ctx, backoff) {
			return info, err
		}
		backoff *= 2
	}
}

// fetchWithRetry is fetch, tried again after a transport failure (reason fetch_error) up to
// fetchAttempts times with backoff. A content failure, not_found, an ETag mismatch (retrying the same
// ETag cannot succeed; Poll re-stats instead), or ctx ending returns at once.
func (l *Loader) fetchWithRetry(ctx context.Context, key, etag string) (*db, string, error) {
	backoff := l.retryBackoff()
	for attempt := 1; ; attempt++ {
		next, reason, err := l.fetch(ctx, key, etag)
		if err == nil || reason != ReasonFetchError || errors.Is(err, ErrETagMismatch) || attempt == fetchAttempts {
			return next, reason, err
		}
		if !sleepCtx(ctx, backoff) {
			return nil, reason, err
		}
		backoff *= 2
	}
}

func (l *Loader) retryBackoff() time.Duration {
	if l.cfg.RetryBackoff > 0 {
		return l.cfg.RetryBackoff
	}
	return DefaultRetryBackoff
}

// sleepCtx waits d, or returns false as soon as ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// degrade handles a failed poll. A database that is serving and not stale keeps answering: a failed
// refresh is never a reason to drop verified data. The state stays ready and one Warn line
// (geoip.fetch_failed) is written per failure streak, not per poll; the next successful poll ends the
// streak. With nothing serving, the poll fails as (unavailable, reason), logged once per change.
// Staleness is still re-evaluated here, so a database that ages out while storage is unreachable stops
// answering on time.
func (l *Loader) degrade(reason string, err error) {
	if d := l.serving.Load(); d != nil {
		if l.isStale(d) {
			l.serving.Store(nil)
			l.transition(StateStale, "", d, nil)
			return
		}
		if reason != l.problem {
			attrs := []any{
				slog.String(obs.LogKeyEvent, EventFetchFailed),
				slog.String("reason", reason),
				slog.String("serving_etag", d.etag),
				slog.String("object", l.cfg.ObjectPath()),
			}
			if err != nil {
				attrs = append(attrs, slog.String("error", err.Error()))
			}
			l.logger.Warn(fmt.Sprintf("geoip refresh failed (reason=%s); still serving the loaded database", reason), attrs...)
		}
		l.problem = reason
		return
	}
	l.fail(StateUnavailable, reason, err)
}

// fetch reads the object at etag (at most MaxObjectBytes), opens and verifies the database, and checks
// its type. A read that finds a different object fails with ErrETagMismatch (reason fetch_error).
func (l *Loader) fetch(ctx context.Context, key, etag string) (*db, string, error) {
	rc, err := l.src.Get(ctx, key, etag)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ReasonNotFound, nil
		}
		return nil, ReasonFetchError, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(rc, MaxObjectBytes+1))
	if err != nil {
		return nil, ReasonFetchError, err
	}
	if n > MaxObjectBytes {
		return nil, ReasonCorrupt, fmt.Errorf("object is larger than %d bytes", MaxObjectBytes)
	}
	reader, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		return nil, ReasonCorrupt, err
	}
	if err := reader.Verify(); err != nil {
		return nil, ReasonCorrupt, err
	}
	dbType := reader.Metadata.DatabaseType
	if !strings.HasSuffix(dbType, "-City") {
		return nil, ReasonWrongType, fmt.Errorf("database type %q is not a -City database", dbType)
	}
	return &db{
		reader:       reader,
		etag:         etag,
		buildTime:    time.Unix(int64(reader.Metadata.BuildEpoch), 0).UTC(),
		databaseType: dbType,
	}, "", nil
}

func (l *Loader) isStale(d *db) bool {
	maxAge := l.cfg.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	return l.cfg.now().Sub(d.buildTime) > maxAge
}

// fail stops answering and records the (state, reason), logging only on a change.
func (l *Loader) fail(state, reason string, err error) {
	l.serving.Store(nil)
	l.transition(state, reason, nil, err)
}

// transition writes one line only when (state, reason) changes (spec §5.5). Info for disabled, loading
// and ready; Warn for unavailable and stale. build_time is attached for ready and stale.
func (l *Loader) transition(state, reason string, d *db, err error) {
	if state == l.state && reason == l.reason {
		return
	}
	from := l.state
	l.state, l.reason = state, reason

	attrs := []any{
		slog.String(obs.LogKeyEvent, EventState),
		slog.String("from", from),
		slog.String("to", state),
		slog.String("reason", reason),
		slog.String("object", l.cfg.ObjectPath()),
	}
	if d != nil && (state == StateReady || state == StateStale) {
		attrs = append(attrs,
			slog.String("build_time", d.buildTime.UTC().Format(time.RFC3339)),
			slog.String("database_type", d.databaseType),
			slog.String("etag", d.etag))
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	msg := fmt.Sprintf("geoip.state from=%s to=%s reason=%s", from, state, reason)
	if state == StateUnavailable || state == StateStale {
		l.logger.Warn(msg, attrs...)
	} else {
		l.logger.Info(msg, attrs...)
	}
}

// State reports the loader's current (state, reason).
func (l *Loader) State() (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state, l.reason
}

// DatabaseType is the serving database's Metadata.DatabaseType, or "" when nothing is served.
func (l *Loader) DatabaseType() string {
	if d := l.serving.Load(); d != nil {
		return d.databaseType
	}
	return ""
}

// cityRecord decodes only the four fields enrichment uses (spec §5.1).
type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	Subdivisions []struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
}

// Lookup answers from the serving database, never blocking and never touching the network. It maps a
// GeoLite2-City record the way Sentry's Relay does (spec §5.2): region is the COUNTRY name. A miss, a
// decode error, no serving database, or a record with all four fields empty is (zero, false).
func (l *Loader) Lookup(a netip.Addr) (validation.GeoInfo, bool) {
	d := l.serving.Load()
	if d == nil {
		return validation.GeoInfo{}, false
	}
	res := d.reader.Lookup(a.Unmap().WithZone(""))
	if !res.Found() {
		return validation.GeoInfo{}, false
	}
	var rec cityRecord
	if err := res.Decode(&rec); err != nil {
		return validation.GeoInfo{}, false
	}
	g := validation.GeoInfo{
		CountryCode: rec.Country.ISOCode,
		City:        rec.City.Names["en"],
		Region:      rec.Country.Names["en"],
	}
	if len(rec.Subdivisions) > 0 {
		g.Subdivision = rec.Subdivisions[0].Names["en"]
	}
	if g == (validation.GeoInfo{}) {
		return validation.GeoInfo{}, false
	}
	return g, true
}
