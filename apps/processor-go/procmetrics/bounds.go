package procmetrics

import (
	"regexp"
	"strings"
	"sync"
)

// sentinel-reporting-and-dashboard D21: the bounds on every label of the D8a counters. These counters carry a project
// name, so each label must come from a set the code (or the project's own declaration) limits.

// ServiceLabelLimit is D21's cap on distinct `service` label values per project; the rest are "other".
const ServiceLabelLimit = 30

// OtherLabel is the overflow value for environment and service.
const OtherLabel = "other"

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is D21's project label: the project's name, lowercased, every run of other characters one "-", trimmed.
// An empty result is "unknown": a label is never empty.
func Slug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "unknown"
	}
	return s
}

// Level maps an occurrence level onto D21's closed set: fatal error warning info debug other.
func Level(level string) string {
	switch l := strings.ToLower(level); l {
	case "fatal", "error", "warning", "info", "debug":
		return l
	default:
		return OtherLabel
	}
}

// Environment is D21's rule: a declared project_environments name, or "other".
func Environment(name string, declared bool) string {
	if declared && name != "" {
		return name
	}
	return OtherLabel
}

// ServiceBounder admits at most ServiceLabelLimit distinct service values per project. A value admitted once stays
// admitted, so a series never flips to "other" mid-life; the 31st distinct value and every later new one is "other".
type ServiceBounder struct {
	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

func NewServiceBounder() *ServiceBounder {
	return &ServiceBounder{seen: map[string]map[string]struct{}{}}
}

// Bound returns the label value for (project, service) and whether it overflowed. An empty service is "" (the
// caller records the unattributed group with no service value, which is labelled "none").
func (b *ServiceBounder) Bound(project, service string) (label string, overflowed bool) {
	if service == "" {
		return "none", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.seen[project]
	if set == nil {
		set = map[string]struct{}{}
		b.seen[project] = set
	}
	if _, ok := set[service]; ok {
		return service, false
	}
	if len(set) >= ServiceLabelLimit {
		return OtherLabel, true
	}
	set[service] = struct{}{}
	return service, false
}
