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
//	go run ./tools/dlq -execute -limit=10              # actually replay (up to) 10 of them
//	go run ./tools/dlq -execute -delete -limit=10      # replay AND remove from the DLQ on success
//
// -delete is intentionally separate from -execute: most dead letters in this repo are schema/constraint
// bugs (VERIFIED_STATE.md), so replay-after-fix is the expected use case, and a replay that fails again
// for the same reason should not have destroyed the evidence. Nothing is ever deleted from the DLQ
// unless the operator passes -delete explicitly, and even then only for messages that were just
// successfully re-published.
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
)

func main() {
	var (
		natsURL        = flag.String("nats-url", getEnv("NATS_URL", "nats://localhost:4222"), "NATS server URL")
		nkeySeed       = flag.String("nkey-seed", os.Getenv("NATS_NKEY_SEED"), "optional NKey seed for authentication")
		dlqStream      = flag.String("stream", getEnv("DLQ_STREAM", "ERROR_EVENTS_DLQ"), "DLQ stream to read from")
		limit          = flag.Int("limit", 10, "maximum number of DLQ messages to inspect/replay in this run (0 = no limit, use with care)")
		startSeq       = flag.Uint64("start-seq", 0, "skip stream sequences below this one (for paging through a DLQ larger than -limit)")
		reasonContains = flag.String("reason-contains", "", "only operate on messages whose X-Sentinel-Dlq-Reason contains this substring (case-insensitive) — the normal way to target a specific, now-fixed bug class")
		execute        = flag.Bool("execute", false, "actually re-publish matching messages onto their original subject. Without this flag the tool only lists what it WOULD do.")
		deleteOnReplay = flag.Bool("delete", false, "after a message is successfully re-published, delete it from the DLQ stream. Has no effect without -execute. Off by default: a replay that fails again should not have destroyed the evidence.")
		timeout        = flag.Duration("timeout", 30*time.Second, "overall timeout for the run")
		previewBytes   = flag.Int("preview-bytes", 200, "how many bytes of each message body to print for context (0 to disable)")
		purge          = flag.Bool("purge", false, "DISCARD matching messages instead of replaying them. Requires -execute. Use when the parked messages can never succeed — e.g. events whose project no longer exists — because replaying those just re-fails and re-parks them. Combine with -reason-contains to purge one bug class and keep the rest.")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, config{
		natsURL:        *natsURL,
		nkeySeed:       *nkeySeed,
		dlqStream:      *dlqStream,
		limit:          *limit,
		startSeq:       *startSeq,
		reasonContains: *reasonContains,
		execute:        *execute,
		deleteOnReplay: *deleteOnReplay,
		previewBytes:   *previewBytes,
		purge:          *purge,
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
	execute        bool
	deleteOnReplay bool
	previewBytes   int
}

type result struct {
	scanned       int
	matched       int
	replayed      int
	deleted       int
	skippedNoSubj int
	errored       int
}

func run(ctx context.Context, cfg config) error {
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
		if cfg.reasonContains != "" {
			fmt.Printf("Would discard only messages whose reason contains %q.\n", cfg.reasonContains)
		} else {
			fmt.Printf("Would discard ALL %d message(s). Add -reason-contains to narrow this.\n", info.State.Msgs)
		}
		fmt.Println("Re-run with -execute to proceed. This is not reversible.")
		return nil
	}
	if cfg.purge {
		return purgeStream(ctx, js, cfg, info)
	}

	mode := "DRY RUN (pass -execute to actually replay)"
	if cfg.execute {
		mode = "EXECUTE (will re-publish matching messages)"
		if cfg.deleteOnReplay {
			mode += " + DELETE on success"
		}
	}
	fmt.Printf("DLQ stream %q: %d message(s) currently parked, sequence range [%d, %d]. Mode: %s.\n",
		cfg.dlqStream, info.State.Msgs, info.State.FirstSeq, info.State.LastSeq, mode)
	if cfg.reasonContains != "" {
		fmt.Printf("Filter: reason contains %q\n", cfg.reasonContains)
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

		reason := headerValue(msg.Header, "X-Sentinel-Dlq-Reason")
		attempts := headerValue(msg.Header, "X-Sentinel-Dlq-Attempts")
		sourceSubject := headerValue(msg.Header, "X-Sentinel-Dlq-Source-Subject")

		if cfg.reasonContains != "" && !strings.Contains(strings.ToLower(reason), strings.ToLower(cfg.reasonContains)) {
			continue
		}
		res.matched++

		fmt.Printf("seq=%d\n", seq)
		fmt.Printf("  X-Sentinel-Dlq-Source-Subject: %s\n", orNone(sourceSubject))
		fmt.Printf("  X-Sentinel-Dlq-Attempts:       %s\n", orNone(attempts))
		fmt.Printf("  X-Sentinel-Dlq-Reason:         %s\n", orNone(reason))
		if cfg.previewBytes > 0 {
			fmt.Printf("  body (%d bytes, preview): %s\n", len(msg.Data), preview(msg.Data, cfg.previewBytes))
		}

		if sourceSubject == "" {
			fmt.Printf("  SKIP: no %s header — cannot determine where to replay this message.\n", "X-Sentinel-Dlq-Source-Subject")
			res.skippedNoSubj++
			fmt.Println(strings.Repeat("-", 78))
			continue
		}

		if !cfg.execute {
			fmt.Printf("  DRY-RUN: would republish to subject %q. Pass -execute to actually do this.\n", sourceSubject)
			fmt.Println(strings.Repeat("-", 78))
			continue
		}

		ack, err := js.Publish(sourceSubject, msg.Data, nats.Context(ctx))
		if err != nil {
			fmt.Printf("  REPLAY FAILED: publish to %q failed: %v (left in DLQ, not deleted)\n", sourceSubject, err)
			res.errored++
			fmt.Println(strings.Repeat("-", 78))
			continue
		}
		fmt.Printf("  REPLAYED: republished to subject %q (new stream=%s seq=%d)\n", sourceSubject, ack.Stream, ack.Sequence)
		res.replayed++

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

	fmt.Printf("\nsummary: scanned=%d matched=%d replayed=%d deleted=%d skipped(no-subject)=%d errors=%d\n",
		res.scanned, res.matched, res.replayed, res.deleted, res.skippedNoSubj, res.errored)
	if !cfg.execute && res.matched > 0 {
		fmt.Println("this was a dry run — nothing was published or deleted. Re-run with -execute to replay.")
	}
	return nil
}

// purgeStream discards parked messages instead of replaying them.
//
// Replay is the right default and remains it: a dead-lettered message is evidence, and re-publishing gives
// it another chance once the bug that parked it is fixed. But some messages can never succeed no matter how
// many times they are replayed — an event whose project has since been deleted will fail
// "project not found" forever, and each attempt re-parks it, so replaying is worse than doing nothing.
// Those need discarding, and until now this tool had no way to do it, which is why 6,148 permanently-dead
// messages accumulated until JetStream returned "insufficient storage resources" and started failing
// unrelated integration tests.
//
// Unfiltered, this uses JetStream's server-side purge — one operation regardless of depth. With
// -reason-contains it has to read each message to decide, so it deletes individually and honours -limit.
func purgeStream(ctx context.Context, js nats.JetStreamContext, cfg config, info *nats.StreamInfo) error {
	if cfg.reasonContains == "" {
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

	sub, err := js.PullSubscribe("", "dlq_purge_"+sanitizeDurable(cfg.reasonContains),
		nats.BindStream(cfg.dlqStream), nats.DeliverAll(), nats.AckExplicit())
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", cfg.dlqStream, err)
	}
	defer sub.Unsubscribe()

	needle := strings.ToLower(cfg.reasonContains)
	var scanned, purged int
	for cfg.limit == 0 || purged < cfg.limit {
		msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
		if err != nil {
			break // no more messages within the wait: done
		}
		m := msgs[0]
		scanned++
		meta, metaErr := m.Metadata()
		reason := headerValue(m.Header, "X-Sentinel-Dlq-Reason")
		if strings.Contains(strings.ToLower(reason), needle) && metaErr == nil {
			if delErr := js.DeleteMsg(cfg.dlqStream, meta.Sequence.Stream); delErr != nil {
				return fmt.Errorf("deleting seq %d: %w", meta.Sequence.Stream, delErr)
			}
			purged++
		}
		_ = m.Ack()
	}
	fmt.Printf("PURGED %d of %d message(s) scanned in %q whose reason contained %q.\n",
		purged, scanned, cfg.dlqStream, cfg.reasonContains)
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
