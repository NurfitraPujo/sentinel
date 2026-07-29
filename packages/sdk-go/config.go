package sentinel

import (
	"errors"
	"strings"
	"time"
)

type Config struct {
	// APIKey is the SECRET credential, sent in the `X-API-Key` header. It is a project-scoped key
	// (`sent_live_...`) or an organization-wide key (`sent_org_...`).
	//
	// It is never placed in the event body. Doing so would copy the credential into every payload
	// that is logged, buffered in NATS, and persisted.
	APIKey string `json:"-"`

	// ProjectKey is the target project's UNIQUE NAME (projects.name) — an identifier, not a secret.
	// It travels in the event body as `project_key` and is how an ORGANIZATION-WIDE key selects
	// which project in its organization an event belongs to.
	//
	// Optional for a project-scoped key, whose project is already fixed by the credential; if set,
	// the server rejects a mismatch with 403 rather than trusting it. Required for an org-wide key.
	//
	// NOTE: before v0.2.0 this single field held the API key and was used for BOTH jobs, which is
	// why every SDK event was accepted and then silently discarded — the server resolved the value
	// against projects.name and never found it. If you are upgrading, move your secret to APIKey.
	ProjectKey     string        `json:"project_key"`
	Endpoint       string        `json:"endpoint"`
	Environment    string        `json:"environment"`
	ReleaseVersion string        `json:"release_version"`
	SampleRate     float64       `json:"sample_rate"`
	MaxBufferSize  int           `json:"max_buffer_size"`
	BatchSize      int           `json:"batch_size"`
	BatchWait      time.Duration `json:"batch_wait"`
	MaxBreadcrumbs int           `json:"max_breadcrumbs"`
	Debug          bool          `json:"debug"`
	// OnError, if set, is invoked whenever a batch of events is dropped -
	// after retries are exhausted on a 5xx/network error, or immediately on
	// a non-retryable 4xx rejection. It is called from the SDK's internal
	// worker goroutine, never from the caller's goroutine (see D8: the SDK
	// must never block application code), so it must not block for long and
	// must be safe to call concurrently with itself.
	OnError func(error) `json:"-"`
}

// looksLikeSecret reports whether v carries one of the API-key prefixes. Used to catch the
// APIKey/ProjectKey swap loudly instead of letting it fail as a silent 100% rejection rate.
func looksLikeSecret(v string) bool {
	for _, prefix := range []string{"sent_live_", "sent_org_", "pk_live_", "sk_"} {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// Validate reports configuration errors that would otherwise surface only as events vanishing.
func (c *Config) Validate() error {
	if c.APIKey == "" {
		if looksLikeSecret(c.ProjectKey) {
			return errors.New("sentinel: Config.APIKey is empty and Config.ProjectKey looks like an API key — " +
				"as of v0.2.0 the secret goes in APIKey and ProjectKey holds the project's unique name")
		}
		return errors.New("sentinel: Config.APIKey is required")
	}
	if looksLikeSecret(c.ProjectKey) {
		return errors.New("sentinel: Config.ProjectKey looks like an API key — it must be the project's unique name (projects.name)")
	}
	return nil
}

func (c *Config) withDefaults() Config {
	cfg := *c
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:8080/ingest"
	}
	if cfg.Environment == "" {
		cfg.Environment = "development"
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 1.0
	}
	if cfg.MaxBufferSize <= 0 {
		cfg.MaxBufferSize = 100
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 10
	}
	if cfg.BatchWait <= 0 {
		cfg.BatchWait = 1 * time.Second
	}
	if cfg.MaxBreadcrumbs <= 0 {
		cfg.MaxBreadcrumbs = 50
	}
	return cfg
}
