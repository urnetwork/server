// Synthetic controls for the §22.1 reader. Every run goes through the public
// Signal boundary and the injected Do seam of a deterministic world.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/stabi"
)

func TestSubnetChainHealthyDeploymentDecidesEveryClass(t *testing.T) {
	world := newSubnetWorld(t)
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	requireNoSubnetAlerts(t, alerts)
	for _, endpoint := range []string{subnetWorldNative1, subnetWorldNative2, subnetWorldEvm1, subnetWorldEvm2} {
		if world.calls[endpoint+" eth_sendRawTransaction"] != 0 || world.calls[endpoint+" author_submitExtrinsic"] != 0 {
			t.Fatal("a write method was contacted")
		}
	}
	if world.calls[subnetWorldNative1+" state_getMetadata"] != 1 {
		t.Fatalf("metadata reads = %d, want one verification", world.calls[subnetWorldNative1+" state_getMetadata"])
	}
}

// The verified runtime is reused; the second run reads no metadata.
func TestSubnetChainVerifiesMetadataOncePerRuntime(t *testing.T) {
	world := newSubnetWorld(t)
	signal := NewSubnetChainSignal()
	settings := world.signalSettings()
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
	if reads := world.calls[subnetWorldNative1+" state_getMetadata"]; reads != 1 {
		t.Fatalf("metadata reads = %d, want 1 across two runs", reads)
	}
}

func TestSubnetChainExplicitDisableIsNotDeployed(t *testing.T) {
	world := newSubnetWorld(t)
	settings := world.signalSettings()
	settings.STConfigStatus = STConfigurationExplicitDisabled
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), settings))
	if len(world.requests) != 0 {
		t.Fatal("a not-deployed subnet was contacted")
	}
}

func TestSubnetChainEnrollmentGapsMakeNoContact(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*SignalSettings)
		reason string
	}{
		{"missing", func(settings *SignalSettings) { settings.SubnetCorrectness = nil }, "expectations-missing"},
		{"invalid", func(settings *SignalSettings) { settings.SubnetCorrectness.invalid = true }, "expectations-invalid"},
		{"expired", func(settings *SignalSettings) {
			settings.SubnetCorrectness.ReviewUntil = settings.SubnetCorrectness.ReviewFrom
		}, "expectations-invalid"},
		{"outside-window", func(settings *SignalSettings) {
			settings.SubnetCorrectness.ReviewUntil = settings.Now().Add(-1).UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
			settings.SubnetCorrectness.ReviewFrom = settings.Now().Add(-2).UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		}, "expectations-outside-review-window"},
		{"same-endpoints", func(settings *SignalSettings) {
			settings.SubnetCorrectness.NativeRpc[1] = settings.SubnetCorrectness.NativeRpc[0]
		}, "expectations-invalid"},
		{"endpoint-query", func(settings *SignalSettings) { settings.SubnetCorrectness.EvmRpc[1] += "?token=secret" }, "expectations-invalid"},
		{"partial-profile", func(settings *SignalSettings) { settings.SubnetCorrectness.NativeProfile.Tempo = 0 }, "expectations-invalid"},
		{"limit-out-of-range", func(settings *SignalSettings) { settings.SubnetCorrectness.Limits.EndpointConcurrency = 9 }, "expectations-invalid"},
		{"deployment-missing", func(settings *SignalSettings) { settings.SubnetCorrectness.Deployment = SubnetDeploymentIdentity{} }, "deployment-identity-missing"},
		{"deployment-blocked", func(settings *SignalSettings) {
			settings.SubnetCorrectness.Deployment = SubnetDeploymentIdentity{Status: subnetDeploymentBlocked}
		}, "deployment-identity-blocked"},
		{"deployment-ambiguous", func(settings *SignalSettings) {
			settings.STDeploymentKey = "945:0x0000000000000000000000000000000000000001"
		}, "deployment-identity-ambiguous"},
		{"generation-unavailable", func(settings *SignalSettings) { settings.SettingsGenerationCheck = nil }, "settings-generation-unavailable"},
		{"generation-stale", func(settings *SignalSettings) {
			settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return false, nil }
		}, "settings-generation-stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			settings := world.signalSettings()
			test.mutate(&settings)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), settings)
			if len(alerts) != 1 {
				t.Fatalf("alerts = %d, want one coverage gap", len(alerts))
			}
			alert := requireSubnetAlert(t, alerts, "subnet-monitor-coverage", "subnet-chain/configuration", "", SeverityWarn)
			if !strings.Contains(alert.Observed, "reason="+test.reason) || !strings.Contains(alert.Observed, "subnet-deployment-governance") || !alert.unobservable {
				t.Fatalf("coverage gap lost its reason or unknown classes: %s", alert.Observed)
			}
			if len(world.requests) != 0 {
				t.Fatal("a configuration gap contacted a source")
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// A generation that turns stale during the run replaces every finding.
func TestSubnetChainStaleGenerationAtCloseWithholdsFindings(t *testing.T) {
	world := newSubnetWorld(t)
	settings := world.signalSettings()
	checks := 0
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		checks++
		return checks == 1, nil
	}
	world.contracts[world.coordinator].slots[subnetErc1967ImplementationSlot] = common.BytesToHash(subnetWorldAddress("unreviewed").Bytes())
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), settings)
	if len(alerts) != 1 || alerts[0].Class != "subnet-monitor-coverage" || !strings.Contains(alerts[0].Observed, "settings-generation-stale") {
		t.Fatalf("stale generation at close kept findings: %+v", alerts)
	}
}

func TestSubnetChainEndpointLossIsUnknownNeverHealthy(t *testing.T) {
	world := newSubnetWorld(t)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if endpoint == subnetWorldNative2 {
			return nil, errors.New("down"), true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	divergence := requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/native", "unobserved", SeverityWarn)
	if divergence.Sustain != subnetTransientSustain || !divergence.unobservable || !strings.Contains(divergence.Observed, "native-rpc-2-rpc-error") {
		t.Fatalf("endpoint loss was not a sustained unknown: %+v", divergence)
	}
	for _, class := range []string{subnetRuntimeIdentityClass, subnetDeploymentGovernanceClass, subnetPolicyScheduleClass} {
		found := false
		for _, alert := range alerts {
			found = found || (alert.Class == class && alert.Frame == "unobserved" && alert.Severity == SeverityWarn)
			if alert.Class == class && alert.Severity == SeverityPage {
				t.Fatalf("endpoint loss produced a proved violation for %s", class)
			}
		}
		if !found {
			t.Fatalf("class %s was not reported unknown", class)
		}
	}
	world.requirePrivate(t, alerts)
}

func TestSubnetChainContradictoryFinalizedHashPages(t *testing.T) {
	world := newSubnetWorld(t)
	forked := subnetWorldBlockHash("fork", world.finalized)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if endpoint == subnetWorldNative2 && (method == "chain_getBlockHash" || method == "chain_getFinalizedHead") {
			var number uint64
			if method == "chain_getBlockHash" {
				_ = json.Unmarshal(params[0], &number)
			}
			if method == "chain_getFinalizedHead" || number == world.finalized {
				return subnetWorldHex(forked), nil, true
			}
		}
		if endpoint == subnetWorldNative2 && method == "chain_getHeader" && subnetWorldString(t, params[0]) == subnetWorldHex(forked) {
			return map[string]any{"parentHash": subnetHashHex(world.nativeHash(world.finalized - 1)), "number": "0x1d4c0"}, nil, true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/native", "", SeverityPage)
	if alert.Sustain != subnetProvedSustain || alert.unobservable {
		t.Fatalf("contradiction is not a proved page: %+v", alert)
	}
	if len(subnetAlerts(alerts, subnetDeploymentGovernanceClass, "")) == 0 {
		t.Fatal("economic checks were not withheld at a contradictory checkpoint")
	}
	for _, alert := range subnetAlerts(alerts, subnetDeploymentGovernanceClass, "") {
		if alert.Severity == SeverityPage {
			t.Fatal("governance was decided at a contradictory checkpoint")
		}
	}
	world.requirePrivate(t, alerts)
}

// An endpoint on another network is a configuration fault, not a fork: its
// foreign genesis is reported before any finalized height is compared.
func TestSubnetChainForeignNetworkEndpointIsNotAFork(t *testing.T) {
	world := newSubnetWorld(t)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if endpoint != subnetWorldNative2 {
			return nil, nil, false
		}
		switch method {
		case "chain_getBlockHash":
			var number uint64
			_ = json.Unmarshal(params[0], &number)
			return subnetWorldHex(subnetWorldBlockHash("other-network", number)), nil, true
		case "chain_getFinalizedHead":
			return subnetWorldHex(subnetWorldBlockHash("other-network", world.finalized)), nil, true
		case "chain_getHeader":
			return map[string]any{"parentHash": subnetWorldHex(subnetWorldBlockHash("other-network", world.finalized-1)), "number": "0x1d4c0"}, nil, true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetRuntimeIdentityClass, "subnet-chain/runtime", "genesis", SeverityWarn)
	if !strings.Contains(alert.Observed, "native-rpc-2-genesis-mismatch") {
		t.Fatal(alert.Observed)
	}
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("a foreign network endpoint paged as a fork: %s %s", alert.Class, alert.Observed)
		}
	}
}

func TestSubnetChainEvmNativeMappingMismatchPages(t *testing.T) {
	world := newSubnetWorld(t)
	world.setStorage("Ethereum", "BlockHash", subnetWorldBlockHash("other-evm", world.finalized), subnetWorldU256(world.finalized))
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/evm", "native-mapping", SeverityPage)
}

func TestSubnetChainEvmEndpointsDisagreePages(t *testing.T) {
	world := newSubnetWorld(t)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if endpoint == subnetWorldEvm2 && method == "eth_getBlockByNumber" && subnetWorldString(t, params[0]) != "finalized" {
			return map[string]any{"number": "0x1d4c0", "hash": common.Hash(subnetWorldBlockHash("evm-fork", world.finalized)).Hex()}, nil, true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/evm", "", SeverityPage)
}

// The agreed checkpoint is the lower head; the lag warns but every other
// check still decides at that height.
func TestSubnetChainLaggingEndpointWarnsAndStillDecides(t *testing.T) {
	world := newSubnetWorld(t)
	behind := world.finalized - 100
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if endpoint == subnetWorldNative2 && method == "chain_getFinalizedHead" {
			return subnetHashHex(world.nativeHash(behind)), nil, true
		}
		return nil, nil, false
	}
	for number := behind - 2; number <= behind+2; number++ {
		world.setStorage("Ethereum", "BlockHash", world.evmHash(number), subnetWorldU256(number))
	}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	lag := requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/native", "lag", SeverityWarn)
	if lag.Sustain != subnetTransientSustain {
		t.Fatalf("lag sustain = %d", lag.Sustain)
	}
	if len(alerts) != 1 {
		t.Fatalf("lag hid or added checks: %+v", alerts)
	}
}

func TestSubnetChainRuntimeReviewStates(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
		frame  string
	}{
		{"unreviewed", func(world *subnetWorld) { world.settings.ReviewedRuntimes = nil }, "runtime-unreviewed", ""},
		{"known-version-other-code", func(world *subnetWorld) {
			world.settings.ReviewedRuntimes[0].CodeHash = subnetWorldHex(subnetWorldKey("other-code"))
		}, "runtime-conflict", ""},
		{"metadata-conflict", func(world *subnetWorld) {
			world.settings.ReviewedRuntimes[0].MetadataHash = subnetWorldHex(subnetWorldKey("other-metadata"))
		}, "runtime-metadata-conflict", "metadata"},
		{"genesis", func(world *subnetWorld) { world.genesis = [32]byte(subnetWorldBlockHash("other-genesis", 0)) }, "genesis-mismatch", "genesis"},
		{"evm-chain", func(world *subnetWorld) { world.deployment.ChainId = 945 }, "chain-id-mismatch", "evm-chain-id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			settings := world.signalSettings()
			if test.name == "evm-chain" {
				settings.SubnetCorrectness.Deployment.ChainId = mainSTProtocolChainID
				settings.STDeploymentKey = settings.SubnetCorrectness.Deployment.deploymentKey()
			}
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), settings)
			alert := requireSubnetAlert(t, alerts, subnetRuntimeIdentityClass, "subnet-chain/runtime", test.frame, SeverityWarn)
			if !strings.Contains(alert.Observed, test.reason) || alert.Sustain != subnetProvedSustain {
				t.Fatalf("runtime identity finding = %+v", alert)
			}
			if !strings.Contains(alert.Action, "not blindly add") {
				t.Fatal("runtime identity action lost the review routing")
			}
			for _, governance := range subnetAlerts(alerts, subnetDeploymentGovernanceClass, "") {
				if governance.Severity == SeverityPage {
					t.Fatal("governance was decided under an unapproved runtime")
				}
			}
			world.requirePrivate(t, alerts)
		})
	}
}

func TestSubnetChainCursorRejectsReplacedFinalizedBlock(t *testing.T) {
	world := newSubnetWorld(t)
	signal := NewSubnetChainSignal()
	settings := world.signalSettings()
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
	accepted := world.finalized
	world.finalized += 5
	world.evmFinalized += 5
	world.nativeHashes[accepted] = subnetWorldBlockHash("replaced", accepted)
	for number := world.finalized - 2; number <= world.finalized+2; number++ {
		world.setStorage("Ethereum", "BlockHash", world.evmHash(number), subnetWorldU256(number))
	}
	alerts := runSubnetWorld(t, signal, settings)
	alert := requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/cursor", "", SeverityPage)
	if !strings.Contains(alert.Observed, "accepted-finalized-block-replaced") {
		t.Fatalf("cursor contradiction = %s", alert.Observed)
	}
}

// A lower agreed height with the accepted block still canonical is a lag.
func TestSubnetChainCursorRegressionWarnsWithoutMovingBack(t *testing.T) {
	world := newSubnetWorld(t)
	signal := NewSubnetChainSignal()
	settings := world.signalSettings()
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
	world.finalized -= 5
	world.evmFinalized -= 5
	alerts := runSubnetWorld(t, signal, settings)
	requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/cursor", "regressed", SeverityWarn)
}

func TestSubnetChainGovernanceDriftPages(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"implementation", func(world *subnetWorld) {
			world.contracts[world.coordinator].slots[subnetErc1967ImplementationSlot] = common.BytesToHash(subnetWorldAddress("unreviewed").Bytes())
		}, "implementation-slot"},
		{"beacon", func(world *subnetWorld) {
			world.contracts[world.coordinator].slots[subnetErc1967BeaconSlot] = common.BytesToHash([]byte{1})
		}, "proxy-kind"},
		{"vault-code", func(world *subnetWorld) { world.contracts[world.vault].code = []byte("replaced vault") }, "settlement-vault-code"},
		{"sink-code-absent", func(world *subnetWorld) { world.contracts[world.sink].code = nil }, "reserve-sink-code"},
		{"vault-link", func(world *subnetWorld) {
			world.contracts[world.vault].views["coordinator"] = subnetWorldConstant(subnetWorldAddress("other"))
		}, "vault-coordinator-link"},
		{"recorder-link", func(world *subnetWorld) {
			world.contracts[world.sink].views["recorder"] = subnetWorldConstant(subnetWorldAddress("other"))
		}, "sink-recorder-link"},
		{"netuid", func(world *subnetWorld) { world.contracts[world.sink].views["netuid"] = subnetWorldConstant(uint16(7)) }, "contract-netuid"},
		{"coldkey", func(world *subnetWorld) {
			world.contracts[world.vault].views["selfColdkey"] = subnetWorldConstant(subnetMirroredColdkey(world.sink))
		}, "contract-coldkeys"},
		{"owner", func(world *subnetWorld) {
			world.contracts[world.coordinator].views["owner"] = subnetWorldConstant(subnetWorldAddress("attacker"))
		}, "owner"},
		{"guardian", func(world *subnetWorld) {
			world.contracts[world.coordinator].views["activeGuardian"] = subnetWorldConstant(subnetWorldAddress("attacker"))
		}, "active-guardian"},
		{"role-overlap", func(world *subnetWorld) {
			world.contracts[world.coordinator].views["activeCommitmentOracle"] = subnetWorldConstant(subnetWorldAddress("guardian"))
			world.settings.Governance.CommitmentOracle = world.settings.Governance.Guardian
		}, "role-overlap"},
		{"multisig-threshold", func(world *subnetWorld) {
			world.contracts[subnetWorldAddress("owner-safe")].views["getThreshold"] = subnetWorldConstant(big.NewInt(1))
		}, "owner-multisig-threshold"},
		{"mainnet-dedicated-owner", func(world *subnetWorld) {
			world.settings.Governance.OwnerThreshold, world.settings.Governance.OwnerSigners = 0, 0
		}, "mainnet-owner-not-multisig"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/deployment", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) || alert.unobservable {
				t.Fatalf("governance drift %s = %s", test.reason, alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// Off mainnet a reviewed dedicated owner account is the healthy control.
func TestSubnetChainTestnetDedicatedOwnerIsHealthy(t *testing.T) {
	world := newSubnetWorld(t)
	world.deployment.ChainId = 945
	world.settings.Governance.OwnerThreshold, world.settings.Governance.OwnerSigners = 0, 0
	delete(world.contracts, subnetWorldAddress("owner-safe"))
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()))
}

func TestSubnetChainPendingRoleRotationWarns(t *testing.T) {
	world := newSubnetWorld(t)
	world.contracts[world.coordinator].views["pendingGuardian"] = subnetWorldConstant(subnetWorldAddress("next-guardian"))
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/deployment", "pending", SeverityWarn)
	if !strings.Contains(alert.Observed, "pending-guardian-unreviewed") {
		t.Fatal(alert.Observed)
	}
	if len(alerts) != 1 {
		t.Fatal("a scheduled rotation was escalated")
	}
}

func TestSubnetChainGovernanceEventsInFrontier(t *testing.T) {
	world := newSubnetWorld(t)
	coordinatorAbi := subnetWorldAbi(t, stabi.STCoordinatorMetaData.ParseABI)
	world.emit(world.coordinator, coordinatorAbi, "Upgraded", world.finalized-50, 0, map[string]any{"implementation": subnetWorldAddress("unreviewed")})
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/deployment", "", SeverityPage)
	if !strings.Contains(alert.Observed, "upgrade-event") {
		t.Fatal(alert.Observed)
	}
	// A reviewed upgrade in the same window is the healthy control.
	world = newSubnetWorld(t)
	world.emit(world.coordinator, coordinatorAbi, "Upgraded", world.finalized-50, 0, map[string]any{"implementation": subnetWorldAddress("coordinator-implementation")})
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()))
}

// The frontier reads only its bounded chunks, then catches up on later runs.
func TestSubnetChainEventFrontierIsBoundedAndAdvances(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.Limits.LogChunkBlocks = 100
	world.settings.Limits.MaxLogRequests = 2
	world.settings.Limits.InitialLogWindowBlocks = 1000
	signal := NewSubnetChainSignal()
	settings := world.signalSettings()
	alerts := runSubnetWorld(t, signal, settings)
	requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/governance-events", "unobserved", SeverityWarn)
	for run := 0; run < 4; run++ {
		alerts = runSubnetWorld(t, signal, settings)
	}
	requireNoSubnetAlerts(t, alerts)
	if reads := world.calls[subnetWorldEvm1+" eth_getLogs"]; reads != 10 {
		t.Fatalf("log reads = %d, want exactly the 1000-block window in 100-block chunks", reads)
	}
}

func TestSubnetChainPolicyScheduleViolations(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"current-unreviewed", func(world *subnetWorld) { world.deployment.PolicyHash = subnetWorldHex(subnetWorldKey("other-policy")) }, "current-policy-unreviewed"},
		{"discontinuous", func(world *subnetWorld) { world.policyVersions[1].EffectiveBlock += 1 }, "policy-boundary-discontinuous"},
		{"invalid-version", func(world *subnetWorld) {
			world.policyVersions[1].CloseGraceBlocks = world.policyVersions[1].RootCommitWindowBlocks + 1
		}, "invalid-policy-version"},
		{"testnet-duration", func(world *subnetWorld) {
			world.policyVersions = append(world.policyVersions, world.policyVersions[1])
			last := &world.policyVersions[2]
			last.EffectiveEpoch = 2
			last.EffectiveBlock = world.policyVersions[1].EffectiveBlock + world.policyVersions[1].EpochBlocks
			last.EpochBlocks, last.RootCommitWindowBlocks, last.FinalizeOffsetBlocks, last.CloseGraceBlocks = 360, 60, 180, 6
		}, "testnet-epoch-on-mainnet"},
		{"document-windows", func(world *subnetWorld) { world.policyVersions[1].FinalizeOffsetBlocks -= 1 }, "policy-windows"},
		{"document-caps", func(world *subnetWorld) { world.policyVersions[1].EpochDepositCapRao = big.NewInt(1) }, "policy-deposit-caps"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			settings := world.signalSettings()
			if test.name == "current-unreviewed" {
				settings.SubnetCorrectness.PolicyDocument = ""
			}
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), settings)
			alert := requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/policy", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatalf("policy finding = %s", alert.Observed)
			}
			if !strings.Contains(alert.Action, "Never inherit a testnet duration") {
				t.Fatal("policy action lost the testnet-duration rule")
			}
		})
	}
}

// The reviewed zero-price launch policy is healthy, and an unreviewed future
// version warns without paging.
func TestSubnetChainPolicyScheduledUnreviewedWarns(t *testing.T) {
	world := newSubnetWorld(t)
	next := world.policyVersions[1]
	next.PolicyHash = subnetWorldKey("future-policy")
	next.EffectiveEpoch = 5
	next.EffectiveBlock = world.policyVersions[1].EffectiveBlock + 4*world.policyVersions[1].EpochBlocks
	world.policyVersions = append(world.policyVersions, next)
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/policy", "scheduled", SeverityWarn)
	if !strings.Contains(alert.Observed, "scheduled-policy-unreviewed") || len(alerts) != 1 {
		t.Fatalf("scheduled policy = %+v", alerts)
	}
	world.settings.ScheduledPolicyHashes = []string{subnetWorldHex(subnetWorldKey("future-policy"))}
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()))
}

func TestSubnetChainPolicyDocumentAbsentIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.PolicyDocument = ""
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/policy", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "policy-document-not-enrolled") || !alert.unobservable || len(alerts) != 1 {
		t.Fatalf("absent document = %+v", alerts)
	}
}

func TestSubnetChainNativeProfile(t *testing.T) {
	netuid := func(world *subnetWorld) []byte { return subnetNetuidArg(world.deployment.Netuid) }
	world := newSubnetWorld(t)
	world.setStorage("SubtensorModule", "CommitRevealWeightsEnabled", false, netuid(world))
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/native-profile", "", SeverityPage)
	if !strings.Contains(alert.Observed, "commit-reveal") {
		t.Fatal(alert.Observed)
	}
	world = newSubnetWorld(t)
	world.setStorage("SubtensorModule", "ImmunityPeriod", uint16(4000), netuid(world))
	alerts = runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/native-profile", "drift", SeverityWarn)
	if !strings.Contains(alert.Observed, "immunity-period") || len(alerts) != 1 {
		t.Fatalf("profile drift = %+v", alerts)
	}
	// An absent value decodes the runtime's metadata default, never zero: the
	// default tempo equals the reviewed one, the default immunity does not.
	world = newSubnetWorld(t)
	world.deleteStorage("SubtensorModule", "Tempo", netuid(world))
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()))
	world = newSubnetWorld(t)
	world.deleteStorage("SubtensorModule", "ImmunityPeriod", netuid(world))
	alerts = runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/native-profile", "drift", SeverityWarn)
	if !strings.Contains(alert.Observed, "immunity-period") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetChainMalformedRpcStaysUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		reply  func(*subnetWorld, string) string
		reason string
	}{
		{"duplicate-result", func(world *subnetWorld, id string) string {
			return `{"jsonrpc":"2.0","id":` + id + `,"result":"0x1","result":"0x2"}`
		}, "envelope-invalid"},
		{"trailing", func(world *subnetWorld, id string) string {
			return `{"jsonrpc":"2.0","id":` + id + `,"result":"0x1"} {}`
		}, "envelope-invalid"},
		{"wrong-id", func(world *subnetWorld, id string) string {
			return `{"jsonrpc":"2.0","id":999999,"result":"0x1"}`
		}, "envelope-id-invalid"},
		{"oversized", func(world *subnetWorld, id string) string {
			return `{"jsonrpc":"2.0","id":` + id + `,"result":"` + strings.Repeat("a", 70<<10) + `"}`
		}, "response-oversized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			world.settings.Limits.MaxResponseBytes = 64 << 10
			settings := world.signalSettings()
			source := &subnetWorldRawSource{world: world, replace: func(endpoint string, body []byte) (string, bool) {
				if endpoint != subnetWorldNative2 || !strings.Contains(string(body), "chain_getFinalizedHead") {
					return "", false
				}
				var request subnetWorldRequest
				_ = json.Unmarshal(body, &request)
				return test.reply(world, big.NewInt(int64(request.Id)).String()), true
			}}
			settings.Source = source
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), settings)
			alert := requireSubnetAlert(t, alerts, subnetFinalityDivergenceClass, "subnet-chain/native", "unobserved", SeverityWarn)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatalf("malformed reply reason = %s", alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// A raw reply substitute for envelope controls; everything else is the world.
type subnetWorldRawSource struct {
	world   *subnetWorld
	replace func(endpoint string, body []byte) (string, bool)
}

func (self *subnetWorldRawSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	return self.world.PostgreSQL(ctx, query)
}
func (self *subnetWorldRawSource) Redis(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
	return self.world.Redis(ctx, host, port, args...)
}
func (self *subnetWorldRawSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.world.Host(ctx, host, command)
}

func (self *subnetWorldRawSource) Do(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if reply, ok := self.replace(request.URL.String(), body); ok {
			return subnetWorldJsonResponse(request, []byte(reply)), nil
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	return self.world.Do(request)
}

func TestSubnetChainCanceledParentWithholdsFindings(t *testing.T) {
	world := newSubnetWorld(t)
	world.contracts[world.coordinator].views["owner"] = subnetWorldConstant(subnetWorldAddress("attacker"))
	ctx, cancel := context.WithCancel(context.Background())
	settings := world.signalSettings()
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == "eth_getStorageAt" {
			cancel()
		}
		return nil, nil, false
	}
	alerts, err := NewSubnetChainSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run returned alerts=%d err=%v", len(alerts), err)
	}
}

func TestSubnetChainRegistrationMetadata(t *testing.T) {
	signal := NewSubnetChainSignal()
	if signal.Number() != "22.1" || signal.Key() != "subnet-chain" || signal.ID() != "subnet/chain" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
	found := false
	for _, registered := range NewSignals() {
		found = found || registered.Key() == "subnet-chain"
	}
	if !found {
		t.Fatal("subnet-chain is not registered")
	}
}
