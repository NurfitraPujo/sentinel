package sentinel

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

type Client struct {
	cfg       Config
	transport *Transport
	mu        sync.RWMutex
}

var (
	defaultClient *Client
	once          sync.Once
)

func Init(cfg Config) *Client {
	c := &Client{
		cfg:       cfg.withDefaults(),
		transport: NewTransport(cfg.withDefaults()),
	}
	defaultClient = c
	return c
}

func getDefaultClient() *Client {
	once.Do(func() {
		if defaultClient == nil {
			defaultClient = Init(Config{})
		}
	})
	return defaultClient
}

func CaptureError(err error, additionalContext ...map[string]interface{}) {
	getDefaultClient().CaptureErrorContext(context.Background(), err, additionalContext...)
}

func CaptureErrorContext(ctx context.Context, err error, additionalContext ...map[string]interface{}) {
	getDefaultClient().CaptureErrorContext(ctx, err, additionalContext...)
}

func (c *Client) CaptureErrorContext(ctx context.Context, err error, additionalContext ...map[string]interface{}) {
	if err == nil {
		return
	}

	if c.cfg.SampleRate < 1.0 && rand.Float64() > c.cfg.SampleRate {
		return
	}

	ctxTags := getTagsMap(ctx)
	if ctxTags == nil {
		ctxTags = make(map[string]interface{})
	}

	if len(additionalContext) > 0 && additionalContext[0] != nil {
		merged := make(map[string]interface{}, len(ctxTags)+len(additionalContext[0]))
		for k, v := range ctxTags {
			merged[k] = v
		}
		for k, v := range additionalContext[0] {
			merged[k] = v
		}
		ctxTags = merged
	}

	event := NewEvent(c.cfg, err, ctxTags)
	traceID, spanID := ExtractTraceIDs(ctx)
	event.TraceID = traceID
	event.SpanID = spanID

	c.transport.Push(event)
}

func Flush(timeout time.Duration) bool {
	if defaultClient != nil && defaultClient.transport != nil {
		return defaultClient.transport.Flush(timeout)
	}
	return true
}
