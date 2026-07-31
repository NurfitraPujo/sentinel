// Command dlq is the replay tool D10 calls out as missing: "Nothing consumes the DLQ. There is no
// drain, no replay, no alerting, and no dashboard surface, so a dead-lettered event is invisible to the
// product." (docs/memory/DECISIONS.md D10). It connects to NATS, reads messages parked in a DLQ stream
// (packages/shared-go/nats.Subscriber.deadLetter published them there — see that file for the delivery
// contract this tool completes but does not renegotiate), prints the X-Sentinel-Dlq-* headers that
// explain why each one failed, and — only when explicitly told to — re-publishes it onto its original
// subject so the processor picks it up again.
//
// Dry-run is the default. Nothing is published, and nothing is deleted, unless the corresponding flag
// says so explicitly:
//
//	go run ./tools/dlq                                # list what's in the DLQ, do nothing else
//	go run ./tools/dlq -limit=10                       # look at only the first 10
//	go run ./tools/dlq -reason-contains="constraint"   # narrow to a known-fixed bug class
//	go run ./tools/dlq -class=transient                # narrow to environmental failures only
//	go run ./tools/dlq -execute -limit=10              # actually replay (up to) 10 of them
//	go run ./tools/dlq -execute -delete -limit=10      # replay AND remove from the DLQ on success
//	go run ./tools/dlq -class=permanent -purge -execute  # discard messages that can never succeed
//	go run ./tools/dlq -drain -execute -limit=50       # unattended-safe: transient only, capped replays
//
// -delete is intentionally separate from -execute: most dead letters in this repo are schema/constraint
// bugs (VERIFIED_STATE.md), so replay-after-fix is the expected use case, and a replay that fails again
// for the same reason should not have destroyed the evidence. Nothing is ever deleted from the DLQ
// unless the operator passes -delete explicitly, and even then only for messages that were just
// successfully re-published. (-drain is the one exception: see its flag description.)
//
// # Class awareness
//
// packages/shared-go/nats.Subscriber stamps every dead-lettered message with an X-Sentinel-Dlq-Class
// header: "permanent" (the same bytes will fail identically every time — a malformed payload, a
// constraint violation, a lookup for something that will never exist) or "transient" (an environmental
// failure — DB down, deadline exceeded — and a genuine replay candidate). This tool imports the
// nats.DLQClass* constants rather than hardcoding the strings, and never infers class by pattern-matching
// X-Sentinel-Dlq-Reason: that header is free text (cause.Error()) and matching against it breaks the
// first time someone rewords an error (docs/memory/BUGS.md B5).
//
// Messages parked before the class header existed have no X-Sentinel-Dlq-Class at all. This tool treats
// that as its own third state, "unclassified" — not as an alias for either real class — and refuses to
// replay them by default (see -allow-unclassified-replay). An unclassified message's failure mode is
// unknown; replaying it blind is exactly the "replay indiscriminately" mistake that parked 6,148 messages
// permanently in the first place. Inspecting and purging unclassified messages is unaffected: an operator
// who has looked at one and decided it is dead can say so explicitly with -class=unclassified -purge.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	sentinelnats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

// classUnclassified is this tool's own sentinel value for "no X-Sentinel-Dlq-Class header present". It
// is deliberately not one of the wire values in sentinelnats — a message with no header is a different
// case from either real class, not a default alias for one of them.
// Defined by the contract in packages/shared-go/nats, not re-spelled here: two readers of this
// header already invented different words for this state once.
const classUnclassified = sentinelnats.DLQClassUnclassified

// replayHeaderKeys are the ONLY headers a replay carries forward from the parked DLQ message onto the
// republished one. They are the W3C distributed-trace propagation keys the OTel NATS carrier reads/writes
// (see packages/shared-go/obs/carrier.go's NATSHeaderCarrier) — traceparent identifies the trace, tracestate
// carries vendor-specific state, baggage carries arbitrary key/value context. Keeping these alive across a
// replay is the whole point of this fix: "this event failed N times and got parked" is the single most
// valuable trace in this system, and a replay that starts a disconnected root span severs it right where it
// matters most.
//
// This tool cannot import packages/shared-go/obs to reuse its constants (obs has no exported names for
// these — they are OTel's own wire vocabulary, always spelled this way by otel.GetTextMapPropagator(), see
// carrier.go's doc comment) and, independently, this package intentionally has no dependency on obs at all,
// so the three keys are inlined here as the literal, stable W3C header names.
var replayHeaderKeys = []string{"traceparent", "tracestate", "baggage"}

// replayHeaders builds the header set for a message being republished onto its original subject.
//
// The trace context survives (see replayHeaderKeys above): severing it here would just move today's bug
// one hop further down the pipe instead of fixing it — packages/shared-go/nats/subscriber.go's deadLetter
// now preserves it going INTO the DLQ, and dropping it here on the way back OUT would still leave the
// replayed event starting a brand-new, disconnected trace.
//
// The X-Sentinel-Dlq-* bookkeeping headers (reason/attempts/source-subject/class) do NOT ride back onto
// the source subject — this is a deliberate choice, not an oversight: those headers describe what
// deadLetter observed about the LAST, failed attempt (why IT was parked), not anything true about this new
// attempt. Carrying them forward would let a stale X-Sentinel-Dlq-Class (or reason, or attempt count) sit
// on a message flowing through the primary subject, where nothing expects DLQ metadata to be present at
// all — and if this replay fails and gets dead-lettered again, deadLetter's own headers.Set calls
// overwrite them anyway, so keeping them here buys nothing except a message that looks, to any casual
// inspector on the primary subject, like it is still sitting in the DLQ.
func replayHeaders(h nats.Header) nats.Header {
	out := nats.Header{}
	for _, key := range replayHeaderKeys {
		if v := headerValue(h, key); v != "" {
			out.Set(key, v)
		}
	}
	return out
}

func main() {
	var (
		natsURL        = flag.String("nats-url", getEnv("NATS_URL", "nats://localhost:4222"), "NATS server URL")
		nkeySeed       = flag.String("nkey-seed", os.Getenv("NATS_NKEY_SEED"), "optional NKey seed for authentication")
		dlqStream      = flag.String("stream", getEnv("DLQ_STREAM", "ERROR_EVENTS_DLQ"), "DLQ stream to read from")
		limit          = flag.Int("limit", 10, "maximum number of DLQ messages to inspect/replay/purge in this run (0 = no limit, use with care). This is also the per-run cap for -drain.")
		startSeq       = flag.Uint64("start-seq", 0, "skip stream sequences below this one (for paging through a DLQ larger than -limit)")
		reasonContains = flag.String("reason-contains", "", "only operate on messages whose X-Sentinel-Dlq-Reason contains this substring (case-insensitive) — the normal way to target a specific, now-fixed bug class")
		class          = flag.String("class", "", `only operate on messages whose X-Sentinel-Dlq-Class is this value: "transient", "permanent", or "unclassified" (no class header — parked before class tagging existed). Empty (default) matches all three. Combines with -reason-contains (AND). Applies to inspect, replay, and -purge.`)
		execute        = flag.Bool("execute", false, "actually re-publish matching messages onto their original subject (or, with -purge, actually discard them). Without this flag the tool only lists what it WOULD do.")
		deleteOnReplay = flag.Bool("delete", false, "after a message is successfully re-published, delete it from the DLQ stream. Has no effect without -execute. Off by default: a replay that fails again should not have destroyed the evidence. (-drain always deletes on successful replay regardless of this flag — see its description.)")
		timeout        = flag.Duration("timeout", 30*time.Second, "overall timeout for the run")
		previewBytes   = flag.Int("preview-bytes", 200, "how many bytes of each message body to print for context (0 to disable)")
		purge          = flag.Bool("purge", false, "DISCARD matching messages instead of replaying them. Requires -execute. Use when the parked messages can never succeed — e.g. events whose project no longer exists — because replaying those just re-fails and re-parks them. Combine with -reason-contains and/or -class to narrow which ones. Cannot be combined with -drain.")

		drain                   = flag.Bool("drain", false, "unattended-safe mode for scheduled runs: forces -class=transient (drain never replays or purges class=permanent, and never touches unclassified messages — it only reports them), forces -delete on every successful replay so the DLQ actually shrinks, and enforces -max-replays via -state-file so a message that keeps re-parking after replay is not retried forever. Still requires -execute to do anything; without it, -drain only previews what it would do. Cannot be combined with -purge.")
		maxReplays              = flag.Int("max-replays", 3, "in -drain mode, the maximum number of times a distinct message (tracked by content hash in -state-file) may be replayed across runs before drain stops touching it and just reports it. 0 disables the cap — NOT recommended for unattended scheduling, since a message stuck failing after replay would then be retried every single run forever.")
		stateFile               = flag.String("state-file", getEnv("DLQ_STATE_FILE", "dlq-drain-state.json"), "in -drain mode, path to the JSON file tracking how many times each message has been replayed across runs (see -max-replays). Created on first use. Ignored outside -drain.")
		allowUnclassifiedReplay = flag.Bool("allow-unclassified-replay", false, "permit -execute to replay (not purge) a message with no X-Sentinel-Dlq-Class header. Off by default: such a message was parked before class tagging existed, so whether it is safe to replay is unknown — pass this only after inspecting the message yourself and deciding it is safe.")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, config{
		natsURL:                 *natsURL,
		nkeySeed:                *nkeySeed,
		dlqStream:               *dlqStream,
		limit:                   *limit,
		startSeq:                *startSeq,
		reasonContains:          *reasonContains,
		class:                   *class,
		execute:                 *execute,
		deleteOnReplay:          *deleteOnReplay,
		previewBytes:            *previewBytes,
		purge:                   *purge,
		drain:                   *drain,
		maxReplays:              *maxReplays,
		stateFile:               *stateFile,
		allowUnclassifiedReplay: *allowUnclassifiedReplay,
	}); err != nil {
		log.Fatalf("dlq: %v", err)
	}
}

type config struct {
	purge          bool
	natsURL        string
	nkeySeed       string
	dlqStream      string
	limit          int
	startSeq       uint64
	reasonContains string
	class          string
	execute        bool
	deleteOnReplay bool
	previewBytes   int

	drain                   bool
	maxReplays              int
	stateFile               string
	allowUnclassifiedReplay bool
}

type result struct {
	scanned             int
	matched             int
	replayed            int
	deleted             int
	skippedNoSubj       int
	skippedUnclassified int
	skippedMaxReplays   int
	errored             int
	seenPermanent       int
	seenTransient       int
	seenUnclassified    int
}

// validClasses are the values -class accepts, beyond "" (no filter).
var validClasses = map[string]bool{
	sentinelnats.DLQClassPermanent: true,
	sentinelnats.DLQClassTransient: true,
	classUnclassified:              true,
}

func run(ctx context.Context, cfg config) error {
	if cfg.class != "" && !validClasses[cfg.class] {
		return fmt.Errorf("-class=%q is not valid: must be %q, %q, %q, or empty (no filter)",
			cfg.class, sentinelnats.DLQClassTransient, sentinelnats.DLQClassPermanent, classUnclassified)
	}

	if cfg.drain {
		if cfg.purge {
			return fmt.Errorf("-drain cannot be combined with -purge: drain replays transient messages, it never discards; run -class=%s -purge separately for that", sentinelnats.DLQClassPermanent)
		}
		if cfg.class != "" && cfg.class != sentinelnats.DLQClassTransient {
			return fmt.Errorf("-drain always targets class=%s; got -class=%q — drop -class or set it to %q", sentinelnats.DLQClassTransient, cfg.class, sentinelnats.DLQClassTransient)
		}
		cfg.class = sentinelnats.DLQClassTransient
		cfg.deleteOnReplay = true // drain must remove what it successfully replays, or the DLQ never shrinks
		if cfg.maxReplays <= 0 {
			fmt.Printf("WARNING: -max-replays=%d disables the re-park cap for this drain run — a message that keeps failing after replay will be retried again on every future run, forever. Not recommended for unattended scheduling.\n", cfg.maxReplays)
		}
	}

	var opts []nats.Option
	if cfg.nkeySeed != "" {
		nkeyOpt, err := nats.NkeyOptionFromSeed(cfg.nkeySeed)
		if err != nil {
			return fmt.Errorf("failed to create NKey option: %w", err)
		}
		opts = append(opts, nkeyOpt)
	}

	conn, err := nats.Connect(cfg.natsURL, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS at %s: %w", cfg.natsURL, err)
	}
	defer conn.Close()

	js, err := conn.JetStream()
	if err != nil {
		return fmt.Errorf("failed to get JetStream context: %w", err)
	}

	info, err := js.StreamInfo(cfg.dlqStream, nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			fmt.Printf("DLQ stream %q does not exist yet — nothing has ever been dead-lettered. Nothing to do.\n", cfg.dlqStream)
			return nil
		}
		return fmt.Errorf("failed to get stream info for %s: %w", cfg.dlqStream, err)
	}

	if info.State.Msgs == 0 {
		fmt.Printf("DLQ stream %q is empty (0 messages). Nothing to do.\n", cfg.dlqStream)
		return nil
	}

	if cfg.purge && !cfg.execute {
		fmt.Printf("DLQ stream %q: %d message(s) parked, sequence range [%d, %d].\n",
			cfg.dlqStream, info.State.Msgs, info.State.FirstSeq, info.State.LastSeq)
		fmt.Println("Mode: DRY RUN (-purge given without -execute). Nothing was discarded.")
		if cfg.reasonContains != "" || cfg.class != "" {
			fmt.Printf("Would discard only messages matching reason-contains=%q class=%q.\n", cfg.reasonContains, cfg.class)
		} else {
			fmt.Printf("Would discard ALL %d message(s). Add -reason-contains and/or -class to narrow this.\n", info.State.Msgs)
		}
		fmt.Println("Re-run with -execute to proceed. This is not reversible.")
		return nil
	}
	if cfg.purge {
		return purgeStream(ctx, js, cfg, info)
	}

	var state *replayState
	if cfg.drain {
		state, err = loadState(cfg.stateFile)
		if err != nil {
			return fmt.Errorf("loading drain state file %s: %w", cfg.stateFile, err)
		}
	}
	stateDirty := false

	mode := "DRY RUN (pass -execute to actually replay)"
	if cfg.execute {
		mode = "EXECUTE (will re-publish matching messages)"
		if cfg.deleteOnReplay {
			mode += " + DELETE on success"
		}
	}
	if cfg.drain {
		mode = "DRAIN, " + mode
	}
	fmt.Printf("DLQ stream %q: %d message(s) currently parked, sequence range [%d, %d]. Mode: %s.\n",
		cfg.dlqStream, info.State.Msgs, info.State.FirstSeq, info.State.LastSeq, mode)
	if cfg.reasonContains != "" {
		fmt.Printf("Filter: reason contains %q\n", cfg.reasonContains)
	}
	if cfg.class != "" {
		fmt.Printf("Filter: class = %q\n", cfg.class)
	}
	fmt.Println(strings.Repeat("-", 78))

	res := result{}
	first := info.State.FirstSeq
	if cfg.startSeq > first {
		first = cfg.startSeq
	}

	for seq := first; seq <= info.State.LastSeq; seq++ {
		if ctx.Err() != nil {
			fmt.Printf("stopping: %v\n", ctx.Err())
			break
		}
		if cfg.limit > 0 && res.matched >= cfg.limit {
			fmt.Printf("reached -limit=%d matching message(s); stop here and re-run with -start-seq=%d to continue.\n", cfg.limit, seq)
			break
		}

		msg, err := js.GetMsg(cfg.dlqStream, seq, nats.Context(ctx))
		if err != nil {
			// Gaps are expected: a prior run may have deleted a message (-delete), or the stream may
			// have applied retention. Not an error worth stopping the whole scan for.
			if errors.Is(err, nats.ErrMsgNotFound) {
				continue
			}
			fmt.Printf("seq=%d: failed to fetch: %v\n", seq, err)
			res.errored++
			continue
		}
		res.scanned++

		reason := headerValue(msg.Header, sentinelnats.DLQReasonHeader)
		attempts := headerValue(msg.Header, sentinelnats.DLQAttemptsHeader)
		sourceSubject := headerValue(msg.Header, sentinelnats.DLQSourceSubjectHeader)
		msgClass := classOf(msg.Header)

		switch msgClass {
		case sentinelnats.DLQClassPermanent:
			res.seenPermanent++
		case sentinelnats.DLQClassTransient:
			res.seenTransient++
		default:
			res.seenUnclassified++
		}

		if !matchesFilters(cfg, reason, msgClass) {
			continue
		}
		res.matched++

		fmt.Printf("seq=%d\n", seq)
		fmt.Printf("  X-Sentinel-Dlq-Source-Subject: %s\n", orNone(sourceSubject))
		fmt.Printf("  X-Sentinel-Dlq-Attempts:       %s\n", orNone(attempts))
		fmt.Printf("  X-Sentinel-Dlq-Class:          %s\n", msgClass)
		fmt.Printf("  X-Sentinel-Dlq-Reason:         %s\n", orNone(reason))
		if cfg.previewBytes > 0 {
			fmt.Printf("  body (%d bytes, preview): %s\n", len(msg.Data), preview(msg.Data, cfg.previewBytes))
		}

		if sourceSubject == "" {
			fmt.Printf("  SKIP: no %s header — cannot determine where to replay this message.\n", sentinelnats.DLQSourceSubjectHeader)
			res.skippedNoSubj++
			fmt.Println(strings.Repeat("-", 78))
			continue
		}

		if msgClass == classUnclassified && !cfg.allowUnclassifiedReplay {
			fmt.Println("  SKIP: unclassified (parked before class tagging existed) — refusing to replay without -allow-unclassified-replay. Inspect this message's reason/body first.")
			res.skippedUnclassified++
			fmt.Println(strings.Repeat("-", 78))
			continue
		}
		if msgClass == sentinelnats.DLQClassPermanent {
			fmt.Println("  NOTE: class=permanent — the same bytes are expected to fail identically every time. Replaying this is very likely wrong; this tool will still do it because -purge was not requested, but -drain never selects permanent-class messages at all.")
		}

		var hash string
		if cfg.drain {
			hash = contentHash(msg.Data)
			rec := state.Records[hash]
			if cfg.maxReplays > 0 && rec.Count >= cfg.maxReplays {
				fmt.Printf("  SKIP: already replayed %d/%d time(s) per %s and re-parked every time — drain will not retry it further. Investigate manually, or purge it once you've confirmed it can never succeed.\n",
					rec.Count, cfg.maxReplays, cfg.stateFile)
				res.skippedMaxReplays++
				fmt.Println(strings.Repeat("-", 78))
				continue
			}
		}

		if !cfg.execute {
			extra := ""
			if cfg.drain {
				extra = " (drain mode: would also delete on success and record the replay in " + cfg.stateFile + ")"
			}
			fmt.Printf("  DRY-RUN: would republish to subject %q.%s Pass -execute to actually do this.\n", sourceSubject, extra)
			fmt.Println(strings.Repeat("-", 78))
			continue
		}

		// PublishMsg, not Publish: a plain Publish(subject, data) has no way to attach headers, so the
		// replayed message would start a brand-new, disconnected trace even though the original
		// traceparent is sitting right here in msg.Header. replayHeaders decides exactly what survives
		// the trip back onto sourceSubject — see its doc comment for which headers and why.
		ack, err := js.PublishMsg(&nats.Msg{Subject: sourceSubject, Data: msg.Data, Header: replayHeaders(msg.Header)}, nats.Context(ctx))
		if err != nil {
			fmt.Printf("  REPLAY FAILED: publish to %q failed: %v (left in DLQ, not deleted)\n", sourceSubject, err)
			res.errored++
			fmt.Println(strings.Repeat("-", 78))
			continue
		}
		fmt.Printf("  REPLAYED: republished to subject %q (new stream=%s seq=%d)\n", sourceSubject, ack.Stream, ack.Sequence)
		res.replayed++

		if cfg.drain {
			rec := state.Records[hash]
			rec.Count++
			rec.LastReplayedAt = time.Now().UTC()
			rec.LastSeq = seq
			state.Records[hash] = rec
			stateDirty = true
		}

		if cfg.deleteOnReplay {
			if err := js.DeleteMsg(cfg.dlqStream, seq, nats.Context(ctx)); err != nil {
				fmt.Printf("  WARNING: replayed successfully but failed to delete seq=%d from DLQ: %v (it will be replayed again on the next run unless removed manually)\n", seq, err)
			} else {
				fmt.Printf("  DELETED: seq=%d removed from %s\n", seq, cfg.dlqStream)
				res.deleted++
			}
		}
		fmt.Println(strings.Repeat("-", 78))
	}

	if cfg.drain && stateDirty {
		if err := saveState(cfg.stateFile, state); err != nil {
			fmt.Printf("WARNING: failed to persist drain state to %s: %v — replay counts from this run were NOT saved, so -max-replays may undercount next run.\n", cfg.stateFile, err)
		}
	}

	fmt.Printf("\nsummary: scanned=%d matched=%d replayed=%d deleted=%d skipped(no-subject)=%d skipped(unclassified)=%d skipped(max-replays)=%d errors=%d\n",
		res.scanned, res.matched, res.replayed, res.deleted, res.skippedNoSubj, res.skippedUnclassified, res.skippedMaxReplays, res.errored)
	fmt.Printf("classes seen while scanning: permanent=%d transient=%d unclassified=%d\n", res.seenPermanent, res.seenTransient, res.seenUnclassified)
	if res.seenPermanent > 0 {
		fmt.Printf("%d permanent-class message(s) parked. Not replayed by this run (drain never touches them; a plain replay run would need -class=%s explicitly). Use -class=%s -purge -execute to discard them once confirmed unrecoverable.\n",
			res.seenPermanent, sentinelnats.DLQClassPermanent, sentinelnats.DLQClassPermanent)
	}
	if !cfg.execute && res.matched > 0 {
		fmt.Println("this was a dry run — nothing was published or deleted. Re-run with -execute to replay.")
	}
	return nil
}

// classOf normalizes the X-Sentinel-Dlq-Class header into one of the three states this tool reasons
// about: the two real wire values, or classUnclassified when the header is absent (a message parked
// before class tagging existed).
func classOf(h nats.Header) string {
	v := headerValue(h, sentinelnats.DLQClassHeader)
	if v == "" {
		return classUnclassified
	}
	return v
}

// matchesFilters reports whether a message satisfies both -reason-contains and -class (each only applied
// when set; unset filters always pass). Used by both the inspect/replay loop and purgeStream's
// per-message path so the two flags behave identically everywhere they're accepted.
func matchesFilters(cfg config, reason, class string) bool {
	if cfg.reasonContains != "" && !strings.Contains(strings.ToLower(reason), strings.ToLower(cfg.reasonContains)) {
		return false
	}
	if cfg.class != "" && cfg.class != class {
		return false
	}
	return true
}

// purgeStream discards parked messages instead of replaying them.
//
// Replay is the right default and remains it: a dead-lettered message is evidence, and re-publishing gives
// it another chance once the bug that parked it is fixed. But some messages can never succeed no matter how
// many times they are replayed — an event whose project has since been deleted will fail
// "project not found" forever, and each attempt re-parks it, so replaying is worse than doing nothing.
// Those need discarding — X-Sentinel-Dlq-Class=permanent identifies exactly this set going forward, and
// -reason-contains remains available for narrowing further or for handling older, unclassified messages
// an operator has inspected and judged by hand.
//
// Unfiltered, this uses JetStream's server-side purge — one operation regardless of depth. With
// -reason-contains and/or -class set it has to read each message to decide, so it deletes individually
// and honours -limit.
func purgeStream(ctx context.Context, js nats.JetStreamContext, cfg config, info *nats.StreamInfo) error {
	if cfg.reasonContains == "" && cfg.class == "" {
		before := info.State.Msgs
		if err := js.PurgeStream(cfg.dlqStream); err != nil {
			return fmt.Errorf("purging stream %s: %w", cfg.dlqStream, err)
		}
		after, err := js.StreamInfo(cfg.dlqStream)
		if err != nil {
			return fmt.Errorf("re-reading stream info after purge: %w", err)
		}
		fmt.Printf("PURGED %d message(s) from %q. %d remain.\n", before-after.State.Msgs, cfg.dlqStream, after.State.Msgs)
		return nil
	}

	durableSeed := cfg.reasonContains + "_" + cfg.class
	sub, err := js.PullSubscribe("", "dlq_purge_"+sanitizeDurable(durableSeed),
		nats.BindStream(cfg.dlqStream), nats.DeliverAll(), nats.AckExplicit())
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", cfg.dlqStream, err)
	}
	defer sub.Unsubscribe()

	var scanned, purged int
	for cfg.limit == 0 || purged < cfg.limit {
		msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
		if err != nil {
			break // no more messages within the wait: done
		}
		m := msgs[0]
		scanned++
		meta, metaErr := m.Metadata()
		reason := headerValue(m.Header, sentinelnats.DLQReasonHeader)
		class := classOf(m.Header)
		if metaErr == nil && matchesFilters(cfg, reason, class) {
			if delErr := js.DeleteMsg(cfg.dlqStream, meta.Sequence.Stream); delErr != nil {
				return fmt.Errorf("deleting seq %d: %w", meta.Sequence.Stream, delErr)
			}
			purged++
		}
		_ = m.Ack()
	}
	fmt.Printf("PURGED %d of %d message(s) scanned in %q matching reason-contains=%q class=%q.\n",
		purged, scanned, cfg.dlqStream, cfg.reasonContains, cfg.class)
	return nil
}

// sanitizeDurable turns an arbitrary filter string into a legal NATS durable name.
func sanitizeDurable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

func headerValue(h nats.Header, key string) string {
	if h == nil {
		return ""
	}
	return h.Get(key)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func preview(data []byte, n int) string {
	s := string(data)
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
