package sentinel

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidUTF8ReplacesOneU00FDPerInvalidByte(t *testing.T) {
	in := "a\xfe\xffb"
	out := validUTF8(in)
	want := "a��b"
	if out != want {
		t.Errorf("validUTF8(%q) = %q, want %q", in, out, want)
	}
}

func TestBoundEventTypedRows(t *testing.T) {
	longClass := strings.Repeat("c", 300)
	ev := &Event{
		EventID:        "",
		ProjectKey:     "proj",
		Platform:       "not valid!!",
		ErrorClass:     longClass,
		Message:        strings.Repeat("m", 10_050),
		Timestamp:      time.Now().UTC(),
		Environment:    "Prod EU",
		ReleaseVersion: strings.Repeat("r", 150),
		Fingerprint:    strings.Repeat("f", 100),
		TraceID:        strings.Repeat("t", 100),
		SpanID:         strings.Repeat("s", 100),
		Level:          "bogus",
		ServerName:     strings.Repeat("h", 300),
		Transaction:    strings.Repeat("x", 250),
		SDK:            &SDKInfo{Name: strings.Repeat("n", 200), Version: strings.Repeat("v", 100)},
		User: &UserInfo{
			ID:        strings.Repeat("i", 300),
			Username:  strings.Repeat("u", 200),
			Email:     strings.Repeat("e", 300),
			IPAddress: strings.Repeat("p", 100),
		},
		Stacktrace: make([]Frame, 150),
	}
	for i := range ev.Stacktrace {
		ev.Stacktrace[i] = Frame{Function: "f", File: fmt.Sprintf("file%d.go", i), Line: 1}
	}

	b := boundEvent(ev, 50)

	if b.EventID == "" {
		t.Error("empty event_id must be re-minted")
	}
	if b.Platform != "go" {
		t.Errorf("Platform = %q, want go", b.Platform)
	}
	if b.ErrorClass != longClass[:255] {
		t.Errorf("ErrorClass not truncated to 255")
	}
	if len([]rune(b.Message)) != 10000 {
		t.Errorf("Message len = %d, want 10000", len([]rune(b.Message)))
	}
	if b.Environment != "prod-eu" {
		t.Errorf("Environment = %q, want prod-eu", b.Environment)
	}
	if len([]rune(b.ReleaseVersion)) != 100 {
		t.Errorf("ReleaseVersion not truncated to 100")
	}
	if b.Fingerprint != "" {
		t.Error("over-long fingerprint must be dropped, not truncated")
	}
	if b.TraceID != "" || b.SpanID != "" {
		t.Error("over-long trace/span ids must be dropped")
	}
	if b.Level != "" {
		t.Errorf("invalid level must become absent, got %q", b.Level)
	}
	if len([]rune(b.ServerName)) != 256 {
		t.Error("ServerName not truncated to 256")
	}
	if len([]rune(b.Transaction)) != 200 {
		t.Error("Transaction not truncated to 200")
	}
	if len([]rune(b.SDK.Name)) != 128 || len([]rune(b.SDK.Version)) != 64 {
		t.Error("SDK name/version not truncated")
	}
	if len(b.User.ID) != 0 {
		t.Errorf("User.ID over 255 bytes must be dropped, got len %d", len(b.User.ID))
	}
	if len([]rune(b.User.Username)) != 128 {
		t.Error("Username not truncated to 128")
	}
	if b.User.Email != "" || b.User.IPAddress != "" {
		t.Error("over-long email/ip must be dropped")
	}
	if len(b.Stacktrace) != 100 {
		t.Errorf("Stacktrace len = %d, want 100", len(b.Stacktrace))
	}

	// The pushed event itself must never be written.
	if ev.EventID != "" {
		t.Error("boundEvent must not write into the pushed event")
	}
	if ev.ErrorClass != longClass {
		t.Error("boundEvent must not write into the pushed event's ErrorClass")
	}
}

func TestBoundEventEmptyErrorClassBecomesError(t *testing.T) {
	ev := &Event{ErrorClass: ""}
	b := boundEvent(ev, 50)
	if b.ErrorClass != "Error" {
		t.Errorf("ErrorClass = %q, want Error", b.ErrorClass)
	}
}

func TestBoundEventTimestampYearOutOfRange(t *testing.T) {
	ev := &Event{Timestamp: time.Unix(1e15, 0)} // year ~58,706
	b := boundEvent(ev, 50)
	if b.Timestamp.Year() < 2000 {
		t.Errorf("out-of-range timestamp must become now-ish, got %v", b.Timestamp)
	}
}

func TestBoundEventInAppFrameEmptyFileClearsInApp(t *testing.T) {
	ev := &Event{Stacktrace: []Frame{{Function: "f", File: "", InApp: true}}}
	b := boundEvent(ev, 50)
	if b.Stacktrace[0].InApp {
		t.Error("an in-app frame with an empty file must have InApp cleared")
	}
}

// TestBoundTagsKeepsTenantID is P1.6's direct-Event.Tags half: tenant_id
// plus k00...k49 assigned directly on the event survives the 50-tag cap
// (the context-derived half lands in C4).
func TestBoundTagsKeepsTenantID(t *testing.T) {
	tags := map[string]string{"tenant_id": "t-1"}
	for i := 0; i < 50; i++ {
		tags[fmt.Sprintf("k%02d", i)] = "v"
	}
	ev := &Event{Tags: tags}
	b := boundEvent(ev, 50)
	if len(b.Tags) != 50 {
		t.Fatalf("len(Tags) = %d, want 50", len(b.Tags))
	}
	if b.Tags["tenant_id"] != "t-1" {
		t.Error("tenant_id must survive the 50-tag cap")
	}
}

func TestBoundTagsDropsInvalidEntries(t *testing.T) {
	tags := map[string]string{
		"ok":                    "value",
		"":                      "value",
		strings.Repeat("k", 33): "value",
		"empty":                 "",
		"newline":               "a\nb",
		"toolong":               strings.Repeat("v", 201),
	}
	b := boundEvent(&Event{Tags: tags}, 50)
	if _, ok := b.Tags["ok"]; !ok {
		t.Error("valid tag must survive")
	}
	for _, k := range []string{"", strings.Repeat("k", 33), "empty", "newline", "toolong"} {
		if _, ok := b.Tags[k]; ok {
			t.Errorf("invalid tag %q must be dropped", k)
		}
	}
}

func TestValidTag(t *testing.T) {
	cases := []struct {
		key, value string
		want       bool
	}{
		{"region", "eu", true},
		{strings.Repeat("k", 32), "v", true},
		{strings.Repeat("k", 33), "v", false},
		{"has space", "v", false},
		{"k", "", false},
		{"k", strings.Repeat("v", 200), true},
		{"k", strings.Repeat("v", 201), false},
		{"k", "a\nb", false},
	}
	for _, c := range cases {
		if got := validTag(c.key, c.value); got != c.want {
			t.Errorf("validTag(%q, %q) = %v, want %v", c.key, c.value, got, c.want)
		}
	}
}

// TestBoundTagsDropsSensitiveKeyThatFailsTheKeyPattern is bounds.go:576:
// a sensitive key still has to match ^[a-zA-Z0-9_.:-]{1,32}$ before it is
// kept (as "[FILTERED]") - the sensitive-key branch used to run before, and
// instead of, that check, so a sensitive key over 32 characters or with an
// invalid character was kept and reported filtered, bypassing the same
// key-shape bound every non-sensitive tag key is held to.
func TestBoundTagsDropsSensitiveKeyThatFailsTheKeyPattern(t *testing.T) {
	tags := map[string]string{
		"password":                    "hunter2", // sensitive, valid key -> kept, filtered
		strings.Repeat("password", 5): "hunter2", // sensitive, over 32 chars -> dropped
		"password key":                "hunter2", // sensitive, invalid character -> dropped
		"region":                      "eu",      // ordinary, valid key -> kept as-is
	}
	got := boundTags(tags)

	if v, ok := got["password"]; !ok || v != "[FILTERED]" {
		t.Errorf(`got["password"] = %q, %v, want "[FILTERED]", true`, v, ok)
	}
	if v, ok := got["region"]; !ok || v != "eu" {
		t.Errorf(`got["region"] = %q, %v, want "eu", true`, v, ok)
	}
	if _, ok := got[strings.Repeat("password", 5)]; ok {
		t.Errorf("expected the over-length sensitive key dropped, got it kept: %v", got)
	}
	if _, ok := got["password key"]; ok {
		t.Errorf("expected the invalid-character sensitive key dropped, got it kept: %v", got)
	}
}
