package controller

// st_ledger_v2_mirror.go — applies mirrored vault2 events to the settlement
// v2 tables (VERSION2-FINAL.md §8.4): funding snapshots, roots by commit seq,
// and path demands by seq, finalized root and account index. Events apply in
// (block, log) order and every write is idempotent, so the conservative
// re-scan of a range changes nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server/model"
)

// stLedgerV2EventData is the union of the mirrored v2 events' fields.
type stLedgerV2EventData struct {
	E             string `json:"e"`
	NoId          string `json:"no_id"`
	Seq           string `json:"seq"`
	Index         string `json:"index"`
	Root          string `json:"root"`
	Total         string `json:"total"`
	LeafCount     string `json:"leaf_count"`
	LedgerHash    string `json:"ledger_hash"`
	ReplacedSeq   string `json:"replaced_seq"`
	RootIndex     string `json:"root_index"`
	Observed      string `json:"observed"`
	Amount        string `json:"amount"`
	FundedThrough string `json:"funded_through"`
	Demander      string `json:"demander"`
	Answerer      string `json:"answerer"`
}

func stApplyLedgerV2Events(ctx context.Context, cfg *StConfig, events []*model.StChainEvent) {
	if !cfg.LedgerV2Enabled() {
		return
	}
	ordered := append([]*model.StChainEvent(nil), events...)
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].BlockNumber != ordered[b].BlockNumber {
			return ordered[a].BlockNumber < ordered[b].BlockNumber
		}
		return ordered[a].LogIndex < ordered[b].LogIndex
	})
	deploymentKey := cfg.DeploymentKey()
	for _, event := range ordered {
		var data stLedgerV2EventData
		if json.Unmarshal([]byte(event.DataJson), &data) != nil {
			continue
		}
		noId, err := stParseUint(data.NoId)
		if err != nil {
			continue
		}
		block := event.BlockNumber
		switch event.Kind {
		case "V2EmissionCaptured", "V2EmissionDeferred":
			deferred := event.Kind == "V2EmissionDeferred"
			if deferred {
				data.Observed, data.Amount = "0", "0"
			}
			funding := &model.StLedgerV2Funding{NoId: noId, Deferred: deferred, BlockNumber: block}
			var epochErr error
			funding.Epoch, epochErr = stParseUint(data.E)
			if epochErr != nil || funding.FundedThrough.SetFromDecimal(data.FundedThrough) != nil ||
				funding.Observed.SetFromDecimal(data.Observed) != nil || funding.Amount.SetFromDecimal(data.Amount) != nil {
				continue
			}
			model.SetStLedgerV2Funding(ctx, deploymentKey, funding)
			if deferred && noId == cfg.NoId {
				stLedgerV2Alert(cfg, StLedgerV2AlertMissedClose, fmt.Sprintf("epoch %d", funding.Epoch), "the epoch was deferred at block %d", block)
			}
		case "V2DistributionCommitted", "V2DistributionFinalized":
			root, ok := stLedgerV2EventRoot(noId, &data)
			if !ok {
				continue
			}
			if event.Kind == "V2DistributionFinalized" {
				rootIndex, err := stParseUint(data.RootIndex)
				if err != nil {
					continue
				}
				root.RootIndex = &rootIndex
				model.SetStLedgerV2RootFinalized(ctx, deploymentKey, root, block)
				continue
			}
			replacedSeq, err := stParseUint(data.ReplacedSeq)
			if err != nil {
				continue
			}
			model.AddStLedgerV2Commit(ctx, deploymentKey, root, block, replacedSeq)
		case "V2DistributionDeleted", "V2PathDemanded", "V2PathAnswered":
			seq, seqErr := stParseUint(data.Seq)
			rootIndex, rootIndexErr := stParseUint(data.RootIndex)
			index, indexErr := stParseUint(data.Index)
			if seqErr != nil || rootIndexErr != nil || indexErr != nil {
				continue
			}
			switch event.Kind {
			case "V2DistributionDeleted":
				model.SetStLedgerV2RootDeleted(ctx, deploymentKey, noId, seq, block)
			case "V2PathDemanded":
				model.AddStLedgerV2Demand(ctx, deploymentKey, &model.StLedgerV2Demand{NoId: noId, Seq: seq, RootIndex: rootIndex, Index: index, Demander: data.Demander, DemandBlock: block})
			default:
				model.SetStLedgerV2DemandAnswered(ctx, deploymentKey, noId, seq, rootIndex, index, data.Answerer, block)
			}
		}
	}
}

// The root fields DistributionCommitted and DistributionFinalized share.
func stLedgerV2EventRoot(noId uint64, data *stLedgerV2EventData) (*model.StLedgerV2Root, bool) {
	root := &model.StLedgerV2Root{NoId: noId}
	var seqErr, epochErr, leafCountErr, rootErr, hashErr error
	root.Seq, seqErr = stParseUint(data.Seq)
	root.Epoch, epochErr = stParseUint(data.E)
	root.LeafCount, leafCountErr = stParseUint(data.LeafCount)
	root.Root, rootErr = stParseHex32(data.Root)
	root.LedgerHash, hashErr = stParseHex32(data.LedgerHash)
	total, totalErr := uint256.FromDecimal(data.Total)
	if seqErr != nil || epochErr != nil || leafCountErr != nil || rootErr != nil || hashErr != nil || totalErr != nil {
		return nil, false
	}
	root.Total = *total
	return root, true
}
