//go:build sidecar_pending

package model

// §8.19's peer-list filter (ACL-M6-04): a peer the client cannot reach is not sent, nor its leaving; a grant's
// activation and revocation change the list, each a new view the resident's recheck sends again; a network with no
// head row keeps today's list, element for element; a state not loaded, or facts not read, send no peers; an isolated
// client stays unlisted; and the facts come in one query a batch and, once settled, from memory. The filter's tests
// build their rows by hand (sidecartest's fakes); the facts' read needs MG0 to MG2, so these run under
// -tags sidecar_pending through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A heads source a test sets.
type peerAclTestHeads map[sidecar.Id]uint64

func (self peerAclTestHeads) Heads(ctx context.Context) (map[sidecar.Id]uint64, error) {
	return maps.Clone(self), nil
}

// A gate over states whose one heads load has finished.
func peerAclTestGate(t testing.TB, states sidecar.AclContractStateSource, heads peerAclTestHeads) *sidecar.AclContractGate {
	t.Helper()
	loads := make(chan error, 4)
	settings := sidecar.DefaultAclContractGateSettings()
	settings.HeadsInterval = 0
	settings.OnHeads = func(networkCount int, err error) {
		loads <- err
	}
	gate := sidecar.NewAclContractGate(context.Background(), states, heads, nil, settings)
	t.Cleanup(gate.Close)
	select {
	case err := <-loads:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no heads load")
	}
	return gate
}

// One network's rows and admissions, and its clients' facts as GetNetworkPeerAclEnds would read them.
type peerAclTestNet struct {
	networkId server.Id
	view      *sidecartest.FakeAclView
	rows      *sidecarprotocol.AclRows
	owner     sidecar.Id
	ends      map[server.Id]*ProvideRelationshipAclEnd
}

func newPeerAclTestNet() *peerAclTestNet {
	self := &peerAclTestNet{
		networkId: server.NewId(),
		view:      sidecartest.NewFakeAclView(),
		rows:      &sidecarprotocol.AclRows{HasDocument: true},
		ends:      map[server.Id]*ProvideRelationshipAclEnd{},
	}
	self.owner = self.view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER)
	return self
}

// A sidecar's client: a bound, strict_exp client acting as itself.
func (self *peerAclTestNet) sidecarClient(sidecarId sidecar.Id) server.Id {
	clientId := server.NewId()
	boundId := server.Id(sidecarId)
	self.ends[clientId] = &ProvideRelationshipAclEnd{NetworkId: self.networkId, SubjectClientId: &clientId, SidecarId: &boundId, StrictExp: true}
	return clientId
}

// A client of no sidecar: an app device of the owner, or with no member a legacy client.
func (self *peerAclTestNet) client(memberId *server.Id, aclEnforcing bool) server.Id {
	clientId := server.NewId()
	self.ends[clientId] = &ProvideRelationshipAclEnd{NetworkId: self.networkId, SubjectClientId: &clientId, MemberId: memberId, AclEnforcing: aclEnforcing}
	return clientId
}

// A dial rule from src's sidecar to targets.
func (self *peerAclTestNet) dial(src sidecar.Id, targets ...*sidecarprotocol.AclTarget) *sidecarprotocol.AclRule {
	atom := sidecartest.FakeAclSidecarAtom(src)
	if !slices.ContainsFunc(self.rows.Atoms, func(row *sidecarprotocol.AclAtom) bool { return bytes.Equal(row.Atom, atom.Atom) }) {
		self.rows.Atoms = append(self.rows.Atoms, atom)
	}
	rule := &sidecarprotocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		SrcAtoms: [][]byte{atom.Atom},
		Targets:  targets,
		Actions:  []uint32{uint32(sidecarprotocol.AclAction_ACL_ACTION_DIAL)},
	}
	self.rows.Rules = append(self.rows.Rules, rule)
	return rule
}

// The client ids a filter sent, in order.
func peerAclTestClientIds(peers []*NetworkPeer) []server.Id {
	clientIds := []server.Id{}
	for _, peer := range peers {
		clientIds = append(clientIds, peer.ClientId)
	}
	return clientIds
}

// A client is sent the peers it reaches and its own sidecar's other client, and neither a peer it does not reach, a
// legacy client a ports-limited rule names, a peer the facts do not hold, nor the leaving of any of those.
func TestFilterNetworkPeersSendsOnlyReachablePeers(t *testing.T) {
	net := newPeerAclTestNet()
	sa, sb, sc := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
	viewer, b, c := net.sidecarClient(sa), net.sidecarClient(sb), net.sidecarClient(sc)
	own := net.sidecarClient(sa)
	ownerMemberId := server.Id(net.owner)
	enforcingApp, legacy := net.client(&ownerMemberId, true), net.client(nil, false)
	net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
	limited := net.dial(sa, &sidecarprotocol.AclTarget{ClientId: enforcingApp.Bytes()}, &sidecarprotocol.AclTarget{ClientId: legacy.Bytes()})
	limited.PortsLimited = true
	states := sidecartest.NewFakeAclContractStates()
	if err := states.Install(sidecar.Id(net.networkId), net.rows, net.view); err != nil {
		t.Fatal(err)
	}
	gate := peerAclTestGate(t, states, peerAclTestHeads{sidecar.Id(net.networkId): 1})
	view := gate.PeerView(sidecar.Id(net.networkId))
	if view.Listing != sidecar.AclPeerListingFiltered {
		t.Fatalf("view: %+v", view)
	}

	now := server.NowUtc()
	unknown := server.NewId()
	peers := []*NetworkPeer{
		{ClientId: viewer}, {ClientId: b}, {ClientId: c}, {ClientId: own}, {ClientId: enforcingApp}, {ClientId: legacy},
		{ClientId: unknown},
		networkPeerDisconnectMarker(b, now), networkPeerDisconnectMarker(c, now),
	}
	sent := FilterNetworkPeers(view, viewer, net.ends, peers, now)
	want := []server.Id{viewer, b, own, enforcingApp, b}
	if got := peerAclTestClientIds(sent); !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if sent[len(sent)-1].DisconnectTime == nil {
		t.Error("b's leaving was not sent as its marker")
	}
	// the shared list is untouched
	if len(peers) != 9 || peers[2].ClientId != c {
		t.Error("the filter changed the shared list")
	}
	// a viewer whose facts are unread is sent nothing
	if sent := FilterNetworkPeers(view, server.NewId(), net.ends, peers, now); len(sent) != 0 {
		t.Errorf("a viewer with no facts was sent %d peers", len(sent))
	}
}

// A grant's activation lists the peer and its revocation unlists it, each a view that differs from the one before,
// which is what makes a resident send its whole list again; a suspended viewer is sent no peer.
func TestFilterNetworkPeersFollowsGrants(t *testing.T) {
	net := newPeerAclTestNet()
	sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
	viewer, b := net.sidecarClient(sa), net.sidecarClient(sb)
	networkId := sidecar.Id(net.networkId)
	states := sidecartest.NewFakeAclContractStates()
	if err := states.Install(networkId, net.rows, net.view); err != nil {
		t.Fatal(err)
	}
	gate := peerAclTestGate(t, states, peerAclTestHeads{networkId: 1})
	peers := []*NetworkPeer{{ClientId: b}}
	now := server.NowUtc()

	before := gate.PeerView(networkId)
	if sent := FilterNetworkPeers(before, viewer, net.ends, peers, now); len(sent) != 0 {
		t.Fatalf("before the grant: %v", peerAclTestClientIds(sent))
	}
	net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
	if err := states.Install(networkId, net.rows, net.view); err != nil {
		t.Fatal(err)
	}
	granted := gate.PeerView(networkId)
	if granted == before {
		t.Fatal("the grant's state reads as the same view")
	}
	if sent := FilterNetworkPeers(granted, viewer, net.ends, peers, now); !slices.Equal(peerAclTestClientIds(sent), []server.Id{b}) {
		t.Fatalf("with the grant: %v", peerAclTestClientIds(sent))
	}
	net.rows.Rules = nil
	if err := states.Install(networkId, net.rows, net.view); err != nil {
		t.Fatal(err)
	}
	revoked := gate.PeerView(networkId)
	if revoked == granted {
		t.Fatal("the revocation's state reads as the same view")
	}
	if sent := FilterNetworkPeers(revoked, viewer, net.ends, peers, now); len(sent) != 0 {
		t.Fatalf("after the revocation: %v", peerAclTestClientIds(sent))
	}

	net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
	net.rows.Suspensions = append(net.rows.Suspensions, &sidecarprotocol.AclSuspension{Subject: sa.Bytes(), SuspendId: sidecar.NewId().Bytes()})
	if err := states.Install(networkId, net.rows, net.view); err != nil {
		t.Fatal(err)
	}
	if sent := FilterNetworkPeers(gate.PeerView(networkId), viewer, net.ends, []*NetworkPeer{{ClientId: viewer}, {ClientId: b}}, now); len(sent) != 0 {
		t.Fatalf("a suspended viewer: %v", peerAclTestClientIds(sent))
	}
}

// A network with no head row keeps today's list, element for element; a network with a head row and no verified
// state, or a gate with no heads yet, sends no peer, and the state is asked to load.
func TestFilterNetworkPeersUnfilteredAndUnavailable(t *testing.T) {
	net := newPeerAclTestNet()
	sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
	viewer, b := net.sidecarClient(sa), net.sidecarClient(sb)
	headless := server.NewId()
	states := sidecartest.NewFakeAclContractStates()
	gate := peerAclTestGate(t, states, peerAclTestHeads{sidecar.Id(net.networkId): 1})
	now := server.NowUtc()
	peers := []*NetworkPeer{{ClientId: viewer}, {ClientId: b}, networkPeerDisconnectMarker(server.NewId(), now)}

	unfiltered := gate.PeerView(sidecar.Id(headless))
	if unfiltered.Listing != sidecar.AclPeerListingUnfiltered {
		t.Fatalf("a network with no head row: %+v", unfiltered)
	}
	if sent := FilterNetworkPeers(unfiltered, viewer, nil, peers, now); !slices.Equal(sent, peers) {
		t.Errorf("a network with no head row was sent %v", peerAclTestClientIds(sent))
	}
	if slices.Contains(states.ChangedNetworks(), sidecar.Id(headless)) {
		t.Error("a network with no head row was asked to load")
	}

	unloaded := gate.PeerView(sidecar.Id(net.networkId))
	if unloaded.Listing != sidecar.AclPeerListingUnavailable {
		t.Fatalf("a head row with no state: %+v", unloaded)
	}
	if sent := FilterNetworkPeers(unloaded, viewer, net.ends, peers, now); len(sent) != 0 {
		t.Errorf("a head row with no state was sent %v", peerAclTestClientIds(sent))
	}
	if !slices.Contains(states.ChangedNetworks(), sidecar.Id(net.networkId)) {
		t.Error("the unloaded network was not asked to load")
	}

	held := make(chan struct{})
	defer close(held)
	coldGate := sidecar.NewAclContractGate(context.Background(), states, peerAclTestHeldHeads{held: held}, nil, nil)
	defer coldGate.Close()
	if cold := coldGate.PeerView(sidecar.Id(headless)); cold.Listing != sidecar.AclPeerListingUnavailable {
		t.Errorf("before the first heads load: %+v", cold)
	}
}

// A heads source whose loads wait until held closes.
type peerAclTestHeldHeads struct {
	held chan struct{}
}

func (self peerAclTestHeldHeads) Heads(ctx context.Context) (map[sidecar.Id]uint64, error) {
	select {
	case <-self.held:
	case <-ctx.Done():
	}
	return nil, context.Canceled
}

// The facts' read: each client's subject, binding and marks in one query, nothing for an inactive client or another
// network's, and settled facts from memory after, while a sidecar's client read before its binding is read again.
func TestGetNetworkPeerAclEndsReadsEachClientsFacts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_ClearNetworkPeerAclFacts()
		networkId, otherNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, "peer-acl-"+networkId.String(), server.NewId())
		Testing_CreateNetwork(ctx, otherNetworkId, "peer-acl-"+otherNetworkId.String(), server.NewId())
		memberId := server.NewId()
		device := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
			return clientId
		}
		bound, app, legacy, pending, inactive, other := device(networkId), device(networkId), device(networkId), device(networkId), device(networkId), device(otherNetworkId)
		sidecarId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, owner := range []struct {
				clientId  server.Id
				memberId  *server.Id
				strictExp bool
			}{
				{clientId: bound, strictExp: true},
				{clientId: app, memberId: &memberId},
				{clientId: pending, strictExp: true},
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client_owner (client_id, member_id, tagged, admin_refused, strict_exp, acl_enforcing)
						VALUES ($1, $2, false, true, $3, true)
					`,
					owner.clientId,
					owner.memberId,
					owner.strictExp,
				))
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
					VALUES ($1, 1, $2, $3, $4, $5)
				`,
				sidecarId,
				bound,
				bytes.Repeat([]byte{1}, 32),
				[]byte{2},
				server.NowUtc(),
			))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, inactive))
		})

		clientIds := []server.Id{bound, app, legacy, pending, inactive, other}
		ends, err := GetNetworkPeerAclEnds(ctx, networkId, clientIds)
		if err != nil {
			t.Fatal(err)
		}
		if end := ends[bound]; end == nil || end.SidecarId == nil || *end.SidecarId != sidecarId || !end.StrictExp || *end.SubjectClientId != bound {
			t.Errorf("the sidecar's client: %+v", end)
		}
		if end := ends[app]; end == nil || end.MemberId == nil || *end.MemberId != memberId || end.SidecarId != nil || !end.AclEnforcing {
			t.Errorf("the app device: %+v", end)
		}
		if end := ends[legacy]; end == nil || end.MemberId != nil || end.SidecarId != nil || end.AclEnforcing {
			t.Errorf("the legacy client: %+v", end)
		}
		if end := ends[pending]; end == nil || end.SidecarId != nil || !end.StrictExp {
			t.Errorf("the sidecar's client before its binding: %+v", end)
		}
		if ends[inactive] != nil || ends[other] != nil {
			t.Errorf("an inactive client or another network's has facts: %+v, %+v", ends[inactive], ends[other])
		}
		if contractEnd := ends[bound].ContractEnd(); !contractEnd.Known || contractEnd.SidecarId != sidecar.Id(sidecarId) || !contractEnd.SidecarHeld {
			t.Errorf("the sidecar's client as the gate reads it: %+v", contractEnd)
		}

		// settled facts answer from memory: no Postgres for them
		attempts, stop := server.DenyPostgresForTest(t)
		cached, err := GetNetworkPeerAclEnds(ctx, networkId, []server.Id{bound, app, legacy})
		stop()
		if err != nil || attempts() != 0 || len(cached) != 3 {
			t.Errorf("settled facts read %d times: %d facts, %v", attempts(), len(cached), err)
		}
		// a sidecar's client read before its binding is read again
		attempts, stop = server.DenyPostgresForTest(t)
		_, err = GetNetworkPeerAclEnds(ctx, networkId, []server.Id{pending})
		stop()
		if err == nil || attempts() == 0 {
			t.Errorf("a pending binding answered from memory: %v, %d reads", err, attempts())
		}
	})
}

// An isolated client is registered where no list reads it, so whatever the rows grant, no client is sent it.
func TestIsolatedPeerStaysUnlistedUnderTheFilter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		net := newPeerAclTestNet()
		sa := net.view.AddDevice(net.owner)
		viewer := net.sidecarClient(sa)
		ownerMemberId := server.Id(net.owner)
		isolated := net.client(&ownerMemberId, true)
		net.dial(sa, &sidecarprotocol.AclTarget{ClientId: isolated.Bytes()})
		states := sidecartest.NewFakeAclContractStates()
		if err := states.Install(sidecar.Id(net.networkId), net.rows, net.view); err != nil {
			t.Fatal(err)
		}
		gate := peerAclTestGate(t, states, peerAclTestHeads{sidecar.Id(net.networkId): 1})
		AddNetworkIsolatedPeer(ctx, net.networkId, isolated, time.Minute)
		AddNetworkPeer(ctx, net.networkId, &NetworkPeer{ClientId: viewer}, server.NewId(), time.Minute)

		_, peers := GetNetworkPeers(ctx, net.networkId)
		sent := FilterNetworkPeers(gate.PeerView(sidecar.Id(net.networkId)), viewer, net.ends, peers, server.NowUtc())
		if slices.Contains(peerAclTestClientIds(sent), isolated) || !slices.Contains(peerAclTestClientIds(sent), viewer) {
			t.Errorf("sent %v", peerAclTestClientIds(sent))
		}
	})
}
