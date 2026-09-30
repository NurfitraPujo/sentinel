package sentrycompat

import (
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// The relay identity headers (spec §3.7, contract §4.7 D12): core's mobile relay tells the ingestor who the app
// user was, and the ingestor believes it only when the connection's immediate peer is inside a trusted-proxy
// prefix (the compat resolver's list, shipped empty, D15). Nothing outside this package reads them.
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

// RelayIdentityFromRequest returns the zero RelayIdentity unless clientip.RequestInfoFromContext(r.Context()) has
// PeerTrusted; then it reads the three headers (spec §3.7.3). logger is Config.Logger.
func RelayIdentityFromRequest(r *http.Request, logger *slog.Logger) RelayIdentity {
	if logger == nil {
		logger = slog.Default()
	}
	info, _ := clientip.RequestInfoFromContext(r.Context())
	if !info.PeerTrusted {
		if len(r.Header.Values(HeaderRelayAuth)) > 0 {
			// How an operator observes the relay's immediate peer before setting the trusted list (RUNBOOK):
			// the peer address only, never a header value, a forwarded address or a key.
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			logger.InfoContext(r.Context(), "relay identity headers ignored: the peer is not a trusted proxy",
				slog.String(obs.LogKeyEvent, "sentrycompat.relay_peer_untrusted"),
				slog.String("peer", host))
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
	// A trusted relay sending anything else is misconfigured, but refusing the envelope would lose the event.
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
