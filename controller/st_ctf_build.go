package controller

// st_ctf_build.go — a CTF root's inputs and objects (sn CAPTURETHEFLAG.md
// §4.3 to §5). The driver reads chain facts at the finalized head (the census
// commitments, their blocks' timestamps, vault2's funding, the carry
// contract's returns), the census objects from the ledger object store, the
// pulses from drand, and the operator's attested inputs (each candidate's
// payout coldkey, recorded with the census at end(k)); package ctf does every
// draw and amount. Published objects are put-if-absent, the header last, and an
// epoch's awards reference is recorded at the first root that covers it, so a
// later root reproduces it exactly (§5, fixed inputs).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// StCtfCensus is an epoch's census as the server built it right after end(k)
// (§4.2, §4.3): its hash, what commitCtfCensus commits, its objects (header,
// row chunks, exclusion object) and every candidate's payout coldkey under the
// provider pool's wallet selection (P5, X5), which is never published.
type StCtfCensus struct {
	Epoch    uint64
	Hash     ledger.Ref
	Objects  ledger.Objects
	Coldkeys map[[16]byte][32]byte
}

// StCtfCensusSource reads the census the server built for an epoch, nil while
// it is not built.
type StCtfCensusSource func(ctx context.Context, epoch uint64) (*StCtfCensus, error)

var stCtfCensusSourceLock sync.Mutex
var stCtfCensusSourceInstance StCtfCensusSource

// SetStCtfCensusSource installs the census builder's reader; nil removes it,
// and no census is then ever built.
func SetStCtfCensusSource(source StCtfCensusSource) {
	stCtfCensusSourceLock.Lock()
	defer stCtfCensusSourceLock.Unlock()
	stCtfCensusSourceInstance = source
}

func stCtfCensusOf(ctx context.Context, epoch uint64) (*StCtfCensus, error) {
	stCtfCensusSourceLock.Lock()
	source := stCtfCensusSourceInstance
	stCtfCensusSourceLock.Unlock()
	if source == nil {
		return nil, nil
	}
	census, err := source(ctx, epoch)
	if err != nil || census == nil {
		return nil, err
	}
	if census.Epoch != epoch {
		return nil, fmt.Errorf("st ctf: the census source answered epoch %d for epoch %d", census.Epoch, epoch)
	}
	if header, ok := census.Objects[census.Hash]; !ok || ledger.RefOf(header) != census.Hash {
		return nil, fmt.Errorf("st ctf: the census of epoch %d does not hold its header %v", epoch, census.Hash)
	}
	return census, nil
}

// stCtfBoundFor maps the clients with an active fleet binding at either
// boundary of an epoch (P5, X5) to true, as the provider rows read them
// (stHeadExcludedClients) and the validator's head oracle does. Tests replace
// it.
var stCtfBoundFor = func(ctx context.Context, cfg *StConfig, epoch uint64, clientIds [][16]byte) (map[[16]byte]bool, error) {
	_, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return nil, err
	}
	if authority == nil || authority.Epoch != epoch {
		return nil, fmt.Errorf("st ctf: the payout authority of epoch %d is absent", epoch)
	}
	excluded, err := stHeadExcludedClients(ctx, cfg, client, epoch, authority.Start.Block, authority.End.Block, clientIds)
	if err != nil {
		return nil, err
	}
	bound := make(map[[16]byte]bool, len(excluded))
	for clientId := range excluded {
		bound[clientId] = true
	}
	return bound, nil
}

// stCtfObjectFetcher serves ledger objects from the store, as
// GET /sn/ledger/object does.
type stCtfObjectFetcher struct{}

func (stCtfObjectFetcher) Fetch(ctx context.Context, ref ledger.Ref) (io.ReadCloser, error) {
	data, err := GetStLedgerV2Object(ctx, ref.String())
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// stCtfObject reads one object, from held objects first, then the store: nil
// when the store holds none.
func stCtfObject(ctx context.Context, held ledger.Objects, ref ledger.Ref) ([]byte, error) {
	if data, ok := held[ref]; ok && ledger.RefOf(data) == ref {
		return data, nil
	}
	if ref == ledger.RefOf(nil) {
		return []byte{}, nil
	}
	data, err := GetStLedgerV2Object(ctx, ref.String())
	if errors.Is(err, startifact.ErrLedgerObjectNotFound) {
		return nil, nil
	}
	return data, err
}

// stCtfEpochCensus is a covered epoch's committed census as Build takes it,
// with its candidates when it decodes.
type stCtfEpochCensus struct {
	input      *ctf.CensusInput
	candidates []ctf.Candidate
	canonical  bool
}

// stCtfCensusObjects gathers what can be had of a committed census: its
// header, and, when the header decodes for this operator and epoch, the row
// chunks and exclusion object it names. Package ctf decides what the census
// is: an object Build needs and was not given stops it (ErrCensusUnavailable,
// a lost census, retried and alerted), and a census that is not canonical
// gives no draw, never a refusal. Gathering what it can, and no more, keeps
// the driver from deciding either itself. candidates are the census's when it
// decodes, for the fleet bindings.
func stCtfCensusObjects(ctx context.Context, cfg *StConfig, epoch uint64, hash ledger.Ref, held ledger.Objects) (ledger.Objects, []ctf.Candidate, bool, error) {
	objects := ledger.Objects{}
	header, err := stCtfObject(ctx, held, hash)
	if err != nil || header == nil {
		return objects, nil, false, err
	}
	objects[hash] = header
	decodedHeader, err := ctf.DecodeCensusHeader(header)
	if err != nil || decodedHeader.NoID != cfg.NoId || decodedHeader.Epoch != epoch {
		return objects, nil, false, nil
	}
	for _, ref := range append(slices.Clone(decodedHeader.Rows), decodedHeader.Excluded) {
		data, err := stCtfObject(ctx, held, ref)
		if err != nil {
			return nil, nil, false, err
		}
		if data != nil {
			objects[ref] = data
		}
	}
	census, err := ctf.DecodeCensus(header, objects)
	if err != nil {
		return objects, nil, false, nil
	}
	return objects, census.Candidates, true, nil
}

// stCtfCommittedCensus reads an epoch's census commitment at the finalized
// head and the census it names: nil when none is committed.
func stCtfCommittedCensus(ctx context.Context, cfg *StConfig, ctfClient StCtfClient, epoch uint64, census *StCtfCensus) (*stCtfEpochCensus, error) {
	hash, block, err := ctfClient.CtfCensus(ctx, cfg.NoId, epoch)
	if err != nil {
		return nil, err
	}
	if hash == ([32]byte{}) {
		return nil, nil
	}
	time, err := ctfClient.CtfBlockTime(ctx, block)
	if err != nil {
		return nil, err
	}
	var held ledger.Objects
	if census != nil && census.Hash == hash {
		held = census.Objects
	}
	objects, candidates, canonical, err := stCtfCensusObjects(ctx, cfg, epoch, hash, held)
	if err != nil {
		return nil, fmt.Errorf("st ctf: epoch %d census: %w", epoch, err)
	}
	return &stCtfEpochCensus{input: &ctf.CensusInput{Hash: hash, Block: block, Time: time, Objects: objects}, candidates: candidates, canonical: canonical}, nil
}

// stCtfDrawInputs completes a covered epoch's input for its committed census:
// the pulse of ρ_k, always, so that Build never lacks it whatever it finds
// the census to be; and, when the census is canonical, its candidates' fleet
// bindings and payout coldkeys, recorded with the census when it was built.
func stCtfDrawInputs(ctx context.Context, cfg *StConfig, input *ctf.EpochInput, census *stCtfEpochCensus, recorded *StCtfCensus) error {
	if census == nil {
		return nil
	}
	pulse, err := stCtfDrandPulse(ctx, cfg.Ctf.DrandUrls, ctf.DrawRound(census.input.Time))
	if err != nil {
		return err
	}
	input.Pulse = pulse
	if !census.canonical {
		return nil
	}
	clientIds := make([][16]byte, 0, len(census.candidates))
	for _, candidate := range census.candidates {
		clientIds = append(clientIds, candidate.ClientID)
	}
	bound, err := stCtfBoundFor(ctx, cfg, input.Epoch, clientIds)
	if err != nil {
		return err
	}
	if recorded == nil || recorded.Hash != census.input.Hash {
		return fmt.Errorf("st ctf: epoch %d has no recorded census matching its commitment %v, so its winners' coldkeys are unknown", input.Epoch, census.input.Hash)
	}
	input.Bound, input.Coldkeys = bound, recorded.Coldkeys
	return nil
}

// stCtfBuildInput reads a live CTF root's inputs for epoch e: the latest
// finalized CTF root, and for each covered epoch p+1 … e its funding, census,
// pulse, returned carry, bindings, coldkeys and fixed awards.
func stCtfBuildInput(ctx context.Context, cfg *StConfig, client StLedgerV2Client, ctfClient StCtfClient, records stCtfRecords, state *StLedgerV2State, carry common.Address, epoch uint64) (*ctf.BuildInput, error) {
	poolId := ctf.PoolID(cfg.NoId)
	in := &ctf.BuildInput{NoID: cfg.NoId, ChainID: cfg.ChainId, Netuid: uint16(cfg.Netuid), Epoch: epoch, FirstEpoch: state.FirstEpoch,
		CarryColdkey: stCtfCarryColdkey(carry), FundedThrough: map[uint64]uint256.Int{}}
	covered := state.FirstEpoch - 1
	if latest := state.Latest; latest != nil {
		previous, err := ctf.ReadPrevious(ctx, ledger.Root{Epoch: latest.Epoch, Root: latest.Root, Total: latest.Total, LeafCount: latest.LeafCount, LedgerHash: latest.LedgerHash}, stCtfObjectFetcher{})
		if err != nil {
			return nil, err
		}
		in.Previous, covered = previous, latest.Epoch
	}
	if epoch <= covered || ledger.MaxCoveredEpochs < epoch-covered {
		return nil, fmt.Errorf("st ctf: a root for epoch %d cannot cover epochs %d to %d", epoch, covered+1, epoch)
	}
	fixed := records.FixedAwards(ctx, poolId, covered+1, epoch)
	pending := state.Pending
	mayFinalize := pending != nil && 0 < state.Quorum && state.Quorum <= state.Approvals
	for k := covered + 1; k <= epoch; k++ {
		funded, err := client.LedgerV2FundedThrough(ctx, poolId, k)
		if err != nil {
			return nil, err
		}
		in.FundedThrough[k] = funded
		input := ctf.EpochInput{Epoch: k}
		if input.Recycled, err = ctfClient.CtfRecycledFor(ctx, carry, k); err != nil {
			return nil, err
		}
		recorded, err := stCtfCensusOf(ctx, k)
		if err != nil {
			return nil, err
		}
		census, err := stCtfCommittedCensus(ctx, cfg, ctfClient, k, recorded)
		if err != nil {
			return nil, err
		}
		if census != nil {
			input.Census = census.input
			if err := stCtfDrawInputs(ctx, cfg, &input, census, recorded); err != nil {
				return nil, err
			}
		}
		// The awards an earlier root covered k with are fixed, unless the
		// operator asked for them to be built again and no pending root that
		// can still finalize lists them (stLedgerV2Rebuilds).
		if record := fixed[k]; record != nil && len(record.ProviderChunks) == 1 && !(record.RebuildRequestedBy != "" && !(mayFinalize && k <= pending.Epoch)) {
			awards, err := ledger.ParseRef(record.ProviderChunks[0])
			if err != nil {
				return nil, fmt.Errorf("st ctf: epoch %d fixed awards %q: %w", k, record.ProviderChunks[0], err)
			}
			input.Awards = &awards
		} else if record != nil && record.RebuildRequestedBy == "" {
			return nil, fmt.Errorf("st ctf: epoch %d's fixed awards record is not one awards reference", k)
		}
		in.Epochs = append(in.Epochs, input)
	}
	return in, nil
}

// stCtfPublish writes a built root's objects: every object but the header, in
// reference order; then the awards references of the epochs built for the
// first time; then the header, so nothing published names a missing object.
func stCtfPublish(ctx context.Context, records stCtfRecords, poolId uint64, in *ctf.BuildInput, built *ctf.Ledger) error {
	refs := make([]ledger.Ref, 0, len(built.Objects))
	for ref := range built.Objects {
		if ref != built.Commit.LedgerHash {
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(refs, func(a ledger.Ref, b ledger.Ref) int { return bytes.Compare(a[:], b[:]) })
	for _, ref := range refs {
		if _, err := PutStLedgerV2Object(ctx, built.Objects[ref]); err != nil {
			return err
		}
	}
	if records != nil {
		for i, e := range built.Epochs {
			if in.Epochs[i].Awards == nil {
				if err := records.RecordFixedAwards(ctx, poolId, e.Epoch, e.AwardsRef.String()); err != nil {
					return err
				}
			}
		}
	}
	_, err := PutStLedgerV2Object(ctx, built.Header)
	return err
}

// stCtfRepublishCensus publishes a census's objects, put-if-absent, in
// reference order.
func stCtfRepublishCensus(ctx context.Context, census *StCtfCensus) error {
	refs := make([]ledger.Ref, 0, len(census.Objects))
	for ref := range census.Objects {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a ledger.Ref, b ledger.Ref) int { return bytes.Compare(a[:], b[:]) })
	for _, ref := range refs {
		if _, err := PutStLedgerV2Object(ctx, census.Objects[ref]); err != nil {
			return err
		}
	}
	return nil
}

// stCtfAccounts reads a published CTF ledger's accounts in position order: its
// header, then the account chunks it lists.
func stCtfAccounts(ctx context.Context, ledgerHash [32]byte) ([]merkle.LedgerAccount, error) {
	data, err := GetStLedgerV2Object(ctx, ledger.Ref(ledgerHash).String())
	if err != nil {
		return nil, err
	}
	header, err := ctf.DecodeHeader(data)
	if err != nil {
		return nil, fmt.Errorf("st ctf: ledger 0x%x: %w", ledgerHash, err)
	}
	chunks := make([][]byte, len(header.Accounts))
	for i, ref := range header.Accounts {
		if chunks[i], err = GetStLedgerV2Object(ctx, ref.String()); err != nil {
			return nil, err
		}
	}
	accounts, err := ledger.DecodeAccountChunks(chunks)
	if err != nil {
		return nil, fmt.Errorf("st ctf: ledger 0x%x: %w", ledgerHash, err)
	}
	return accounts, nil
}

// stCtfTreeCacheSize keeps the CTF pool's latest finalized and pending trees,
// with room for one replacement of each.
const stCtfTreeCacheSize = 4

var stCtfTreeLock sync.Mutex
var stCtfTrees = map[[32]byte]*stLedgerV2Tree{}
var stCtfTreeOrder [][32]byte

func stCtfCacheTree(ledgerHash [32]byte, tree *stLedgerV2Tree) {
	stCtfTreeLock.Lock()
	defer stCtfTreeLock.Unlock()
	if _, ok := stCtfTrees[ledgerHash]; ok {
		return
	}
	if len(stCtfTreeOrder) == stCtfTreeCacheSize {
		delete(stCtfTrees, stCtfTreeOrder[0])
		stCtfTreeOrder = stCtfTreeOrder[1:]
	}
	stCtfTrees[ledgerHash] = tree
	stCtfTreeOrder = append(stCtfTreeOrder, ledgerHash)
}

// stCtfCachedTree returns the tree of a committed CTF root, built from its
// published accounts on a miss, and requires it to match the chain.
func stCtfCachedTree(ctx context.Context, ledgerHash [32]byte, root [32]byte, total *uint256.Int, leafCount uint64) (*stLedgerV2Tree, error) {
	stCtfTreeLock.Lock()
	cached := stCtfTrees[ledgerHash]
	stCtfTreeLock.Unlock()
	if cached == nil {
		accounts, err := stCtfAccounts(ctx, ledgerHash)
		if err != nil {
			return nil, err
		}
		tree, err := merkle.BuildLedgerSumTree(ctx, accounts)
		if err != nil {
			return nil, err
		}
		cached = &stLedgerV2Tree{tree: tree, accounts: accounts}
	}
	builtRoot, builtTotal := cached.tree.Root()
	if builtRoot != root || !builtTotal.Eq(total) || cached.tree.LeafCount() != leafCount {
		return nil, fmt.Errorf("st ctf: the published accounts of ledger 0x%x do not rebuild its root", ledgerHash)
	}
	stCtfCacheTree(ledgerHash, cached)
	return cached, nil
}

// stCtfServerNow is the driver's clock; tests replace it.
var stCtfServerNow = server.NowUtc
