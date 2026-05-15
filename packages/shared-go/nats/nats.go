package nats

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
)

type PublisherConfig struct {
	URL         string
	Subject     string
	Timeout     time.Duration
	NKeySeed    string
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string
}

type Publisher struct {
	conn    *nats.Conn
	js      nats.JetStreamContext
	subject string
	timeout time.Duration
}

func NewPublisher(ctx context.Context, cfg PublisherConfig) (*Publisher, error) {
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

	return &Publisher{
		conn:    conn,
		js:      js,
		subject: cfg.Subject,
		timeout: cfg.Timeout,
	}, nil
}

func (p *Publisher) Publish(ctx context.Context, data []byte) error {
	_, err := p.js.Publish(p.subject, data, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}
	return nil
}

func (p *Publisher) Close() error {
	if p.conn != nil {
		p.conn.Close()
	}
	return nil
}

func buildTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	tlsConfig := &tls.Config{}

	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert")
		}
		tlsConfig.RootCAs = caPool
	}

	return tlsConfig, nil
}
