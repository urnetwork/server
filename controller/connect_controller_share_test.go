//go:build sidecar_pending

package controller

// §6.10.1's share rules at CreateContract (PROTO-M6-01; D-PROTO-9, X11, C13), checked across ACL's gate and this
// code: §22.5's §8 row "no Network-mode share contract", a share's companion carrying its forward contract's seals,
// and only the dialer's network escrowed in both directions. ACL-M6-03's rule, which turns a Public relationship
// with a share into FriendsAndFamily and moves its escrow to the dialer's network, is not built yet, so a
// cross-network pair's contracts here are Public, the path a share's take until that rule lands; when it lands,
// these cases hold for FriendsAndFamily unchanged, with the destination's FriendsAndFamily key in place of its
// Public one. On the private stack, with the seal test's fixtures.

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/model"
)

// A cross-network pair never gets Network, though its destination provides Network and FriendsAndFamily beside
// Public: the forward contract is Public, and the destination's companion stays a Stream companion, never normalized
// to Network (D-PROTO-9).
func TestCreateContractShareNeverNetwork(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		x := newAclGateTestNet(t, ctx, states, false)
		y := newAclGateTestNet(t, ctx, states, false)
		dialer := x.client(nil)
		target := y.client(nil)
		sealTestProvide(ctx, dialer, model.ProvideModeNetwork, model.ProvideModeStream)
		sealTestProvide(ctx, target, model.ProvideModeNetwork, model.ProvideModeFriendsAndFamily, model.ProvideModePublic, model.ProvideModeStream)

		forward, _ := sealTestCreate(t, ctx, dialer, &protocol.CreateContract{DestinationId: target.Bytes()})
		if forward.ProvideMode == protocol.ProvideMode_Network {
			t.Fatalf("a cross-network forward contract is Network")
		}
		companion, _ := sealTestCreate(t, ctx, target, &protocol.CreateContract{DestinationId: dialer.Bytes(), Companion: true})
		if companion.ProvideMode == protocol.ProvideMode_Network {
			t.Fatalf("a cross-network companion was normalized to Network")
		}
	})
}

// A share's companion (return) contract carries its forward contract's seals: the dialer's subject and network, and
// the forward's class.
func TestCreateContractShareCompanionKeepsTheForwardSeals(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		x := newAclGateTestNet(t, ctx, states, false)
		y := newAclGateTestNet(t, ctx, states, false)
		dialer := x.client(nil)
		target := y.client(nil)
		sealTestProvide(ctx, dialer, model.ProvideModeNetwork, model.ProvideModeStream)
		sealTestProvide(ctx, target, model.ProvideModePublic, model.ProvideModeStream)

		_, forward := sealTestCreate(t, ctx, dialer, &protocol.CreateContract{DestinationId: target.Bytes()})
		_, companion := sealTestCreate(t, ctx, target, &protocol.CreateContract{DestinationId: dialer.Bytes(), Companion: true})
		if !bytes.Equal(companion.SourceSubjectClientId, forward.SourceSubjectClientId) || !bytes.Equal(companion.SourceNetworkId, forward.SourceNetworkId) ||
			companion.ContractClass != forward.ContractClass {
			t.Errorf("the companion's seals %x %x %s differ from the forward's %x %x %s",
				companion.SourceSubjectClientId, companion.SourceNetworkId, companion.ContractClass,
				forward.SourceSubjectClientId, forward.SourceNetworkId, forward.ContractClass)
		}
		if !bytes.Equal(forward.SourceNetworkId, x.networkId.Bytes()) {
			t.Errorf("the forward's network seal %x is not the dialer's", forward.SourceNetworkId)
		}
	})
}

// Only the dialer's network is escrowed, for both directions (owner decision 11): the forward contract and the
// destination's companion each reserve from the dialer's network, and the destination's network holds nothing.
func TestCreateContractShareEscrowsTheDialerAlone(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		x := newAclGateTestNet(t, ctx, states, false)
		y := newAclGateTestNet(t, ctx, states, false)
		dialer := x.client(nil)
		target := y.client(nil)
		sealTestProvide(ctx, dialer, model.ProvideModeNetwork, model.ProvideModeStream)
		sealTestProvide(ctx, target, model.ProvideModePublic, model.ProvideModeStream)

		sealTestCreate(t, ctx, dialer, &protocol.CreateContract{DestinationId: target.Bytes()})
		afterForward := model.GetOpenTransferByteCount(ctx, x.networkId)
		if afterForward <= 0 {
			t.Fatalf("the forward contract escrowed nothing from the dialer's network")
		}
		sealTestCreate(t, ctx, target, &protocol.CreateContract{DestinationId: dialer.Bytes(), Companion: true})
		if afterCompanion := model.GetOpenTransferByteCount(ctx, x.networkId); afterCompanion <= afterForward {
			t.Errorf("the companion escrowed nothing more from the dialer's network: %d after %d", afterCompanion, afterForward)
		}
		if held := model.GetOpenTransferByteCount(ctx, y.networkId); held != 0 {
			t.Errorf("the destination's network holds %d", held)
		}
	})
}
