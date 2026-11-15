//go:build sidecar_pending

package model

// §7.8's network OAuth clients (network_oauth_client_model.go; TEAM-M6-08), which need MG6: an Admin's client with its
// secret shown once and only its hash kept, listed with no secret and revoked; every refusal of the caller and the
// scopes; a one-command admin token spent once; the notice a new client owes; and an OAuth client's auth key of at
// most an hour (D-TEAM-6). The team tier is stubbed on, since QUOTA-M6-01's tier is not in transfer_balance yet.

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A team with its Owner, an Admin and a Member, each with a credential, and the clients with the tier on.
type oauthClientFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	admin         *session.ClientSession
	adminMember   *NetworkMember
	member        *session.ClientSession
	clients       *networkOauthClients
}

func newOauthClientFixture(t testing.TB) *oauthClientFixture {
	t.Helper()
	ctx := context.Background()
	_, restore := Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restore)
	_, _, _, personal := testTeamPersonalNetwork(t, ctx, "oauth-client-owner")
	created := createTestTeam(t, testNetworkTeams(server.NowUtc), personal, testTeamName("oauth-client-team"))
	self := &oauthClientFixture{ctx: ctx, teamNetworkId: *created.NetworkId}
	sessionOf := func(role NetworkMemberRole) (*session.ClientSession, *NetworkMember) {
		var member *NetworkMember
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: self.teamNetworkId, UserId: server.NewId(), Role: role})
			server.Raise(err)
		})
		memberId := member.MemberId
		return session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: member.UserId, MemberId: &memberId}), member
	}
	self.admin, self.adminMember = sessionOf(NetworkMemberRoleAdmin)
	self.member, _ = sessionOf(NetworkMemberRoleMember)
	self.clients = &networkOauthClients{
		now:    server.NowUtc,
		tierOf: func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierTeam },
	}
	return self
}

// An Admin creates a client: the answer alone holds the secret, the row its SHA-256, its scopes sorted and its creator;
// the secret authenticates it and nothing else does; the list shows no secret; a revocation removes it; each is an
// admin event, and the creation owes the authority notice.
func TestOauthClientCreateListRevoke(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newOauthClientFixture(t)
		created, err := self.clients.create(&CreateNetworkOauthClientArgs{Scopes: []string{ApiKeyScopeLogsRead, ApiKeyScopeAuthKeys, ApiKeyScopeAuthKeys}}, self.admin)
		connect.AssertEqual(t, err, nil)
		if created.Error != nil {
			t.Fatalf("the client: %+v", created.Error)
		}
		connect.AssertEqual(t, strings.HasPrefix(created.ClientId, NetworkOauthClientIdPrefix), true)
		connect.AssertEqual(t, strings.HasPrefix(created.ClientSecret, NetworkOauthClientSecretPrefix), true)
		connect.AssertEqual(t, created.Scopes, []string{ApiKeyScopeAuthKeys, ApiKeyScopeLogsRead})
		connect.AssertEqual(t, strings.Contains(created.String()+created.GoString(), created.ClientSecret), false)

		secret, ok := parseNetworkOauthClientSecret(created.ClientSecret)
		connect.AssertEqual(t, ok, true)
		secretSha256 := sha256.Sum256(secret)
		var stored []byte
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT secret_sha256 FROM network_oauth_client WHERE oauth_client_id = $1`, *created.OauthClientId).Scan(&stored))
		})
		connect.AssertEqual(t, stored, secretSha256[:])

		client := AuthenticateNetworkOauthClient(self.ctx, created.ClientId, created.ClientSecret)
		connect.AssertEqual(t, client != nil, true)
		connect.AssertEqual(t, client.CreatorMemberId, self.adminMember.MemberId)
		connect.AssertEqual(t, AuthenticateNetworkOauthClient(self.ctx, created.ClientId, NetworkOauthClientSecretPrefix+"abc") == nil, true)
		connect.AssertEqual(t, AuthenticateNetworkOauthClient(self.ctx, NetworkOauthClientIdText(server.NewId()), created.ClientSecret) == nil, true)

		listed, err := self.clients.list(self.admin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(listed.OauthClients), 1)
		connect.AssertEqual(t, listed.OauthClients[0].ClientId, created.ClientId)

		revoked, err := self.clients.revoke(&RevokeNetworkOauthClientArgs{OauthClientId: *created.OauthClientId}, self.admin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, revoked.Error == nil, true)
		connect.AssertEqual(t, AuthenticateNetworkOauthClient(self.ctx, created.ClientId, created.ClientSecret) == nil, true)
		again, err := self.clients.revoke(&RevokeNetworkOauthClientArgs{OauthClientId: *created.OauthClientId}, self.admin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Error.Message, networkOauthClientUnknownMessage)

		actions := []string{}
		for _, event := range GetNetworkAdminEvents(self.ctx, self.teamNetworkId, 0, 100) {
			actions = append(actions, event.Action)
		}
		connect.AssertEqual(t, actions[len(actions)-2:], []string{AdminEventActionOauthClientCreate, AdminEventActionOauthClientRevoke})
	})
}

// Who may not manage clients: a Member, a client token, an API key, a network outside the team tier; and no scope, or
// one §7.8 does not name, is refused.
func TestOauthClientRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newOauthClientFixture(t)
		args := &CreateNetworkOauthClientArgs{Scopes: []string{ApiKeyScopeAuthKeys}}
		if _, err := self.clients.create(args, self.member); err == nil {
			t.Error("a Member created a client")
		}
		clientId := server.NewId()
		client := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.adminMember.UserId, MemberId: self.admin.ByJwt.MemberId, ClientId: &clientId})
		if _, err := self.clients.create(args, client); err == nil {
			t.Error("a client token created a client")
		}
		apiKey := session.Testing_CreateClientSession(self.ctx, self.admin.ByJwt)
		apiKey.ApiKeyAuthenticated = true
		if _, err := self.clients.create(args, apiKey); err == nil {
			t.Error("an API key created a client")
		}
		outside := &networkOauthClients{now: server.NowUtc, tierOf: func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierPro }}
		if _, err := outside.create(args, self.admin); err == nil {
			t.Error("a network outside the team tier created a client")
		}
		for _, scopes := range [][]string{{}, {"mcp:read"}, {ApiKeyScopeAuthKeys, "admin"}} {
			refused, err := self.clients.create(&CreateNetworkOauthClientArgs{Scopes: scopes}, self.admin)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refused.Error != nil, true)
		}
		listed, err := self.clients.list(self.admin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(listed.OauthClients), 0)
	})
}

// A one-command admin token with the all scope creates once: its replay answers the first client without its secret,
// which never rests on the server, and makes no second client.
func TestOauthClientByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newOauthClientFixture(t)
		jti := server.NewId().String()
		tokenSession := session.Testing_CreateClientSession(self.ctx, self.admin.ByJwt)
		tokenSession.UnsignedIdentity = true
		tokenSession.AdminCommand = &session.AdminCommandUse{
			NetworkId: self.teamNetworkId, MemberId: self.adminMember.MemberId, Jti: jti, Cmd: "oauth-client-test-cmd",
			Scopes: []string{ApiKeyScopeAll}, ExpireTime: server.NowUtc().Add(time.Hour),
		}
		first, err := self.clients.create(&CreateNetworkOauthClientArgs{Scopes: []string{ApiKeyScopeAuthKeys}}, tokenSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.HasPrefix(first.ClientSecret, NetworkOauthClientSecretPrefix), true)
		replay, err := self.clients.create(&CreateNetworkOauthClientArgs{Scopes: []string{ApiKeyScopeAuthKeys}}, tokenSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *replay.OauthClientId, *first.OauthClientId)
		connect.AssertEqual(t, replay.ClientSecret, "")
		var storedResult []byte
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT result FROM admin_token_use WHERE jti = $1`, jti).Scan(&storedResult))
		})
		connect.AssertEqual(t, strings.Contains(string(storedResult), first.ClientSecret), false)
		listed, err := self.clients.list(self.admin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(listed.OauthClients), 1)
	})
}

// D-TEAM-6: a key an OAuth client's token creates lives at most an hour; the same request under the creator's own
// credential may ask for longer.
func TestAnOauthClientsAuthKeyLivesAtMostAnHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newOauthClientFixture(t)
		clientSession := session.Testing_CreateClientSession(self.ctx, self.admin.ByJwt)
		clientSession.UnsignedIdentity = true
		clientSession.OauthClient = &session.OauthClientUse{
			NetworkId: self.teamNetworkId, MemberId: self.adminMember.MemberId, OauthClientId: server.NewId(),
			Scopes: []string{ApiKeyScopeAuthKeys}, ExpireTime: server.NowUtc().Add(time.Hour),
		}
		settings := DefaultNetworkAuthKeySettings()
		longer, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: int64(settings.OauthClientMaxLifetime/time.Second) + 1, MaxUses: 1}, clientSession, settings, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, longer.Error != nil, true)
		hour, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: int64(settings.OauthClientMaxLifetime / time.Second), MaxUses: 1}, clientSession, settings, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		if hour.Error != nil {
			t.Fatalf("an hour's key: %+v", hour.Error)
		}
		own, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: int64(2 * time.Hour / time.Second), MaxUses: 1}, self.admin, settings, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, own.Error == nil, true)
	})
}
