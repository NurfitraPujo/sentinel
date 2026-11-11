package sentrycompat

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// The relay identity headers (spec §3.7, contract §4.7 D12): core's mobile relay tells the ingestor who the app
// user was, and the ingestor believes it only when the request authenticated with a `relay` key (sentinel-relay-key-trust;
// this replaced the trusted-peer network test of D12/D15). Nothing outside this package reads them.
const (
	HeaderRelayAuth     = "X-Sentinel-Relay-Auth"
	HeaderRelayUserID   = "X-Sentinel-Relay-User-Id"
	HeaderRelayTenantID = "X-Sentinel-Relay-Tenant-Id"

	RelayAuthUser      = "user"
	RelayAuthAnonymous = "anonymous"

	TagTenantID = "tenant_id"
	// TagRelayAuth is the tag the relay's auth value is written to. A body-supplied one is never kept (D14):
	// MapEvent drops it, so its presence on a published compat event means core's relay asserted it.
	TagRelayAuth = "relay_auth"
)

// metadataIdentityKeys are the six metadata keys the processor falls back to for the affected user and tenant
// (event.go readAffectedIDs: indexer.UserIDFromMetadata, indexer.TenantIDFromMetadata). Without removing them an
// anonymous event would still be attributed through `extra`.
var metadataIdentityKeys = []string{"user_id", "userId", "user", "tenant_id", "tenantId", "organization_id"}

// RelayIdentity is what the relay asserted about the app user, when it may assert anything.
type RelayIdentity struct {
	Auth     string // "user" or "anonymous"; "" means: apply nothing
	UserID   string // set only when Auth == "user" and the header is non-empty
	TenantID string // set only when Auth == "user" and the header is non-empty
}

func headerValue(r *http.Request, name string) string {
	return strings.TrimSpace(stripNULs(r.Header.Get(name)))
}

// RelayIdentityFromRequest returns the zero RelayIdentity unless the authenticated key's scope is auth.ScopeRelay
// (sentinel-relay-key-trust §4.2, K1); then it reads the three headers (spec §3.7.3), whatever the peer. From any
// other key they are ignored and, when one was sent, one line records the key's scope. logger is Config.Logger.
func RelayIdentityFromRequest(r *http.Request, logger *slog.Logger) RelayIdentity {
	if logger == nil {
		logger = slog.Default()
	}
	scope, _ := auth.ScopeFromContext(r.Context())
	if scope != auth.ScopeRelay {
		if len(r.Header.Values(HeaderRelayAuth))+len(r.Header.Values(HeaderRelayUserID))+len(r.Header.Values(HeaderRelayTenantID)) > 0 {
			// The key's scope only: never a header value, a forwarded address, a peer or a key.
			logger.InfoContext(r.Context(), "relay identity headers ignored: the key is not a relay key",
				slog.String(obs.LogKeyEvent, "sentrycompat.relay_headers_ignored"),
				slog.String("reason", "not_relay_key"),
				slog.String("scope", scope))
		}
		return RelayIdentity{}
	}
	raw := headerValue(r, HeaderRelayAuth)
	switch strings.ToLower(raw) {
	case RelayAuthAnonymous:
		return RelayIdentity{Auth: RelayAuthAnonymous}
	case RelayAuthUser:
		return RelayIdentity{
			Auth:     RelayAuthUser,
			UserID:   headerValue(r, HeaderRelayUserID),
			TenantID: headerValue(r, HeaderRelayTenantID),
		}
	case "":
		return RelayIdentity{}
	}
	// A relay key sending anything else is misconfigured, but refusing the envelope would lose the event.
	logger.WarnContext(r.Context(), "relay identity header has an invalid value; nothing applied",
		slog.String(obs.LogKeyEvent, "sentrycompat.relay_auth_invalid"))
	return RelayIdentity{}
}

// Apply writes the relay's assertion onto a mapped event (spec §3.7.4, D12, D14), right after MapEvent and
// before Config.Scope. MapEvent has already dropped a body tag relay_auth, so tags.relay_auth on a published
// compat event is only ever a value written here. With Auth == "" nothing changes.
func (id RelayIdentity) Apply(p *validation.ErrorPayload) {
	if id.Auth == "" {
		return
	}
	if p.Tags == nil {
		p.Tags = map[string]string{}
	}
	relayKeys := map[string]bool{TagRelayAuth: true}
	switch id.Auth {
	case RelayAuthUser:
		if id.UserID != "" {
			if p.User == nil {
				p.User = &validation.UserInfo{}
			}
			p.User.ID = id.UserID
		}
		if id.TenantID != "" {
			p.Tags[TagTenantID] = id.TenantID
			relayKeys[TagTenantID] = true
		}
	case RelayAuthAnonymous:
		// The anonymous route is open to anyone: the event is attributed to no user and no tenant.
		if p.User != nil {
			p.User.ID = ""
		}
		delete(p.Tags, TagTenantID)
		for _, k := range metadataIdentityKeys {
			delete(p.Metadata, k)
		}
	}
	p.Tags[TagRelayAuth] = id.Auth

	// Room for the relay's tags: the bounds step keeps the 50 lexically SMALLEST keys, which would cut
	// relay_auth and tenant_id first on a tag-heavy event. Identity wins: drop the largest other keys.
	if len(p.Tags) > validation.MaxTags {
		var others []string
		for k := range p.Tags {
			if !relayKeys[k] {
				others = append(others, k)
			}
		}
		sort.Strings(others)
		for i := len(others) - 1; i >= 0 && len(p.Tags) > validation.MaxTags; i-- {
			delete(p.Tags, others[i])
		}
	}
}
