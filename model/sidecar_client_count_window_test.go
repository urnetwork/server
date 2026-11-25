//go:build sidecar_pending

package model

// §20.8's forwarding row (owner decision 197): a sidecar's links' window clients (§17.5), its forwarding client F and
// F's own window clients inherit the sidecar's strict_exp at mint, and none counts toward sidecar_clients, so a
// sidecar counts once, by its own client. Every client here is minted through AuthNetworkClient as production mints
// it, so its marks are the ones the mint writes. MG0 holds the owner row and MG8 forward_only, so these run under
// -tags sidecar_pending, through .stack/server-test.sh.

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A network's two sidecars and each kind of client the forwarding row names.
type sidecarWindowClients struct {
	// S, a top-level sidecar client bound to its sidecar, and its token's session
	*forwardOnlyFixture
	// an app device client, which the network's root mints admin_refused, with no strict_exp
	appDevice *AuthNetworkClientResult
	// the app's sidecar, which the device's own token mints as its strict_exp child (§5.6's sdk host)
	appSidecar *AuthNetworkClientResult
	// two window clients of S's links, each minted by S's token as S's child
	windows []*AuthNetworkClientResult
	// S's forwarding client F, and a window client of F's links, minted by F's token as F's child
	forwarding       *AuthNetworkClientResult
	forwardingWindow *AuthNetworkClientResult
}

func newSidecarWindowClients(t testing.TB) *sidecarWindowClients {
	t.Helper()
	self := &sidecarWindowClients{forwardOnlyFixture: newForwardOnlyFixture(t)}
	self.appDevice = self.mint(t, self.root, &AuthNetworkClientArgs{Description: "app", DeviceSpec: "test", AdminRefused: true})
	self.appSidecar = self.mint(t, self.sessionOf(t, self.appDevice), &AuthNetworkClientArgs{
		SourceClientId:   self.appDevice.ClientId,
		Description:      "app sidecar",
		DeviceSpec:       "test",
		AdminRefused:     true,
		StrictExp:        true,
		DeclaresNoEgress: true,
	})
	for range 2 {
		self.windows = append(self.windows, self.mint(t, self.sidecarSession, &AuthNetworkClientArgs{
			SourceClientId: self.sidecarClient.ClientId,
			Description:    "window",
			DeviceSpec:     "test",
		}))
	}
	self.forwarding = self.mint(t, self.sidecarSession, self.forwardArgs(testSidecarKey(t)))
	self.forwardingWindow = self.mint(t, self.sessionOf(t, self.forwarding), &AuthNetworkClientArgs{
		SourceClientId: self.forwarding.ClientId,
		Description:    "window",
		DeviceSpec:     "test",
	})
	Testing_ForgetSidecarClientMarks()
	return self
}

// The clients that never count: S's window clients, F and F's window client.
func (self *sidecarWindowClients) exemptClientIds() []server.Id {
	clientIds := []server.Id{}
	for _, window := range self.windows {
		clientIds = append(clientIds, *window.ClientId)
	}
	return append(clientIds, *self.forwarding.ClientId, *self.forwardingWindow.ClientId)
}

// The forwarding row as a table: a sidecar's window and forwarding clients are strict_exp children, as the mint writes
// them, and carry no sidecar mark, while both kinds of sidecar client carry it and the app device does not; as
// children they also take no concurrent slot and stay out of the create cap's count, so all three columns exempt them.
func TestSidecarClientMarkExemptsWindowAndForwardingClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg0Linked() || !server.SidecarMg8Linked() {
			t.Fatal("a pending build does not link both MG0 and MG8")
		}
		self := newSidecarWindowClients(t)

		for _, clientId := range self.exemptClientIds() {
			row := self.ownerRow(t, clientId)
			if row == nil || !row.strictExp || !row.adminRefused {
				t.Fatalf("client %s: owner row %+v, want the strict_exp it inherits", clientId, row)
			}
			_, topLevel, _, peer, _ := GetNetworkPeerProfile(self.ctx, clientId)
			connect.AssertEqual(t, peer != nil, true)
			connect.AssertEqual(t, topLevel, false)
		}

		for _, c := range []struct {
			name     string
			clientId server.Id
			marked   bool
		}{
			{name: "sidecar", clientId: *self.sidecarClient.ClientId, marked: true},
			{name: "app sidecar", clientId: *self.appSidecar.ClientId, marked: true},
			{name: "app device", clientId: *self.appDevice.ClientId, marked: false},
			{name: "window", clientId: *self.windows[0].ClientId, marked: false},
			{name: "second window", clientId: *self.windows[1].ClientId, marked: false},
			{name: "forwarding", clientId: *self.forwarding.ClientId, marked: false},
			{name: "forwarding window", clientId: *self.forwardingWindow.ClientId, marked: false},
		} {
			networkId, marked := getSidecarClientMark(self.ctx, c.clientId)
			if marked != c.marked || networkId != self.networkId {
				t.Errorf("%s: marked %t in network %s, want %t in %s", c.name, marked, networkId, c.marked, self.networkId)
			}
		}

		// the create cap counts the top-level clients alone: S and the app device
		var count int
		server.Tx(self.ctx, func(tx server.PgTx) {
			count = countNetworkActiveTopLevelClients(self.ctx, tx, self.networkId, 100)
		})
		connect.AssertEqual(t, count, 2)

		// the concurrent gate passes them with the network's one slot taken
		defer Testing_SetEnforceConcurrentClients(true)()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		AddNetworkPeer(self.ctx, self.networkId, &NetworkPeer{ClientId: *self.appDevice.ClientId}, server.NewId(), time.Hour)
		for _, clientId := range self.exemptClientIds() {
			connect.AssertEqual(t, CanConnectNetworkPeer(self.ctx, clientId, false), true)
		}
	})
}

// A network at its sidecar_clients limit, both its sidecars connected: each window client and the forwarding client
// still opens at connect's gate, which adds no entry for it, and is not counted by its resident, the resident's
// question costing no query after the gate's; and a further sidecar, top-level or a device's child, is refused at
// enrollment's check and at connect, while the connected sidecars stay.
func TestSidecarClientsLimitStillOpensWindowClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newSidecarWindowClients(t)
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 2})()
		ttl := time.Hour

		for _, clientId := range []server.Id{*self.sidecarClient.ClientId, *self.appSidecar.ClientId} {
			admission := AdmitSidecarClient(self.ctx, clientId, ttl)
			connect.AssertEqual(t, admission.Admitted, true)
			connect.AssertEqual(t, admission.Added, true)
		}
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(self.ctx, self.networkId), int64(2))

		for _, clientId := range self.exemptClientIds() {
			admission := AdmitSidecarClient(self.ctx, clientId, ttl)
			if !admission.Admitted || admission.NetworkId != nil || admission.Added {
				t.Errorf("client %s at the sidecar_clients limit: %+v, want admitted with no entry", clientId, admission)
			}
			attempts, stop := server.DenyPostgresForTest(t)
			counts := CountsAsSidecarClient(self.ctx, clientId)
			stop()
			connect.AssertEqual(t, counts, false)
			connect.AssertEqual(t, attempts(), uint64(0))
		}
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(self.ctx, self.networkId), int64(2))
		connect.AssertEqual(t, CountsAsSidecarClient(self.ctx, *self.sidecarClient.ClientId), true)
		connect.AssertEqual(t, CountsAsSidecarClient(self.ctx, *self.appSidecar.ClientId), true)

		refusal := SidecarClientsExceeded(self.ctx, self.networkId)
		if refusal == nil {
			t.Fatal("a third sidecar was admitted at enrollment")
		}
		connect.AssertEqual(t, refusal.LimitKey, "sidecar_clients")
		connect.AssertEqual(t, refusal.Limit, int64(2))
		connect.AssertEqual(t, refusal.Used, int64(2))

		further := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "third sidecar", DeviceSpec: "test", StrictExp: true, DeclaresNoEgress: true})
		connect.AssertEqual(t, AdmitSidecarClient(self.ctx, *further.ClientId, ttl).Admitted, false)
		furtherChild := self.mint(t, self.sessionOf(t, self.appDevice), &AuthNetworkClientArgs{
			SourceClientId: self.appDevice.ClientId,
			Description:    "second app sidecar",
			DeviceSpec:     "test",
			AdminRefused:   true,
			StrictExp:      true,
		})
		connect.AssertEqual(t, AdmitSidecarClient(self.ctx, *furtherChild.ClientId, ttl).Admitted, false)
		connect.AssertEqual(t, AdmitSidecarClient(self.ctx, *self.sidecarClient.ClientId, ttl).Admitted, true)
		connect.AssertEqual(t, AdmitSidecarClient(self.ctx, *self.appSidecar.ClientId, ttl).Admitted, true)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(self.ctx, self.networkId), int64(2))

		// dark: nothing is counted, with no I/O
		Testing_SetEnforceSidecarLimits(false)
		attempts, stop := server.DenyPostgresForTest(t)
		connect.AssertEqual(t, CountsAsSidecarClient(self.ctx, *self.sidecarClient.ClientId), false)
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
	})
}

// A child whose source row is gone, as the child reap leaves one when it removes a stale source: F stays exempt by its
// forward_only and F's window client stays the child of a strict_exp client, while an ordinary strict_exp child, whose
// lineage is lost, counts.
func TestSidecarClientMarkOfAChildWhoseSourceIsGone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newSidecarWindowClients(t)
		sidecarClientId := *self.sidecarClient.ClientId
		deleteNetworkClientRow(self.ctx, sidecarClientId)
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `DELETE FROM network_client_owner WHERE client_id = $1`, sidecarClientId))
		})
		Testing_ForgetSidecarClientMarks()

		for _, c := range []struct {
			name     string
			clientId server.Id
			marked   bool
		}{
			{name: "forwarding", clientId: *self.forwarding.ClientId, marked: false},
			{name: "forwarding window", clientId: *self.forwardingWindow.ClientId, marked: false},
			{name: "window", clientId: *self.windows[0].ClientId, marked: true},
		} {
			_, marked := getSidecarClientMark(self.ctx, c.clientId)
			if marked != c.marked {
				t.Errorf("%s: marked %t, want %t", c.name, marked, c.marked)
			}
		}
	})
}
