// st_ledger_v2_model persists settlement v2 (VERSION2-FINAL.md §6, §8) for
// the operator server: the provider rows recorded per covered epoch, each
// pool's account index, the per-epoch amounts of finalized roots, and the
// mirror of vault2's funding snapshots, roots and path demands. Rows are
// scoped by the st deployment key and the pool's no_id, and none is rewritten
// once its chain fact or record is fixed.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/holiman/uint256"
	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The v2 driver's st_publish kinds, and the status of a v2 step that ended
// without its chain write because its window closed or its intent was spent.
const (
	StPublishKindV2Close    = "v2close"
	StPublishKindV2Commit   = "v2commit"
	StPublishKindV2Finalize = "v2finalize"
	StPublishKindV2Answer   = "v2answer"

	StPublishStatusMissed = "missed"
)

// Root statuses mirror a commit's outcome in vault2.
const (
	StLedgerV2RootPending   = "pending"
	StLedgerV2RootFinalized = "finalized"
	StLedgerV2RootReplaced  = "replaced"
	StLedgerV2RootDeleted   = "deleted"
)

var (
	// ErrStLedgerV2EpochRowsFixed refuses a second, different record for an
	// epoch whose provider rows or null declaration is already recorded.
	ErrStLedgerV2EpochRowsFixed = errors.New("st ledger v2: the epoch's provider rows are already recorded")
	// ErrStLedgerV2Record refuses a finalized-root record that would not extend
	// the pool's account index and amounts exactly once and in root order.
	ErrStLedgerV2Record = errors.New("st ledger v2: finalized root record refused")
)

// StLedgerV2EpochRows is the record of one covered epoch's provider rows: the
// chunk references of its rows (empty when it has none), or, when DeclaredBy
// is set, the operator's declaration that its inputs are unrecoverable, which
// the header shows as providers: null. RebuildRequestedBy names who made the
// operator's explicit request to build the epoch's rows again.
type StLedgerV2EpochRows struct {
	Epoch              uint64
	ProviderChunks     []string
	DeclaredBy         string
	RebuildRequestedBy string
	CreateTime         time.Time
}

// Null reports the explicit unrecoverable declaration.
func (self *StLedgerV2EpochRows) Null() bool {
	return self.DeclaredBy != ""
}

// RecordStLedgerV2EpochRows records an epoch's provider chunk references once
// (§6.4 fixed inputs). Recording the same references again is a no-op; a
// record whose rebuild the operator requested is replaced, which ends the
// request; any other existing record is ErrStLedgerV2EpochRowsFixed.
func RecordStLedgerV2EpochRows(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, providerChunks []string) error {
	if providerChunks == nil {
		providerChunks = []string{}
	}
	existing := recordStLedgerV2EpochRows(ctx, deploymentKey, noId, epoch, providerChunks, "")
	if existing != nil && (existing.Null() || !slices.Equal(existing.ProviderChunks, providerChunks)) {
		return fmt.Errorf("%w: epoch %d", ErrStLedgerV2EpochRowsFixed, epoch)
	}
	return nil
}

// DeclareStLedgerV2EpochUnrecoverable records the operator's explicit
// declaration, naming who made it, that an epoch's inputs are unrecoverable.
// It is refused once the epoch's provider rows are recorded; repeating a
// declaration is a no-op.
func DeclareStLedgerV2EpochUnrecoverable(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, declaredBy string) error {
	if declaredBy == "" {
		return errors.New("st ledger v2: an unrecoverable declaration must name who makes it")
	}
	existing := recordStLedgerV2EpochRows(ctx, deploymentKey, noId, epoch, nil, declaredBy)
	if existing != nil && !existing.Null() {
		return fmt.Errorf("%w: epoch %d", ErrStLedgerV2EpochRowsFixed, epoch)
	}
	return nil
}

// Inserts the record when absent, replaces with rows a record whose rebuild
// was requested, and otherwise returns the existing one.
func recordStLedgerV2EpochRows(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, providerChunks []string, declaredBy string) (existing *StLedgerV2EpochRows) {
	key := requireStDeploymentKey(deploymentKey)
	var declared *string
	if declaredBy != "" {
		declared = &declaredBy
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_ledger_v2_epoch_rows (deployment_key, no_id, epoch, provider_chunks, declared_by, create_time)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (deployment_key, no_id, epoch) DO UPDATE
				SET provider_chunks = EXCLUDED.provider_chunks, declared_by = NULL, rebuild_requested_by = NULL, create_time = EXCLUDED.create_time
				WHERE st_ledger_v2_epoch_rows.rebuild_requested_by IS NOT NULL AND EXCLUDED.declared_by IS NULL
			`,
			key, int64(noId), int64(epoch), providerChunks, declared, server.NowUtc(),
		))
		if tag.RowsAffected() == 0 {
			existing = getStLedgerV2EpochRowsInTx(ctx, tx, key, noId, epoch, epoch)[epoch]
		}
	})
	return existing
}

// GetStLedgerV2EpochRows returns the records of epochs first … last, by epoch.
func GetStLedgerV2EpochRows(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, first uint64, last uint64) map[uint64]*StLedgerV2EpochRows {
	key := requireStDeploymentKey(deploymentKey)
	var records map[uint64]*StLedgerV2EpochRows
	server.Tx(ctx, func(tx server.PgTx) {
		records = getStLedgerV2EpochRowsInTx(ctx, tx, key, noId, first, last)
	})
	return records
}

func getStLedgerV2EpochRowsInTx(ctx context.Context, tx server.PgTx, key string, noId uint64, first uint64, last uint64) map[uint64]*StLedgerV2EpochRows {
	records := map[uint64]*StLedgerV2EpochRows{}
	result, err := tx.Query(
		ctx,
		`
			SELECT epoch, provider_chunks, declared_by, rebuild_requested_by, create_time
			FROM st_ledger_v2_epoch_rows
			WHERE deployment_key = $1 AND no_id = $2 AND epoch BETWEEN $3 AND $4
		`,
		key, int64(noId), int64(first), int64(last),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			record := &StLedgerV2EpochRows{}
			var epoch int64
			var declaredBy, rebuildRequestedBy *string
			server.Raise(result.Scan(&epoch, &record.ProviderChunks, &declaredBy, &rebuildRequestedBy, &record.CreateTime))
			record.Epoch = uint64(epoch)
			if declaredBy != nil {
				record.DeclaredBy = *declaredBy
			}
			if rebuildRequestedBy != nil {
				record.RebuildRequestedBy = *rebuildRequestedBy
			}
			records[record.Epoch] = record
		}
	})
	return records
}

// StLedgerV2Account is a coldkey's fixed position in a pool's ledger.
type StLedgerV2Account struct {
	NoId      uint64
	Index     uint64
	Coldkey   [32]byte
	RootIndex uint64
}

// GetStLedgerV2AccountCount returns the number of accounts recorded for a pool.
// Positions are recorded contiguously from 0, so the count is the highest
// position plus one, which the primary key answers without a scan.
func GetStLedgerV2AccountCount(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) uint64 {
	key := requireStDeploymentKey(deploymentKey)
	var count int64
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT coalesce(max(account_index) + 1, 0) FROM st_ledger_v2_account WHERE deployment_key = $1 AND no_id = $2`, key, int64(noId)).Scan(&count))
	})
	return uint64(count)
}

// GetStLedgerV2AccountsForColdkey returns a coldkey's account in every pool
// where one is recorded, by no_id.
func GetStLedgerV2AccountsForColdkey(ctx context.Context, deploymentKey StDeploymentKey, coldkey [32]byte) []*StLedgerV2Account {
	key := requireStDeploymentKey(deploymentKey)
	accounts := []*StLedgerV2Account{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT no_id, account_index, root_index
				FROM st_ledger_v2_account
				WHERE deployment_key = $1 AND coldkey = $2
				ORDER BY no_id
			`,
			key, coldkey[:],
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var noId, index, rootIndex int64
				server.Raise(result.Scan(&noId, &index, &rootIndex))
				accounts = append(accounts, &StLedgerV2Account{NoId: uint64(noId), Index: uint64(index), Coldkey: coldkey, RootIndex: uint64(rootIndex)})
			}
		})
	})
	return accounts
}

// StLedgerV2Amount is one coldkey's amount, in rao, of one covered epoch.
type StLedgerV2Amount struct {
	NoId      uint64
	Epoch     uint64
	Coldkey   [32]byte
	Amount    uint256.Int
	RootIndex uint64
}

// RecordStLedgerV2Root writes, in one transaction, a finalized root's new
// accounts at positions firstIndex … and its covered epochs' positive
// amounts, then marks the root recorded. Roots are recorded once and in root
// order; repeating a record is a no-op. Anything else is ErrStLedgerV2Record.
func RecordStLedgerV2Root(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, seq uint64, firstIndex uint64, newColdkeys [][32]byte, amounts []*StLedgerV2Amount) (returnErr error) {
	key := requireStDeploymentKey(deploymentKey)
	refuse := func(format string, args ...any) {
		panic(fmt.Errorf("%w: "+format, append([]any{ErrStLedgerV2Record}, args...)...))
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok || !errors.Is(err, ErrStLedgerV2Record) {
				panic(recovered)
			}
			returnErr = err
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		var rootIndex *int64
		var status string
		var recorded *time.Time
		err := tx.QueryRow(
			ctx,
			`SELECT status, root_index, recorded_time FROM st_ledger_v2_root WHERE deployment_key = $1 AND no_id = $2 AND seq = $3 FOR UPDATE`,
			key, int64(noId), int64(seq),
		).Scan(&status, &rootIndex, &recorded)
		if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		if status != StLedgerV2RootFinalized {
			refuse("seq %d is not a finalized root", seq)
		}
		if recorded != nil {
			return
		}
		var earlier bool
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM st_ledger_v2_root WHERE deployment_key = $1 AND no_id = $2 AND root_index < $3 AND recorded_time IS NULL)`,
			key, int64(noId), *rootIndex,
		).Scan(&earlier))
		if earlier {
			refuse("an earlier root of pool %d is not recorded", noId)
		}
		var count int64
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM st_ledger_v2_account WHERE deployment_key = $1 AND no_id = $2`, key, int64(noId)).Scan(&count))
		if uint64(count) != firstIndex {
			refuse("pool %d holds %d accounts, not %d", noId, count, firstIndex)
		}
		now := server.NowUtc()
		insertedAccounts, insertedAmounts, positiveAmounts := int64(0), int64(0), int64(0)
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for offset, coldkey := range newColdkeys {
				batch.Queue(
					`
						INSERT INTO st_ledger_v2_account (deployment_key, no_id, account_index, coldkey, root_index, create_time)
						VALUES ($1, $2, $3, $4, $5, $6)
						ON CONFLICT DO NOTHING
					`,
					key, int64(noId), int64(firstIndex)+int64(offset), coldkey[:], *rootIndex, now,
				)
			}
			for _, amount := range amounts {
				if amount.Amount.IsZero() {
					continue
				}
				batch.Queue(
					`
						INSERT INTO st_ledger_v2_amount (deployment_key, no_id, epoch, coldkey, amount, root_index)
						VALUES ($1, $2, $3, $4, $5, $6)
						ON CONFLICT DO NOTHING
					`,
					key, int64(noId), int64(amount.Epoch), amount.Coldkey[:], amount.Amount.Dec(), *rootIndex,
				)
			}
		}, func(results server.PgBatchResults) {
			for range newColdkeys {
				insertedAccounts += server.RaisePgResult(results.Exec()).RowsAffected()
			}
			for _, amount := range amounts {
				if !amount.Amount.IsZero() {
					positiveAmounts += 1
					insertedAmounts += server.RaisePgResult(results.Exec()).RowsAffected()
				}
			}
		})
		if insertedAccounts != int64(len(newColdkeys)) {
			refuse("a new coldkey of pool %d already has an account", noId)
		}
		if insertedAmounts != positiveAmounts {
			refuse("an epoch of pool %d already has an amount for a coldkey", noId)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE st_ledger_v2_root SET recorded_time = $4 WHERE deployment_key = $1 AND no_id = $2 AND seq = $3`,
			key, int64(noId), int64(seq), now,
		))
	})
	return nil
}

// GetStLedgerV2Amounts returns an epoch's recorded amounts for a pool.
func GetStLedgerV2Amounts(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*StLedgerV2Amount {
	return getStLedgerV2Amounts(ctx, deploymentKey, `no_id = $2 AND epoch = $3`, int64(noId), int64(epoch))
}

// GetStLedgerV2AmountsForColdkey returns a coldkey's recorded amounts in every
// pool, by pool then epoch.
func GetStLedgerV2AmountsForColdkey(ctx context.Context, deploymentKey StDeploymentKey, coldkey [32]byte) []*StLedgerV2Amount {
	return getStLedgerV2Amounts(ctx, deploymentKey, `coldkey = $2`, coldkey[:])
}

func getStLedgerV2Amounts(ctx context.Context, deploymentKey StDeploymentKey, where string, args ...any) []*StLedgerV2Amount {
	key := requireStDeploymentKey(deploymentKey)
	amounts := []*StLedgerV2Amount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT no_id, epoch, coldkey, amount::text, root_index
				FROM st_ledger_v2_amount
				WHERE deployment_key = $1 AND `+where+`
				ORDER BY no_id, epoch, coldkey
			`,
			append([]any{key}, args...)...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				amount := &StLedgerV2Amount{}
				var noId, epoch, rootIndex int64
				var coldkey []byte
				var value string
				server.Raise(result.Scan(&noId, &epoch, &coldkey, &value, &rootIndex))
				amount.NoId, amount.Epoch, amount.RootIndex = uint64(noId), uint64(epoch), uint64(rootIndex)
				copy(amount.Coldkey[:], coldkey)
				server.Raise(amount.Amount.SetFromDecimal(value))
				amounts = append(amounts, amount)
			}
		})
	})
	return amounts
}

// StLedgerV2Funding mirrors one epoch's vault2 funding snapshot.
type StLedgerV2Funding struct {
	NoId          uint64
	Epoch         uint64
	FundedThrough uint256.Int
	Observed      uint256.Int
	Amount        uint256.Int
	Deferred      bool
	BlockNumber   uint64
}

// SetStLedgerV2Funding mirrors a snapshot. Snapshots are written once on
// chain, so a replay changes nothing.
func SetStLedgerV2Funding(ctx context.Context, deploymentKey StDeploymentKey, funding *StLedgerV2Funding) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_ledger_v2_funding (deployment_key, no_id, epoch, funded_through, observed, amount, deferred, block_number)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
			`,
			key, int64(funding.NoId), int64(funding.Epoch), funding.FundedThrough.Dec(), funding.Observed.Dec(), funding.Amount.Dec(), funding.Deferred, int64(funding.BlockNumber),
		))
	})
}

// GetStLedgerV2Funding returns an epoch's mirrored snapshot, or nil.
func GetStLedgerV2Funding(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *StLedgerV2Funding {
	key := requireStDeploymentKey(deploymentKey)
	var funding *StLedgerV2Funding
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT funded_through::text, observed::text, amount::text, deferred, block_number
				FROM st_ledger_v2_funding
				WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				funding = &StLedgerV2Funding{NoId: noId, Epoch: epoch}
				var fundedThrough, observed, amount string
				var block int64
				server.Raise(result.Scan(&fundedThrough, &observed, &amount, &funding.Deferred, &block))
				server.Raise(funding.FundedThrough.SetFromDecimal(fundedThrough))
				server.Raise(funding.Observed.SetFromDecimal(observed))
				server.Raise(funding.Amount.SetFromDecimal(amount))
				funding.BlockNumber = uint64(block)
			}
		})
	})
	return funding
}

// StLedgerV2Root mirrors one commit to a pool's pending slot and its outcome.
type StLedgerV2Root struct {
	NoId       uint64
	Seq        uint64
	Epoch      uint64
	Root       [32]byte
	Total      uint256.Int
	LeafCount  uint64
	LedgerHash [32]byte
	// CommitBlock is nil when only the finalization was mirrored.
	CommitBlock  *uint64
	Status       string
	StatusBlock  *uint64
	RootIndex    *uint64
	RecordedTime *time.Time
}

// AddStLedgerV2Commit mirrors a DistributionCommitted event: the new pending
// root, and the pending root it replaced (replacedSeq 0 when none).
func AddStLedgerV2Commit(ctx context.Context, deploymentKey StDeploymentKey, root *StLedgerV2Root, commitBlock uint64, replacedSeq uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_ledger_v2_root (deployment_key, no_id, seq, epoch, root, total, leaf_count, ledger_hash, commit_block, status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending')
				ON CONFLICT (deployment_key, no_id, seq) DO NOTHING
			`,
			key, int64(root.NoId), int64(root.Seq), int64(root.Epoch), root.Root[:], root.Total.Dec(), int64(root.LeafCount), root.LedgerHash[:], int64(commitBlock),
		))
		if replacedSeq != 0 {
			setStLedgerV2RootStatusInTx(ctx, tx, key, root.NoId, replacedSeq, StLedgerV2RootReplaced, commitBlock)
		}
	})
}

// SetStLedgerV2RootDeleted mirrors a DistributionDeleted event.
func SetStLedgerV2RootDeleted(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, seq uint64, block uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		setStLedgerV2RootStatusInTx(ctx, tx, key, noId, seq, StLedgerV2RootDeleted, block)
	})
}

// Only a pending root has an outcome to record.
func setStLedgerV2RootStatusInTx(ctx context.Context, tx server.PgTx, key string, noId uint64, seq uint64, status string, block uint64) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE st_ledger_v2_root SET status = $4, status_block = $5 WHERE deployment_key = $1 AND no_id = $2 AND seq = $3 AND status = 'pending'`,
		key, int64(noId), int64(seq), status, int64(block),
	))
}

// SetStLedgerV2RootFinalized mirrors a DistributionFinalized event; root
// carries the event's fields, including its root index.
func SetStLedgerV2RootFinalized(ctx context.Context, deploymentKey StDeploymentKey, root *StLedgerV2Root, block uint64) {
	key := requireStDeploymentKey(deploymentKey)
	if root.RootIndex == nil {
		panic("st ledger v2: a finalized root needs its root index")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_ledger_v2_root (deployment_key, no_id, seq, epoch, root, total, leaf_count, ledger_hash, status, status_block, root_index)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'finalized', $9, $10)
				ON CONFLICT (deployment_key, no_id, seq) DO UPDATE
				SET status = 'finalized', status_block = EXCLUDED.status_block, root_index = EXCLUDED.root_index
				WHERE st_ledger_v2_root.status = 'pending'
			`,
			key, int64(root.NoId), int64(root.Seq), int64(root.Epoch), root.Root[:], root.Total.Dec(), int64(root.LeafCount), root.LedgerHash[:], int64(block), int64(*root.RootIndex),
		))
	})
}

const stLedgerV2RootColumns = `no_id, seq, epoch, root, total::text, leaf_count, ledger_hash, commit_block, status, status_block, root_index, recorded_time`

func scanStLedgerV2Root(result server.PgResult) *StLedgerV2Root {
	root := &StLedgerV2Root{}
	var noId, seq, epoch, leafCount int64
	var rootBytes, ledgerHash []byte
	var total string
	var commitBlock, statusBlock, rootIndex *int64
	server.Raise(result.Scan(&noId, &seq, &epoch, &rootBytes, &total, &leafCount, &ledgerHash, &commitBlock, &root.Status, &statusBlock, &rootIndex, &root.RecordedTime))
	root.NoId, root.Seq, root.Epoch, root.LeafCount = uint64(noId), uint64(seq), uint64(epoch), uint64(leafCount)
	copy(root.Root[:], rootBytes)
	copy(root.LedgerHash[:], ledgerHash)
	server.Raise(root.Total.SetFromDecimal(total))
	root.CommitBlock, root.StatusBlock, root.RootIndex = stOptionalUint64(commitBlock), stOptionalUint64(statusBlock), stOptionalUint64(rootIndex)
	return root
}

func stOptionalUint64(value *int64) *uint64 {
	if value == nil {
		return nil
	}
	v := uint64(*value)
	return &v
}

func getStLedgerV2Roots(ctx context.Context, deploymentKey StDeploymentKey, where string, args ...any) []*StLedgerV2Root {
	key := requireStDeploymentKey(deploymentKey)
	roots := []*StLedgerV2Root{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT `+stLedgerV2RootColumns+` FROM st_ledger_v2_root WHERE deployment_key = $1 AND `+where, append([]any{key}, args...)...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				roots = append(roots, scanStLedgerV2Root(result))
			}
		})
	})
	return roots
}

func firstStLedgerV2Root(roots []*StLedgerV2Root) *StLedgerV2Root {
	if len(roots) == 0 {
		return nil
	}
	return roots[0]
}

// GetStLedgerV2Root returns the mirrored root of a commit seq, or nil.
func GetStLedgerV2Root(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, seq uint64) *StLedgerV2Root {
	return firstStLedgerV2Root(getStLedgerV2Roots(ctx, deploymentKey, `no_id = $2 AND seq = $3`, int64(noId), int64(seq)))
}

// GetStLedgerV2PendingRoot returns the pool's mirrored pending root, or nil.
func GetStLedgerV2PendingRoot(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) *StLedgerV2Root {
	return firstStLedgerV2Root(getStLedgerV2Roots(ctx, deploymentKey, `no_id = $2 AND status = 'pending' ORDER BY seq DESC LIMIT 1`, int64(noId)))
}

// GetStLedgerV2FinalizedRoot returns the finalized root at a root index, or nil.
func GetStLedgerV2FinalizedRoot(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, rootIndex uint64) *StLedgerV2Root {
	return firstStLedgerV2Root(getStLedgerV2Roots(ctx, deploymentKey, `no_id = $2 AND root_index = $3`, int64(noId), int64(rootIndex)))
}

// GetLatestStLedgerV2FinalizedRoot returns the pool's latest finalized root, or nil.
func GetLatestStLedgerV2FinalizedRoot(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) *StLedgerV2Root {
	return firstStLedgerV2Root(getStLedgerV2Roots(ctx, deploymentKey, `no_id = $2 AND status = 'finalized' ORDER BY root_index DESC LIMIT 1`, int64(noId)))
}

// GetNextUnrecordedStLedgerV2Root returns the pool's lowest finalized root
// whose accounts and amounts are not recorded yet, or nil.
func GetNextUnrecordedStLedgerV2Root(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) *StLedgerV2Root {
	return firstStLedgerV2Root(getStLedgerV2Roots(ctx, deploymentKey, `no_id = $2 AND status = 'finalized' AND recorded_time IS NULL ORDER BY root_index LIMIT 1`, int64(noId)))
}

// GetStLedgerV2RootsFromEpoch returns the pool's mirrored roots for epochs at
// or after minEpoch, by seq.
func GetStLedgerV2RootsFromEpoch(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, minEpoch uint64) []*StLedgerV2Root {
	return getStLedgerV2Roots(ctx, deploymentKey, `no_id = $2 AND epoch >= $3 ORDER BY seq`, int64(noId), int64(minEpoch))
}

// StLedgerV2Demand mirrors one demand to open, in a pending root, the path of
// an account of the finalized root RootIndex.
type StLedgerV2Demand struct {
	NoId        uint64
	Seq         uint64
	RootIndex   uint64
	Index       uint64
	Demander    string
	DemandBlock uint64
	Answerer    *string
	AnswerBlock *uint64
}

// AddStLedgerV2Demand mirrors a PathDemanded event.
func AddStLedgerV2Demand(ctx context.Context, deploymentKey StDeploymentKey, demand *StLedgerV2Demand) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_ledger_v2_demand (deployment_key, no_id, seq, root_index, account_index, demander, demand_block)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (deployment_key, no_id, seq, root_index, account_index) DO NOTHING
			`,
			key, int64(demand.NoId), int64(demand.Seq), int64(demand.RootIndex), int64(demand.Index), demand.Demander, int64(demand.DemandBlock),
		))
	})
}

// SetStLedgerV2DemandAnswered mirrors a PathAnswered event.
func SetStLedgerV2DemandAnswered(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, seq uint64, rootIndex uint64, index uint64, answerer string, block uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE st_ledger_v2_demand SET answerer = $6, answer_block = $7
				WHERE deployment_key = $1 AND no_id = $2 AND seq = $3 AND root_index = $4 AND account_index = $5 AND answer_block IS NULL
			`,
			key, int64(noId), int64(seq), int64(rootIndex), int64(index), answerer, int64(block),
		))
	})
}

// GetOpenStLedgerV2Demands returns a commit seq's unanswered demands, by root
// index and then account index.
func GetOpenStLedgerV2Demands(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, seq uint64) []*StLedgerV2Demand {
	key := requireStDeploymentKey(deploymentKey)
	demands := []*StLedgerV2Demand{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT root_index, account_index, demander, demand_block
				FROM st_ledger_v2_demand
				WHERE deployment_key = $1 AND no_id = $2 AND seq = $3 AND answer_block IS NULL
				ORDER BY root_index, account_index
			`,
			key, int64(noId), int64(seq),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				demand := &StLedgerV2Demand{NoId: noId, Seq: seq}
				var rootIndex, index, block int64
				server.Raise(result.Scan(&rootIndex, &index, &demand.Demander, &block))
				demand.RootIndex, demand.Index, demand.DemandBlock = uint64(rootIndex), uint64(index), uint64(block)
				demands = append(demands, demand)
			}
		})
	})
	return demands
}
