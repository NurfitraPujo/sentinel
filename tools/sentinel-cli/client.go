package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Exit codes, per the CLI contract (README.md documents these for operators / other agents).
const (
	ExitOK         = 0
	ExitNetwork    = 1 // network failure, or a 5xx / otherwise-unmapped server error
	ExitUsage      = 2
	ExitAuth       = 3 // 401/403
	ExitNotFound   = 4 // 404
	ExitConflict   = 5 // 409
	ExitValidation = 6 // 400/422
)

// exitCodeForStatus maps an HTTP status code from the server onto this CLI's process exit code.
func exitCodeForStatus(status int) int {
	switch {
	case status >= 200 && status < 300:
		return ExitOK
	case status == 401 || status == 403:
		return ExitAuth
	case status == 404:
		return ExitNotFound
	case status == 409:
		return ExitConflict
	case status == 400 || status == 422:
		return ExitValidation
	default:
		return ExitNetwork
	}
}

// Client is a thin HTTP client for the /api/agent/* contract: baseURL + bearer key, nothing else.
// It never interprets response bodies beyond extracting an error message for non-2xx responses —
// every command decides for itself how to parse/print the json.RawMessage it gets back.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func NewClient(baseURL, key string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Result is what every Client method returns: the raw HTTP status and body. Commands decide how
// to interpret/print it; Client never does.
type Result struct {
	Status int
	Body   json.RawMessage
}

// errorMessage attempts to pull a human-readable message out of a non-2xx JSON error body. The
// agent routes use both {"error": "..."} (manual-validation routes) and {"message": "..."}
// (SvelteKit's error() helper) shapes — see apps/dashboard-web/src/routes/api/agent/**/+server.ts.
func errorMessage(body json.RawMessage) string {
	var withError struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &withError); err == nil && withError.Error != "" {
		return withError.Error
	}
	var withMessage struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &withMessage); err == nil && withMessage.Message != "" {
		return withMessage.Message
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "(no error body)"
	}
	return trimmed
}

// Do performs one JSON request against path (e.g. "/api/agent/issues"), with optional query
// params and an optional JSON-encodable body (nil for none). It always sets the Bearer header.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body interface{}) (*Result, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", u, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	return &Result{Status: resp.StatusCode, Body: data}, nil
}

// UploadFile performs a multipart/form-data POST with the given file under form field "file"
// (apps/dashboard-web/src/lib/server/upload-core.ts reads exactly that field, plus an optional
// "filename" field it falls back to file.name if omitted).
func (c *Client) UploadFile(ctx context.Context, path, filePath string, fileReader io.Reader, filename string) (*Result, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("creating form file: %w", err)
	}
	if _, err := io.Copy(part, fileReader); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filePath, err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, &buf)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return &Result{Status: resp.StatusCode, Body: data}, nil
}
