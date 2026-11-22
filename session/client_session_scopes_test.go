//go:build sidecar_pending

package session_test

// The scopes a request's credential carries (ClientSession.CredentialScopes; §7.8), which cut its creator's role, on
// Postgres with MG0's scopes column.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// An API key's scopes come from its row as it authenticates, and a legacy key's nil keeps its whole reach; a signed
// token has none of its own; a session authenticated again with a token keeps nothing of the key it held before; and an
// admin token's and an OAuth client token's scopes come from their use, a use naming none reaching nothing.
func TestCredentialScopesOfEachCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("scopes%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		rootSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: networkName,
		})
		created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "scoped"}, rootSession)
		if err != nil || created.Error != nil {
			t.Fatalf("the key: %v %+v", err, created)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_api_key SET scopes = $2::jsonb WHERE api_key_id = $1`, created.Id, `["auth_keys"]`))
		})
		legacy, err := apikey.Testing_CreateApiKey(networkId, ctx)
		if err != nil {
			t.Fatal(err)
		}
		rootToken := session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()

		authenticate := func(clientSession *session.ClientSession, authorization string) {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", authorization)
			if err := clientSession.Auth(request); err != nil {
				t.Fatalf("auth: %v", err)
			}
		}

		keySession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer keySession.Cancel()
		authenticate(keySession, "Bearer "+created.ApiKey)
		if scopes := keySession.CredentialScopes(); !slices.Equal(scopes, []string{"auth_keys"}) {
			t.Fatalf("the scoped key's credential scopes %v", scopes)
		}
		// the same session, authenticated again with a token, keeps nothing of the key
		authenticate(keySession, "Bearer "+rootToken)
		if keySession.ApiKeyScopes != nil || keySession.CredentialScopes() != nil {
			t.Fatalf("a token after a key carries scopes %v (%v)", keySession.CredentialScopes(), keySession.ApiKeyScopes)
		}

		legacySession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer legacySession.Cancel()
		authenticate(legacySession, "Bearer "+legacy.ApiKey)
		if !legacySession.ApiKeyAuthenticated || legacySession.CredentialScopes() != nil {
			t.Fatalf("a legacy key's credential scopes %v", legacySession.CredentialScopes())
		}

		adminCommand := &session.ClientSession{AdminCommand: &session.AdminCommandUse{Scopes: []string{"auth_keys"}}}
		if scopes := adminCommand.CredentialScopes(); !slices.Equal(scopes, []string{"auth_keys"}) {
			t.Fatalf("an admin token's credential scopes %v", scopes)
		}
		oauthClient := &session.ClientSession{OauthClient: &session.OauthClientUse{Scopes: []string{"logs:read"}}}
		if scopes := oauthClient.CredentialScopes(); !slices.Equal(scopes, []string{"logs:read"}) {
			t.Fatalf("an OAuth client token's credential scopes %v", scopes)
		}
		for _, unscoped := range []*session.ClientSession{
			{AdminCommand: &session.AdminCommandUse{}},
			{OauthClient: &session.OauthClientUse{}},
		} {
			if scopes := unscoped.CredentialScopes(); scopes == nil || len(scopes) != 0 {
				t.Fatalf("a scoped credential naming no scope reaches %v", scopes)
			}
		}
	})
}
