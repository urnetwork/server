// Synthetic controls for the §22.5 reader over the deterministic world.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"

	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/stabi"
)

// The classes whose sources are not deployed stay explicitly unknown on a
// healthy deployment; nothing else alerts.
func requireOnlySubnetHeadStandingUnknowns(t *testing.T, alerts Alerts) {
	t.Helper()
	standing := map[string]string{
		subnetPromotionEconomicsClass: "pay-ledger-unpublished",
		subnetHeadSelectionClass:      "validator-decision-unpublished",
	}
	for _, alert := range alerts {
		reason, ok := standing[alert.Class]
		if !ok || alert.Frame != "unobserved" || alert.Severity != SeverityWarn || !alert.unobservable || !strings.Contains(alert.Observed, reason) {
			t.Errorf("unexpected alert %s %s %s %s: %s", alert.Severity, alert.Class, alert.Target, alert.Frame, alert.Observed)
		}
		delete(standing, alert.Class)
	}
	for class := range standing {
		t.Errorf("standing unknown %s is missing", class)
	}
}

func TestSubnetHeadHealthyDeploymentDecidesTheChainChecks(t *testing.T) {
	world := newSubnetWorld(t)
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	requireOnlySubnetHeadStandingUnknowns(t, alerts)
	world.requirePrivate(t, alerts)
}

// The census replays from the deployment block; until it reaches the
// checkpoint every census-dependent check is unknown, never healthy.
func TestSubnetHeadBindingCensusMustBeComplete(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.Limits.LogChunkBlocks = 1000
	world.settings.Limits.MaxLogRequests = 40
	signal := NewSubnetHeadSignal()
	settings := world.signalSettings()
	alerts := runSubnetWorld(t, signal, settings)
	for _, class := range []string{subnetHeadWeightBypassClass, subnetHeadNativePayoutClass, subnetPromotionChurnClass} {
		found := false
		for _, alert := range subnetAlerts(alerts, class, "") {
			found = found || strings.Contains(alert.Observed, "binding-census-incomplete")
		}
		if !found {
			t.Fatalf("incomplete census left %s decided: %+v", class, alerts)
		}
	}
	for run := 0; run < 3; run++ {
		alerts = runSubnetWorld(t, signal, settings)
	}
	requireOnlySubnetHeadStandingUnknowns(t, alerts)
}

func TestSubnetHeadWeightBypass(t *testing.T) {
	treasury := subnetWorldKey("treasury-recipient")
	setup := func(t *testing.T, extra subnetWeightPair, enrollTreasury bool) *subnetWorld {
		world := newSubnetWorld(t)
		if enrollTreasury {
			world.settings.TreasuryHotkeys = []string{subnetWorldHex(treasury)}
		}
		world.setStorage("SubtensorModule", "Keys", types.AccountID(treasury), subnetNetuidArg(world.deployment.Netuid), subnetNetuidArg(40))
		world.setWeights(1, []subnetWeightPair{{Uid: 11, Weight: 20000}, {Uid: 30, Weight: 20000}, extra}, world.finalized-100)
		return world
	}
	// A reviewed treasury recipient is an eligible recipient.
	world := setup(t, subnetWeightPair{Uid: 40, Weight: 10000}, true)
	requireOnlySubnetHeadStandingUnknowns(t, runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings()))
	// An unbound, non-treasury Uid is a bypass.
	world = setup(t, subnetWeightPair{Uid: 41, Weight: 10000}, true)
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetHeadWeightBypassClass, "subnet-head/validator-1", "", SeverityPage)
	if !strings.Contains(alert.Observed, "unbound-uid-weighted") {
		t.Fatal(alert.Observed)
	}
	if len(subnetAlerts(alerts, subnetHeadWeightBypassClass, "subnet-head/validator-2")) != 0 {
		t.Fatal("an independent validator's row was judged by another's")
	}
	// Without the reviewed treasury, an unexplained Uid stays unknown.
	world = setup(t, subnetWeightPair{Uid: 41, Weight: 10000}, false)
	alerts = runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetHeadWeightBypassClass, "subnet-head/weights", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "treasury-recipients-not-enrolled") {
		t.Fatal(alert.Observed)
	}
	// A validator weighting itself.
	world = setup(t, subnetWeightPair{Uid: 1, Weight: 10000}, true)
	alerts = runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetHeadWeightBypassClass, "subnet-head/validator-1", "", SeverityPage)
	if !strings.Contains(alert.Observed, "self-weight") {
		t.Fatal(alert.Observed)
	}
}

// A binding that expired before this epoch no longer makes its Uid eligible.
func TestSubnetHeadExpiredBindingIsUnbound(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.TreasuryHotkeys = []string{subnetWorldHex(subnetWorldKey("treasury-recipient"))}
	world.bindHead([16]byte{0x44}, subnetWorldKey("expired-hotkey"), 44, 50000, 1, 1)
	world.setWeights(2, []subnetWeightPair{{Uid: 12, Weight: 20000}, {Uid: 44, Weight: 5000}}, world.finalized-100)
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetHeadWeightBypassClass, "subnet-head/validator-2", "", SeverityPage)
	// Revocation ends a binding before its effective epoch.
	world = newSubnetWorld(t)
	world.settings.TreasuryHotkeys = []string{subnetWorldHex(subnetWorldKey("treasury-recipient"))}
	coordinatorAbi := subnetWorldAbi(t, stabi.STCoordinatorMetaData.ParseABI)
	world.emit(world.coordinator, coordinatorAbi, "FleetBindingRevoked", 70000, uint(len(world.logs)), map[string]any{
		"clientId": [16]byte(subnetWorldHash("fleet-2-client").Bytes()[:16]), "generation": uint64(1), "effectiveEpoch": uint64(3),
	})
	alerts = runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetHeadWeightBypassClass, "subnet-head/validator-1", "", SeverityPage)
	if !strings.Contains(alert.Observed, "unbound-uid-weighted") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetHeadLimitExceededPages(t *testing.T) {
	world := newSubnetWorld(t)
	pairs := []subnetWeightPair{{Uid: 11, Weight: 1000}}
	for i := 0; i < maximumSubnetHeadFleets+1; i++ {
		uid := uint16(100 + i)
		world.bindHead([16]byte{0x90, byte(i >> 8), byte(i)}, subnetWorldKey(fmt.Sprintf("head-%d", i)), uid, 60000+uint64(i), 3, 8)
		pairs = append(pairs, subnetWeightPair{Uid: uid, Weight: 100})
	}
	world.setWeights(1, pairs, world.finalized-100)
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetHeadSelectionClass, "subnet-head/selection", "", SeverityPage)
	if !strings.Contains(alert.Observed, "head-limit-exceeded:validator-1") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetHeadNativePayout(t *testing.T) {
	netuid := func(world *subnetWorld) []byte { return subnetNetuidArg(world.deployment.Netuid) }
	world := newSubnetWorld(t)
	world.setStorage("SubtensorModule", "Keys", types.AccountID(subnetWorldKey("newcomer")), netuid(world), subnetNetuidArg(30))
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetHeadNativePayoutClass, "subnet-head/payout", "", SeverityPage)
	if !strings.Contains(alert.Observed, "head-uid-reassigned") {
		t.Fatal(alert.Observed)
	}
	world = newSubnetWorld(t)
	hotkey := subnetWorldKey("fleet-2-hotkey")
	world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetMirroredColdkey(world.vault)), hotkey[:])
	alerts = runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetHeadNativePayoutClass, "subnet-head/payout", "", SeverityPage)
	if !strings.Contains(alert.Observed, "head-incentive-to-custody-key") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetHeadChurnAcrossRuns(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.Limits.MaxHeadChurnPerRun = 1
	world.bindHead([16]byte{0x31}, subnetWorldKey("fleet-3-hotkey"), 31, 61000, 3, 8)
	world.bindHead([16]byte{0x32}, subnetWorldKey("fleet-4-hotkey"), 32, 61000, 3, 8)
	signal := NewSubnetHeadSignal()
	settings := world.signalSettings()
	netuid := subnetNetuidArg(world.deployment.Netuid)
	for _, uid := range []uint16{31, 32} {
		world.setStorage("SubtensorModule", "Keys", types.AccountID(subnetWorldKey(fmt.Sprintf("fleet-%d-hotkey", uid-28))), netuid, subnetNetuidArg(uid))
	}
	requireOnlySubnetHeadStandingUnknowns(t, runSubnetWorld(t, signal, settings))
	for _, uid := range []uint16{31, 32} {
		world.setStorage("SubtensorModule", "Keys", types.AccountID(subnetWorldKey(fmt.Sprintf("replacement-%d", uid))), netuid, subnetNetuidArg(uid))
	}
	world.moveFinalized(world.finalized + 10)
	alerts := runSubnetWorld(t, signal, settings)
	alert := requireSubnetAlert(t, alerts, subnetPromotionChurnClass, "subnet-head/churn", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "changed_head_uids=2") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetHeadTierIsolation(t *testing.T) {
	headClient := [16]byte{1, 1}
	providers := []payoutartifact.ProviderInput{
		{ClientID: headClient, NetworkID: [16]byte{1, 100}, Coldkey: [32]byte{1, 1}, UsageBytes: 1000, Assignments: 10, Confirmations: 10, Eligible: true},
		{ClientID: [16]byte{1, 2}, NetworkID: [16]byte{1, 101}, Coldkey: [32]byte{1, 2}, UsageBytes: 2000, Assignments: 10, Confirmations: 10, Eligible: true},
	}
	// A tail provider bound at a boundary is paid by both tiers.
	world := newSubnetWorld(t)
	world.bindHead(headClient, subnetWorldKey("fleet-2-hotkey"), 30, 59000, 2, 8)
	world.commitArtifactWith(1, world.currentEpoch-1, providers)
	world.activeBindings[headClient] = stabi.STCoordinatorBindingRecord{Uid: 30, Generation: 1, Hotkey: subnetWorldKey("fleet-2-hotkey")}
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetTierDoublePayClass, "subnet-head/no-1", "", SeverityPage)
	if !strings.Contains(alert.Observed, "head-client-in-tail") {
		t.Fatal(alert.Observed)
	}
	world.requirePrivate(t, alerts)
	// The same client correctly head-excluded is healthy.
	excluded := append([]payoutartifact.ProviderInput(nil), providers...)
	excluded[0].Eligible, excluded[0].HeadExcluded, excluded[0].ExclusionReason, excluded[0].BindingGeneration = false, true, "head_fleet_active", 1
	world = newSubnetWorld(t)
	world.bindHead(headClient, subnetWorldKey("fleet-2-hotkey"), 30, 59000, 2, 8)
	world.commitArtifactWith(1, world.currentEpoch-1, excluded)
	world.activeBindings[headClient] = stabi.STCoordinatorBindingRecord{Uid: 30, Generation: 1, Hotkey: subnetWorldKey("fleet-2-hotkey")}
	requireOnlySubnetHeadStandingUnknowns(t, runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings()))
	// A head exclusion with no boundary binding pays neither tier; it warns
	// under its own frame so it never stands in for a double pay.
	world = newSubnetWorld(t)
	world.commitArtifactWith(1, world.currentEpoch-1, excluded)
	alerts = runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetTierDoublePayClass, "subnet-head/no-1", "unpaid", SeverityWarn)
	if !strings.Contains(alert.Observed, "head-excluded-unbound") {
		t.Fatal(alert.Observed)
	}
	// Pruned boundary state is unknown, never healthy.
	world = newSubnetWorld(t)
	world.bindHead(headClient, subnetWorldKey("fleet-2-hotkey"), 30, 59000, 2, 8)
	world.commitArtifactWith(1, world.currentEpoch-1, providers)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == "eth_call" && strings.Contains(string(params[1]), world.evmHash(world.epochStartBlock(world.currentEpoch-1)).Hex()) {
			return nil, errors.New("state pruned"), true
		}
		return nil, nil, false
	}
	alerts = runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetTierDoublePayClass, "subnet-head/no-1", "unobserved", SeverityWarn)
}

func TestSubnetHeadNativeSourceLossIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	world.deleteStorage("SubtensorModule", "Incentive", subnetNetuidArg(world.deployment.Netuid))
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == "state_getStorage" && len(params) == 2 && strings.Contains(string(params[0]), world.storageKeyHex("SubtensorModule", "LastUpdate", subnetNetuidArg(world.deployment.Netuid))) {
			return nil, errors.New("unavailable"), true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetHeadSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetHeadWeightBypassClass, "subnet-head/weights", "unobserved", SeverityWarn)
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("source loss produced a proved violation: %+v", alert)
		}
	}
}

func TestSubnetHeadRegistrationMetadata(t *testing.T) {
	signal := NewSubnetHeadSignal()
	if signal.Number() != "22.5" || signal.Key() != "subnet-head" || signal.ID() != "subnet/head" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
}

// §22.10 acceptance: a validator row weighting itself is a proved accounting
// counterexample; restoring the world's row is the healthy read. Head
// selection replay and realized pay remain standing gaps of a healthy run.
func init() {
	subnetFamilyAcceptanceCases["22.5"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			return NewSubnetHeadSignal(), newSubnetWorld(t).signalSettings()
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			world := newSubnetWorld(t)
			world.setWeights(1, []subnetWeightPair{{Uid: 11, Weight: 20000}, {Uid: 30, Weight: 20000}, {Uid: 1, Weight: 10000}}, world.finalized-100)
			return NewSubnetHeadSignal(), world.signalSettings(), func() {
				world.setWeights(1, []subnetWeightPair{{Uid: 11, Weight: 20000}, {Uid: 12, Weight: 20000}, {Uid: 30, Weight: 25535}}, world.finalized-100)
			}
		},
		violatedClass: subnetHeadWeightBypassClass,
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			return subnetWorldAcceptanceUnknown(t, NewSubnetHeadSignal(), "state_getStorage")
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			return subnetWorldAcceptanceCanceled(t, NewSubnetHeadSignal(), "state_getStorage")
		},
		fixedGaps: []subnetFamilyAcceptanceGap{
			{class: subnetPromotionEconomicsClass, frame: "unobserved"},
			{class: subnetHeadSelectionClass, frame: "unobserved"},
		},
	}
}
