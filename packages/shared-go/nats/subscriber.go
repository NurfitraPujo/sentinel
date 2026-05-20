package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

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
}

type Subscriber struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	cfg    SubscriberConfig
	errors chan error
	done   chan struct{}
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
	sub, err := s.js.PullSubscribe(s.cfg.Subject, s.cfg.Consumer, nats.BindStream(s.cfg.Stream))
	if err != nil {
		return fmt.Errorf("failed to create pull subscription: %w", err)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			default:
				msgs, err := sub.Fetch(s.cfg.BatchSize, nats.Context(ctx))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					if err != nats.ErrTimeout {
						s.errors <- err
					}
					continue
				}

				for _, msg := range msgs {
					data := msg.Data
					if err := handler(data); err != nil {
						msg.Nak()
						continue
					}
					msg.Ack()
				}
			}
		}
	}()

	return nil
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
