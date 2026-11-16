//go:build sidecar_pending

package controller

// BRIDGE-M8-07's forwarding rows on T3 (DESIGN-DETAILS 4.6; FS-38, FI-11, D-BRIDGE-17, D-SDK-32, owner decision 197):
// sidecartest's fault builders for FS-38's forwarding-key delegations, the same ones sidecartest runs on T2
// (sidecar/sidecartest/fault/fs_bridge_forwarding_m8.go), on BridgeNet with server's platform (bridgeM4Platform): every
// contract the windows ask for is server's controller's, so C13 seals what server writes from its rows and the
// WindowDelegation a forwarding key signed rides server's contract unchanged to the host's verify. The twin opens its
// flows under the forwarding key's tags; each refused delegation, a key no Delegation names, a superseded one's, a
// revoked one's, another sidecar's, a short key id and a lapse, opens no upstream socket; the operator's minted
// contracts carrying a forged or replayed forwarding delegation open none either, beside server's honest ones; and a
// DelegationRevoke ends the live connection its key admitted while the identity key's carries on.
//
// The authorization log is sidecartest's FakeApi here, as on T2 (connect_bridge_m4_test.go says why). §7.4's
// forward_only rule, that a forwarding client's exchange gets the config's r alone, is server/oauth's
// TestExchangeScopesForwardingClientGetsTheConfigsReadAlone (TEAM-M8-02). Run under -tags sidecar_pending through
// .stack/server-test.sh.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// FS-38's forwarding twin and refusals on server's CreateContract.
func TestFs38ForwardingOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-38's forwarding twin", fault.Fs38ForwardingDelegation)
		bridgeM4RunSchedule(t, ctx, "FS-38's forwarding refusals", fault.Fs38ForwardingCase)
	})
}

// A DelegationRevoke of the subject's forwarding Delegation, and a newer one superseding it, each end exactly the flows
// its key admitted, on server's contracts, beside their twin, which keeps both connections open.
func TestFs38ForwardingRevocationOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-38's forwarding revocation twin", fault.Fs38ForwardingFlowsStayUnrevoked)
		bridgeM4RunSchedule(t, ctx, "FS-38's forwarding revocation", fault.Fs38ForwardingRevokeCase)
		bridgeM4RunSchedule(t, ctx, "FS-38's forwarding supersession", fault.Fs38ForwardingSupersedeCase)
	})
}

// FS-38's forwarding forged case, beyond the T2 its schedule names: the operator's contracts minted through the
// host's provide secret, beside server's honest ones.
func TestFs38ForwardingForgedOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-38's forwarding forged case", fault.Fs38ForwardingForgedCase)
	})
}
