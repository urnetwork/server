package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The lag API's refusals (lag.dev/PLAN.md A1, A6). A refusal is a normal
// answer carrying `error`, so the browser reads the code and the wait
// without parsing an http error.
type LagError struct {
	// limit or unavailable
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	// how long to wait before asking again; 0 when there is no wait to name
	RetryAfterMs int64 `json:"retry_after_ms"`
}

const (
	// a rate limit: per /24 (/48), or per client
	LagErrorCodeLimit = "limit"
	// the operator cannot answer now, or there is nothing to answer with
	LagErrorCodeUnavailable = "unavailable"
)

// The guest sessions counted per /24 (/48).
const lagSessionRateLimitName = "session"

// The description and device spec of a lag guest's client.
const lagGuestClientDescription = "lag.dev"

// A guest session (A1): the jwt of the guest's one client, which lag.dev
// keeps in local storage and reuses until `expires_at` (L11).
type LagSessionResult struct {
	ByClientJwt string     `json:"by_client_jwt,omitempty"`
	NetworkId   *server.Id `json:"network_id,omitempty"`
	ClientId    *server.Id `json:"client_id,omitempty"`
	// RFC 3339, whole seconds, utc
	ExpiresAt string    `json:"expires_at,omitempty"`
	Error     *LagError `json:"error,omitempty"`
}

// Answers `POST /lag/session` (A1): creates a guest network marked as lag
// with the configured expiry and one client, with the model functions the
// network create and auth-client routes use, and returns the client's jwt.
// Creation is limited per /24 (/48) of the caller.
//
// The jwt is an ordinary client token. Nothing about it expires at
// `expires_at`; the guest sweep (A2) removes the network then, and a removed
// network fails ValidateByJwtState on the token's next use.
func LagSession(clientSession *session.ClientSession) (result *LagSessionResult, err error) {
	// every answer counts by its outcome (X9, lag_metrics.go)
	defer func() {
		countLagSession(result, err)
	}()
	settings := GetLagSettings()
	ctx := clientSession.Ctx

	addr, _, err := clientSession.ParseClientIpPort()
	if err != nil {
		// the parse error would quote the address, so it goes no further
		// (amendment 9)
		return nil, errors.New("could not read the caller address")
	}
	limit, err := model.CheckLagPrefixRateLimit(
		ctx,
		lagSessionRateLimitName,
		addr,
		settings.GuestLimit,
		settings.GuestLimitWindow,
	)
	if err != nil {
		return nil, err
	}
	if !limit.Allowed {
		return &LagSessionResult{
			Error: &LagError{
				Code: LagErrorCodeLimit,
				Message: "Too many lag.dev sessions have started recently from your network. " +
					"The limit is shared by everyone on it, so this may not be your own activity.",
				RetryAfterMs: limit.RetryAfter.Milliseconds(),
			},
		}, nil
	}

	now := server.NowUtc()
	// whole seconds, so the expiry stored is exactly the one answered
	expiresAt := now.Add(settings.GuestTtl).Truncate(time.Second)
	guest := model.CreateLagGuestNetwork(ctx, now, expiresAt)

	unavailable := func(cause string) (*LagSessionResult, error) {
		glog.Infof("[lag]guest session %s not created: %s\n", guest.NetworkId, cause)
		model.RemoveLagGuest(ctx, guest.NetworkId)
		return &LagSessionResult{
			Error: &LagError{
				Code:    LagErrorCodeUnavailable,
				Message: "Could not start a lag.dev session. Try again shortly.",
				// a moment, not a window: nothing was counted against the
				// caller but this one attempt
				RetryAfterMs: 5000,
			},
		}, nil
	}

	// the network's session, which the client token shares when sessions
	// are on, as for a signup followed by an auth-client call. Both carry the
	// lag guest claim, which limits the client token to the lag routes and
	// keeps it off the connect transport
	byJwt := session.NewByJwt(guest.NetworkId, guest.UserId, guest.NetworkName, false, false)
	byJwt.LagGuest = true
	if _, err := session.MintNetworkSession(ctx, byJwt, "lag"); err != nil {
		return unavailable(err.Error())
	}
	authClient, err := model.AuthNetworkClient(
		&model.AuthNetworkClientArgs{
			Description: lagGuestClientDescription,
			DeviceSpec:  lagGuestClientDescription,
		},
		clientSession.WithByJwt(byJwt),
	)
	switch {
	case err != nil:
		return unavailable(err.Error())
	case authClient.Error != nil:
		return unavailable(authClient.Error.Message)
	case authClient.ByClientJwt == nil || authClient.ClientId == nil:
		return unavailable("no client token")
	}

	networkId := guest.NetworkId
	return &LagSessionResult{
		ByClientJwt: *authClient.ByClientJwt,
		NetworkId:   &networkId,
		ClientId:    authClient.ClientId,
		ExpiresAt:   guest.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}
