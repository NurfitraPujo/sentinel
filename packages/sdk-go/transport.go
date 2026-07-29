package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// sendMaxAttempts bounds the number of times a single batch is sent
	// before it is dropped. Retries only happen for network errors and 5xx
	// responses - a 4xx response is not retried, since resending the same
	// payload cannot change a validation outcome.
	sendMaxAttempts    = 4
	sendInitialBackoff = 200 * time.Millisecond
	sendMaxBackoff     = 5 * time.Second
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

	endpoint := strings.TrimSuffix(t.cfg.Endpoint, "/")
	if len(batch) > 1 {
		endpoint = fmt.Sprintf("%s/batch", endpoint)
	}

	var body []byte
	var err error
	if len(batch) == 1 {
		body, err = json.Marshal(batch[0])
	} else {
		body, err = json.Marshal(batch)
	}
	if err != nil {
		t.dropBatch(batch, fmt.Errorf("sentinel: marshal batch: %w", err))
		return
	}

	backoff := sendInitialBackoff
	var lastErr error

	for attempt := 1; attempt <= sendMaxAttempts; attempt++ {
		status, respBody, sendErr := t.doSend(endpoint, body)

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
		} else {
			// 4xx: not retryable - the payload itself was rejected and
			// resending it unchanged cannot succeed.
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
		time.Sleep(backoff)
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

// doSend performs a single HTTP POST attempt and returns the response status
// code and (truncated) body. err is non-nil only for transport-level
// failures (DNS, connection refused, timeout, etc.) - a non-2xx HTTP
// response is reported via the returned status code, not err.
func (t *Transport) doSend(endpoint string, body []byte) (statusCode int, respBody []byte, err error) {
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.cfg.APIKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return resp.StatusCode, respBody, nil
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
