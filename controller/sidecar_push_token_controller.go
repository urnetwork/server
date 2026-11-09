package controller

// POST /sidecar/push-token (§18.7's step 0; D-SDK-16): the caller's own client's wake token, which apps
// post at every launch and again after restore_required (§19.10's rows step). Client class, on §7.4's
// strict_exp allowlist (api/route_authz.go), with no proof: a copied token can only set the wake token of
// the client it belongs to. An empty token deletes, leaving a tombstone (model.SetPushToken).
//
// Dark until M8: while MG8 is not in the binary or sidecar.yml's wake switch is off it answers 503 with
// Retry-After, so no token is collected before the push sender that uses it runs (§19.11).
//
// The token is never logged or answered back (I-13): refusals name what was wrong, never the value.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// POST /sidecar/push-token's body (sidecar.SidecarPushTokenArgs).
type SidecarPushTokenArgs struct {
	// apns or fcm; may be empty with an empty token.
	Platform string `json:"platform"`
	// Empty deletes the caller's token.
	Token string `json:"token"`
}

// What the body shows when printed: never the token.
func (self SidecarPushTokenArgs) String() string {
	return fmt.Sprintf("SidecarPushTokenArgs{Platform: %q, Token: (%d bytes)}", self.Platform, len(self.Token))
}

// The same as String.
func (self SidecarPushTokenArgs) GoString() string {
	return self.String()
}

// POST /sidecar/push-token's answer: nothing but the 200.
type SidecarPushTokenResult struct {
}

// A refusal with its status and, for a closed gate, its Retry-After, as RaiseHttpError reads them.
type sidecarPushTokenRefusal struct {
	status            int
	message           string
	retryAfterSeconds int
}

func (self *sidecarPushTokenRefusal) Error() string {
	return fmt.Sprintf("%d %s", self.status, self.message)
}

func (self *sidecarPushTokenRefusal) RetryAfterSeconds() int {
	return self.retryAfterSeconds
}

// Whether the route is open: MG8 in the binary and sidecar.yml's wake switch on. The handler asks before
// it reads the body.
func SidecarPushTokenOpen() bool {
	return server.SidecarMg8Linked() && server.CurrentSidecarConfig().Wake()
}

// POST /sidecar/push-token: sets or deletes the bearer's own client's token and applies it to the
// client's slot. 503 with Retry-After while the route is dark, 400 for a platform or token the route does
// not take, 401 for a client removed since its token was checked.
func SidecarSetPushToken(args *SidecarPushTokenArgs, clientSession *session.ClientSession) (*SidecarPushTokenResult, error) {
	if !SidecarPushTokenOpen() {
		return nil, &sidecarPushTokenRefusal{status: http.StatusServiceUnavailable, message: "Wake tokens are not available yet.", retryAfterSeconds: 60}
	}
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return nil, &sidecarPushTokenRefusal{status: http.StatusUnauthorized, message: "Not authorized."}
	}
	if args == nil {
		return nil, &sidecarPushTokenRefusal{status: http.StatusBadRequest, message: "Invalid platform or token."}
	}
	_, err := model.SetPushToken(clientSession.Ctx, *clientSession.ByJwt.ClientId, args.Platform, args.Token, nil)
	switch {
	case err == nil:
		return &SidecarPushTokenResult{}, nil
	case errors.Is(err, model.ErrPushTokenInvalid):
		return nil, &sidecarPushTokenRefusal{status: http.StatusBadRequest, message: "Invalid platform or token."}
	case errors.Is(err, model.ErrPushTokenClientRemoved):
		return nil, &sidecarPushTokenRefusal{status: http.StatusUnauthorized, message: "Not authorized."}
	case errors.Is(err, model.ErrPushTokenUnavailable):
		return nil, &sidecarPushTokenRefusal{status: http.StatusServiceUnavailable, message: "Wake tokens are not available yet.", retryAfterSeconds: 60}
	}
	return nil, err
}
