package event

import (
	"strings"
	"testing"
)

// One case per row of D18's rule (service, tag_source, request_scoped), plus D19's group order.
func TestDeriveOrigin(t *testing.T) {
	cases := []struct {
		name        string
		tags        map[string]string
		transaction string
		url         string
		want        Origin
	}{
		{"no tags", nil, "", "", Origin{}},
		{"service only", map[string]string{"service": "api"}, "", "", Origin{Service: "api"}},
		{"service trimmed", map[string]string{"service": "  jobs worker  "}, "", "", Origin{Service: "jobs worker"}},
		{"blank service is absent", map[string]string{"service": "   "}, "", "", Origin{}},
		{"service capped at 64", map[string]string{"service": strings.Repeat("é", 70)}, "", "", Origin{Service: strings.Repeat("é", 64)}},
		{"http_handler is request scoped", map[string]string{"service": "api", "source": "http_handler"}, "", "", Origin{Service: "api", TagSource: "http_handler", RequestScoped: true}},
		{"http_panic is request scoped", map[string]string{"service": "api", "source": "http_panic"}, "", "", Origin{Service: "api", TagSource: "http_panic", RequestScoped: true}},
		{"logger with transaction and url is request scoped", map[string]string{"service": "api", "source": "logger"}, "GET /x", "https://h/x", Origin{Service: "api", TagSource: "logger", RequestScoped: true}},
		{"logger with transaction only is not", map[string]string{"service": "api", "source": "logger"}, "GET /x", "", Origin{Service: "api", TagSource: "logger"}},
		{"logger with url only is not", map[string]string{"service": "api", "source": "logger"}, "", "https://h/x", Origin{Service: "api", TagSource: "logger"}},
		{"logger with neither is not (core today)", map[string]string{"service": "api", "source": "logger"}, "", "", Origin{Service: "api", TagSource: "logger"}},
		{"job is not request scoped", map[string]string{"service": "jobs worker", "source": "job"}, "GET /x", "https://h/x", Origin{Service: "jobs worker", TagSource: "job"}},
		{"worker_handler is not", map[string]string{"service": "worker", "source": "worker_handler"}, "", "", Origin{Service: "worker", TagSource: "worker_handler"}},
		{"unknown source is stored but not request scoped", map[string]string{"source": "goroutine_panic"}, "", "", Origin{TagSource: "goroutine_panic"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveOrigin(c.tags, c.transaction, c.url); got != c.want {
				t.Fatalf("DeriveOrigin = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestOriginGroupOrder(t *testing.T) {
	cases := []struct {
		name string
		o    Origin
		want string
	}{
		{"request scoped wins over job", Origin{Service: "api", TagSource: "job", RequestScoped: true}, GroupAPI},
		{"job", Origin{Service: "jobs worker", TagSource: "job"}, GroupJobs},
		{"worker_handler", Origin{Service: "worker", TagSource: "worker_handler"}, GroupNATS},
		{"nats_client", Origin{Service: "worker", TagSource: "nats_client"}, GroupNATS},
		{"logged api error is other", Origin{Service: "api", TagSource: "logger"}, GroupOther},
		{"goroutine panic is other", Origin{Service: "worker", TagSource: "goroutine_panic"}, GroupOther},
		{"no service is unattributed", Origin{TagSource: "logger"}, GroupUnattributed},
		{"empty is unattributed", Origin{}, GroupUnattributed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.o.Group(); got != c.want {
				t.Fatalf("Group = %q, want %q", got, c.want)
			}
		})
	}
}
