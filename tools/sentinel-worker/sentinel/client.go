// Package sentinel is the HTTP client for the Sentinel /api/agent/* contract used by
// sentinel-worker (see docs/plans/AGENT_WORKER_PLAN.md §1 "sentinel/client.go").
//
// This file starts as a deliberate copy of tools/sentinel-cli/client.go (168 lines, already the
// whole reusable core) — the CLI is `package main` throughout so it cannot be imported directly.
// The plan (§1) treats this duplication as cheaper than extracting a shared module, which would
// either drag the CLI into go.work or create a 4th published module dependency. Revisit only if a
// third consumer appears.
package sentinel

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

// Exit-code-equivalent status classes, mirrored from tools/sentinel-cli/client.go's
// exitCodeForStatus so sentinel/retry.go's envelope classification stays aligned with the CLI's
// documented contract.
const (
	ClassOK         = 0
	ClassNetwork    = 1 // network failure, or a 5xx / otherwise-unmapped server error
	ClassAuth       = 3 // 401/403
	ClassNotFound   = 4 // 404
	ClassConflict   = 5 // 409
	ClassValidation = 6 // 400/422
	ClassRateLimit  = 7 // 429 (plan §2.4 "Rate limited" row; the CLI has no equivalent exit code)
)

// ClassifyStatus maps an HTTP status code from the server onto a ClassXxx constant above. It is
// the envelope half of the two-level classification described in plan §2.4/§2.3 (the other half,
// per-op batch results[i].status, is classified the same way by the caller).
func ClassifyStatus(status int) int {
	switch {
	case status >= 200 && status < 300:
		return ClassOK
	case status == 401 || status == 403:
		return ClassAuth
	case status == 404:
		return ClassNotFound
	case status == 409:
		return ClassConflict
	case status == 400 || status == 422:
		return ClassValidation
	case status == 429:
		return ClassRateLimit
	default:
		return ClassNetwork
	}
}

// Client is a thin HTTP client for the /api/agent/* contract: baseURL + bearer key, nothing else.
// It never interprets response bodies beyond extracting an error message for non-2xx responses —
// callers decide for themselves how to parse/print the json.RawMessage they get back.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client

	// OnAuthStatus, when non-nil, is called after every Do() envelope response with true for a 2xx
	// response and false for a 401 -- the seam health.Status.SetAuthValid plugs into so /readyz's
	// "auth valid" leg (plan §7) reflects reality: a 401 flips it not-ready, and a subsequent
	// successful call clears it again (e.g. after keyguard rotates the key, plan §2.5). Other
	// statuses (403, 404, 5xx, ...) are left alone -- they are not evidence about THIS credential's
	// validity one way or the other.
	OnAuthStatus func(ok bool)
}

// NewClient builds a Client. The key is read fresh from Client.Key on every call, not captured at
// construction time — this lets keyguard (plan §2.5) swap the in-memory key after a rotation
// without callers needing to rebuild the client.
func NewClient(baseURL, key string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Result is what every Client method returns: the raw HTTP status, body, and response headers.
// Callers decide how to interpret it; Client never does. Header is populated so retry.go's
// WaitRateLimit (which needs the Retry-After header) is reachable from every endpoint wrapper, not
// just from a caller that drops down to Do() directly.
type Result struct {
	Status int
	Body   json.RawMessage
	Header http.Header
}

// ErrorMessage attempts to pull a human-readable message out of a non-2xx JSON error body. The
// agent routes use both {"error": "..."} (manual-validation routes) and {"message": "..."}
// (SvelteKit's error() helper) shapes — see apps/dashboard-web/src/routes/api/agent/**/+server.ts.
func ErrorMessage(body json.RawMessage) string {
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

// StatusError wraps a non-2xx Result so retry-aware callers (e.g. loop.PollLoop.Run, plan §2.4)
// can classify the failure (via ClassifyEnvelope/WaitRateLimit) without every endpoint wrapper
// needing its own bespoke error type. Endpoint wrappers that return a bare `error` for non-2xx
// responses (loop package adapters) should wrap it in a *StatusError so the status/header survive
// past the `error` interface.
type StatusError struct {
	Status int
	Header http.Header
	Body   json.RawMessage
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%d %s", e.Status, ErrorMessage(e.Body))
}

// MaxRetryAfter bounds the Retry-After duration RetryAfter will ever honor (validator finding: "no
// upper bound at all ... a server (or a misbehaving proxy) answering `Retry-After: 86400` parks the
// poll loop for a day with no way to shut it down"). A hostile/broken header can delay a poll cycle,
// but never long enough to defeat a graceful SIGTERM drain for a whole workday.
const MaxRetryAfter = 5 * time.Minute

// RetryAfter reads a Retry-After header as seconds (plan §2.4's "Rate limited" row: "sleep exactly
// Retry-After, default 60"). A missing or unparsable header returns the given default. The result
// is capped at MaxRetryAfter regardless of what the header claims.
func RetryAfter(h http.Header, def time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	d := def
	if v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		}
	}
	if d > MaxRetryAfter {
		d = MaxRetryAfter
	}
	return d
}

// Do performs one JSON request against path (e.g. "/api/agent/issues"), with optional query
// params and an optional JSON-encodable body (nil for none). It always sets the Bearer header
// using the Client's CURRENT Key field, so a keyguard rotation swap takes effect on the next call.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body interface{}) (*Result, *http.Response, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("request to %s failed: %w", u, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp, fmt.Errorf("reading response body: %w", err)
	}

	if c.OnAuthStatus != nil {
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			c.OnAuthStatus(false)
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			c.OnAuthStatus(true)
		}
	}

	return &Result{Status: resp.StatusCode, Body: data, Header: resp.Header}, resp, nil
}

// --- endpoint methods used by N8a (plan §1 "Endpoints used in N8a") ------------------------------
//
// These are thin wrappers over Do() that know each endpoint's path/query/body shape but still
// leave response-body interpretation to the caller (Result.Body is always the raw JSON) — same
// division of responsibility as Do() itself. Idempotency-key plumbing (C4): the caller derives the
// key ("<jobId>:<opIndex>", plan §2.2) and passes it in; these methods just carry it into the
// body's "idempotency_key" field. An empty key omits the field entirely.

// GetEvents calls GET /api/agent/events?after&limit&type&project (plan §2.1 poll loop).
func (c *Client) GetEvents(ctx context.Context, after int64, limit int, typ, project string) (*Result, error) {
	q := url.Values{}
	if after > 0 {
		q.Set("after", fmt.Sprintf("%d", after))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if typ != "" {
		q.Set("type", typ)
	}
	if project != "" {
		q.Set("project", project)
	}
	res, _, err := c.Do(ctx, "GET", "/api/agent/events", q, nil)
	return res, err
}

// GetIssue calls GET /api/agent/issues/:id.
func (c *Client) GetIssue(ctx context.Context, issueID string) (*Result, error) {
	res, _, err := c.Do(ctx, "GET", "/api/agent/issues/"+url.PathEscape(issueID), nil, nil)
	return res, err
}

// IssuesListOptions is the query surface of GET /api/agent/issues used by the bootstrap sweep
// (plan §2.1) and the sweep's claimed=me seeding pass: since/sort/limit/cursor/claimed/waiting.
type IssuesListOptions struct {
	Since   string // RFC3339; plan §2.1's "since <now - WORKER_BACKFILL_HOURS>"
	Sort    string // e.g. "firstSeen"
	Limit   int
	Cursor  string // keyset cursor
	Claimed string // "me" (C12) — empty omits the filter
	Waiting bool   // waiting-on-reporter filter
}

// ListIssues calls GET /api/agent/issues with the given filter/pagination options.
func (c *Client) ListIssues(ctx context.Context, opts IssuesListOptions) (*Result, error) {
	q := url.Values{}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Claimed != "" {
		q.Set("claimed", opts.Claimed)
	}
	if opts.Waiting {
		q.Set("waiting", "true")
	}
	res, _, err := c.Do(ctx, "GET", "/api/agent/issues", q, nil)
	return res, err
}

// ListProjects calls GET /api/agent/projects (C15's agentSettings live here).
func (c *Client) ListProjects(ctx context.Context) (*Result, error) {
	res, _, err := c.Do(ctx, "GET", "/api/agent/projects", nil, nil)
	return res, err
}

// GetSelf calls GET /api/agent/self (C13's key.createdAt lives here; keyguard's age fallback).
func (c *Client) GetSelf(ctx context.Context) (*Result, error) {
	res, _, err := c.Do(ctx, "GET", "/api/agent/self", nil, nil)
	return res, err
}

// ClaimConflict is the 409 body shape for a foreign-claimant conflict (C1: "Foreign claimant is
// still 409 with {claimedBy, claimedAt}").
type ClaimConflict struct {
	ClaimedBy string `json:"claimedBy"`
	ClaimedAt string `json:"claimedAt"`
}

// ClaimResult is the 200 body shape of a successful (including self-reclaim) claim (C1: "200
// {success, issue, alreadyClaimed: true}" on self-reclaim; the flag is absent on a fresh claim).
type ClaimResult struct {
	Success        bool            `json:"success"`
	Issue          json.RawMessage `json:"issue"`
	AlreadyClaimed bool            `json:"alreadyClaimed"`
}

// ClaimIssue calls POST /api/agent/issues/:id/claim. Per C1, both a fresh claim and a self-reclaim
// return 200 (ClaimResult, AlreadyClaimed distinguishing the two) — the worker's ensure-claimed
// rule never special-cases a self-409, because that path no longer exists. A 409 means a FOREIGN
// claimant; its body is parsed into ClaimConflict for the caller.
func (c *Client) ClaimIssue(ctx context.Context, issueID string) (*Result, *ClaimConflict, error) {
	res, _, err := c.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(issueID)+"/claim", nil, map[string]interface{}{})
	if err != nil {
		return nil, nil, err
	}
	if res.Status == http.StatusConflict {
		var conflict ClaimConflict
		if jerr := json.Unmarshal(res.Body, &conflict); jerr == nil {
			return res, &conflict, nil
		}
	}
	return res, nil, nil
}

// BatchOperation is one entry in a POST /api/agent/batch request (plan §2.3). The server reads
// each op's fields as `{op, issueId, params?}` (apps/dashboard-web/src/routes/api/agent/batch/+server.ts)
// and then runs `params` through the SAME single-route handler as the non-batch endpoint
// (agent-ops.ts) — so idempotency_key MUST live INSIDE params, not as a sibling of it, matching
// the single-route body shape exactly (SENTINEL_AGENT_GUIDE.md: "Each op's `params` shape is
// exactly the JSON body its single-route equivalent takes"). NewBatchOperation below merges the
// key into Params so callers cannot accidentally place it at the wrong level (C4).
type BatchOperation struct {
	Op      string      `json:"op"`
	IssueID string      `json:"issueId"`
	Params  interface{} `json:"params,omitempty"`
}

// NewBatchOperation builds a BatchOperation, merging idempotencyKey (C4: "<jobId>:<opIndex>",
// derived by the caller) into params as the "idempotency_key" field when non-empty. params may be
// nil (ops that take no body, e.g. issues.claim) — a non-empty key still produces a params map in
// that case. params, if given, must be a map[string]interface{} (or nil) so the key can be merged
// in; other Params shapes should be built by the caller directly if a key is not needed.
func NewBatchOperation(op, issueID string, params map[string]interface{}, idempotencyKey string) BatchOperation {
	if idempotencyKey == "" {
		if params == nil {
			return BatchOperation{Op: op, IssueID: issueID}
		}
		return BatchOperation{Op: op, IssueID: issueID, Params: params}
	}
	merged := map[string]interface{}{}
	for k, v := range params {
		merged[k] = v
	}
	merged["idempotency_key"] = idempotencyKey
	return BatchOperation{Op: op, IssueID: issueID, Params: merged}
}

// BatchRequest is the POST /api/agent/batch body: stopOnError:false per plan §2.3 (the worker
// always uses partial-completion semantics, never the CLI's stop-on-first-error default).
type BatchRequest struct {
	Operations  []BatchOperation `json:"operations"`
	StopOnError bool             `json:"stopOnError"`
}

// PostBatch calls POST /api/agent/batch.
func (c *Client) PostBatch(ctx context.Context, req BatchRequest) (*Result, error) {
	res, _, err := c.Do(ctx, "POST", "/api/agent/batch", nil, req)
	return res, err
}

// PostComment calls POST /api/agent/issues/:id/comments with an idempotency key (C4). The server
// requires the comment text under "body_md" (apps/dashboard-web/src/lib/server/agent-ops.ts:222-223),
// not "body".
func (c *Client) PostComment(ctx context.Context, issueID, bodyMD, idempotencyKey string) (*Result, error) {
	payload := map[string]interface{}{"body_md": bodyMD}
	if idempotencyKey != "" {
		payload["idempotency_key"] = idempotencyKey
	}
	res, _, err := c.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(issueID)+"/comments", nil, payload)
	return res, err
}

// PostQuestion calls POST /api/agent/issues/:id/questions with an idempotency key (C4) — blocking
// questions are only safe to retry when a key is sent (plan §2.2).
func (c *Client) PostQuestion(ctx context.Context, issueID string, params map[string]interface{}, idempotencyKey string) (*Result, error) {
	payload := map[string]interface{}{}
	for k, v := range params {
		payload[k] = v
	}
	if idempotencyKey != "" {
		payload["idempotency_key"] = idempotencyKey
	}
	res, _, err := c.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(issueID)+"/questions", nil, payload)
	return res, err
}

// PostProgress calls POST /api/agent/issues/:id/progress with an idempotency key (C4). The server
// requires the note under "message_md" (apps/dashboard-web/src/lib/server/agent-ops.ts:411-412),
// not "note".
func (c *Client) PostProgress(ctx context.Context, issueID, messageMD, idempotencyKey string) (*Result, error) {
	payload := map[string]interface{}{"message_md": messageMD}
	if idempotencyKey != "" {
		payload["idempotency_key"] = idempotencyKey
	}
	res, _, err := c.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(issueID)+"/progress", nil, payload)
	return res, err
}

// UploadFile performs a multipart/form-data POST with the given file under form field "file"
// (apps/dashboard-web/src/lib/server/upload-core.ts reads exactly that field, plus an optional
// "filename" field it falls back to file.name if omitted).
func (c *Client) UploadFile(ctx context.Context, path string, fileReader io.Reader, filename string) (*Result, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("creating form file: %w", err)
	}
	if _, err := io.Copy(part, fileReader); err != nil {
		return nil, fmt.Errorf("reading upload body: %w", err)
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
	return &Result{Status: resp.StatusCode, Body: data, Header: resp.Header}, nil
}
