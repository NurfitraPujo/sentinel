package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// entryMarshalDecodeError reports whether v cannot be marshalled by
// encoding/json, or marshals but cannot be decoded back the way the ingestor
// decodes it (a number beyond float64, nesting past encoding/json's 10,000
// level limit) - spec §3.5 step 2.
func entryMarshalDecodeError(v interface{}) bool {
	raw, err := json.Marshal(v)
	if err != nil {
		return true
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return true
	}
	return false
}

// removeFailingEntries is spec §3.5 step 2's isolation: a value inside
// metadata, contexts, request.data or a crumb's data that cannot be
// marshalled or decoded costs only its own entry, never the event. It
// mutates b's own copies (never the pushed event) and returns the names it
// removed, for the SDK's marker.
func removeFailingEntries(b *Event) (removedContexts, removedMetadata []string) {
	if len(b.Metadata) > 0 {
		out := make(map[string]interface{}, len(b.Metadata))
		for k, v := range b.Metadata {
			if entryMarshalDecodeError(v) {
				removedMetadata = append(removedMetadata, k)
				continue
			}
			out[k] = v
		}
		b.Metadata = out
	}
	if len(b.Contexts) > 0 {
		out := make(map[string]map[string]interface{}, len(b.Contexts))
		for k, v := range b.Contexts {
			if entryMarshalDecodeError(v) {
				removedContexts = append(removedContexts, k)
				continue
			}
			out[k] = v
		}
		b.Contexts = out
	}
	if b.Request != nil && b.Request.Data != nil && entryMarshalDecodeError(b.Request.Data) {
		req := *b.Request
		req.Data = nil
		b.Request = &req
	}
	if len(b.Breadcrumbs) > 0 {
		crumbs := make([]Breadcrumb, len(b.Breadcrumbs))
		copy(crumbs, b.Breadcrumbs)
		for i := range crumbs {
			if crumbs[i].Data != nil && entryMarshalDecodeError(crumbs[i].Data) {
				crumbs[i].Data = nil
			}
		}
		b.Breadcrumbs = crumbs
	}
	return removedContexts, removedMetadata
}

// prepareEvent runs spec §3.5 steps 1-4 on ev and returns the JSON body it
// is sent as, never writing into ev itself.
func prepareEvent(ev *Event, cfg Config) (raw []byte, dropErr error) {
	b, mirror, raw, err := boundAndMeasure(ev, cfg)
	if err != nil {
		return nil, err
	}
	return shedToFit(b, mirror, raw, cfg)
}

// boundAndMeasure runs spec §3.5 steps 1-3 on ev - bind a copy, isolate what
// cannot be marshalled/decoded, then apply the decoded-bounds step - and
// returns the bound event, its decode mirror and the JSON body those steps
// produce, all before step 4's shedding. Exposed (over prepareEvent alone)
// so a test can measure the one-element array size and publishCost a
// construction would reach before any shedding runs (spec §10.2 P1.11).
func boundAndMeasure(ev *Event, cfg Config) (*Event, *decodeMirror, []byte, error) {
	b := boundEvent(ev, cfg.MaxBreadcrumbs)

	marker := &sdkMarker{}
	raw, err := json.Marshal(b)
	if err != nil || entryMarshalDecodeError(json.RawMessage(raw)) {
		removedContexts, removedMetadata := removeFailingEntries(b)
		if len(removedContexts) > 0 {
			marker.contexts = true
			for _, k := range removedContexts {
				marker.addKey("contexts." + k)
			}
		}
		if len(removedMetadata) > 0 {
			marker.metadata = true
			for _, k := range removedMetadata {
				marker.addKey(k)
			}
		}
		raw, err = json.Marshal(b)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sentinel: event %s could not be marshalled after removing failing entries: %w", b.EventID, err)
		}
	}

	var mirror decodeMirror
	if err := json.Unmarshal(raw, &mirror); err != nil {
		return nil, nil, nil, fmt.Errorf("sentinel: event %s could not be decoded as the ingestor will: %w", b.EventID, err)
	}

	if len(b.Contexts) > 0 {
		contexts, removed := boundContextsDecoded(mirror.Contexts)
		if len(removed) > 0 {
			marker.contexts = true
			for _, k := range removed {
				marker.addKey("contexts." + k)
			}
		}
		b.Contexts = contexts
	}
	b.Metadata = boundMetadataDecoded(mirror.Metadata, marker)
	if b.Request != nil {
		var data interface{}
		if mirror.Request != nil {
			data = mirror.Request.Data
		}
		req := *b.Request
		req.Data = boundRequestDataDecoded(data)
		b.Request = &req
	}
	if len(b.Breadcrumbs) > 0 {
		crumbs := make([]Breadcrumb, len(b.Breadcrumbs))
		copy(crumbs, b.Breadcrumbs)
		dataList := make([]map[string]interface{}, len(crumbs))
		for i := range crumbs {
			var d map[string]interface{}
			if i < len(mirror.Breadcrumbs) {
				d = mirror.Breadcrumbs[i].Data
			}
			dataList[i] = boundBreadcrumbDataDecoded(d)
		}
		crumbs, dataList = boundBreadcrumbListSize(crumbs, dataList)
		for i := range crumbs {
			crumbs[i].Data = dataList[i]
		}
		b.Breadcrumbs = crumbs
	}

	b.Metadata = marker.applyMarker(b.Metadata)
	mirror.Metadata = b.Metadata

	raw, err = json.Marshal(b)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sentinel: event %s could not be marshalled after bounding: %w", b.EventID, err)
	}

	return b, &mirror, raw, nil
}

// shedToFit is spec §3.5 step 4: while the one-element array is over
// 5,242,880 bytes or the publish cost is over 1,000,000 bytes, shed one
// field and measure both again, in order: (a) request.headers/env, (b)
// request.data, (c) breadcrumbs, (d) drop the event alone. Each step
// changes only b (and a copy of b.Request), never the pushed event.
func shedToFit(b *Event, m *decodeMirror, raw []byte, cfg Config) ([]byte, error) {
	fits := func(raw []byte) bool {
		arraySize := len(raw) + 2 // the enclosing brackets
		return arraySize <= maxBatchBodyBytes && publishCost(b, m) <= maxPublishBytes
	}
	if fits(raw) {
		return raw, nil
	}

	logShed := func(step string) {
		if cfg.Debug {
			log.Printf("sentinel: event %s shed %s to fit the ingestor's per-event limits", b.EventID, step)
		}
	}

	// (a) request.headers and request.env.
	if b.Request != nil && (len(b.Request.Headers) > 0 || len(b.Request.Env) > 0) {
		req := *b.Request
		req.Headers = nil
		req.Env = nil
		b.Request = &req
		var err error
		raw, err = json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("sentinel: event %s could not be marshalled while shedding headers: %w", b.EventID, err)
		}
		logShed("request.headers/env")
		if fits(raw) {
			return raw, nil
		}
	}

	// (b) request.data.
	if b.Request != nil && b.Request.Data != nil {
		req := *b.Request
		req.Data = nil
		b.Request = &req
		m.Request = nil
		var err error
		raw, err = json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("sentinel: event %s could not be marshalled while shedding request.data: %w", b.EventID, err)
		}
		logShed("request.data")
		if fits(raw) {
			return raw, nil
		}
	}

	// (c) breadcrumbs.
	if len(b.Breadcrumbs) > 0 {
		b.Breadcrumbs = nil
		m.Breadcrumbs = nil
		var err error
		raw, err = json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("sentinel: event %s could not be marshalled while shedding breadcrumbs: %w", b.EventID, err)
		}
		logShed("breadcrumbs")
		if fits(raw) {
			return raw, nil
		}
	}

	// (d) drop the event alone.
	arraySize := len(raw) + 2
	return nil, fmt.Errorf("sentinel: event %s dropped: over the ingestor's per-event limits (array %d bytes, publish cost %d bytes) even with request maps, request data and breadcrumbs shed",
		b.EventID, arraySize, publishCost(b, m))
}

const (
	// sendMaxAttempts bounds the number of times a single batch is sent
	// before it is dropped. Retries happen for network errors, 5xx responses
	// and 429 - every OTHER 4xx is not retried, since resending the same
	// payload cannot change a validation outcome.
	sendMaxAttempts    = 4
	sendInitialBackoff = 200 * time.Millisecond
	sendMaxBackoff     = 5 * time.Second
	// sendMaxRetryAfter caps how long a server-supplied Retry-After can park a
	// batch. The ingestor sends 60s (middleware/ratelimit.go); this bound is
	// what stops a misconfigured or hostile value from parking events for
	// hours. Waiting is also interruptible - see the sleep below - so a Close
	// or Flush does not have to sit through it.
	sendMaxRetryAfter = 60 * time.Second
	// maxErrorBodyBytes caps how much of a non-2xx response body is read,
	// so a misbehaving server can't make the SDK buffer an unbounded amount
	// of memory just to log an error.
	maxErrorBodyBytes = 4096
)

type Transport struct {
	cfg          Config
	client       *http.Client
	eventChan    chan *Event
	droppedCount uint64
	wg           sync.WaitGroup
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewTransport(cfg Config) *Transport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:       cfg,
		eventChan: make(chan *Event, cfg.MaxBufferSize),
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 10 * time.Second,
		},
		ctx:    ctx,
		cancel: cancel,
	}

	t.wg.Add(1)
	go t.workerLoop()
	return t
}

func (t *Transport) Push(event *Event) {
	select {
	case t.eventChan <- event:
	default:
		// FIFO eviction: drain 1 item then push
		select {
		case <-t.eventChan:
			atomic.AddUint64(&t.droppedCount, 1)
		default:
		}
		select {
		case t.eventChan <- event:
		default:
			atomic.AddUint64(&t.droppedCount, 1)
		}
	}
}

func (t *Transport) workerLoop() {
	defer t.wg.Done()
	batch := make([]*Event, 0, t.cfg.BatchSize)
	ticker := time.NewTicker(t.cfg.BatchWait)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			t.drainBatch(batch)
			return
		case event, ok := <-t.eventChan:
			if !ok {
				t.drainBatch(batch)
				return
			}
			batch = append(batch, event)
			if len(batch) >= t.cfg.BatchSize {
				t.sendBatch(batch)
				batch = make([]*Event, 0, t.cfg.BatchSize)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				t.sendBatch(batch)
				batch = make([]*Event, 0, t.cfg.BatchSize)
			}
		}
	}
}

func (t *Transport) drainBatch(batch []*Event) {
	// Drain remaining events in channel
	for {
		select {
		case event, ok := <-t.eventChan:
			if !ok {
				if len(batch) > 0 {
					t.sendBatch(batch)
				}
				return
			}
			batch = append(batch, event)
			if len(batch) >= t.cfg.BatchSize {
				t.sendBatch(batch)
				batch = make([]*Event, 0, t.cfg.BatchSize)
			}
		default:
			if len(batch) > 0 {
				t.sendBatch(batch)
			}
			return
		}
	}
}

// sendBatch delivers batch to the ingestor, retrying transient failures with
// capped exponential backoff. It always runs on the transport's internal
// worker goroutine (from workerLoop/drainBatch), never on the caller's
// goroutine - Push() never calls this directly - so any blocking here
// (backoff sleeps, retries) never blocks application code (decision D8).
//
// Prior to this, a 4xx/5xx response was completely silent: the status code
// was never inspected, so a 100% rejection rate (VERIFIED_STATE.md S4)
// produced no error, log line, or metric anywhere. That is fixed here.
func (t *Transport) sendBatch(batch []*Event) {
	if len(batch) == 0 {
		return
	}

	// Every event is bound and marshalled per event (spec §3.5): a value
	// that cannot be marshalled or decoded costs only its own event, never
	// the whole batch, and a per-event JSON body is what step 4 (C3)
	// measures and sheds. Here (C2) the survivors are simply joined into one
	// array, a lone event included.
	perEvent := make([][]byte, 0, len(batch))
	sent := make([]*Event, 0, len(batch))
	for _, ev := range batch {
		raw, err := prepareEvent(ev, t.cfg)
		if err != nil {
			t.dropBatch([]*Event{ev}, err)
			continue
		}
		perEvent = append(perEvent, raw)
		sent = append(sent, ev)
	}
	if len(sent) == 0 {
		return
	}

	// Cut the joined body at the ingestor's batch-endpoint limits (spec
	// §3.5 step 4, §8 guarantee 3): 5,242,880 bytes (maxBatchBodyBytes,
	// brackets and commas counted) or 500 events (maxBatchEvents). Every
	// element already fits alone under maxBatchBodyBytes - shedToFit
	// guarantees that for each event individually - so a chunk never has to
	// split a single element.
	start := 0
	size := 2 // the enclosing brackets
	for i := range perEvent {
		add := len(perEvent[i])
		comma := 0
		if i > start {
			comma = 1
		}
		if i > start && (size+comma+add > maxBatchBodyBytes || i-start >= maxBatchEvents) {
			t.sendChunk(sent[start:i], perEvent[start:i])
			start = i
			size = 2
			comma = 0
		}
		size += comma + add
	}
	t.sendChunk(sent[start:], perEvent[start:])
}

// sendChunk sends a single POST of raws (already cut to fit the ingestor's
// batch-endpoint limits by sendBatch) for batch, retrying transient failures
// with capped exponential backoff.
func (t *Transport) sendChunk(batch []*Event, raws [][]byte) {
	if len(batch) == 0 {
		return
	}

	endpoint := strings.TrimSuffix(t.cfg.Endpoint, "/") + "/batch"
	body := joinBatchBody(raws)

	backoff := sendInitialBackoff
	var lastErr error

	for attempt := 1; attempt <= sendMaxAttempts; attempt++ {
		status, respBody, retryAfter, sendErr := t.doSend(endpoint, body)

		// wait is the delay before the next attempt: exponential backoff, unless the
		// server told us how long to wait, in which case it wins.
		wait := backoff

		if sendErr != nil {
			// Network error: always retryable.
			lastErr = fmt.Errorf("sentinel: send batch: %w", sendErr)
		} else if status >= 200 && status < 300 {
			// Success at the HTTP level. For a multi-event batch this does
			// NOT mean every event was ingested (VERIFIED_STATE.md S15):
			// apps/ingestor-go's /ingest/batch endpoint returns 2xx once at
			// least one item ingested, and reports per-item failures in the
			// response body (see batchIngestResult). Prior to this, that
			// body was never read: a batch of 3 valid + 2 invalid events
			// was recorded as a complete success - nothing logged under
			// Debug, OnError never called, droppedCount never incremented
			// for the 2 that failed. Surface those the same way a 4xx does.
			t.handlePartialFailure(batch, respBody)
			return
		} else if status >= 500 {
			// Server error: retryable.
			lastErr = fmt.Errorf("sentinel: ingest returned status %d", status)
		} else if status == http.StatusTooManyRequests {
			// Rate limited. This is the one 4xx where resending IS the correct
			// action, and the ingestor even says when: it answers 429 with
			// `Retry-After: 60` (apps/ingestor-go/middleware/ratelimit.go).
			// This branch used to fall through to the generic 4xx case below and
			// discard the batch outright, so a rate-limited client silently threw
			// away telemetry it could have delivered a minute later.
			lastErr = fmt.Errorf("sentinel: ingest rate limited: status=%d", status)
			if retryAfter > 0 {
				wait = retryAfter
			}
			if t.cfg.Debug {
				log.Printf("sentinel: rate limited, retrying %d event(s) in %s", len(batch), wait)
			}
		} else {
			// Every other 4xx: not retryable - the payload itself was rejected
			// and resending it unchanged cannot succeed.
			lastErr = fmt.Errorf("sentinel: ingest rejected batch: status=%d body=%s", status, respBody)
			if t.cfg.Debug {
				log.Printf("sentinel: dropping %d event(s): status=%d body=%s", len(batch), status, respBody)
			}
			t.dropBatch(batch, lastErr)
			return
		}

		if attempt == sendMaxAttempts {
			break
		}
		// A plain sleep, deliberately NOT selected against t.ctx: Flush cancels that
		// context before draining (see Flush), so waiting on it here would mean no
		// batch is ever retried during a flush — silently disabling the existing 5xx
		// retries too, which is a worse failure than the one this fix addresses.
		//
		// The cost is that a batch parked on a 60s Retry-After can outlast a short
		// Flush timeout, which then returns false. That is what Flush's bool already
		// means, and sendMaxRetryAfter bounds how long it can be.
		time.Sleep(wait)
		backoff *= 2
		if backoff > sendMaxBackoff {
			backoff = sendMaxBackoff
		}
	}

	// Retries exhausted: either a persistent network error or persistent 5xx.
	if t.cfg.Debug {
		log.Printf("sentinel: dropping %d event(s) after %d attempts: %v", len(batch), sendMaxAttempts, lastErr)
	}
	t.dropBatch(batch, lastErr)
}

// joinBatchBody joins raws into the JSON array body sendChunk posts.
func joinBatchBody(raws [][]byte) []byte {
	size := 2
	for i, r := range raws {
		size += len(r)
		if i > 0 {
			size++
		}
	}
	body := make([]byte, 0, size)
	body = append(body, '[')
	for i, raw := range raws {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, raw...)
	}
	body = append(body, ']')
	return body
}

// doSend performs a single HTTP POST attempt and returns the response status
// code, (truncated) body and any Retry-After the server asked for. err is
// non-nil only for transport-level failures (DNS, connection refused, timeout,
// etc.) - a non-2xx HTTP response is reported via the returned status code,
// not err.
func (t *Transport) doSend(endpoint string, body []byte) (statusCode int, respBody []byte, retryAfter time.Duration, err error) {
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.cfg.APIKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, nil, 0, err
	}
	defer resp.Body.Close()

	respBody, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return resp.StatusCode, respBody, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

// parseRetryAfter reads an HTTP Retry-After header, which RFC 9110 allows in two
// forms: delay-seconds, or an HTTP-date. The ingestor sends the seconds form
// ("60"); the date form is handled anyway because this header comes from
// whatever sits in front of the ingestor too — a proxy or gateway rate-limiting
// ahead of it may well send a date.
//
// Returns 0 for anything unparseable, negative, or already in the past, which
// makes the caller fall back to its own exponential backoff. The result is
// capped at sendMaxRetryAfter so a bad value cannot park a batch indefinitely.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}

	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > sendMaxRetryAfter {
			return sendMaxRetryAfter
		}
		return d
	}

	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d <= 0 {
			return 0
		}
		if d > sendMaxRetryAfter {
			return sendMaxRetryAfter
		}
		return d
	}

	return 0
}

// batchIngestResult mirrors apps/ingestor-go/main.go's batchResult - the
// response body of a 2xx from POST {endpoint}/batch. It is a cross-boundary
// wire contract with no compiler checking it (ARCHITECTURE.md B5): if the
// ingestor's batchResult shape changes, this must change in the same
// change. The single-event endpoint's success body, {"status":"accepted"},
// has none of these fields and decodes harmlessly to a zero value.
type batchIngestResult struct {
	Ingested int `json:"ingested"`
	Failed   int `json:"failed"`
	Errors   []struct {
		Index   int    `json:"index"`
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// handlePartialFailure inspects a 2xx response body for the per-item
// failures the /ingest/batch endpoint reports (see batchIngestResult) and,
// if any are present, surfaces them exactly the way a 4xx response does
// today: a Debug log line, Config.OnError, and an increment to the dropped
// count - for only the items that actually failed, not the whole batch.
//
// respBody may not parse as JSON at all, or may parse but not be this
// shape - an older ingestor that predates P2-5, a proxy in front of it that
// strips or rewrites the body, or the single-event endpoint's bare
// {"status":"accepted"}. All of those must be handled without panicking or
// blocking the caller; on any doubt this treats the batch as a full
// success rather than risk manufacturing a false drop.
func (t *Transport) handlePartialFailure(batch []*Event, respBody []byte) {
	var result batchIngestResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return
	}
	if result.Failed <= 0 {
		return
	}

	// Defensive clamp: never report/drop more than were actually sent, in
	// case of a malformed or malicious response body.
	failed := result.Failed
	if failed > len(batch) {
		failed = len(batch)
	}

	err := fmt.Errorf("sentinel: ingest accepted batch with %d/%d event(s) failed", failed, len(batch))
	if t.cfg.Debug {
		log.Printf("sentinel: %d of %d event(s) in batch rejected: %v", failed, len(batch), result.Errors)
	}
	atomic.AddUint64(&t.droppedCount, uint64(failed))
	if t.cfg.OnError != nil {
		t.cfg.OnError(err)
	}
}

// dropBatch accounts for a batch that will never be delivered and, if
// configured, notifies the application via Config.OnError. It must not
// block: OnError runs on the internal worker goroutine, and a slow/blocking
// hook would delay all subsequent batches.
func (t *Transport) dropBatch(batch []*Event, err error) {
	atomic.AddUint64(&t.droppedCount, uint64(len(batch)))
	if t.cfg.OnError != nil {
		t.cfg.OnError(err)
	}
}

func (t *Transport) Flush(timeout time.Duration) bool {
	t.cancel()
	c := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(c)
	}()

	select {
	case <-c:
		return true
	case <-time.After(timeout):
		return false
	}
}
