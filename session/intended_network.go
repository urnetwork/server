package session

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
)

// A client may say which network a request is meant for: the network the
// screen it acts from shows as signed in. A browser keeps one stored jwt for
// all of its tabs, so a tab still showing network A sends network B's jwt once
// another tab has signed in to B. The intended network makes that request
// fail instead of acting on B (AUTHZ1.md §10). The header is optional: a
// request without it is served exactly as before.
const IntendedNetworkIdHeader = "X-UR-Intended-Network-Id"

// The stable codes of the intended network refusals, in the JSON body's
// "code" (IntendedNetworkError).
const (
	// the credential signs in to a network other than the intended one
	IntendedNetworkMismatchCode = "intended_network_mismatch"
	// the header is not exactly one network id
	InvalidIntendedNetworkIdCode = "invalid_intended_network_id"
)

const IntendedNetworkMismatchMessage = "This request was meant for a different network than the one signed in. Nothing was changed."
const InvalidIntendedNetworkIdMessage = IntendedNetworkIdHeader + " must be one network id."

// IntendedNetworkError is an intended network refusal. Its status is never 401
// or 403: the credential is valid, and the apps sign out on those (a browser
// sign-out would end the sign-in every tab shares). The router answers it as
// JSON {"error": message, "code": code}.
type IntendedNetworkError struct {
	Code    string
	Status  int
	Message string
}

func (self *IntendedNetworkError) Error() string {
	return fmt.Sprintf("%d %s", self.Status, self.Message)
}

func (self *IntendedNetworkError) HttpErrorResultBody() any {
	return map[string]any{"error": self.Message, "code": self.Code}
}

func intendedNetworkMismatchError() error {
	return &IntendedNetworkError{
		Code:    IntendedNetworkMismatchCode,
		Status:  http.StatusPreconditionFailed,
		Message: IntendedNetworkMismatchMessage,
	}
}

func invalidIntendedNetworkIdError() error {
	return &IntendedNetworkError{
		Code:    InvalidIntendedNetworkIdCode,
		Status:  http.StatusBadRequest,
		Message: InvalidIntendedNetworkIdMessage,
	}
}

// RequestIntendedNetworkId reads the network a request says it is meant for.
// ok is false when the request does not say. A header that is not exactly one
// network id, the zero id included, is an error: the client asked for a check
// that cannot be made.
func RequestIntendedNetworkId(header http.Header) (networkId server.Id, ok bool, err error) {
	values := header.Values(IntendedNetworkIdHeader)
	if len(values) == 0 {
		return server.Id{}, false, nil
	}
	if len(values) == 1 {
		networkId, err := server.ParseId(strings.TrimSpace(values[0]))
		if err == nil && networkId != (server.Id{}) {
			return networkId, true, nil
		}
	}
	return server.Id{}, false, invalidIntendedNetworkIdError()
}

// CheckIntendedNetwork refuses a request whose X-UR-Intended-Network-Id names
// a network other than the one its bearer credential signs in to. It returns
// nil for every other request, which reaches the handler unchanged:
//   - a request without the header;
//   - a request without a URnetwork bearer credential, or whose credential does
//     not verify: the handler's authentication refuses it as before, so a bad
//     credential is still its 401;
//   - a credential of the intended network.
//
// The check reads only the credential: it does not validate account or
// session state, which stays the handler's authentication.
//
// A refusal is an IntendedNetworkError: 412 for another network's credential,
// 400 for a malformed header. ErrAuthUnavailable means the credential's network
// could not be resolved (an API key, or a legacy token without its network
// claim) because a dependency failed; the request is refused, not served
// unchecked.
func CheckIntendedNetwork(ctx context.Context, req *http.Request) (returnErr error) {
	intendedNetworkId, ok, err := RequestIntendedNetworkId(req.Header)
	if err != nil || !ok {
		return err
	}

	checkCtx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			err, isError := recovered.(error)
			if !isError || !authDependencyUnavailable(err) {
				panic(recovered)
			}
			returnErr = ErrAuthUnavailable
		}
	}()

	networkId, resolved, err := requestCredentialNetworkId(checkCtx, req, intendedNetworkId)
	if err != nil {
		if checkCtx.Err() != nil || authDependencyUnavailable(err) {
			return ErrAuthUnavailable
		}
		// the credential does not verify: the handler refuses it
		return nil
	}
	if !resolved || networkId == intendedNetworkId {
		return nil
	}
	if glog.V(1) {
		glog.Infof("[session]refused a request meant for network %s with a credential of network %s\n", intendedNetworkId, networkId)
	}
	return intendedNetworkMismatchError()
}

// The API key lookup the check resolves a key's network with, the one
// authentication uses. Tests replace it.
var intendedNetworkApiKeyLookup = func(ctx context.Context, apiKey string) *apikey.NetworkByApiKey {
	return apikey.GetNetworkByApiKey(apiKey, ctx)
}

// requestCredentialNetworkId returns the network the request's bearer
// credential signs in to. resolved is false for a request without a URnetwork
// bearer credential and for an API key that names no network.
//
// A token's claims are read unverified first. Claims that name the intended
// network need no verification here: the check then refuses nothing, and the
// handler verifies the token. A token whose claims name another network, or
// none, is verified before its network is relied on, so a forged token is never
// refused by the check; the handler refuses it.
func requestCredentialNetworkId(ctx context.Context, req *http.Request, intendedNetworkId server.Id) (networkId server.Id, resolved bool, err error) {
	authorization := req.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, authBearerPrefix) {
		return server.Id{}, false, nil
	}
	token := authorization[len(authBearerPrefix):]

	if strings.HasPrefix(token, apiKeyTokenPrefix) {
		if len(token) != apiKeyTokenLength {
			return server.Id{}, false, nil
		}
		network := intendedNetworkApiKeyLookup(ctx, token)
		if network == nil {
			return server.Id{}, false, nil
		}
		return network.NetworkId, true, nil
	}

	if claimedNetworkId, ok := byJwtNetworkIdUnverified(token); ok && claimedNetworkId == intendedNetworkId {
		return claimedNetworkId, true, nil
	}
	byJwt, err := ParseByJwtForAudience(ctx, token, ByJwtAudienceApi)
	if err != nil {
		return server.Id{}, false, err
	}
	return byJwt.NetworkId, true, nil
}

// byJwtNetworkIdUnverified returns the network a token's claims name, read
// without verifying the token and without any lookup. ok is false for claims
// that do not parse or name no network.
func byJwtNetworkIdUnverified(jwtStr string) (server.Id, bool) {
	byJwt := &ByJwt{}
	if _, _, err := gojwt.NewParser().ParseUnverified(jwtStr, byJwt); err != nil {
		return server.Id{}, false
	}
	return byJwt.NetworkId, byJwt.NetworkId != (server.Id{})
}
