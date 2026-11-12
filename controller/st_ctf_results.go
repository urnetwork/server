package controller

// st_ctf_results.go — what a built CTF root records for readers (sn
// CAPTURETHEFLAG.md §4.4, §4.5, §5, §9.1), in the capture the flag tables
// (model/ctf_model.go): each drawn epoch's draw, each covered epoch's result
// with its awards, pots and pending awards, fixed at the first live root that
// covers it, and the ledger itself. GET /sn/ctf and its routes read them.

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// stCtfEpochReport is what a covered epoch's result keeps beyond its columns
// (ctf_epoch.report): the census's excused samples and exclusion counts, the
// draw's randomness and seed, and the candidates the draw left out as bound.
type stCtfEpochReport struct {
	ExcusedSamples []uint16                `json:"excused_samples"`
	Excluded       []stCtfEpochReportCount `json:"excluded"`
	Randomness     string                  `json:"randomness,omitempty"`
	Seed           string                  `json:"seed,omitempty"`
	Bound          int                     `json:"bound"`
}

type stCtfEpochReportCount struct {
	Country string `json:"country"`
	Role    uint8  `json:"role"`
	Reason  uint8  `json:"reason"`
	Count   uint32 `json:"count"`
}

// stCtfLedgerReport is a built ledger's report (ctf_ledger.report): the
// epochs it covers and its accounts.
type stCtfLedgerReport struct {
	Epochs      []uint64 `json:"epochs"`
	Accounts    int      `json:"accounts"`
	NewAccounts int      `json:"new_accounts"`
}

// stCtfUnitFamilies returns the unit families a covered epoch's census
// lists, empty for an epoch without a canonical census.
func stCtfUnitFamilies(e *ctf.EpochResult, objects ledger.Objects) map[[32]byte]uint8 {
	families := map[[32]byte]uint8{}
	if e.Census == nil || e.CensusErr != nil {
		return families
	}
	census, err := ctf.DecodeCensus(objects[*e.Census], objects)
	if err != nil {
		return families
	}
	for _, candidate := range census.Candidates {
		families[candidate.Unit] = candidate.Family
	}
	return families
}

// stCtfEpochResultRows converts a covered epoch's result into its rows.
func stCtfEpochResultRows(built *ctf.Ledger, e *ctf.EpochResult) (*model.CtfEpochResult, []*model.CtfAward, []*model.CtfPot, []*model.CtfPendingAward, error) {
	report := &stCtfEpochReport{ExcusedSamples: e.ExcusedSamples, Excluded: []stCtfEpochReportCount{}, Bound: len(e.Bound)}
	if report.ExcusedSamples == nil {
		report.ExcusedSamples = []uint16{}
	}
	for _, exclusion := range e.Excluded {
		report.Excluded = append(report.Excluded, stCtfEpochReportCount{Country: exclusion.Country.String(), Role: uint8(exclusion.Role), Reason: uint8(exclusion.Reason), Count: exclusion.Count})
	}
	if e.Drawn {
		report.Randomness, report.Seed = "0x"+hex.EncodeToString(e.Randomness[:]), "0x"+hex.EncodeToString(e.Seed[:])
	}
	reportJson, err := json.Marshal(report)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	result := &model.CtfEpochResult{
		Epoch: e.Epoch, LedgerEpoch: built.Commit.Epoch, LedgerHash: built.Commit.LedgerHash,
		Drawn: e.Drawn, Round: e.Round, Candidates: uint64(e.Candidates),
		Funding: e.Funding, Recycled: e.Recycled, Owed: e.Owed, Returned: e.Returned, Credited: e.Credited, Matured: e.Matured,
		Fresh: e.Fresh, Shortfall: e.Shortfall, SlotBase: e.SlotBase, SlotRemainder: int(e.SlotRemainder), Awards: len(e.Awards),
		PaidSlots: e.PaidSlots, PaidPots: e.PaidPots, PaidPending: e.PaidPending, PendingTotal: e.PendingTotal, Carried: e.Carried,
		AwardsRef: e.AwardsRef, PotsRef: e.PotsRef, PendingRef: e.PendingRef, Report: reportJson,
	}
	if e.Census != nil {
		hash, block := [32]byte(*e.Census), e.CensusBlock
		result.CensusHash, result.CensusBlock = &hash, &block
	}
	if e.CensusErr != nil {
		result.CensusError = e.CensusErr.Error()
	}
	families := stCtfUnitFamilies(e, built.Objects)
	awards := make([]*model.CtfAward, 0, len(e.Awards))
	for _, award := range e.Awards {
		awards = append(awards, &model.CtfAward{Epoch: e.Epoch, Role: int(award.Role), CountryCode: award.Country.String(), Rank: award.Rank,
			Unit: award.Unit, UnitFamily: int(families[award.Unit]), ClientId: server.Id(award.ClientID), Coldkey: award.Coldkey,
			Slot: award.Slot, Pot: award.Pot, Deferred: award.Deferred})
	}
	pots := make([]*model.CtfPot, 0, len(e.Pots))
	for _, pot := range e.Pots {
		pots = append(pots, &model.CtfPot{Epoch: e.Epoch, CountryCode: pot.Country.String(), Role: int(pot.Role), Units: int(pot.Units), Winners: pot.Winners, In: pot.In, Out: pot.Out})
	}
	pending := make([]*model.CtfPendingAward, 0, len(e.Pending))
	for _, award := range e.Pending {
		pending = append(pending, &model.CtfPendingAward{Epoch: e.Epoch, AwardEpoch: award.Epoch, Role: int(award.Role), CountryCode: award.Country.String(),
			Rank: award.Rank, Coldkey: award.Coldkey, Amount: award.Amount})
	}
	return result, awards, pots, pending, nil
}

// stCtfLedgerRow is a built ledger's row.
func stCtfLedgerRow(in *ctf.BuildInput, built *ctf.Ledger, shadow bool, report []byte) (*model.CtfLedger, error) {
	if report == nil {
		ledgerReport := &stCtfLedgerReport{Accounts: len(built.Accounts), NewAccounts: len(built.Accounts)}
		if in.Previous != nil {
			ledgerReport.NewAccounts -= len(in.Previous.Accounts)
		}
		for _, input := range in.Epochs {
			ledgerReport.Epochs = append(ledgerReport.Epochs, input.Epoch)
		}
		var err error
		if report, err = json.Marshal(ledgerReport); err != nil {
			return nil, err
		}
	}
	row := &model.CtfLedger{Epoch: built.Commit.Epoch, FirstEpoch: in.Epochs[0].Epoch, LedgerHash: built.Commit.LedgerHash, Shadow: shadow,
		Root: built.Commit.Root, Total: built.Commit.Total, LeafCount: built.Commit.LeafCount, Header: built.Header, Report: report,
		Status: model.CtfLedgerStatusBuilt}
	if shadow {
		row.Status = model.CtfLedgerStatusShadow
	}
	return row, nil
}

// stCtfRecordBuild records a live root before its header is published: the
// draw of each drawn epoch, the result of each epoch no committed root has
// carried, and the ledger. A result is fixed once its root is committed
// (stCtfResultCommitted); until then a later build replaces it, since only an
// uncommitted build recorded it. Draws and ledgers are written once.
func stCtfRecordBuild(ctx context.Context, cfg *StConfig, records stCtfRecords, in *ctf.BuildInput, built *ctf.Ledger) error {
	recorded := records.EpochResults(ctx, in.Epochs[0].Epoch, built.Commit.Epoch)
	for i, e := range built.Epochs {
		input := &in.Epochs[i]
		if e.Drawn && input.Pulse != nil && e.Census != nil {
			records.CreateDraw(ctx, &model.CtfDraw{Epoch: e.Epoch, CensusHash: *e.Census, Round: e.Round, Signature: input.Pulse.Signature, Randomness: e.Randomness, Seed: e.Seed})
		}
		if input.Awards != nil {
			continue
		}
		if previous := recorded[e.Epoch]; previous != nil {
			if previous.LedgerHash == built.Commit.LedgerHash {
				continue
			}
			records.RemoveEpochResult(ctx, e.Epoch)
		}
		result, awards, pots, pending, err := stCtfEpochResultRows(built, e)
		if err != nil {
			return err
		}
		records.CreateEpochResult(ctx, result, awards, pots, pending)
	}
	row, err := stCtfLedgerRow(in, built, false, nil)
	if err != nil {
		return err
	}
	records.CreateLedger(ctx, row)
	return nil
}

// stCtfLedgerStatus maps a mirrored root's status to its ledger's.
func stCtfLedgerStatus(root *model.StLedgerV2Root) *model.CtfLedgerStatusUpdate {
	update := &model.CtfLedgerStatusUpdate{Seq: &root.Seq, CommitBlock: root.CommitBlock}
	switch root.Status {
	case model.StLedgerV2RootPending:
		update.Status = model.CtfLedgerStatusCommitted
	case model.StLedgerV2RootFinalized:
		update.Status, update.RootIndex, update.FinalizeBlock = model.CtfLedgerStatusFinalized, root.RootIndex, root.StatusBlock
	case model.StLedgerV2RootReplaced:
		update.Status = model.CtfLedgerStatusReplaced
	case model.StLedgerV2RootDeleted:
		update.Status = model.CtfLedgerStatusDeleted
	default:
		return nil
	}
	return update
}
