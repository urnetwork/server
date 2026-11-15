//go:build sidecar_pending

package oauth

// §7.8's client_credentials grant (client_credentials.go; TEAM-M6-08, D-TEAM-6), which needs MG6: a team's CI client
// gets an hour's token acting as its creating member within the client's scopes; a scope the client does not carry,
// another secret or a revoked client is refused; the token reaches only the routes registered for it, as its creator;
// and it stops once its client is revoked or its creator leaves. The team tier is stubbed on, since QUOTA-M6-01's tier
// is not in transfer_balance yet.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A team, its Owner, an Admin who creates a CI client, and that client's credentials, with the tier on.
type clientCredentialsFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	owner         *session.ClientSession
	creator       *model.NetworkMember
	created       *model.CreateNetworkOauthClientResult
}

func newClientCredentialsFixture(t testing.TB, label string, scopes ...string) *clientCredentialsFixture {
	t.Helper()
	installUrcliTest(t)
	ctx := context.Background()
	_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restore)
	previousTier := clientCredentialsRequireTier
	clientCredentialsRequireTier = func(ctx context.Context, networkId server.Id) error { return nil }
	t.Cleanup(func() { clientCredentialsRequireTier = previousTier })

	personalNetworkId, ownerUserId := server.NewId(), server.NewId()
	suffix := strings.ReplaceAll(server.NewId().String(), "-", "")[24:32]
	model.Testing_CreateNetwork(ctx, personalNetworkId, fmt.Sprintf("%s-owner-%s", label, suffix), ownerUserId)
	personal := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: ownerUserId})
	team, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: fmt.Sprintf("%s-team-%s", label, suffix)}, personal)
	if err != nil || team.Error != nil {
		t.Fatalf("team-create: %v %+v", err, team)
	}
	self := &clientCredentialsFixture{ctx: ctx, teamNetworkId: *team.NetworkId}
	ownerMemberId := *team.MemberId
	self.owner = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
	server.Tx(ctx, func(tx server.PgTx) {
		creator, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: self.teamNetworkId, UserId: server.NewId(), Role: model.NetworkMemberRoleAdmin})
		server.Raise(err)
		self.creator = creator
	})
	creatorMemberId := self.creator.MemberId
	creatorSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.creator.UserId, MemberId: &creatorMemberId})
	created, err := model.Testing_CreateNetworkOauthClient(&model.CreateNetworkOauthClientArgs{Scopes: scopes}, creatorSession, func(ctx context.Context, networkId server.Id) model.NetworkTier {
		return model.NetworkTierTeam
	})
	if err != nil || created.Error != nil {
		t.Fatalf("the client: %v %+v", err, created)
	}
	self.created = created
	return self
}

// The grant mints an hour's token as the creator within the client's scopes, or within the scopes asked of them; a
// scope the client lacks, another secret, an unknown client and another resource are refused.
func TestClientCredentialsMintsAnHoursTokenAsItsCreator(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientCredentialsFixture(t, "cc-mint", model.ApiKeyScopeAuthKeys, model.ApiKeyScopeLogsRead)
		args := &ClientCredentialsArgs{ClientId: self.created.ClientId, ClientSecret: self.created.ClientSecret}
		response, err := ExchangeClientCredentials(self.ctx, args)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, response.TokenType, "Bearer")
		connect.AssertEqual(t, response.Scope, "auth_keys logs:read")
		connect.AssertEqual(t, IsOauthClientTokenType(response.AccessToken), true)
		claims, err := VerifyOauthClientToken(response.AccessToken)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.MemberId, self.creator.MemberId)
		connect.AssertEqual(t, claims.NetworkId, self.teamNetworkId)
		connect.AssertEqual(t, claims.OauthClientId, *self.created.OauthClientId)
		connect.AssertEqual(t, claims.ExpiresAt.Sub(claims.IssuedAt.Time), time.Hour)
		// an admin token's check takes none of it, and the by_jwt parser none either
		if _, err := VerifyAdminToken(response.AccessToken); err == nil {
			t.Error("a client token verified as an admin token")
		}

		narrowed, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: args.ClientId, ClientSecret: args.ClientSecret, Scopes: []string{model.ApiKeyScopeAuthKeys}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, narrowed.Scope, model.ApiKeyScopeAuthKeys)

		if _, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: args.ClientId, ClientSecret: args.ClientSecret, Scopes: []string{model.ApiKeyScopeDevicesCore}}); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("a scope the client lacks: %v", err)
		}
		other := self.created.ClientSecret[:len(self.created.ClientSecret)-1] + "a"
		if other == self.created.ClientSecret {
			other = self.created.ClientSecret[:len(self.created.ClientSecret)-1] + "b"
		}
		if _, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: args.ClientId, ClientSecret: other}); !errors.Is(err, ErrInvalidClient) {
			t.Errorf("another secret: %v", err)
		}
		if _, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: model.NetworkOauthClientIdText(server.NewId()), ClientSecret: args.ClientSecret}); !errors.Is(err, ErrInvalidClient) {
			t.Errorf("an unknown client: %v", err)
		}
		if _, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: args.ClientId, ClientSecret: args.ClientSecret, Resource: "https://api.elsewhere.example"}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("another resource: %v", err)
		}
	})
}

// The token reaches only the routes registered for it, as its creating member, unsigned so it renews nothing; and it
// stops at once when its client is revoked, and when its creator leaves the team.
func TestAClientTokenActsAsItsCreatorUntilRevoked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientCredentialsFixture(t, "cc-session", model.ApiKeyScopeAuthKeys)
		session.RegisterOauthClientRoute("POST", "/network/auth-keys")
		response, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: self.created.ClientId, ClientSecret: self.created.ClientSecret})
		connect.AssertEqual(t, err, nil)
		authenticate := func(method string, path string) (*session.ClientSession, error) {
			request := httptest.NewRequest(method, path, strings.NewReader("{}"))
			request.RemoteAddr = "192.0.2.10:443"
			request.Header.Set("Authorization", "Bearer "+response.AccessToken)
			clientSession, err := session.NewClientSessionFromRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			return clientSession, clientSession.Auth(request)
		}
		clientSession, err := authenticate("POST", "/network/auth-keys")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, clientSession.OauthClient != nil, true)
		connect.AssertEqual(t, clientSession.UnsignedIdentity, true)
		connect.AssertEqual(t, *clientSession.ByJwt.MemberId, self.creator.MemberId)
		connect.AssertEqual(t, clientSession.ByJwt.UserId, self.creator.UserId)
		connect.AssertEqual(t, clientSession.OauthClient.Scopes, []string{model.ApiKeyScopeAuthKeys})
		if _, err := authenticate("POST", "/auth/network-refresh"); err == nil {
			t.Error("a client token reached a route not registered for it")
		}

		revoked, err := model.Testing_RevokeNetworkOauthClient(&model.RevokeNetworkOauthClientArgs{OauthClientId: *self.created.OauthClientId}, self.owner, func(ctx context.Context, networkId server.Id) model.NetworkTier {
			return model.NetworkTierTeam
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, revoked.Error == nil, true)
		if _, err := authenticate("POST", "/network/auth-keys"); err == nil {
			t.Error("a revoked client's token authenticated")
		}
		if _, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: self.created.ClientId, ClientSecret: self.created.ClientSecret}); !errors.Is(err, ErrInvalidClient) {
			t.Errorf("a revoked client's grant: %v", err)
		}
	})
}

// A creator who leaves the team takes its clients' reach with it: the grant refuses, and a live token stops.
func TestAClientStopsWhenItsCreatorLeaves(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientCredentialsFixture(t, "cc-leave", model.ApiKeyScopeAuthKeys)
		session.RegisterOauthClientRoute("POST", "/network/auth-keys")
		response, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: self.created.ClientId, ClientSecret: self.created.ClientSecret})
		connect.AssertEqual(t, err, nil)
		server.Tx(self.ctx, func(tx server.PgTx) {
			_, err := model.DenyNetworkMemberInTx(self.ctx, tx, &model.NetworkMemberDenial{NetworkId: self.teamNetworkId, MemberId: self.creator.MemberId, Time: server.NowUtc()})
			server.Raise(err)
		})
		if _, err := ExchangeClientCredentials(self.ctx, &ClientCredentialsArgs{ClientId: self.created.ClientId, ClientSecret: self.created.ClientSecret}); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("a removed creator's client: %v", err)
		}
		request := httptest.NewRequest("POST", "/network/auth-keys", strings.NewReader("{}"))
		request.RemoteAddr = "192.0.2.10:443"
		request.Header.Set("Authorization", "Bearer "+response.AccessToken)
		clientSession, err := session.NewClientSessionFromRequest(request)
		connect.AssertEqual(t, err, nil)
		if err := clientSession.Auth(request); err == nil {
			t.Error("a removed creator's live token authenticated")
		}
	})
}
