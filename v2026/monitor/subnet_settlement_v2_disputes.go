// SIGNALS.md §22.7 settlement v2 disputes (incident 2026-11-05, plan R4b).
// vault2's dispute game is optimistic: a root that restates a finalized
// balance finalizes unless someone deletes it with checkPath or blocks it
// with demandPath within its challenge window. Every dispute is therefore an
// incident, and so is a pending root replaced before it finalized. The
// family reads vault2's dispute events over a bounded finalized frontier
// whose first window is vault2's own challenge window, so a restarted monitor
// still sees every dispute of the last challenge window.
package monitor

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/v2026/stabi"
)

// LedgerTree.Fault, in declaration order.
var subnetLedgerTreeFaults = []string{"none", "inconsistent-node", "lower-value", "wrong-leaf"}

// The process-local dispute frontier of one vault2: the next block to read,
// zero before the first published run, and vault2's immutable challenge
// window once read. Safe for concurrent use.
type subnetSettlementDisputeFrontier struct {
	stateLock       sync.Mutex
	vault           common.Address
	next            uint64
	challengeBlocks uint64
}

func newSubnetSettlementDisputeFrontier() *subnetSettlementDisputeFrontier {
	return &subnetSettlementDisputeFrontier{}
}

// The position for this run, resetting when the pinned vault2 changed.
func (self *subnetSettlementDisputeFrontier) position(vault common.Address) (uint64, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.vault != vault {
		self.vault, self.next, self.challengeBlocks = vault, 0, 0
	}
	return self.next, self.challengeBlocks
}

// Records a published run's read; the frontier never moves back.
func (self *subnetSettlementDisputeFrontier) commit(vault common.Address, read *subnetSettlementDisputeRead) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.vault != vault {
		return
	}
	self.challengeBlocks = read.challengeBlocks
	if self.next < read.next {
		self.next = read.next
	}
}

// One run's read: its windows and where the frontier stands after them.
type subnetSettlementDisputeRead struct {
	windows         []*subnetReplayLogWindow
	challengeBlocks uint64
	next            uint64
	complete        bool
}

// Reads vault2's commit, demand and deletion events toward the checkpoint and
// verifies every log block is canonical. Nothing is committed here.
func readSubnetSettlementV2Disputes(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin, checkpoint subnetReplayEvmCheckpoint, frontier *subnetSettlementDisputeFrontier, settings *SubnetReplaySettings) (*subnetSettlementDisputeRead, error) {
	v2, err := subnetRootDeadlineV2Abis()
	if err != nil {
		return nil, newSubnetReplayUnknown("binding-unavailable", err)
	}
	next, challengeBlocks := frontier.position(pin.settlementVaultV2)
	if challengeBlocks == 0 {
		view := newSubnetReplayView(v2.vault, pin.settlementVaultV2, "CHALLENGE_BLOCKS")
		if err := readSubnetReplayViews(ctx, rpc, checkpoint, view); err != nil {
			return nil, err
		}
		if challengeBlocks, err = subnetReplayValue[uint64](view, 0); err != nil {
			return nil, err
		}
		if challengeBlocks == 0 {
			return nil, newSubnetReplayUnknown("challenge-window-invalid", nil)
		}
	}
	// The first window is the challenge window, in which a demand can still
	// be made against the newest pending roots.
	if next == 0 {
		next = 1
		if challengeBlocks <= checkpoint.number {
			next = checkpoint.number - challengeBlocks + 1
		}
	}
	if checkpoint.number+1 < next {
		return nil, newSubnetReplayUnknown("finality-regressed", nil)
	}
	read := &subnetSettlementDisputeRead{challengeBlocks: challengeBlocks, next: next, complete: true}
	windows := subnetReplayLogWindows(next, checkpoint, settings)
	if len(windows) == 0 {
		return read, nil
	}
	topics := []common.Hash{v2.vault.Events["DistributionCommitted"].ID, v2.vault.Events["PathDemanded"].ID, v2.vault.Events["DistributionDeleted"].ID}
	if err := readSubnetReplayTopicLogs(ctx, rpc, pin.settlementVaultV2, topics, settings.LogMaximumEntriesPerRange, windows...); err != nil {
		return nil, err
	}
	if err := verifySubnetReplayLogBlocks(ctx, rpc, windows...); err != nil {
		return nil, err
	}
	read.windows = windows
	read.next = windows[len(windows)-1].to + 1
	read.complete = read.next == checkpoint.number+1
	return read, nil
}

// One dispute incident: an operator's pending root sequence and its kind.
type subnetSettlementDisputeKey struct {
	noId uint64
	seq  uint64
	kind string
}

type subnetSettlementDispute struct {
	count      int
	firstBlock uint64
	lastBlock  uint64
	observed   string
}

// Every demand, deletion and replacing commit pages; an ordinary commit is
// silent. A known topic that does not decode leaves the read unknown.
func evaluateSubnetSettlementV2Disputes(read *subnetSettlementDisputeRead, scope subnetReplayScope) ([]finding, string) {
	v2, err := subnetRootDeadlineV2Abis()
	if err != nil {
		return nil, "binding-unavailable"
	}
	binding := stabi.NewSTSettlementVault2()
	shape := func(name string, log *types.Log) bool {
		event := v2.vault.Events[name]
		topics, words := 1, 0
		for _, input := range event.Inputs {
			if input.Indexed {
				topics += 1
			} else {
				words += 1
			}
		}
		return len(log.Topics) == topics && len(log.Data) == 32*words
	}
	disputes := map[subnetSettlementDisputeKey]*subnetSettlementDispute{}
	record := func(noId *big.Int, seq uint64, kind string, block uint64, observed string) bool {
		if noId == nil || !noId.IsUint64() {
			return false
		}
		key := subnetSettlementDisputeKey{noId: noId.Uint64(), seq: seq, kind: kind}
		dispute, ok := disputes[key]
		if !ok {
			dispute = &subnetSettlementDispute{firstBlock: block, observed: observed}
			disputes[key] = dispute
		}
		dispute.count += 1
		dispute.lastBlock = block
		return true
	}
	for _, window := range read.windows {
		for i := range window.logs {
			log := &window.logs[i]
			switch log.Topics[0] {
			case v2.vault.Events["DistributionCommitted"].ID:
				event, err := binding.UnpackDistributionCommittedEvent(log)
				if !shape("DistributionCommitted", log) || err != nil || event.Total == nil {
					return nil, "dispute-event-malformed"
				}
				if event.ReplacedSeq == 0 {
					continue
				}
				if !record(event.NoId, event.Seq, "replacing-commit", log.BlockNumber, fmt.Sprintf("replaced_seq=%d epoch=%d leaf_count=%d total=%s", event.ReplacedSeq, event.Epoch, event.LeafCount, event.Total.String())) {
					return nil, "dispute-event-malformed"
				}
			case v2.vault.Events["PathDemanded"].ID:
				event, err := binding.UnpackPathDemandedEvent(log)
				if !shape("PathDemanded", log) || err != nil || event.RootIndex == nil {
					return nil, "dispute-event-malformed"
				}
				if !record(event.NoId, event.Seq, "path-demanded", log.BlockNumber, "demanded_root_index="+event.RootIndex.String()) {
					return nil, "dispute-event-malformed"
				}
			case v2.vault.Events["DistributionDeleted"].ID:
				event, err := binding.UnpackDistributionDeletedEvent(log)
				if !shape("DistributionDeleted", log) || err != nil || event.Level == nil || event.RootIndex == nil || event.Index == nil {
					return nil, "dispute-event-malformed"
				}
				fault := "unsupported"
				if int(event.Fault) < len(subnetLedgerTreeFaults) {
					fault = subnetLedgerTreeFaults[event.Fault]
				}
				if !record(event.NoId, event.Seq, "distribution-deleted", log.BlockNumber, fmt.Sprintf("fault=%s level=%s root_index=%s index=%s", fault, event.Level.String(), event.RootIndex.String(), event.Index.String())) {
					return nil, "dispute-event-malformed"
				}
			}
		}
	}
	keys := make([]subnetSettlementDisputeKey, 0, len(disputes))
	for key := range disputes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].noId != keys[j].noId {
			return keys[i].noId < keys[j].noId
		}
		if keys[i].seq != keys[j].seq {
			return keys[i].seq < keys[j].seq
		}
		return keys[i].kind < keys[j].kind
	})
	findings := []finding{}
	for _, key := range keys {
		dispute := disputes[key]
		issue := subnetSettlementV2DisputeIssue(key.kind)
		issue.observed = fmt.Sprintf("%s events=%d %s", key.kind, dispute.count, dispute.observed)
		issue.deadline = fmt.Sprintf("first_event_block=%d last_event_block=%d challenge_blocks=%d", dispute.firstBlock, dispute.lastBlock, read.challengeBlocks)
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-payout-root", subnetSettlementOperatorTarget(key.noId), "v2-seq-"+strconv.FormatUint(key.seq, 10)+"/"+key.kind, scope, subnetSettlementPlaybook, issue))
	}
	return findings, ""
}

// The fixed page of one dispute kind; the action follows the incident runbook.
func subnetSettlementV2DisputeIssue(kind string) subnetReplayIssue {
	const runbook = "factury security/incident-20261105/REMEDIATION-PLAN.md, \"Runbook: suspected restatement or governance compromise\""
	issue := subnetReplayIssue{tier: tierPage, sustain: 1}
	switch kind {
	case "path-demanded":
		issue.invariant, issue.expected = "v2-pending-root-needs-no-demand", "no-path-demand"
		issue.symptom = "Someone demanded an opening of an operator's pending settlement v2 root"
		issue.mechanism = "Anyone can pay vault2's demand fee to require that the pending root be opened at one account of a finalized root, and the root cannot finalize while a demand is open. A watcher demands when it cannot verify the pending root from the published ledger, so a demand means a withheld or unverifiable tree, or someone probing for a lowered account."
		issue.baseline = "Every pending root's ledger is published and verifies, so no watcher demands a path."
		issue.action = "Check that this root's ledger objects are served and that the watcher verifies them, and answer every open demand before the commit block plus challenge_blocks. If the tree is withheld or a demand cannot be answered, follow the runbook (" + runbook + ", steps 1 and 3). Keep the transaction hashes and the served objects."
		issue.verify = "Every demand is answered and the root finalizes, or it is deleted or replaced, and the incident record attributes the demander."
	case "distribution-deleted":
		issue.invariant, issue.expected = "v2-pending-root-has-no-fault", "no-fault"
		issue.symptom = "A settlement v2 pending root was deleted as faulty"
		issue.mechanism = "checkPath proved that the operator's pending root lowers, reassigns or hides an account of a finalized root, or opened a demanded account with a fault, so vault2 deleted it. The root signer committed a ledger that restates a finalized balance: a stolen root-signer key, a hostile coordinator or a ledger bug."
		issue.baseline = "No pending root is ever deleted: each keeps every finalized account at its coldkey with at least its previous value."
		issue.action = "Treat the root signer and settlement v2 governance as suspect and follow the runbook (" + runbook + "): pause v1 with the guardian if the coordinator path is hostile, rotate the root signer and the auditor, keep the challenge transaction and the served objects, and tell the owner. Never recommit the same tree."
		issue.verify = "The incident record attributes the faulty root, and the operator's next root verifies and finalizes."
	default:
		issue.invariant, issue.expected = "v2-pending-root-finalizes-before-replacement", "replaced_seq=0"
		issue.symptom = "An operator's settlement v2 pending root was replaced before it finalized"
		issue.mechanism = "The coordinator lets the root signer replace a pending root only after the next epoch has ended, so the replaced root never finalized: its auditor quorum did not approve it, a demand stayed open, or nobody finalized it. An auditor that refuses a root it cannot verify is the defense working."
		issue.baseline = "Each pending root is approved by the auditor quorum and finalized after its challenge window, so every commit has replacedSeq zero."
		issue.action = "Find why the replaced root did not finalize: read the auditor's refusal and ledger audit, any open demands and the finalizer. If the auditor refused a root it could not verify, follow the runbook (" + runbook + ", steps 1 and 3) and verify the replacement root before its window closes."
		issue.verify = "The incident record explains the replaced root, and the replacement verifies and finalizes."
	}
	return issue
}
