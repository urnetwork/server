// SIGNALS.md §22.7.1: the root deadline must be observed independently of
// payout computation, task results, artifact publication and history replay.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const subnetRootDeadlineProbeId = "subnet/root-deadline"
const subnetRootDeadlinePlaybook = "SIGNALS.md §22.7.1"

func NewSubnetRootDeadlineSignal() Signal {
	return &signalAdapter{
		number: "22.7.1", key: "subnet-root-deadline",
		name: "Subnet payout root commit deadline", probe: &subnetRootDeadlineProbe{},
	}
}

type subnetRootDeadlineProbe struct{}

func (*subnetRootDeadlineProbe) id() string             { return subnetRootDeadlineProbeId }
func (*subnetRootDeadlineProbe) tier() string           { return tierPage }
func (*subnetRootDeadlineProbe) cadence() time.Duration { return DefaultSubnetReplaySettings().Cadence }
func (*subnetRootDeadlineProbe) runBudget() time.Duration {
	return DefaultSubnetReplaySettings().RunBudget
}

func (*subnetRootDeadlineProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.subnetReplay
	scope := subnetReplayScope{environment: env.cfg.env, checkpointKind: "evm", source: "archive-relay"}
	gap := func(reason string) ([]finding, error) {
		return []finding{subnetReplayCoverage(subnetRootDeadlineProbeId, "subnet-root-deadline/coverage", "source", scope,
			subnetRootDeadlinePlaybook, reason, []string{"subnet-root-deadline"}, 1)}, nil
	}
	deployment := subnetReplayDeploymentOf(env)
	if deployment.notDeployed {
		return nil, nil
	}
	if deployment.reason != "" {
		return gap(deployment.reason)
	}
	if reason := settings.budgetReason(); reason != "" {
		return gap(reason)
	}
	if settings.RootDeadlineLeadBlocks == 0 {
		return gap("deadline-lead-invalid")
	}
	target, rpcPort, reason := selectSubnetReplayEndpoint(env, deployment.pin)
	if reason != "" {
		return gap(reason)
	}
	runCtx, cancel := context.WithTimeout(ctx, settings.RunBudget)
	defer cancel()
	rpc := newSubnetReplayRpc(&subnetReplaySshRelay{runner: env.runner, target: target, rpcPort: rpcPort, settings: settings}, settings)
	state, err := readSubnetRootDeadline(runCtx, rpc, deployment.pin, settings)
	if err == nil {
		scope.checkpointNumber = state.checkpoint.number
		err = recheckSubnetReplayEvmCheckpoint(runCtx, rpc, state.checkpoint)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if reason := subnetReplayUnknownReason(err); reason != "" {
			return gap(reason)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return gap("run-budget-exhausted")
		}
		return nil, err
	}
	scope.completeness = "complete"
	findings := []finding{}
	for _, missing := range state.missing {
		remaining := uint64(0)
		if state.checkpoint.number < state.deadline {
			remaining = state.deadline - state.checkpoint.number
		}
		findings = append(findings, subnetReplayFinding(subnetRootDeadlineProbeId, "subnet-root-deadline",
			fmt.Sprintf("subnet-root-deadline/no-%d", missing.noId), subnetSettlementEpochFrame(state.epoch, "root"), scope,
			subnetRootDeadlinePlaybook, subnetReplayIssue{
				tier: tierPage, sustain: 1, invariant: "root-confirmed-before-deadline-lead",
				expected: "confirmed-root", observed: "root-absent settlement=" + missing.version,
				deadline:  fmt.Sprintf("deadline_block=%d finalized_block=%d remaining_blocks=%d deadline_passed=%t", state.deadline, state.checkpoint.number, remaining, state.deadline < state.checkpoint.number),
				symptom:   "An operator's payout root is still unconfirmed near or past its commit deadline",
				mechanism: "The independent finalized chain read found no root for the latest closed epoch. A running census, absent leaves or unpublished artifact cannot suppress this deadline page.",
				baseline:  "Confirm each due root before the deadline lead; a v2 epoch closed with no funding above the last finalized total needs no new root.",
				action:    "Check the epoch close, payout computation, artifact publication and root transaction now. Confirm any intentional no-allocation outcome. Resolve ambiguous transactions before retrying; after the deadline record the miss and repair the next epoch, without backdating a root.",
				verify:    "The root is visible at a finalized checkpoint, or the v2 close proves no new funding to allocate. Preserve a missed-window incident for follow-up even after the next epoch replaces this scope.",
			}))
	}
	return findings, nil
}
