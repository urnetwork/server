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
	"net/netip"

	"github.com/urfoundation/sn/ledger"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// StLedgerV2UsageStepExtenderCensus is the usage step that captures an epoch's
// extender census.
const StLedgerV2UsageStepExtenderCensus = "extender_census"

// errStLedgerV2ExtenderCensusMissing is a build of an epoch the amendment
// covers before its census is captured; the build retries.
var errStLedgerV2ExtenderCensusMissing = errors.New("st ledger v2: the epoch's extender census is not captured yet")

// stLedgerV2ExtenderCensusDue reports whether an epoch's census is still to
// be captured. A failed read, such as a table not migrated yet, is logged and
// plans nothing, so it can never stop the users or the statement.
func stLedgerV2ExtenderCensusDue(ctx context.Context, cfg *StConfig, epoch uint64) (due bool) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]v2 epoch %d extender census read failed: %v\n", epoch, r)
			due = false
		}
	}()
	return model.GetStLedgerV2ExtenderCensus(ctx, cfg.DeploymentKey(), cfg.NoId, epoch) == nil
}

// stLedgerV2CaptureExtenderCensus captures an epoch's census once, as its
// records stand when the step runs, as soon as the epoch's end boundary is
// finalized. An epoch already captured is never captured again.
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
	rows, err := stLedgerV2ExtenderCensusRows(ctx, cfg, client, authority)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d extender census: %s", epoch, err))
	}
	captured := model.CaptureStLedgerV2ExtenderCensus(ctx, key, cfg.NoId, epoch, model.EncodeStExtenderCensus(rows), uint64(len(rows)), server.NowUtc())
	eligible, units := stExtenderCensusCounts(rows)
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d extender census captured at %s: %d rows, %d eligible extenders in %d units", epoch, captured.CaptureTime.UTC().Format("2006-01-02T15:04:05Z"), captured.Rows, eligible, units)}
}

func stExtenderCensusCounts(rows []ledger.CensusRow) (eligible int, units int) {
	seen := map[[32]byte]bool{}
	for i := range rows {
		if rows[i].Eligible() {
			eligible++
			seen[rows[i].Unit] = true
		}
	}
	return eligible, len(seen)
}

// stLedgerV2ExtenderCensusRows builds an epoch's census from the extender
// directory, under the rules provider rows follow: units keyed as the head's
// egress prefix (§8.4), coldkeys by the provider rows' wallet selection, and
// head bindings at either of the epoch's boundary blocks. The census row
// publishes the coldkey it selected; the resolutions are not retained here,
// so the census can never disagree with the retention the provider rows make
// of the same clients' wallets.
func stLedgerV2ExtenderCensusRows(ctx context.Context, cfg *StConfig, client StClient, authority *StPayoutEpochAuthority) ([]ledger.CensusRow, error) {
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		return nil, err
	}
	records := model.GetStExtenderCensusRecords(ctx, authority.StartTime, authority.EndTime)
	settings := VerifySettings()
	rules := &model.StExtenderCensusRules{
		Unit:      func(ip netip.Addr) [32]byte { return model.VerifyEgressIpHashWithSettings(ip, settings) },
		Durable:   map[server.Id]bool{},
		Declared:  pool.declared,
		Coldkeys:  map[server.Id][32]byte{},
		HeadBound: map[server.Id]bool{},
	}
	if extenders, err := EnvExtenderConfig(); err == nil {
		for _, networkId := range extenders.DurableNetworkIds() {
			rules.Durable[networkId] = true
		}
	}
	if len(records) == 0 {
		return model.BuildStExtenderCensus(records, rules)
	}
	// the provider clients, once each
	seen := map[server.Id]bool{}
	usages := []*model.StProviderUsage{}
	clientIds := [][16]byte{}
	for _, record := range records {
		if !seen[record.ClientId] {
			seen[record.ClientId] = true
			usages = append(usages, &model.StProviderUsage{ClientId: record.ClientId, NetworkId: record.NetworkId})
			clientIds = append(clientIds, stId16(record.ClientId))
		}
	}
	work, workConfigured, err := stLoadProviderWorkAuthority(ctx, cfg, authority)
	if err != nil {
		return nil, err
	}
	resolutions, err := stLedgerV2Wallets(ctx, cfg, work, workConfigured, authority, usages)
	if err != nil {
		return nil, err
	}
	for clientId, resolution := range resolutions {
		if resolution != nil {
			rules.Coldkeys[clientId] = resolution.Coldkey
		}
	}
	excluded, err := stHeadExcludedClients(ctx, cfg, client, authority.Epoch, authority.Start.Block, authority.End.Block, clientIds)
	if err != nil {
		return nil, err
	}
	for clientId := range excluded {
		rules.HeadBound[server.Id(clientId)] = true
	}
	return model.BuildStExtenderCensus(records, rules)
}

// stLedgerV2CapturedCensus reads an epoch's captured census.
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

// stLedgerV2RecordedExtenders is a recorded epoch's extender component as
// ledger.Build takes it again: the Z it was built with, and its census read
// back from the census chunks it was built with, which the build must
// reproduce (fixed inputs). A component's presence must match the amendment:
// rows recorded without one for an epoch the amendment covers, or with one for
// an epoch it does not, are refused, and only an operator rebuild of the
// epoch builds both again.
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
	chunks := make([][]byte, len(recorded.CensusChunks))
	refs := make([]ledger.Ref, len(chunks))
	for i, ref := range recorded.CensusChunks {
		chunk, err := GetStLedgerV2Object(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("st ledger v2: epoch %d census: %w", covered.Epoch, err)
		}
		// the object store returns only bytes that hash to the reference
		chunks[i], refs[i] = chunk, ledger.RefOf(chunk)
	}
	census, err := ledger.DecodeCensusChunks(chunks)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: epoch %d census: %w", covered.Epoch, err)
	}
	return &ledger.ExtenderInput{Bytes: recorded.Bytes, Census: census, Chunks: refs}, nil
}

// stLedgerV2EpochExtenders is a built epoch's extender component as the
// epoch-rows record keeps it, nil without one.
func stLedgerV2EpochExtenders(epoch *ledger.EpochAllocation) *model.StLedgerV2EpochExtenders {
	if epoch.Extenders == nil {
		return nil
	}
	census := make([]string, len(epoch.Extenders.Census))
	for i, ref := range epoch.Extenders.Census {
		census[i] = ref.String()
	}
	return &model.StLedgerV2EpochExtenders{Bytes: epoch.Extenders.Bytes, CensusChunks: census}
}
