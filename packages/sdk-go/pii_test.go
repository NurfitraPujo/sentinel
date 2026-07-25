package sentinel

import "testing"

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
