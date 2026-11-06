//go:build sidecar_pending

package model

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §7.4's marks and §7.7's owner rows, which need MG0.

// A sidecar.yml that lists the fixture's network in preview_networks.
func (self *clientOwnerFixture) pushPreview(t testing.TB) {
	t.Helper()
	t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", self.networkId)))
}

type clientOwnerRow struct {
	ownerUserId  *server.Id
	adminRefused bool
	strictExp    bool
}

// The client's owner row, or nil for none.
func (self *clientOwnerFixture) ownerRow(t testing.TB, clientId server.Id) (row *clientOwnerRow) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`SELECT owner_user_id, admin_refused, strict_exp FROM network_client_owner WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &clientOwnerRow{}
				server.Raise(result.Scan(&row.ownerUserId, &row.adminRefused, &row.strictExp))
			}
		})
	})
	return
}

func requireTokenLifetime(t testing.TB, ctx *clientOwnerFixture, signed string, lifetime time.Duration) *session.ByJwt {
	t.Helper()
	byJwt, err := session.ParseByJwt(ctx.ctx, signed)
	if err != nil {
		t.Fatal(err)
	}
	if d := byJwt.ExpiresAt.Time.Sub(server.NowUtc().Add(lifetime)); d < -time.Minute || time.Minute < d {
		t.Fatalf("exp = %s, want %s from now", byJwt.ExpiresAt.Time, lifetime)
	}
	return byJwt
}

const (
	strictExpLifetime = 7 * 24 * time.Hour
	ordinaryLifetime  = 30 * 24 * time.Hour
)

// An admin_refused client is refused like a child. Its row names the user who
// owns it, it keeps today's 30 days, and no strict_exp comes with it: the app
// device clients of §18.7 are minted this way.
func TestAdminRefusedClientIsRefusedAdmin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)

		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})
		connect.AssertEqual(t, client.AdminRefused, true)
		connect.AssertEqual(t, client.StrictExp, false)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *client.ClientId), true)

		row := self.ownerRow(t, *client.ClientId)
		connect.AssertEqual(t, row != nil && row.adminRefused && !row.strictExp, true)
		connect.AssertEqual(t, *row.ownerUserId, self.userId)

		byJwt := requireTokenLifetime(t, self, *client.ByClientJwt, ordinaryLifetime)
		connect.AssertEqual(t, byJwt.StrictExp, false)
	})
}

// A legacy mint writes no row, so the check reads it as what it is.
func TestUnmarkedMintWritesNoOwnerRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		if row := self.ownerRow(t, *client.ClientId); row != nil {
			t.Fatalf("an unmarked client has a row: %+v", row)
		}
		connect.AssertEqual(t, client.AdminRefused, false)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *client.ClientId), false)
	})
}

// strict_exp mints the sidecar token of D-TEAM-14: the claim, a 7-day exp and the
// root claim, and the reply confirms both marks. strict_exp alone implies
// admin_refused: nothing that expires in 7 days keeps the apps' admin routes.
func TestStrictExpMintIsASidecarToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		self.pushPreview(t)

		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		connect.AssertEqual(t, client.StrictExp, true)
		connect.AssertEqual(t, client.AdminRefused, true)
		row := self.ownerRow(t, *client.ClientId)
		connect.AssertEqual(t, row.strictExp && row.adminRefused, true)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *client.ClientId), true)

		byJwt := requireTokenLifetime(t, self, *client.ByClientJwt, strictExpLifetime)
		connect.AssertEqual(t, byJwt.StrictExp, true)
		connect.AssertEqual(t, byJwt.RootClientId != nil, true)
		connect.AssertEqual(t, *byJwt.RootClientId, *client.ClientId)
	})
}

// A marked client's children inherit both marks, whoever mints them: the sidecar
// with its own token, or the network credential naming it as the source.
func TestChildrenInheritBothMarks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		self.pushPreview(t)

		sidecar := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", AdminRefused: true, StrictExp: true})
		for name, caller := range map[string]*session.ClientSession{
			"its own token":      self.sessionOf(t, sidecar),
			"network credential": self.root,
		} {
			child := self.mint(t, caller, &AuthNetworkClientArgs{SourceClientId: sidecar.ClientId, Description: "window", DeviceSpec: "test"})
			row := self.ownerRow(t, *child.ClientId)
			if row == nil || !row.adminRefused || !row.strictExp || !child.StrictExp || !child.AdminRefused {
				t.Fatalf("%s: child marks = %+v, reply %+v", name, row, child)
			}
			byJwt := requireTokenLifetime(t, self, *child.ByClientJwt, strictExpLifetime)
			connect.AssertEqual(t, byJwt.StrictExp, true)
			connect.AssertEqual(t, *byJwt.RootClientId, *sidecar.ClientId)
		}
	})
}

// Owner decision 131: an app device client minted admin_refused, and so its window
// clients, carry no strict_exp and keep today's lifetime, because window clients
// never refresh. The window client inherits what its device has and no more.
func TestAdminRefusedDeviceWindowClientsCarryNoStrictExp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)

		device := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})
		window := self.mint(t, self.sessionOf(t, device), &AuthNetworkClientArgs{SourceClientId: device.ClientId, Description: "window", DeviceSpec: "test"})

		row := self.ownerRow(t, *window.ClientId)
		connect.AssertEqual(t, row != nil && row.adminRefused && !row.strictExp, true)
		connect.AssertEqual(t, window.StrictExp, false)
		byJwt := requireTokenLifetime(t, self, *window.ByClientJwt, ordinaryLifetime)
		connect.AssertEqual(t, byJwt.StrictExp, false)
	})
}

// An sdk host's device token mints its sidecar's child with the marks (§5.6): an
// unmarked parent does not hold them back.
func TestLegacyDeviceMintsAMarkedChild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		self.pushPreview(t)

		device := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		child := self.mint(t, self.sessionOf(t, device), &AuthNetworkClientArgs{SourceClientId: device.ClientId, Description: "sidecar", DeviceSpec: "test", AdminRefused: true, StrictExp: true})
		connect.AssertEqual(t, child.StrictExp && child.AdminRefused, true)
		requireTokenLifetime(t, self, *child.ByClientJwt, strictExpLifetime)
		if row := self.ownerRow(t, *device.ClientId); row != nil {
			t.Fatalf("the parent gained a row: %+v", row)
		}
	})
}

func countNetworkClients(self *clientOwnerFixture) (count int) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT COUNT(*) FROM network_client WHERE network_id = $1`, self.networkId).Scan(&count))
	})
	return
}

// D-TRUST-3: until stage 2 a strict_exp mint is refused outside preview_networks,
// with nothing written, and an inherited mark asks the same question: a network
// taken off the list mints no more.
func TestNonPreviewNetworkRefusesAStrictExpMint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)

		requireRefused := func(args *AuthNetworkClientArgs, caller *session.ClientSession) {
			t.Helper()
			before := countNetworkClients(self)
			result, err := AuthNetworkClient(args, caller)
			connect.AssertEqual(t, err, nil)
			if result.Error == nil || result.Error.Message != clientStrictExpNotPreviewMessage || result.ByClientJwt != nil {
				t.Fatalf("auth-client = %+v, error = %+v", result, result.Error)
			}
			connect.AssertEqual(t, countNetworkClients(self), before)
		}

		requireRefused(&AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true}, self.root)

		// admin_refused alone needs no preview: it is the apps' mark too
		self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})

		// another network's listing is no listing
		pop := server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", server.NewId()))
		requireRefused(&AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true}, self.root)
		pop()

		pop = server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", self.networkId))
		sidecar := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		sidecarSession := self.sessionOf(t, sidecar)
		pop()

		requireRefused(&AuthNetworkClientArgs{SourceClientId: sidecar.ClientId, Description: "window", DeviceSpec: "test"}, sidecarSession)
		requireRefused(&AuthNetworkClientArgs{SourceClientId: sidecar.ClientId, Description: "window", DeviceSpec: "test"}, self.root)
		requireRefused(&AuthNetworkClientArgs{ClientId: sidecar.ClientId, Description: "sidecar", DeviceSpec: "test"}, self.root)
	})
}

// A re-auth gets the token its client's marks call for, whoever asks: the network
// credential's own 30 days do not reach a sidecar's client. A mark cannot be added
// after creation, because the check caches both facts as immutable.
func TestReauthFollowsTheClientsMarks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		self.pushPreview(t)

		sidecar := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		for _, args := range []*AuthNetworkClientArgs{
			{ClientId: sidecar.ClientId, Description: "sidecar", DeviceSpec: "test"},
			// what `sidecar` always sends
			{ClientId: sidecar.ClientId, Description: "sidecar", DeviceSpec: "test", AdminRefused: true, StrictExp: true},
		} {
			reissued := self.mint(t, self.root, args)
			connect.AssertEqual(t, reissued.StrictExp && reissued.AdminRefused, true)
			byJwt := requireTokenLifetime(t, self, *reissued.ByClientJwt, strictExpLifetime)
			connect.AssertEqual(t, byJwt.StrictExp, true)
		}

		legacy := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		for _, args := range []*AuthNetworkClientArgs{
			{ClientId: legacy.ClientId, Description: "device", DeviceSpec: "test", AdminRefused: true},
			{ClientId: legacy.ClientId, Description: "device", DeviceSpec: "test", StrictExp: true},
		} {
			result, err := AuthNetworkClient(args, self.root)
			connect.AssertEqual(t, err, nil)
			if result.Error == nil || result.Error.Message != clientMarksImmutableMessage {
				t.Fatalf("re-auth with a new mark = %+v, error = %+v", result, result.Error)
			}
		}
		if row := self.ownerRow(t, *legacy.ClientId); row != nil {
			t.Fatalf("a refused re-auth wrote a row: %+v", row)
		}

		adminRefused := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})
		result, err := AuthNetworkClient(&AuthNetworkClientArgs{ClientId: adminRefused.ClientId, Description: "device", DeviceSpec: "test", StrictExp: true}, self.root)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil || result.Error.Message != clientMarksImmutableMessage {
			t.Fatalf("re-auth adding strict_exp = %+v, error = %+v", result, result.Error)
		}
	})
}

// A hosted proxy's token comes from its host with no mark, so a strict_exp client
// has none, and a refused create leaves nothing, not even its exit.
func TestStrictExpClientHostsNoProxy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		self.pushPreview(t)

		sidecar := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		writesBefore := authClientTestWritesNow(self.ctx)
		proxyConfig := &ProxyConfig{
			InitialDeviceState: &ExtendedProxyDeviceState{
				ProxyDeviceState: ProxyDeviceState{Location: authClientTestBestAvailableLocation()},
			},
		}
		for _, args := range []*AuthNetworkClientArgs{
			{Description: "proxy", DeviceSpec: "test", StrictExp: true, ProxyConfig: proxyConfig},
			{SourceClientId: sidecar.ClientId, Description: "proxy", DeviceSpec: "test", ProxyConfig: proxyConfig},
		} {
			result, err := AuthNetworkClient(args, self.root)
			connect.AssertEqual(t, err, nil)
			if result.Error == nil || result.Error.Message != clientStrictExpProxyMessage {
				t.Fatalf("auth-client = %+v, error = %+v", result, result.Error)
			}
		}
		if writes := authClientTestWritesNow(self.ctx); writes != writesBefore {
			t.Fatalf("the refusal left %+v, want %+v", writes, writesBefore)
		}
	})
}

// An owner row goes with its client (§7.7): by the reap's cascade, and by the
// orphan sweep when that cascade was interrupted.
func TestOwnerRowGoesWithItsClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)

		reaped := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})
		kept := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})
		removeReapedNetworkClientDependents(self.ctx, []server.Id{*reaped.ClientId}, nil)
		if row := self.ownerRow(t, *reaped.ClientId); row != nil {
			t.Fatalf("the reap kept the row: %+v", row)
		}

		orphan := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test", AdminRefused: true})
		deleteNetworkClientRow(self.ctx, *orphan.ClientId)
		SweepOrphanNetworkClientData(self.ctx, 1000)
		if row := self.ownerRow(t, *orphan.ClientId); row != nil {
			t.Fatalf("the sweep kept the orphan row: %+v", row)
		}
		if row := self.ownerRow(t, *kept.ClientId); row == nil {
			t.Fatal("the sweep removed the row of a live client")
		}
	})
}

// A session as an API key authenticates one: the network's identity, with the key
// beside it and not in its claims.
func (self *clientOwnerFixture) apiKeySession(t testing.TB, apiKeyId server.Id) *session.ClientSession {
	t.Helper()
	byJwt := session.NewByJwt(self.networkId, self.userId, "owner", false, false)
	apiKeySession := session.NewLocalClientSession(self.ctx, "192.0.2.3:1", byJwt)
	apiKeySession.ApiKeyAuthenticated = true
	apiKeySession.ApiKeyId = &apiKeyId
	t.Cleanup(apiKeySession.Cancel)
	return apiKeySession
}

func (self *clientOwnerFixture) createApiKey(t testing.TB) server.Id {
	t.Helper()
	result, err := CreateApiKey(&CreateApiKeyArgs{Name: "backend"}, self.root)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error == nil, true)
	return result.Id
}

func (self *clientOwnerFixture) authCode(t testing.TB, creator *session.ClientSession) string {
	t.Helper()
	result, err := AuthCodeCreate(&AuthCodeCreateArgs{Uses: 3, DurationMinutes: 10}, creator)
	if err != nil || result == nil || result.Error != nil || result.AuthCode == "" {
		t.Fatal("code creation failed", result, err)
	}
	return result.AuthCode
}

// Logs in with the code and returns the network token's claims and its session.
func (self *clientOwnerFixture) login(t testing.TB, code string) (*session.ByJwt, *session.ClientSession) {
	t.Helper()
	login := session.NewLocalClientSession(self.ctx, "192.0.2.4:1", nil)
	t.Cleanup(login.Cancel)
	result, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: code}, login)
	if err != nil || result == nil || result.Error != nil || result.ByJwt == "" {
		t.Fatal("login failed", result, err)
	}
	byJwt, err := session.ParseByJwt(self.ctx, result.ByJwt)
	connect.AssertEqual(t, err, nil)
	loginSession := session.NewLocalClientSession(self.ctx, "192.0.2.5:1", byJwt)
	t.Cleanup(loginSession.Cancel)
	return byJwt, loginSession
}

func (self *clientOwnerFixture) requireLoginRefused(t testing.TB, code string) {
	t.Helper()
	login := session.NewLocalClientSession(self.ctx, "192.0.2.4:1", nil)
	defer login.Cancel()
	result, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: code}, login)
	if err != nil || result == nil || result.Error == nil || result.ByJwt != "" {
		t.Fatal("a code with no key logged in", result, err)
	}
}

func (self *clientOwnerFixture) requireTokenLive(t testing.TB, byJwt *session.ByJwt, live bool) {
	t.Helper()
	err := session.ValidateByJwtState(self.ctx, byJwt, false)
	if live && err != nil {
		t.Fatalf("a live token was refused: %s", err)
	}
	if !live && err == nil {
		t.Fatal("a token outlived its key")
	}
}

// server/session/AUTH.md's gap, closed: a code carries its key's id, the token it
// mints carries it too, and removing the key ends the token and refuses the code.
func TestRemovingAnApiKeyEndsItsAuthCodeLoginToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)
		creator := self.apiKeySession(t, apiKeyId)
		code := self.authCode(t, creator)

		var stored *server.Id
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT api_key_id FROM auth_code WHERE auth_code = $1`, code).Scan(&stored))
		})
		connect.AssertEqual(t, stored != nil && *stored == apiKeyId, true)

		byJwt, _ := self.login(t, code)
		connect.AssertEqual(t, byJwt.ApiKeyId != nil && *byJwt.ApiKeyId == apiKeyId, true)
		self.requireTokenLive(t, byJwt, true)

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		self.requireTokenLive(t, byJwt, false)
		self.requireLoginRefused(t, code)

		// the code's rows died with the key (§2.8 api-11)
		var codeCount int
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT COUNT(*) FROM auth_code WHERE api_key_id = $1`, apiKeyId).Scan(&codeCount))
		})
		connect.AssertEqual(t, codeCount, 0)
	})
}

// Removing a key does not rest on the delete finding every code: a code that
// survives its key, as one created in a race with the delete can, still refuses.
func TestCodeLoginRefusesACodeWhoseKeyIsGone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)
		code := self.authCode(t, self.apiKeySession(t, apiKeyId))

		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `DELETE FROM account_api_key WHERE api_key_id = $1`, apiKeyId))
		})
		self.requireLoginRefused(t, code)
	})
}

// Every credential that descends from the login ends with the key: its renewal, a
// client it minted, and a second code and the login that one mints. Otherwise the
// login would hand its authority to something the removal cannot reach.
func TestRemovingAnApiKeyEndsEverythingDescendedFromItsLogin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)
		byJwt, loginSession := self.login(t, self.authCode(t, self.apiKeySession(t, apiKeyId)))

		renewed := byJwt.Renew()
		connect.AssertEqual(t, renewed.ApiKeyId != nil && *renewed.ApiKeyId == apiKeyId, true)

		client := self.mint(t, loginSession, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		clientByJwt, err := session.ParseByJwt(self.ctx, *client.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, clientByJwt.ApiKeyId != nil && *clientByJwt.ApiKeyId == apiKeyId, true)

		secondByJwt, _ := self.login(t, self.authCode(t, loginSession))
		connect.AssertEqual(t, secondByJwt.ApiKeyId != nil && *secondByJwt.ApiKeyId == apiKeyId, true)
		pendingCode := self.authCode(t, loginSession)

		for _, live := range []*session.ByJwt{renewed, clientByJwt, secondByJwt} {
			self.requireTokenLive(t, live, true)
		}

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		for _, ended := range []*session.ByJwt{renewed, clientByJwt, secondByJwt} {
			self.requireTokenLive(t, ended, false)
		}
		self.requireLoginRefused(t, pendingCode)
	})
}

// What a key mints directly is not a login: an embed backend's client tokens name
// no key, so rotating the key leaves its installations running, as it always has.
func TestAClientMintedByAnApiKeyOutlivesTheKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)

		client := self.mint(t, self.apiKeySession(t, apiKeyId), &AuthNetworkClientArgs{Description: "installation", DeviceSpec: "embed"})
		byJwt, err := session.ParseByJwt(self.ctx, *client.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byJwt.ApiKeyId == nil, true)

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		self.requireTokenLive(t, byJwt, true)
	})
}

// A code the root token created names no key and removing a key leaves it alone,
// and so does removing another network's key by its id.
func TestRemovingAnApiKeyLeavesOtherCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)
		otherKeyId := self.createApiKey(t)

		rootCode := self.authCode(t, self.root)
		otherKeyCode := self.authCode(t, self.apiKeySession(t, otherKeyId))
		rootLogin, _ := self.login(t, rootCode)
		connect.AssertEqual(t, rootLogin.ApiKeyId == nil, true)
		otherLogin, _ := self.login(t, otherKeyCode)

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		self.requireTokenLive(t, rootLogin, true)
		self.requireTokenLive(t, otherLogin, true)
		self.login(t, rootCode)
		self.login(t, otherKeyCode)

		stranger := newClientOwnerFixture(t)
		connect.AssertEqual(t, DeleteApiKey(&otherKeyId, stranger.root), nil)
		self.requireTokenLive(t, otherLogin, true)
	})
}

// A tagged credential answered by the Redis revocation record would never reach
// the query that reads the key, so a credential that names a key stays on the
// query, and the removal holds with the record on.
func TestRemovingAnApiKeyEndsItsLoginUnderTheRevocationRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetRevocationRecordEnabled(true)()

		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)
		byJwt, _ := self.login(t, self.authCode(t, self.apiKeySession(t, apiKeyId)))
		connect.AssertEqual(t, byJwt.SessionId != nil, true)
		self.requireTokenLive(t, byJwt, true)

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		self.requireTokenLive(t, byJwt, false)
	})
}

// A redemption survives a lost response by its request id, and the retry reads the
// stored credential: once the key is gone that answer ends too.
func TestRedemptionRetryEndsWithTheKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		apiKeyId := self.createApiKey(t)
		code := self.authCode(t, self.apiKeySession(t, apiKeyId))

		requestId := server.NewId()
		login := session.NewLocalClientSession(self.ctx, "192.0.2.6:1", nil)
		defer login.Cancel()
		args := &AuthCodeLoginArgs{AuthCode: code, RequestId: &requestId}
		first, err := AuthCodeLogin(args, login)
		if err != nil || first == nil || first.Error != nil {
			t.Fatal("redemption failed", first, err)
		}
		retried, err := AuthCodeLogin(args, login)
		if err != nil || retried == nil || retried.ByJwt != first.ByJwt {
			t.Fatal("a retry before the removal changed the answer", retried, err)
		}

		// the stored credential names the key
		var encoded []byte
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT credential FROM auth_code_redemption WHERE request_id = $1`, requestId).Scan(&encoded))
		})
		stored := &session.ByJwt{}
		server.Raise(json.Unmarshal(encoded, stored))
		connect.AssertEqual(t, stored.ApiKeyId != nil && *stored.ApiKeyId == apiKeyId, true)

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		if retried, err := AuthCodeLogin(args, login); err == nil && retried != nil && retried.ByJwt != "" {
			t.Fatal("a retry after the removal returned the token")
		}
	})
}
