// SIGNALS.md §22.7 maps to signal_subnet_settlement.go and
// signal_subnet_settlement_test.go. The family reader replays pool emission
// capture, roots, claims, carry and vault conservation from one finalized evm
// checkpoint of the reviewed deployment: the state census decides the exact
// §6.1 identities, the retained event journal reconstructs counters and
// claim attribution, and committed roots are reproduced from their artifacts.
package monitor

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Classes this family decides; a gap names the subset it leaves undecided.
var subnetSettlementClasses = []string{
	"subnet-epoch-progress", "subnet-capture-delta", "subnet-payout-root", "subnet-claim-correctness",
	"subnet-claim-credit-payment", "subnet-carry-isolation", "subnet-vault-conservation",
}

// Classes that need the complete event journal.
var subnetSettlementJournalClasses = []string{
	"subnet-capture-delta", "subnet-claim-correctness", "subnet-claim-credit-payment", "subnet-carry-isolation",
}

func NewSubnetSettlementSignal() Signal {
	return &signalAdapter{
		number: "22.7",
		key:    "subnet-settlement",
		name:   "Subnet pool capture, roots, claims, carry and conservation",
		probe:  newSubnetSettlementProbe(),
	}
}

// One signal instance owns its retained frontier, verified artifacts and
// counterexample latch for the watcher's lifetime.
type subnetSettlementProbe struct {
	latch    *subnetReplayLatch
	journal  *subnetSettlementJournal
	verified *subnetSettlementVerified
}

func newSubnetSettlementProbe() *subnetSettlementProbe {
	return &subnetSettlementProbe{latch: newSubnetReplayLatch(), journal: newSubnetSettlementJournal(), verified: newSubnetSettlementVerified()}
}

func (self *subnetSettlementProbe) id() string { return subnetSettlementProbeId }

func (self *subnetSettlementProbe) tier() string { return tierPage }

func (self *subnetSettlementProbe) cadence() time.Duration {
	return DefaultSubnetReplaySettings().Cadence
}

func (self *subnetSettlementProbe) runBudget() time.Duration {
	return DefaultSubnetReplaySettings().RunBudget
}

func (self *subnetSettlementProbe) check(ctx context.Context, env *probeEnv) (findings []finding, resultErr error) {
	settings := env.cfg.subnetReplay
	scope := subnetReplayScope{environment: env.cfg.env, checkpointKind: "evm", source: "archive-relay"}
	coverage := func(frame, reason string, undecided []string) finding {
		return subnetReplayCoverage(subnetSettlementProbeId, "subnet-settlement/coverage", frame, scope, subnetSettlementPlaybook, reason, undecided, 2)
	}
	deployment := subnetReplayDeploymentOf(env)
	if deployment.notDeployed {
		return nil, nil
	}
	if deployment.reason != "" {
		return []finding{coverage("deployment", deployment.reason, subnetSettlementClasses)}, nil
	}
	if reason := settings.budgetReason(); reason != "" {
		return []finding{coverage("budget", reason, subnetSettlementClasses)}, nil
	}
	pin := deployment.pin
	gaps := []finding{}
	enrollmentValid := settings.enrollmentReason() == ""
	if !enrollmentValid {
		gaps = append(gaps, coverage("enrollment", settings.enrollmentReason(), append(append([]string(nil), subnetSettlementJournalClasses...), "subnet-payout-root")))
	}
	if pin.settlementV2Declared {
		gaps = append(gaps, coverage("settlement-v2", "settlement-v2-reader-unimplemented", subnetSettlementClasses))
	}
	target, rpcPort, reason := selectSubnetReplayEndpoint(env, pin)
	if reason != "" {
		return append(gaps, coverage("source", reason, subnetSettlementClasses)), nil
	}
	runCtx, cancel := context.WithTimeout(ctx, settings.RunBudget)
	defer cancel()
	// Any parent cancellation withholds every verdict, keeping its cause.
	defer func() {
		select {
		case <-ctx.Done():
			findings = nil
			resultErr = &signalContextError{contextErr: ctx.Err(), sourceErr: resultErr}
		default:
		}
	}()
	sourceGap := func(err error) ([]finding, error) {
		if reason := subnetReplayUnknownReason(err); reason != "" {
			return append(gaps, coverage("source", reason, subnetSettlementClasses)), nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return append(gaps, coverage("source", "run-budget-exhausted", subnetSettlementClasses)), nil
		}
		return nil, err
	}
	rpc := newSubnetReplayRpc(&subnetReplaySshRelay{runner: env.runner, target: target, rpcPort: rpcPort, settings: settings}, settings)
	state, err := readSubnetSettlementState(runCtx, rpc, pin, settings)
	if err != nil {
		return sourceGap(err)
	}
	scope.checkpointNumber = state.checkpoint.number
	scope.completeness = "complete"
	if !state.censusComplete {
		scope.completeness = "partial:" + state.censusReason
		gaps = append(gaps, coverage("census", state.censusReason, []string{"subnet-vault-conservation", "subnet-carry-isolation", "subnet-claim-credit-payment"}))
	}
	verdicts := evaluateSubnetSettlementState(state, settings, scope)

	// The event journal: read toward the checkpoint, evaluate a snapshot,
	// commit only after the closing recheck.
	contracts, err := subnetSettlementAbis()
	if err != nil {
		return sourceGap(newSubnetReplayUnknown("binding-unavailable", err))
	}
	decoder := newSubnetSettlementDecoder(contracts, state.vault.minimumTransferTaoRao)
	var windows []*subnetReplayLogWindow
	switch {
	case !enrollmentValid:
	case settings.ReplayFromBlock == 0:
		gaps = append(gaps, coverage("event-journal", "replay-not-enrolled", subnetSettlementJournalClasses))
	default:
		windows, err = readSubnetSettlementJournalWindows(runCtx, rpc, self.journal, pin.settlementVault, settings.ReplayFromBlock, state.checkpoint, settings)
		if err != nil {
			if reason := subnetReplayUnknownReason(err); reason != "" {
				gaps = append(gaps, coverage("event-journal", reason, subnetSettlementJournalClasses))
				windows = nil
			} else {
				return sourceGap(err)
			}
		}
		if err == nil {
			next, replay := self.journal.Snapshot()
			for _, window := range windows {
				if window.from != next {
					break
				}
				for i := range window.logs {
					decoder.apply(replay, &window.logs[i])
				}
				next = window.to + 1
			}
			if next != state.checkpoint.number+1 {
				remaining := state.checkpoint.number + 1 - next
				gaps = append(gaps, coverage("event-journal", "frontier-behind-"+subnetReplayBlocksBucket(remaining), subnetSettlementJournalClasses))
			} else {
				credits, complete, err := readSubnetSettlementCredits(runCtx, rpc, contracts, pin.settlementVault, state.checkpoint, replay, settings)
				if err != nil {
					if reason := subnetReplayUnknownReason(err); reason == "" {
						return sourceGap(err)
					} else {
						gaps = append(gaps, coverage("claimant-credit", reason, []string{"subnet-claim-credit-payment"}))
					}
				}
				if !complete {
					gaps = append(gaps, coverage("claimant-credit", "claimant-credit-bound", []string{"subnet-claim-credit-payment"}))
				}
				verdicts = append(verdicts, evaluateSubnetSettlementJournal(state, replay, credits, settings, scope)...)
			}
		}
	}

	// Root reproduction from the enrolled public origins.
	if enrollmentValid && len(settings.ApiOrigins) == 2 {
		client, closeIdle := subnetReplayApiClient(env, settings)
		reproduced, err := reproduceSubnetSettlementArtifacts(runCtx, client, settings.ApiOrigins, pin, state, self.verified, settings, scope)
		closeIdle()
		if err != nil {
			return sourceGap(err)
		}
		verdicts = append(verdicts, reproduced...)
	} else if enrollmentValid {
		gaps = append(gaps, coverage("artifact-reproduction", "api-origins-not-enrolled", []string{"subnet-payout-root"}))
	}

	// Recheck canonicality before any verdict or cursor advance.
	if err := recheckSubnetReplayEvmCheckpoint(runCtx, rpc, state.checkpoint); err != nil {
		return sourceGap(err)
	}
	if 0 < len(windows) {
		self.journal.Apply(pin.settlementVault, settings.ReplayFromBlock, windows, decoder)
	}
	return self.latch.Retain(env.now(), append(gaps, verdicts...)), nil
}

// Reads current claimCredit for the bounded claimant set at the checkpoint.
func readSubnetSettlementCredits(ctx context.Context, rpc *subnetReplayRpc, contracts subnetSettlementContracts, vault common.Address, checkpoint subnetReplayEvmCheckpoint, replay *subnetSettlementReplay, settings *SubnetReplaySettings) (map[[32]byte]*big.Int, bool, error) {
	coldkeys, complete := subnetSettlementCreditColdkeys(replay, settings.MaximumCreditChecks)
	views := make([]*subnetReplayView, len(coldkeys))
	for i, coldkey := range coldkeys {
		views[i] = newSubnetReplayView(contracts.vault, vault, "claimCredit", coldkey)
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, complete, err
	}
	credits := map[[32]byte]*big.Int{}
	for i, coldkey := range coldkeys {
		value, err := subnetReplayValue[*big.Int](views[i], 0)
		if err != nil {
			return nil, complete, err
		}
		credits[coldkey] = value
	}
	return credits, complete, nil
}

// A bounded public label for a frontier gap: exact block counts would create
// a new identity every run.
func subnetReplayBlocksBucket(blocks uint64) string {
	switch {
	case blocks <= 1000:
		return "under-1k-blocks"
	case blocks <= 10000:
		return "under-10k-blocks"
	case blocks <= 100000:
		return "under-100k-blocks"
	default:
		return "over-100k-blocks"
	}
}
