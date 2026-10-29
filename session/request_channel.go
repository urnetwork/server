package session

import (
	"context"
	"net/http"
)

// How a request was made, as a notice to the account may say it (the security
// notices, model/security_notice_model.go). Every field is advisory: the client
// chooses its headers, so none of it is evidence for an access decision.
type RequestChannel struct {
	// the device type the client declared in X-UR-ClientInfo ("android",
	// "web", ...; "unknown" when it sent none)
	DeviceType string
	// the request's Origin header, which a browser sets on the website's
	// cross-origin calls and an app does not send
	BrowserOrigin string
	// an API key, not a signed token, authenticated the request
	ApiKey bool
}

// The channel of the request whose session ctx belongs to, for work that runs
// under the session's context and has no session of its own (a model write).
// False under any other context. It never carries the client address.
func RequestChannelFromContext(ctx context.Context) (RequestChannel, bool) {
	clientSession, ok := ctx.Value(clientObservationContextKey{}).(*ClientSession)
	if !ok || clientSession == nil {
		return RequestChannel{}, false
	}
	channel := RequestChannel{
		DeviceType: clientSession.ClientInfo.DeviceType,
		ApiKey:     clientSession.ApiKeyAuthenticated,
	}
	if origins := http.Header(clientSession.Header).Values("Origin"); len(origins) == 1 {
		channel.BrowserOrigin = origins[0]
	}
	return channel, true
}
