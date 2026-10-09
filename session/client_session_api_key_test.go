package session_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// An API key authenticates as the network it belongs to. ApiKeyAuthenticated
// tells that session apart from one with a signed network token, so a route
// that signs tokens (POST /auth/network-refresh) refuses to sign the key's
// identity back to its holder as a token.
func TestClientSessionMarksOnlyApiKeyAuthentication(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("apikey%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		rootSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: networkName,
		})
		created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "backend"}, rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, created.Error, nil)
		rootToken := session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()

		authenticate := func(clientSession *session.ClientSession, authorization string) error {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				return err
			}
			request.Header.Set("Authorization", authorization)
			return clientSession.Auth(request)
		}

		// the API key authenticates as the network, marked
		keySession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer keySession.Cancel()
		connect.AssertEqual(t, authenticate(keySession, "Bearer "+created.ApiKey), nil)
		connect.AssertEqual(t, keySession.ByJwt.NetworkId, networkId)
		connect.AssertEqual(t, keySession.ByJwt.ClientId == nil, true)
		connect.AssertEqual(t, keySession.ApiKeyAuthenticated, true)
		// a view with a replaced identity does not carry the mark
		connect.AssertEqual(t, keySession.WithByJwt(keySession.ByJwt).ApiKeyAuthenticated, false)

		// the signed network token authenticates as the same network, unmarked
		tokenSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer tokenSession.Cancel()
		connect.AssertEqual(t, authenticate(tokenSession, "Bearer "+rootToken), nil)
		connect.AssertEqual(t, tokenSession.ByJwt.NetworkId, networkId)
		connect.AssertEqual(t, tokenSession.ApiKeyAuthenticated, false)

		// authenticating the marked session again with the token clears it
		connect.AssertEqual(t, authenticate(keySession, "Bearer "+rootToken), nil)
		connect.AssertEqual(t, keySession.ApiKeyAuthenticated, false)

		// a failed authentication leaves no mark and no identity, whether the
		// session was marked before or not
		connect.AssertEqual(t, authenticate(keySession, "Bearer "+created.ApiKey), nil)
		connect.AssertEqual(t, keySession.ApiKeyAuthenticated, true)
		unknownKey := "urn_" + strings.Repeat("0", 52)
		connect.AssertEqual(t, authenticate(keySession, "Bearer "+unknownKey) != nil, true)
		connect.AssertEqual(t, keySession.ByJwt == nil, true)
		connect.AssertEqual(t, keySession.ApiKeyAuthenticated, false)

		// a session built for trusted work is never marked
		localSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", tokenSession.ByJwt)
		defer localSession.Cancel()
		connect.AssertEqual(t, localSession.ApiKeyAuthenticated, false)
	})
}
