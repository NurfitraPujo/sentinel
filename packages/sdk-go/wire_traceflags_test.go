package sentinel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventJSONCarriesTraceFlags(t *testing.T) {
	b, err := json.Marshal(&Event{EventID: "e1", TraceFlags: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"trace_flags":1`) {
		t.Fatalf("trace_flags missing from wire payload: %s", b)
	}
	b, _ = json.Marshal(&Event{EventID: "e1"})
	if !strings.Contains(string(b), `"trace_flags":0`) {
		t.Fatalf("unsampled must serialise as 0, not be omitted: %s", b)
	}
}
