// Synthetic controls for the §22.3 reader over the deterministic world.
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

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/stabi"
)

func TestSubnetDepositsHealthyZeroPriceLaunch(t *testing.T) {
	world := newSubnetWorld(t)
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	requireNoSubnetAlerts(t, alerts)
	if len(world.requests) == 0 || world.calls[subnetWorldEvm1+" eth_call"]+world.calls[subnetWorldEvm1+" eth_getLogs"] == 0 {
		t.Fatal("the reader did not observe its sources")
	}
}

func TestSubnetDepositsReservePrincipalViolations(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"deficit", func(world *subnetWorld) {
			world.contracts[world.sink].views["liveStake"] = subnetWorldConstant(big.NewInt(1499))
		}, "reserve-deficit"},
		{"read-paths", func(world *subnetWorld) {
			world.contracts[subnetStakingPrecompile].views["getStake"] = subnetWorldConstant(big.NewInt(1600))
		}, "live-stake-read-paths-differ"},
		{"operator-sum", func(world *subnetWorld) {
			world.contracts[world.sink].views["principal"] = subnetWorldConstant(big.NewInt(1400))
		}, "operator-principal-sum"},
		{"conviction", func(world *subnetWorld) {
			world.contracts[world.coordinator].views["cumulativeConviction"] = subnetWorldConstant(big.NewInt(1))
		}, "conviction-principal-differs:1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetReservePrincipalClass, "subnet-deposits/reserve", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) || alert.Sustain != subnetProvedSustain {
				t.Fatal(alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// The reserve never loses stake, principal or its hotkey between runs.
func TestSubnetDepositsReserveHistoryAcrossRuns(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"outflow", func(world *subnetWorld) {
			world.contracts[world.sink].views["liveStake"] = subnetWorldConstant(big.NewInt(1502))
		}, "reserve-outflow"},
		{"hotkey", func(world *subnetWorld) {
			world.contracts[world.sink].views["reserveHotkey"] = subnetWorldConstant(subnetWorldKey("other-reserve"))
		}, "reserve-hotkey-changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			signal := NewSubnetDepositsSignal()
			settings := world.signalSettings()
			requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
			world.moveFinalized(world.finalized + 10)
			test.mutate(world)
			if test.name == "hotkey" {
				reserve := subnetWorldKey("other-reserve")
				world.setStorage("SubtensorModule", "Delegates", types.NewU16(0), reserve[:])
				world.setStorage("SubtensorModule", "ChildkeyTake", types.NewU16(0), reserve[:], subnetNetuidArg(world.deployment.Netuid))
				world.contracts[subnetStakingPrecompile].views["getStake"] = subnetWorldConstant(big.NewInt(1504))
			}
			alerts := runSubnetWorld(t, signal, settings)
			alert := requireSubnetAlert(t, alerts, subnetReservePrincipalClass, "subnet-deposits/reserve", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatal(alert.Observed)
			}
		})
	}
	// One rao of share rounding is within the runtime allowance.
	world := newSubnetWorld(t)
	signal := NewSubnetDepositsSignal()
	settings := world.signalSettings()
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
	world.moveFinalized(world.finalized + 10)
	world.contracts[world.sink].views["liveStake"] = subnetWorldConstant(big.NewInt(1503))
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
}

// A withheld run must not advance the reserve history it would compare to.
func TestSubnetDepositsWithheldRunDoesNotAdvanceHistory(t *testing.T) {
	world := newSubnetWorld(t)
	signal := NewSubnetDepositsSignal()
	settings := world.signalSettings()
	checks := 0
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		checks++
		return checks != 2, nil
	}
	world.contracts[world.sink].views["liveStake"] = subnetWorldConstant(big.NewInt(2000))
	world.contracts[subnetStakingPrecompile].views["getStake"] = subnetWorldConstant(big.NewInt(2000))
	alerts := runSubnetWorld(t, signal, settings)
	if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "settings-generation-stale") {
		t.Fatalf("withheld run = %+v", alerts)
	}
	world.moveFinalized(world.finalized + 10)
	world.contracts[world.sink].views["liveStake"] = subnetWorldConstant(big.NewInt(1504))
	world.contracts[subnetStakingPrecompile].views["getStake"] = subnetWorldConstant(big.NewInt(1504))
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
}

func TestSubnetDepositsReserveTake(t *testing.T) {
	world := newSubnetWorld(t)
	reserve := subnetWorldKey("reserve")
	world.setStorage("SubtensorModule", "Delegates", types.NewU16(100), reserve[:])
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetReserveYieldClass, "subnet-deposits/reserve", "", SeverityPage)
	if !strings.Contains(alert.Observed, "reserve-delegate-take") {
		t.Fatal(alert.Observed)
	}
	// An unset take is the runtime's nonzero default take, not zero.
	world = newSubnetWorld(t)
	world.deleteStorage("SubtensorModule", "Delegates", reserve[:])
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetReserveYieldClass, "subnet-deposits/reserve", "", SeverityPage)
}

// A deposit and its receipt, emitted in one transaction.
func subnetWorldDepositPair(t *testing.T, world *subnetWorld, block uint64, noId, epoch, nonce int64, amount, total, operator, live *big.Int, policyHash [32]byte, conviction bool) {
	t.Helper()
	coordinatorAbi := subnetWorldAbi(t, stabi.STCoordinatorMetaData.ParseABI)
	sinkAbi := subnetWorldAbi(t, stabi.STReserveSinkMetaData.ParseABI)
	index := uint(len(world.logs))
	receipt := world.emit(world.sink, sinkAbi, "ReservePrincipalAdded", block, index, map[string]any{
		"epoch": big.NewInt(epoch), "noId": big.NewInt(noId), "amount": amount, "operatorPrincipal": operator, "totalPrincipal": total, "liveStake": live,
	})
	name := "Deposit"
	if conviction {
		name = "ConvictionAdded"
	}
	world.emit(world.coordinator, coordinatorAbi, name, block, index+1, map[string]any{
		"noId": big.NewInt(noId), "epoch": big.NewInt(epoch), "funder": subnetWorldAddress("funder"), "amount": amount, "policyHash": policyHash, "nonce": big.NewInt(nonce),
	})
	world.logs[len(world.logs)-1].TxHash = receipt
}

// Removes the deposit and receipt logs, keeping every other contract's.
func subnetWorldDropDeposits(world *subnetWorld) {
	kept := world.logs[:0]
	for _, log := range world.logs {
		if log.Address != world.sink && !(log.Address == world.coordinator && len(log.Topics) != 0 && subnetWorldIsDepositTopic(log.Topics[0])) {
			kept = append(kept, log)
		}
	}
	world.logs = kept
}

func subnetWorldIsDepositTopic(topic common.Hash) bool {
	coordinatorAbi, _ := stabi.STCoordinatorMetaData.ParseABI()
	return topic == coordinatorAbi.Events["Deposit"].ID || topic == coordinatorAbi.Events["ConvictionAdded"].ID
}

func TestSubnetDepositsEventAttribution(t *testing.T) {
	policyHash := func(world *subnetWorld) [32]byte { return world.policyVersions[1].PolicyHash }
	healthy := func(world *subnetWorld) {
		subnetWorldDepositPair(t, world, world.finalized-100, 1, 3, 7, big.NewInt(10), big.NewInt(1510), big.NewInt(510), big.NewInt(1514), policyHash(world), false)
		world.contracts[world.coordinator].views["nextDepositNonce"] = func(inputs []any) ([]any, error) {
			if inputs[0].(*big.Int).Uint64() == 1 {
				return []any{big.NewInt(8)}, nil
			}
			return []any{new(big.Int)}, nil
		}
		world.contracts[world.sink].views["principal"] = subnetWorldConstant(big.NewInt(1510))
		world.contracts[world.sink].views["liveStake"] = subnetWorldConstant(big.NewInt(1514))
		world.contracts[subnetStakingPrecompile].views["getStake"] = subnetWorldConstant(big.NewInt(1514))
		world.contracts[world.sink].views["operatorPrincipal"] = func(inputs []any) ([]any, error) {
			if inputs[0].(*big.Int).Uint64() == 1 {
				return []any{big.NewInt(510)}, nil
			}
			return []any{big.NewInt(1000)}, nil
		}
		world.contracts[world.coordinator].views["cumulativeConviction"] = world.contracts[world.sink].views["operatorPrincipal"]
	}
	world := newSubnetWorld(t)
	healthy(world)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"no-receipt", func(world *subnetWorld) {
			kept := world.logs[:0]
			for _, log := range world.logs {
				if log.Address != world.sink {
					kept = append(kept, log)
				}
			}
			world.logs = kept
		}, "deposit-without-receipt"},
		{"receipt-amount", func(world *subnetWorld) {
			sinkAbi := subnetWorldAbi(t, stabi.STReserveSinkMetaData.ParseABI)
			data, _ := sinkAbi.Events["ReservePrincipalAdded"].Inputs.NonIndexed().Pack(big.NewInt(9), big.NewInt(510), big.NewInt(1510), big.NewInt(1514))
			for i := range world.logs {
				if world.logs[i].Address == world.sink {
					world.logs[i].Data = data
				}
			}
		}, "deposit-without-receipt"},
		{"policy-hash", func(world *subnetWorld) {
			subnetWorldDropDeposits(world)
			subnetWorldDepositPair(t, world, world.finalized-100, 1, 3, 7, big.NewInt(10), big.NewInt(1510), big.NewInt(510), big.NewInt(1514), subnetWorldKey("other-policy"), false)
		}, "deposit-policy-hash"},
		{"epoch", func(world *subnetWorld) {
			subnetWorldDropDeposits(world)
			subnetWorldDepositPair(t, world, world.finalized-100, 1, 2, 7, big.NewInt(10), big.NewInt(1510), big.NewInt(510), big.NewInt(1514), policyHash(world), false)
		}, "deposit-epoch"},
		{"nonce-counter", func(world *subnetWorld) {
			world.contracts[world.coordinator].views["nextDepositNonce"] = subnetWorldConstant(big.NewInt(9))
		}, "nonce-counter-differs:1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			healthy(world)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetDepositAttributionClass, "subnet-deposits/events", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatal(alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// Nonces are one shared sequence per operator across runs.
func TestSubnetDepositsNonceSequenceAcrossRuns(t *testing.T) {
	world := newSubnetWorld(t)
	signal := NewSubnetDepositsSignal()
	settings := world.signalSettings()
	world.contracts[world.coordinator].views["nextDepositNonce"] = subnetWorldConstant(big.NewInt(0))
	subnetWorldDepositPair(t, world, world.finalized-100, 1, 3, 7, big.NewInt(10), big.NewInt(1510), big.NewInt(510), big.NewInt(1514), world.policyVersions[1].PolicyHash, false)
	alerts := runSubnetWorld(t, signal, settings)
	requireSubnetAlert(t, alerts, subnetDepositAttributionClass, "subnet-deposits/events", "", SeverityPage)
	// The later window skips nonce 8.
	world.moveFinalized(world.finalized + 200)
	subnetWorldDepositPair(t, world, world.finalized-50, 1, 3, 9, big.NewInt(10), big.NewInt(1520), big.NewInt(520), big.NewInt(1524), world.policyVersions[1].PolicyHash, true)
	alerts = runSubnetWorld(t, signal, settings)
	alert := requireSubnetAlert(t, alerts, subnetDepositAttributionClass, "subnet-deposits/events", "", SeverityPage)
	if !strings.Contains(alert.Observed, "nonce-gap") {
		t.Fatal(alert.Observed)
	}
}

// Once the frontier covers the epoch, the counters must equal the census.
func TestSubnetDepositsEpochCountersWhenCovered(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.Limits.InitialLogWindowBlocks = 20000
	world.settings.Limits.LogChunkBlocks = 5000
	world.contracts[world.coordinator].views["epochDeposits"] = func(inputs []any) ([]any, error) {
		if inputs[1].(*big.Int).Uint64() == 1 {
			return []any{big.NewInt(25)}, nil
		}
		return []any{new(big.Int)}, nil
	}
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetDepositAttributionClass, "subnet-deposits/events", "", SeverityPage)
	if !strings.Contains(alert.Observed, "epoch-deposit-counter-differs:1") {
		t.Fatal(alert.Observed)
	}
	// Without full epoch coverage the counters are not judged.
	world = newSubnetWorld(t)
	world.contracts[world.coordinator].views["epochDeposits"] = func(inputs []any) ([]any, error) {
		return []any{big.NewInt(25)}, nil
	}
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	if len(subnetAlerts(alerts, subnetDepositAttributionClass, "subnet-deposits/events")) != 0 {
		t.Fatalf("an uncovered epoch was judged: %+v", alerts)
	}
}

func TestSubnetDepositsReceiptCredit(t *testing.T) {
	for _, test := range []struct {
		name   string
		second []*big.Int
		reason string
	}{
		{"total-chain", []*big.Int{big.NewInt(10), big.NewInt(1521), big.NewInt(520), big.NewInt(1530)}, "total-principal-chain"},
		{"deficit", []*big.Int{big.NewInt(10), big.NewInt(1520), big.NewInt(520), big.NewInt(1519)}, "receipt-deficit"},
		{"surplus-short", []*big.Int{big.NewInt(10), big.NewInt(1520), big.NewInt(520), big.NewInt(1521)}, "receipt-credit-short"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			policyHash := world.policyVersions[1].PolicyHash
			subnetWorldDepositPair(t, world, world.finalized-100, 1, 3, 7, big.NewInt(10), big.NewInt(1510), big.NewInt(510), big.NewInt(1514), policyHash, false)
			subnetWorldDepositPair(t, world, world.finalized-90, 1, 3, 8, test.second[0], test.second[1], test.second[2], test.second[3], policyHash, true)
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetReserveCreditDeltaClass, "subnet-deposits/reserve-receipts", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatal(alert.Observed)
			}
		})
	}
}

// A priced epoch whose operators deposit exactly the required amount.
func subnetWorldPricedDeposits(t *testing.T, world *subnetWorld) map[uint64]*big.Int {
	t.Helper()
	world.usePricedPolicy()
	required := map[uint64]*big.Int{}
	for noId := uint64(1); noId <= 2; noId++ {
		_, amount := world.commitArtifact(noId, world.currentEpoch-1, []uint64{1000 * noId, 2000})
		required[noId] = amount
		world.setDeposit(noId, amount)
	}
	world.contracts[subnetStakingPrecompile].views["getStake"] = func([]any) ([]any, error) {
		return world.contracts[world.sink].views["liveStake"](nil)
	}
	return required
}

func TestSubnetDepositsReplaysTheExactDepositAudit(t *testing.T) {
	world := newSubnetWorld(t)
	subnetWorldPricedDeposits(t, world)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	for _, test := range []struct {
		name  string
		delta int64
	}{{"under", -1}, {"over", 1}} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			required := subnetWorldPricedDeposits(t, world)
			world.setDeposit(1, new(big.Int).Add(required[1], big.NewInt(test.delta)))
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
			if !strings.Contains(alert.Observed, "state=deposit_mismatch") || !strings.Contains(alert.Observed, "disposition=zero_pool_weight") {
				t.Fatal(alert.Observed)
			}
			if len(subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-2")) != 0 {
				t.Fatal("a compliant sibling operator was flagged")
			}
			world.requirePrivate(t, alerts)
		})
	}
}

func TestSubnetDepositsArtifactStates(t *testing.T) {
	// Before the root window closes a missing commitment is pending.
	world := newSubnetWorld(t)
	world.usePricedPolicy()
	world.moveFinalized(world.epochStartBlock(world.currentEpoch) + 100)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	// After it closes, every operator is unavailable: each warns and the
	// whole set pages.
	world = newSubnetWorld(t)
	world.usePricedPolicy()
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "state=artifact_unavailable") {
		t.Fatal(alert.Observed)
	}
	requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "", SeverityPage)
	// An origin serving another artifact under the committed name equivocates.
	world = newSubnetWorld(t)
	subnetWorldPricedDeposits(t, world)
	route := world.routes["/sn/artifact"]
	world.routes["/sn/artifact"] = func(request *http.Request) *http.Response {
		response := route(request)
		if request.URL.Host == "api-two.private.example.test" {
			response.Header.Set("ETag", `"sha256:`+strings.Repeat("ab", 32)+`"`)
		}
		return response
	}
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "state=artifact_equivocation") {
		t.Fatal(alert.Observed)
	}
	// Unreadable origins keep the audit unknown, never a mismatch.
	world = newSubnetWorld(t)
	subnetWorldPricedDeposits(t, world)
	world.routes["/sn/artifact"] = func(request *http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}
	}
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "unobserved", SeverityWarn)
	for _, alert := range subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "") {
		if alert.Frame != "unobserved" {
			t.Fatalf("unreadable artifacts produced a decided audit: %+v", alert)
		}
	}
}

func TestSubnetDepositsPolicyDocumentAbsentIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.PolicyDocument = ""
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "policy-document-not-enrolled") || len(alerts) != 1 {
		t.Fatalf("absent policy = %+v", alerts)
	}
}

func TestSubnetDepositsPenaltyBypass(t *testing.T) {
	setup := func(t *testing.T, poolWeight uint16, updated uint64) *subnetWorld {
		world := newSubnetWorld(t)
		required := subnetWorldPricedDeposits(t, world)
		world.setDeposit(1, new(big.Int).Add(required[1], big.NewInt(1)))
		world.setWeights(1, []subnetWeightPair{{Uid: 11, Weight: poolWeight}, {Uid: 12, Weight: 1000}}, updated)
		world.setWeights(2, []subnetWeightPair{{Uid: 12, Weight: 1000}}, updated)
		return world
	}
	world := setup(t, 500, 119000)
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetDepositPenaltyBypassClass, "subnet-deposits/no-1", "", SeverityPage)
	if !strings.Contains(alert.Observed, "rows_weighting_pool=1") {
		t.Fatal(alert.Observed)
	}
	// A row applied before the audit's native cycle is old weight.
	world = setup(t, 500, world.epochStartBlock(world.currentEpoch)+100)
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	if len(subnetAlerts(alerts, subnetDepositPenaltyBypassClass, "")) != 0 {
		t.Fatalf("an old applied row was judged a bypass: %+v", alerts)
	}
	// A zeroed pool is the enforced penalty.
	world = setup(t, 0, 119000)
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	if len(subnetAlerts(alerts, subnetDepositPenaltyBypassClass, "")) != 0 {
		t.Fatalf("a zeroed pool was judged a bypass: %+v", alerts)
	}
}

func TestSubnetDepositsEventSourceLossIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == "eth_getLogs" {
			return nil, errors.New("range too large"), true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetDepositAttributionClass, "subnet-deposits/events", "unobserved", SeverityWarn)
	requireSubnetAlert(t, alerts, subnetReserveCreditDeltaClass, "subnet-deposits/reserve-receipts", "unobserved", SeverityWarn)
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("log loss produced a proved violation: %+v", alert)
		}
	}
}

func TestSubnetDepositsRegistrationMetadata(t *testing.T) {
	signal := NewSubnetDepositsSignal()
	if signal.Number() != "22.3" || signal.Key() != "subnet-deposits" || signal.ID() != "subnet/deposits" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
	_, _ = abi.JSON(strings.NewReader(subnetStakingAbiJson))
	_ = common.Address{}
}
