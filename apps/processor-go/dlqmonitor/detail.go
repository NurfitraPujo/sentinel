// Package dlqmonitor turns packages/shared-go/nats.DLQStats into an actionable severity signal and, on
// a state transition, an operational alert dispatched through apps/processor-go/alerts.
//
// Why this exists: the processor's /health endpoint already reports dlq_depth and flips status to
// "attention" on the first dead-lettered message, but nothing watched that endpoint and "non-zero" is
// too sensitive to page on — a single poison message is normal. The DLQ still reached 6,148 messages
// and exhausted JetStream storage before anyone noticed. This package is the fix: a depth+age-aware
// three-tier severity (Healthy/Attention/Critical) shared by /health and an in-process monitor that
// pages only on a Critical transition, both ways.
package dlqmonitor

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	sharedNats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

// Detail is a DLQ backlog snapshot: the depth/publish-failure counters packages/shared-go/nats.DLQStats
// already reports, plus the age and class of the single oldest parked message. The latter two are best
// effort — HasOldestAge/OldestClass are zero-valued when unavailable (e.g. the optional Detailer was
// never wired, or the underlying JetStream calls failed) rather than treated as an error, exactly like
// DLQStats itself already degrades gracefully when the DLQ stream does not exist yet.
type Detail struct {
	Stats sharedNats.DLQStats

	// HasOldestAge is true when OldestAge was determined from the DLQ stream's oldest message
	// timestamp. False (with OldestAge left zero) when depth is 0, no Detailer was configured, or the
	// StreamInfo call failed.
	HasOldestAge bool
	OldestAge    time.Duration

	// OldestClass is sharedNats.DLQClassPermanent, sharedNats.DLQClassTransient, sharedNats.DLQClassUnclassified (fetched
	// but unclassifiable/missing header), or "" (not fetched at all). A permanent oldest message will
	// never clear on its own; a transient one may once its cause resolves — see Classify.
	OldestClass string
}

// StatsSource is the subset of *packages/shared-go/nats.Subscriber this package depends on. Declaring
// it as an interface (rather than importing the concrete type everywhere) keeps Classify/Monitor unit
// testable without a live NATS connection.
type StatsSource interface {
	DLQStats(ctx context.Context) (sharedNats.DLQStats, error)
}

// OldestMessageSource enriches a DLQStats snapshot with the age and class of the oldest parked message.
// Implemented against a live JetStream connection by JetStreamDetailer (see detailer.go); fakeable in
// tests. A nil OldestMessageSource is valid and means "age/class enrichment unavailable" — GetDetail
// degrades to Stats-only in that case.
type OldestMessageSource interface {
	// OldestMessage reports whether an oldest message exists (hasAge), its age, and its
	// DLQClassHeader value ("permanent", "transient", or "unclassified" if the header is missing/unrecognized).
	// An error here is a fetch failure (e.g. NATS unreachable), not "no messages" — that case returns
	// hasAge=false, err=nil.
	OldestMessage(ctx context.Context, stream string) (hasAge bool, age time.Duration, class string, err error)
}

// GetDetail combines a StatsSource with an optional OldestMessageSource into a single Detail. oldest may
// be nil. The oldest-message lookup is skipped entirely when depth is 0 (nothing to look up) or the
// stream name is empty (DLQStats could not derive one) — both cheap, common, non-error cases.
func GetDetail(ctx context.Context, stats StatsSource, oldest OldestMessageSource) (Detail, error) {
	s, err := stats.DLQStats(ctx)
	if err != nil {
		return Detail{}, err
	}
	detail := Detail{Stats: s}

	if oldest == nil || s.Depth == 0 || s.Stream == "" {
		return detail, nil
	}

	hasAge, age, class, err := oldest.OldestMessage(ctx, s.Stream)
	if err != nil {
		slog.WarnContext(ctx, "dlqmonitor: oldest-message detail unavailable",
			slog.String("stream", s.Stream), slog.String("error", err.Error()))
		return detail, nil
	}
	detail.HasOldestAge = hasAge
	detail.OldestAge = age
	detail.OldestClass = class
	return detail, nil
}

// Severity is the three-tier classification /health and Monitor both use.
type Severity int

const (
	// Healthy: nothing dead-lettered, nothing stuck.
	Healthy Severity = iota
	// Attention: a backlog exists but is small and fresh — visible, not urgent. A single poison
	// message lands here, not Critical, which is the whole point (see package doc).
	Attention
	// Critical: the backlog is large, stale, or events could not even be captured in the DLQ
	// (PublishFailures). Someone must act.
	Critical
)

// Thresholds configures where Attention becomes Critical.
type Thresholds struct {
	// Depth: at or above this many dead-lettered messages, the backlog is Critical regardless of age.
	Depth uint64
	// CriticalAge: at or above this age for the OLDEST message, the backlog is Critical regardless of
	// depth — a stale backlog means nobody is triaging, which matters more than raw size (see package
	// doc and docs/plans/E2E_RECOVERY_PLAN.md).
	CriticalAge time.Duration
}

// DefaultDepthThreshold is the default value of Thresholds.Depth.
//
// Not a measured number — a defensible one. The incident this feature responds to reached 6,148
// messages before anyone noticed. A handful of poison messages (single digits) accumulating during
// normal operation is not itself a sign anything is broken; every payload format has some baseline rate
// of malformed/unprocessable events. 25 sits clearly above that noise floor while still catching a
// systemic failure at roughly 1/250th the size of the incident, i.e. long before JetStream storage is
// anywhere near threatened.
const DefaultDepthThreshold = 25

// DefaultCriticalAge is the default value of Thresholds.CriticalAge.
//
// A dead-lettered message is meant to be triaged via tools/dlq. An hour is long enough that a backlog
// actively being worked, or one whose transient cause is expected to clear on its own shortly, does not
// falsely page — but short enough to catch outright neglect well before it snowballs the way the
// 6,148-message incident did.
const DefaultCriticalAge = time.Hour

// ThresholdsFromEnv reads PROCESSOR_DLQ_DEPTH_THRESHOLD (a positive integer) and
// PROCESSOR_DLQ_CRITICAL_AGE (a Go duration string, e.g. "1h") from the environment, falling back to
// DefaultDepthThreshold/DefaultCriticalAge for anything unset or unparseable. Mirrors the
// getEnv/ALERT_CONFIG_REFRESH_INTERVAL pattern already used by alerts.refreshInterval.
func ThresholdsFromEnv() Thresholds {
	depth := uint64(DefaultDepthThreshold)
	if v := os.Getenv("PROCESSOR_DLQ_DEPTH_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			depth = uint64(n)
		} else {
			slog.Warn("dlqmonitor: ignoring invalid PROCESSOR_DLQ_DEPTH_THRESHOLD; using default",
				slog.String("value", v), slog.Int("default", DefaultDepthThreshold))
		}
	}

	age := DefaultCriticalAge
	if v := os.Getenv("PROCESSOR_DLQ_CRITICAL_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			age = d
		} else {
			slog.Warn("dlqmonitor: ignoring invalid PROCESSOR_DLQ_CRITICAL_AGE; using default",
				slog.String("value", v), slog.Duration("default", DefaultCriticalAge))
		}
	}

	return Thresholds{Depth: depth, CriticalAge: age}
}

// StatusHealthy, StatusAttention, and StatusCritical are the exact strings Classify returns for each
// Severity — shared verbatim between /health's "status" field and Monitor's log/alert text so the two
// surfaces never describe the same condition differently. StatusAttention keeps the wording the
// endpoint originally shipped with (tests/e2e only asserts non-empty, not exact content, but there is no
// reason to change text nothing requires changing).
const (
	StatusHealthy   = "healthy"
	StatusAttention = "attention: dead-lettered events awaiting replay (see tools/dlq)"
	StatusCritical  = "critical: dead-lettered events require triage (see tools/dlq)"
)

// Classify maps a Detail against Thresholds to a Severity and its /health status string.
//
// Critical fires on any of three independent conditions: depth at/above the threshold, the oldest
// message at/above the critical age, or a nonzero PublishFailures. PublishFailures is always Critical
// regardless of thresholds — those events could not even be captured in the DLQ and are sitting Nak'd
// in the source stream instead, which is worse than an ordinary backlog (see
// packages/shared-go/nats.DLQStats's doc comment).
func Classify(d Detail, th Thresholds) (Severity, string) {
	if d.Stats.Depth == 0 && d.Stats.PublishFailures == 0 {
		return Healthy, StatusHealthy
	}

	critical := d.Stats.PublishFailures > 0 ||
		d.Stats.Depth >= th.Depth ||
		(d.HasOldestAge && d.OldestAge >= th.CriticalAge)

	if critical {
		return Critical, StatusCritical
	}
	return Attention, StatusAttention
}
