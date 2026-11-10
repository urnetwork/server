package controller

// §7.6's SSO discovery (R-TEAM-4; TEAM-M6-06): POST /auth/sso-discover takes a
// sign-in address and, when its domain is one a team holds verified and the
// team has an IdP, begins a sign-in through that IdP (BeginNetworkSso) and
// answers the IdP's authorization request.
//
// Discovery must not say whether an unverified domain or an address exists.
// It reads only the address's domain and no user (model.NetworkSsoIdpOfEmail),
// and every address it does not route answers alike, an empty result: one
// that does not parse, a domain no network added, one added and not verified,
// one dropped, and a verified one whose team has no IdP. The limit and the
// availability checks run before the domain is read, so their refusals say
// nothing of it either.
//
// The caller's own sign-in, when the request carries one, is the flow's
// linking user (D-TEAM-23). While the process has no flow store every call
// answers 503, as BeginNetworkSso would for a routed one.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tunables of discovery's limit.
type NetworkSsoDiscoverSettings struct {
	// One caller address's discoveries in AddressLookback: 30 in 10 min, an
	// office signing in behind one address. Each routed discovery fetches the
	// IdP's discovery document, so the limit also bounds what api asks of an
	// IdP for one caller. Non-positive limits nothing.
	AddressLimit    int
	AddressLookback time.Duration
}

func DefaultNetworkSsoDiscoverSettings() *NetworkSsoDiscoverSettings {
	return &NetworkSsoDiscoverSettings{
		AddressLimit:    30,
		AddressLookback: 10 * time.Minute,
	}
}

// The prefix of discovery's IP rate-limit histories.
const networkSsoDiscoverRateLimitKeyPrefix = "sso_discover."

type NetworkSsoDiscoverArgs struct {
	// the address the person signs in with; only its domain is read
	Email string `json:"email"`
}

type NetworkSsoDiscoverResult struct {
	// the IdP's authorization request to send the browser to; absent when
	// the address's domain routes to no IdP
	AuthorizationUrl string `json:"authorization_url,omitempty"`
}

// Counts one discovery against the caller's address hash and refuses past the
// limit. A caller whose address cannot be read is not limited here.
func limitNetworkSsoDiscover(clientSession *session.ClientSession, settings *NetworkSsoDiscoverSettings) error {
	if settings.AddressLimit <= 0 {
		return nil
	}
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		if err != nil {
			return nil
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	}
	_, allowed, err := server.CheckIpRateLimitAttempt(
		clientSession.Ctx,
		rateLimitClient,
		"",
		server.NowUtc(),
		server.IpRateLimitAttemptSettings{
			KeyPrefix:       networkSsoDiscoverRateLimitKeyPrefix,
			AddressLookback: settings.AddressLookback,
			// the history holds this attempt and refuses at its limit
			AddressLimit:   settings.AddressLimit + 1,
			GlobalLookback: settings.AddressLookback,
			GlobalLimit:    settings.AddressLimit + 1,
		},
	)
	if err != nil {
		return fmt.Errorf("sso discover rate limit: %w", err)
	}
	if !allowed {
		return fmt.Errorf("%d Too many sign-in attempts. Try again later.", http.StatusTooManyRequests)
	}
	return nil
}

// POST /auth/sso-discover (§7.6).
func NetworkSsoDiscover(discover *NetworkSsoDiscoverArgs, clientSession *session.ClientSession) (*NetworkSsoDiscoverResult, error) {
	return networkSsoDiscoverWithSettings(discover, clientSession, DefaultNetworkSsoDiscoverSettings())
}

func networkSsoDiscoverWithSettings(discover *NetworkSsoDiscoverArgs, clientSession *session.ClientSession, settings *NetworkSsoDiscoverSettings) (*NetworkSsoDiscoverResult, error) {
	// before the domain is read, so neither refusal depends on it
	if model.CurrentNetworkSsoFlowStore() == nil {
		return nil, fmt.Errorf("%d Sign-in through an IdP is not available yet.", http.StatusServiceUnavailable)
	}
	if err := limitNetworkSsoDiscover(clientSession, settings); err != nil {
		return nil, err
	}
	idpId := model.NetworkSsoIdpOfEmail(clientSession.Ctx, discover.Email)
	if idpId == nil {
		return &NetworkSsoDiscoverResult{}, nil
	}
	var linkingSession *session.ClientSession
	if clientSession.ByJwt != nil {
		linkingSession = clientSession
	}
	begin, err := BeginNetworkSso(clientSession.Ctx, *idpId, linkingSession)
	if err != nil {
		return nil, err
	}
	return &NetworkSsoDiscoverResult{AuthorizationUrl: begin.AuthorizationUrl}, nil
}
