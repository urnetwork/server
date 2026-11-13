package controller

// st_ledger_v2_extender_shadow.go — the SN25 extender pool's live shadow runs
// (sn WHITEPAPER.md §8.6.7: epochs 6 and 7). Only the two epochs before E_x run
// live in shadow (stExtenderShadowNotLive): an earlier epoch, such as epoch 4,
// gets the offline replay instead (st_ledger_v2_extender_replay.go), and an
// epoch from E_x is built live. After each of the two ends, and once its census
// is captured, its rows are recorded and its close is final, the server builds
// what §8.6 would have paid for it: the epoch read as if E_x were the epoch
// itself, its provider rows without the pooled shares, and its extender
// component from the real census, as a pool's first root over the one epoch
// funded with the epoch's real f_k. It commits nothing, records no fixed input
// and retains no wallet resolution, so a shadow never binds a live root and is
// never paid. It reaches the chain only through views (stExtenderShadowChain).
//
// E_x is the first epoch an extender pool entry puts the amendment in force,
// signed or configured. The entry is signed during epoch 7, after epoch 6's
// shadow is due, so until one is held the deployment's plan stands in
// (stExtenderPoolPlannedFirstEpoch): 8 on mainnet.
//
// The report (schema urnetwork-extender-shadow-v1) maps one to one onto
// ledger.VerifyInput (stExtenderShadowVerifyInput), so the validator's
// recompute, ledger.Verify with the epoch as E_x, runs on it. It is published
// with every object it names, served by GET /sn/ledger/object, recorded once in
// st_ledger_v2_extender_shadow, and logged with its reference. Beside the root it
// carries what review needs: Z's parts, the pooled shares Z left out, W_k, Z',
// M and N, the registered platform endpoints, the chunks of the provider rows
// as recorded, and each coldkey's amount under the rule in force and under §8.6,
// from the same funding.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/holiman/uint256"
	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/demand/usage"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The usage step that builds an epoch's shadow report.
const StLedgerV2UsageStepExtenderShadow = "extender_shadow"

const stExtenderShadowSchema = "urnetwork-extender-shadow-v1"

// Raised once when an epoch's shadow is given up: its read kept running past
// its bound (StLedgerV2GiveUpExtenderShadow).
const StLedgerV2AlertExtenderShadow = "v2_extender_shadow"

// Tunables of a shadow run.
type StLedgerV2ExtenderShadowSettings struct {
	// Close-time span of one slice of the epoch's contracts, each read in its
	// own short REPEATABLE READ snapshot (model.GetStEpochAmendedUsageInSlices),
	// so no snapshot pins the primary for the whole epoch. An hour of a 7-day
	// epoch closes about 2.4M contracts on average, a read of 15 to 20 s, well
	// inside the role's 15-minute transaction_timeout. Non-positive reads the
	// epoch in one snapshot, as a root's build does.
	ReadSliceSpan time.Duration
	// Reads of one slice that may fail in a row, each from nothing, before the
	// run fails and its step retries. Below 1 reads each slice once.
	ReadSliceAttempts int
	// Wait before a failed slice is read again.
	ReadSliceRetryDelay time.Duration
}

// Hour-long slices, each tried three times, 15 s apart.
func DefaultStLedgerV2ExtenderShadowSettings() *StLedgerV2ExtenderShadowSettings {
	return &StLedgerV2ExtenderShadowSettings{
		ReadSliceSpan:       1 * time.Hour,
		ReadSliceAttempts:   3,
		ReadSliceRetryDelay: 15 * time.Second,
	}
}

// The epoch read's slicing.
func (self *StLedgerV2ExtenderShadowSettings) slicing() model.StEpochUsageSlices {
	return model.StEpochUsageSlices{Span: self.ReadSliceSpan, Attempts: self.ReadSliceAttempts, RetryDelay: self.ReadSliceRetryDelay}
}

// The chain as a shadow run reads it: views only.
type stExtenderShadowChain interface {
	LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error)
	LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error)
}

// The epochs before E_x that run live in shadow (sn WHITEPAPER.md §8.6.7, "The
// shadow runs": epochs 6 and 7 for E_x = 8).
const stExtenderShadowLiveEpochs = 2

// The extender pool's planned first epoch, by deployment, for the epochs before
// an amendment entry names E_x: sn WHITEPAPER.md §8.6.7's rollout, E_x = 8 on
// mainnet (chain 964, sn 25, no 1), as the owner decided on 2026-11-10. The
// entry that fixes E_x is signed during epoch 7, after epoch 6's live shadow is
// due, so until it is held the plan says which epochs run in shadow; an entry
// held, signed or configured, says it from then on. The plan never puts the
// amendment in force: only an entry does (st_ledger_v2_amendments.go). A
// deployment with no plan has no live shadow until an entry names E_x. A seam
// the tests replace.
var stExtenderPoolPlannedFirstEpoch = func(chainId uint64, netuid uint64, noId uint64) (firstEpoch uint64, planned bool) {
	if chainId == 964 && netuid == 25 && noId == 1 {
		return 8, true
	}
	return 0, false
}

// E_x as the shadow rule reads it: the first epoch any extender pool entry for
// the operator puts the amendment in force, signed or configured, as
// amendment.File.At answers, and before any entry names one, the deployment's
// plan. named reports that an entry fixed it; known is false with neither.
func stExtenderPoolFirstEpoch(cfg *StConfig, file *amendment.File, fallback *amendment.Fallback) (firstEpoch uint64, named bool, known bool) {
	for _, entry := range append(file.Entries(), fallback.Entries()...) {
		if entry.Kind() == amendment.KindExtenders && entry.NoID == cfg.NoId && (!named || entry.FromEpoch < firstEpoch) {
			firstEpoch, named = entry.FromEpoch, true
		}
	}
	if named {
		return firstEpoch, true, true
	}
	firstEpoch, known = stExtenderPoolPlannedFirstEpoch(cfg.ChainId, cfg.Netuid, cfg.NoId)
	return firstEpoch, false, known
}

// Says why epoch has no live shadow, and "" for one of the
// stExtenderShadowLiveEpochs epochs before E_x, which runs live in shadow. An
// epoch from a planned E_x that no entry names yet has none either: the start
// has moved, and the two epochs before the one an entry names later are the
// shadow runs. It reads nothing: file and fallback are the amendments the
// caller holds.
func stExtenderShadowNotLive(cfg *StConfig, file *amendment.File, fallback *amendment.Fallback, epoch uint64) string {
	firstEpoch, named, known := stExtenderPoolFirstEpoch(cfg, file, fallback)
	source := "as planned; no amendment names it yet"
	if named {
		source = "as the amendments name it"
	}
	switch {
	case !known:
		return fmt.Sprintf("no amendment names the extender pool's first epoch and none is planned for this deployment, so epoch %d has no live shadow", epoch)
	case named && firstEpoch <= epoch:
		return fmt.Sprintf("epoch %d is covered by the extender pool amendment from epoch %d, so its component is built live and it has no shadow", epoch, firstEpoch)
	case firstEpoch <= epoch:
		return fmt.Sprintf("epoch %d is not before the extender pool's planned first epoch %d, which no amendment names yet, so it has no live shadow", epoch, firstEpoch)
	case epoch+stExtenderShadowLiveEpochs < firstEpoch:
		return fmt.Sprintf("only the %d epochs before the extender pool's first epoch %d (%s) run live in shadow, so epoch %d has none; an earlier epoch has the offline replay", stExtenderShadowLiveEpochs, firstEpoch, source, epoch)
	}
	return ""
}

// The epochs whose shadow run concluded without a report in this process: one
// that does not run live in shadow, or one with nothing to report. The sync
// plans them no more; a restart reads each once more.
var stExtenderShadowConcluded sync.Map

// The key a shadow step concludes under, by deployment, operator and epoch.
func stExtenderShadowKey(cfg *StConfig, epoch uint64) string {
	return fmt.Sprintf("%s|%d|%d", cfg.DeploymentKey(), cfg.NoId, epoch)
}

// Reports whether an epoch's shadow report is still to be built: it is one of
// the two epochs before E_x, by the amendments this server holds
// (stLedgerV2HeldAmendments, which fetches nothing), its census is captured,
// its live root has recorded its rows without an extender component, and no
// report is recorded. Every other epoch plans nothing, before any of its records
// is read. An epoch whose rows carry a component is one the amendment covers,
// built live; one declared unrecoverable has nothing to shadow. A failed read,
// such as a table not migrated yet, is logged and plans nothing.
func stLedgerV2ExtenderShadowDue(ctx context.Context, cfg *StConfig, epoch uint64) (due bool) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]v2 epoch %d extender shadow read failed: %v\n", epoch, r)
			due = false
		}
	}()
	if _, concluded := stExtenderShadowConcluded.Load(stExtenderShadowKey(cfg, epoch)); concluded {
		return false
	}
	file, fallback, err := stLedgerV2HeldAmendments(ctx, cfg)
	if err != nil {
		glog.Errorf("[st]v2 epoch %d extender shadow: the amendments held: %s\n", epoch, stScrubEndpoints(err.Error()))
		return false
	}
	if stExtenderShadowNotLive(cfg, file, fallback, epoch) != "" {
		return false
	}
	key := cfg.DeploymentKey()
	rows := model.GetStLedgerV2EpochRows(ctx, key, cfg.NoId, epoch, epoch)[epoch]
	if rows == nil || rows.Null() || rows.Extenders != nil {
		return false
	}
	return model.GetStLedgerV2ExtenderCensus(ctx, key, cfg.NoId, epoch) != nil && model.GetStLedgerV2ExtenderShadow(ctx, key, cfg.NoId, epoch) == nil
}

// Concludes an epoch's shadow step without a report, saying why.
func stExtenderShadowConclude(cfg *StConfig, epoch uint64, format string, args ...any) *StPublishOutcome {
	stExtenderShadowConcluded.Store(stExtenderShadowKey(cfg, epoch), true)
	return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf(format, args...)}
}

// Gives up an epoch's shadow once timeouts of its chain's attempts ran past
// bound, each a full read cut short: it alerts once, records the count in the
// shadow phase's gauges, and the sync of this process plans the epoch no more.
// The step's chain holds the epoch past its sync window, so no read starts
// again after a restart (taskworker/work/st_ledger_v2_usage_work.go). Nothing
// is recorded for the epoch; its shadow is missed, as §8.6.7 allows for epoch
// 6 ("epoch 7 alone is the live shadow").
func StLedgerV2GiveUpExtenderShadow(epoch uint64, timeouts int, bound time.Duration) {
	cfg, _, err := stRequire()
	if err != nil {
		glog.Errorf("[st]v2 epoch %d extender shadow given up after %d timed-out attempts: %s\n", epoch, timeouts, err)
		return
	}
	stExtenderShadowConcluded.Store(stExtenderShadowKey(cfg, epoch), true)
	stExtenderPoolCounts.WithLabelValues("shadow", "timed_out_attempts").Set(float64(timeouts))
	stLedgerV2Alert(cfg, StLedgerV2AlertExtenderShadow, fmt.Sprintf("epoch %d", epoch),
		"epoch %d's extender shadow read ran past %s in %d attempts, so its shadow is given up: no report is built, and no read of it starts again", epoch, bound, timeouts)
}

// Builds, publishes and records epoch's shadow report. An epoch that does not
// run live in shadow concludes first, by the amendments this server holds,
// before any of its records, the chain or the published amendments are read;
// that is how a step an earlier binary planned for such an epoch ends. It
// waits for the epoch's rows to be recorded by its live root and for its
// close, and concludes without a report for an epoch the published amendments
// put outside the shadow runs, one declared unrecoverable, or one with no
// eligible usage. It reads the epoch's contracts in settings' short slices.
func stLedgerV2RunExtenderShadow(ctx context.Context, cfg *StConfig, chain stExtenderShadowChain, epoch uint64, settings *StLedgerV2ExtenderShadowSettings) *StPublishOutcome {
	file, fallback, err := stLedgerV2HeldAmendments(ctx, cfg)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow: the amendments held: %s", epoch, err))
	}
	if notLive := stExtenderShadowNotLive(cfg, file, fallback, epoch); notLive != "" {
		return stExtenderShadowConclude(cfg, epoch, "%s", notLive)
	}
	key := cfg.DeploymentKey()
	if recorded := model.GetStLedgerV2ExtenderShadow(ctx, key, cfg.NoId, epoch); recorded != nil {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d shadow report already recorded: %s", epoch, recorded.ReportRef)}
	}
	amendments, err := stLedgerV2ReadAmendments(ctx, cfg)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow: the amendments in force: %s", epoch, err))
	}
	in, err := amendments.file.At(cfg.NoId, epoch, amendments.fallback)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow: the amendments in force: %s", epoch, err))
	}
	if in.Extenders != nil {
		return stExtenderShadowConclude(cfg, epoch, "epoch %d is covered by the extender pool amendment from epoch %d, so its component is built live and it has no shadow", epoch, in.Extenders.FromEpoch)
	}
	// the published file may name E_x before the stored copy does
	if notLive := stExtenderShadowNotLive(cfg, amendments.file, amendments.fallback, epoch); notLive != "" {
		return stExtenderShadowConclude(cfg, epoch, "%s", notLive)
	}
	census, err := stLedgerV2CapturedCensus(ctx, cfg, epoch)
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	recorded := model.GetStLedgerV2EpochRows(ctx, key, cfg.NoId, epoch, epoch)[epoch]
	switch {
	case recorded == nil:
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d's rows are not recorded by its root yet", epoch))
	case recorded.Null():
		return stExtenderShadowConclude(cfg, epoch, "epoch %d is declared unrecoverable, so it has no shadow", epoch)
	case recorded.Extenders != nil:
		return stExtenderShadowConclude(cfg, epoch, "epoch %d's rows were recorded with an extender component, so it has no shadow", epoch)
	}
	// the provider pool's f_k for the epoch, read once its close is final:
	// fundedThrough[k] − fundedThrough[k−1], the latter 0 at the pool's first
	// epoch; wait says why it cannot be read yet
	readFunding := func() (funding uint256.Int, wait string, err error) {
		state, err := chain.LedgerV2State(ctx, cfg.NoId)
		if err != nil {
			return funding, "", err
		}
		switch {
		case !state.Registered || epoch < state.FirstEpoch:
			return funding, "", fmt.Errorf("epoch %d is not a settlement v2 epoch of pool %d", epoch, cfg.NoId)
		case state.LastFundedEpoch < epoch:
			return funding, fmt.Sprintf("the pool's epoch %d is not closed yet", epoch), nil
		}
		through, err := chain.LedgerV2FundedThrough(ctx, cfg.NoId, epoch)
		if err != nil {
			return funding, "", err
		}
		var before uint256.Int
		if state.FirstEpoch < epoch {
			if before, err = chain.LedgerV2FundedThrough(ctx, cfg.NoId, epoch-1); err != nil {
				return funding, "", err
			}
		}
		if through.Lt(&before) {
			return funding, "", fmt.Errorf("pool %d's fundedThrough decreases at epoch %d", cfg.NoId, epoch)
		}
		funding.Sub(&through, &before)
		return funding, "", nil
	}
	funding, wait, err := readFunding()
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow funding: %s", epoch, err))
	}
	if wait != "" {
		return stLedgerV2UsageRetry(wait)
	}
	platformEndpoints, err := stPlatformEndpoints(ctx)
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	amendment := model.StExtenderAmendment{FirstEpoch: epoch, PlatformEndpoints: platformEndpoints}
	rows, pool, err := stLedgerV2AmendedEpochRows(ctx, epoch, amendment, amendments.pool, false, settings.slicing())
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow rows: %s", epoch, err))
	}
	recordedRows, err := stLedgerV2RecordedRows(ctx, epoch, recorded.ProviderChunks)
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	input := &stExtenderShadowInput{
		ChainId: cfg.ChainId, Netuid: uint16(cfg.Netuid), NoId: cfg.NoId, Epoch: epoch, Funding: funding,
		Rows: rows, RecordedChunks: recorded.ProviderChunks, Recorded: recordedRows, Pool: pool, Census: census,
		PlatformEndpoints: platformEndpoints.Count(),
	}
	report, shadow, err := stExtenderShadowBuild(ctx, input)
	if errors.Is(err, ledger.ErrEpochNotAllocated) {
		return stExtenderShadowConclude(cfg, epoch, "epoch %d has no eligible usage under the amendment, so it has no shadow root", epoch)
	}
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow build: %s", epoch, err))
	}
	// the chunks before the header, the header before the report
	refs := make([]ledger.Ref, 0, len(shadow.Objects))
	header := ledger.Ref(shadow.Commit.LedgerHash)
	for ref := range shadow.Objects {
		if ref != header {
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(refs, func(a, b ledger.Ref) int { return bytes.Compare(a[:], b[:]) })
	for _, ref := range append(refs, header) {
		if _, err := PutStLedgerV2Object(ctx, shadow.Objects[ref]); err != nil {
			return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow object: %s", epoch, err))
		}
	}
	reportRef, err := PutStLedgerV2Object(ctx, report)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d shadow report: %s", epoch, err))
	}
	kept := model.RecordStLedgerV2ExtenderShadow(ctx, key, cfg.NoId, epoch, reportRef, report)
	split := &shadow.Epochs[0].Extenders.ExtenderSplit
	stExtenderPoolMetrics("shadow", epoch, pool, split, ledger.EligibleUsage(rows), platformEndpoints.InForce(server.NowUtc()))
	glog.Infof("[st]v2 epoch %d extender shadow report %s: Z = %d bytes (%d intra-platform, %d intra-network), Z' = %d, N = %d in M = %d units, f_k = %s rao\n",
		epoch, kept.ReportRef, pool.Bytes(), pool.IntraPlatform, pool.IntraNetwork, split.Weight, split.Eligible, split.Units, funding.Dec())
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d shadow report %s", epoch, kept.ReportRef)}
}

// Reads an epoch's recorded provider rows from their published chunks.
func stLedgerV2RecordedRows(ctx context.Context, epoch uint64, refs []string) ([]protocol.LedgerUsageRow, error) {
	chunks := make([][]byte, len(refs))
	for i, ref := range refs {
		chunk, err := GetStLedgerV2Object(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("st ledger v2: epoch %d recorded chunk %d: %w", epoch, i, err)
		}
		chunks[i] = chunk
	}
	rows, err := ledger.DecodeProviderChunks(chunks)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: epoch %d recorded rows: %w", epoch, err)
	}
	return rows, nil
}

// Everything a shadow report is built from.
type stExtenderShadowInput struct {
	ChainId uint64
	Netuid  uint16
	NoId    uint64
	Epoch   uint64
	// the epoch's real f_k.
	Funding uint256.Int
	// the epoch's provider rows under the amendment, without the pooled
	// shares, and Pool the epoch reader's pool.
	Rows []protocol.LedgerUsageRow
	Pool *model.StEpochExtenderPool
	// the epoch's rows as its live root recorded them, and RecordedChunks their
	// chunks.
	Recorded       []protocol.LedgerUsageRow
	RecordedChunks []string
	Census         []ledger.CensusRow
	// the number of registrations read
	PlatformEndpoints int
}

// The shadow report's parts, in the order they are written.
type stExtenderShadowRoot struct {
	Epoch      uint64 `json:"epoch"`
	Root       string `json:"root"`
	Total      string `json:"total"`
	LeafCount  uint64 `json:"leaf_count"`
	LedgerHash string `json:"ledger_hash"`
}

// The pooled bytes Z leaves out, as the report states them.
type stExtenderShadowLeftOut struct {
	UnfundedBytes        uint64 `json:"unfunded_bytes"`
	UnfundedContracts    uint64 `json:"unfunded_contracts"`
	MeasurementBytes     uint64 `json:"measurement_bytes"`
	MeasurementContracts uint64 `json:"measurement_contracts"`
}

// One coldkey's amounts in the report: under the shadow and as recorded.
type stExtenderShadowColdkey struct {
	Coldkey string `json:"coldkey"`
	// the coldkey's amount from the epoch's rows as recorded, the
	// rule in force; Shadow its amount under §8.6. Both are from the same f_k.
	Recorded string `json:"recorded"`
	Shadow   string `json:"shadow"`
}

// The shadow report as published: compact JSON, keys in declaration order.
type stExtenderShadowReport struct {
	Schema        string               `json:"schema"`
	ChainId       uint64               `json:"chain_id"`
	Netuid        uint16               `json:"netuid"`
	NoId          uint64               `json:"no_id"`
	Epoch         uint64               `json:"epoch"`
	FirstEpoch    uint64               `json:"first_epoch"`
	ExtendersFrom uint64               `json:"extenders_from"`
	Pending       stExtenderShadowRoot `json:"pending"`
	FundedThrough map[string]string    `json:"funded_through"`
	// the shadow's provider rows, and RecordedChunks the epoch's rows as its live
	// root recorded them.
	ProviderChunks        []string                  `json:"provider_chunks"`
	RecordedChunks        []string                  `json:"recorded_chunks"`
	EligibleProviderBytes uint64                    `json:"eligible_provider_bytes"`
	ExtenderPool          *usage.ExtenderPool       `json:"extender_pool"`
	LeftOut               stExtenderShadowLeftOut   `json:"left_out"`
	PlatformEndpoints     int                       `json:"platform_endpoints"`
	Coldkeys              []stExtenderShadowColdkey `json:"coldkeys"`
}

// Bytes as 0x-prefixed lowercase hex.
func stExtenderShadowHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

// Builds a shadow epoch's ledger, a pool's first root over the one epoch, and
// its report: compact JSON, keys in declaration order, amounts as decimal
// strings of rao. The rows as recorded are built from the same funding without
// a component, for each coldkey's amount under the rule in force.
func stExtenderShadowBuild(ctx context.Context, in *stExtenderShadowInput) ([]byte, *ledger.Ledger, error) {
	if in.Pool == nil {
		return nil, nil, errors.New("st ledger v2: a shadow needs the epoch's extender pool")
	}
	epochFundedThroughs := map[uint64]uint256.Int{in.Epoch: in.Funding}
	shadow, err := ledger.Build(ctx, ledger.BuildInput{NoID: in.NoId, Epoch: in.Epoch, FirstEpoch: in.Epoch, FundedThrough: epochFundedThroughs,
		Epochs: []ledger.EpochInput{{Epoch: in.Epoch, Rows: in.Rows, Extenders: &ledger.ExtenderInput{Bytes: in.Pool.Bytes(), Census: in.Census}}}})
	if err != nil {
		return nil, nil, err
	}
	coldkeyAmounts := map[[32]byte]*stExtenderShadowColdkey{}
	add := func(deltas []protocol.LedgerDelta, shadow bool) {
		for _, delta := range deltas {
			entry := coldkeyAmounts[delta.Coldkey]
			if entry == nil {
				entry = &stExtenderShadowColdkey{Coldkey: stExtenderShadowHex(delta.Coldkey[:]), Recorded: "0", Shadow: "0"}
				coldkeyAmounts[delta.Coldkey] = entry
			}
			if shadow {
				entry.Shadow = delta.Delta.Dec()
			} else {
				entry.Recorded = delta.Delta.Dec()
			}
		}
	}
	add(shadow.Epochs[0].Deltas, true)
	recorded, err := ledger.Build(ctx, ledger.BuildInput{NoID: in.NoId, Epoch: in.Epoch, FirstEpoch: in.Epoch, FundedThrough: epochFundedThroughs,
		Epochs: []ledger.EpochInput{{Epoch: in.Epoch, Rows: in.Recorded}}})
	switch {
	case err == nil:
		add(recorded.Epochs[0].Deltas, false)
	case !errors.Is(err, ledger.ErrEpochNotAllocated):
		return nil, nil, err
	}
	allocation := shadow.Epochs[0]
	component := allocation.Extenders
	header := &ledger.HeaderExtenders{Bytes: component.Bytes, Census: component.Census, Rows: component.Chunks}
	committed, err := usage.CommittedExtenders(header, in.Census, component.Rows)
	if err != nil {
		return nil, nil, err
	}
	pool, err := usage.NewExtenderPool(uint64(in.Pool.IntraPlatform), uint64(in.Pool.IntraNetwork), in.Pool.Contracts, in.Pool.Payers, committed)
	if err != nil {
		return nil, nil, err
	}
	providerChunks := make([]string, len(allocation.Providers))
	for i, ref := range allocation.Providers {
		providerChunks[i] = ref.String()
	}
	report := &stExtenderShadowReport{
		Schema: stExtenderShadowSchema, ChainId: in.ChainId, Netuid: in.Netuid, NoId: in.NoId, Epoch: in.Epoch, FirstEpoch: in.Epoch, ExtendersFrom: in.Epoch,
		Pending: stExtenderShadowRoot{Epoch: shadow.Commit.Epoch, Root: stExtenderShadowHex(shadow.Commit.Root[:]), Total: shadow.Commit.Total.Dec(),
			LeafCount: shadow.Commit.LeafCount, LedgerHash: stExtenderShadowHex(shadow.Commit.LedgerHash[:])},
		FundedThrough:         map[string]string{strconv.FormatUint(in.Epoch, 10): in.Funding.Dec()},
		ProviderChunks:        providerChunks,
		RecordedChunks:        append([]string{}, in.RecordedChunks...),
		EligibleProviderBytes: ledger.EligibleUsage(in.Rows),
		ExtenderPool:          pool,
		LeftOut: stExtenderShadowLeftOut{UnfundedBytes: uint64(in.Pool.UnfundedBytes), UnfundedContracts: in.Pool.UnfundedContracts,
			MeasurementBytes: uint64(in.Pool.MeasurementBytes), MeasurementContracts: in.Pool.MeasurementContracts},
		PlatformEndpoints: in.PlatformEndpoints,
		Coldkeys:          make([]stExtenderShadowColdkey, 0, len(coldkeyAmounts)),
	}
	coldkeys := make([][32]byte, 0, len(coldkeyAmounts))
	for coldkey := range coldkeyAmounts {
		coldkeys = append(coldkeys, coldkey)
	}
	slices.SortFunc(coldkeys, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	for _, coldkey := range coldkeys {
		report.Coldkeys = append(report.Coldkeys, *coldkeyAmounts[coldkey])
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, nil, err
	}
	return encoded, shadow, nil
}

// Reads a shadow report back into the chain facts of ledger.VerifyInput, as the
// validator's recompute takes them: the pool's first root over the one epoch,
// funded with its f_k, with the epoch as E_x. fetcher serves the objects it
// names. Heads is left to the caller: nil skips check 6.
func stExtenderShadowVerifyInput(data []byte, fetcher ledger.Fetcher) (*ledger.VerifyInput, error) {
	var report stExtenderShadowReport
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return nil, err
	}
	if report.Schema != stExtenderShadowSchema || report.NoId == 0 || report.Epoch == 0 || report.FirstEpoch != report.Epoch || report.ExtendersFrom != report.Epoch {
		return nil, fmt.Errorf("st ledger v2: not an extender shadow report of one epoch")
	}
	bytes32 := func(text string) ([32]byte, error) {
		var out [32]byte
		encoded, ok := strings.CutPrefix(text, "0x")
		decoded, err := hex.DecodeString(encoded)
		if !ok || err != nil || len(decoded) != 32 {
			return out, fmt.Errorf("st ledger v2: %q is not 0x and 32 bytes", text)
		}
		copy(out[:], decoded)
		return out, nil
	}
	amount := func(text string) (uint256.Int, error) {
		value, err := uint256.FromDecimal(text)
		if err != nil {
			return uint256.Int{}, err
		}
		return *value, nil
	}
	in := &ledger.VerifyInput{NoID: report.NoId, FirstEpoch: report.FirstEpoch, ExtendersFrom: report.ExtendersFrom,
		FundedThrough: map[uint64]uint256.Int{}, Fetcher: fetcher, Custody: ledger.NewMemoryCustody()}
	in.Pending = ledger.Root{Epoch: report.Pending.Epoch, LeafCount: report.Pending.LeafCount}
	var err error
	if in.Pending.Root, err = bytes32(report.Pending.Root); err != nil {
		return nil, err
	}
	if in.Pending.LedgerHash, err = bytes32(report.Pending.LedgerHash); err != nil {
		return nil, err
	}
	if in.Pending.Total, err = amount(report.Pending.Total); err != nil {
		return nil, err
	}
	for k, through := range report.FundedThrough {
		epoch, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return nil, err
		}
		if in.FundedThrough[epoch], err = amount(through); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// Narrows the chain clients to the shadow run's views.
type stLedgerV2ExtenderShadowViews struct {
	client StLedgerV2Client
}

// The pool's settlement v2 state, a view.
func (self stLedgerV2ExtenderShadowViews) LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error) {
	return self.client.LedgerV2State(ctx, noId)
}

// The pool's fundedThrough at an epoch, a view.
func (self stLedgerV2ExtenderShadowViews) LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error) {
	return self.client.LedgerV2FundedThrough(ctx, noId, epoch)
}

// An epoch's recorded shadow report for the configured deployment and operator,
// nil before one is recorded.
func GetStLedgerV2ExtenderShadow(ctx context.Context, epoch uint64) (*model.StLedgerV2ExtenderShadow, error) {
	cfg, _, err := stRequire()
	if err != nil {
		return nil, err
	}
	return model.GetStLedgerV2ExtenderShadow(ctx, cfg.DeploymentKey(), cfg.NoId, epoch), nil
}
