package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
		return
	}

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.cfg.ProjectKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
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
