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

const defaultGitHubAPIBase = "https://api.github.com"

// GitHubProvider is a Provider backed by a fine-grained GitHub PAT (plan §4.5: "GitHub App
// installation tokens deferred").
type GitHubProvider struct {
	Token string
	// APIBase overrides the default https://api.github.com — configurable for tests.
	APIBase string
	HTTP    *http.Client
}

// NewGitHubProvider builds a GitHubProvider with default API base and http.Client.
func NewGitHubProvider(token string) *GitHubProvider {
	return &GitHubProvider{Token: token, APIBase: defaultGitHubAPIBase, HTTP: http.DefaultClient}
}

func (p *GitHubProvider) apiBase() string {
	if p.APIBase != "" {
		return p.APIBase
	}
	return defaultGitHubAPIBase
}

func (p *GitHubProvider) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

// Auth returns the fine-grained-PAT credential: GitHub's HTTPS auth accepts the token as the
// username with any non-empty password.
func (p *GitHubProvider) Auth() GitCredential {
	return GitHubTokenCredential(p.Token)
}

type githubCreatePRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

type githubPullResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"` // "open" | "closed"
	Merged  bool   `json:"merged"`
}

// CreatePR opens a pull request via POST /repos/{owner}/{repo}/pulls (REST v3).
func (p *GitHubProvider) CreatePR(ctx context.Context, repo RepoRef, spec PRSpec) (PR, error) {
	if err := validateRepoRef("github", "CreatePR", repo); err != nil {
		return PR{}, err
	}
	reqBody, err := json.Marshal(githubCreatePRRequest{
		Title: spec.Title,
		Body:  spec.Body,
		Head:  spec.Head,
		Base:  spec.Base,
	})
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(github): marshal create-pr request: %w", err)
	}

	reqURL := fmt.Sprintf("%s/repos/%s/%s/pulls", p.apiBase(), url.PathEscape(repo.Owner), url.PathEscape(repo.Repo))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(github): build create-pr request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.httpClient().Do(httpReq)
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(github): create-pr request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PR{}, newError("github", "CreatePR", resp.StatusCode, readErrorBody(resp.Body, p.Token))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return PR{}, fmt.Errorf("gitprovider(github): read create-pr response: %w", err)
	}
	var out githubPullResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return PR{}, fmt.Errorf("gitprovider(github): decode create-pr response: %w", err)
	}
	return PR{ID: fmt.Sprintf("%d", out.Number), Number: out.Number, URL: out.HTMLURL}, nil
}

// PRStatus reads a pull request's state via GET /repos/{owner}/{repo}/pulls/{n} and maps GitHub's
// state+merged shape onto the provider-agnostic PRState.
func (p *GitHubProvider) PRStatus(ctx context.Context, repo RepoRef, id string) (PRState, error) {
	if err := validateRepoRef("github", "PRStatus", repo); err != nil {
		return "", err
	}
	if err := validatePRID("github", "PRStatus", id); err != nil {
		return "", err
	}
	reqURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%s", p.apiBase(), url.PathEscape(repo.Owner), url.PathEscape(repo.Repo), url.PathEscape(id))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("gitprovider(github): build pr-status request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.httpClient().Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("gitprovider(github): pr-status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newError("github", "PRStatus", resp.StatusCode, readErrorBody(resp.Body, p.Token))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("gitprovider(github): read pr-status response: %w", err)
	}
	var out githubPullResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("gitprovider(github): decode pr-status response: %w", err)
	}
	return mapGitHubState(out.State, out.Merged), nil
}

func mapGitHubState(state string, merged bool) PRState {
	if merged {
		return PRStateMerged
	}
	if state == "open" {
		return PRStateOpen
	}
	return PRStateDeclined
}

func (p *GitHubProvider) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
