package sentinel

import "testing"

func TestNormalizeLevel(t *testing.T) {
	cases := map[string]string{
		"fatal":    "fatal",
		"error":    "error",
		"warning":  "warning",
		"info":     "info",
		"debug":    "debug",
		"warn":     "warning",
		"panic":    "fatal",
		"trace":    "debug",
		"ERROR":    "error",
		" Warn ":   "warning",
		"critical": "",
		"err":      "",
		"notice":   "",
		"":         "",
	}
	for in, want := range cases {
		if got := NormalizeLevel(in); got != want {
			t.Errorf("NormalizeLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
