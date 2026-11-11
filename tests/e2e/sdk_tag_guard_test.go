//go:build !e2e

package e2e

import "testing"

// TestSDKRowsRequireTheE2ETag fails when the suite is run without `-tags=e2e`, which silently excludes
// sdk_test.go and with it matrix rows U8, U9 and U10.
//
// sdk_test.go carries `//go:build e2e` because it imports packages/sdk-go, a separate module reachable
// only in workspace mode — see the long comment at the top of that file. The consequence is a suite that
// reports "ok" having skipped three rows, and nothing in Go's output says so: an excluded file leaves no
// trace at all, unlike a skipped test.
//
// That is the failure mode P0-4 exists to prevent, and it is why this file is the inverse of a skip: under
// SENTINEL_E2E=1 it is a hard failure, so a CI job that forgets the tag reports a missing SDK path instead
// of a green run. Locally, without SENTINEL_E2E, it is a skip carrying the same explanation.
func TestSDKRowsRequireTheE2ETag(t *testing.T) {
	const msg = "matrix rows U8-U10 (real SDK against the real ingestor) were NOT run: sdk_test.go is " +
		"excluded without -tags=e2e. Use: SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/..."
	if e2eRequired {
		t.Fatal(msg)
	}
	t.Skip(msg)
}
