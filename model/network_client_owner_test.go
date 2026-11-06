package model

import (
	"context"
	"testing"

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
