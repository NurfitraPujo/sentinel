package main

import "runtime/debug"

// version is stamped at build time: go build -ldflags "-X main.version=v0.2.41". When empty (a plain
// `go build`, or `go install ...@version`), buildVersion falls back to the module info the Go
// toolchain embeds, and finally to "dev".
var version = ""

func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	return versionFrom(version, bi, ok)
}

// versionFrom is the pure form of buildVersion so tests can exercise every branch.
func versionFrom(ldflag string, bi *debug.BuildInfo, ok bool) string {
	if ldflag != "" {
		return ldflag
	}
	if !ok || bi == nil {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		return "dev-" + rev + dirty
	}
	return "dev"
}

func userAgent() string { return "sentinel/" + buildVersion() }
