package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The client admin check (network_client_owner_model.go; §7.4's Check). These
// hold whether or not the binary links MG0: the child fact needs only
// network_client.

type clientOwnerFixture struct {
	ctx       context.Context
	networkId server.Id
	userId    server.Id
	// the network's root token, as a session
	root *session.ClientSession
}

func newClientOwnerFixture(t testing.TB) *clientOwnerFixture {
	t.Helper()
	ctx := t.Context()
	networkId := server.NewId()
	userId := server.NewId()
	networkName := "owner-" + networkId.String()
	Testing_CreateNetwork(ctx, networkId, networkName, userId)
	root := session.NewLocalClientSession(ctx, "192.0.2.1:1", session.NewByJwt(networkId, userId, networkName, false, false))
	t.Cleanup(root.Cancel)
	return &clientOwnerFixture{
		ctx:       ctx,
		networkId: networkId,
		userId:    userId,
		root:      root,
	}
}

// Mints a client with args, requiring that it succeeds.
func (self *clientOwnerFixture) mint(t testing.TB, caller *session.ClientSession, args *AuthNetworkClientArgs) *AuthNetworkClientResult {
	t.Helper()
	result, err := AuthNetworkClient(args, caller)
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		t.Fatalf("auth-client = %+v, error = %+v", result, result.Error)
	}
	return result
}

// A session of the token the mint returned.
func (self *clientOwnerFixture) sessionOf(t testing.TB, result *AuthNetworkClientResult) *session.ClientSession {
	t.Helper()
	byJwt, err := session.ParseByJwt(self.ctx, *result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.2:1", byJwt)
	t.Cleanup(clientSession.Cancel)
	return clientSession
}

func deleteNetworkClientRow(ctx context.Context, clientId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id = $1`, clientId))
	})
}

// A child is refused and a top-level client with no owner row is a legacy client,
// which is what the shipped apps are.
func TestClientRefusesAdminRefusesAChild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		parent := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		child := self.mint(t, self.root, &AuthNetworkClientArgs{SourceClientId: parent.ClientId, Description: "window", DeviceSpec: "test"})

		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *parent.ClientId), false)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *child.ClientId), true)
	})
}

// A client the token's network does not have gives no answer, which refuses. Such
// a token is no credential anyway, and a later row must not find a cached no.
func TestClientRefusesAdminRefusesAClientItCannotFind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})

		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, server.NewId()), true)
		// another network's client
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, server.NewId(), *client.ClientId), true)
		// and that was not remembered against the client
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *client.ClientId), false)
	})
}

// A lookup that fails refuses (D-TEAM-1) and is asked again, so a bad moment is
// never remembered as an answer in either direction.
func TestClientRefusesAdminFailsClosedOnALookupError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})

		canceled, cancel := context.WithCancel(self.ctx)
		cancel()
		connect.AssertEqual(t, ClientRefusesAdmin(canceled, self.networkId, *client.ClientId), true)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *client.ClientId), false)
	})
}

// Both facts are immutable, so each answer is kept: with its row gone, this process
// answers from the cache, and a process that never asked has to read again.
func TestClientRefusesAdminCachesItsAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		parent := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		child := self.mint(t, self.root, &AuthNetworkClientArgs{SourceClientId: parent.ClientId, Description: "window", DeviceSpec: "test"})

		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *parent.ClientId), false)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *child.ClientId), true)

		deleteNetworkClientRow(self.ctx, *child.ClientId)
		deleteNetworkClientRow(self.ctx, *parent.ClientId)

		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *parent.ClientId), false)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *child.ClientId), true)

		Testing_ForgetClientRefusesAdmin()
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *parent.ClientId), true)
	})
}

// A credential that names an API key lives while the network has the key, network
// and client token alike, and on a build that does not link MG0 too: the check
// reads account_api_key, which every release has. A key another network holds is
// no key of this one.
func TestStateCheckReadsTheApiKeyAClaimNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		result, err := CreateApiKey(&CreateApiKeyArgs{Name: "backend"}, self.root)
		connect.AssertEqual(t, err, nil)
		apiKeyId := result.Id

		network := session.NewByJwt(self.networkId, self.userId, "owner", false, false)
		network.ApiKeyId = &apiKeyId
		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		clientByJwt := self.sessionOf(t, client).ByJwt
		clientByJwt.ApiKeyId = &apiKeyId

		stranger := newClientOwnerFixture(t)
		foreign := session.NewByJwt(stranger.networkId, stranger.userId, "owner", false, false)
		foreign.ApiKeyId = &apiKeyId
		if err := session.ValidateByJwtState(self.ctx, foreign, false); err == nil {
			t.Fatal("another network's key passed the state check")
		}

		connect.AssertEqual(t, session.ValidateByJwtState(self.ctx, network, false), nil)
		connect.AssertEqual(t, session.ValidateByJwtState(self.ctx, clientByJwt, true), nil)

		connect.AssertEqual(t, DeleteApiKey(&apiKeyId, self.root), nil)
		if err := session.ValidateByJwtState(self.ctx, network, false); err == nil {
			t.Fatal("a network token outlived the key it names")
		}
		if err := session.ValidateByJwtState(self.ctx, clientByJwt, true); err == nil {
			t.Fatal("a client token outlived the key it names")
		}
	})
}

// A full cache costs a read, not an answer (boundedClientMap).
func TestClientAdminCacheIsBounded(t *testing.T) {
	settings := DefaultClientAdminSettings()
	if settings.CachedClientMaxCount <= 0 {
		t.Fatalf("cached clients = %d", settings.CachedClientMaxCount)
	}
	answers := newBoundedClientMap[bool](2)
	first := server.NewId()
	answers.Store(first, true)
	answers.Store(server.NewId(), true)
	answers.Store(server.NewId(), true)
	if _, ok := answers.Load(first); ok {
		t.Fatal("a map past its bound kept its oldest entry")
	}
	connect.AssertEqual(t, answers.Len() <= 2, true)
}

// The preview refusal alone carries a code (owner decision 326): outside
// preview_networks a strict_exp mint is refused with not_preview, a hosted
// proxy's included; in the list a strict_exp proxy is refused with no code; and
// an unmarked mint, or a strict_exp one in the list, is not refused.
func TestRefuseClientMarksCodesThePreviewRefusalAlone(t *testing.T) {
	// the gate stands until stage 2 is live
	defer Testing_SetSidecarStage2Live(false)()
	networkId := server.NewId()
	strictExp := clientMarks{adminRefused: true, strictExp: true}

	func() {
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", server.NewId()))()
		for _, proxy := range []bool{false, true} {
			message, code := refuseClientMarks(networkId, strictExp, proxy)
			if message != "This network cannot hold sidecar clients yet." || code != "not_preview" {
				t.Fatalf("proxy %t: refusal = %q, code %q", proxy, message, code)
			}
			// admin_refused alone needs no preview: it is the apps' mark too
			message, code = refuseClientMarks(networkId, clientMarks{adminRefused: true}, proxy)
			if message != "" || code != "" {
				t.Fatalf("proxy %t: an admin_refused mint was refused: %q, code %q", proxy, message, code)
			}
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId))()
		message, code := refuseClientMarks(networkId, strictExp, true)
		if message != clientStrictExpProxyMessage || code != "" {
			t.Fatalf("a strict_exp proxy = %q, code %q", message, code)
		}
		message, code = refuseClientMarks(networkId, strictExp, false)
		if message != "" || code != "" {
			t.Fatalf("a listed network refused a strict_exp mint: %q, code %q", message, code)
		}
	}()
}

// AuthNetworkClientError as it was before its code (owner decision 326). A
// refusal with no code encodes byte for byte as this does.
type authNetworkClientErrorBeforeCode struct {
	ClientLimitExceeded bool   `json:"client_limit_exceeded"`
	UpgradeRequired     bool   `json:"upgrade_required,omitempty"`
	NoUniqueSupply      bool   `json:"no_unique_supply,omitempty"`
	Message             string `json:"message"`
}

// Requires a refusal with no code whose 200 body, as the route writes it, is byte
// for byte what the same refusal answered before the code; returns the body.
func requireRefusalBodyBeforeCode(t testing.TB, result *AuthNetworkClientResult) string {
	t.Helper()
	if result == nil || result.Error == nil || result.Error.Code != "" || result.ClientId != nil || result.ByClientJwt != nil {
		t.Fatalf("auth-client = %+v", result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(&struct {
		Error *authNetworkClientErrorBeforeCode `json:"error,omitempty"`
	}{
		Error: &authNetworkClientErrorBeforeCode{
			ClientLimitExceeded: result.Error.ClientLimitExceeded,
			UpgradeRequired:     result.Error.UpgradeRequired,
			NoUniqueSupply:      result.Error.NoUniqueSupply,
			Message:             result.Error.Message,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(before) {
		t.Fatalf("body = %s, before the code %s", body, before)
	}
	return string(body)
}

// Owner decision 326 codes the preview refusal alone, so a legacy mint answers
// byte for byte what it did: its client, and each client limit's refusal (the
// top-level cap, the plan's concurrent limit, the proxy cap and the proxy create
// rate), none with a code.
func TestLegacyMintBodiesAreWhatTheyWere(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		self := newClientOwnerFixture(t)
		deviceArgs := &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"}

		device := self.mint(t, self.root, deviceArgs)
		body, err := json.Marshal(device)
		connect.AssertEqual(t, err, nil)
		jwtJson, err := json.Marshal(*device.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		clientIdJson, err := json.Marshal(*device.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, string(body), `{"by_client_jwt":`+string(jwtJson)+`,"client_id":`+string(clientIdJson)+`}`)

		func() {
			defer Testing_SetEnforceConcurrentClients(true)()
			defer Testing_SetConcurrentClientsLimit(1, 1)()

			// the top-level cap: an allowance of one, which the device fills
			// unconnected
			connect.AssertEqual(t, SetNetworkTopLevelClientLimit(self.ctx, self.networkId, 1), nil)
			result, err := AuthNetworkClient(deviceArgs, self.root)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, requireRefusalBodyBeforeCode(t, result), `{"error":{"client_limit_exceeded":true,"message":"Client limit exceeded."}}`)

			// the plan's concurrent limit: the tier's one, which the device fills
			// connected
			ClearNetworkTopLevelClientLimit(self.ctx, self.networkId)
			registerTestPeer(t, self.ctx, self.networkId, *device.ClientId)
			result, err = AuthNetworkClient(deviceArgs, self.root)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error != nil && result.Error.ClientLimitExceeded && result.Error.UpgradeRequired, true)
			requireRefusalBodyBeforeCode(t, result)
		}()

		// the proxy limits, on a network of their own: its creates are not the
		// device network's top-level clients
		_, proxySession := testProxyLimitNetwork(self.ctx)

		// the proxy cap, one active proxy, with the rate lifted
		func() {
			defer Testing_SetProxyClientLimits(1, 1)()
			defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
			connect.AssertEqual(t, testAuthProxyClient(t, proxySession).Error, (*AuthNetworkClientError)(nil))
			result := testAuthProxyClient(t, proxySession)
			connect.AssertEqual(t, result.Error != nil && result.Error.ClientLimitExceeded, true)
			requireRefusalBodyBeforeCode(t, result)
		}()

		// the proxy create rate, one a minute, with the cap lifted: a minute of
		// its own, so the cap's creates above are not counted in it
		func() {
			defer Testing_SetProxyClientLimits(0, 0)()
			defer Testing_SetProxyClientCreatesPerMinute(1, 1)()
			now := time.Unix(1_762_320_010, 0)
			defer Testing_SetProxyClientCreateNow(func() time.Time { return now })()
			connect.AssertEqual(t, testAuthProxyClient(t, proxySession).Error, (*AuthNetworkClientError)(nil))
			result := testAuthProxyClient(t, proxySession)
			connect.AssertEqual(t, result.Error != nil && result.Error.ClientLimitExceeded, true)
			requireRefusalBodyBeforeCode(t, result)
		}()
	})
}
