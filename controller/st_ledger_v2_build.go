package controller

// st_ledger_v2_build.go — the operator server's ledger builder
// (VERSION2-FINAL.md §6, §8.2) on the ledger package. A covered epoch already
// recorded reuses its published chunks or null declaration (§6.4 fixed
// inputs); any other covered epoch's provider rows come from v1's inputs
// (st_ledger_v2_rows.go). The previous finalized root's accounts are read
// back from its published objects. ledger.Build then runs the allocation walk
// over the funding vault2 snapshotted, and builds the accounts, the sum tree
// and the objects.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server/model"
)

// ErrStLedgerV2RebuildRefused refuses the operator's rebuild of an epoch
// whose provider rows cannot be built again.
var ErrStLedgerV2RebuildRefused = errors.New("st ledger v2: the epoch's provider rows cannot be rebuilt")

// ErrStLedgerV2RebuildWithdrawRefused refuses the operator's withdrawal of an
// epoch's rebuild request that is not pending.
var ErrStLedgerV2RebuildWithdrawRefused = errors.New("st ledger v2: the epoch's rebuild request cannot be withdrawn")

// stLedgerV2Builder is the server's LedgerBuilder.
type stLedgerV2Builder struct{}

// InstallStLedgerBuilder installs the server's ledger builder when settlement
// v2 is configured, and reports whether it did. The task worker, whose v2
// driver steps build ledgers, and the API router, whose ledger routes read
// them, install it when they are built at start.
func InstallStLedgerBuilder() bool {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() {
		return false
	}
	SetStLedgerBuilder(stLedgerV2Builder{})
	return true
}

// Build builds the configured pool's root for input.Epoch. fundedThrough[k]
// is the previous root's total, which vault2 committed as fundedThrough[p],
// plus the funding of the covered epochs through k.
func (self stLedgerV2Builder) Build(ctx context.Context, input *StLedgerV2BuildInput) (*StLedgerV2Build, error) {
	in := ledger.BuildInput{NoID: input.NoId, Epoch: input.Epoch, FundedThrough: map[uint64]uint256.Int{}}
	var funded uint256.Int
	if previous := input.Previous; previous != nil {
		accounts, err := self.Accounts(ctx, previous.LedgerHash)
		if err != nil {
			return nil, err
		}
		in.Previous = &ledger.Previous{
			Root:     ledger.Root{Epoch: previous.Epoch, Root: previous.Root, Total: previous.Total, LeafCount: previous.LeafCount, LedgerHash: previous.LedgerHash},
			Accounts: accounts,
		}
		funded = previous.Total
	} else if 0 < len(input.Epochs) {
		// the pool's first root covers firstEpoch … e
		in.FirstEpoch = input.Epochs[0].Epoch
	}
	for _, covered := range input.Epochs {
		epoch, err := stLedgerV2EpochInput(ctx, covered)
		if err != nil {
			return nil, err
		}
		in.Epochs = append(in.Epochs, epoch)
		funded.Add(&funded, &covered.Funding)
		in.FundedThrough[covered.Epoch] = funded
	}
	built, err := ledger.Build(ctx, in)
	if errors.Is(err, ledger.ErrEpochNotAllocated) {
		return nil, fmt.Errorf("%w: epoch %d", ErrStLedgerV2NoRoot, input.Epoch)
	}
	if err != nil {
		return nil, err
	}

	build := &StLedgerV2Build{Header: built.Header, Accounts: built.Accounts}
	// every object but the header and the recorded epochs' published chunks
	published := map[ledger.Ref]bool{sha256.Sum256(built.Header): true}
	for i, epoch := range built.Epochs {
		providers := &StLedgerV2EpochProviders{Epoch: epoch.Epoch, Null: epoch.Null}
		if !epoch.Null {
			providers.ProviderChunks = make([]string, len(epoch.Providers))
			for j, ref := range epoch.Providers {
				providers.ProviderChunks[j] = ref.String()
				published[ref] = published[ref] || input.Epochs[i].Rows != nil
			}
		}
		build.Epochs = append(build.Epochs, providers)
		for _, delta := range epoch.Deltas {
			if !delta.Delta.IsZero() {
				build.Amounts = append(build.Amounts, &model.StLedgerV2Amount{NoId: input.NoId, Epoch: epoch.Epoch, Coldkey: delta.Coldkey, Amount: delta.Delta})
			}
		}
	}
	refs := []ledger.Ref{}
	for ref := range built.Objects {
		if !published[ref] {
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(refs, func(a ledger.Ref, b ledger.Ref) int {
		return bytes.Compare(a[:], b[:])
	})
	for _, ref := range refs {
		build.Chunks = append(build.Chunks, built.Objects[ref])
	}
	return build, nil
}

// stLedgerV2EpochInput is one covered epoch's ledger input: its recorded null
// declaration, or its recorded rows read back from their published chunks,
// or, when nothing is recorded, rows built from v1's inputs.
func stLedgerV2EpochInput(ctx context.Context, covered *StLedgerV2CoveredEpoch) (ledger.EpochInput, error) {
	input := ledger.EpochInput{Epoch: covered.Epoch}
	if covered.Rows == nil {
		rows, err := stLedgerV2NewEpochRows(ctx, covered.Epoch)
		input.Rows = rows
		return input, err
	}
	if covered.Rows.Null() {
		input.Null = true
		return input, nil
	}
	chunks := make([][]byte, len(covered.Rows.ProviderChunks))
	input.Chunks = make([]ledger.Ref, len(chunks))
	for i, ref := range covered.Rows.ProviderChunks {
		chunk, err := GetStLedgerV2Object(ctx, ref)
		if err != nil {
			return input, fmt.Errorf("st ledger v2: epoch %d: %w", covered.Epoch, err)
		}
		// the object store returns only bytes that hash to the reference
		chunks[i], input.Chunks[i] = chunk, ledger.RefOf(chunk)
	}
	rows, err := ledger.DecodeProviderChunks(chunks)
	if err != nil {
		return input, fmt.Errorf("st ledger v2: epoch %d: %w", covered.Epoch, err)
	}
	input.Rows = rows
	return input, nil
}

// Accounts reads a published ledger's account rows in position order: its
// header, then the account chunks the header lists.
func (stLedgerV2Builder) Accounts(ctx context.Context, ledgerHash [32]byte) ([]merkle.LedgerAccount, error) {
	data, err := GetStLedgerV2Object(ctx, ledger.Ref(ledgerHash).String())
	if err != nil {
		return nil, err
	}
	header, err := ledger.DecodeHeader(data)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: ledger 0x%x: %w", ledgerHash, err)
	}
	chunks := make([][]byte, len(header.Accounts))
	for i, ref := range header.Accounts {
		if chunks[i], err = GetStLedgerV2Object(ctx, ref.String()); err != nil {
			return nil, err
		}
	}
	accounts, err := ledger.DecodeAccountChunks(chunks)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: ledger 0x%x: %w", ledgerHash, err)
	}
	return accounts, nil
}

// StLedgerV2RebuildEpoch is the operator's explicit command naming an epoch
// whose recorded provider rows, or null declaration, are to be built again
// from v1's inputs (§6.4 fixed inputs). It records the request with who made
// it; the next root built while no pending root that can still finalize lists
// the epoch's recorded rows builds them again (stLedgerV2CommitEpoch). It is
// refused for an epoch the latest finalized root covers, whose rows are final,
// for an epoch without a record, which the next root builds anyway, and under
// a v2 pair other than the recorded activation's (SV-9).
func StLedgerV2RebuildEpoch(ctx context.Context, epoch uint64, requestedBy string) error {
	cfg, client, err := stLedgerV2Require()
	if err != nil {
		return err
	}
	if err := stLedgerV2ActivationAdmission(ctx, cfg); err != nil {
		return err
	}
	state, err := client.LedgerV2State(ctx, cfg.NoId)
	if err != nil {
		return err
	}
	if state.Latest != nil && epoch <= state.Latest.Epoch {
		return fmt.Errorf("%w: epoch %d is covered by finalized root %d", ErrStLedgerV2RebuildRefused, epoch, state.Latest.Index)
	}
	requested, err := model.RequestStLedgerV2EpochRebuild(ctx, cfg.DeploymentKey(), cfg.NoId, epoch, requestedBy)
	if err != nil {
		return err
	}
	if !requested {
		return fmt.Errorf("%w: epoch %d has no recorded provider rows", ErrStLedgerV2RebuildRefused, epoch)
	}
	glog.Infof("[st]v2 epoch %d provider rows to be rebuilt as requested by %s\n", epoch, requestedBy)
	return nil
}

// StLedgerV2WithdrawRebuild is the operator's explicit command withdrawing an
// epoch's pending rebuild request (StLedgerV2RebuildEpoch), so the driver
// treats the epoch as if the request had never been made (§6.4 fixed inputs,
// §8.6). Like the request, it names who makes it and is logged; any operator
// may withdraw any request. It returns who made the request. It is refused for
// an epoch without a record in this deployment's pool, for one without a
// pending request, as when its rows were already rebuilt for the request,
// when its rows are rebuilt, or its request changes, between reading the
// request and withdrawing it, and under a v2 pair other than the recorded
// activation's (SV-9).
func StLedgerV2WithdrawRebuild(ctx context.Context, epoch uint64, withdrawnBy string) (string, error) {
	cfg := stConfig()
	if !cfg.LedgerV2Enabled() {
		return "", errStLedgerV2NotConfigured
	}
	if withdrawnBy == "" {
		return "", errors.New("st ledger v2: a rebuild withdrawal must name who makes it")
	}
	if err := stLedgerV2ActivationAdmission(ctx, cfg); err != nil {
		return "", err
	}
	key := cfg.DeploymentKey()
	record := model.GetStLedgerV2EpochRows(ctx, key, cfg.NoId, epoch, epoch)[epoch]
	switch {
	case record == nil:
		return "", fmt.Errorf("%w: epoch %d has no recorded provider rows in pool %d of deployment %s", ErrStLedgerV2RebuildWithdrawRefused, epoch, cfg.NoId, key)
	case record.RebuildRequestedBy == "":
		return "", fmt.Errorf("%w: epoch %d has no pending rebuild request", ErrStLedgerV2RebuildWithdrawRefused, epoch)
	}
	if !model.WithdrawStLedgerV2EpochRebuild(ctx, key, cfg.NoId, record) {
		return "", fmt.Errorf("%w: epoch %d's rows were rebuilt, or its request changed, while it was being withdrawn", ErrStLedgerV2RebuildWithdrawRefused, epoch)
	}
	glog.Infof("[st]v2 epoch %d rebuild request by %s withdrawn by %s\n", epoch, record.RebuildRequestedBy, withdrawnBy)
	return record.RebuildRequestedBy, nil
}
