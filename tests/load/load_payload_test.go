package load

import (
	"encoding/json"
	"testing"
)

// The SENTINEL_LOAD_USER_POOL addition (sentinel-affected-users §13, decision 29 — an optional
// full-stack extra, not a criterion), pinned without a stack: the ids are drawn from the pool by
// sequence number, are ABSENT (not empty) at pool 0, and the metadata key names are the ones
// indexer.ExtractSearchFields reads at highest precedence.

func TestLoadPayload_PoolZeroSendsNoIDKeys(t *testing.T) {
	p := loadPayload(LoadTestConfig{UserPool: 0}, "load-test", 7)
	if _, ok := p.Metadata["user_id"]; ok {
		t.Fatalf("pool 0 must not send user_id, got %v", p.Metadata["user_id"])
	}
	if _, ok := p.Metadata["tenant_id"]; ok {
		t.Fatalf("pool 0 must not send tenant_id, got %v", p.Metadata["tenant_id"])
	}
	if p.Metadata["load_test"] != true {
		t.Fatalf("the load_test marker must survive the pool addition, got %v", p.Metadata)
	}
	if p.Environment != "load-test" {
		t.Fatalf("environment passthrough broke: %q", p.Environment)
	}
}

func TestLoadPayload_PoolDrawsBySequenceAndWraps(t *testing.T) {
	cfg := LoadTestConfig{UserPool: 50}
	cases := []struct {
		seq              int64
		wantUser, wantTn string
	}{
		{0, "load-user-0", "load-tenant-0"},
		{49, "load-user-49", "load-tenant-49"},
		{50, "load-user-0", "load-tenant-0"}, // wraps: pool 50 has exactly 50 distinct ids
		{123, "load-user-23", "load-tenant-23"},
	}
	for _, c := range cases {
		p := loadPayload(cfg, "load-test", c.seq)
		if p.Metadata["user_id"] != c.wantUser || p.Metadata["tenant_id"] != c.wantTn {
			t.Fatalf("seq %d: got user_id=%v tenant_id=%v, want %s / %s",
				c.seq, p.Metadata["user_id"], p.Metadata["tenant_id"], c.wantUser, c.wantTn)
		}
	}
}

func TestLoadPayload_PoolTenThousandIsTenThousandDistinctIDs(t *testing.T) {
	cfg := LoadTestConfig{UserPool: 10000}
	seen := map[string]struct{}{}
	for seq := int64(0); seq < 10000; seq++ {
		seen[loadPayload(cfg, "load-test", seq).Metadata["user_id"].(string)] = struct{}{}
	}
	if len(seen) != 10000 {
		t.Fatalf("pool 10,000 over 10,000 events must yield 10,000 distinct user ids, got %d", len(seen))
	}
	if _, dup := seen[loadPayload(cfg, "load-test", 10000).Metadata["user_id"].(string)]; !dup {
		t.Fatal("event 10,000 must draw an id already in the pool")
	}
}

func TestLoadPayload_MarshalsTheIDsAsStrings(t *testing.T) {
	// The processor counts a value only when it is a JSON string (contract §4.3); a number here
	// would be silently skipped and the run would measure the no-id path while claiming a pool.
	raw, err := json.Marshal(loadPayload(LoadTestConfig{UserPool: 3}, "load-test", 2))
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Metadata["user_id"]) != `"load-user-2"` || string(back.Metadata["tenant_id"]) != `"load-tenant-2"` {
		t.Fatalf("ids must be JSON strings, got %s / %s", back.Metadata["user_id"], back.Metadata["tenant_id"])
	}
}
