package sentinel

import (
	"context"
	"testing"
)

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	ctx = WithUser(ctx, "usr_100")
	ctx = WithTenant(ctx, "org_acme")
	ctx = WithTag(ctx, "request_id", "req_999")

	tags := getTagsMap(ctx)
	if tags["user_id"] != "usr_100" {
		t.Errorf("expected user_id usr_100, got %v", tags["user_id"])
	}
	if tags["tenant_id"] != "org_acme" {
		t.Errorf("expected tenant_id org_acme, got %v", tags["tenant_id"])
	}
	if tags["request_id"] != "req_999" {
		t.Errorf("expected request_id req_999, got %v", tags["request_id"])
	}
}
