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
// DelegationRevoke ends the live connection its key admitted while the identity key's carries on. The same rows at an
// app host, whose device role its own Forwarder feeds with the app's sidecar stopped, run §22.5's forwarding cases and
// Fwd.tla's twins as schedules (sidecar/sidecartest/fault/fs_bridge_forwarding_app_m8.go) on server's contracts.
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

// An app host fed by its own Forwarder with the app's sidecar stopped (DESIGN-DETAILS 4.6), on server's contracts: its
// twin admitting the subject's forwarding key; a grant at CompileDelay and a revocation at once with no compile; a
// DelegationRevoke racing a held compile; and a supersession racing a contract.
func TestFwdAppHostOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "an app host's forwarding twin", fault.FwdAppHostAdmits)
		bridgeM4RunSchedule(t, ctx, "an app host within §8.9's targets", fault.FwdAppWithinTargetsCase)
		bridgeM4RunSchedule(t, ctx, "a DelegationRevoke racing an app host's compile", fault.FwdAppRevokeRacesCompileCase)
		bridgeM4RunSchedule(t, ctx, "a supersession racing a contract at an app host", fault.FwdAppSupersedeRacesContractCase)
	})
}

// Fwd.tla's LowerStateAccepted and StateDropsOverlay as schedules on server's contracts: a restarted Forwarder over a
// lost store and a lagging api refused below the posture's pin, the revoked key refused throughout.
func TestFwdAppCompileBelowANewerStateOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "a compile below the state in force", fault.FwdAppLowerStateDroppedCase)
		bridgeM4RunSchedule(t, ctx, "a compile below an overlay revocation", fault.FwdAppStateKeepsOverlayCase)
	})
}

// Fwd.tla's OverlayLifts as schedules on server's contracts: an Unsuspend lifts nothing until its compile, and one the
// rule table refuses, which the in-process api sequences anyway, lifts nothing at all.
func TestFwdAppUnsuspendOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "an Unsuspend no compile judged", fault.FwdAppUnsuspendWaitsCase)
		bridgeM4RunSchedule(t, ctx, "a refused Unsuspend", fault.FwdAppRefusedUnsuspendCase)
	})
}

// FwdReach-NoCompile and Fwd.tla's RepeatsOnly as schedules on server's contracts, under the heap ceiling and over
// the memory cap.
func TestFwdAppNoCompileOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "an uncheckable revocation with compiles deferred", fault.FwdAppNoCompileCase)
		bridgeM4RunSchedule(t, ctx, "an uncheckable revocation over the memory cap", fault.FwdAppOverCapCase)
	})
}

// Fwd.tla's NoFloor as a schedule on server's contracts, beside its twin.
func TestFwdAppRollbackOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "a restart with no rollback", fault.FwdAppRestartKeepsTheState)
		bridgeM4RunSchedule(t, ctx, "a rolled-back posture store", fault.FwdAppRollbackCase)
	})
}
