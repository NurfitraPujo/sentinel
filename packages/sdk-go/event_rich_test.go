package sentinel

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestServerNamePrecedence(t *testing.T) {
	t.Run("cfg.ServerName wins", func(t *testing.T) {
		t.Setenv("SENTINEL_SERVER_NAME", "env-name")
		ev := NewEvent(Config{ServerName: "cfg-name"}, nil, nil)
		if ev.ServerName != "cfg-name" {
			t.Errorf("ServerName = %q, want cfg-name", ev.ServerName)
		}
	})
	t.Run("env var wins over hostname", func(t *testing.T) {
		t.Setenv("SENTINEL_SERVER_NAME", "env-name")
		ev := NewEvent(Config{}, nil, nil)
		if ev.ServerName != "env-name" {
			t.Errorf("ServerName = %q, want env-name", ev.ServerName)
		}
	})
	t.Run("falls back to hostname", func(t *testing.T) {
		t.Setenv("SENTINEL_SERVER_NAME", "")
		ev := NewEvent(Config{}, nil, nil)
		want, _ := os.Hostname()
		if ev.ServerName != want {
			t.Errorf("ServerName = %q, want hostname %q", ev.ServerName, want)
		}
	})
}

func TestAutomaticContexts(t *testing.T) {
	ev := NewEvent(Config{}, nil, nil)
	if ev.Level != LevelError {
		t.Errorf("Level = %q, want %q", ev.Level, LevelError)
	}
	if ev.SDK == nil || ev.SDK.Name != SDKName || ev.SDK.Version != SDKVersion {
		t.Fatalf("SDK = %+v, want {%s %s}", ev.SDK, SDKName, SDKVersion)
	}
	if ev.Tags == nil {
		t.Error("Tags must be non-nil")
	}
	if ev.Metadata == nil {
		t.Error("Metadata must be non-nil even for nil ctxTags")
	}
	rt := ev.Contexts["runtime"]
	if rt["name"] != "go" {
		t.Errorf("contexts.runtime.name = %v, want go", rt["name"])
	}
	if rt["version"] == "" {
		t.Error("contexts.runtime.version must be set")
	}
	if ev.Contexts["os"]["name"] != runtime.GOOS {
		t.Errorf("contexts.os.name = %v, want %v", ev.Contexts["os"]["name"], runtime.GOOS)
	}
	if ev.Contexts["device"]["arch"] != runtime.GOARCH {
		t.Errorf("contexts.device.arch = %v, want %v", ev.Contexts["device"]["arch"], runtime.GOARCH)
	}
}

// TestAutomaticContextsInnerMapsPerEvent proves no map - outer or inner - is
// ever cached or shared between events (spec §3.2): writing into one
// event's Contexts must never be visible on another.
func TestAutomaticContextsInnerMapsPerEvent(t *testing.T) {
	cfg := Config{}
	ev1 := NewEvent(cfg, nil, nil)
	ev2 := NewEvent(cfg, nil, nil)

	ev1.Contexts["os"]["name"] = "mutated"
	ev1.Contexts["extra"] = map[string]interface{}{"k": "v"}

	if ev2.Contexts["os"]["name"] == "mutated" {
		t.Error("ev2's os context was mutated by a write to ev1's")
	}
	if _, ok := ev2.Contexts["extra"]; ok {
		t.Error("ev2 saw a context added only to ev1")
	}
}

// TestCallerSetContextWins is P1.5: the event's existing value wins over
// what ApplyContext would add, for every row except Level and
// WithFingerprint/WithErrorClass (spec §4.3).
func TestCallerSetContextWins(t *testing.T) {
	ctx := WithUserInfo(context.Background(), UserInfo{ID: "ctx-user"})
	ctx = WithTag(ctx, "region", "eu")

	ev := NewEvent(Config{}, nil, nil)
	ev.User = &UserInfo{ID: "caller-user"}
	ev.Tags["region"] = "us"

	ApplyContext(ctx, ev)

	if ev.User.ID != "caller-user" {
		t.Errorf("User.ID = %q, want caller-user (caller wins)", ev.User.ID)
	}
	if ev.Tags["region"] != "us" {
		t.Errorf("Tags[region] = %q, want us (caller wins)", ev.Tags["region"])
	}
}

// TestApplyContextMergesUserFieldByField is P1.5: a field the event's
// existing user does not have is filled from the context, field by field.
func TestApplyContextMergesUserFieldByField(t *testing.T) {
	ctx := WithUserInfo(context.Background(), UserInfo{ID: "ctx-id", Email: "ctx@example.test"})

	ev := NewEvent(Config{}, nil, nil)
	ev.User = &UserInfo{Username: "caller-name"}

	ApplyContext(ctx, ev)

	if ev.User.ID != "ctx-id" {
		t.Errorf("User.ID = %q, want ctx-id (filled from context)", ev.User.ID)
	}
	if ev.User.Email != "ctx@example.test" {
		t.Errorf("User.Email = %q, want ctx@example.test", ev.User.Email)
	}
	if ev.User.Username != "caller-name" {
		t.Errorf("User.Username = %q, want caller-name (caller wins)", ev.User.Username)
	}
}

// TestApplyContextOptionWinsOverMetadataKeys is P1.5: an SDK option
// (WithEventErrorClass/WithEventFingerprint) wins over a context metadata
// key on the same field.
func TestApplyContextOptionWinsOverMetadataKeys(t *testing.T) {
	ctx := WithTag(context.Background(), "error_class", "CtxClass")
	ctx = WithTag(ctx, "fingerprint", "ctx-fp")

	ev := NewEvent(Config{}, nil, nil, WithEventErrorClass("OptionClass"), WithEventFingerprint("option-fp"))
	ApplyContext(ctx, ev)

	if ev.ErrorClass != "OptionClass" {
		t.Errorf("ErrorClass = %q, want OptionClass (the option wins)", ev.ErrorClass)
	}
	if ev.Fingerprint != "option-fp" {
		t.Errorf("Fingerprint = %q, want option-fp (the option wins)", ev.Fingerprint)
	}
}

// TestBoundTagsKeepsTenantIDViaContext is P1.6's context half: tenant_id set
// with WithTenant survives the 50-tag cap alongside 49 other WithTag'd tags.
func TestBoundTagsKeepsTenantIDViaContext(t *testing.T) {
	ctx := WithTenant(context.Background(), "t-ctx")
	for i := 0; i < 60; i++ {
		ctx = WithTag(ctx, keyN(i), "v")
	}
	ev := NewEvent(Config{}, nil, nil)
	ApplyContext(ctx, ev)
	b := boundEvent(ev, 50)
	if b.Tags["tenant_id"] != "t-ctx" {
		t.Errorf("expected tenant_id to survive the 50-tag cap, got %v", b.Tags["tenant_id"])
	}
	if len(b.Tags) != 50 {
		t.Errorf("len(Tags) = %d, want 50", len(b.Tags))
	}
}

// TestAutomaticContextsRaceBeforeSend is P1.5: BeforeSend running
// concurrently with the worker marshalling an earlier event must never
// race, because every event's Contexts (and their inner maps) are its own.
func TestAutomaticContextsRaceBeforeSend(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	cfg := Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 512,
		BatchSize:     1,
		BatchWait:     time.Second,
		BeforeSend: func(ev *Event) *Event {
			ev.Contexts["os"]["mutated"] = true
			return ev
		},
	}
	tr := newTestTransport(t, cfg)
	for i := 0; i < 400; i++ {
		ev := NewEvent(cfg, nil, nil)
		ApplyContext(nil, ev)
		ev = cfg.ApplyBeforeSend(ev)
		tr.Push(ev)
	}
	if !tr.Flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

func keyN(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return "k" + string(letters[i%26]) + string(letters[(i/26)%26])
}
