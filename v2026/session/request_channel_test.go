package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A model write under a request's context reads how the request was made: the
// device type the client declared, the browser origin, and an API key that
// authenticated the session after it was made. Any other context has none.
func TestRequestChannelFromContextReadsTheRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.bringyour.com/account/payout-wallet", nil)
	req.Header.Set(ClientInfoHeader, `{"v":1,"device_type":"android","app_version":"1.2.3"}`)
	req.Header.Set("Origin", "https://ur.io")
	clientSession, err := NewClientSessionFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Cancel()

	channel, ok := RequestChannelFromContext(clientSession.Ctx)
	if !ok || channel.DeviceType != "android" || channel.BrowserOrigin != "https://ur.io" || channel.ApiKey {
		t.Fatalf("channel = %+v, %v; want the android app from https://ur.io without an API key", channel, ok)
	}
	// authentication sets the flag on the session the context holds
	clientSession.ApiKeyAuthenticated = true
	if channel, _ := RequestChannelFromContext(clientSession.Ctx); !channel.ApiKey {
		t.Fatal("an API key that authenticated the session is not seen through its context")
	}

	if _, ok := RequestChannelFromContext(context.Background()); ok {
		t.Fatal("a context without a request has a channel")
	}
	if _, ok := RequestChannelFromContext(Testing_CreateClientSession(context.Background(), nil).Ctx); ok {
		t.Fatal("a session made without a request has a channel")
	}
}
