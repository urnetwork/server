package controller

// st_ctf_shadow.go — the shadow run (sn CAPTURETHEFLAG.md §11.2, step 6).
// After an epoch ends, the server draws its census and builds the CTF ledger
// exactly as it would live, without committing anything, and publishes an
// unsigned report that the validator's audit reads. It reaches the chain only
// through views (stCtfShadowChain), so it cannot send a transaction, and it
// records no fixed input: a shadow epoch's awards never bind a live root.
//
// Nothing is committed, so the run stands in for the chain's facts: the
// census block B_k is end(k) + w/2, the last block a commitment could use,
// with its EVM timestamp, and ρ_k follows from it as live. Each report is a
// pool's first root over its one epoch, with no returned carry. Its funding
// is the CTF pool's own when vault2 has captured the epoch for it, and
// otherwise a tenth of the provider pool's f_k, CTF's share of μ (§2.1). Every
// input is fixed once it exists, so a retry publishes the same bytes.
//
// The report (schema urnetwork-ctf-shadow-v1) maps one to one onto
// ctf.VerifyInput (stCtfShadowVerifyInput), and is served, with every object it
// names, by GET /sn/ledger/object; its reference is the epoch's ctfshadow
// publish row's reason and is logged.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

const stCtfShadowSchema = "urnetwork-ctf-shadow-v1"

// The funding bases of a shadow report.
const (
	stCtfShadowBasisPool          = "pool"
	stCtfShadowBasisProviderTenth = "provider_tenth"
)

// stCtfShadowChain is the chain as a shadow run reads it: views only.
type stCtfShadowChain interface {
	LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error)
	LedgerV2EpochWindow(ctx context.Context, epoch uint64) (uint64, uint64, error)
	LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error)
	CtfBlockTime(ctx context.Context, block uint64) (uint64, error)
}

// stCtfViews narrows the chain clients to stCtfShadowChain.
type stCtfViews struct {
	client StLedgerV2Client
	ctf    StCtfClient
}

func (self stCtfViews) LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error) {
	return self.client.LedgerV2State(ctx, noId)
}

func (self stCtfViews) LedgerV2EpochWindow(ctx context.Context, epoch uint64) (uint64, uint64, error) {
	return self.client.LedgerV2EpochWindow(ctx, epoch)
}

func (self stCtfViews) LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error) {
	return self.client.LedgerV2FundedThrough(ctx, noId, epoch)
}

func (self stCtfViews) CtfBlockTime(ctx context.Context, block uint64) (uint64, error) {
	return self.ctf.CtfBlockTime(ctx, block)
}

// stCtfShadowCensusBlock stands in for the census commitment's block: the
// last block of its window, end(k) + w/2.
func stCtfShadowCensusBlock(end uint64, window uint64) uint64 {
	return end + window/2
}

// stCtfShadowFunding is a shadow epoch's funding f_k and its basis: the CTF
// pool's own when vault2 has captured the epoch for it, else a tenth of the
// provider pool's, read once its close is final. wait says why it cannot be
// read yet.
func stCtfShadowFunding(ctx context.Context, cfg *StConfig, chain stCtfShadowChain, epoch uint64, end uint64, window uint64) (funding uint256.Int, basis string, provider uint256.Int, wait string, err error) {
	read := func(noId uint64, first uint64) (uint256.Int, error) {
		var f uint256.Int
		through, err := chain.LedgerV2FundedThrough(ctx, noId, epoch)
		if err != nil {
			return f, err
		}
		var before uint256.Int
		if first < epoch {
			if before, err = chain.LedgerV2FundedThrough(ctx, noId, epoch-1); err != nil {
				return f, err
			}
		}
		if through.Lt(&before) {
			return f, fmt.Errorf("st ctf: pool %d's fundedThrough decreases at epoch %d", noId, epoch)
		}
		f.Sub(&through, &before)
		return f, nil
	}
	pool, err := chain.LedgerV2State(ctx, ctf.PoolID(cfg.NoId))
	if err != nil {
		return funding, "", provider, "", err
	}
	if pool.Registered && pool.FirstEpoch <= epoch && epoch <= pool.LastFundedEpoch {
		funding, err = read(ctf.PoolID(cfg.NoId), pool.FirstEpoch)
		return funding, stCtfShadowBasisPool, provider, "", err
	}
	providers, err := chain.LedgerV2State(ctx, cfg.NoId)
	if err != nil {
		return funding, "", provider, "", err
	}
	switch {
	case !providers.Registered || epoch < providers.FirstEpoch:
	case providers.LastFundedEpoch < epoch && providers.HeadBlock <= end+window:
		return funding, "", provider, fmt.Sprintf("the provider pool's epoch %d is not closed yet", epoch), nil
	case providers.LastFundedEpoch < epoch:
		// never closed: the next close defers it, and it funds nothing
	default:
		if provider, err = read(cfg.NoId, providers.FirstEpoch); err != nil {
			return funding, "", provider, "", err
		}
	}
	funding.Div(&provider, uint256.NewInt(10))
	return funding, stCtfShadowBasisProviderTenth, provider, "", nil
}

// stCtfShadowCarry is the carry contract a shadow report names: the configured
// one, else the signed entry's for the epoch or the next, before which a shadow
// epoch usually falls.
func stCtfShadowCarry(ctx context.Context, cfg *StConfig, epoch uint64) (common.Address, error) {
	if cfg.Ctf.CarryAddress != (common.Address{}) {
		return cfg.Ctf.CarryAddress, nil
	}
	for _, k := range []uint64{epoch, epoch + 1} {
		entry, err := stCtfInForce(ctx, cfg, k)
		if err != nil {
			return common.Address{}, err
		}
		if carry := stCtfCarryAddress(cfg, entry); carry != (common.Address{}) {
			return carry, nil
		}
	}
	return common.Address{}, nil
}

// stCtfRunShadow builds and publishes epoch k's shadow report.
func stCtfRunShadow(ctx context.Context, cfg *StConfig, chain stCtfShadowChain, records stCtfRecords, epoch uint64) (*StPublishOutcome, error) {
	kind := model.StPublishKindCtfShadow
	if status, reason := records.Outcome(ctx, epoch, kind); status != "" {
		return &StPublishOutcome{Status: status, Reason: reason}, nil
	}
	state, err := chain.LedgerV2State(ctx, cfg.NoId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	end, window, err := chain.LedgerV2EpochWindow(ctx, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	block := stCtfShadowCensusBlock(end, window)
	if state.HeadBlock < block {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("the shadow census block of epoch %d is block %d", epoch, block), block), nil
	}
	census, err := stCtfCensusOf(ctx, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if census == nil {
		next, _, err := chain.LedgerV2EpochWindow(ctx, epoch+1)
		if err != nil {
			return stLedgerV2Retry(err), nil
		}
		if next <= state.HeadBlock {
			return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusMissed, "no census of epoch %d was built before epoch %d ended", epoch, epoch+1), nil
		}
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("the census of epoch %d is not built yet", epoch), 0), nil
	}
	carry, err := stCtfShadowCarry(ctx, cfg, epoch)
	if err != nil {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("capture the flag's terms are unknown: %s", err), 0), nil
	}
	if carry == (common.Address{}) {
		stCtfAlert(cfg, StCtfAlertConfig, fmt.Sprintf("shadow epoch %d", epoch), "no carry contract is known for the shadow run of epoch %d: set ctf.carry_address", epoch)
		return stLedgerV2Wait(cfg, state, "no carry contract is known", 0), nil
	}
	funding, basis, provider, wait, err := stCtfShadowFunding(ctx, cfg, chain, epoch, end, window)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if wait != "" {
		return stLedgerV2Wait(cfg, state, wait, 0), nil
	}
	if funding.IsZero() {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "epoch %d funds nothing (%s), so it has no shadow root", epoch, basis), nil
	}
	blockTime, err := chain.CtfBlockTime(ctx, block)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	round := ctf.DrawRound(blockTime)
	pulse, err := stCtfDrandPulse(ctx, cfg.Ctf.DrandUrls, round)
	if errors.Is(err, errStCtfPulseNotYet) {
		return stCtfWaitUntil(err.Error(), stCtfRoundTime(round).Add(time.Second)), nil
	}
	if err != nil {
		stCtfDrandAlert(cfg, err)
		return stLedgerV2Retry(err), nil
	}
	input := ctf.EpochInput{Epoch: epoch, Census: &ctf.CensusInput{Hash: census.Hash, Block: block, Time: blockTime, Objects: census.Objects}, Pulse: pulse, Coldkeys: census.Coldkeys}
	if decoded, err := ctf.DecodeCensus(census.Objects[census.Hash], census.Objects); err == nil {
		clientIds := make([][16]byte, 0, len(decoded.Candidates))
		for _, candidate := range decoded.Candidates {
			clientIds = append(clientIds, candidate.ClientID)
		}
		if input.Bound, err = stCtfBoundFor(ctx, cfg, epoch, clientIds); err != nil {
			return stLedgerV2Retry(err), nil
		}
	}
	in := ctf.BuildInput{NoID: cfg.NoId, ChainID: cfg.ChainId, Netuid: uint16(cfg.Netuid), Epoch: epoch, FirstEpoch: epoch,
		CarryColdkey: stCtfCarryColdkey(carry), Epochs: []ctf.EpochInput{input}, FundedThrough: map[uint64]uint256.Int{epoch: funding}}
	built, err := ctf.Build(ctx, in)
	if err != nil {
		stCtfAlert(cfg, StCtfAlertBuild, fmt.Sprintf("shadow epoch %d", epoch), "the shadow root of epoch %d cannot be built: %s", epoch, err)
		return stLedgerV2Retry(err), nil
	}
	if err := stCtfPublish(ctx, cfg, nil, &in, built); err != nil {
		return stLedgerV2Retry(err), nil
	}
	report, err := stCtfShadowReportOf(cfg, &in, built, carry, basis, &provider)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	ref, err := PutStLedgerV2Object(ctx, report)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	row, err := stCtfLedgerRow(&in, built, true, report)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	records.CreateLedger(ctx, row)
	stCtfReport(cfg, built, "shadow ")
	stCtfLastEpoch.WithLabelValues("shadow").Set(float64(epoch))
	glog.Infof("[st]ctf shadow epoch %d report %s\n", epoch, ref)
	return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusConfirmed, "report %s", ref), nil
}

// The shadow report's parts, in the order they are written.
type stCtfShadowRoot struct {
	Epoch      uint64 `json:"epoch"`
	Root       string `json:"root"`
	Total      string `json:"total"`
	LeafCount  uint64 `json:"leaf_count"`
	LedgerHash string `json:"ledger_hash"`
}

type stCtfShadowCensus struct {
	Hash  string `json:"hash"`
	Block uint64 `json:"block"`
	Time  uint64 `json:"time"`
}

type stCtfShadowPulse struct {
	Round     uint64 `json:"round"`
	Signature string `json:"signature"`
}

type stCtfShadowEpoch struct {
	Epoch    uint64             `json:"epoch"`
	Census   *stCtfShadowCensus `json:"census"`
	Pulse    *stCtfShadowPulse  `json:"pulse"`
	Recycled string             `json:"recycled"`
}

type stCtfShadowFundingBasis struct {
	Basis           string `json:"basis"`
	ProviderFunding string `json:"provider_funding"`
}

type stCtfShadowSummary struct {
	Candidates int    `json:"candidates"`
	Drawn      bool   `json:"drawn"`
	Winners    int    `json:"winners"`
	Fresh      string `json:"fresh"`
	Slot       string `json:"slot"`
	Paid       string `json:"paid"`
	Pending    string `json:"pending"`
	Carried    string `json:"carried"`
}

type stCtfShadowReport struct {
	Schema        string                  `json:"schema"`
	ChainId       uint64                  `json:"chain_id"`
	Netuid        uint16                  `json:"netuid"`
	NoId          uint64                  `json:"no_id"`
	PoolId        uint64                  `json:"pool_id"`
	Epoch         uint64                  `json:"epoch"`
	FirstEpoch    uint64                  `json:"first_epoch"`
	CarryColdkey  string                  `json:"carry_coldkey"`
	CarryContract *string                 `json:"carry_contract"`
	Previous      *stCtfShadowRoot        `json:"previous"`
	Pending       stCtfShadowRoot         `json:"pending"`
	FundedThrough map[string]string       `json:"funded_through"`
	Funding       stCtfShadowFundingBasis `json:"funding"`
	Epochs        []stCtfShadowEpoch      `json:"epochs"`
	Summary       stCtfShadowSummary      `json:"summary"`
}

func stCtfHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

// stCtfShadowReportOf encodes a shadow build's report: compact JSON, keys in
// declaration order, amounts as decimal strings of rao.
func stCtfShadowReportOf(cfg *StConfig, in *ctf.BuildInput, built *ctf.Ledger, carry common.Address, basis string, provider *uint256.Int) ([]byte, error) {
	carryContract := stCtfHex(carry.Bytes())
	report := &stCtfShadowReport{
		Schema: stCtfShadowSchema, ChainId: in.ChainID, Netuid: in.Netuid, NoId: in.NoID, PoolId: ctf.PoolID(in.NoID),
		Epoch: in.Epoch, FirstEpoch: in.FirstEpoch, CarryColdkey: stCtfHex(in.CarryColdkey[:]), CarryContract: &carryContract,
		Pending: stCtfShadowRoot{Epoch: built.Commit.Epoch, Root: stCtfHex(built.Commit.Root[:]), Total: built.Commit.Total.Dec(),
			LeafCount: built.Commit.LeafCount, LedgerHash: stCtfHex(built.Commit.LedgerHash[:])},
		FundedThrough: map[string]string{},
		Funding:       stCtfShadowFundingBasis{Basis: basis, ProviderFunding: provider.Dec()},
	}
	for k, through := range in.FundedThrough {
		report.FundedThrough[strconv.FormatUint(k, 10)] = through.Dec()
	}
	for _, input := range in.Epochs {
		epoch := stCtfShadowEpoch{Epoch: input.Epoch, Recycled: input.Recycled.Dec()}
		if input.Census != nil {
			epoch.Census = &stCtfShadowCensus{Hash: input.Census.Hash.String(), Block: input.Census.Block, Time: input.Census.Time}
		}
		if input.Pulse != nil {
			epoch.Pulse = &stCtfShadowPulse{Round: input.Pulse.Round, Signature: stCtfHex(input.Pulse.Signature)}
		}
		report.Epochs = append(report.Epochs, epoch)
	}
	if n := len(built.Epochs); n != 0 {
		last := built.Epochs[n-1]
		slot, paid := last.SlotAmount(ctf.NumSlots-1), last.Paid()
		report.Summary = stCtfShadowSummary{Candidates: last.Candidates, Drawn: last.Drawn, Winners: len(last.Awards),
			Fresh: last.Fresh.Dec(), Slot: slot.Dec(), Paid: paid.Dec(), Pending: last.PendingTotal.Dec(), Carried: last.Carried.Dec()}
	}
	return json.Marshal(report)
}

// stCtfShadowVerifyInput reads a shadow report back into the chain facts of
// ctf.VerifyInput, as the validator's audit does; fetcher serves the objects
// it names.
func stCtfShadowVerifyInput(data []byte, fetcher ledger.Fetcher) (*ctf.VerifyInput, error) {
	var report stCtfShadowReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	if report.Schema != stCtfShadowSchema || report.Netuid == 0 || report.PoolId != ctf.PoolID(report.NoId) {
		return nil, fmt.Errorf("st ctf: not a shadow report of a capture the flag pool")
	}
	bytes32 := func(s string) ([32]byte, error) {
		var out [32]byte
		encoded, ok := strings.CutPrefix(s, "0x")
		decoded, err := hex.DecodeString(encoded)
		if !ok || err != nil || len(decoded) != 32 {
			return out, fmt.Errorf("st ctf: %q is not 0x and 32 bytes", s)
		}
		copy(out[:], decoded)
		return out, nil
	}
	amount := func(s string) (uint256.Int, error) {
		value, err := uint256.FromDecimal(s)
		if err != nil {
			return uint256.Int{}, err
		}
		return *value, nil
	}
	in := &ctf.VerifyInput{NoID: report.NoId, ChainID: report.ChainId, Netuid: report.Netuid, FirstEpoch: report.FirstEpoch,
		FundedThrough: map[uint64]uint256.Int{}, Epochs: map[uint64]ctf.EpochFacts{}, Fetcher: fetcher, Custody: ledger.NewMemoryCustody()}
	var err error
	if in.CarryColdkey, err = bytes32(report.CarryColdkey); err != nil {
		return nil, err
	}
	root := func(r *stCtfShadowRoot) (*ledger.Root, error) {
		out := &ledger.Root{Epoch: r.Epoch, LeafCount: r.LeafCount}
		var err error
		if out.Root, err = bytes32(r.Root); err != nil {
			return nil, err
		}
		if out.LedgerHash, err = bytes32(r.LedgerHash); err != nil {
			return nil, err
		}
		out.Total, err = amount(r.Total)
		return out, err
	}
	pending, err := root(&report.Pending)
	if err != nil {
		return nil, err
	}
	in.Pending = *pending
	if report.Previous != nil {
		if in.Previous, err = root(report.Previous); err != nil {
			return nil, err
		}
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
	for _, e := range report.Epochs {
		facts := ctf.EpochFacts{}
		if facts.Recycled, err = amount(e.Recycled); err != nil {
			return nil, err
		}
		if e.Census != nil {
			hash, err := ledger.ParseRef(e.Census.Hash)
			if err != nil {
				return nil, err
			}
			facts.Census = &ctf.CensusCommitment{Hash: hash, Block: e.Census.Block, Time: e.Census.Time}
		}
		if e.Pulse != nil {
			encoded, ok := strings.CutPrefix(e.Pulse.Signature, "0x")
			signature, err := hex.DecodeString(encoded)
			if !ok || err != nil {
				return nil, fmt.Errorf("st ctf: pulse signature %q is not 0x hex", e.Pulse.Signature)
			}
			facts.Pulse = &ctf.Pulse{Round: e.Pulse.Round, Signature: signature}
		}
		in.Epochs[e.Epoch] = facts
	}
	return in, nil
}
