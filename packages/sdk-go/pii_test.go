package sentinel

import (
	"testing"
	"time"
)

func timeoutChan() <-chan time.Time {
	return time.After(2 * time.Second)
}

func TestScrubPII(t *testing.T) {
	input := map[string]interface{}{
		"user_id":       "usr_123",
		"password":      "secret123",
		"Authorization": "Bearer token123",
		"credit_card":   "4111222233334444",
		"nested": map[string]interface{}{
			"api_key": "key_999",
			"safe":    "hello",
		},
	}

	scrubbed := ScrubPII(input)

	if scrubbed["password"] != "[FILTERED]" {
		t.Errorf("expected password to be filtered, got %v", scrubbed["password"])
	}
	if scrubbed["Authorization"] != "[FILTERED]" {
		t.Errorf("expected Authorization to be filtered, got %v", scrubbed["Authorization"])
	}
	if scrubbed["credit_card"] != "[FILTERED]" {
		t.Errorf("expected credit_card to be filtered, got %v", scrubbed["credit_card"])
	}
	if scrubbed["user_id"] != "usr_123" {
		t.Errorf("expected user_id to be preserved, got %v", scrubbed["user_id"])
	}

	nested := scrubbed["nested"].(map[string]interface{})
	if nested["api_key"] != "[FILTERED]" {
		t.Errorf("expected nested api_key to be filtered, got %v", nested["api_key"])
	}
	if nested["safe"] != "hello" {
		t.Errorf("expected nested safe value to be preserved, got %v", nested["safe"])
	}
}

// TestScrubPIIDescendsIntoLists proves ScrubPII now descends through
// []interface{} as well as maps, at any depth (spec §6.2 item 3, T37).
func TestScrubPIIDescendsIntoLists(t *testing.T) {
	input := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"password": "in-array"},
			[]interface{}{map[string]interface{}{"api_key": "nested-list"}},
		},
	}
	scrubbed := ScrubPII(input)
	items := scrubbed["items"].([]interface{})
	first := items[0].(map[string]interface{})
	if first["password"] != "[FILTERED]" {
		t.Errorf("expected password inside a list to be filtered, got %v", first["password"])
	}
	nestedList := items[1].([]interface{})
	nestedMap := nestedList[0].(map[string]interface{})
	if nestedMap["api_key"] != "[FILTERED]" {
		t.Errorf("expected api_key inside a nested list to be filtered, got %v", nestedMap["api_key"])
	}

	// The original must be untouched (a deep copy, not an in-place scrub).
	origItems := input["items"].([]interface{})
	origFirst := origItems[0].(map[string]interface{})
	if origFirst["password"] != "in-array" {
		t.Error("ScrubPII must not mutate its input")
	}
}

// TestScrubPIICycleSafe proves a self-holding map is returned shared rather
// than overflowing the stack (spec §6.2 item 3, T37).
func TestScrubPIICycleSafe(t *testing.T) {
	cyclic := map[string]interface{}{"safe": "v"}
	cyclic["self"] = cyclic

	done := make(chan map[string]interface{}, 1)
	go func() {
		done <- ScrubPII(cyclic)
	}()
	select {
	case scrubbed := <-done:
		if scrubbed["safe"] != "v" {
			t.Errorf("expected safe key preserved, got %v", scrubbed["safe"])
		}
		if _, ok := scrubbed["self"].(map[string]interface{}); !ok {
			t.Error("expected the cyclic key to still be a map")
		}
	case <-timeoutChan():
		t.Fatal("ScrubPII did not return: likely an infinite recursion on a cycle")
	}
}

// TestPIIKeysCoverSpecList is P3.9: the SDK's PII key list must be a superset
// of docs/sdk-specification.md §5's Mandatory Masking Keys (spec §6.2 item
// 3, T14).
func TestPIIKeysCoverSpecList(t *testing.T) {
	specKeys := []string{
		"access_token", "api-key", "api_key", "apikey", "auth", "authorization",
		"bearer", "card_number", "client_secret", "cookie", "credential",
		"credentials", "credit_card", "cvv", "passwd", "password", "private_key",
		"pwd", "refresh_token", "secret", "social_security", "ssn", "token",
	}
	for _, k := range specKeys {
		if !isSensitiveKey(k) {
			t.Errorf("defaultPIIKeys must cover %q (docs/sdk-specification.md §5)", k)
		}
	}
	// Explicitly named as covered by the server too:
	for _, k := range []string{"Set-Cookie", "X-API-Key"} {
		if !isSensitiveKey(k) {
			t.Errorf("defaultPIIKeys must cover %q", k)
		}
	}
}
