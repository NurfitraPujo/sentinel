package sentinel

import (
	"reflect"
	"strings"
)

func mapPointer(m map[string]interface{}) uintptr {
	return reflect.ValueOf(m).Pointer()
}

func sliceKey(s []interface{}) unsafeSliceKey {
	return unsafeSliceKey{ptr: reflect.ValueOf(s).Pointer(), len: len(s)}
}

// defaultPIIKeys are matched as SUBSTRINGS of the (lowercased) metadata key.
//
// The list must cover every key named in docs/sdk-specification.md section 5 ("Mandatory Masking
// Keys") and specs/007-go-client-sdk/security-constraints.md. It previously omitted pass, passwd,
// bearer, card_number, cvv and social_security. That gap was dormant only because the SDK emitted
// its scrubbed map under the JSON key `context`, which the ingestor never decoded — so no client
// metadata reached Postgres at all. Renaming it to `metadata` (the S4 fix) made the gap live.
var defaultPIIKeys = []string{
	"password",
	"passwd",
	"pwd",
	"pass",
	"token",
	"bearer",
	"secret",
	"authorization",
	"auth",
	"credit_card",
	"card_number",
	"cvv",
	"ssn",
	"social_security",
	"api_key",
	"apikey",
	"api-key",
	"cookie",
	"credential",
	"private_key",
}

// ScrubPII returns a deep copy of metadata with every sensitive key's value
// replaced by "[FILTERED]", descending through both map[string]interface{}
// values and []interface{} values, at any depth and through either - a map
// inside a list, a list inside a map or a list (spec §6.2 item 3, T37).
// Every other value (a slice of any other element type, a map[string]string,
// a pointer, a struct) is copied by reference: copying those would need
// reflection over caller types.
//
// A map or list already on the current recursion path is copied by
// reference instead of walked again, so a self-holding value returns
// (shared) rather than recursing forever.
func ScrubPII(metadata map[string]interface{}) map[string]interface{} {
	if metadata == nil {
		return nil
	}
	return scrubMap(metadata, map[uintptr]bool{}, map[unsafeSliceKey]bool{})
}

// unsafeSliceKey identifies a []interface{} by its backing array pointer and
// length, mirroring how encoding/json detects a cycle in a slice.
type unsafeSliceKey struct {
	ptr uintptr
	len int
}

func scrubMap(m map[string]interface{}, onMapPath map[uintptr]bool, onSlicePath map[unsafeSliceKey]bool) map[string]interface{} {
	ptr := mapPointer(m)
	if onMapPath[ptr] {
		return m
	}
	onMapPath[ptr] = true
	defer delete(onMapPath, ptr)

	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		if isSensitiveKey(k) {
			result[k] = "[FILTERED]"
			continue
		}
		result[k] = scrubValue(v, onMapPath, onSlicePath)
	}
	return result
}

func scrubValue(v interface{}, onMapPath map[uintptr]bool, onSlicePath map[unsafeSliceKey]bool) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return scrubMap(val, onMapPath, onSlicePath)
	case []interface{}:
		return scrubSlice(val, onMapPath, onSlicePath)
	default:
		return v
	}
}

func scrubSlice(s []interface{}, onMapPath map[uintptr]bool, onSlicePath map[unsafeSliceKey]bool) []interface{} {
	key := sliceKey(s)
	if onSlicePath[key] {
		return s
	}
	onSlicePath[key] = true
	defer delete(onSlicePath, key)

	out := make([]interface{}, len(s))
	for i, v := range s {
		out[i] = scrubValue(v, onMapPath, onSlicePath)
	}
	return out
}

func isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	for _, piiKey := range defaultPIIKeys {
		if strings.Contains(lowerKey, piiKey) {
			return true
		}
	}
	return false
}
