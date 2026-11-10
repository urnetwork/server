//go:build sidecar_pending

package controller

// §22.5's §8.10 rows through CreateContract on the private stack (ACL-M4-01): forward, a companion checked reversed, a
// reply on its origin, the head-row miss, the witness exception's control and its refusal for a revoked, parked or
// suspended peer, each NoPermission refusal, D-ACL-5's legacy rule, a window child acting as its sidecar, and the
// relationship's one query carrying the gate's facts. Each network's state is hand-made (sidecartest's fakes), and its
// head row is network_acl_head's, which the gate's heads snapshot reads. MG0, MG1 and MG2 hold the tables, so these run
// under -tags sidecar_pending, through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// What a client's owner row says.
type aclGateTestOwner struct {
	memberId         *server.Id
	tagged           bool
	strictExp        bool
	aclEnforcing     bool
	declaresNoEgress bool
}

// One network for the gate's cases: its clients in Postgres, its hand-made state and, when asked, its head row.
type aclGateTestNet struct {
	t         testing.TB
	ctx       context.Context
	networkId server.Id
	owner     sidecar.Id
	view      *sidecartest.FakeAclView
	rows      *sidecarprotocol.AclRows
	atoms     map[string]bool
	states    *sidecartest.FakeAclContractStates
}

func newAclGateTestNet(t testing.TB, ctx context.Context, states *sidecartest.FakeAclContractStates, head bool) *aclGateTestNet {
	t.Helper()
	self := &aclGateTestNet{
		t:         t,
		ctx:       ctx,
		networkId: server.NewId(),
		view:      sidecartest.NewFakeAclView(),
		rows:      &sidecarprotocol.AclRows{HasDocument: true},
		atoms:     map[string]bool{},
		states:    states,
	}
	self.owner = self.view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER)
	model.Testing_CreateNetwork(ctx, self.networkId, "acl-gate-"+self.networkId.String(), server.NewId())
	server.Raise(model.AddBasicTransferBalance(ctx, self.networkId, 256*1024*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
	if head {
		chain := sha256.Sum256(self.networkId.Bytes())
		if _, err := model.AdvanceNetworkAclHead(ctx, self.networkId, 1, chain[:], true); err != nil {
			t.Fatal(err)
		}
	}
	return self
}

// A client of the network providing Network, with an owner row when owner is set.
func (self *aclGateTestNet) client(owner *aclGateTestOwner) server.Id {
	self.t.Helper()
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, self.networkId, server.NewId(), clientId, "synthetic", "synthetic")
	if owner != nil {
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
					INSERT INTO network_client_owner (client_id, member_id, tagged, admin_refused, strict_exp, acl_enforcing, declares_no_egress)
					VALUES ($1, $2, $3, true, $4, $5, $6)
				`,
				clientId,
				owner.memberId,
				owner.tagged,
				owner.strictExp,
				owner.aclEnforcing,
				owner.declaresNoEgress,
			))
		})
	}
	model.SetProvide(self.ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModeNetwork: bytes.Repeat([]byte{7}, 32)})
	return clientId
}

// A sidecar's own client: strict_exp, bound to sidecarId.
func (self *aclGateTestNet) sidecarClient(sidecarId sidecar.Id) server.Id {
	self.t.Helper()
	clientId := self.client(&aclGateTestOwner{strictExp: true})
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`
				INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
				VALUES ($1, 1, $2, $3, $4, $5)
			`,
			server.Id(sidecarId),
			clientId,
			bytes.Repeat([]byte{1}, 32),
			[]byte{2},
			server.NowUtc(),
		))
	})
	return clientId
}

// An app device of the network's owner member, enforcing or not.
func (self *aclGateTestNet) appClient(aclEnforcing bool) server.Id {
	memberId := server.Id(self.owner)
	return self.client(&aclGateTestOwner{memberId: &memberId, aclEnforcing: aclEnforcing})
}

// A child of parent whose stored subject is subject, nil leaving it unresolved, as a client minted before MG2.
func (self *aclGateTestNet) child(parent server.Id, subject *server.Id) server.Id {
	self.t.Helper()
	clientId := self.client(nil)
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`UPDATE network_client SET source_client_id = $2, acl_subject_client_id = $3 WHERE client_id = $1`,
			clientId,
			parent,
			subject,
		))
	})
	return clientId
}

// A rule letting src's sidecar dial targets.
func (self *aclGateTestNet) dial(src sidecar.Id, targets ...*sidecarprotocol.AclTarget) *sidecarprotocol.AclRule {
	atom := sidecartest.FakeAclSidecarAtom(src)
	if !self.atoms[string(atom.Atom)] {
		self.atoms[string(atom.Atom)] = true
		self.rows.Atoms = append(self.rows.Atoms, atom)
	}
	return self.rule([][]byte{atom.Atom}, targets...)
}

// A dial rule from src atoms.
func (self *aclGateTestNet) rule(srcAtoms [][]byte, targets ...*sidecarprotocol.AclTarget) *sidecarprotocol.AclRule {
	rule := &sidecarprotocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		SrcAtoms: srcAtoms,
		Targets:  targets,
		Actions:  []uint32{uint32(sidecarprotocol.AclAction_ACL_ACTION_DIAL)},
	}
	self.rows.Rules = append(self.rows.Rules, rule)
	return rule
}

// Installs the network's state as the rows and view now stand.
func (self *aclGateTestNet) install() {
	self.t.Helper()
	if err := self.states.Install(sidecar.Id(self.networkId), self.rows, self.view); err != nil {
		self.t.Fatal(err)
	}
}

// The process's gate over states, its one heads load read from network_acl_head before it returns, switched on in
// sidecar.yml and installed until the test ends. Heads load only at construction, so no background read crosses a
// tripwire.
func testingAclContractGate(t testing.TB, ctx context.Context, states sidecar.AclContractStateSource) *sidecar.AclContractGate {
	t.Helper()
	loads := make(chan error, 8)
	settings := sidecar.DefaultAclContractGateSettings()
	settings.HeadsInterval = 0
	settings.OnHeads = func(networkCount int, err error) {
		loads <- err
	}
	gate := NewAclContractGate(ctx, states, settings)
	t.Cleanup(gate.Close)
	select {
	case err := <-loads:
		if err != nil {
			t.Fatalf("heads: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no heads load")
	}
	t.Cleanup(SetAclContractGate(gate))
	t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: true}\n"))
	return gate
}

// CreateContract from source to destination, its result.
func testingAclCreateContract(t testing.TB, ctx context.Context, source server.Id, destination server.Id, companion bool) *protocol.CreateContractResult {
	t.Helper()
	frames, err := CreateContract(ctx, source, &protocol.CreateContract{
		DestinationId:     destination.Bytes(),
		TransferByteCount: 1024 * 1024,
		Companion:         companion,
	}, connect.DefaultContractManagerSettings())
	if err != nil || len(frames) != 1 {
		t.Fatalf("create contract: %d frames, %v", len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	returnConnectControlFrames(frames)
	if err != nil {
		t.Fatal(err)
	}
	return message.(*protocol.CreateContractResult)
}

func testingAclContractCreated(result *protocol.CreateContractResult) bool {
	return result.Error == nil && result.Contract != nil
}

func testingAclContractRefused(result *protocol.CreateContractResult) bool {
	return result.Contract == nil && result.Error != nil && *result.Error == protocol.ContractError_NoPermission
}

// The gate's verdict for a request, from the relationship's one query, as CreateContract decides it.
func testingAclVerdict(t testing.TB, ctx context.Context, gate *sidecar.AclContractGate, source server.Id, destination server.Id, companion bool) sidecar.AclContractVerdict {
	t.Helper()
	details := getProvideRelationshipDetailsFor(ctx, source, destination, true)
	return decideContractAcl(gate, server.CurrentSidecarConfig(), source, destination, &protocol.CreateContract{Companion: companion}, &details, server.NowUtc())
}

// The test env with no reruns and a live origin notifications owner, as a resident's.
func testingAclRun(t *testing.T, run func(t testing.TB, ctx context.Context)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		if !server.SidecarMg0Linked() || !server.SidecarMg1Linked() || !server.SidecarMg2Linked() {
			t.Fatal("a pending build does not link MG0, MG1 and MG2")
		}
		notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
		run(t, model.WithContractOriginNotifications(ctx, notifications))
	})
}

// Non-companion a→b needs reach(a,b), and b's later non-companion to a is a reply that rides a's origin.
func TestCreateContractAclForward(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
		app := net.appClient(true)
		net.install()
		gate := testingAclContractGate(t, ctx, states)

		if verdict := testingAclVerdict(t, ctx, gate, a, b, false); verdict.Outcome != sidecar.AclContractAllow || verdict.Class != sidecar.AclContractClassData {
			t.Fatalf("a→b: %+v", verdict)
		}
		if result := testingAclCreateContract(t, ctx, a, b, false); !testingAclContractCreated(result) {
			t.Fatalf("a→b: %+v", result)
		}
		// b reaches a only as a reply, which a's open contract to b carries
		if verdict := testingAclVerdict(t, ctx, gate, b, a, false); verdict.Outcome != sidecar.AclContractReply {
			t.Fatalf("b→a: %+v", verdict)
		}
		if result := testingAclCreateContract(t, ctx, b, a, false); !testingAclContractCreated(result) {
			t.Fatalf("b→a over a's origin: %+v", result)
		}
		// an app device with no grant reaches neither, and is no witness
		for _, pair := range [][2]server.Id{{app, a}, {a, app}} {
			if result := testingAclCreateContract(t, ctx, pair[0], pair[1], false); !testingAclContractRefused(result) {
				t.Errorf("%s→%s: %+v", pair[0], pair[1], result)
			}
		}
	})
}

// A companion a→b answers b, so it needs reach(b,a), the reverse of what it names (D-ACL-4).
func TestCreateContractAclCompanionReversed(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa := net.view.AddDevice(net.owner)
		a := net.sidecarClient(sa)
		app := net.appClient(true)
		// a reaches the app device, which reaches nothing
		net.dial(sa, &sidecarprotocol.AclTarget{ClientId: app.Bytes()})
		net.install()
		gate := testingAclContractGate(t, ctx, states)

		if verdict := testingAclVerdict(t, ctx, gate, app, a, true); verdict.Outcome != sidecar.AclContractAllow || verdict.Reason != sidecar.AclContractReasonReach {
			t.Errorf("the app device's companion to a: %+v", verdict)
		}
		if result := testingAclCreateContract(t, ctx, app, a, true); !testingAclContractCreated(result) {
			t.Errorf("the app device's companion to a: %+v", result)
		}
		if result := testingAclCreateContract(t, ctx, a, app, true); !testingAclContractRefused(result) {
			t.Errorf("a's companion to the app device, which reaches no one: %+v", result)
		}
	})
}

// A network with no head row costs the gate one map miss and reads no state: today's path, untouched (R-ACL-8).
func TestCreateContractAclHeadRowMiss(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		headed := newAclGateTestNet(t, ctx, states, true)
		headed.install()
		net := newAclGateTestNet(t, ctx, states, false)
		a, b := net.client(nil), net.client(nil)
		gate := testingAclContractGate(t, ctx, states)
		if !gate.HasHead(sidecar.Id(headed.networkId)) || gate.HasHead(sidecar.Id(net.networkId)) {
			t.Fatal("the heads snapshot is not network_acl_head's")
		}
		before := gate.Stats()
		for _, companion := range []bool{false, true} {
			if verdict := testingAclVerdict(t, ctx, gate, a, b, companion); verdict.Outcome != sidecar.AclContractSkip || verdict.Reason != sidecar.AclContractReasonNoHead {
				t.Errorf("companion %t: %+v", companion, verdict)
			}
		}
		if result := testingAclCreateContract(t, ctx, a, b, false); !testingAclContractCreated(result) {
			t.Errorf("a→b with no head row: %+v", result)
		}
		if skips := gate.Stats().SkipCount - before.SkipCount; skips != 3 {
			t.Errorf("%d skips", skips)
		}
		for _, networkId := range states.ChangedNetworks() {
			if networkId == sidecar.Id(net.networkId) {
				t.Error("a network with no head row was loaded")
			}
		}
	})
}

// Two admitted sidecars with no grant get a control contract; a revoked, parked or suspended peer gets none.
func TestCreateContractAclControl(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		revoked, parked, suspended := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		net.view.Revoke(revoked)
		net.view.Park(parked)
		net.rows.Suspensions = append(net.rows.Suspensions, &sidecarprotocol.AclSuspension{Subject: suspended.Bytes(), SuspendId: sidecar.NewId().Bytes()})
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		net.install()
		gate := testingAclContractGate(t, ctx, states)

		if verdict := testingAclVerdict(t, ctx, gate, a, b, false); verdict.Outcome != sidecar.AclContractAllow || verdict.Class != sidecar.AclContractClassControl || verdict.Reason != sidecar.AclContractReasonWitness {
			t.Fatalf("two admitted sidecars: %+v", verdict)
		}
		if result := testingAclCreateContract(t, ctx, a, b, false); !testingAclContractCreated(result) {
			t.Fatalf("the control contract: %+v", result)
		}
		for name, peer := range map[string]sidecar.Id{"revoked": revoked, "parked": parked, "suspended": suspended} {
			peerClient := net.sidecarClient(peer)
			for _, pair := range [][2]server.Id{{a, peerClient}, {peerClient, a}} {
				if result := testingAclCreateContract(t, ctx, pair[0], pair[1], false); !testingAclContractRefused(result) {
					t.Errorf("a %s peer: %+v", name, result)
				}
			}
		}
	})
}

// Each NoPermission refusal, none recorded as the route's: a repeat is decided again, never replayed.
func TestCreateContractAclNoPermission(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sb, sn := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
		// a hosted device's client, reaching b by a rule
		sh := net.view.AddDevice(net.owner)
		hosted := net.sidecarClient(sh)
		net.dial(sh, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO proxy_device_config (proxy_id, client_id, instance_id, config_json) VALUES ($1, $2, $3, '{}')`,
				server.NewId(),
				hosted,
				server.NewId(),
			))
		})
		declared := net.client(&aclGateTestOwner{strictExp: true, declaresNoEgress: true})
		noEgress := net.sidecarClient(sn)
		egress, err := proto.Marshal(&bridge.BridgeRows{Egress: []*bridge.BridgeEgressRow{{Subject: sn.Bytes(), Mode: bridge.BridgeEgressMode_BRIDGE_EGRESS_MODE_NONE}}})
		if err != nil {
			t.Fatal(err)
		}
		net.rows.Bridge = egress
		appX, appY := net.appClient(true), net.appClient(true)
		net.install()
		// a network with a head row and no state
		cold := newAclGateTestNet(t, ctx, states, true)
		coldA, coldB := cold.client(nil), cold.client(nil)
		// a Public provider
		providers := newAclGateTestNet(t, ctx, states, false)
		provider := providers.client(nil)
		model.SetProvide(ctx, provider, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{9}, 32)})
		gate := testingAclContractGate(t, ctx, states)
		notifications := model.GetContractOriginNotifications(ctx)

		cases := []struct {
			name        string
			source      server.Id
			destination server.Id
			reason      sidecar.AclContractReason
		}{
			{name: "a hosted device", source: hosted, destination: b, reason: sidecar.AclContractReasonHosted},
			{name: "a declared client's Public contract", source: declared, destination: provider, reason: sidecar.AclContractReasonDeclaredNoEgress},
			{name: "a subject in none", source: noEgress, destination: provider, reason: sidecar.AclContractReasonEgress},
			{name: "no reach and no witness", source: appX, destination: appY, reason: sidecar.AclContractReasonNoReach},
			{name: "no loaded state", source: coldA, destination: coldB, reason: sidecar.AclContractReasonNotLoaded},
		}
		for _, c := range cases {
			if verdict := testingAclVerdict(t, ctx, gate, c.source, c.destination, false); verdict.Outcome != sidecar.AclContractRefuse || verdict.Reason != c.reason {
				t.Errorf("%s: %+v, want %s", c.name, verdict, c.reason)
			}
			before := rejectionCount(t, "internal", "acl_"+c.reason.String(), "false")
			for i := range 2 {
				if result := testingAclCreateContract(t, ctx, c.source, c.destination, false); !testingAclContractRefused(result) {
					t.Errorf("%s, attempt %d: %+v", c.name, i, result)
				}
			}
			if after := rejectionCount(t, "internal", "acl_"+c.reason.String(), "false"); after != before+2 {
				t.Errorf("%s: %v refusals counted, want 2", c.name, after-before)
			}
			if _, recorded := notifications.RouteRejection(c.source, c.destination, false); recorded {
				t.Errorf("%s: recorded as the route's rejection", c.name)
			}
		}
		if !slices.Contains(states.ChangedNetworks(), sidecar.Id(cold.networkId)) {
			t.Error("the cold network was not asked to load")
		}
		// the refusals named nothing a's own contract needs
		if result := testingAclCreateContract(t, ctx, a, b, false); !testingAclContractCreated(result) {
			t.Errorf("a→b: %+v", result)
		}
	})
}

// D-ACL-5: a legacy client is reached only through a rule with ports_limited false, and is a source otherwise.
func TestCreateContractAclLegacy(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sd := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, d := net.sidecarClient(sa), net.sidecarClient(sd)
		limitedLegacy, openLegacy := net.client(nil), net.client(nil)
		enforcing := net.appClient(true)
		limited := net.dial(sa, &sidecarprotocol.AclTarget{ClientId: limitedLegacy.Bytes()}, &sidecarprotocol.AclTarget{ClientId: enforcing.Bytes()})
		limited.PortsLimited = true
		net.dial(sa, &sidecarprotocol.AclTarget{ClientId: openLegacy.Bytes()})
		// every untagged non-sidecar may dial d
		net.rule([][]byte{sidecar.AclAutogroupAtom(sidecarprotocol.AclAutogroup_ACL_AUTOGROUP_UNTAGGED).Bytes()}, &sidecarprotocol.AclTarget{SidecarId: sd.Bytes()})
		net.install()
		testingAclContractGate(t, ctx, states)

		if result := testingAclCreateContract(t, ctx, a, limitedLegacy, false); !testingAclContractRefused(result) {
			t.Errorf("a legacy client under a ports-limited rule: %+v", result)
		}
		if result := testingAclCreateContract(t, ctx, a, enforcing, false); !testingAclContractCreated(result) {
			t.Errorf("an enforcing client under the same rule: %+v", result)
		}
		if result := testingAclCreateContract(t, ctx, a, openLegacy, false); !testingAclContractCreated(result) {
			t.Errorf("a legacy client under an unrestricted rule: %+v", result)
		}
		if result := testingAclCreateContract(t, ctx, limitedLegacy, d, false); !testingAclContractCreated(result) {
			t.Errorf("the legacy client as a source: %+v", result)
		}
	})
}

// A window child acts as the subject its row stores (D-ACL-3), and a child whose subject is unresolved is refused.
func TestCreateContractAclWindowChild(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
		app := net.appClient(true)
		net.install()
		window := net.child(a, &a)
		unresolved := net.child(app, nil)
		gate := testingAclContractGate(t, ctx, states)

		if verdict := testingAclVerdict(t, ctx, gate, window, b, false); verdict.Outcome != sidecar.AclContractAllow || verdict.Reason != sidecar.AclContractReasonReach {
			t.Errorf("a's window child to b: %+v", verdict)
		}
		if result := testingAclCreateContract(t, ctx, window, b, false); !testingAclContractCreated(result) {
			t.Errorf("a's window child to b: %+v", result)
		}
		if verdict := testingAclVerdict(t, ctx, gate, unresolved, b, false); verdict.Outcome != sidecar.AclContractRefuse || verdict.Reason != sidecar.AclContractReasonSubjectUnknown {
			t.Errorf("an unresolved child: %+v", verdict)
		}
	})
}

// The gate's facts ride the relationship's one query: one acquisition, each end's network, subject, binding, owner
// row, hosted mark, roles and principal.
func TestCreateContractAclRelationshipOneQuery(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa := net.view.AddDevice(net.owner)
		a := net.sidecarClient(sa)
		window := net.child(a, &a)
		app := net.appClient(true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_role (client_id, role) VALUES ($1, 'synthetic-b'), ($1, 'synthetic-a')`, app))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET principal = 'synthetic-principal' WHERE client_id = $1`, app))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO proxy_device_config (proxy_id, client_id, instance_id, config_json) VALUES ($1, $2, $3, '{}')`,
				server.NewId(),
				app,
				server.NewId(),
			))
		})

		attempts, stop := server.DenyPostgresForTest(t)
		permitted, revoke := server.PermitPostgresAcquisitionForTest(t, ctx)
		details := model.GetProvideRelationshipDetailsWithAcl(permitted, window, app)
		revoke()
		stop()
		connect.AssertEqual(t, attempts(), uint64(1))
		source, destination := details.SourceAcl, details.DestinationAcl
		if source == nil || destination == nil {
			t.Fatalf("no facts: %+v", details)
		}
		connect.AssertEqual(t, source.NetworkId, net.networkId)
		connect.AssertEqual(t, *source.SubjectClientId, a)
		connect.AssertEqual(t, *source.SidecarId, server.Id(sa))
		connect.AssertEqual(t, source.StrictExp, true)
		connect.AssertEqual(t, source.Hosted, false)
		connect.AssertEqual(t, destination.NetworkId, net.networkId)
		connect.AssertEqual(t, *destination.SubjectClientId, app)
		connect.AssertEqual(t, destination.SidecarId == nil, true)
		connect.AssertEqual(t, *destination.MemberId, server.Id(net.owner))
		connect.AssertEqual(t, destination.AclEnforcing, true)
		connect.AssertEqual(t, destination.Hosted, true)
		connect.AssertEqual(t, destination.Roles, []string{"synthetic-a", "synthetic-b"})
		connect.AssertEqual(t, destination.Principal, "synthetic-principal")

		// the plain read carries none, and a missing client none either
		connect.AssertEqual(t, model.GetProvideRelationshipDetails(ctx, window, app).SourceAcl == nil, true)
		connect.AssertEqual(t, model.GetProvideRelationshipDetailsWithAcl(ctx, server.NewId(), app).SourceAcl == nil, true)
	})
}
