package nats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// defaultMaxDeliver caps redelivery attempts when SubscriberConfig.MaxDeliver
// is not set. Before this existed, the consumer had no delivery cap at all
// (nats-init.sh creates it with `--defaults`, i.e. unlimited redeliveries),
// so a single permanently-unprocessable message would Nak forever and, being
// pulled back into every Fetch batch ahead of newer messages, starve all
// subsequent events (VERIFIED_STATE.md S13 / E2E_RECOVERY_PLAN.md P4-4:
// ~510 unstorable messages produced 5,874 "Processing event" log lines and
// no newly-published event was ever observed processed).
const defaultMaxDeliver = 7

type SubscriberConfig struct {
	URL         string
	Stream      string
	Subject     string
	Consumer    string
	BatchSize   int
	BatchWait   time.Duration
	NKeySeed    string
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string

	// MaxDeliver caps how many times JetStream will (re)deliver a message to
	// this consumer before it is considered exhausted. Defaults to
	// defaultMaxDeliver when <= 0. This is enforced two ways: it is written
	// into the JetStream consumer config (ensureConsumer), and it is also
	// checked explicitly against each message's delivery count in the
	// handler loop, so behavior does not depend on whichever process
	// happened to create the durable consumer first.
	MaxDeliver int

	// AckWait overrides the consumer's redelivery timeout. Zero uses the
	// server default (30s).
	AckWait time.Duration

	// DLQSubject is where exhausted/permanently-failed messages are
	// published before being terminated. Defaults to Subject + ".dlq" when
	// empty. If the DLQ publish fails the message is NOT terminated — it is
	// Nak'd and left in the source stream, because terminating an event that
	// was never captured anywhere is unrecoverable loss and JetStream is an
	// at-least-once transport. Redelivery still cannot loop forever: the
	// consumer's server-side MaxDeliver bounds it, after which the message is
	// parked in the stream, unacked and preserved. See DLQPublishFailures.
	DLQSubject string

	// DLQStream is the JetStream stream the DLQSubject is published on.
	// Defaults to Stream + "_DLQ" when empty. Created on demand (lazily, the
	// first time a message is actually dead-lettered) if it does not exist.
	DLQStream string
}

type Subscriber struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	cfg    SubscriberConfig
	errors chan error
	done   chan struct{}

	droppedErrors atomic.Uint64
	lastDropLog   atomic.Int64
	// dlqPublishFailures counts events that could not be captured in the DLQ and were therefore
	// left in the source stream instead of being terminated. Non-zero means events are parked and
	// unprocessed — surface it.
	dlqPublishFailures atomic.Uint64
}

// dropLogInterval bounds how often sendError may log a dropped error (S17).
const dropLogInterval = 10 * time.Second

// PermanentError marks a message-handler failure as non-retryable: the same
// bytes will fail identically on every redelivery (a malformed/invalid
// payload, a foreign-key or check-constraint violation, a "not found"
// lookup for a value that will never appear). The Subscriber dead-letters
// permanent errors immediately rather than spending the message's whole
// MaxDeliver budget on redeliveries that cannot succeed — see
// VERIFIED_STATE.md S13: "a permanent failure must not consume all its
// retries."
//
// Handlers must reserve this for content-caused failures. Infrastructure
// failures (DB connection refused, context deadline, network partition)
// must NOT be wrapped as permanent — those are exactly what redelivery (and
// the processor's degradation buffer) exists to recover from.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so the Subscriber treats it as non-retryable. Returns
// nil if err is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err (or anything it wraps) was produced by
// Permanent.
func IsPermanent(err error) bool {
	var permErr *PermanentError
	return errors.As(err, &permErr)
}

// Headers written onto every dead-lettered message. These are a wire contract between the Subscriber
// (which parks messages) and anything that later inspects, replays, or discards them — tools/dlq today.
// Changing a name here requires changing the reader in the same commit; there is no compiler between them
// (docs/memory/BUGS.md B5).
const (
	// DLQReasonHeader carries cause.Error() — human-readable, free text. Do NOT branch on its contents.
	DLQReasonHeader = "X-Sentinel-Dlq-Reason"
	// DLQAttemptsHeader carries how many deliveries were spent before parking.
	DLQAttemptsHeader = "X-Sentinel-Dlq-Attempts"
	// DLQSourceSubjectHeader carries the subject the message was originally published to.
	DLQSourceSubjectHeader = "X-Sentinel-Dlq-Source-Subject"
	// DLQClassHeader carries DLQClassPermanent or DLQClassTransient. This is the machine-readable field
	// to branch on.
	DLQClassHeader = "X-Sentinel-Dlq-Class"
)

// Values for DLQClassHeader.
const (
	// DLQClassPermanent means the same bytes will fail identically on every redelivery — a malformed
	// payload, a constraint violation, a lookup for something that will never exist. Replaying one is
	// guaranteed to re-fail and re-park it.
	DLQClassPermanent = "permanent"
	// DLQClassTransient means the failure was environmental (database down, deadline exceeded) and the
	// message is a genuine candidate for replay once the cause is resolved.
	DLQClassTransient = "transient"
	// DLQClassUnclassified is what a READER reports for a message carrying no DLQClassHeader, or one whose
	// value it does not recognize. It is never written by deadLetter — every message parked since the
	// header was introduced has a real class. It exists because messages parked BEFORE that change have no
	// class at all, and because "the header said something I don't understand" must be distinguishable
	// from "the failure was transient" rather than silently defaulting to the replayable case.
	//
	// This constant exists at all because two independent readers were written against this contract and
	// invented different words for the same state ("unknown" in the processor's /health, "unclassified" in
	// tools/dlq). That is B5 in miniature: a vocabulary with no single definition drifts immediately. One
	// name, defined here, used by both.
	DLQClassUnclassified = "unclassified"
)

// dlqClass reports the class to record for a failure that exhausted its retries or was marked permanent.
func dlqClass(cause error) string {
	if IsPermanent(cause) {
		return DLQClassPermanent
	}
	return DLQClassTransient
}

func NewSubscriber(ctx context.Context, cfg SubscriberConfig) (*Subscriber, error) {
	var opts []nats.Option

	if cfg.NKeySeed != "" {
		nkeyOpt, err := nats.NkeyOptionFromSeed(cfg.NKeySeed)
		if err != nil {
			return nil, fmt.Errorf("failed to create NKEY option: %w", err)
		}
		opts = append(opts, nkeyOpt)
	}

	if cfg.TLSCertFile != "" {
		tlsConfig, err := buildTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS config: %w", err)
		}
		opts = append(opts, nats.Secure(tlsConfig))
	}

	conn, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	return &Subscriber{
		conn:   conn,
		js:     js,
		cfg:    cfg,
		errors: make(chan error, 1),
		done:   make(chan struct{}),
	}, nil
}

func (s *Subscriber) Subscribe(ctx context.Context, handler func([]byte) error) error {
	// Deliberately not ctx: consumer provisioning is one-time setup, not
	// part of the per-message fetch loop that ctx governs below. Tying it to
	// ctx would make Subscribe fail whenever called with an
	// already-canceled or immediately-canceled context, which is valid
	// (Subscribe's contract is "start the background loop, which will
	// itself observe ctx.Done() and exit without processing anything").
	if err := s.ensureConsumer(context.Background()); err != nil {
		return fmt.Errorf("failed to create pull subscription: %w", err)
	}

	sub, err := s.js.PullSubscribe(s.cfg.Subject, s.cfg.Consumer, nats.BindStream(s.cfg.Stream))
	if err != nil {
		return fmt.Errorf("failed to create pull subscription: %w", err)
	}

	// Re-assert the consumer config once more shortly after startup. Some
	// deployments run a separate one-shot provisioner (scripts/nats-init.sh)
	// that unconditionally issues its own `nats consumer add ... --defaults`
	// (i.e. unlimited MaxDeliver) for this same durable name; depending on
	// container-orchestration ordering guarantees, that can race with — and
	// land after — the ensureConsumer call above, silently reverting
	// MaxDeliver/AckWait back to the provisioner's defaults. This second
	// pass wins that race in practice without requiring this process to
	// coordinate with, or depend on, whatever else provisions the stream.
	// It is a defense in depth: handleMessage's own per-message delivery
	// count check terminates exhausted messages regardless of what the
	// server-side consumer config says, so a lost race here degrades
	// nothing beyond this secondary safety net.
	go func() {
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			return
		case <-s.done:
			return
		}
		if err := s.ensureConsumer(context.Background()); err != nil {
			log.Printf("nats: post-startup consumer reconciliation failed for %s/%s: %v", s.cfg.Stream, s.cfg.Consumer, err)
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			default:
				// S17: Fetch used to be handed the caller's context directly. Callers that pass a
				// deadline-less context (auth/apikey.go passes context.Background()) made every Fetch
				// return "nats: context requires a deadline" INSTANTLY, and the loop spun with no
				// backoff — 55% CPU on a fully idle ingestor, flooding the log pipeline hard enough to
				// suppress the whole pod's output. Always give Fetch its own bounded deadline.
				wait := s.cfg.BatchWait
				if wait <= 0 {
					wait = 5 * time.Second
				}
				fetchCtx, cancelFetch := context.WithTimeout(ctx, wait)
				msgs, err := sub.Fetch(s.cfg.BatchSize, nats.Context(fetchCtx))
				cancelFetch()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					// An empty window is normal, not an error: nats.ErrTimeout comes from MaxWait,
					// DeadlineExceeded from the per-fetch context above.
					if err != nats.ErrTimeout && !errors.Is(err, context.DeadlineExceeded) {
						s.sendError(err)
					}
					continue
				}

				for _, msg := range msgs {
					s.handleMessage(ctx, msg, handler)
				}
			}
		}
	}()

	return nil
}

// handleMessage runs handler against a single delivered message and decides
// between Ack, Nak (retry), and dead-lettering based on the handler's error
// (and whether it is a PermanentError) and the message's delivery count.
func (s *Subscriber) handleMessage(ctx context.Context, msg *nats.Msg, handler func([]byte) error) {
	numDelivered := uint64(1)
	if meta, metaErr := msg.Metadata(); metaErr == nil && meta != nil {
		numDelivered = meta.NumDelivered
	}

	err := handler(msg.Data)
	if err == nil {
		msg.Ack()
		return
	}

	maxDeliver := uint64(s.maxDeliver())
	exhausted := maxDeliver > 0 && numDelivered >= maxDeliver
	if IsPermanent(err) || exhausted {
		s.deadLetter(ctx, msg, err, numDelivered)
		return
	}

	// NakWithDelay, not a bare Nak. A bare Nak redelivers immediately and the pull loop refetches
	// immediately, so the entire MaxDeliver budget burns in milliseconds — a Postgres restart or a
	// brief network blip would dead-letter the event long before the database came back, which
	// contradicts DECISIONS.md D1/D2 ("MUST be flushed to the database automatically once connection
	// is restored"). Backing off spreads the same 5 deliveries over ~2 minutes, so bounded retry can
	// actually outlast a transient fault. The anti-livelock guarantee (S13) is unaffected: it comes
	// from MaxDeliver plus PermanentError classification, not from retrying fast.
	msg.NakWithDelay(retryBackoff(numDelivered))
}

// retryBackoff returns the delay before the Nth redelivery.
//
// The budget is sized for INFRASTRUCTURE RECOVERY, not for poison messages. A content-caused failure
// never spends it — nats.Permanent dead-letters on the first delivery — so the only thing this window
// governs is "how long may a database or network outage last before we give up and park the event".
//
// It used to be 1+5+15+30 = ~51s, which CI proved is too tight: restarting a Postgres CONTAINER on a
// GitHub runner took longer than that, so every event was dead-lettered while the database was still
// coming back up. Local runs never caught it because the restart was faster than the budget. With the
// schedule below and MaxDeliver 7 the window is ~8.5 minutes, which covers a realistic restart,
// failover or brief partition. Widening it costs nothing for permanent failures and is the difference
// between "recovered automatically" and "an operator must run tools/dlq".
func retryBackoff(numDelivered uint64) time.Duration {
	schedule := []time.Duration{
		1 * time.Second,
		5 * time.Second,
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		300 * time.Second,
	}
	if numDelivered == 0 {
		return schedule[0]
	}
	if int(numDelivered) > len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[numDelivered-1]
}

// deadLetter publishes the raw message body (with failure metadata in headers) to the configured DLQ
// subject, logs loudly, and terminates the original message so JetStream stops redelivering it.
//
// Term() is called ONLY once the message is known to be captured somewhere durable. JetStream is an
// at-least-once transport, and Term is the application explicitly waiving that guarantee for one
// message — so waiving it before the event is safely stored is real, unrecoverable event loss.
// If the DLQ publish fails we Nak with a long delay instead: the message stays in the (file-backed)
// source stream, which is exactly where at-least-once wants it. That cannot livelock, because the
// consumer's server-side MaxDeliver stops redelivery on its own once the budget is spent — the
// message is then parked-but-preserved rather than dropped.
func (s *Subscriber) deadLetter(ctx context.Context, msg *nats.Msg, cause error, numDelivered uint64) {
	subject := s.dlqSubject()

	if subject == "" {
		log.Printf("nats: DEAD-LETTER (no DLQ subject configured): terminating unprocessable message on subject=%s after %d deliveries, cause=%v",
			s.cfg.Subject, numDelivered, cause)
	} else {
		if err := s.ensureDLQStream(ctx, subject); err != nil {
			log.Printf("nats: DEAD-LETTER: failed to ensure DLQ stream for subject=%s: %v", subject, err)
		}

		headers := nats.Header{}
		headers.Set(DLQReasonHeader, cause.Error())
		headers.Set(DLQAttemptsHeader, strconv.FormatUint(numDelivered, 10))
		headers.Set(DLQSourceSubjectHeader, s.cfg.Subject)
		// The CLASS is what makes a parked message safe to act on later, and it is recorded here because
		// here is the only place that knows it authoritatively. X-Sentinel-Dlq-Reason is free text
		// (cause.Error()), so anything downstream deciding "is this worth replaying?" from the reason
		// alone is string-matching an error message — which silently breaks the first time someone
		// rewords one.
		//
		// This matters concretely: a replay of a PERMANENT failure cannot succeed. It re-fails, re-parks,
		// and leaves the DLQ exactly as full as before, so a drainer that replays indiscriminately is
		// worse than one that does nothing. 6,148 messages accumulated that way before anyone noticed.
		headers.Set(DLQClassHeader, dlqClass(cause))
		dlqMsg := &nats.Msg{Subject: subject, Data: msg.Data, Header: headers}

		if _, err := s.js.PublishMsg(dlqMsg, nats.Context(ctx)); err != nil {
			log.Printf("nats: DEAD-LETTER PUBLISH FAILED: subject=%s dlq_subject=%s attempts=%d cause=%v publish_err=%v — NOT terminating; leaving the message in %s so it is not lost",
				s.cfg.Subject, subject, numDelivered, cause, err, s.cfg.Stream)
			s.dlqPublishFailures.Add(1)
			// Preserve the event rather than drop it. MaxDeliver bounds redelivery server-side.
			msg.NakWithDelay(retryBackoff(numDelivered))
			return
		}
		log.Printf("nats: DEAD-LETTERED message: subject=%s dlq_subject=%s attempts=%d cause=%v",
			s.cfg.Subject, subject, numDelivered, cause)
	}

	if err := msg.Term(); err != nil {
		log.Printf("nats: DEAD-LETTER: failed to terminate message on subject=%s: %v", s.cfg.Subject, err)
	}
}

// maxDeliver returns the configured redelivery cap, or defaultMaxDeliver
// when unset.
func (s *Subscriber) maxDeliver() int {
	if s.cfg.MaxDeliver > 0 {
		return s.cfg.MaxDeliver
	}
	return defaultMaxDeliver
}

func (s *Subscriber) dlqSubject() string {
	if s.cfg.DLQSubject != "" {
		return s.cfg.DLQSubject
	}
	if s.cfg.Subject == "" {
		return ""
	}
	return s.cfg.Subject + ".dlq"
}

func (s *Subscriber) dlqStreamName() string {
	if s.cfg.DLQStream != "" {
		return s.cfg.DLQStream
	}
	if s.cfg.Stream == "" {
		return ""
	}
	return s.cfg.Stream + "_DLQ"
}

// DLQStats summarizes the current state of the dead-letter queue: this call is what closes D10's "Known
// gap — this decision is not fully honored yet" item (a): "surface DLQ depth on /health or a metric."
type DLQStats struct {
	// Stream is the DLQ stream name this reports on. Empty if it cannot be derived (no
	// Stream/DLQStream configured on this Subscriber).
	Stream string
	// Depth is the number of messages currently held in the DLQ stream — i.e. events dead-lettered and
	// waiting for an operator to look at them (see tools/dlq). Zero if the stream does not exist yet,
	// which is the common, healthy case: ensureDLQStream only creates it lazily on the first
	// dead-letter (deadLetter).
	Depth uint64
	// PublishFailures mirrors DLQPublishFailures(): events that could NOT be captured in the DLQ and
	// were therefore left (Nak'd) in the SOURCE stream instead of being terminated — see deadLetter's
	// comment on why Term() is never called without a successful DLQ publish first. These events are
	// NOT reflected in Depth; they are not in the DLQ stream at all.
	PublishFailures uint64
}

// DLQStats reports dead-letter queue depth plus DLQPublishFailures in a single call suitable for a
// health/metrics endpoint. It is cheap to poll on every request: it reuses this Subscriber's existing
// JetStream connection (StreamInfo is one request/reply over the connection already open for message
// delivery) rather than opening a new connection per call. It does not create the DLQ stream as a side
// effect — if nothing has ever been dead-lettered, the stream may not exist yet, and that is reported as
// Depth 0, not as an error.
func (s *Subscriber) DLQStats(ctx context.Context) (DLQStats, error) {
	stream := s.dlqStreamName()
	stats := DLQStats{Stream: stream, PublishFailures: s.dlqPublishFailures.Load()}
	if stream == "" {
		return stats, nil
	}

	info, err := s.js.StreamInfo(stream, nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			return stats, nil
		}
		return stats, fmt.Errorf("failed to get DLQ stream info for %s: %w", stream, err)
	}
	stats.Depth = info.State.Msgs
	return stats, nil
}

// ensureDLQStream lazily creates the DLQ stream the first time a message is
// actually dead-lettered, so a deployment that never dead-letters anything
// never pays for it (and so existing tests/deployments whose NATS user
// lacks stream-admin permissions on unrelated streams are unaffected).
func (s *Subscriber) ensureDLQStream(ctx context.Context, subject string) error {
	stream := s.dlqStreamName()
	if stream == "" {
		return fmt.Errorf("cannot derive a DLQ stream name (no Stream/DLQStream configured)")
	}

	if _, err := s.js.StreamInfo(stream, nats.Context(ctx)); err == nil {
		return nil
	} else if !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}

	_, err := s.js.AddStream(&nats.StreamConfig{
		Name:     stream,
		Subjects: []string{subject},
		Storage:  nats.FileStorage,
	}, nats.Context(ctx))
	if err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		return err
	}
	return nil
}

// ensureConsumer makes the durable consumer's server-side config match this
// Subscriber's MaxDeliver/AckWait, whether that consumer was already
// created out-of-band (e.g. scripts/nats-init.sh, historically with
// unlimited redeliveries) or does not exist yet. Without this, MaxDeliver
// on SubscriberConfig would have no effect against a stack whose consumer
// was provisioned before this field existed.
func (s *Subscriber) ensureConsumer(ctx context.Context) error {
	cfg := &nats.ConsumerConfig{
		Durable:       s.cfg.Consumer,
		AckPolicy:     nats.AckExplicitPolicy,
		DeliverPolicy: nats.DeliverAllPolicy,
		MaxDeliver:    s.maxDeliver(),
	}
	if s.cfg.AckWait > 0 {
		cfg.AckWait = s.cfg.AckWait
	}

	_, err := s.js.ConsumerInfo(s.cfg.Stream, s.cfg.Consumer, nats.Context(ctx))
	if err != nil {
		if !errors.Is(err, nats.ErrConsumerNotFound) {
			return err
		}
		_, err = s.js.AddConsumer(s.cfg.Stream, cfg, nats.Context(ctx))
		if err == nil {
			return nil
		}
		if !errors.Is(err, nats.ErrConsumerNameAlreadyInUse) {
			return err
		}
		// Lost the create race to another provisioner (e.g.
		// scripts/nats-init.sh) between the ConsumerInfo check above and
		// this AddConsumer call. Fall through to UpdateConsumer instead of
		// silently accepting whatever config that other creator used.
	}

	_, err = s.js.UpdateConsumer(s.cfg.Stream, cfg, nats.Context(ctx))
	return err
}

func (s *Subscriber) Stop() {
	close(s.done)
}

func (s *Subscriber) Close() error {
	s.Stop()
	if s.conn != nil {
		s.conn.Close()
	}
	return nil
}

func (s *Subscriber) Errors() <-chan error {
	return s.errors
}

// DroppedErrors reports how many subscriber errors were discarded because
// nothing was reading Errors() (capacity 1) when they occurred. Errors()
// MUST be drained by every caller of Subscribe — the processor already
// does; historically the ingestor's api-key-invalidation subscriber did
// not, which blocked its goroutine permanently after the second error
// (VERIFIED_STATE.md S13). sendError below makes that failure mode
// non-fatal (a dropped error instead of a deadlocked goroutine), but a
// caller that never drains Errors() will still lose error visibility, so
// this counter exists for callers to surface via logs/metrics instead.
func (s *Subscriber) DroppedErrors() uint64 {
	return s.droppedErrors.Load()
}

// DLQPublishFailures returns the number of times an event could not be captured in the DLQ. Any
// non-zero value means events are sitting unprocessed in the source stream and need operator action.
func (s *Subscriber) DLQPublishFailures() uint64 {
	return s.dlqPublishFailures.Load()
}

// sendError delivers err on the (capacity-1) Errors() channel without
// blocking. A blocking send here previously wedged the subscriber's fetch
// goroutine permanently the moment a second error arrived before the first
// was drained (VERIFIED_STATE.md S13) — no caller could ever process
// another message again after that point, silently. Preferring to drop and
// count is strictly better than that: the fetch loop keeps running.
func (s *Subscriber) sendError(err error) {
	select {
	case s.errors <- err:
	default:
		dropped := s.droppedErrors.Add(1)
		// Rate-limit the drop log. Unconditional logging here turned a tight error loop into a log
		// flood that saturated journald and suppressed output for every container in the pod (S17) —
		// in a codebase whose whole failure history is "nobody could see it", losing logs is worse
		// than losing the individual message.
		if !s.shouldLogDrop() {
			return
		}
		log.Printf("nats: dropping subscriber error because Errors() is not being drained (capacity=1, dropped=%d so far): %v", dropped, err)
	}
}

// shouldLogDrop allows at most one drop log per dropLogInterval.
func (s *Subscriber) shouldLogDrop() bool {
	now := time.Now().UnixNano()
	last := s.lastDropLog.Load()
	if now-last < int64(dropLogInterval) {
		return false
	}
	return s.lastDropLog.CompareAndSwap(last, now)
}
