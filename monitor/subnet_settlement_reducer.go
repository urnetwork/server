// §22.7 reducers over one finalized settlement state census. Every rule is
// exact integer arithmetic over the vault's own counters, its live escrow
// stake and the immutable per-entitlement state; nothing is smoothed. With a
// complete census the counters are reconstructed independently:
//
//	totalCaptured        = Σ funded
//	pendingFunding       = Σ funded over funded entitlements
//	outstandingLiability = Σ funded over finalized, root-missed and carried − totalPaid
//	carry(no)            = Σ root-missed funded + Σ carried (total − claimed)
//	                       − Σ finalized and carried (total − funded), per operator
//	implied claim credit = Σ claimed over finalized and carried − totalPaid >= 0
//
// Aggregate contract counters are public chain state and are rendered as
// exact integers; claimant-level values and identities are never rendered.
package monitor

import (
	"fmt"
	"math/big"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
)

const subnetSettlementProbeId = "subnet/settlement"
const subnetSettlementPlaybook = "SIGNALS.md §22.7"

const subnetSettlementConservationAction = "Stop reporting settlement as healthy. Replay the vault's finalized logs from deployment to find the first divergent receipt, preserve the entire affected liability set and route to custody incident response. Never fund an escrow discrepancy from the reserve, correct state or restart a writer to clear this alert."
const subnetSettlementConservationVerify = "Independent replay at this checkpoint attributes the divergence and a recorded disposition closes it; every later finalized checkpoint satisfies the identity."

func subnetSettlementOperatorTarget(noId uint64) string {
	return "subnet-settlement/no-" + strconv.FormatUint(noId, 10)
}

func subnetSettlementEpochFrame(epoch uint64, condition string) string {
	return "epoch-" + strconv.FormatUint(epoch, 10) + "/" + condition
}

// Evaluates every state-only invariant. Census sums run only when the census
// is complete; per-epoch rules run for every row that was read.
func evaluateSubnetSettlementState(state *subnetSettlementState, settings *SubnetReplaySettings, scope subnetReplayScope) []finding {
	findings := []finding{}
	vault := state.vault
	conservation := func(frame, invariant string, expected, observed *big.Int) {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-vault-conservation", "subnet-settlement/vault", frame, scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: invariant,
			expected: expected.String(), observed: observed.String(), delta: subnetReplayDelta(expected, observed),
			symptom:   "A settlement vault conservation identity fails at a finalized checkpoint",
			mechanism: "The vault's exact counters, live escrow stake or complete entitlement census contradict WHITEPAPER §6.1. A deficit means escrow cannot cover accounted obligations, or a captured or paid amount was recorded without the matching state.",
			baseline:  "totalCaptured = totalPaid + escrowAccounted; escrowAccounted = pendingFunding + outstandingLiability; liveEscrowStake >= escrowAccounted; every counter equals its sum over the complete entitlement census.",
			action:    subnetSettlementConservationAction,
			verify:    subnetSettlementConservationVerify,
		}))
	}
	paidPlusAccounted := new(big.Int).Add(vault.totalPaid, vault.escrowAccounted)
	identityPaid := vault.totalCaptured.Cmp(paidPlusAccounted) == 0
	if !identityPaid {
		conservation("captured-paid-accounted", "totalCaptured=totalPaid+escrowAccounted", paidPlusAccounted, vault.totalCaptured)
	}
	pendingPlusOutstanding := new(big.Int).Add(vault.pendingFunding, vault.outstandingLiability)
	identityAccounted := vault.escrowAccounted.Cmp(pendingPlusOutstanding) == 0
	if !identityAccounted {
		conservation("accounted-pending-outstanding", "escrowAccounted=pendingFunding+outstandingLiability", pendingPlusOutstanding, vault.escrowAccounted)
	}
	identityBacking := vault.liveEscrowStake.Cmp(vault.escrowAccounted) >= 0
	if !identityBacking {
		conservation("escrow-backing", "liveEscrowStake>=escrowAccounted", vault.escrowAccounted, vault.liveEscrowStake)
	}
	if vault.conservationHolds != (identityPaid && identityAccounted && identityBacking) {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-vault-conservation", "subnet-settlement/vault", "getter-recomputation", scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: "conservationHolds()=recomputed-identities",
			expected: strconv.FormatBool(identityPaid && identityAccounted && identityBacking), observed: strconv.FormatBool(vault.conservationHolds),
			symptom:   "The vault's conservation getter disagrees with its own counters at one checkpoint",
			mechanism: "conservationHolds() evaluates the same counters read here at the same block hash. Disagreement means the reads are not one consistent state or the deployed code is not the reviewed vault.",
			baseline:  "conservationHolds() equals the independently recomputed identities at one finalized block hash.",
			action:    "Treat settlement as unknown and route to deployment governance review (§22.1). Recheck the vault code identity and both reads at the same finalized hash; do not trust either value alone.",
			verify:    "The getter and the recomputation agree at a later finalized checkpoint with the reviewed vault code.",
		}))
	}

	findings = append(findings, evaluateSubnetSettlementEntries(state, settings, scope)...)
	if !state.censusComplete {
		return findings
	}

	// Whole-history reconstruction from the complete entitlement census.
	capturedSum := new(big.Int)
	pendingSum := new(big.Int)
	settledFunded := new(big.Int)
	claimedSum := new(big.Int)
	carryByOperator := map[uint64]*big.Int{}
	for _, operator := range state.operators {
		carryByOperator[operator.noId] = new(big.Int)
	}
	for _, entry := range state.entries {
		value := entry.entitlement
		capturedSum.Add(capturedSum, value.Funded)
		carry := carryByOperator[entry.noId]
		switch value.Status {
		case subnetSettlementFunded:
			pendingSum.Add(pendingSum, value.Funded)
		case subnetSettlementFinalized:
			settledFunded.Add(settledFunded, value.Funded)
			claimedSum.Add(claimedSum, value.Claimed)
			carry.Sub(carry, new(big.Int).Sub(value.Total, value.Funded))
		case subnetSettlementRootMissed:
			settledFunded.Add(settledFunded, value.Funded)
			carry.Add(carry, value.Funded)
		case subnetSettlementCarried:
			settledFunded.Add(settledFunded, value.Funded)
			claimedSum.Add(claimedSum, value.Claimed)
			carry.Sub(carry, new(big.Int).Sub(value.Total, value.Funded))
			carry.Add(carry, new(big.Int).Sub(value.Total, value.Claimed))
		}
	}
	if vault.totalCaptured.Cmp(capturedSum) != 0 {
		conservation("captured-census", "totalCaptured=sum(funded)", capturedSum, vault.totalCaptured)
	}
	if vault.pendingFunding.Cmp(pendingSum) != 0 {
		conservation("pending-census", "pendingFunding=sum(funded where status=funded)", pendingSum, vault.pendingFunding)
	}
	outstandingExpected := new(big.Int).Sub(settledFunded, vault.totalPaid)
	if vault.outstandingLiability.Cmp(outstandingExpected) != 0 {
		conservation("outstanding-census", "outstandingLiability=sum(funded where status in finalized,root-missed,carried)-totalPaid", outstandingExpected, vault.outstandingLiability)
	}
	impliedCredit := new(big.Int).Sub(claimedSum, vault.totalPaid)
	if impliedCredit.Sign() < 0 {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-credit-payment", "subnet-settlement/vault", "paid-exceeds-claimed", scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: "totalPaid<=sum(claimed)",
			expected: claimedSum.String(), observed: vault.totalPaid.String(), delta: subnetReplayDelta(claimedSum, vault.totalPaid),
			symptom:   "The vault paid more than every claim ever credited",
			mechanism: "Payment moves only accumulated claim credit, and credit arises only from accepted claims. Paid above claimed means a double payment or a payment reported without credit.",
			baseline:  "totalPaid never exceeds the sum of claimed amounts over finalized and carried entitlements.",
			action:    subnetSettlementConservationAction,
			verify:    subnetSettlementConservationVerify,
		}))
	}
	for _, operator := range state.operators {
		expected := carryByOperator[operator.noId]
		if operator.carry.Cmp(expected) == 0 {
			continue
		}
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-carry-isolation", subnetSettlementOperatorTarget(operator.noId), "carry-census", scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: "carry(no)=root-missed+expired-remainder-consumed",
			expected: expected.String(), observed: operator.carry.String(), delta: subnetReplayDelta(expected, operator.carry),
			symptom:   "An operator's vault carry differs from its own entitlement history",
			mechanism: "Missed-root funding and expired unclaimed remainder become only the same operator's carry and are consumed once by its next finalized root. Any other value is cross-operator movement, double carry or lost credit.",
			baseline:  "carry(no) equals that operator's root-missed funding plus expired remainder minus the carry its finalized roots consumed.",
			action:    "Reconcile both adjacent epochs of this operator from finalized vault events before closing; preserve every affected entitlement and never reopen an old one.",
			verify:    "Independent event replay attributes the difference and a recorded disposition closes it; the carry identity holds at later checkpoints.",
		}))
	}
	return findings
}

// Per-row shape, clock and root rules. A row of an operator that was not yet
// registered at its epoch must be empty.
func evaluateSubnetSettlementEntries(state *subnetSettlementState, settings *SubnetReplaySettings, scope subnetReplayScope) []finding {
	findings := []finding{}
	block := state.checkpoint.number
	for _, entry := range state.entries {
		value := entry.entitlement
		target := subnetSettlementOperatorTarget(entry.noId)
		rootPresent := entry.root.PayoutRoot != [32]byte{}
		shape := func(rule string) {
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-vault-conservation", target, subnetSettlementEpochFrame(entry.epoch, "entitlement-shape"), scope, subnetSettlementPlaybook, subnetReplayIssue{
				tier: tierPage, sustain: 1, invariant: rule,
				expected: "state-consistent-with-status", observed: "status=" + subnetSettlementStatusName(value.Status),
				symptom:   "An entitlement's amounts contradict its own status",
				mechanism: "Each vault status admits only specific amounts: unset holds nothing, funded holds captured stake without a total, root-missed carries exactly its funding, and finalized or carried totals cover their funding and claims.",
				baseline:  "Every entitlement's funded, total, claimed and root fields match its status at the finalized checkpoint.",
				action:    subnetSettlementConservationAction,
				verify:    subnetSettlementConservationVerify,
			}))
		}
		switch value.Status {
		case subnetSettlementUnset:
			if value.Funded.Sign() != 0 || value.Total.Sign() != 0 || value.Claimed.Sign() != 0 || value.PayoutRoot != [32]byte{} || value.ArtifactHash != [32]byte{} || value.ExpiryBlock != 0 {
				shape("unset-entitlement-is-empty")
			}
		case subnetSettlementFunded:
			if value.Total.Sign() != 0 || value.Claimed.Sign() != 0 || value.PayoutRoot != [32]byte{} || value.ExpiryBlock != 0 {
				shape("funded-entitlement-has-no-total")
			}
		case subnetSettlementRootMissed:
			if value.Total.Cmp(value.Funded) != 0 || value.Claimed.Sign() != 0 || value.PayoutRoot != [32]byte{} {
				shape("root-missed-total-equals-funded")
			}
		case subnetSettlementFinalized, subnetSettlementCarried:
			if value.PayoutRoot == [32]byte{} || value.ArtifactHash == [32]byte{} || value.Total.Cmp(value.Funded) < 0 {
				shape("finalized-total-covers-funded")
			}
			if value.Total.Cmp(value.Claimed) < 0 {
				findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-correctness", target, subnetSettlementEpochFrame(entry.epoch, "claimed-exceeds-total"), scope, subnetSettlementPlaybook, subnetReplayIssue{
					tier: tierPage, sustain: 1, invariant: "claimed<=total",
					expected: value.Total.String(), observed: value.Claimed.String(), delta: subnetReplayDelta(value.Total, value.Claimed),
					symptom:   "Claims credited more than an entitlement's finalized total",
					mechanism: "A valid proof creates exactly its leaf's integer share of the entitlement once; the shares of one root sum to at most 10,000 bps of its total. Claimed above total is excess credit.",
					baseline:  "claimed(epoch,no) <= poolTotal(epoch,no) for every finalized or carried entitlement.",
					action:    "Investigate this epoch's finalized claim receipts by coordinates without auto-submitting or reversing claims; preserve the root, artifact and every receipt.",
					verify:    "Independent replay attributes the excess and a recorded disposition closes it.",
				}))
			}
		}
		if !entry.operatorKnown {
			if value.Status != subnetSettlementUnset || rootPresent {
				shape("unregistered-operator-has-no-settlement")
			}
			continue
		}
		findings = append(findings, evaluateSubnetSettlementProgress(entry, block, settings, scope)...)
		findings = append(findings, evaluateSubnetSettlementRoot(entry, block, scope)...)
	}
	return findings
}

// Clock rules in evm blocks from the effective policy of the epoch itself.
func evaluateSubnetSettlementProgress(entry *subnetSettlementEntry, block uint64, settings *SubnetReplaySettings, scope subnetReplayScope) []finding {
	findings := []finding{}
	value := entry.entitlement
	clock := entry.clock
	policy := clock.policy
	end := clock.endBlock
	target := subnetSettlementOperatorTarget(entry.noId)
	rootPresent := entry.root.PayoutRoot != [32]byte{}
	settled := value.Status == subnetSettlementFinalized || value.Status == subnetSettlementRootMissed || value.Status == subnetSettlementCarried
	progress := func(condition, invariant string, tier string, sustain int, deadline uint64, symptom, mechanism, action string) {
		remaining := "0"
		if block < deadline {
			remaining = strconv.FormatUint(deadline-block, 10)
		}
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-epoch-progress", target, subnetSettlementEpochFrame(entry.epoch, condition), scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tier, sustain: sustain, invariant: invariant,
			expected: "status-and-root-within-policy-window", observed: fmt.Sprintf("status=%s root_present=%t", subnetSettlementStatusName(value.Status), rootPresent),
			deadline:  fmt.Sprintf("deadline_block=%d finalized_block=%d remaining_blocks=%s", deadline, block, remaining),
			symptom:   symptom,
			mechanism: mechanism,
			baseline:  "Each due operator closes within close grace, commits its root within the root window and finalizes no earlier than the finalize offset, independently of any other operator.",
			action:    action,
			verify:    "The operator's entitlement reaches its required status at a finalized checkpoint and the next epoch progresses within its windows.",
		}))
	}
	if block < end {
		if value.Status != subnetSettlementUnset || rootPresent {
			progress("early-close", "no-settlement-before-epoch-end", tierPage, 1, end,
				"Settlement state exists for an epoch that has not ended",
				"The coordinator admits a close or root only at or after the epoch's exclusive end block; earlier state contradicts the contract's own clock.",
				"Treat settlement for this operator as unknown, preserve the finalized state and route to deployment governance review (§22.1).")
		}
		return findings
	}
	closeDeadline := end + policy.CloseGraceBlocks
	rootDeadline := end + policy.RootCommitWindowBlocks
	finalizeAt := end + policy.FinalizeOffsetBlocks
	if value.Status == subnetSettlementUnset && block <= closeDeadline && closeDeadline-block <= settings.CloseAtRiskBlocks {
		progress("close", "close-within-grace", tierWarn, 2, closeDeadline,
			"An operator's epoch close is at risk of missing its grace window",
			"The close captures the pool stake into escrow only inside the close grace window; after it the epoch records zero and the stake waits for the next timely boundary.",
			"Check the operator's close task and chain writer for this epoch without resubmitting an ambiguous transaction.")
	}
	if value.Status == subnetSettlementUnset && closeDeadline < block && block < finalizeAt {
		progress("close", "close-within-grace", tierWarn, 2, closeDeadline,
			"An operator missed its epoch close and the zero record is not yet written",
			"A missed close must record zero through the permissionless deferral and leave the stake for the next timely boundary; it must never assign a multi-epoch delta to the first missed epoch.",
			"Confirm the deferral is scheduled and that the next close is timely; do not attempt a late capture.")
	}
	if !rootPresent && !settled && block <= rootDeadline && rootDeadline-block <= settings.RootAtRiskBlocks {
		progress("root", "root-within-window", tierWarn, 2, rootDeadline,
			"An operator's payout root is at risk of missing its commit window",
			"The coordinator accepts the operator's root only until the end of the root window; afterwards the epoch finalizes as root-missed and its funding carries forward.",
			"Check the operator's payout computation, artifact publication and root writer for this epoch; do not resubmit an ambiguous transaction.")
	}
	if !rootPresent && rootDeadline < block && block < end+policy.EpochBlocks && value.Status != subnetSettlementCarried {
		progress("root", "root-within-window", tierWarn, 2, rootDeadline,
			"An operator missed its payout root window",
			"Without a root the epoch finalizes as root-missed: its funding carries to the same operator's next root, and providers of this epoch receive no entitlement from it.",
			"Record the missed publication and repair the operator's root path before the next epoch's window; never backdate or invent a root.")
	}
	if settled && block < finalizeAt {
		progress("finalization", "finalize-not-before-offset", tierPage, 1, finalizeAt,
			"An epoch was finalized before its finalize offset",
			"The coordinator finalizes only at or after the epoch end plus the finalize offset; an earlier finalized state contradicts the reviewed policy.",
			"Preserve the finalized state, treat this operator's settlement as unknown and route to deployment governance review (§22.1).")
	}
	if !settled && finalizeAt <= block {
		tier, sustain := tierWarn, 2
		symptom := "An epoch is due for finalization"
		if finalizeAt+settings.FinalizeOverdueBlocks <= block {
			tier, sustain = tierPage, 1
			symptom = "A required epoch finalization is overdue"
		}
		progress("finalization", "finalize-after-offset", tier, sustain, finalizeAt+settings.FinalizeOverdueBlocks, symptom,
			"Finalization is permissionless once the finalize offset passes; an epoch left unfinalized blocks its entitlement, claims and carry for this operator only.",
			"Check the finalize task and its writer funding; finalize through the normal reviewed path. Other operators continue independently.")
	}
	if value.Status == subnetSettlementFinalized || value.Status == subnetSettlementCarried {
		if value.ExpiryBlock < clock.expiryFloor {
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-carry-isolation", target, subnetSettlementEpochFrame(entry.epoch, "expiry-schedule"), scope, subnetSettlementPlaybook, subnetReplayIssue{
				tier: tierPage, sustain: 1, invariant: "expiryBlock>=start(epoch+claimTtl+claimGrace+1)-1",
				expected: strconv.FormatUint(clock.expiryFloor, 10), observed: strconv.FormatUint(value.ExpiryBlock, 10),
				delta:     strconv.FormatInt(int64(value.ExpiryBlock)-int64(clock.expiryFloor), 10),
				symptom:   "An entitlement's claim expiry precedes its snapshotted claim ttl and grace",
				mechanism: "Finalization snapshots expiry at the start of the epoch after claim ttl and grace; an earlier expiry lets unclaimed credit carry before providers could claim it.",
				baseline:  "Every finalized expiry is at or after its policy floor.",
				action:    "Preserve the entitlement and its finalization receipt and route to custody review; do not expire or carry it early.",
				verify:    "Independent replay attributes the schedule and a recorded disposition closes it.",
			}))
		}
	}
	if value.Status == subnetSettlementCarried && block <= value.ExpiryBlock {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-carry-isolation", target, subnetSettlementEpochFrame(entry.epoch, "early-expiry"), scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: "carried-only-after-expiry",
			expected: "finalized-block>" + strconv.FormatUint(value.ExpiryBlock, 10), observed: strconv.FormatUint(block, 10),
			symptom:   "An entitlement carried before its claim expiry",
			mechanism: "Expiry is permissionless only after the snapshotted expiry block; carrying earlier strands valid claims and moves their credit into carry.",
			baseline:  "An entitlement becomes carried only after its expiry block.",
			action:    "Preserve the entitlement and expiry receipt and route to custody review; do not reopen or rewrite it.",
			verify:    "Independent replay attributes the early expiry and a recorded disposition closes it.",
		}))
	}
	return findings
}

// Root commitment rules: window, committer and the finalized entitlement.
func evaluateSubnetSettlementRoot(entry *subnetSettlementEntry, block uint64, scope subnetReplayScope) []finding {
	findings := []finding{}
	value := entry.entitlement
	root := entry.root
	clock := entry.clock
	target := subnetSettlementOperatorTarget(entry.noId)
	rootPresent := root.PayoutRoot != [32]byte{}
	rule := func(condition, invariant, expected, observed, symptom, mechanism string) {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-payout-root", target, subnetSettlementEpochFrame(entry.epoch, condition), scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: invariant, expected: expected, observed: observed,
			symptom:   symptom,
			mechanism: mechanism,
			baseline:  "The authorized operator root and artifact hash, committed inside the root window, are exactly the root and hash its finalized entitlement holds.",
			action:    "Preserve the root commitment, finalization receipt and artifact by hash; route to the operator's root-signing owner and custody review. Never replace a root or reopen a finalized entitlement.",
			verify:    "Independent replay at this checkpoint attributes the mismatch and a recorded disposition closes it.",
		}))
	}
	if rootPresent {
		if root.ArtifactHash == [32]byte{} || root.CommitBlock < clock.endBlock || clock.endBlock+clock.policy.RootCommitWindowBlocks < root.CommitBlock || block < root.CommitBlock {
			rule("commit-window", "root-committed-inside-window", "commit-block-in-root-window", "commit-block="+strconv.FormatUint(root.CommitBlock, 10),
				"A payout root was committed outside its root window",
				"The coordinator accepts a root only between the epoch end and the end of the root window, with a nonzero artifact hash.")
		}
		if root.Committer != entry.operator.RootSigner || root.Committer == (common.Address{}) {
			rule("committer", "committer=operatorAt(no,epoch).rootSigner", "operator-root-signer", "different-committer",
				"A payout root was committed by an address other than the operator's root signer for that epoch",
				"Only the root signer of the operator version effective at the epoch can commit; another committer is an unauthorized root.")
		}
	}
	switch value.Status {
	case subnetSettlementFinalized, subnetSettlementCarried:
		if !rootPresent {
			rule("finalized-without-root", "finalized-entitlement-has-committed-root", "root-present", "root-absent",
				"An entitlement is finalized without a committed root",
				"Finalization with a root copies the committed root and artifact hash into the immutable vault; without one the epoch must finalize as root-missed.")
		} else if value.PayoutRoot != root.PayoutRoot || value.ArtifactHash != root.ArtifactHash {
			rule("root-rewrite", "entitlement-root=committed-root", "identical-root-and-artifact", "different-root-or-artifact",
				"A finalized entitlement holds a different root or artifact than its commitment",
				"The vault keeps exactly the committed root and artifact hash; a difference is a finalized entitlement rewrite or a forged finalization.")
		}
	case subnetSettlementRootMissed:
		if rootPresent {
			rule("missed-with-root", "root-missed-has-no-root", "root-absent", "root-present",
				"An epoch was finalized as root-missed although a root was committed",
				"Finalization uses a committed root when present; root-missed with a root discards a valid payout list.")
		}
	}
	return findings
}

// Fixed public name of a vault status.
func subnetSettlementStatusName(status uint8) string {
	switch status {
	case subnetSettlementUnset:
		return "unset"
	case subnetSettlementFunded:
		return "funded"
	case subnetSettlementFinalized:
		return "finalized"
	case subnetSettlementRootMissed:
		return "root-missed"
	case subnetSettlementCarried:
		return "carried"
	default:
		return "unsupported"
	}
}
