//go:build sidecar_pending

package controller

// §8.11's share half of the contract gate through CreateContract on the private stack (ACL-M6-03; §6.10.1 item 4,
// D-ACL-10, owner decision 11): a Public pair whose two halves hold, each in its own network's state, toward a marked
// sidecar is FriendsAndFamily, escrowed from the dialer's network in both directions; an sdk device client never
// gets it, whatever it advertises; a share is never a Network contract; and either half missing leaves today's
// Public path, which a sidecar that provides no Public refuses. Each network's state is hand-made (sidecartest's
// fakes) and its head row network_acl_head's. Run under -tags sidecar_pending through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// x shares its sidecar's client b with y, whose sidecar's client a dials it.
type aclShareTestPair struct {
	x, y       *aclGateTestNet
	sa, sb     sidecar.Id
	a, b       server.Id
	inbound    *sidecarprotocol.AclRule
	outbound   *sidecarprotocol.AclRule
	xNetworkOf *sidecarprotocol.AclAtom
}

func newAclShareTestPair(t testing.TB, ctx context.Context, states *sidecartest.FakeAclContractStates) *aclShareTestPair {
	t.Helper()
	self := &aclShareTestPair{
		x: newAclGateTestNet(t, ctx, states, true),
		y: newAclGateTestNet(t, ctx, states, true),
	}
	self.sa = self.y.view.AddDevice(self.y.owner)
	self.sb = self.x.view.AddDevice(self.x.owner)
	self.a = self.y.sidecarClient(self.sa)
	self.b = self.x.sidecarClient(self.sb)
	// a takes its replies on companion contracts
	model.SetProvide(ctx, self.a, map[model.ProvideMode][]byte{
		model.ProvideModeNetwork: bytes.Repeat([]byte{7}, 32),
		model.ProvideModeStream:  bytes.Repeat([]byte{6}, 32),
	})
	// b provides what a sidecar holding an inbound share provides (PROTO-M6-01's HoldProvideMode)
	model.SetProvide(ctx, self.b, map[model.ProvideMode][]byte{
		model.ProvideModeNetwork:          bytes.Repeat([]byte{7}, 32),
		model.ProvideModeFriendsAndFamily: bytes.Repeat([]byte{8}, 32),
	})
	// y's half: a dials network:x
	self.outbound = self.y.dial(self.sa, &sidecarprotocol.AclTarget{NetworkId: self.x.networkId.Bytes()})
	// x's half: network:y dials b
	self.xNetworkOf = &sidecarprotocol.AclAtom{
		Atom:       sidecar.AclDerivedAtom(sidecar.AclDerivedAtomNetwork, self.y.networkId.Bytes()).Bytes(),
		NetworkIds: [][]byte{self.y.networkId.Bytes()},
	}
	self.x.rows.Atoms = append(self.x.rows.Atoms, self.xNetworkOf)
	self.inbound = self.x.rule([][]byte{self.xNetworkOf.Atom}, &sidecarprotocol.AclTarget{SidecarId: self.sb.Bytes()})
	return self
}

// The network a contract's escrow is reserved from.
func testingAclContractPayer(t testing.TB, ctx context.Context, result *protocol.CreateContractResult) server.Id {
	t.Helper()
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	var payerNetworkId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT payer_network_id FROM transfer_contract WHERE contract_id = $1`,
			server.Id(storedContract.ContractId),
		).Scan(&payerNetworkId))
	})
	return payerNetworkId
}

// Both halves held: a's contract to b is FriendsAndFamily, never Network, escrowed from y, and b's companion back to a
// is escrowed from y too.
func TestCreateContractAclShareIsFriendsAndFamily(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		pair := newAclShareTestPair(t, ctx, states)
		pair.x.install()
		pair.y.install()
		gate := testingAclContractGate(t, ctx, states)

		verdict := testingAclVerdict(t, ctx, gate, pair.a, pair.b, false)
		if !verdict.Shared() || verdict.Decision.Rule.GetRuleId() == nil || verdict.ShareDecision.Rule.GetRuleId() == nil {
			t.Fatalf("a→b: %+v", verdict)
		}
		details := getProvideRelationshipDetailsFor(ctx, pair.a, pair.b, true)
		if !model.AclShareBetween(&details, verdict) || details.Mode != model.ProvideModePublic {
			t.Fatalf("the rule: %+v", details)
		}

		forward := testingAclCreateContract(t, ctx, pair.a, pair.b, false)
		if !testingAclContractCreated(forward) {
			t.Fatalf("a→b: %+v", forward)
		}
		if mode := forward.Contract.ProvideMode; mode != protocol.ProvideMode_FriendsAndFamily {
			t.Fatalf("a→b settled as %s", mode)
		}
		if payer := testingAclContractPayer(t, ctx, forward); payer != pair.y.networkId {
			t.Errorf("a→b escrowed from %s, want the dialer's %s", payer, pair.y.networkId)
		}

		companion := testingAclCreateContract(t, ctx, pair.b, pair.a, true)
		if !testingAclContractCreated(companion) {
			t.Fatalf("b's companion to a: %+v", companion)
		}
		if mode := companion.Contract.ProvideMode; mode == protocol.ProvideMode_Network {
			t.Fatalf("b's companion settled as Network")
		}
		if payer := testingAclContractPayer(t, ctx, companion); payer != pair.y.networkId {
			t.Errorf("b's companion escrowed from %s, want the dialer's %s", payer, pair.y.networkId)
		}
	})
}

// An sdk device client that advertises FriendsAndFamily beside Public gets Public, whatever x's half names.
func TestCreateContractAclShareNeverForADeviceClient(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		pair := newAclShareTestPair(t, ctx, states)
		device := pair.x.appClient(false)
		model.SetProvide(ctx, device, map[model.ProvideMode][]byte{
			model.ProvideModePublic:           bytes.Repeat([]byte{9}, 32),
			model.ProvideModeFriendsAndFamily: bytes.Repeat([]byte{8}, 32),
		})
		pair.x.rule([][]byte{pair.xNetworkOf.Atom}, &sidecarprotocol.AclTarget{ClientId: device.Bytes()})
		pair.x.install()
		pair.y.install()
		gate := testingAclContractGate(t, ctx, states)

		if verdict := testingAclVerdict(t, ctx, gate, pair.a, device, false); verdict.Shared() {
			t.Fatalf("a→device: %+v", verdict)
		}
		result := testingAclCreateContract(t, ctx, pair.a, device, false)
		if !testingAclContractCreated(result) || result.Contract.ProvideMode != protocol.ProvideMode_Public {
			t.Fatalf("a→device: %+v", result)
		}
	})
}

// Either half missing leaves the pair Public, and b, a sidecar providing no Public, refuses it.
func TestCreateContractAclShareNeedsBothHalves(t *testing.T) {
	for _, missing := range []string{"x's", "y's"} {
		testingAclRun(t, func(t testing.TB, ctx context.Context) {
			states := sidecartest.NewFakeAclContractStates()
			pair := newAclShareTestPair(t, ctx, states)
			switch missing {
			case "x's":
				pair.x.rows.Rules = nil
			case "y's":
				pair.y.rows.Rules = nil
			}
			pair.x.install()
			pair.y.install()
			gate := testingAclContractGate(t, ctx, states)

			if verdict := testingAclVerdict(t, ctx, gate, pair.a, pair.b, false); verdict.Shared() || verdict.Reason != sidecar.AclContractReasonPublic {
				t.Fatalf("without %s half: %+v", missing, verdict)
			}
			if result := testingAclCreateContract(t, ctx, pair.a, pair.b, false); !testingAclContractRefused(result) {
				t.Errorf("without %s half: %+v", missing, result)
			}
		})
	}
}

// With no gate installed nothing is a share: the pair takes today's Public path.
func TestCreateContractAclShareNeedsTheGate(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		pair := newAclShareTestPair(t, ctx, states)
		pair.x.install()
		pair.y.install()
		if result := testingAclCreateContract(t, ctx, pair.a, pair.b, false); !testingAclContractRefused(result) {
			t.Errorf("with no gate: %+v", result)
		}
	})
}
