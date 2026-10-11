package controller

// st_ledger_v2_builder.go — the seam between the settlement v2 driver and the
// ledger build (VERSION2-FINAL.md §6, §8.2). The driver reads chain state and
// the database, hands the builder one root's inputs, publishes what it
// returns and commits it. The builder is written on top of the ledger package;
// the driver never encodes or decodes ledger objects itself.

import (
	"context"
	"errors"
	"sync"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server/model"
)

// ErrStLedgerV2NoRoot is returned by LedgerBuilder.Build when the root's epoch
// does not allocate (no eligible usage, or an unrecoverable epoch), so the
// carry walk cannot end at zero and there is no root to commit (§6.4).
var ErrStLedgerV2NoRoot = errors.New("st ledger v2: the root's epoch does not allocate")

// LedgerBuilder builds a pool's ledger for one root and reads published ones.
type LedgerBuilder interface {
	// Build builds the root for input.Epoch from the previous finalized root's
	// accounts and the covered epochs' provider rows: a recorded epoch reuses
	// its recorded chunks or null declaration, and any other epoch's rows are
	// built from the usage census and wallet selection. Output for equal
	// inputs is byte-identical.
	Build(ctx context.Context, input *StLedgerV2BuildInput) (*StLedgerV2Build, error)
	// Accounts reads the account rows, in position order, of the published
	// ledger whose header has this sha256.
	Accounts(ctx context.Context, ledgerHash [32]byte) ([]merkle.LedgerAccount, error)
}

// StLedgerV2BuildInput is one root's inputs, read by the driver from vault2 at
// the finalized head and from the epoch-rows records.
type StLedgerV2BuildInput struct {
	NoId uint64
	// Epoch is e, the root's epoch.
	Epoch uint64
	// Previous is the latest finalized root, or nil for the pool's first root.
	Previous *StLedgerV2FinalizedRoot
	// Epochs are the covered epochs p+1 … e in order.
	Epochs []*StLedgerV2CoveredEpoch
}

// StLedgerV2CoveredEpoch is one covered epoch's funding and recorded rows.
type StLedgerV2CoveredEpoch struct {
	Epoch uint64
	// Funding is f_k = fundedThrough[k] − fundedThrough[k−1].
	Funding uint256.Int
	// Rows is the epoch's recorded provider rows, or nil when it has none yet.
	Rows *model.StLedgerV2EpochRows
}

// StLedgerV2Build is a built root.
type StLedgerV2Build struct {
	// Header is the canonical header; ledgerHash is its sha256.
	Header []byte
	// Chunks are the objects the header lists that are not published yet: the
	// newly built provider chunks and the account chunks.
	Chunks [][]byte
	// Epochs are the covered epochs' provider rows as the header lists them.
	Epochs []*StLedgerV2EpochProviders
	// Accounts are all of the root's accounts in position order. The driver
	// builds the sum tree from them for the commitment.
	Accounts []merkle.LedgerAccount
	// Amounts are each covered epoch's positive amount per coldkey.
	Amounts []*model.StLedgerV2Amount
}

// StLedgerV2EpochProviders is a covered epoch's provider chunk references;
// Null marks providers: null.
type StLedgerV2EpochProviders struct {
	Epoch          uint64
	ProviderChunks []string
	Null           bool
}

var stLedgerBuilderLock sync.Mutex
var stLedgerBuilderInstance LedgerBuilder

// SetStLedgerBuilder installs the ledger builder; nil removes it.
func SetStLedgerBuilder(builder LedgerBuilder) {
	stLedgerBuilderLock.Lock()
	defer stLedgerBuilderLock.Unlock()
	stLedgerBuilderInstance = builder
}

func stLedgerBuilder() LedgerBuilder {
	stLedgerBuilderLock.Lock()
	defer stLedgerBuilderLock.Unlock()
	return stLedgerBuilderInstance
}
