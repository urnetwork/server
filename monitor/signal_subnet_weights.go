// SIGNALS.md §22.6 maps to signal_subnet_weights.go and
// signal_subnet_weights_test.go. The family reader checks the native weight
// lifecycle at one finalized native block of the reviewed chain: each
// enrolled validator's applied row against its own policy masks, recipients
// and signed cap; pending timelocked commits against their reveal schedule
// and the chain's stored drand round; missed weight cycles; validator trust;
// and dividends across a full tempo. Every permitted validator is read from
// the chain census, never from a producer list. Replaying a validator's
// signed decision and the Yuma bond and clipping arithmetic needs records and
// a replay engine that do not exist; both stay explicit coverage gaps.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"
)

const subnetWeightsProbeId = "subnet/weights"
const subnetWeightsPlaybook = "SIGNALS.md §22.6"

var subnetWeightsClasses = []string{"subnet-weight-vector", "subnet-crv4-lifecycle", "subnet-consensus-divergence", "subnet-validator-native-payout"}

func NewSubnetWeightsSignal() Signal {
	return &signalAdapter{
		number: "22.6",
		key:    "subnet-weights",
		name:   "Subnet native weight lifecycle, consensus and validator dividends",
		probe:  newSubnetWeightsProbe(),
	}
}

// The first native block at which an eligible validator was seen with zero
// dividends. Safe for concurrent use.
type subnetWeightDividends struct {
	stateLock sync.Mutex
	firstZero map[[32]byte]uint64
}

// Records an observation; returns the first zero block while zero persists.
func (self *subnetWeightDividends) Observe(hotkey [32]byte, block uint64, zero bool) (uint64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !zero {
		delete(self.firstZero, hotkey)
		return 0, false
	}
	first, ok := self.firstZero[hotkey]
	if !ok || block < first {
		self.firstZero[hotkey] = block
		return block, true
	}
	return first, true
}

type subnetWeightsProbe struct {
	latch     *subnetReplayLatch
	runtimes  *subnetWeightRuntimes
	dividends *subnetWeightDividends
}

func newSubnetWeightsProbe() *subnetWeightsProbe {
	return &subnetWeightsProbe{latch: newSubnetReplayLatch(), runtimes: newSubnetWeightRuntimes(), dividends: &subnetWeightDividends{firstZero: map[[32]byte]uint64{}}}
}

func (self *subnetWeightsProbe) id() string { return subnetWeightsProbeId }

func (self *subnetWeightsProbe) tier() string { return tierPage }

func (self *subnetWeightsProbe) cadence() time.Duration {
	return DefaultSubnetReplaySettings().Cadence
}

func (self *subnetWeightsProbe) runBudget() time.Duration {
	return DefaultSubnetReplaySettings().RunBudget
}

func (self *subnetWeightsProbe) check(ctx context.Context, env *probeEnv) (findings []finding, resultErr error) {
	settings := env.cfg.subnetReplay
	scope := subnetReplayScope{environment: env.cfg.env, checkpointKind: "native", source: "archive-relay-with-verified-runtime-metadata"}
	coverage := func(frame, reason string, undecided []string) finding {
		return subnetReplayCoverage(subnetWeightsProbeId, "subnet-weights/coverage", frame, scope, subnetWeightsPlaybook, reason, undecided, 2)
	}
	deployment := subnetReplayDeploymentOf(env)
	if deployment.notDeployed {
		return nil, nil
	}
	if deployment.reason != "" {
		return []finding{coverage("deployment", deployment.reason, subnetWeightsClasses)}, nil
	}
	if reason := settings.budgetReason(); reason != "" {
		return []finding{coverage("budget", reason, subnetWeightsClasses)}, nil
	}
	if reason := settings.enrollmentReason(); reason != "" {
		return []finding{coverage("enrollment", reason, subnetWeightsClasses)}, nil
	}
	pin := deployment.pin
	gaps := []finding{
		coverage("decision-replay", "validator-decision-records-unpublished", []string{"subnet-weight-vector"}),
		coverage("yuma-replay", "bond-and-clipping-replay-unimplemented", []string{"subnet-consensus-divergence", "subnet-validator-native-payout"}),
	}
	enrolledHotkeys := make([][32]byte, 0, len(settings.Validators))
	for _, validator := range settings.Validators {
		hotkey, err := subnetReplayAccountId(validator)
		if err != nil {
			return append(gaps, coverage("enrollment", "enrollment-invalid", subnetWeightsClasses)), nil
		}
		enrolledHotkeys = append(enrolledHotkeys, hotkey)
	}
	if len(enrolledHotkeys) == 0 {
		gaps = append(gaps, coverage("validators", "validators-not-enrolled", subnetWeightsClasses))
	}
	if settings.MaximumWeightLimitU16 == 0 {
		gaps = append(gaps, coverage("weight-cap", "weight-cap-not-enrolled", []string{"subnet-weight-vector"}))
	}
	target, rpcPort, reason := selectSubnetReplayEndpoint(env, pin)
	if reason != "" {
		return append(gaps, coverage("source", reason, subnetWeightsClasses)), nil
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
	sourceGap := func(err error) ([]finding, error) {
		if reason := subnetReplayUnknownReason(err); reason != "" {
			return append(gaps, coverage("source", reason, subnetWeightsClasses)), nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return append(gaps, coverage("source", "run-budget-exhausted", subnetWeightsClasses)), nil
		}
		return nil, err
	}
	rpc := newSubnetReplayRpc(&subnetReplaySshRelay{runner: env.runner, target: target, rpcPort: rpcPort, settings: settings}, settings)
	state, err := readSubnetWeightState(runCtx, rpc, self.runtimes, pin, enrolledHotkeys, settings)
	if err != nil {
		return sourceGap(err)
	}
	scope.checkpointNumber = state.checkpoint.number
	scope.completeness = "complete"
	if !state.validatorsComplete {
		scope.completeness = "partial:validator-rows-bound"
		gaps = append(gaps, coverage("validator-rows", "validator-rows-bound", subnetWeightsClasses))
	}
	if !state.commitsComplete {
		gaps = append(gaps, coverage("pending-commits", "commit-keys-bound", []string{"subnet-crv4-lifecycle"}))
	}
	verdicts := evaluateSubnetWeights(state, self.dividends, settings, scope)
	if err := recheckSubnetWeightCheckpoint(runCtx, rpc, state.checkpoint); err != nil {
		return sourceGap(err)
	}
	if err := recheckSubnetReplayEvmCheckpoint(runCtx, rpc, state.evmCheckpoint); err != nil {
		return sourceGap(err)
	}
	return self.latch.Retain(env.now(), append(gaps, verdicts...)), nil
}

// The weight reducers. Policy masks, recipients and the cap apply to enrolled
// validators, whose reviewed policy is known; lifecycle timing applies to
// every pending commit of a permitted validator.
func evaluateSubnetWeights(state *subnetWeightState, dividends *subnetWeightDividends, settings *SubnetReplaySettings, scope subnetReplayScope) []finding {
	findings := []finding{}
	block := state.checkpoint.number
	cycle := uint64(state.tempo) + 1
	byHotkey := map[[32]byte]*subnetWeightValidator{}
	for _, validator := range state.validators {
		byHotkey[validator.hotkey] = validator
	}
	pendingByAccount := map[[32]byte][]subnetWeightCommit{}
	for _, commit := range state.commits {
		pendingByAccount[commit.Account] = append(pendingByAccount[commit.Account], commit)
	}
	enrolledTarget := func(ordinal int) string { return "subnet-weights/validator-" + strconv.Itoa(ordinal) }
	vector := func(target, frame, invariant, expected, observed, symptom, mechanism string) {
		findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-weight-vector", target, frame, scope, subnetWeightsPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: invariant, expected: expected, observed: observed,
			symptom: symptom, mechanism: mechanism,
			baseline: "Each enrolled validator's applied row honors its self and escrow masks, names only registered recipients and stays inside the signed policy cap.",
			action:   "Preserve the finalized row and the validator's measurement and intent records privately; route to the validator owner. Explain every component before normalizing, and never submit corrective weights from the monitor.",
			verify:   "Independent replay of the validator's signed decision attributes the row and a recorded disposition closes it.",
		}))
	}
	enrolledByHotkey := map[[32]byte]subnetWeightEnrolled{}
	for _, enrolled := range state.enrolled {
		enrolledByHotkey[enrolled.hotkey] = enrolled
		target := enrolledTarget(enrolled.ordinal)
		validator := byHotkey[enrolled.hotkey]
		if validator == nil || !enrolled.registered || validator.uid != enrolled.uid {
			findings = append(findings, subnetReplayUndecided(subnetWeightsProbeId, "subnet-crv4-lifecycle", target, "permit", scope, subnetWeightsPlaybook, "validator-not-permitted-or-unregistered", 2))
			continue
		}
		seen := map[uint16]bool{}
		maximum, sum := new(big.Int), new(big.Int)
		for _, pair := range validator.row {
			uid, value := uint16(pair.Uid), uint16(pair.Value)
			switch {
			case seen[uid]:
				vector(target, "duplicate-recipient", "one-entry-per-recipient", "unique-uids", "duplicate-uid", "An enrolled validator's applied row repeats a recipient", "The runtime stores one value per recipient; a repeated recipient is a malformed applied row.")
			case state.subnetworkN <= uid:
				vector(target, "unregistered-recipient", "recipient-uid<subnetworkN", strconv.Itoa(int(state.subnetworkN)), strconv.Itoa(int(uid)), "An enrolled validator weights an unregistered UID", "Every recipient must be a registered UID of the subnet at the applied block.")
			case uid == validator.uid && 0 < value:
				vector(target, "self-weight", "self-mask", "0", strconv.Itoa(int(value)), "An enrolled validator's applied row weights its own UID", "The reviewed policy masks the validator's own UID; a positive self weight diverts emission to the validator itself.")
			case state.escrowRegistered && uid == state.escrowUid && 0 < value:
				vector(target, "escrow-recipient", "escrow-mask", "0", strconv.Itoa(int(value)), "An enrolled validator weights the claims escrow UID", "The claims escrow is not a pool or head recipient; weight on it moves emission into escrow without pool attribution.")
			}
			seen[uid] = true
			if maximum.Cmp(big.NewInt(int64(value))) < 0 {
				maximum.SetInt64(int64(value))
			}
			sum.Add(sum, big.NewInt(int64(value)))
		}
		if 0 < settings.MaximumWeightLimitU16 && 0 < sum.Sign() {
			left := new(big.Int).Mul(maximum, big.NewInt(65535))
			right := new(big.Int).Mul(sum, big.NewInt(int64(settings.MaximumWeightLimitU16)))
			if 0 < left.Cmp(right) {
				vector(target, "cap-breach", "max*65535<=sum*cap", right.String(), left.String(), "An enrolled validator's applied row exceeds the signed weight cap", "The signed steering policy caps each recipient's normalized share; the stored row's largest share exceeds it. The runtime's reported cap is not the authority.")
			}
		}

		// One durable intent per native epoch: two pending commits in one tempo
		// window are a duplicate effect.
		windows := map[uint64]int{}
		for _, commit := range pendingByAccount[validator.hotkey] {
			windows[uint64(commit.CommitBlock)/cycle] += 1
		}
		for window, count := range windows {
			if 1 < count {
				findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-crv4-lifecycle", target, "duplicate-intent", scope, subnetWeightsPlaybook, subnetReplayIssue{
					tier: tierPage, sustain: 1, invariant: "one-pending-commit-per-tempo-window",
					expected: "1", observed: strconv.Itoa(count), deadline: "tempo_window=" + strconv.FormatUint(window, 10),
					symptom:   "An enrolled validator has more than one pending commit in one tempo",
					mechanism: "The validator owns one durable intent per native epoch and reconciles an uncertain submission before replacing it; two pending commits are a duplicate effect.",
					baseline:  "Each native epoch carries at most one pending commit per validator.",
					action:    "Retain both extrinsics and dispatch results and reconcile the validator's intent history; never resend from the monitor.",
					verify:    "Later finalized blocks show a single applied row per epoch.",
				}))
				break
			}
		}

		// A missed required cycle: no recent applied row and no pending commit.
		if validator.lastUpdate+settings.WeightStaleTempos*cycle < block && len(pendingByAccount[validator.hotkey]) == 0 {
			findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-crv4-lifecycle", target, "cycle", scope, subnetWeightsPlaybook, subnetReplayIssue{
				tier: tierPage, sustain: 1, invariant: "weights-applied-within-WeightStaleTempos",
				expected: "last-update>=" + strconv.FormatUint(block-settings.WeightStaleTempos*cycle, 10), observed: "last-update=" + strconv.FormatUint(validator.lastUpdate, 10),
				deadline:  fmt.Sprintf("tempo_blocks=%d stale_tempos=%d finalized_block=%d", cycle, settings.WeightStaleTempos, block),
				symptom:   "An enrolled validator missed its required weight cycles",
				mechanism: "No applied row and no pending commit for several tempos: the validator's durable intent did not reach the chain, so its pools and heads lose weight.",
				baseline:  "One durable intent per native epoch reaches commit, reveal and application.",
				action:    "Reconcile the validator's pending intent through its own reconciler before any replacement; never resend an ambiguous extrinsic.",
				verify:    "A later finalized block shows a fresh applied row for the validator.",
			}))
		}

		// Trust and dividends of an active enrolled validator.
		active := block <= validator.lastUpdate+settings.WeightStaleTempos*cycle
		if active && validator.trust < settings.ValidatorTrustCollapse {
			findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-consensus-divergence", target, "validator-trust", scope, subnetWeightsPlaybook, subnetReplayIssue{
				tier: tierWarn, sustain: 2, invariant: "validator-trust>=collapse-threshold",
				expected: ">=" + strconv.Itoa(int(settings.ValidatorTrustCollapse)), observed: strconv.Itoa(int(validator.trust)),
				symptom:   "An active enrolled validator's trust collapsed",
				mechanism: "Validator trust reflects agreement with stake-weighted consensus; a collapse while weights apply means its rows diverge from the majority, which can be valid independent measurement or a defect.",
				baseline:  "Each active validator's trust stays above the collapse threshold while it applies weights.",
				action:    "Compare the validator's declared, revealed and applied rows with its measurement evidence; identical rows do not prove copying and divergence is not slashing.",
				verify:    "Trust recovers above the threshold over later tempos, or the divergence is attributed.",
			}))
		}
		if active && settings.ValidatorTrustCollapse <= validator.trust {
			first, zero := dividends.Observe(validator.hotkey, block, validator.dividends == 0)
			if zero && cycle <= block-first {
				findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-validator-native-payout", target, "dividends", scope, subnetWeightsPlaybook, subnetReplayIssue{
					tier: tierWarn, sustain: 2, invariant: "eligible-validator-earns-dividends-within-a-tempo",
					expected: "dividends>0", observed: "dividends=0-since-block-" + strconv.FormatUint(first, 10),
					symptom:   "An eligible enrolled validator earned no dividends for a full tempo",
					mechanism: "A permitted, active validator with healthy trust should receive dividends after its mature cycles; zero across a full tempo is a payout gap, not proof of misattribution.",
					baseline:  "Each eligible validator's native dividends are positive after every mature cycle.",
					action:    "Replay the pinned native algorithm for the epoch before alerting the validator owner; separate delegation, bonds and clipping from attribution.",
					verify:    "A later finalized block shows positive dividends.",
				}))
			}
		} else {
			dividends.Observe(validator.hotkey, block, false)
		}
	}

	// Pending commits: reveal schedule against the stored drand round.
	for _, commit := range state.commits {
		validator := byHotkey[commit.Account]
		if validator == nil || block < uint64(commit.CommitBlock) {
			continue
		}
		due := uint64(commit.CommitBlock) + (state.revealPeriodEpochs+settings.RevealOverdueEpochs)*cycle
		if block <= due {
			continue
		}
		target, tier, sustain := "subnet-weights/census", tierWarn, 2
		frame := "uid-" + strconv.Itoa(int(validator.uid)) + "/reveal"
		if enrolled, ok := enrolledByHotkey[commit.Account]; ok {
			target, frame = enrolledTarget(enrolled.ordinal), "reveal"
			tier, sustain = tierPage, 1
		}
		if state.lastStoredRound < uint64(commit.RevealRound) {
			findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-crv4-lifecycle", target, frame+"-drand", scope, subnetWeightsPlaybook, subnetReplayIssue{
				tier: tierWarn, sustain: 2, invariant: "drand-round-stored-before-reveal",
				expected: "stored-round>=" + strconv.FormatUint(uint64(commit.RevealRound), 10), observed: "stored-round=" + strconv.FormatUint(state.lastStoredRound, 10),
				symptom:   "A due weight reveal is waiting for its drand round",
				mechanism: "A timelocked commit reveals only once the chain stores its drand round; ingestion lag delays every validator's application.",
				baseline:  "The chain stores each drand round before the commits that need it are due.",
				action:    "Treat this as a drand ingestion dependency, not a validator fault; follow the chain's drand ingestion.",
				verify:    "The stored round passes the commit's round and the commit is applied.",
			}))
			continue
		}
		findings = append(findings, subnetReplayFinding(subnetWeightsProbeId, "subnet-crv4-lifecycle", target, frame, scope, subnetWeightsPlaybook, subnetReplayIssue{
			tier: tier, sustain: sustain, invariant: "revealable-commit-applied-within-RevealOverdueEpochs",
			expected: "commit-applied-by-block-" + strconv.FormatUint(due, 10), observed: "commit-still-pending",
			deadline:  fmt.Sprintf("due_block=%d finalized_block=%d", due, block),
			symptom:   "A revealable timelocked weight commit was not applied",
			mechanism: "The chain stores the commit's drand round and its reveal epoch passed, but the commit is still pending: the revealed row was not applied.",
			baseline:  "Every commit is revealed and applied in its reveal epoch once its drand round is stored.",
			action:    "Retain the commit's extrinsic and dispatch result; reconcile the validator's intent before any replacement, never resend an ambiguous extrinsic.",
			verify:    "A later finalized block shows the commit applied and the validator's row updated.",
		}))
	}
	return findings
}
