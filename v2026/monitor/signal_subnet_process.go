// SIGNALS.md §22.9 maps to signal_subnet_process.go and
// signal_subnet_process_test.go. The family reader checks the operator
// service's process correctness against the chain: its finalized event mirror
// advances and stays on canonical history, its subnet tasks run, its
// chain-write intents resolve, and every scoped writer (each active operator's
// root and deposit signers, and every producer intent sender) is funded. The
// validator and miner process, capacity, read-ownership, quota, stream and
// adversarial classes need exporters that do not exist; they stay an explicit
// coverage gap rather than a green absence.
package monitor

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const subnetProcessProbeId = "subnet/process"
const subnetProcessPlaybook = "SIGNALS.md §22.9"

var subnetProcessClasses = []string{"subnet-runtime-liveness", "subnet-state-durability", "subnet-writer-funding"}

// Classes no reachable source decides today.
var subnetProcessTelemetryClasses = []string{
	"subnet-capacity-margin", "subnet-rpc-read-ownership", "subnet-observation-quota",
	"subnet-evidence-stream-capacity", "subnet-adversarial-resilience",
}

func NewSubnetProcessSignal() Signal {
	return &signalAdapter{
		number: "22.9",
		key:    "subnet-process",
		name:   "Subnet process correctness, resources and adversarial noise",
		probe:  newSubnetProcessProbe(),
	}
}

type subnetProcessProbe struct {
	latch *subnetReplayLatch
}

func newSubnetProcessProbe() *subnetProcessProbe {
	return &subnetProcessProbe{latch: newSubnetReplayLatch()}
}

func (self *subnetProcessProbe) id() string { return subnetProcessProbeId }

func (self *subnetProcessProbe) tier() string { return tierWarn }

func (self *subnetProcessProbe) cadence() time.Duration {
	return DefaultSubnetReplaySettings().Cadence
}

func (self *subnetProcessProbe) runBudget() time.Duration {
	return DefaultSubnetReplaySettings().RunBudget
}

func (self *subnetProcessProbe) check(ctx context.Context, env *probeEnv) (findings []finding, resultErr error) {
	settings := env.cfg.subnetReplay
	scope := subnetReplayScope{environment: env.cfg.env, checkpointKind: "evm", source: "archive-relay-and-producer-database"}
	coverage := func(frame, reason string, undecided []string) finding {
		return subnetReplayCoverage(subnetProcessProbeId, "subnet-process/coverage", frame, scope, subnetProcessPlaybook, reason, undecided, 2)
	}
	deployment := subnetReplayDeploymentOf(env)
	if deployment.notDeployed {
		return nil, nil
	}
	if deployment.reason != "" {
		return []finding{coverage("deployment", deployment.reason, append(append([]string(nil), subnetProcessClasses...), subnetProcessTelemetryClasses...))}, nil
	}
	if reason := settings.budgetReason(); reason != "" {
		return []finding{coverage("budget", reason, subnetProcessClasses)}, nil
	}
	if reason := settings.enrollmentReason(); reason != "" {
		return []finding{coverage("enrollment", reason, []string{"subnet-writer-funding"})}, nil
	}
	pin := deployment.pin
	gaps := []finding{
		coverage("process-telemetry", "validator-miner-process-exporters-absent", subnetProcessTelemetryClasses),
		coverage("role-liveness", "validator-miner-progress-exporters-absent", []string{"subnet-runtime-liveness"}),
	}
	// An explicitly disabled producer is expected to stop; its writers and
	// durable rows are then not a process incident.
	producerExpected := env.cfg.stConfigStatus.normalized() != STConfigurationExplicitDisabled
	target, rpcPort, reason := selectSubnetReplayEndpoint(env, pin)
	if reason != "" {
		return append(gaps, coverage("source", reason, subnetProcessClasses)), nil
	}
	runCtx, cancel := context.WithTimeout(ctx, settings.RunBudget)
	defer cancel()
	defer func() {
		select {
		case <-ctx.Done():
			findings = nil
			resultErr = &signalContextError{contextErr: ctx.Err(), sourceErr: resultErr}
		default:
		}
	}()
	sourceGap := func(frame string, err error, undecided []string) ([]finding, error) {
		if reason := subnetReplayUnknownReason(err); reason != "" {
			return append(gaps, coverage(frame, reason, undecided)), nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return append(gaps, coverage(frame, "run-budget-exhausted", undecided)), nil
		}
		return nil, err
	}
	rpc := newSubnetReplayRpc(&subnetReplaySshRelay{runner: env.runner, target: target, rpcPort: rpcPort, settings: settings}, settings)
	state, err := readSubnetSettlementState(runCtx, rpc, pin, settings)
	if err != nil {
		return sourceGap("source", err, subnetProcessClasses)
	}
	scope.checkpointNumber = state.checkpoint.number
	scope.completeness = "complete"
	verdicts := []finding{}

	var producer *subnetProcessProducer
	if producerExpected {
		producer, err = readSubnetProcessProducer(runCtx, env, pin, settings)
		if err != nil {
			if reason := subnetReplayUnknownReason(err); reason == "" {
				return nil, err
			} else {
				gaps = append(gaps, coverage("producer", reason, subnetProcessClasses))
				producer = nil
			}
		}
	}

	// Chain-derived writers: the current epoch's active operator signers.
	addresses := []common.Address{}
	type signerRole struct {
		noId uint64
		role string
	}
	roles := map[common.Address][]signerRole{}
	for _, entry := range state.entries {
		if entry.epoch != state.currentEpoch || !entry.operatorKnown || !entry.operator.Active {
			continue
		}
		for _, role := range []struct {
			address common.Address
			name    string
		}{{address: entry.operator.RootSigner, name: "root-signer"}, {address: entry.operator.DepositSigner, name: "deposit-signer"}} {
			addresses = append(addresses, role.address)
			roles[role.address] = append(roles[role.address], signerRole{noId: entry.noId, role: role.name})
		}
	}
	producerWriters := []common.Address{}
	if producer != nil {
		seen := map[common.Address]bool{}
		for _, intent := range producer.intents {
			if !seen[intent.from] {
				seen[intent.from] = true
				producerWriters = append(producerWriters, intent.from)
			}
		}
		sort.Slice(producerWriters, func(i, j int) bool { return producerWriters[i].Hex() < producerWriters[j].Hex() })
		addresses = append(addresses, producerWriters...)
	}
	writers, err := readSubnetProcessWriters(runCtx, rpc, state.checkpoint, addresses)
	if err != nil {
		return sourceGap("writers", err, []string{"subnet-writer-funding"})
	}

	// Mirror liveness and durability against the chain checkpoint.
	var mirrorBlock *subnetReplayRpcBlock
	if producer != nil && producer.mirror.present && producer.mirror.hashKnown && 0 < producer.mirror.nextBlock && producer.mirror.nextBlock-1 <= state.checkpoint.number {
		call := &subnetReplayRpcCall{method: "eth_getBlockByNumber", params: []any{subnetReplayHexBlock(producer.mirror.nextBlock - 1), false}, result: &mirrorBlock}
		if err := rpc.call(runCtx, call); err != nil {
			return sourceGap("source", err, subnetProcessClasses)
		}
		if err := subnetReplayRpcFailed(call); err != nil {
			return sourceGap("source", err, subnetProcessClasses)
		}
	}
	if producer != nil {
		verdicts = append(verdicts, evaluateSubnetProcessProducer(producer, mirrorBlock, writers, state, settings, scope)...)
		if producer.intentsPartial {
			gaps = append(gaps, coverage("producer-intents", "producer-intent-bound", []string{"subnet-writer-funding"}))
		}
		if producer.tasksPartial {
			gaps = append(gaps, coverage("producer-tasks", "producer-task-bound", []string{"subnet-runtime-liveness"}))
		}
	}

	// Funding of every scoped writer.
	liability, enrolled := new(big.Int).SetString(settings.WriterActionLiabilityWei, 10)
	enrolled = enrolled && 0 < settings.WriterMinimumActions && 0 < liability.Sign()
	if !enrolled {
		gaps = append(gaps, coverage("writer-runway", "writer-runway-not-enrolled", []string{"subnet-writer-funding"}))
	}
	funding := func(target, frame string, writer *subnetProcessWriter) {
		balance := (*big.Int)(writer.balance)
		switch {
		case balance.Sign() == 0:
			verdicts = append(verdicts, subnetReplayFinding(subnetProcessProbeId, "subnet-writer-funding", target, frame, scope, subnetProcessPlaybook, subnetReplayIssue{
				tier: tierWarn, sustain: 2, invariant: "scoped-writer-funded", expected: "balance>0", observed: "balance=0",
				symptom:   "A scoped subnet writer has no funds for its due transactions",
				mechanism: "A writer with zero balance cannot pay gas for its next root, deposit, close or finalize transaction, so its due work will miss its deadline.",
				baseline:  "Every scoped signer holds enough native gas for its due work within the approved per-action ceilings.",
				action:    "Fund the writer through the approved treasury path; never auto-fund, trade, restake or resend an ambiguous transaction.",
				verify:    "A finalized checkpoint shows the writer funded and its due transaction lands.",
			}))
		case enrolled:
			actions := new(big.Int).Div(balance, liability)
			if actions.Cmp(new(big.Int).SetUint64(settings.WriterMinimumActions)) < 0 {
				verdicts = append(verdicts, subnetReplayFinding(subnetProcessProbeId, "subnet-writer-funding", target, frame, scope, subnetProcessPlaybook, subnetReplayIssue{
					tier: tierWarn, sustain: 2, invariant: "balance/actionLiability>=minimumActions",
					expected: strconv.FormatUint(settings.WriterMinimumActions, 10) + "-fundable-actions", observed: actions.String() + "-fundable-actions",
					symptom:   "A scoped subnet writer's funded runway is below its enrolled minimum",
					mechanism: "At the approved per-action gas liability the balance funds fewer actions than the enrolled minimum, so a price or burn movement can strand due work.",
					baseline:  "Every scoped signer can fund at least the enrolled number of actions.",
					action:    "Top up the writer through the approved treasury path before its next due action; never auto-fund.",
					verify:    "A finalized checkpoint shows the runway at or above the enrolled minimum.",
				}))
			}
		}
	}
	roleAddresses := make([]common.Address, 0, len(roles))
	for address := range roles {
		roleAddresses = append(roleAddresses, address)
	}
	sort.Slice(roleAddresses, func(i, j int) bool { return roleAddresses[i].Hex() < roleAddresses[j].Hex() })
	for _, address := range roleAddresses {
		for _, role := range roles[address] {
			funding("subnet-process/no-"+strconv.FormatUint(role.noId, 10), role.role, writers[address])
		}
	}
	for ordinal, address := range producerWriters {
		if _, chainRole := roles[address]; chainRole {
			continue
		}
		funding("subnet-process/producer", "intent-writer-"+strconv.Itoa(ordinal+1), writers[address])
	}

	if err := recheckSubnetReplayEvmCheckpoint(runCtx, rpc, state.checkpoint); err != nil {
		return sourceGap("source", err, subnetProcessClasses)
	}
	return self.latch.Retain(env.now(), append(gaps, verdicts...)), nil
}

func subnetReplayHexBlock(number uint64) string {
	return "0x" + strconv.FormatUint(number, 16)
}

// Producer rules: mirror progress and canonical boundary, task liveness, and
// unresolved intents and nonces.
func evaluateSubnetProcessProducer(producer *subnetProcessProducer, mirrorBlock *subnetReplayRpcBlock, writers map[common.Address]*subnetProcessWriter, state *subnetSettlementState, settings *SubnetReplaySettings, scope subnetReplayScope) []finding {
	findings := []finding{}
	liveness := func(frame, invariant, expected, observed, symptom, mechanism, action string) {
		findings = append(findings, subnetReplayFinding(subnetProcessProbeId, "subnet-runtime-liveness", "subnet-process/operator", frame, scope, subnetProcessPlaybook, subnetReplayIssue{
			tier: tierWarn, sustain: 2, invariant: invariant, expected: expected, observed: observed,
			symptom: symptom, mechanism: mechanism,
			baseline: "The operator service advances its finalized mirror, runs its subnet tasks and resolves its chain writes within their deadlines.",
			action:   action,
			verify:   "Two later runs show the progress marker advancing and the named work resolving.",
		}))
	}
	mirror := producer.mirror
	checkpoint := state.checkpoint.number
	switch {
	case !mirror.present:
		liveness("event-mirror", "producer-mirror-exists", "mirror-row", "absent",
			"The operator service has no finalized event mirror for the reviewed deployment",
			"The subnet tasks mirror finalized coordinator events before closing, committing or finalizing epochs; with no mirror for this deployment the producer is not serving it.",
			"Check the operator service's st.yml deployment selection and its StSyncChain task; do not point it at another deployment to clear this.")
	default:
		lag := uint64(0)
		if mirror.nextBlock <= checkpoint {
			lag = checkpoint + 1 - mirror.nextBlock
		}
		if settings.MirrorLagWarnBlocks < lag {
			liveness("event-mirror", "mirror-lag<=MirrorLagWarnBlocks", "lag<="+strconv.FormatUint(settings.MirrorLagWarnBlocks, 10), "lag="+strconv.FormatUint(lag, 10),
				"The operator service's finalized event mirror lags the chain",
				"The mirror drives every epoch task's discovery; a growing lag delays closes, roots and finalization behind the chain's deadlines even while the process reports healthy.",
				"Inspect the StSyncChain task's last runs and its RPC budget; preserve the mirror and never reset its high-water mark to clear the lag.")
		}
		if int64(settings.MirrorStaleAge/time.Second) < mirror.ageSeconds && 0 < lag {
			liveness("event-mirror-age", "mirror-updated-within-MirrorStaleAge", "age<="+strconv.FormatInt(int64(settings.MirrorStaleAge/time.Second), 10)+"s", "age="+strconv.FormatInt(mirror.ageSeconds, 10)+"s",
				"The operator service's finalized event mirror stopped advancing",
				"The mirror's last update is older than its stale bound while the chain moved on: a stalled sync behind a healthy process.",
				"Inspect the StSyncChain task owner and its last error; do not restart shared infrastructure as a fix.")
		}
		if mirrorBlock != nil {
			identity, ok := mirrorBlock.identity()
			if !ok || identity.hash != mirror.blockHash {
				findings = append(findings, subnetReplayFinding(subnetProcessProbeId, "subnet-state-durability", "subnet-process/operator", "event-mirror-boundary", scope, subnetProcessPlaybook, subnetReplayIssue{
					tier: tierPage, sustain: 1, invariant: "mirror-boundary=canonical-block(nextBlock-1)",
					expected: "canonical-hash-at-boundary", observed: "different-hash",
					symptom:   "The operator service's mirror continues from a non-canonical boundary",
					mechanism: "The producer records the canonical hash of the block before its next scan; a different hash at that finalized height means its durable mirror was built on history the chain does not contain.",
					baseline:  "The mirror's stored boundary hash is the canonical finalized hash at its height.",
					action:    "Quarantine the mirror for authorized recovery and stop new writes from it; never delete journals, truncate history or reset its cursor to clear this.",
					verify:    "Authorized recovery rebuilds the mirror from canonical finalized history and the boundary matches at later runs.",
				}))
			}
		}
	}
	sync := false
	failing := []string{}
	for _, task := range producer.tasks {
		if task.function == "StSyncChain" {
			sync = true
			if int64(settings.MirrorStaleAge/time.Second) < task.ageSeconds {
				liveness("sync-task", "sync-task-run-within-MirrorStaleAge", "overdue<="+strconv.FormatInt(int64(settings.MirrorStaleAge/time.Second), 10)+"s", "overdue="+strconv.FormatInt(task.ageSeconds, 10)+"s",
					"The operator service's chain sync task is overdue",
					"The periodic sync task stays pending past its run time, so no worker is advancing the subnet pipeline.",
					"Check the taskworker fleet's claim and admission for subnet tasks; preserve the task row.")
			}
		}
		if settings.TaskRescheduleWarnCount <= task.errorCount {
			failing = append(failing, task.function)
		}
	}
	if !sync {
		liveness("sync-task", "sync-task-scheduled", "pending-sync-task", "absent",
			"The operator service's chain sync task is not scheduled",
			"The periodic sync task reschedules itself; with no pending row the pipeline stopped discovering epochs and deadlines.",
			"Check the taskworker scheduler for the subnet tasks; do not insert task rows by hand.")
	}
	if 0 < len(failing) {
		sort.Strings(failing)
		liveness("task-errors", "subnet-tasks-below-reschedule-error-bound", "errors<"+strconv.FormatInt(settings.TaskRescheduleWarnCount, 10), "failing="+strings.Join(failing, ","),
			"Subnet pipeline tasks are repeatedly failing",
			"The named tasks have been rescheduled with errors at least the warning bound; their epoch work is at risk of missing its deadline.",
			"Read the tasks' bounded error classes and their writer funding; never resend an ambiguous transaction.")
	}
	uncertain, stale, consumed := 0, 0, 0
	for _, intent := range producer.intents {
		writer := writers[intent.from]
		if intent.status == "uncertain" {
			uncertain += 1
			continue
		}
		if int64(settings.IntentUnresolvedAge/time.Second) < intent.ageSeconds {
			stale += 1
			if writer != nil && intent.nonce < uint64(*writer.nonce) {
				consumed += 1
			}
		}
	}
	intentIssue := func(frame, invariant, observed, symptom, mechanism string) {
		findings = append(findings, subnetReplayFinding(subnetProcessProbeId, "subnet-writer-funding", "subnet-process/producer", frame, scope, subnetProcessPlaybook, subnetReplayIssue{
			tier: tierWarn, sustain: 2, invariant: invariant, expected: "0", observed: observed,
			symptom: symptom, mechanism: mechanism,
			baseline: "Every chain-write intent resolves to finalized or failed with its nonce reconciled.",
			action:   "Reconcile the intents through the producer's own reconciler; never resend, cancel or replace an ambiguous transaction by hand.",
			verify:   "A later run shows no unresolved intents for the deployment.",
		}))
	}
	if 0 < uncertain {
		intentIssue("uncertain-intents", "no-uncertain-intents", strconv.Itoa(uncertain),
			"Chain-write intents are in an uncertain state",
			"An uncertain intent may or may not have reached the chain; until reconciled, its nonce and effect are unknown and later writes from the same signer can stall.")
	}
	if 0 < stale {
		intentIssue("stale-intents", "intents-resolve-within-IntentUnresolvedAge", strconv.Itoa(stale),
			"Chain-write intents remain unresolved past their age bound",
			"Prepared, signed, broadcast or mined intents older than the unresolved bound are not reaching finality.")
	}
	if 0 < consumed {
		intentIssue("consumed-nonces", "unresolved-intent-nonce>=finalized-account-nonce", strconv.Itoa(consumed),
			"Unresolved intents hold nonces the chain already consumed",
			"The signer's finalized nonce passed these intents' nonces, so another transaction used them; the intents can never land as prepared and must be reconciled.")
	}
	return findings
}
