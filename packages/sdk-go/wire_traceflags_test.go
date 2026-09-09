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

// Custom grouping only works if the field actually reaches the wire under the
// name the ingestor decodes.
func TestEventJSONCarriesFingerprint(t *testing.T) {
	b, err := json.Marshal(&Event{EventID: "e1", Fingerprint: "checkout-timeout"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"fingerprint":"checkout-timeout"`) {
		t.Fatalf("fingerprint missing from wire payload: %s", b)
	}

	// Omitted when unset: the overwhelming majority of events do not carry one,
	// and an empty string would be indistinguishable from "group these together
	// under the empty key" if the server ever stopped treating "" as absent.
	b, _ = json.Marshal(&Event{EventID: "e1"})
	if strings.Contains(string(b), "fingerprint") {
		t.Fatalf("empty fingerprint must be omitted: %s", b)
	}
}
