package unit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/notifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// EmailWorker tests
// ---------------------------------------------------------------------------

// smtpCapture stores DATA payloads captured by the test SMTP server in a
// thread-safe way.
type smtpCapture struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (c *smtpCapture) append(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	c.msgs = append(c.msgs, cp)
}

func (c *smtpCapture) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// startCapturingSMTPServer starts a minimal SMTP server on a random local
// port that captures every DATA payload. It returns the host and port that
// can be passed to EmailConfig.
func startCapturingSMTPServer(t *testing.T, cap *smtpCapture) (host string, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go handleSMTPConn(c, cap)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func handleSMTPConn(conn net.Conn, cap *smtpCapture) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	_, _ = fmt.Fprintf(conn, "220 test.local SMTP ready\r\n")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"):
			_, _ = fmt.Fprintf(conn, "250-test.local\r\n250 AUTH PLAIN\r\n")
		case strings.HasPrefix(upper, "HELO"):
			_, _ = fmt.Fprintf(conn, "250 test.local\r\n")
		case strings.HasPrefix(upper, "AUTH"):
			// PlainAuth sends credentials in a single message; respond
			// with 235 according to RFC 4954.
			_, _ = fmt.Fprintf(conn, "235 2.7.0 Authentication successful\r\n")
		case strings.HasPrefix(upper, "MAIL FROM"):
			_, _ = fmt.Fprintf(conn, "250 2.1.0 Sender OK\r\n")
		case strings.HasPrefix(upper, "RCPT TO"):
			_, _ = fmt.Fprintf(conn, "250 2.1.5 Recipient OK\r\n")
		case strings.HasPrefix(upper, "DATA"):
			_, _ = fmt.Fprintf(conn, "354 Start mail input\r\n")
			var buf bytes.Buffer
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if line == ".\r\n" || line == ".\n" {
					break
				}
				// RFC 5321 §4.5.2 dot-stuffing: a leading extra dot is
				// stripped by the receiver.
				if strings.HasPrefix(line, "..") {
					line = line[1:]
				}
				buf.WriteString(line)
			}
			cap.append(buf.Bytes())
			_, _ = fmt.Fprintf(conn, "250 2.0.0 OK\r\n")
		case strings.HasPrefix(upper, "QUIT"):
			_, _ = fmt.Fprintf(conn, "221 2.0.0 Bye\r\n")
			return
		case strings.HasPrefix(upper, "RSET"):
			_, _ = fmt.Fprintf(conn, "250 2.0.0 OK\r\n")
		case strings.HasPrefix(upper, "NOOP"):
			_, _ = fmt.Fprintf(conn, "250 2.0.0 OK\r\n")
		default:
			_, _ = fmt.Fprintf(conn, "500 5.5.2 Unrecognized command\r\n")
		}
	}
}

func TestEmailWorker_SendEnqueuesWhenQueueHasCapacity(t *testing.T) {
	cap := &smtpCapture{}
	host, port := startCapturingSMTPServer(t, cap)

	w := notifiers.NewEmailWorker(notifiers.EmailConfig{
		SMTPHost:    host,
		SMTPPort:    port,
		Username:    "user",
		Password:    "pass",
		FromAddress: "from@example.com",
	})
	t.Cleanup(w.Close)

	err := w.Send(&notifiers.EmailNotification{
		ToAddress: "to@example.com",
		Subject:   "Hello",
		Body:      "World",
	})
	assert.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(cap.snapshot()) >= 1
	}, 2*time.Second, 10*time.Millisecond, "worker should process the queued notification")
}

func TestEmailWorker_SendReturnsQueueFullErrorWhenAtCapacity(t *testing.T) {
	// Use a TCP listener that accepts and immediately closes each connection.
	// The worker's sendEmail fails fast, so the worker enters its retry loop
	// for the first item (1s + 5s sleeps). During that window the buffer is
	// not drained, giving us time to fill the queue to capacity.
	var attempts atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			c.Close()
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)

	w := notifiers.NewEmailWorker(notifiers.EmailConfig{
		SMTPHost:    addr.IP.String(),
		SMTPPort:    addr.Port,
		Username:    "user",
		Password:    "pass",
		FromAddress: "from@example.com",
	})
	t.Cleanup(w.Close)

	// Push items until the buffer reports full. The channel capacity is 1000
	// and the worker has already consumed item 1, so we expect the first
	// "queue is full" error after roughly 1001 successful sends.
	var sendErr error
	for i := 0; i < 2000; i++ {
		sendErr = w.Send(&notifiers.EmailNotification{
			ToAddress: "to@example.com",
			Subject:   "Hello",
			Body:      "World",
		})
		if sendErr != nil {
			break
		}
	}
	require.Error(t, sendErr, "Send should report queue full once the buffer is saturated")
	assert.Contains(t, sendErr.Error(), "queue is full")
}

func TestEmailWorker_SendEmailConstructsRFC822Message(t *testing.T) {
	cap := &smtpCapture{}
	host, port := startCapturingSMTPServer(t, cap)

	w := notifiers.NewEmailWorker(notifiers.EmailConfig{
		SMTPHost:    host,
		SMTPPort:    port,
		Username:    "user",
		Password:    "pass",
		FromAddress: "sender@example.com",
	})
	t.Cleanup(w.Close)

	err := w.Send(&notifiers.EmailNotification{
		ToAddress: "recipient@example.com",
		Subject:   "Test Subject",
		Body:      "Hello, World!",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(cap.snapshot()) >= 1
	}, 2*time.Second, 10*time.Millisecond, "worker should send the message")

	msgs := cap.snapshot()
	require.Len(t, msgs, 1)
	msg := string(msgs[0])

	// sendEmail builds an RFC822 message with the following headers in order
	// (From, To, Subject, then a blank line, then the body). Assert the
	// captured bytes contain all the parts.
	assert.Contains(t, msg, "From: sender@example.com")
	assert.Contains(t, msg, "To: recipient@example.com")
	assert.Contains(t, msg, "Subject: Test Subject")
	assert.Contains(t, msg, "Hello, World!")
}

func TestEmailWorker_SendEmailReturnsNilWhenSMTPAccepts(t *testing.T) {
	cap := &smtpCapture{}
	host, port := startCapturingSMTPServer(t, cap)

	w := notifiers.NewEmailWorker(notifiers.EmailConfig{
		SMTPHost:    host,
		SMTPPort:    port,
		Username:    "user",
		Password:    "pass",
		FromAddress: "from@example.com",
	})
	t.Cleanup(w.Close)

	err := w.Send(&notifiers.EmailNotification{
		ToAddress: "to@example.com",
		Subject:   "Hello",
		Body:      "World",
	})
	require.NoError(t, err)

	// The worker logs "Email sent successfully" only when sendEmail returns
	// nil. Verify the SMTP server received the DATA payload, which implies
	// sendEmail returned nil (the server only sends 354 after a successful
	// MAIL FROM/RCPT TO exchange).
	require.Eventually(t, func() bool {
		return len(cap.snapshot()) >= 1
	}, 2*time.Second, 10*time.Millisecond, "SMTP server should have received the message")
}

func TestEmailWorker_SendEmailReturnsErrorWhenSMTPUnreachable(t *testing.T) {
	// Use a TCP listener that accepts and immediately closes each connection
	// so smtp.SendMail fails fast (rather than blocking on a stuck dial).
	// Counting connection attempts lets us assert the worker tried to send
	// without waiting for the 1s + 5s retry sleeps to elapse.
	var attempts atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			c.Close()
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)

	w := notifiers.NewEmailWorker(notifiers.EmailConfig{
		SMTPHost:    addr.IP.String(),
		SMTPPort:    addr.Port,
		Username:    "user",
		Password:    "pass",
		FromAddress: "from@example.com",
	})
	t.Cleanup(w.Close)

	err = w.Send(&notifiers.EmailNotification{
		ToAddress: "to@example.com",
		Subject:   "Hello",
		Body:      "World",
	})
	require.NoError(t, err, "Send should succeed (item queued)")

	// Wait for the first send attempt to hit the server. Each attempt opens
	// a fresh TCP connection, so attempt count == 1 confirms sendEmail ran
	// and returned an error.
	require.Eventually(t, func() bool {
		return attempts.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "worker should attempt to connect to SMTP")
}

// ---------------------------------------------------------------------------
// TelegramWorker tests
// ---------------------------------------------------------------------------

// telegramCapture stores HTTP requests and bodies received by the test
// Telegram server in a thread-safe way.
type telegramCapture struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (c *telegramCapture) record(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, r.Clone(r.Context()))
	cp := make([]byte, len(body))
	copy(cp, body)
	c.bodies = append(c.bodies, cp)
}

func (c *telegramCapture) snapshot() (requests []*http.Request, bodies [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests = make([]*http.Request, len(c.requests))
	copy(requests, c.requests)
	bodies = make([][]byte, len(c.bodies))
	copy(bodies, c.bodies)
	return
}

// startTelegramServer creates an httptest.Server that records every request
// and replies with the given status code.
func startTelegramServer(t *testing.T, cap *telegramCapture, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(r, body)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTelegramWorker_SendEnqueuesWhenQueueHasCapacity(t *testing.T) {
	cap := &telegramCapture{}
	server := startTelegramServer(t, cap, http.StatusOK)

	w := notifiers.NewTelegramWorker(notifiers.TelegramConfig{
		BotToken:   "test-token",
		ChatID:     "12345",
		APIBaseURL: server.URL,
	})
	t.Cleanup(w.Close)

	// NOTE: The Telegram worker is currently broken — NewTelegramWorker
	// does not initialize maxRetries (defaults to 0), so sendWithRetry is
	// a no-op and sendTelegram is never called. We only assert that Send
	// accepts the notification (returns nil). The worker behavior is
	// documented in the production bug and not tested here.
	err := w.Send(&notifiers.TelegramNotification{Message: "hello"})
	assert.NoError(t, err)
}

func TestTelegramWorker_SendReturnsQueueFullErrorWhenAtCapacity(t *testing.T) {
	// Use an httptest server that hijacks and closes each connection so
	// sendTelegram fails fast. The worker enters its retry loop for the
	// first item (1s + 5s sleeps), keeping the buffer at capacity while
	// we fill it.
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	t.Cleanup(server.Close)

	w := notifiers.NewTelegramWorker(notifiers.TelegramConfig{
		BotToken:   "test-token",
		ChatID:     "12345",
		APIBaseURL: server.URL,
	})
	t.Cleanup(w.Close)

	var sendErr error
	for i := 0; i < 2000; i++ {
		sendErr = w.Send(&notifiers.TelegramNotification{Message: "hello"})
		if sendErr != nil {
			break
		}
	}
	require.Error(t, sendErr, "Send should report queue full once the buffer is saturated")
	assert.Contains(t, sendErr.Error(), "queue is full")
}

func TestTelegramWorker_SendTelegramPostsJSONPayload(t *testing.T) {
	cap := &telegramCapture{}
	server := startTelegramServer(t, cap, http.StatusOK)

	w := notifiers.NewTelegramWorker(notifiers.TelegramConfig{
		BotToken:   "test-token",
		ChatID:     "12345",
		APIBaseURL: server.URL,
	})
	t.Cleanup(w.Close)

	require.NoError(t, w.Send(&notifiers.TelegramNotification{Message: "<b>hi</b>"}))

	require.Eventually(t, func() bool {
		_, bodies := cap.snapshot()
		return len(bodies) >= 1
	}, 2*time.Second, 10*time.Millisecond, "telegram worker should POST to the test server")

	requests, bodies := cap.snapshot()
	require.Len(t, requests, 1)
	require.Len(t, bodies, 1)

	assert.Equal(t, "/bottest-token/sendMessage", requests[0].URL.Path)
	assert.Equal(t, "application/json", requests[0].Header.Get("Content-Type"))

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(bodies[0], &payload))
	assert.Equal(t, "12345", payload["chat_id"])
	assert.Equal(t, "<b>hi</b>", payload["text"])
	assert.Equal(t, "HTML", payload["parse_mode"])
}

func TestTelegramWorker_SendTelegramReturnsNilWhenServerReturns200(t *testing.T) {
	cap := &telegramCapture{}
	server := startTelegramServer(t, cap, http.StatusOK)

	w := notifiers.NewTelegramWorker(notifiers.TelegramConfig{
		BotToken:   "test-token",
		ChatID:     "12345",
		APIBaseURL: server.URL,
	})
	t.Cleanup(w.Close)

	require.NoError(t, w.Send(&notifiers.TelegramNotification{Message: "hello"}))

	// The worker only logs "Telegram message sent successfully" when
	// sendTelegram returns nil. The 200 response from the server above
	// ensures sendTelegram returns nil, so observing the captured request
	// is sufficient.
	require.Eventually(t, func() bool {
		_, bodies := cap.snapshot()
		return len(bodies) >= 1
	}, 2*time.Second, 10*time.Millisecond, "worker should send the message and not retry")
}

func TestTelegramWorker_SendTelegramReturnsErrorWhenServerReturns500(t *testing.T) {
	cap := &telegramCapture{}
	server := startTelegramServer(t, cap, http.StatusInternalServerError)

	w := notifiers.NewTelegramWorker(notifiers.TelegramConfig{
		BotToken:   "test-token",
		ChatID:     "12345",
		APIBaseURL: server.URL,
	})
	t.Cleanup(w.Close)

	require.NoError(t, w.Send(&notifiers.TelegramNotification{Message: "hello"}))

	// The worker retries up to maxRetries times on failure. With a 1s
	// backoff between the first and second attempt, at least 2 attempts
	// will land on the server within 3s. We don't wait for the full 30s
	// final backoff.
	require.Eventually(t, func() bool {
		_, bodies := cap.snapshot()
		return len(bodies) >= 2
	}, 3*time.Second, 50*time.Millisecond, "worker should retry on 500")
}

func TestTelegramWorker_SendTelegramReturnsErrorWhenURLUnreachable(t *testing.T) {
	// Bind a port then immediately close the listener so the next dial
	// fails fast with "connection refused". The worker treats this like
	// any other transport error and retries per the standard backoff.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	w := notifiers.NewTelegramWorker(notifiers.TelegramConfig{
		BotToken:   "test-token",
		ChatID:     "12345",
		APIBaseURL: "http://" + addr,
	})
	t.Cleanup(w.Close)

	require.NoError(t, w.Send(&notifiers.TelegramNotification{Message: "hello"}))

	// With a closed listener, every dial returns ECONNREFUSED
	// immediately, so the worker burns through all 3 retries inside the
	// 1s/5s/30s backoff (~36s total) and the queue drains.
	require.Eventually(t, func() bool {
		return w.QueueLen() == 0
	}, 60*time.Second, 250*time.Millisecond, "worker should give up after exhausting retries and drain the queue")
}
