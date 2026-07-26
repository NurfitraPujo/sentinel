package sentinel

import "time"

type Config struct {
	// ProjectKey is the API Key used for authentication (`X-API-Key`).
	// Supports Project API keys (`sent_live_...`) or Organization-Wide API keys (`sent_org_...`).
	// When using Org-Wide keys, specify target project key or header.
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
