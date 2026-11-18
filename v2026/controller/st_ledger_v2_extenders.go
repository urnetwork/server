package controller

// st_ledger_v2_extenders.go — the SN25 extender pool in the settlement v2
// build (sn WHITEPAPER.md §8.6.2, §8.6.7). For every covered epoch the
// extender pool amendment covers (st_ledger_v2_amendments.go), the build
// reads the epoch's provider rows without the shares the amendment pools,
// and gives ledger.Build the epoch's extender component: Z, the pooled
// shares of the contracts a balance funded less the declared measurement and
// test traffic, and the census captured at the epoch's end. The ledger
// splits Z, capped at the epoch's eligible provider usage, over the census's
// units and allocates the share rows with the provider rows. Before E_x, and
// without the amendment, every epoch is built exactly as before.
//
// The census is captured once, as soon as the epoch's end boundary is
// finalized, beside its users (st_ledger_v2_usage.go), for every epoch: it is
// what the epoch's component is built from once the amendment covers it, and
// before that it is the shadow of what would have been.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urfoundation/sn/v2026/ledger"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The usage step that captures an epoch's extender census.
const StLedgerV2UsageStepExtenderCensus = "extender_census"

// A build of an epoch the amendment covers before its census is captured; the
// build retries.
var errStLedgerV2ExtenderCensusMissing = errors.New("st ledger v2: the epoch's extender census is not captured yet")

// Reports whether an epoch's census is still to be captured. A failed read,
// such as a table not migrated yet, is logged and plans nothing, so it can
// never stop the users or the statement.
func stLedgerV2ExtenderCensusDue(ctx context.Context, cfg *StConfig, epoch uint64) (due bool) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]v2 epoch %d extender census read failed: %v\n", epoch, r)
			due = false
		}
	}()
	return model.GetStLedgerV2ExtenderCensus(ctx, cfg.DeploymentKey(), cfg.NoId, epoch) == nil
}

// Captures an epoch's census once, as its records stand when the step runs, as
// soon as the epoch's end boundary is finalized. An epoch already captured is
// never captured again.
func stLedgerV2CaptureExtenderCensus(ctx context.Context, cfg *StConfig, client StClient, epoch uint64) *StPublishOutcome {
	key := cfg.DeploymentKey()
	if captured := model.GetStLedgerV2ExtenderCensus(ctx, key, cfg.NoId, epoch); captured != nil {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d extender census already captured: %d rows", epoch, captured.Rows)}
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	if authority == nil || authority.Epoch != epoch || authority.PolicyHash != cfg.PolicyHash {
		return stLedgerV2UsageRetry(fmt.Sprintf("the payout authority of epoch %d is absent or differs", epoch))
	}
	// the census from the extender directory under the shared eligibility
	// rules (extender_eligibility.go), the rules capture the flag's census
	// judges extenders by
	records := model.GetStExtenderCensusRecords(ctx, authority.StartTime, authority.EndTime)
	rules, err := stExtenderEligibilityRules(ctx, cfg, client, authority, records)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d extender census: %s", epoch, err))
	}
	rows, err := model.BuildStExtenderCensus(records, rules)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d extender census: %s", epoch, err))
	}
	captured := model.CaptureStLedgerV2ExtenderCensus(ctx, key, cfg.NoId, epoch, model.EncodeStExtenderCensus(rows), uint64(len(rows)), server.NowUtc())
	if kept, err := model.DecodeStExtenderCensus(captured.Census); err == nil {
		stExtenderCensusMetrics(epoch, kept)
	}
	eligible, units := stExtenderCensusCounts(rows)
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d extender census captured at %s: %d rows, %d eligible extenders in %d units", epoch, captured.CaptureTime.UTC().Format("2006-01-02T15:04:05Z"), captured.Rows, eligible, units)}
}

// A census's eligible extenders and the units that hold them.
func stExtenderCensusCounts(rows []ledger.CensusRow) (eligible int, units int) {
	seenUnits := map[[32]byte]bool{}
	for i := range rows {
		if rows[i].Eligible() {
			eligible++
			seenUnits[rows[i].Unit] = true
		}
	}
	return eligible, len(seenUnits)
}

// Reads an epoch's captured census.
func stLedgerV2CapturedCensus(ctx context.Context, cfg *StConfig, epoch uint64) ([]ledger.CensusRow, error) {
	captured := model.GetStLedgerV2ExtenderCensus(ctx, cfg.DeploymentKey(), cfg.NoId, epoch)
	if captured == nil {
		return nil, fmt.Errorf("%w: epoch %d", errStLedgerV2ExtenderCensusMissing, epoch)
	}
	rows, err := model.DecodeStExtenderCensus(captured.Census)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: epoch %d's captured extender census: %w", epoch, err)
	}
	if uint64(len(rows)) != captured.Rows {
		return nil, fmt.Errorf("st ledger v2: epoch %d's captured extender census holds %d rows, not %d", epoch, len(rows), captured.Rows)
	}
	return rows, nil
}

// A recorded epoch's extender component as ledger.Build takes it again: the Z
// it was built with, and its census and relay rows read back from the census
// and relay chunks it was built with, which the build must reproduce (fixed
// inputs). A component recorded before its relay chunks were recorded was built
// with no relay rows, and is built again with none. A component's presence must
// match the amendment: rows recorded without one for an epoch the amendment
// covers, or with one for an epoch it does not, are refused, and only an
// operator rebuild of the epoch builds both again.
func stLedgerV2RecordedExtenders(ctx context.Context, covered *StLedgerV2CoveredEpoch, amendment model.StExtenderAmendment) (*ledger.ExtenderInput, error) {
	recorded := covered.Rows.Extenders
	switch inForce := amendment.InForce(covered.Epoch); {
	case covered.Rows.Null() && recorded != nil:
		return nil, fmt.Errorf("st ledger v2: epoch %d is declared unrecoverable and has an extender component", covered.Epoch)
	case covered.Rows.Null():
		return nil, nil
	case inForce && recorded == nil:
		return nil, fmt.Errorf("st ledger v2: epoch %d's rows were recorded without the extender component the amendment in force from epoch %d calls for; only an operator rebuild of the epoch builds both", covered.Epoch, amendment.FirstEpoch)
	case !inForce && recorded != nil:
		return nil, fmt.Errorf("st ledger v2: epoch %d's rows were recorded with an extender component that no amendment in force calls for", covered.Epoch)
	case recorded == nil:
		return nil, nil
	}
	read := func(what string, recordedRefs []string) ([][]byte, []ledger.Ref, error) {
		chunks := make([][]byte, len(recordedRefs))
		refs := make([]ledger.Ref, len(chunks))
		for i, ref := range recordedRefs {
			chunk, err := GetStLedgerV2Object(ctx, ref)
			if err != nil {
				return nil, nil, fmt.Errorf("st ledger v2: epoch %d %s: %w", covered.Epoch, what, err)
			}
			// the object store returns only bytes that hash to the reference
			chunks[i], refs[i] = chunk, ledger.RefOf(chunk)
		}
		return chunks, refs, nil
	}
	chunks, refs, err := read("census", recorded.CensusChunks)
	if err != nil {
		return nil, err
	}
	census, err := ledger.DecodeCensusChunks(chunks)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: epoch %d census: %w", covered.Epoch, err)
	}
	relayChunks, relayRefs, err := read("relay rows", recorded.RelayChunks)
	if err != nil {
		return nil, err
	}
	relay, err := ledger.DecodeRelayChunks(relayChunks)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: epoch %d relay rows: %w", covered.Epoch, err)
	}
	return &ledger.ExtenderInput{Bytes: recorded.Bytes, Census: census, Relay: relay, Chunks: refs, RelayChunks: relayRefs}, nil
}

// Chunk references as the epoch-rows record keeps them.
func stLedgerV2RefTexts(refs []ledger.Ref) []string {
	texts := make([]string, len(refs))
	for i, ref := range refs {
		texts[i] = ref.String()
	}
	return texts
}

// An epoch's relay rows as its component takes them (sn ledger RelayRow,
// WHITEPAPER.md §8.6.2): the relay bytes of each census client that relayed
// any, once per client, in the census's client order. A relaying client the
// census lacks has no row, as the census fixes whom the component can pay;
// whether a census client's row counts is the split's to decide.
func stLedgerV2RelayRows(relay *model.StEpochExtenderRelay, census []ledger.CensusRow) []ledger.RelayRow {
	rows := []ledger.RelayRow{}
	if relay == nil {
		return rows
	}
	for i := range census {
		clientId := census[i].ClientID
		// a client's census rows are adjacent
		if 0 < len(rows) && rows[len(rows)-1].ClientID == clientId {
			continue
		}
		if relayBytes := relay.ClientIdRelayBytes[server.Id(clientId)]; 0 < relayBytes {
			rows = append(rows, ledger.RelayRow{ClientID: clientId, RelayBytes: uint64(relayBytes)})
		}
	}
	return rows
}

// What the epoch-rows record keeps of an epoch's extender pool beside the
// component the ledger builds: Z and its parts, and the contracts and payer
// networks behind it. The census chunks are the build's
// (stLedgerV2EpochExtenders).
func stLedgerV2ExtenderParts(pool *model.StEpochExtenderPool) *model.StLedgerV2EpochExtenders {
	return &model.StLedgerV2EpochExtenders{
		Bytes: pool.Bytes(), IntraPlatform: uint64(pool.IntraPlatform), IntraNetwork: uint64(pool.IntraNetwork),
		Contracts: pool.Contracts, Payers: pool.Payers,
	}
}

// A built epoch's extender component as the epoch-rows record keeps it, nil
// without one: the build's Z and census and relay chunks, with the parts its
// input held. A component built without parts, or with parts of another Z, is
// refused.
func stLedgerV2EpochExtenders(epoch *ledger.EpochAllocation, parts *model.StLedgerV2EpochExtenders) (*model.StLedgerV2EpochExtenders, error) {
	switch {
	case epoch.Extenders == nil && parts == nil:
		return nil, nil
	case epoch.Extenders == nil || parts == nil || parts.Bytes != epoch.Extenders.Bytes:
		return nil, fmt.Errorf("st ledger v2: epoch %d's extender component is built without the pool parts of its bytes", epoch.Epoch)
	}
	component := *parts
	component.CensusChunks = stLedgerV2RefTexts(epoch.Extenders.Census)
	component.RelayChunks = stLedgerV2RefTexts(epoch.Extenders.Relay)
	if err := component.Check(); err != nil {
		return nil, fmt.Errorf("st ledger v2: epoch %d: %w", epoch.Epoch, err)
	}
	return &component, nil
}
