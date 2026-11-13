// Synthetic controls for the §22.2 reader over the deterministic world.
package monitor

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/v2026/crv4"
	"github.com/urfoundation/sn/v2026/stabi"
)

func TestSubnetIdentityHealthyDeploymentDecidesEveryClass(t *testing.T) {
	world := newSubnetWorld(t)
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	requireNoSubnetAlerts(t, alerts)
	if world.calls[subnetWorldEvm1+" eth_getTransactionByHash"] != 1 {
		t.Fatal("the accepted binding was not re-verified from its calldata")
	}
}

func TestSubnetIdentityCheckpointGateIsOneCoverageGap(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.ReviewedRuntimes = nil
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	if len(alerts) != 1 {
		t.Fatalf("checkpoint gate produced %d alerts", len(alerts))
	}
	alert := requireSubnetAlert(t, alerts, "subnet-monitor-coverage", "subnet-identity/checkpoint", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "runtime-unreviewed") || !strings.Contains(alert.Observed, subnetPoolCustodyIdentityClass) {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetIdentityOperatorCensus(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.Operators = append(world.settings.Operators, SubnetReviewedOperator{NoId: 3, ArtifactSigner: world.settings.Operators[0].ArtifactSigner})
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPoolCustodyIdentityClass, "subnet-identity/census", "census", SeverityWarn)
	if !strings.Contains(alert.Observed, "reviewed-operator-unregistered:3") || len(alerts) != 1 {
		t.Fatalf("census = %+v", alerts)
	}
	world = newSubnetWorld(t)
	world.settings.Operators = world.settings.Operators[:1]
	alerts = runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetPoolCustodyIdentityClass, "subnet-identity/census", "census", SeverityWarn)
	if !strings.Contains(alert.Observed, "unreviewed-operator:2") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetIdentityPoolCustodyViolations(t *testing.T) {
	netuid := func(world *subnetWorld) []byte { return subnetNetuidArg(world.deployment.Netuid) }
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"uid-reassigned", func(world *subnetWorld) {
			world.setStorage("SubtensorModule", "Keys", types.AccountID(subnetWorldKey("newcomer")), netuid(world), subnetNetuidArg(11))
		}, "uid-reassigned"},
		{"foreign-owner", func(world *subnetWorld) {
			pool := subnetWorldKey("operator-1-pool")
			world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("foreign")), pool[:])
		}, "pool-foreign-owner"},
		{"deregistered", func(world *subnetWorld) {
			pool := subnetWorldKey("operator-1-pool")
			world.deleteStorage("SubtensorModule", "Uids", netuid(world), pool[:])
		}, "pool-uid-mismatch"},
		{"vault-differs", func(world *subnetWorld) {
			world.contracts[world.vault].views["pools"] = func(inputs []any) ([]any, error) {
				noId := inputs[0].(*big.Int).Uint64()
				return []any{subnetWorldKey(fmt.Sprintf("operator-%d-pool-other", noId)), uint16(10 + noId), true}, nil
			}
		}, "vault-pool-hotkey-differs"},
		{"deposit-is-escrow", func(world *subnetWorld) {
			original := world.contracts[world.coordinator].views["operatorAt"]
			world.contracts[world.coordinator].views["operatorAt"] = func(inputs []any) ([]any, error) {
				values, err := original(inputs)
				version := values[0].(stabi.STCoordinatorOperatorVersion)
				version.DepositHotkey = subnetWorldKey("escrow")
				return []any{version}, err
			}
		}, "role-overlap"},
		{"shared-deposit-hotkey", func(world *subnetWorld) {
			original := world.contracts[world.coordinator].views["operatorAt"]
			world.contracts[world.coordinator].views["operatorAt"] = func(inputs []any) ([]any, error) {
				values, err := original(inputs)
				version := values[0].(stabi.STCoordinatorOperatorVersion)
				version.DepositHotkey = subnetWorldKey("shared-deposit")
				return []any{version}, err
			}
		}, "duplicate-deposit-hotkey"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
			found := false
			for _, alert := range subnetAlerts(alerts, subnetPoolCustodyIdentityClass, "") {
				if alert.Severity == SeverityPage && strings.Contains(alert.Observed, test.reason) && alert.Sustain == subnetProvedSustain && !alert.unobservable {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing custody page %s in %+v", test.reason, alerts)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// An inactive vault pool under an active operator warns; ownership holds.
func TestSubnetIdentityInactivePoolWarnsWithoutPaging(t *testing.T) {
	world := newSubnetWorld(t)
	world.contracts[world.vault].views["pools"] = func(inputs []any) ([]any, error) {
		noId := inputs[0].(*big.Int).Uint64()
		return []any{subnetWorldKey(fmt.Sprintf("operator-%d-pool", noId)), uint16(10 + noId), noId != 2}, nil
	}
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetPoolCustodyIdentityClass, "subnet-identity/no-2", "pool-inactive", SeverityWarn)
	if len(alerts) != 1 {
		t.Fatalf("an inactive pool produced more than its warning: %+v", alerts)
	}
}

// One operator's unreadable views stay unknown for that operator only.
func TestSubnetIdentityOperatorViewFailureIsLocal(t *testing.T) {
	world := newSubnetWorld(t)
	original := world.contracts[world.coordinator].views["operatorAt"]
	world.contracts[world.coordinator].views["operatorAt"] = func(inputs []any) ([]any, error) {
		if inputs[0].(*big.Int).Uint64() == 2 {
			return nil, errors.New("revert")
		}
		return original(inputs)
	}
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetPoolCustodyIdentityClass, "subnet-identity/no-2", "unobserved", SeverityWarn)
	if len(subnetAlerts(alerts, subnetPoolCustodyIdentityClass, "subnet-identity/no-1")) != 0 || len(alerts) != 1 {
		t.Fatalf("a sibling operator was affected: %+v", alerts)
	}
}

// An inactive operator's pool is not an active custody obligation.
func TestSubnetIdentityInactiveOperatorIsNotChecked(t *testing.T) {
	world := newSubnetWorld(t)
	pool := subnetWorldKey("operator-2-pool")
	world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("foreign")), pool[:])
	original := world.contracts[world.coordinator].views["operatorAt"]
	world.contracts[world.coordinator].views["operatorAt"] = func(inputs []any) ([]any, error) {
		values, err := original(inputs)
		version := values[0].(stabi.STCoordinatorOperatorVersion)
		version.Active = inputs[0].(*big.Int).Uint64() != 2
		return []any{version}, err
	}
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()))
}

func TestSubnetIdentityEscrowCustody(t *testing.T) {
	world := newSubnetWorld(t)
	world.contracts[world.vault].views["escrowRegistered"] = subnetWorldConstant(false)
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPoolCustodyIdentityClass, "subnet-identity/escrow", "", SeverityPage)
	if !strings.Contains(alert.Observed, "escrow-unregistered") {
		t.Fatal(alert.Observed)
	}
	world = newSubnetWorld(t)
	escrow := subnetWorldKey("escrow")
	world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("foreign")), escrow[:])
	alerts = runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetPoolCustodyIdentityClass, "subnet-identity/escrow", "", SeverityPage)
	if !strings.Contains(alert.Observed, "escrow-foreign-owner") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetIdentityValidatorEligibility(t *testing.T) {
	netuid := func(world *subnetWorld) []byte { return subnetNetuidArg(world.deployment.Netuid) }
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"permit-lost", func(world *subnetWorld) {
			permits := make([]types.Bool, 32)
			permits[1] = true
			world.setStorage("SubtensorModule", "ValidatorPermit", permits, netuid(world))
		}, "permit-absent"},
		{"unregistered", func(world *subnetWorld) {
			hotkey := subnetWorldKey("validator-2-hotkey")
			world.deleteStorage("SubtensorModule", "Uids", netuid(world), hotkey[:])
		}, "validator-unregistered"},
		{"stake", func(world *subnetWorld) {
			hotkey := subnetWorldKey("validator-2-hotkey")
			world.setStorage("SubtensorModule", "TotalHotkeyAlpha", types.NewU64(999), hotkey[:], netuid(world))
		}, "stake-below-threshold"},
		{"coldkey", func(world *subnetWorld) {
			hotkey := subnetWorldKey("validator-2-hotkey")
			world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("buyer")), hotkey[:])
		}, "coldkey-differs"},
		{"short-permit-vector", func(world *subnetWorld) {
			world.setStorage("SubtensorModule", "ValidatorPermit", []types.Bool{true, true}, netuid(world))
		}, "permit-absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetValidatorEligibilityClass, "subnet-identity/validator-2", "", SeverityWarn)
			if !strings.Contains(alert.Observed, test.reason) || len(subnetAlerts(alerts, subnetValidatorEligibilityClass, "subnet-identity/validator-1")) != 0 {
				t.Fatalf("eligibility = %+v", alerts)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

func TestSubnetIdentityCommitmentMirrors(t *testing.T) {
	hotkey, _ := subnetWorldFleetKeys(t)
	fleet := hotkey.PublicKey()
	mirror := func(world *subnetWorld, hash [32]byte, block uint64, blockHash [32]byte) {
		world.contracts[world.coordinator].views["mirroredCommitments"] = func(inputs []any) ([]any, error) {
			if inputs[0].([32]byte) != fleet {
				return []any{[32]byte{}, [32]byte{}, uint64(0)}, nil
			}
			return []any{hash, blockHash, block}, nil
		}
	}
	commit := func(world *subnetWorld, block uint64) {
		world.setRawStorage(crv4.CommitmentsPalletName, "CommitmentOf", subnetWorldCommitmentRegistration(t, subnetWorldKey("fleet-1-manifest"), block), subnetNetuidArg(world.deployment.Netuid), fleet[:])
	}
	for _, test := range []struct {
		name     string
		mutate   func(*subnetWorld)
		frame    string
		severity Severity
		reason   string
	}{
		{"conflict", func(world *subnetWorld) {
			block := world.finalized - 100
			mirror(world, subnetWorldKey("other-manifest"), block, [32]byte(world.nativeHash(block)))
		}, "", SeverityPage, "mirror-conflicts"},
		{"newer", func(world *subnetWorld) {
			block := world.finalized - 10
			mirror(world, subnetWorldKey("other-manifest"), block, [32]byte(world.nativeHash(block)))
		}, "", SeverityPage, "mirror-newer-than-native"},
		{"non-canonical", func(world *subnetWorld) {
			block := world.finalized - 100
			mirror(world, subnetWorldKey("fleet-1-manifest"), block, subnetWorldBlockHash("fork", block))
		}, "", SeverityPage, "mirror-block-not-canonical"},
		{"expired", func(world *subnetWorld) {
			commit(world, world.finalized-world.policyVersions[1].CommitmentMaxAgeBlocks-1)
			mirror(world, [32]byte{}, 0, [32]byte{})
		}, "", SeverityPage, "mirror-deadline-missed"},
		{"stale", func(world *subnetWorld) {
			commit(world, world.finalized-2000)
			mirror(world, [32]byte{}, 0, [32]byte{})
		}, "lag", SeverityWarn, "mirror-stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetCommitmentMirrorLagClass, "subnet-identity/commitments", test.frame, test.severity)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatal(alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
	// A pending commitment inside the lag is not yet late.
	world := newSubnetWorld(t)
	commit(world, world.finalized-10)
	mirror(world, [32]byte{}, 0, [32]byte{})
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()))
}

func TestSubnetIdentityCommitmentCensusBoundsAndLegacy(t *testing.T) {
	world := newSubnetWorld(t)
	other := subnetWorldKey("legacy-validator")
	legacy := append(make([]byte, 12), 0x04, 0x02)
	legacy[8] = 7
	legacy = append(legacy, make([]byte, 32)...)
	legacy[len(legacy)-1] = 1
	world.setRawStorage(crv4.CommitmentsPalletName, "CommitmentOf", legacy, subnetNetuidArg(world.deployment.Netuid), other[:])
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()))
	world.settings.Limits.MaxCommitments = 1
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetCommitmentMirrorLagClass, "subnet-identity/commitments", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "commitment-census-exceeds-bound") {
		t.Fatal(alert.Observed)
	}
}

// Rebuilds the world's binding with one mutation and re-signs it correctly
// unless the mutation is to a signature.
func subnetWorldReplaceBinding(t *testing.T, world *subnetWorld, mutate func(*stabi.STCoordinatorFleetBinding), signature func(clientSignature []byte) []byte) {
	t.Helper()
	coordinatorAbi := subnetWorldAbi(t, stabi.STCoordinatorMetaData.ParseABI)
	world.logs = world.logs[:0]
	world.txs = map[common.Hash]subnetWorldTransaction{}
	hotkey, client := subnetWorldFleetKeys(t)
	binding := stabi.STCoordinatorFleetBinding{
		ChainId: world.deployment.ChainId, Netuid: world.deployment.Netuid, Coordinator: world.coordinator,
		FleetId: subnetWorldKey("fleet-1"), Hotkey: hotkey.PublicKey(), ClientId: [16]byte(subnetWorldHash("fleet-1-client").Bytes()[:16]),
		ClientKey: [32]byte(client.Public().(ed25519.PublicKey)), Generation: 1, ValidFromEpoch: 4, ValidToEpoch: 5, CommitmentHash: subnetWorldKey("fleet-1-manifest"),
	}
	mutate(&binding)
	world.addFleetBinding(binding, client, hotkey, world.finalized-50)
	if signature != nil {
		for hash, transaction := range world.txs {
			method := coordinatorAbi.Methods["bindFleetMember"]
			values, err := method.Inputs.Unpack(transaction.input[4:])
			if err != nil {
				t.Fatal(err)
			}
			data, err := method.Inputs.Pack(values[0], signature(values[1].([]byte)), values[2])
			if err != nil {
				t.Fatal(err)
			}
			transaction.input = append(append([]byte{}, method.ID...), data...)
			world.txs[hash] = transaction
		}
	}
}

func TestSubnetIdentityAcceptedInvalidBindingPages(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*stabi.STCoordinatorFleetBinding)
		signature func([]byte) []byte
		reason    string
	}{
		{"client-signature", func(*stabi.STCoordinatorFleetBinding) {}, func(signature []byte) []byte {
			changed := append([]byte{}, signature...)
			changed[0] ^= 1
			return changed
		}, "client-signature"},
		{"domain", func(binding *stabi.STCoordinatorFleetBinding) { binding.ChainId = 945 }, nil, "binding-domain"},
		{"not-future", func(binding *stabi.STCoordinatorFleetBinding) { binding.ValidFromEpoch = 3 }, nil, "binding-not-future-effective"},
		{"span", func(binding *stabi.STCoordinatorFleetBinding) { binding.ValidToEpoch = binding.ValidFromEpoch + 12 }, nil, "binding-validity-span"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			subnetWorldReplaceBinding(t, world, test.mutate, test.signature)
			alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetFleetBindingInvalidClass, "subnet-identity/bindings", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatal(alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// A binding sent through a contract wallet has no direct calldata to verify.
func TestSubnetIdentityIndirectBindingIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	for hash, transaction := range world.txs {
		transaction.to = subnetWorldAddress("owner-safe")
		world.txs[hash] = transaction
	}
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetFleetBindingInvalidClass, "subnet-identity/bindings", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "binding-calldata-indirect") || len(alerts) != 1 {
		t.Fatalf("indirect binding = %+v", alerts)
	}
}

func TestSubnetIdentityTransactionSourceLossIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == "eth_getTransactionByHash" {
			return nil, errors.New("pruned"), true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetFleetBindingInvalidClass, "subnet-identity/bindings", "unobserved", SeverityWarn)
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatal("source loss produced a proved violation")
		}
	}
}

func TestSubnetIdentityRegistrationMetadata(t *testing.T) {
	signal := NewSubnetIdentitySignal()
	if signal.Number() != "22.2" || signal.Key() != "subnet-identity" || signal.ID() != "subnet/identity" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
}

// §22.10 acceptance: a pool hotkey owned by a foreign coldkey is a proved
// custody counterexample; restoring the vault owner is the healthy read.
func init() {
	subnetFamilyAcceptanceCases["22.2"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			return NewSubnetIdentitySignal(), newSubnetWorld(t).signalSettings()
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			world := newSubnetWorld(t)
			pool := subnetWorldKey("operator-1-pool")
			original := types.AccountID(subnetMirroredColdkey(world.vault))
			world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("foreign")), pool[:])
			return NewSubnetIdentitySignal(), world.signalSettings(), func() {
				world.setStorage("SubtensorModule", "Owner", original, pool[:])
			}
		},
		violatedClass: subnetPoolCustodyIdentityClass,
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			return subnetWorldAcceptanceUnknown(t, NewSubnetIdentitySignal(), "eth_getTransactionByHash")
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			return subnetWorldAcceptanceCanceled(t, NewSubnetIdentitySignal(), "eth_getTransactionByHash")
		},
	}
}
