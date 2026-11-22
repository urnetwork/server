package oauth

// `ur login` for every member of a team (§7.5's Member: own devices; §5.6, §7.8). ur-cli's one v0 command is POST
// /network/auth-keys for a single-use key of at most 5 minutes, a key for the person's own devices, which a team
// member makes whatever its role: the auth key model holds a role without the keys permission to an untagged key that
// is not pre-approved (model's authKeyReachOf). The consent page's four routes ask the keys permission of a team
// member's credential (api/route_permission.go), since in general they grant an OAuth client access; these recognizers
// let the role gate admit a member whose role lacks it to the one consent whose admin token creates only such a key, as
// IsEdgeSignInRequest admits a members site's sign-in. Each reads only what the gate needs and leaves the body readable:
// the handler validates the request in full, and refuses one it passed that ur-cli may not run.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Whether request asks `ur login`'s one command: ur-cli under the auth_keys scope alone, its details naming POST
// /network/auth-keys with a body validateUrcliCommand takes, a single-use key of at most MaxAuthKeyDuration with no
// tag, ephemeral or pre-approved field, so the admin token it leads to creates a key for its member's own devices and
// nothing else.
func isUrcliOwnKeyRequest(request *AuthorizationRequest) bool {
	if request.ClientId != UrcliClientId || !slices.Equal(request.Scopes, []string{UrcliScopeAuthKeys}) {
		return false
	}
	details, err := sidecar.ParseAdminCommandDetails(request.AuthorizationDetails)
	if err != nil {
		return false
	}
	command := details.Command()
	return command.Method == http.MethodPost && command.Path == "/network/auth-keys" &&
		validateUrcliCommand(command, DefaultUrcliSettings()) == nil
}

// The request's body within the consent request's bound, left readable again for the handler; false for none or one
// past the bound.
func readUrcliConsentBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, false
	}
	limit := DefaultOauthHandlerSettings().ConsentRequestMaxByteCount
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || limit < int64(len(body)) {
		return nil, false
	}
	return body, true
}

// Lets the role gate admit a team member whose role lacks the keys permission to POST /oauth/consent and POST
// /oauth/authorize for `ur login`'s one command (isUrcliOwnKeyRequest), read from the consent request in the body.
func IsUrcliOwnKeyRequest(r *http.Request) bool {
	body, ok := readUrcliConsentBody(r)
	if !ok {
		return false
	}
	request := &ConsentRequest{}
	return json.Unmarshal(body, request) == nil && isUrcliOwnKeyRequest(request.authorizationRequest())
}

// IsUrcliOwnKeyRequest's check for the device grant's consent page, POST /oauth/device_consent and POST
// /oauth/device_approve: the waiting device authorization the body's user code names asks `ur login`'s one command.
// With no store, or one that cannot answer, it admits nothing.
func IsUrcliOwnKeyDeviceRequest(r *http.Request) bool {
	body, ok := readUrcliConsentBody(r)
	if !ok {
		return false
	}
	request := &DeviceConsentRequest{}
	if json.Unmarshal(body, request) != nil {
		return false
	}
	store := currentDeviceAuthorizationStore()
	if store == nil {
		return false
	}
	authorization, err := deviceAuthorizationOfUserCode(r.Context(), store, request.UserCode, DefaultDeviceGrantSettings(), server.NowUtc())
	return err == nil && isUrcliOwnKeyRequest(authorization.authorizationRequest())
}
