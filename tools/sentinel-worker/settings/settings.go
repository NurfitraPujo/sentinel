// Package settings implements sentinel-worker's settings-refresh loop (plan §4.5, C15/C16):
// periodically pulling per-project agent policy (fixEnabled, maxPrsPerDay, repo connection) from
// GET /api/agent/projects and, when any project has a repo connection, a provider->credential map
// from GET /api/agent/repo-credentials. Both responses are read-only inputs to the rest of the
// worker (FIX-engine wiring for N8f, repoctx/gitprovider for N8c's other packages); this package
// owns only the fetch, disambiguation, and in-memory Store -- never the git plumbing itself.
//
// SECURITY (plan §4.5/§4.6): fetched credential secrets live in the Store's memory ONLY. They are
// never written to the journal, the state dir, or a snapshot, and must never be logged. Callers
// that need a secret (the future askpass helper) read it via Store.CredentialFor at the moment of
// use; nothing here exposes a serialization path for a Credential's Secret.
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// RepoConn is the repo connection embedded in a project's agentSettings (C15's `repo` shape,
// dashboard's AgentProjectRepo / guide §2a).
type RepoConn struct {
	Provider      string `json:"provider"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"defaultBranch"`
	TestCmd       string `json:"testCmd"`
	AgentCmd      string `json:"agentCmd"`
	CloneDepth    int    `json:"cloneDepth"`
}

// ProjectSettings is one GET /api/agent/projects row's C15 agentSettings, plus the project
// identity fields the worker needs to key off of.
type ProjectSettings struct {
	ProjectID    string
	Name         string
	FixEnabled   bool
	MaxPRsPerDay *int // nil = server reported null = no self-enforced cap (C15: "self-enforced by the agent")
	Repo         *RepoConn
}

// FixReady reports whether this project satisfies plan §4.5's FIX precondition: fixEnabled AND a
// repo connection exists. fixEnabled without a connection means propose-only, per C15.
func (p ProjectSettings) FixReady() bool {
	return p.FixEnabled && p.Repo != nil
}

// CredentialSecret is C16's `secret: {token} | {username, appPassword}` union, decoded losslessly
// from whichever shape the server sent. Exactly one of (Token) or (Username && AppPassword) is
// populated for a well-formed credential.
type CredentialSecret struct {
	Token       string
	Username    string
	AppPassword string
}

// IsToken reports whether this secret is the single-token form (GitHub fine-grained PAT / access
// token) rather than the username+app-password form (Bitbucket).
func (s CredentialSecret) IsToken() bool { return s.Token != "" }

// Usable reports whether this secret carries a value the git plumbing can actually authenticate
// with: either the single-token form or a complete username+app-password pair. A credential row
// whose `secret` is `{}`, `null`, or has only one half of the username/appPassword pair is NOT
// usable -- Refresh must skip it (never resolve it into Store.credentials, never let it suppress
// env fallback, never report it as available on /readyz or in metrics).
func (s CredentialSecret) Usable() bool {
	return s.Token != "" || (s.Username != "" && s.AppPassword != "")
}

// String implements fmt.Stringer so that fmt's "%v"/"%+v"/"%s" verbs (including when a
// CredentialSecret or Credential is embedded in a logged struct) never print the plaintext
// secret -- see the package doc comment's SECURITY note.
func (s CredentialSecret) String() string { return "[REDACTED]" }

// LogValue implements slog.LogValuer so a bare slog attribute of a CredentialSecret (or a struct
// embedding one) never emits the plaintext secret.
func (s CredentialSecret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// GoString implements fmt.GoStringer (finding 3) so the "%#v" verb -- which dumps a struct's
// fields verbatim via reflection and is NOT intercepted by String()/LogValue()/MarshalJSON, only
// by GoStringer -- never prints the plaintext Token/Username/AppPassword. Without this,
// fmt.Sprintf("%#v", secret) (e.g. from a debug print or a future %#v-based log helper) bypasses
// every other redaction path on this type.
func (s CredentialSecret) GoString() string { return "settings.CredentialSecret{[REDACTED]}" }

// MarshalJSON redacts the secret on any JSON serialization path. UnmarshalJSON below is the only
// way a real value enters this type; there is deliberately no inverse for Marshal.
func (s CredentialSecret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

func (s *CredentialSecret) UnmarshalJSON(data []byte) error {
	var withToken struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &withToken); err == nil && withToken.Token != "" {
		s.Token = withToken.Token
		return nil
	}
	var withUser struct {
		Username    string `json:"username"`
		AppPassword string `json:"appPassword"`
	}
	if err := json.Unmarshal(data, &withUser); err == nil && withUser.Username != "" && withUser.AppPassword != "" {
		s.Username = withUser.Username
		s.AppPassword = withUser.AppPassword
		return nil
	}
	// Neither union arm matched a usable value: an empty object, null, or a shape with only one
	// half of the username/appPassword pair. Leave the zero value (Usable() == false) rather than
	// erroring the whole projects/credentials refresh over one malformed row -- Refresh's loop
	// below is responsible for skipping non-usable rows and recording why.
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return nil
}

// Credential is one entry from GET /api/agent/repo-credentials (C16).
type Credential struct {
	ID       string
	Provider string
	Label    string
	Secret   CredentialSecret
}

// String, LogValue, and MarshalJSON on Credential are defense-in-depth on top of
// CredentialSecret's own redaction (fmt and encoding/json already recurse into the Secret field
// and honour its Stringer/LogValuer/Marshaler there) -- this guarantees the whole Credential value
// never leaks the token even if a future refactor stops using CredentialSecret as a named field
// type fmt/json would recurse into.
func (c Credential) String() string {
	return fmt.Sprintf("{ID:%s Provider:%s Label:%s Secret:[REDACTED]}", c.ID, c.Provider, c.Label)
}

func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", c.ID),
		slog.String("provider", c.Provider),
		slog.String("label", c.Label),
		slog.String("secret", "[REDACTED]"),
	)
}

func (c Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Label    string `json:"label"`
		Secret   string `json:"secret"`
	}{ID: c.ID, Provider: c.Provider, Label: c.Label, Secret: "[REDACTED]"})
}

// GoString implements fmt.GoStringer (finding 3) for the same reason CredentialSecret.GoString
// does: "%#v" is not intercepted by String()/LogValue()/MarshalJSON, so without this a bare
// fmt.Sprintf("%#v", cred) would reflect straight past Secret's own redaction and print its
// unexported Token/Username/AppPassword fields verbatim.
func (c Credential) GoString() string {
	return fmt.Sprintf("settings.Credential{ID:%#v, Provider:%#v, Label:%#v, Secret:[REDACTED]}", c.ID, c.Provider, c.Label)
}

// projectsResponse mirrors GET /api/agent/projects' wire shape (agent-reads.ts's AgentProject /
// AgentProjectAgentSettings / AgentProjectRepo).
type projectsResponse struct {
	Projects []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		AgentSettings struct {
			FixEnabled   bool      `json:"fixEnabled"`
			MaxPRsPerDay *int      `json:"maxPrsPerDay"`
			Repo         *RepoConn `json:"repo"`
		} `json:"agentSettings"`
	} `json:"projects"`
}

// credentialsResponse mirrors GET /api/agent/repo-credentials' wire shape.
type credentialsResponse struct {
	Credentials []struct {
		ID       string           `json:"id"`
		Provider string           `json:"provider"`
		Label    string           `json:"label"`
		Secret   CredentialSecret `json:"secret"`
	} `json:"credentials"`
}

// Client is the subset of sentinel.Client this package needs, kept as an interface so tests can
// drive it against an httptest server without a real network dependency (matching loop.EventsClient's
// convention).
type Client interface {
	ListProjects(ctx context.Context) (*sentinel.Result, error)
	GetRepoCredentials(ctx context.Context) (*sentinel.Result, error)
}

// EnvLookup is the minimal env-read seam Store needs for GIT_GITHUB_TOKEN / GIT_BITBUCKET_* env
// fallback (C16: "env fallback ... used when server credentials absent"), kept as an interface so
// tests can supply a fake environment.
type EnvLookup func(key string) (string, bool)

// providerStatus records why a provider's credential is unavailable, for /readyz detail and
// metrics. Empty string means available.
type providerStatus struct {
	reason string
}

// Store holds the most recently refreshed settings snapshot: per-project agent policy and a
// provider->credential map. All reads/writes are mutex-guarded so the refresh loop (one writer)
// and job runners / readiness handlers (many readers) can run concurrently.
//
// Credentials are held ONLY in the `credentials` field below, in memory, for the lifetime of the
// process (or until the next successful refresh replaces them) -- see the package doc comment's
// SECURITY note. Nothing in this type implements MarshalJSON, GobEncode, or any other
// serialization for Credential/CredentialSecret, by design: there must be no accidental path from
// this struct to a file.
type Store struct {
	mu sync.RWMutex

	projects      map[string]ProjectSettings // by projectId
	credentials   map[string]Credential      // by provider, disambiguated (server-resolved + env fallback)
	serverCreds   map[string]Credential      // subset of credentials that came from the server (not env fallback) -- retained across a non-authoritative refresh
	serverUnavail map[string]providerStatus  // subset of unavailable produced by the server response itself (ambiguity/label-not-found) -- retained across a non-authoritative refresh
	unavailable   map[string]providerStatus  // by provider: why no usable credential exists

	credentialsFetched   bool // whether GET /api/agent/repo-credentials has ever been attempted
	credentialsForbidden bool // 403 from the last attempt (C16: log the provisioning hint)

	lastRefresh time.Time
}

// NewStore returns an empty Store. Callers must call Refresh at least once before reading a
// meaningful snapshot; readers on an unrefreshed Store simply see "no projects, no credentials"
// rather than an error, matching this package's "never crash, report per-project" philosophy.
func NewStore() *Store {
	return &Store{
		projects:      map[string]ProjectSettings{},
		credentials:   map[string]Credential{},
		serverCreds:   map[string]Credential{},
		serverUnavail: map[string]providerStatus{},
		unavailable:   map[string]providerStatus{},
	}
}

// String implements fmt.Stringer for *Store itself (finding 6). Without this, fmt/slog reflecting
// into Store's structure to print it would walk `credentials`/`serverCreds` — both UNEXPORTED
// fields — and Go's reflect package refuses to call String()/LogValue() on a value reached only
// through an unexported field (CanInterface() is false for it and everything beneath it), so
// Credential's and CredentialSecret's own String()/LogValue()/MarshalJSON redaction (see those
// types above) is silently skipped and the raw struct fields — including the plaintext token —
// print instead. A caller doing `slog.Any("store", store)` or `fmt.Sprintf("%+v", store)` (or the
// same via a struct that merely embeds *Store) must never be able to dump a secret this way; this
// method and LogValue below are the only safe path once a *Store value's field access already
// disables per-field method dispatch.
func (s *Store) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	providers := make([]string, 0, len(s.credentials))
	for p := range s.credentials {
		providers = append(providers, p)
	}
	sortStrings(providers)
	return fmt.Sprintf("Store{projects:%d credentialProviders:%v credentialsFetched:%v credentialsForbidden:%v lastRefresh:%s}",
		len(s.projects), providers, s.credentialsFetched, s.credentialsForbidden, s.lastRefresh.Format(time.RFC3339))
}

// LogValue implements slog.LogValuer for *Store (finding 6), for the same reason String does: a
// bare slog attribute of a *Store (or a struct embedding one) must never fall back to reflecting
// into the unexported `credentials`/`serverCreds` maps and printing a raw Credential's fields.
// Only structural facts are logged -- project count, the list of providers with a currently
// resolved credential (names only, never the credential values), fetch/forbidden state, and last
// refresh time -- exactly the same information String's redacted summary exposes.
func (s *Store) LogValue() slog.Value {
	s.mu.RLock()
	defer s.mu.RUnlock()
	providers := make([]string, 0, len(s.credentials))
	for p := range s.credentials {
		providers = append(providers, p)
	}
	sortStrings(providers)
	return slog.GroupValue(
		slog.Int("project_count", len(s.projects)),
		slog.Any("credential_providers", providers),
		slog.Bool("credentials_fetched", s.credentialsFetched),
		slog.Bool("credentials_forbidden", s.credentialsForbidden),
		slog.Time("last_refresh", s.lastRefresh),
	)
}

// GoString implements fmt.GoStringer for *Store (finding 3), for the same reason String and
// LogValue above do: "%#v" is a THIRD fmt verb, not intercepted by String()/LogValue(), that
// reflects straight into a struct's fields -- including unexported ones on the reflect path %#v
// uses -- and would otherwise dump `credentials`/`serverCreds` (maps of Credential, whose own
// String/LogValue/MarshalJSON are all skipped once reached via reflection through an unexported
// field) with plaintext tokens inside. Returns the same structural summary as String, in Go
// syntax, so "%#v" on a *Store is exactly as safe as "%v"/"%s"/a slog attribute.
func (s *Store) GoString() string {
	return s.String()
}

// Project returns the current settings for a project id, and whether one was found.
func (s *Store) Project(projectID string) (ProjectSettings, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[projectID]
	return p, ok
}

// Projects returns a snapshot slice of every known project's settings.
func (s *Store) Projects() []ProjectSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProjectSettings, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	return out
}

// CredentialFor returns the resolved credential for a git provider ("github" | "bitbucket"), and
// whether one is currently available. Consumed by the (future) askpass helper at the moment of
// use -- never logged, never serialized.
func (s *Store) CredentialFor(provider string) (Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.credentials[provider]
	return c, ok
}

// ProviderUnavailableReason returns why `provider` has no usable credential ("" if it does, or if
// nothing has ever been refreshed). Used by readiness detail.
func (s *Store) ProviderUnavailableReason(provider string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unavailable[provider].reason
}

// ReadinessProvider is one row of ReadinessDetail's per-provider view.
type ReadinessProvider struct {
	Provider    string
	Available   bool
	Unavailable string // reason, empty when Available
}

// ReadinessProject is one project's row in ReadinessDetail.Projects (plan §4.5: "a connection
// whose provider matches no held credential is reported per-project via /readyz detail +
// metrics"). Only projects with a repo connection get a row -- a project with no connection has
// nothing to report here.
type ReadinessProject struct {
	ProjectID           string
	Name                string
	Provider            string
	FixEnabled          bool
	CredentialAvailable bool
	Reason              string // non-empty whenever CredentialAvailable is false
}

// ReadinessDetail is the /readyz payload + health-gauge addition this package owns (task brief):
// per-provider credential availability, per-project rows, and repo-connection counts. Never
// returns an error -- a connection whose provider has no usable credential is reported here, not
// treated as fatal.
type ReadinessDetail struct {
	Providers            []ReadinessProvider
	Projects             []ReadinessProject // per-project rows, one per project with a repo connection
	RepoConnectionsTotal int
	RepoConnectionsReady int // connections whose provider currently has a usable credential
	LastRefresh          time.Time
	CredentialsForbidden bool // C16: 403 from the last repo-credentials fetch
}

// Readiness computes the current ReadinessProvider list and connection counts from the snapshot.
// Deterministic ordering (sorted by provider) for stable /readyz output.
func (s *Store) Readiness() ReadinessDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := map[string]bool{}
	var providers []string
	for p := range s.credentials {
		if !seen[p] {
			seen[p] = true
			providers = append(providers, p)
		}
	}
	for p := range s.unavailable {
		if !seen[p] {
			seen[p] = true
			providers = append(providers, p)
		}
	}
	for _, proj := range s.projects {
		if proj.Repo != nil && !seen[proj.Repo.Provider] {
			seen[proj.Repo.Provider] = true
			providers = append(providers, proj.Repo.Provider)
		}
	}
	sortStrings(providers)

	detail := ReadinessDetail{LastRefresh: s.lastRefresh, CredentialsForbidden: s.credentialsForbidden}
	for _, p := range providers {
		_, available := s.credentials[p]
		reason := s.unavailable[p].reason
		if !available && reason == "" {
			// No ambiguity, no bad label, no server error recorded -- just plain "nothing resolved
			// for this provider" (no server credential and no env fallback configured). Without
			// this the readiness row was Available:false with an empty diagnostic.
			reason = fmt.Sprintf("no usable credential for provider %q (no server credential resolved, no env fallback configured)", p)
		}
		detail.Providers = append(detail.Providers, ReadinessProvider{
			Provider:    p,
			Available:   available,
			Unavailable: reason,
		})
	}

	// Per-project rows (plan §4.5: "a connection whose provider matches no held credential is
	// reported per-project"), sorted by project id for stable /readyz output.
	var projectIDs []string
	for id, proj := range s.projects {
		if proj.Repo != nil {
			projectIDs = append(projectIDs, id)
		}
	}
	sortStrings(projectIDs)
	for _, id := range projectIDs {
		proj := s.projects[id]
		detail.RepoConnectionsTotal++
		_, available := s.credentials[proj.Repo.Provider]
		reason := ""
		if !available {
			reason = s.unavailable[proj.Repo.Provider].reason
			if reason == "" {
				reason = fmt.Sprintf("no usable credential for provider %q (no server credential resolved, no env fallback configured)", proj.Repo.Provider)
			}
		} else {
			detail.RepoConnectionsReady++
		}
		detail.Projects = append(detail.Projects, ReadinessProject{
			ProjectID:           proj.ProjectID,
			Name:                proj.Name,
			Provider:            proj.Repo.Provider,
			FixEnabled:          proj.FixEnabled,
			CredentialAvailable: available,
			Reason:              reason,
		})
	}
	return detail
}

// sortStrings is a tiny insertion sort to avoid importing "sort" for a handful of provider names
// in the hot readiness path; correctness over micro-optimization is not the point here, this just
// keeps the import list minimal like the rest of this package.
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j-1] > ss[j]; j-- {
			ss[j-1], ss[j] = ss[j], ss[j-1]
		}
	}
}

// EnvFallback captures the GIT_GITHUB_TOKEN / GIT_BITBUCKET_TOKEN / GIT_BITBUCKET_USER+
// GIT_BITBUCKET_APP_PASSWORD env vars (plan §4.5/§5), used when the server has no usable
// credential for a provider (C16: "Env tokens ... remain bootstrap/fallback").
type EnvFallback struct {
	GitHubToken          string
	BitbucketToken       string
	BitbucketUser        string
	BitbucketAppPassword string
}

// envCredentials builds the provider->Credential map implied by EnvFallback, skipping any
// provider with no usable value configured.
func envCredentials(env EnvFallback) map[string]Credential {
	out := map[string]Credential{}
	if env.GitHubToken != "" {
		out["github"] = Credential{Provider: "github", Label: "env", Secret: CredentialSecret{Token: env.GitHubToken}}
	}
	if env.BitbucketToken != "" {
		out["bitbucket"] = Credential{Provider: "bitbucket", Label: "env", Secret: CredentialSecret{Token: env.BitbucketToken}}
	} else if env.BitbucketUser != "" && env.BitbucketAppPassword != "" {
		out["bitbucket"] = Credential{
			Provider: "bitbucket",
			Label:    "env",
			Secret:   CredentialSecret{Username: env.BitbucketUser, AppPassword: env.BitbucketAppPassword},
		}
	}
	return out
}

// labelsFromConfig parses WORKER_CREDENTIAL_LABELS ("provider=label" entries, plan §4.5) into a
// provider->label map. Malformed entries (no "=") are ignored -- LoadConfig-level validation is
// out of this package's scope; a malformed entry simply behaves as if it were absent (ambiguity
// warning still fires if the provider genuinely has multiple credentials).
func labelsFromConfig(entries []string) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				out[e[:i]] = e[i+1:]
				break
			}
		}
	}
	return out
}

// Refresh performs one full settings-refresh pass (plan §4.5): GET /api/agent/projects, then --
// only when at least one project has a repo connection -- GET /api/agent/repo-credentials,
// resolving a provider->credential map per WORKER_CREDENTIAL_LABELS disambiguation, WARNing (via
// logger) and marking a provider unavailable when its credentials are ambiguous, and falling back
// to env credentials for any provider the server left unresolved. It never returns an error for a
// credentials-layer problem (403/503/ambiguity) -- those are recorded in the Store for readiness
// and logged; the worker keeps triaging regardless (C16). A non-nil error return means the
// PROJECTS fetch itself failed (network/5xx/auth) -- the caller should retry per its own backoff
// policy, same as resolveAgentID in main.go.
func (s *Store) Refresh(ctx context.Context, client Client, credentialLabels []string, env EnvFallback, logger *slog.Logger) error {
	res, err := client.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("GET /api/agent/projects: %w", err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("GET /api/agent/projects: %d %s", res.Status, sentinel.ErrorMessage(res.Body))
	}
	var parsed projectsResponse
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return fmt.Errorf("parsing GET /api/agent/projects response: %w", err)
	}

	projects := make(map[string]ProjectSettings, len(parsed.Projects))
	hasRepo := false
	for _, row := range parsed.Projects {
		ps := ProjectSettings{
			ProjectID:    row.ID,
			Name:         row.Name,
			FixEnabled:   row.AgentSettings.FixEnabled,
			MaxPRsPerDay: row.AgentSettings.MaxPRsPerDay,
			Repo:         row.AgentSettings.Repo,
		}
		projects[row.ID] = ps
		if ps.Repo != nil {
			hasRepo = true
		}
	}

	credentials := map[string]Credential{}
	unavailable := map[string]providerStatus{}
	forbidden := false
	// authoritative reports whether this pass got a definitive answer about server credentials (a
	// 200 with a parseable body, or a 403 denial) -- either way, `credentials`/`unavailable` built
	// below fully replace the previous snapshot. A non-authoritative outcome (network error, 5xx,
	// unparseable body) is NOT evidence of revocation (plan §4.5), so on that path we keep the
	// previous snapshot's server-resolved credentials/unavailable state instead of discarding them;
	// see the finding this addresses: a single transient 500 used to drop every server credential
	// for up to WORKER_SETTINGS_REFRESH.
	authoritative := false

	if hasRepo {
		labels := labelsFromConfig(credentialLabels)
		byProvider := map[string][]Credential{}

		credRes, credErr := client.GetRepoCredentials(ctx)
		switch {
		case credErr != nil:
			if logger != nil {
				logger.Error("settings refresh: GET /api/agent/repo-credentials failed (transient), keeping previous server credential snapshot", "error", credErr)
			}
		case credRes.Status == 403:
			forbidden = true
			authoritative = true
			if logger != nil {
				// C16: "403 means log the canAccessRepoCredentials provisioning hint + mark
				// credentials unavailable (worker still triages)".
				logger.Error("settings refresh: this agent is not authorized to access repo credentials (403); an org admin must grant canAccessRepoCredentials on this agent for FIX/PR flows to use server-managed git credentials -- falling back to env credentials in the meantime")
			}
		case credRes.Status < 200 || credRes.Status >= 300:
			if logger != nil {
				logger.Error("settings refresh: GET /api/agent/repo-credentials returned a server error (transient), keeping previous server credential snapshot", "status", credRes.Status, "detail", sentinel.ErrorMessage(credRes.Body))
			}
		default:
			var cr credentialsResponse
			if err := json.Unmarshal(credRes.Body, &cr); err != nil {
				if logger != nil {
					logger.Error("settings refresh: parsing GET /api/agent/repo-credentials response failed (transient), keeping previous server credential snapshot", "error", err)
				}
			} else {
				authoritative = true
				for _, row := range cr.Credentials {
					c := Credential{ID: row.ID, Provider: row.Provider, Label: row.Label, Secret: row.Secret}
					if !c.Secret.Usable() {
						// C16/finding: a credential row with an empty, null, or unrecognised
						// secret shape must never resolve into Store.credentials -- it would
						// suppress a working env fallback and lie on /readyz + metrics.
						reason := fmt.Sprintf("server credential %q for provider %q has an empty/unrecognised secret", row.ID, row.Provider)
						unavailable[row.Provider] = providerStatus{reason: reason}
						if logger != nil {
							logger.Warn("settings refresh: server credential has an empty/unrecognised secret, skipping", "provider", row.Provider, "credential_id", row.ID)
						}
						continue
					}
					byProvider[row.Provider] = append(byProvider[row.Provider], c)
				}
			}
		}

		for provider, creds := range byProvider {
			switch {
			case len(creds) == 1:
				credentials[provider] = creds[0]
				// A usable credential resolved for this provider -- clear any
				// empty/unrecognised-secret reason recorded above for a sibling row of the
				// same provider, or it would linger and lie on /readyz alongside a working
				// credential.
				delete(unavailable, provider)
			case labels[provider] != "":
				found := false
				for _, c := range creds {
					if c.Label == labels[provider] {
						credentials[provider] = c
						found = true
						break
					}
				}
				if found {
					delete(unavailable, provider)
				}
				if !found {
					reason := fmt.Sprintf("WORKER_CREDENTIAL_LABELS names label %q for provider %q but no active credential has that label", labels[provider], provider)
					unavailable[provider] = providerStatus{reason: reason}
					if logger != nil {
						logger.Warn("settings refresh: credential label not found", "provider", provider, "label", labels[provider])
					}
				}
			default:
				reason := fmt.Sprintf("%d active credentials for provider %q; set WORKER_CREDENTIAL_LABELS to disambiguate", len(creds), provider)
				unavailable[provider] = providerStatus{reason: reason}
				if logger != nil {
					logger.Warn("settings refresh: ambiguous credentials for provider, none applied (set WORKER_CREDENTIAL_LABELS)", "provider", provider, "count", len(creds))
				}
			}
		}
	}

	// Snapshot the server-resolved maps (independent copies) BEFORE env fallback is layered on top
	// of `credentials`/`unavailable` below -- these are what gets retained across a future
	// non-authoritative refresh, so they must never alias the map env fallback mutates in place.
	serverCreds := make(map[string]Credential, len(credentials))
	for p, c := range credentials {
		serverCreds[p] = c
	}
	serverUnavail := make(map[string]providerStatus, len(unavailable))
	for p, st := range unavailable {
		serverUnavail[p] = st
	}

	if hasRepo && !authoritative {
		// The credentials fetch failed for a non-authoritative reason (network error, 5xx,
		// unparseable body) -- that is not evidence of revocation (plan §4.5), so keep whatever the
		// previous refresh resolved from the server instead of discarding it. env fallback below
		// still applies on top for providers neither this nor the previous pass resolved.
		s.mu.RLock()
		serverCreds = make(map[string]Credential, len(s.serverCreds))
		for p, c := range s.serverCreds {
			serverCreds[p] = c
		}
		serverUnavail = make(map[string]providerStatus, len(s.serverUnavail))
		for p, st := range s.serverUnavail {
			serverUnavail[p] = st
		}
		forbidden = s.credentialsForbidden
		s.mu.RUnlock()
		credentials = serverCreds
		unavailable = serverUnavail
		if logger != nil {
			logger.Warn("settings refresh: repo-credentials fetch was non-authoritative this pass, retaining previous server credential snapshot", "providers_retained", len(serverCreds))
		}
	}

	// Env fallback (C16): fill any provider the server left unresolved (absent, 403/503/error, or
	// ambiguous) with an env-derived credential, per project's stated precedence ("Env tokens ...
	// remain bootstrap/fallback"). Server-resolved credentials always win when present.
	for provider, c := range envCredentials(env) {
		if _, ok := credentials[provider]; !ok {
			credentials[provider] = c
			delete(unavailable, provider) // env fallback resolves the ambiguity/absence for readiness purposes
		}
	}

	s.mu.Lock()
	s.projects = projects
	s.credentials = credentials
	s.serverCreds = serverCreds
	s.serverUnavail = serverUnavail
	s.unavailable = unavailable
	s.credentialsFetched = hasRepo
	s.credentialsForbidden = forbidden
	s.lastRefresh = time.Now()
	s.mu.Unlock()

	return nil
}

// RefreshLoop runs Refresh on `interval` until ctx is cancelled, logging (not crashing) on
// failure -- matching plan §6's "invalid config keeps the process up" philosophy extended to
// runtime refresh failures. Callers should invoke Refresh once synchronously before starting this
// loop (same convention as main.go's other background loops) so the first snapshot is available
// before the poll loop starts routing jobs.
func RefreshLoop(ctx context.Context, s *Store, client Client, interval time.Duration, credentialLabels []string, env EnvFallback, logger *slog.Logger) {
	if interval <= 0 {
		// A zero/negative interval means the caller passed an unconfigured Config (e.g. a test
		// exercising runPipeline directly, or a future caller that forgot to set it) -- treat it as
		// "refresh disabled" rather than panicking time.NewTicker, matching this package's "never
		// crash" posture (LoadConfig itself rejects <=0 in normal operation).
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Refresh(ctx, client, credentialLabels, env, logger); err != nil && logger != nil {
				logger.Error("settings refresh failed, keeping previous snapshot", "error", err)
			}
		}
	}
}
