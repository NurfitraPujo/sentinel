package gitprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const defaultBitbucketAPIBase = "https://api.bitbucket.org"

// BitbucketProvider is a Provider backed by Bitbucket Cloud 2.0 (plan §4.5: "Server/DC out of
// scope v1 — different API family"). Auth is EITHER an access token (Token) OR a
// username+app-password pair (Username/AppPassword) — Token takes priority when both are set.
type BitbucketProvider struct {
	Token       string
	Username    string
	AppPassword string
	// APIBase overrides the default https://api.bitbucket.org — configurable for tests.
	APIBase string
	HTTP    *http.Client
}

// NewGitHubProvider-style constructors are intentionally omitted here: the two valid auth shapes
// (token vs username+app-password) make a single positional constructor ambiguous — callers
// build the struct literal directly.

func (p *BitbucketProvider) apiBase() string {
	if p.APIBase != "" {
		return p.APIBase
	}
	return defaultBitbucketAPIBase
}

func (p *BitbucketProvider) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

// Auth returns the token credential when Token is set, else the username+app-password basic
// credential.
func (p *BitbucketProvider) Auth() GitCredential {
	if p.Token != "" {
		return BitbucketTokenCredential(p.Token)
	}
	return BitbucketBasicCredential(p.Username, p.AppPassword)
}

func (p *BitbucketProvider) setAuthHeader(req *http.Request) {
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
		return
	}
	req.SetBasicAuth(p.Username, p.AppPassword)
}

type bitbucketBranchRef struct {
	Name string `json:"name"`
}

type bitbucketCreatePRRequest struct {
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Source      bitbucketPRBranch `json:"source"`
	Destination bitbucketPRBranch `json:"destination"`
}

type bitbucketPRBranch struct {
	Branch bitbucketBranchRef `json:"branch"`
}

type bitbucketPRResponse struct {
	ID    int    `json:"id"`
	State string `json:"state"` // OPEN | MERGED | DECLINED | SUPERSEDED
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

// CreatePR opens a pull request via POST /2.0/repositories/{workspace}/{repo_slug}/pullrequests.
// RepoRef.Owner is the Bitbucket workspace, RepoRef.Repo is the repo slug.
func (p *BitbucketProvider) secrets() []string {
	return []string{p.Token, p.AppPassword}
}

func (p *BitbucketProvider) CreatePR(ctx context.Context, repo RepoRef, spec PRSpec) (PR, error) {
	if err := validateRepoRef("bitbucket", "CreatePR", repo); err != nil {
		return PR{}, err
	}
	reqBody, err := json.Marshal(bitbucketCreatePRRequest{
		Title:       spec.Title,
		Description: spec.Body,
		Source:      bitbucketPRBranch{Branch: bitbucketBranchRef{Name: spec.Head}},
		Destination: bitbucketPRBranch{Branch: bitbucketBranchRef{Name: spec.Base}},
	})
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(bitbucket): marshal create-pr request: %w", err)
	}

	reqURL := fmt.Sprintf("%s/2.0/repositories/%s/%s/pullrequests", p.apiBase(), url.PathEscape(repo.Owner), url.PathEscape(repo.Repo))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(bitbucket): build create-pr request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	p.setAuthHeader(httpReq)

	resp, err := p.httpClient().Do(httpReq)
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(bitbucket): create-pr request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PR{}, newError("bitbucket", "CreatePR", resp.StatusCode, readErrorBody(resp.Body, p.secrets()...))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(bitbucket): read create-pr response: %w", err)
	}
	var out bitbucketPRResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return PR{}, fmt.Errorf("gitprovider(bitbucket): decode create-pr response: %w", err)
	}
	return PR{ID: fmt.Sprintf("%d", out.ID), Number: out.ID, URL: out.Links.HTML.Href}, nil
}

// PRStatus reads a pull request's state via GET .../pullrequests/{id} and maps Bitbucket's
// OPEN|MERGED|DECLINED|SUPERSEDED onto the provider-agnostic PRState (SUPERSEDED, like DECLINED,
// is a terminal not-merged outcome, so it maps to PRStateDeclined).
func (p *BitbucketProvider) PRStatus(ctx context.Context, repo RepoRef, id string) (PRState, error) {
	if err := validateRepoRef("bitbucket", "PRStatus", repo); err != nil {
		return "", err
	}
	if err := validatePRID("bitbucket", "PRStatus", id); err != nil {
		return "", err
	}
	reqURL := fmt.Sprintf("%s/2.0/repositories/%s/%s/pullrequests/%s", p.apiBase(), url.PathEscape(repo.Owner), url.PathEscape(repo.Repo), url.PathEscape(id))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("gitprovider(bitbucket): build pr-status request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	p.setAuthHeader(httpReq)

	resp, err := p.httpClient().Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("gitprovider(bitbucket): pr-status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newError("bitbucket", "PRStatus", resp.StatusCode, readErrorBody(resp.Body, p.secrets()...))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("gitprovider(bitbucket): read pr-status response: %w", err)
	}
	var out bitbucketPRResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("gitprovider(bitbucket): decode pr-status response: %w", err)
	}
	return mapBitbucketState(out.State), nil
}

func mapBitbucketState(state string) PRState {
	switch state {
	case "MERGED":
		return PRStateMerged
	case "OPEN":
		return PRStateOpen
	default: // DECLINED, SUPERSEDED, or anything unrecognized
		return PRStateDeclined
	}
}
