package unit

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// sentinel-event-schema-v2 spec §8.4 / §9.2 V3b: docs/sdk-specification.md §3 "Protocol Validation
// Constraints" is a table whose every numeric Bound cell names the validation.Max* constant it
// documents. The doc drifted from the code twice before (metadata was documented but unenforced; message
// was documented in bytes but counted in code points), so a prose review cannot hold it: this test
// parses the table and fails on any disagreement, in either direction.

// specLimitConstants is every exported bound constant the table must document. Go cannot look a
// constant up by name, so the name -> value map is spelled out here; a constant added to the package
// without a row (or a row naming a constant not listed here) fails the test.
var specLimitConstants = map[string]int{
	"MaxProjectKeyLength":         validation.MaxProjectKeyLength,
	"MaxPlatformLength":           validation.MaxPlatformLength,
	"MaxEnvironmentLength":        validation.MaxEnvironmentLength,
	"MaxMessageLength":            validation.MaxMessageLength,
	"MaxErrorClassLength":         validation.MaxErrorClassLength,
	"MaxTraceIDLength":            validation.MaxTraceIDLength,
	"MaxSpanIDLength":             validation.MaxSpanIDLength,
	"MaxReleaseVersionLength":     validation.MaxReleaseVersionLength,
	"MaxFingerprintLength":        validation.MaxFingerprintLength,
	"MaxEventIDLength":            validation.MaxEventIDLength,
	"MaxStacktraceFrames":         validation.MaxStacktraceFrames,
	"MaxFilePathLength":           validation.MaxFilePathLength,
	"MaxFunctionNameLength":       validation.MaxFunctionNameLength,
	"MaxMetadataSize":             validation.MaxMetadataSize,
	"MaxLoggerLength":             validation.MaxLoggerLength,
	"MaxServerNameLength":         validation.MaxServerNameLength,
	"MaxTransactionLength":        validation.MaxTransactionLength,
	"MaxDistLength":               validation.MaxDistLength,
	"MaxSourceLength":             validation.MaxSourceLength,
	"MaxSDKNameLength":            validation.MaxSDKNameLength,
	"MaxSDKVersionLength":         validation.MaxSDKVersionLength,
	"MaxUserIDBytes":              validation.MaxUserIDBytes,
	"MaxUsernameLength":           validation.MaxUsernameLength,
	"MaxUserEmailLength":          validation.MaxUserEmailLength,
	"MaxUserIPAddressLength":      validation.MaxUserIPAddressLength,
	"MaxGeoTextLength":            validation.MaxGeoTextLength,
	"MaxGeoCountryCodeLength":     validation.MaxGeoCountryCodeLength,
	"MaxTags":                     validation.MaxTags,
	"MaxTagKeyLength":             validation.MaxTagKeyLength,
	"MaxTagValueLength":           validation.MaxTagValueLength,
	"MaxContexts":                 validation.MaxContexts,
	"MaxContextKeyLength":         validation.MaxContextKeyLength,
	"MaxContextsSize":             validation.MaxContextsSize,
	"MaxRequestMethodLength":      validation.MaxRequestMethodLength,
	"MaxRequestURLLength":         validation.MaxRequestURLLength,
	"MaxRequestQueryStringLength": validation.MaxRequestQueryStringLength,
	"MaxRequestHeaders":           validation.MaxRequestHeaders,
	"MaxRequestHeaderKeyLength":   validation.MaxRequestHeaderKeyLength,
	"MaxRequestHeaderValueLength": validation.MaxRequestHeaderValueLength,
	"MaxRequestEnv":               validation.MaxRequestEnv,
	"MaxRequestEnvKeyLength":      validation.MaxRequestEnvKeyLength,
	"MaxRequestEnvValueLength":    validation.MaxRequestEnvValueLength,
	"MaxRequestDataSize":          validation.MaxRequestDataSize,
	"MaxBreadcrumbs":              validation.MaxBreadcrumbs,
	"MaxBreadcrumbsSize":          validation.MaxBreadcrumbsSize,
	"MaxBreadcrumbTypeLength":     validation.MaxBreadcrumbTypeLength,
	"MaxBreadcrumbCategoryLength": validation.MaxBreadcrumbCategoryLength,
	"MaxBreadcrumbMessageLength":  validation.MaxBreadcrumbMessageLength,
	"MaxBreadcrumbDataSize":       validation.MaxBreadcrumbDataSize,
	"MaxExceptionValues":          validation.MaxExceptionValues,
	"MaxExceptionTypeLength":      validation.MaxExceptionTypeLength,
	"MaxExceptionValueLength":     validation.MaxExceptionValueLength,
	"MaxMechanismTypeLength":      validation.MaxMechanismTypeLength,
	"MaxExceptionFrames":          validation.MaxExceptionFrames,
	"MaxFrameModuleLength":        validation.MaxFrameModuleLength,
	"MaxFrameAbsPathLength":       validation.MaxFrameAbsPathLength,
	"MaxFrameContextLineLength":   validation.MaxFrameContextLineLength,
	"MaxFrameContextLines":        validation.MaxFrameContextLines,
	"MaxServerTruncatedKeys":      validation.MaxServerTruncatedKeys,
	"MaxServerTruncatedKeyBytes":  validation.MaxServerTruncatedKeyBytes,
}

func readSDKSpec(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "sdk-specification.md"))
	require.NoError(t, err)
	return string(b)
}

// sdkSpecSection returns the lines after the heading line `heading`, up to the next heading of the same
// or a higher level, or the next `---` rule.
func sdkSpecSection(t *testing.T, doc, heading string) string {
	t.Helper()
	level := strings.Index(heading, " ")
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if line != heading {
			continue
		}
		var out []string
		for _, l := range lines[i+1:] {
			hashes := len(l) - len(strings.TrimLeft(l, "#"))
			if (hashes > 0 && hashes <= level && strings.HasPrefix(l[hashes:], " ")) || strings.HasPrefix(l, "---") {
				break
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("docs/sdk-specification.md has no %q heading", heading)
	return ""
}

var boundCellPattern = regexp.MustCompile("(\\d+) \\(`(Max\\w+)`\\)")

func TestSDKSpecLimits_TableMatchesValidationConstants(t *testing.T) {
	section := sdkSpecSection(t, readSDKSpec(t), "### Protocol Validation Constraints")

	documented := map[string]bool{}
	rows := 0
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Field") {
			continue
		}
		cells := strings.Split(line, " | ")
		require.GreaterOrEqual(t, len(cells), 5, "a limits row must have the five columns: %q", line)
		rows++
		bound := cells[1]
		matches := boundCellPattern.FindAllStringSubmatch(bound, -1)
		if len(matches) == 0 {
			// A row with no numeric bound (level, column >= 0, the timestamp window, NUL) names no constant,
			// and must not carry a stray number that looks like one.
			assert.NotContains(t, bound, "`Max", "row %q names a constant in a form the parser cannot read", cells[0])
			continue
		}
		require.Len(t, matches, 1, "one constant per row: %q", line)
		name := matches[0][2]
		value, err := strconv.Atoi(matches[0][1])
		require.NoError(t, err)
		want, ok := specLimitConstants[name]
		if assert.True(t, ok, "row %q names %s, which is not an exported validation constant listed by this test", cells[0], name) {
			assert.Equal(t, want, value, "row %q: the doc says %d, validation.%s is %d", cells[0], value, name, want)
		}
		documented[name] = true
	}
	require.Greater(t, rows, 40, "the limits table was not found or not parsed")

	for name := range specLimitConstants {
		assert.True(t, documented[name], "validation.%s has no row in docs/sdk-specification.md §3", name)
	}
}

// The doc must describe the bounds in the units the code measures them in. Two historical drifts are
// pinned directly: message is code points (not bytes), and the metadata bound is serialized bytes.
func TestSDKSpecLimits_UnitsMatchTheCode(t *testing.T) {
	section := sdkSpecSection(t, readSDKSpec(t), "### Protocol Validation Constraints")
	unitOf := func(field string) string {
		for _, line := range strings.Split(section, "\n") {
			if strings.HasPrefix(line, "| `"+field+"` |") {
				return strings.Split(line, " | ")[2]
			}
		}
		t.Fatalf("no row for %s", field)
		return ""
	}
	assert.Equal(t, "code points", unitOf("message"))
	assert.Equal(t, "code points", unitOf("fingerprint"))
	assert.Equal(t, "bytes", unitOf("metadata"))
	assert.Equal(t, "bytes", unitOf("user.id"))
}

// The doc's vocabulary line is the closed set the code emits, in the code's order.
func TestSDKSpecLimits_VocabularyMatchesObs(t *testing.T) {
	section := sdkSpecSection(t, readSDKSpec(t), "### Protocol Validation Constraints")
	idx := strings.Index(section, "**The vocabulary**")
	require.GreaterOrEqual(t, idx, 0)
	m := regexp.MustCompile("\n`([^`]+)`").FindStringSubmatch(section[idx:])
	require.NotNil(t, m, "the vocabulary line must follow the **The vocabulary** paragraph")
	assert.Equal(t, obs.TruncatedFieldVocabulary(), strings.Fields(m[1]))
	assert.Contains(t, section, "`metadata."+validation.MetadataKeyServerTruncated+"`")
	assert.Contains(t, section, "`metadata."+validation.MetadataKeyServerTruncatedKeys+"`")
}

// V8d (sentinel-event-schema-v2 §8.4, §9.2): docs §5 "Mandatory Masking Keys" lists exactly the keys the
// server's masker redacts, and §5 names the card-number token. The key list is the one line of the
// section made only of backticked keys.
func TestSDKSpecMasking_KeysMatchTheMasker(t *testing.T) {
	doc := readSDKSpec(t)
	section := sdkSpecSection(t, doc, "### Mandatory Masking Keys")
	var documented []string
	for _, line := range strings.Split(section, "\n") {
		keys := regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(line, -1)
		if len(keys) == 0 || strings.TrimSpace(regexp.MustCompile("`[^`]+`").ReplaceAllString(line, "")) != "" {
			continue
		}
		for _, k := range keys {
			documented = append(documented, k[1])
		}
	}
	assert.Equal(t, masker.SensitiveKeys(), documented, "docs §5 must list masker.SensitiveKeys(), sorted")

	security := sdkSpecSection(t, doc, "## 5. Security & PII Sanitization Rules")
	assert.Contains(t, security, "`"+masker.PANToken+"`")
	assert.Contains(t, security, "***REDACTED***")
}
