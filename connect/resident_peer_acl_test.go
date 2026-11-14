//go:build sidecar_pending

package connect

// §8.19's peer filter at the resident's send (ACL-M6-04): with the process's contract gate on, a client of a network
// with a head row is sent only the peers it reaches, a network with no head row gets the frames it gets with no gate,
// byte for byte, and a state not loaded sends the reset with no peer. The resident is built as the pool-ownership
// tests build one, its frames taken at the send hook, so no transport runs. Its facts are Postgres rows, and MG0 to
// MG2 hold their tables, so this runs under -tags sidecar_pending through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A network for the filter: its clients in Postgres, its hand-made state, and a head row when asked.
type residentPeerAclNet struct {
	ctx       context.Context
	t         testing.TB
	networkId server.Id
	view      *sidecartest.FakeAclView
	rows      *sidecarprotocol.AclRows
	owner     sidecar.Id
}

func newResidentPeerAclNet(t testing.TB, ctx context.Context, head bool) *residentPeerAclNet {
	t.Helper()
	self := &residentPeerAclNet{
		ctx:       ctx,
		t:         t,
		networkId: server.NewId(),
		view:      sidecartest.NewFakeAclView(),
		rows:      &sidecarprotocol.AclRows{HasDocument: true},
	}
	self.owner = self.view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER)
	model.Testing_CreateNetwork(ctx, self.networkId, "peer-acl-"+self.networkId.String(), server.NewId())
	if head {
		chain := sha256.Sum256(self.networkId.Bytes())
		if _, err := model.AdvanceNetworkAclHead(ctx, self.networkId, 1, chain[:], true); err != nil {
			t.Fatal(err)
		}
	}
	return self
}

// A sidecar of the network and its client: strict_exp, bound.
func (self *residentPeerAclNet) sidecarClient() (sidecar.Id, server.Id) {
	self.t.Helper()
	sidecarId := self.view.AddDevice(self.owner)
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, self.networkId, server.NewId(), clientId, "synthetic", "synthetic")
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`
				INSERT INTO network_client_owner (client_id, member_id, tagged, admin_refused, strict_exp, acl_enforcing)
				VALUES ($1, NULL, false, true, true, true)
			`,
			clientId,
		))
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
	return sidecarId, clientId
}

// A rule letting src dial dst.
func (self *residentPeerAclNet) dial(src sidecar.Id, dst sidecar.Id) {
	atom := sidecartest.FakeAclSidecarAtom(src)
	self.rows.Atoms = append(self.rows.Atoms, atom)
	self.rows.Rules = append(self.rows.Rules, &sidecarprotocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		SrcAtoms: [][]byte{atom.Atom},
		Targets:  []*sidecarprotocol.AclTarget{{SidecarId: dst.Bytes()}},
		Actions:  []uint32{uint32(sidecarprotocol.AclAction_ACL_ACTION_DIAL)},
	})
}

// The process's gate over states, its heads read once, on until the test ends.
func residentPeerAclGate(t testing.TB, ctx context.Context, states sidecar.AclContractStateSource) {
	t.Helper()
	loads := make(chan error, 4)
	settings := sidecar.DefaultAclContractGateSettings()
	settings.HeadsInterval = 0
	settings.OnHeads = func(networkCount int, err error) {
		loads <- err
	}
	gate := controller.NewAclContractGate(ctx, states, settings)
	t.Cleanup(gate.Close)
	select {
	case err := <-loads:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no heads load")
	}
	t.Cleanup(controller.SetAclContractGate(gate))
	t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: true}\n"))
}

// The frames a resident for clientId of networkId sends for one event, each copied before the send returns it.
func residentPeerAclFrames(t testing.TB, ctx context.Context, networkId server.Id, clientId server.Id, event *model.NetworkPeerEvent) []*protocol.Frame {
	t.Helper()
	clientSettings := clientconnect.DefaultClientSettings()
	clientSettings.ControlPingTimeout = 0
	clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	clientSettings.Log = clientconnect.NewNoopLogger()
	client := clientconnect.NewClient(ctx, clientconnect.ControlId, clientconnect.NewNoContractClientOob(), clientSettings)
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	resident := &Resident{ctx: ctx, clientId: clientId, client: client, peerNetworkId: &networkId}
	frames := []*protocol.Frame{}
	resident.beforeListenerFrameSendForTest = func(frame *protocol.Frame) {
		frames = append(frames, &protocol.Frame{MessageType: frame.MessageType, MessageBytes: slices.Clone(frame.MessageBytes)})
	}
	resident.handleNetworkPeerEvent(event)
	return frames
}

// The client ids the frames' NetworkPeersUpdates carry, and whether a reset led them.
func residentPeerAclSent(t testing.TB, frames []*protocol.Frame) (bool, []server.Id) {
	t.Helper()
	reset := false
	clientIds := []server.Id{}
	for i, frame := range frames {
		message, err := clientconnect.FromFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		switch v := message.(type) {
		case *protocol.NetworkPeersReset:
			reset = i == 0
		case *protocol.NetworkPeersUpdate:
			for _, peer := range v.Peers {
				clientIds = append(clientIds, server.Id(peer.ClientId))
			}
		}
	}
	return reset, clientIds
}

// With the gate on, a client of a network with a head row is sent the peers it reaches and not one it does not, and
// a state that is not loaded sends the reset alone.
func TestResidentSendsOnlyReachablePeers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		model.Testing_ClearNetworkPeerAclFacts()
		net := newResidentPeerAclNet(t, ctx, true)
		sa, viewer := net.sidecarClient()
		sb, b := net.sidecarClient()
		_, c := net.sidecarClient()
		net.dial(sa, sb)
		states := sidecartest.NewFakeAclContractStates()
		if err := states.Install(sidecar.Id(net.networkId), net.rows, net.view); err != nil {
			t.Fatal(err)
		}
		residentPeerAclGate(t, ctx, states)

		event := &model.NetworkPeerEvent{
			NetworkPeerEventType: model.NetworkPeerEventTypeReset,
			Peers:                []*model.NetworkPeer{{ClientId: viewer}, {ClientId: b}, {ClientId: c}},
		}
		reset, sent := residentPeerAclSent(t, residentPeerAclFrames(t, ctx, net.networkId, viewer, event))
		if !reset || !slices.Equal(sent, []server.Id{b}) {
			t.Fatalf("sent %v (reset %t), want b alone", sent, reset)
		}

		// the state gone from the cache: the reset alone, never the unfiltered list
		states.Remove(sidecar.Id(net.networkId))
		reset, sent = residentPeerAclSent(t, residentPeerAclFrames(t, ctx, net.networkId, viewer, event))
		if !reset || len(sent) != 0 {
			t.Fatalf("with no state, sent %v (reset %t)", sent, reset)
		}
	})
}

// A network with no head row is sent, with the gate on, exactly the frames it is sent with no gate.
func TestResidentPeersOfANetworkWithNoHeadRowAreTodays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		net := newResidentPeerAclNet(t, ctx, false)
		_, viewer := net.sidecarClient()
		_, b := net.sidecarClient()
		event := &model.NetworkPeerEvent{
			NetworkPeerEventType: model.NetworkPeerEventTypeReset,
			Peers:                []*model.NetworkPeer{{ClientId: viewer}, {ClientId: b, DeviceName: "synthetic"}},
		}
		today := residentPeerAclFrames(t, ctx, net.networkId, viewer, event)

		residentPeerAclGate(t, ctx, sidecartest.NewFakeAclContractStates())
		attempts, stop := server.DenyPostgresForTest(t)
		gated := residentPeerAclFrames(t, ctx, net.networkId, viewer, event)
		stop()
		if attempts() != 0 {
			t.Errorf("a network with no head row read Postgres %d times", attempts())
		}
		if len(today) != len(gated) {
			t.Fatalf("%d frames, %d today", len(gated), len(today))
		}
		for i := range today {
			if today[i].MessageType != gated[i].MessageType || !bytes.Equal(today[i].MessageBytes, gated[i].MessageBytes) {
				t.Errorf("frame %d differs from today's", i)
			}
		}
	})
}
