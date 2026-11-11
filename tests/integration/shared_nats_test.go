package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sharednats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type natsPackageFixture struct {
	url      string
	stream   string
	subject  string
	consumer string
	js       gonats.JetStreamContext
}

var natsPackageSequence atomic.Uint64

func newNATSPackageFixture(t *testing.T) *natsPackageFixture {
	t.Helper()

	env := tc.Setup(t, tc.WithResources(tc.NATSResource))
	natsURL := env.NATSConfig.URL
	require.NotEmpty(t, natsURL, "NATS test URL must be configured")

	nc, err := gonats.Connect(natsURL)
	require.NoError(t, err)

	js, err := nc.JetStream()
	require.NoError(t, err)

	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), natsPackageSequence.Add(1))
	fixture := &natsPackageFixture{
		url:      natsURL,
		stream:   "U12_" + id,
		subject:  "test.u12." + id,
		consumer: "u12_" + id,
		js:       js,
	}

	_, err = js.AddStream(&gonats.StreamConfig{
		Name:     fixture.stream,
		Subjects: []string{fixture.subject},
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = js.DeleteStream(fixture.stream)
		nc.Close()
	})

	return fixture
}

func newNATSPackagePublisher(t *testing.T, ctx context.Context, url, subject string, configure func(*sharednats.PublisherConfig)) *sharednats.Publisher {
	t.Helper()

	cfg := sharednats.PublisherConfig{
		URL:     url,
		Subject: subject,
		Timeout: time.Second,
	}
	if configure != nil {
		configure(&cfg)
	}

	publisher, err := sharednats.NewPublisher(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, publisher.Close())
	})
	return publisher
}

func newNATSPackageSubscriber(t *testing.T, ctx context.Context, fixture *natsPackageFixture, url string, configure func(*sharednats.SubscriberConfig)) *sharednats.Subscriber {
	t.Helper()

	cfg := sharednats.SubscriberConfig{
		URL:       url,
		Stream:    fixture.stream,
		Subject:   fixture.subject,
		Consumer:  fixture.consumer,
		BatchSize: 1,
		BatchWait: 100 * time.Millisecond,
	}
	if configure != nil {
		configure(&cfg)
	}

	subscriber, err := sharednats.NewSubscriber(ctx, cfg)
	require.NoError(t, err)
	return subscriber
}

func TestNatsPackagePublisherPublish(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher := newNATSPackagePublisher(t, ctx, fixture.url, fixture.subject, nil)
	require.NoError(t, publisher.Publish(ctx, []byte("publisher-success")))

	require.Eventually(t, func() bool {
		info, err := fixture.js.StreamInfo(fixture.stream)
		return err == nil && info.State.Msgs == 1
	}, 2*time.Second, 20*time.Millisecond)
}

func TestNatsPackageRoundTrip(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	certFile, keyFile, serverCertificate := writeNATSTestCertificate(t)
	tlsURL := startNATSTLSProxy(t, fixture.url, serverCertificate)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	configureTLS := func(certFile, keyFile string) func(*sharednats.PublisherConfig) {
		return func(cfg *sharednats.PublisherConfig) {
			cfg.TLSCertFile = certFile
			cfg.TLSKeyFile = keyFile
			cfg.TLSCAFile = certFile
		}
	}
	publisher := newNATSPackagePublisher(t, ctx, tlsURL, fixture.subject, configureTLS(certFile, keyFile))

	subscriber := newNATSPackageSubscriber(t, ctx, fixture, tlsURL, func(cfg *sharednats.SubscriberConfig) {
		cfg.TLSCertFile = certFile
		cfg.TLSKeyFile = keyFile
		cfg.TLSCAFile = certFile
	})
	t.Cleanup(func() {
		cancel()
		require.NoError(t, subscriber.Close())
	})

	received := make(chan []byte, 1)
	require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
		received <- append([]byte(nil), data...)
		return nil
	}))

	want := []byte("subscriber-round-trip")
	require.NoError(t, publisher.Publish(ctx, want))

	select {
	case got := <-received:
		assert.Equal(t, want, got)
	case err := <-subscriber.Errors():
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscriber round trip")
	}
}

// TestNatsPackagePublishWithHeadersPropagatesTraceparent proves the W3C traceparent header survives the
// PublishWithHeaders -> JetStream -> Subscribe round trip intact. apps/processor-go/main.go:163 extracts
// the consumer span's parent EXCLUSIVELY from the header handed to the subscriber's handler
// (obs.NATSHeaderCarrier(headers)), so if this header were dropped, truncated, or otherwise mangled in
// transit, every consumer span would silently start a new root trace instead of continuing the
// producer's — with no test failure anywhere to say so. Equality (not mere presence) is asserted for
// exactly that reason: a non-nil-but-wrong header would defeat trace propagation just as completely as a
// missing one, and only an exact-value comparison catches it.
func TestNatsPackagePublishWithHeadersPropagatesTraceparent(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher := newNATSPackagePublisher(t, ctx, fixture.url, fixture.subject, nil)
	subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, subscriber.Close())
	})

	// Realistic W3C traceparent: version-traceid-spanid-flags (00-<32 hex>-<16 hex>-01).
	wantTraceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	received := make(chan sharednats.Header, 1)
	require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
		received <- headers
		return nil
	}))

	headers := gonats.Header{}
	headers.Set("traceparent", wantTraceparent)
	require.NoError(t, publisher.PublishWithHeaders(ctx, []byte("with-traceparent"), sharednats.Header(headers)))

	select {
	case gotHeaders := <-received:
		require.NotNil(t, gotHeaders, "subscriber handler must receive the message headers")
		assert.Equal(t, wantTraceparent, gotHeaders.Get("traceparent"),
			"traceparent header must survive the PublishWithHeaders -> JetStream -> Subscribe round trip unchanged")
	case err := <-subscriber.Errors():
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscriber to receive the published message")
	}
}

func TestNatsPackageSubscriberNakRedelivers(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher := newNATSPackagePublisher(t, ctx, fixture.url, fixture.subject, nil)
	subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, subscriber.Close())
	})

	attempts := make(chan int, 2)
	releaseRedelivery := make(chan struct{})
	var attemptCount atomic.Int32
	require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
		assert.Equal(t, []byte("nak-and-redeliver"), data)
		attempt := int(attemptCount.Add(1))
		attempts <- attempt
		if attempt == 1 {
			return errors.New("retry this message")
		}
		<-releaseRedelivery
		return nil
	}))

	require.NoError(t, publisher.Publish(ctx, []byte("nak-and-redeliver")))
	assert.Equal(t, 1, receiveNATSAttempt(t, ctx, attempts))
	assert.Equal(t, 2, receiveNATSAttempt(t, ctx, attempts))

	var info *gonats.ConsumerInfo
	require.Eventually(t, func() bool {
		var err error
		info, err = fixture.js.ConsumerInfo(fixture.stream, fixture.consumer)
		return err == nil && info.NumRedelivered == 1
	}, 2*time.Second, 20*time.Millisecond, "Nak should increment the JetStream redelivery count")
	assert.Equal(t, 1, info.NumRedelivered)
	assert.Equal(t, int32(2), attemptCount.Load())

	close(releaseRedelivery)
	require.Eventually(t, func() bool {
		info, err := fixture.js.ConsumerInfo(fixture.stream, fixture.consumer)
		return err == nil && info.NumAckPending == 0 && info.AckFloor.Consumer == 2
	}, 2*time.Second, 20*time.Millisecond, "the successful redelivery should be acknowledged")
}

func receiveNATSAttempt(t *testing.T, ctx context.Context, attempts <-chan int) int {
	t.Helper()

	select {
	case attempt := <-attempts:
		return attempt
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscriber handler")
		return 0
	}
}

func TestNatsPackageSubscriberAck(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher := newNATSPackagePublisher(t, ctx, fixture.url, fixture.subject, nil)
	subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, subscriber.Close())
	})

	handled := make(chan struct{}, 1)
	require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
		assert.Equal(t, []byte("acknowledge-me"), data)
		handled <- struct{}{}
		return nil
	}))
	require.NoError(t, publisher.Publish(ctx, []byte("acknowledge-me")))

	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscriber handler")
	}

	require.Eventually(t, func() bool {
		info, err := fixture.js.ConsumerInfo(fixture.stream, fixture.consumer)
		return err == nil && info.NumAckPending == 0 && info.AckFloor.Consumer == 1
	}, 2*time.Second, 20*time.Millisecond, "a nil handler error should acknowledge the message")
}

func TestNatsPackageSubscriberStop(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher := newNATSPackagePublisher(t, ctx, fixture.url, fixture.subject, nil)
	subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var handled atomic.Int32
	require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
		handled.Add(1)
		started <- struct{}{}
		<-release
		return nil
	}))

	require.NoError(t, publisher.Publish(ctx, []byte("before-stop")))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscriber handler")
	}

	subscriber.Stop()
	close(release)
	require.Eventually(t, func() bool {
		info, err := fixture.js.ConsumerInfo(fixture.stream, fixture.consumer)
		return err == nil && info.AckFloor.Consumer == 1
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, publisher.Publish(ctx, []byte("after-stop")))
	assert.Never(t, func() bool {
		return handled.Load() > 1
	}, 500*time.Millisecond, 20*time.Millisecond, "Stop should prevent further handler calls")
	assert.Equal(t, int32(1), handled.Load())
}

func TestNatsPackageSubscriberClose(t *testing.T) {
	fixture := newNATSPackageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher := newNATSPackagePublisher(t, ctx, fixture.url, fixture.subject, nil)
	subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var handled atomic.Int32
	require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
		handled.Add(1)
		started <- struct{}{}
		<-release
		return nil
	}))

	require.NoError(t, publisher.Publish(ctx, []byte("before-close")))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscriber handler")
	}

	require.NoError(t, subscriber.Close())
	close(release)

	err := subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create pull subscription")

	require.NoError(t, publisher.Publish(ctx, []byte("after-close")))
	assert.Never(t, func() bool {
		return handled.Load() > 1
	}, 500*time.Millisecond, 20*time.Millisecond, "Close should prevent further handler calls")
	assert.Equal(t, int32(1), handled.Load())
}

func TestNatsPackageErrorPaths(t *testing.T) {
	t.Run("publish honors canceled context", func(t *testing.T) {
		fixture := newNATSPackageFixture(t)
		publisher := newNATSPackagePublisher(t, context.Background(), fixture.url, fixture.subject, nil)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := publisher.Publish(ctx, []byte("will-not-publish"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to publish message")
	})

	t.Run("subscribe rejects an unknown stream", func(t *testing.T) {
		fixture := newNATSPackageFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		fixture.stream += "_MISSING"
		subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)
		defer func() { require.NoError(t, subscriber.Close()) }()

		err := subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error { return nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create pull subscription")
	})

	t.Run("subscribe exits when already canceled", func(t *testing.T) {
		fixture := newNATSPackageFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		subscriber := newNATSPackageSubscriber(t, context.Background(), fixture, fixture.url, nil)
		var handled atomic.Int32
		require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
			handled.Add(1)
			return nil
		}))
		_, err := fixture.js.Publish(fixture.subject, []byte("already-canceled"))
		require.NoError(t, err)
		assert.Never(t, func() bool { return handled.Load() != 0 }, 300*time.Millisecond, 20*time.Millisecond,
			"an already-canceled context should halt before Fetch")
		require.NoError(t, subscriber.Close())
	})

	t.Run("subscribe exits when fetch context is canceled", func(t *testing.T) {
		fixture := newNATSPackageFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, nil)
		var handled atomic.Int32
		require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error {
			handled.Add(1)
			return nil
		}))
		require.Eventually(t, func() bool {
			info, err := fixture.js.ConsumerInfo(fixture.stream, fixture.consumer)
			return err == nil && info.NumWaiting == 1
		}, 2*time.Second, 20*time.Millisecond, "subscriber should be blocked in Fetch")
		cancel()
		_, err := fixture.js.Publish(fixture.subject, []byte("after-cancel"))
		require.NoError(t, err)
		assert.Never(t, func() bool { return handled.Load() != 0 }, 300*time.Millisecond, 20*time.Millisecond,
			"canceling the context should halt the subscriber handler")
		require.NoError(t, subscriber.Close())
	})

	t.Run("subscribe reports non-timeout fetch errors", func(t *testing.T) {
		fixture := newNATSPackageFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		subscriber := newNATSPackageSubscriber(t, ctx, fixture, fixture.url, func(cfg *sharednats.SubscriberConfig) {
			cfg.BatchSize = 0
		})
		require.NoError(t, subscriber.Subscribe(ctx, func(ctx context.Context, data []byte, headers sharednats.Header) error { return nil }))
		select {
		case err := <-subscriber.Errors():
			require.Error(t, err)
			assert.ErrorIs(t, err, gonats.ErrInvalidArg)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for Subscriber.Errors")
		}
		cancel()
		require.NoError(t, subscriber.Close())
	})

	t.Run("invalid NKey seeds are rejected", func(t *testing.T) {
		_, err := sharednats.NewPublisher(context.Background(), sharednats.PublisherConfig{
			URL:      "nats://127.0.0.1:1",
			NKeySeed: "not-a-seed",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create NKEY option")

		_, err = sharednats.NewSubscriber(context.Background(), sharednats.SubscriberConfig{
			URL:      "nats://127.0.0.1:1",
			NKeySeed: "not-a-seed",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create NKEY option")
	})

	t.Run("valid NKey seed files reach the connection path", func(t *testing.T) {
		keyPair, err := nkeys.CreateUser()
		require.NoError(t, err)
		seed, err := keyPair.Seed()
		require.NoError(t, err)
		seedFile := filepath.Join(t.TempDir(), "user.nk")
		require.NoError(t, os.WriteFile(seedFile, seed, 0o600))

		_, err = sharednats.NewPublisher(context.Background(), sharednats.PublisherConfig{
			URL:      "nats://127.0.0.1:1",
			NKeySeed: seedFile,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to connect to NATS")

		_, err = sharednats.NewSubscriber(context.Background(), sharednats.SubscriberConfig{
			URL:      "nats://127.0.0.1:1",
			NKeySeed: seedFile,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to connect to NATS")
	})

	t.Run("invalid TLS files are rejected", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.pem")
		_, err := sharednats.NewPublisher(context.Background(), sharednats.PublisherConfig{
			URL:         "nats://127.0.0.1:1",
			TLSCertFile: missing,
			TLSKeyFile:  missing,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to build TLS config")

		_, err = sharednats.NewSubscriber(context.Background(), sharednats.SubscriberConfig{
			URL:         "nats://127.0.0.1:1",
			TLSCertFile: missing,
			TLSKeyFile:  missing,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to build TLS config")
	})

	t.Run("invalid CA files are rejected", func(t *testing.T) {
		certFile, keyFile, _ := writeNATSTestCertificate(t)

		_, err := sharednats.NewPublisher(context.Background(), sharednats.PublisherConfig{
			URL:         "nats://127.0.0.1:1",
			TLSCertFile: certFile,
			TLSKeyFile:  keyFile,
			TLSCAFile:   filepath.Join(t.TempDir(), "missing-ca.pem"),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read CA cert")

		invalidCA := filepath.Join(t.TempDir(), "invalid-ca.pem")
		require.NoError(t, os.WriteFile(invalidCA, []byte("not a PEM certificate"), 0o600))
		_, err = sharednats.NewPublisher(context.Background(), sharednats.PublisherConfig{
			URL:         "nats://127.0.0.1:1",
			TLSCertFile: certFile,
			TLSKeyFile:  keyFile,
			TLSCAFile:   invalidCA,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse CA cert")
	})
}

func writeNATSTestCertificate(t *testing.T) (certFile, keyFile string, certificate tls.Certificate) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "NATS U12 integration test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:         true,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile = filepath.Join(dir, "nats-test-cert.pem")
	keyFile = filepath.Join(dir, "nats-test-key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	certificate, err = tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return certFile, keyFile, certificate
}

func startNATSTLSProxy(t *testing.T, upstreamURL string, certificate tls.Certificate) string {
	t.Helper()

	parsed, err := url.Parse(upstreamURL)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Host)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}

	var connections sync.WaitGroup
	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer downstream.Close()

				upstream, err := net.DialTimeout("tcp", parsed.Host, 2*time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()

				// NATS advertises TLS in its initial plaintext INFO line, then the
				// peers upgrade the existing socket. Preserve the real INFO payload.
				upstreamReader := bufio.NewReader(upstream)
				infoLine, err := upstreamReader.ReadString('\n')
				if err != nil || len(infoLine) < len("INFO ") {
					return
				}
				var info map[string]interface{}
				if json.Unmarshal([]byte(infoLine[len("INFO "):]), &info) != nil {
					return
				}
				info["tls_available"] = true
				encodedInfo, err := json.Marshal(info)
				if err != nil {
					return
				}
				if _, err := fmt.Fprintf(downstream, "INFO %s\r\n", encodedInfo); err != nil {
					return
				}

				tlsDownstream := tls.Server(downstream, serverTLS)
				if err := tlsDownstream.Handshake(); err != nil {
					return
				}
				copyDone := make(chan struct{})
				go func() {
					_, _ = io.Copy(upstream, tlsDownstream)
					_ = upstream.Close()
					close(copyDone)
				}()
				_, _ = io.Copy(tlsDownstream, upstreamReader)
				<-copyDone
			}()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		connections.Wait()
	})
	return "tls://" + listener.Addr().String()
}
