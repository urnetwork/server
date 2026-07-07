// st_model persists the subtensor (st) settlement state: provider claim
// wallets, the mirrored contract epoch machine, per-epoch payout leaves,
// publish (tx) records, and the synced contract event log.
//
// Design notes:
//   - `st_wallet` is deliberately separate from `account_wallet`/`payout_wallet`
//     so the USDC payout planner never sees subnet wallets (PLAN.md D-2).
//   - `st_epoch` mirrors the on-chain epoch windows in block numbers; the
//     contract clock is authoritative, never wall clock. `status` progresses
//     open -> closed -> committed -> finalized and never regresses.
//   - `st_payout_leaf` stores one leaf per (epoch, no_id, coldkey) — the
//     contract dedups miner claims by (noId, coldkey), so a coldkey backing
//     multiple networks gets exactly one aggregated leaf. `network_id` is a
//     representative contributing network (min uuid), informational only.
//   - The hot epoch summary is cached in Redis under `{st_epoch}state` as a
//     read-through cache for `GET /sn/epoch`.
//
// All functions are safe for concurrent use.
package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// st epoch lifecycle status values (varchar, not enum, per migration
// conventions).
const (
	StEpochStatusOpen      = "open"
	StEpochStatusClosed    = "closed"
	StEpochStatusCommitted = "committed"
	StEpochStatusFinalized = "finalized"
)

// st publish kinds and statuses for `st_publish` rows.
const (
	StPublishKindCommit      = "commit"
	StPublishKindDeposit     = "deposit"
	StPublishKindDepositPush = "deposit_push"
	StPublishKindFinalize    = "finalize"

	StPublishStatusPending   = "pending"
	StPublishStatusConfirmed = "confirmed"
	StPublishStatusFailed    = "failed"
	// the write was found already applied on chain; no tx was sent
	StPublishStatusSkipped = "skipped"
)

// the Redis key for the hot epoch summary (hash-tagged so any future
// multi-key operations shard together)
const stEpochSummaryRedisKey = "{st_epoch}state"

// StWallet is a network's subtensor claim wallet (ss58 coldkey).
type StWallet struct {
	NetworkId     server.Id
	ColdkeySs58   string
	ColdkeyPubkey [32]byte
	SetTime       time.Time
}

// SetStWallet upserts the claim wallet for a network.
func SetStWallet(ctx context.Context, networkId server.Id, coldkeySs58 string, coldkeyPubkey [32]byte) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_wallet (
                    network_id,
                    coldkey_ss58,
                    coldkey_pubkey,
                    set_time
                )
                VALUES ($1, $2, $3, $4)
                ON CONFLICT (network_id) DO UPDATE
                SET
                    coldkey_ss58 = $2,
                    coldkey_pubkey = $3,
                    set_time = $4
            `,
			networkId,
			coldkeySs58,
			coldkeyPubkey[:],
			server.NowUtc(),
		))
	})
}

// GetStWallet returns the claim wallet for a network, or nil if unset.
func GetStWallet(ctx context.Context, networkId server.Id) *StWallet {
	var wallet *StWallet
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    coldkey_ss58,
                    coldkey_pubkey,
                    set_time
                FROM st_wallet
                WHERE network_id = $1
            `,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				wallet = &StWallet{
					NetworkId: networkId,
				}
				var coldkeyPubkey []byte
				server.Raise(result.Scan(
					&wallet.ColdkeySs58,
					&coldkeyPubkey,
					&wallet.SetTime,
				))
				copy(wallet.ColdkeyPubkey[:], coldkeyPubkey)
			}
		})
	})
	return wallet
}

// GetAllStWalletColdkeys returns the coldkey pubkey for every network with a
// claim wallet set. The table is small (one row per opted-in network), so a
// full read is fine for the epoch close path.
func GetAllStWalletColdkeys(ctx context.Context) map[server.Id][32]byte {
	networkIdColdkeys := map[server.Id][32]byte{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    network_id,
                    coldkey_pubkey
                FROM st_wallet
            `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var coldkeyPubkey []byte
				server.Raise(result.Scan(&networkId, &coldkeyPubkey))
				var coldkey [32]byte
				copy(coldkey[:], coldkeyPubkey)
				networkIdColdkeys[networkId] = coldkey
			}
		})
	})
	return networkIdColdkeys
}

// StEpoch mirrors one contract epoch and its deadline blocks.
// All block fields are contract (EVM) block numbers.
type StEpoch struct {
	Epoch               uint64
	StartBlock          uint64
	CommitDeadlineBlock uint64
	TrailsDeadlineBlock uint64
	FinalizeBlock       uint64
	Status              string
	FinalizedTime       *time.Time
}

// UpsertStEpoch inserts or refreshes an epoch row. The status is only
// advanced, never regressed (open < closed < committed < finalized), so a
// late window refresh cannot un-finalize an epoch.
func UpsertStEpoch(ctx context.Context, epoch *StEpoch) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_epoch (
                    epoch,
                    start_block,
                    commit_deadline_block,
                    trails_deadline_block,
                    finalize_block,
                    status
                )
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (epoch) DO UPDATE
                SET
                    start_block = $2,
                    commit_deadline_block = $3,
                    trails_deadline_block = $4,
                    finalize_block = $5,
                    status = CASE
                        WHEN array_position(ARRAY['open','closed','committed','finalized'], st_epoch.status) <
                             array_position(ARRAY['open','closed','committed','finalized'], EXCLUDED.status)
                            THEN EXCLUDED.status
                        ELSE st_epoch.status
                    END
            `,
			int64(epoch.Epoch),
			int64(epoch.StartBlock),
			int64(epoch.CommitDeadlineBlock),
			int64(epoch.TrailsDeadlineBlock),
			int64(epoch.FinalizeBlock),
			epoch.Status,
		))
	})
}

// SetStEpochStatus advances the status of an epoch (never regresses; see
// UpsertStEpoch). Setting finalized records `finalized_time` once.
func SetStEpochStatus(ctx context.Context, epoch uint64, status string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE st_epoch
                SET
                    status = $2,
                    finalized_time = CASE
                        WHEN $2 = 'finalized' AND finalized_time IS NULL THEN $3
                        ELSE finalized_time
                    END
                WHERE
                    epoch = $1 AND
                    array_position(ARRAY['open','closed','committed','finalized'], status) <
                    array_position(ARRAY['open','closed','committed','finalized'], $2)
            `,
			int64(epoch),
			status,
			server.NowUtc(),
		))
	})
}

func scanStEpoch(result server.PgResult) *StEpoch {
	epoch := &StEpoch{}
	var epochInt, startBlock, commitDeadlineBlock, trailsDeadlineBlock, finalizeBlock int64
	server.Raise(result.Scan(
		&epochInt,
		&startBlock,
		&commitDeadlineBlock,
		&trailsDeadlineBlock,
		&finalizeBlock,
		&epoch.Status,
		&epoch.FinalizedTime,
	))
	epoch.Epoch = uint64(epochInt)
	epoch.StartBlock = uint64(startBlock)
	epoch.CommitDeadlineBlock = uint64(commitDeadlineBlock)
	epoch.TrailsDeadlineBlock = uint64(trailsDeadlineBlock)
	epoch.FinalizeBlock = uint64(finalizeBlock)
	return epoch
}

const stEpochSelectColumns = `
    epoch,
    start_block,
    commit_deadline_block,
    trails_deadline_block,
    finalize_block,
    status,
    finalized_time
`

// GetStEpoch returns one epoch row, or nil.
func GetStEpoch(ctx context.Context, epoch uint64) *StEpoch {
	var stEpoch *StEpoch
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch WHERE epoch = $1`,
			int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				stEpoch = scanStEpoch(result)
			}
		})
	})
	return stEpoch
}

// GetLatestStEpoch returns the highest-numbered epoch row, or nil.
func GetLatestStEpoch(ctx context.Context) *StEpoch {
	var stEpoch *StEpoch
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch ORDER BY epoch DESC LIMIT 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				stEpoch = scanStEpoch(result)
			}
		})
	})
	return stEpoch
}

// GetLatestFinalizedStEpoch returns the highest finalized epoch, or nil.
// Claims are served against finalized epochs by default.
func GetLatestFinalizedStEpoch(ctx context.Context) *StEpoch {
	var stEpoch *StEpoch
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT `+stEpochSelectColumns+`
                FROM st_epoch
                WHERE status = 'finalized'
                ORDER BY epoch DESC
                LIMIT 1
            `,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				stEpoch = scanStEpoch(result)
			}
		})
	})
	return stEpoch
}

// GetStEpochsWithStatus returns epochs in a given status, ascending.
// Used by the sync task to catch up missed per-epoch pipeline steps.
func GetStEpochsWithStatus(ctx context.Context, status string) []*StEpoch {
	stEpochs := []*StEpoch{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch WHERE status = $1 ORDER BY epoch ASC`,
			status,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				stEpochs = append(stEpochs, scanStEpoch(result))
			}
		})
	})
	return stEpochs
}

// StEpochSummary is the hot mirror of the contract clock served by
// `GET /sn/epoch`. Json tags are the client-facing contract — do not change.
type StEpochSummary struct {
	Epoch               uint64 `json:"epoch"`
	StartBlock          uint64 `json:"start_block"`
	CommitDeadlineBlock uint64 `json:"commit_deadline_block"`
	TrailsDeadlineBlock uint64 `json:"trails_deadline_block"`
	FinalizeBlock       uint64 `json:"finalize_block"`
	TEpochBlocks        uint64 `json:"t_epoch_blocks"`
	ChainId             uint64 `json:"chain_id"`
	ContractAddress     string `json:"contract_address"`
}

// SetStEpochSummaryCache writes the hot epoch summary to Redis with a ttl.
// The sync task refreshes it about every minute; the ttl only bounds
// staleness if the task stalls.
func SetStEpochSummaryCache(ctx context.Context, summary *StEpochSummary, ttl time.Duration) {
	summaryJson, err := json.Marshal(summary)
	if err != nil {
		panic(err)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, stEpochSummaryRedisKey, string(summaryJson), ttl).Err())
	})
}

// GetStEpochSummaryCache reads the hot epoch summary from Redis, or nil on
// miss.
func GetStEpochSummaryCache(ctx context.Context) *StEpochSummary {
	var summary *StEpochSummary
	server.Redis(ctx, func(r server.RedisClient) {
		summaryJson, err := r.Get(ctx, stEpochSummaryRedisKey).Result()
		if err == redis.Nil {
			return
		}
		server.Raise(err)
		summary = &StEpochSummary{}
		server.Raise(json.Unmarshal([]byte(summaryJson), summary))
	})
	return summary
}

// StPayoutLeaf is one committed payout tree leaf: a coldkey and its share of
// the epoch pool in basis points. `LeafIndex` is the deterministic input
// order (ascending coldkey bytes) used to rebuild the exact tree.
type StPayoutLeaf struct {
	Epoch     uint64
	NoId      uint64
	NetworkId server.Id
	Coldkey   [32]byte
	ShareBps  int
	LeafIndex int
}

// SetStPayoutLeaves replaces the full leaf set for (epoch, noId). The
// replace makes epoch-close recomputation idempotent before the root is
// committed on chain.
func SetStPayoutLeaves(ctx context.Context, epoch uint64, noId uint64, leaves []*StPayoutLeaf) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_payout_leaf WHERE epoch = $1 AND no_id = $2`,
			int64(epoch),
			int64(noId),
		))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, leaf := range leaves {
				batch.Queue(
					`
                        INSERT INTO st_payout_leaf (
                            epoch,
                            no_id,
                            network_id,
                            coldkey,
                            share_bps,
                            leaf_index
                        )
                        VALUES ($1, $2, $3, $4, $5, $6)
                    `,
					int64(epoch),
					int64(noId),
					leaf.NetworkId,
					leaf.Coldkey[:],
					leaf.ShareBps,
					leaf.LeafIndex,
				)
			}
		})
	})
}

// GetStPayoutLeaves returns the leaf set for (epoch, noId) ordered by
// leaf index — the exact input order for rebuilding the Merkle tree.
func GetStPayoutLeaves(ctx context.Context, epoch uint64, noId uint64) []*StPayoutLeaf {
	leaves := []*StPayoutLeaf{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    network_id,
                    coldkey,
                    share_bps,
                    leaf_index
                FROM st_payout_leaf
                WHERE epoch = $1 AND no_id = $2
                ORDER BY leaf_index ASC
            `,
			int64(epoch),
			int64(noId),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				leaf := &StPayoutLeaf{
					Epoch: epoch,
					NoId:  noId,
				}
				var coldkey []byte
				server.Raise(result.Scan(
					&leaf.NetworkId,
					&coldkey,
					&leaf.ShareBps,
					&leaf.LeafIndex,
				))
				copy(leaf.Coldkey[:], coldkey)
				leaves = append(leaves, leaf)
			}
		})
	})
	return leaves
}

// GetStPayoutLeafForColdkey returns the single leaf for a coldkey in
// (epoch, noId), or nil. This is the claim-proof lookup for one network's
// wallet.
func GetStPayoutLeafForColdkey(ctx context.Context, epoch uint64, noId uint64, coldkey [32]byte) *StPayoutLeaf {
	var leaf *StPayoutLeaf
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    network_id,
                    share_bps,
                    leaf_index
                FROM st_payout_leaf
                WHERE epoch = $1 AND no_id = $2 AND coldkey = $3
            `,
			int64(epoch),
			int64(noId),
			coldkey[:],
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				leaf = &StPayoutLeaf{
					Epoch:   epoch,
					NoId:    noId,
					Coldkey: coldkey,
				}
				server.Raise(result.Scan(
					&leaf.NetworkId,
					&leaf.ShareBps,
					&leaf.LeafIndex,
				))
			}
		})
	})
	return leaf
}

// StPublish is one attempted chain write (commit/deposit/finalize) and its
// outcome. Precedent: `CompletePayment` recording `tx_hash`.
type StPublish struct {
	PublishId  server.Id
	Epoch      uint64
	Kind       string
	TxHash     *string
	Status     string
	Error      *string
	CreateTime time.Time
	UpdateTime time.Time
}

// AddStPublish records a new pending publish and returns its id.
func AddStPublish(ctx context.Context, epoch uint64, kind string) server.Id {
	publishId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_publish (
                    publish_id,
                    epoch,
                    kind,
                    status,
                    create_time,
                    update_time
                )
                VALUES ($1, $2, $3, $4, $5, $5)
            `,
			publishId,
			int64(epoch),
			kind,
			StPublishStatusPending,
			now,
		))
	})
	return publishId
}

// UpdateStPublish resolves a publish with a status, optional tx hash and
// optional error message.
func UpdateStPublish(ctx context.Context, publishId server.Id, status string, txHash *string, errorMessage *string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE st_publish
                SET
                    status = $2,
                    tx_hash = $3,
                    error = $4,
                    update_time = $5
                WHERE publish_id = $1
            `,
			publishId,
			status,
			txHash,
			errorMessage,
			server.NowUtc(),
		))
	})
}

// GetStPublishes returns all publish records for an epoch, oldest first.
func GetStPublishes(ctx context.Context, epoch uint64) []*StPublish {
	publishes := []*StPublish{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    publish_id,
                    kind,
                    tx_hash,
                    status,
                    error,
                    create_time,
                    update_time
                FROM st_publish
                WHERE epoch = $1
                ORDER BY create_time ASC
            `,
			int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				publish := &StPublish{
					Epoch: epoch,
				}
				server.Raise(result.Scan(
					&publish.PublishId,
					&publish.Kind,
					&publish.TxHash,
					&publish.Status,
					&publish.Error,
					&publish.CreateTime,
					&publish.UpdateTime,
				))
				publishes = append(publishes, publish)
			}
		})
	})
	return publishes
}

// StChainEvent is one mirrored contract log, unique on
// (block_number, log_index).
type StChainEvent struct {
	BlockNumber uint64
	LogIndex    int
	TxHash      string
	Kind        string
	DataJson    string
}

// UpsertStEvents inserts events, ignoring rows already mirrored (log ranges
// are re-scanned conservatively, so duplicates are expected).
func UpsertStEvents(ctx context.Context, events []*StChainEvent) {
	if len(events) == 0 {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, event := range events {
				batch.Queue(
					`
                        INSERT INTO st_event (
                            block_number,
                            log_index,
                            tx_hash,
                            kind,
                            data_json
                        )
                        VALUES ($1, $2, $3, $4, $5)
                        ON CONFLICT (block_number, log_index) DO NOTHING
                    `,
					int64(event.BlockNumber),
					event.LogIndex,
					event.TxHash,
					event.Kind,
					event.DataJson,
				)
			}
		})
	})
}

// GetStEvents returns mirrored events in [minBlock, maxBlock], ordered.
func GetStEvents(ctx context.Context, minBlock uint64, maxBlock uint64) []*StChainEvent {
	events := []*StChainEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    block_number,
                    log_index,
                    tx_hash,
                    kind,
                    data_json
                FROM st_event
                WHERE $1 <= block_number AND block_number <= $2
                ORDER BY block_number ASC, log_index ASC
            `,
			int64(minBlock),
			int64(maxBlock),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				event := &StChainEvent{}
				var blockNumber int64
				server.Raise(result.Scan(
					&blockNumber,
					&event.LogIndex,
					&event.TxHash,
					&event.Kind,
					&event.DataJson,
				))
				event.BlockNumber = uint64(blockNumber)
				events = append(events, event)
			}
		})
	})
	return events
}

// parseDepositedEvent extracts (epoch, noId, amount) from a Deposited
// `st_event.data_json` (the decimal-string fields written by the event decoder
// in st_controller.go: `e`, `no_id`, `amount`). Returns ok=false on any
// malformed row.
func parseDepositedEvent(dataJson string) (epoch uint64, noId uint64, amount *big.Int, ok bool) {
	var data struct {
		E      string `json:"e"`
		NoId   string `json:"no_id"`
		Amount string `json:"amount"`
	}
	if err := json.Unmarshal([]byte(dataJson), &data); err != nil {
		return 0, 0, nil, false
	}
	e, err := strconv.ParseUint(data.E, 10, 64)
	if err != nil {
		return 0, 0, nil, false
	}
	n, err := strconv.ParseUint(data.NoId, 10, 64)
	if err != nil {
		return 0, 0, nil, false
	}
	amt, amtOk := new(big.Int).SetString(data.Amount, 10)
	if !amtOk {
		return 0, 0, nil, false
	}
	return e, n, amt, true
}

// SumStDepositedRao sums the α (rao) credited to (epoch, noId) from the
// mirrored Deposited event log (`st_event`). v0.4 (D25) dropped the contract's
// per-NO deposit ledger (DT/totalDT), so the Deposited(e, noId, from, amount)
// event log IS the authoritative per-NO deposit record — both the deposit
// idempotency check and the bringyourctl display sum it from here rather than
// reading contract state. The event carries its own epoch `e`, so this filters
// on that field (deposits are infrequent, so the full scan by kind is cheap and
// matches the GetHeadBoundCkeysInEpoch precedent).
func SumStDepositedRao(ctx context.Context, epoch uint64, noId uint64) *big.Int {
	total := big.NewInt(0)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT data_json
                FROM st_event
                WHERE kind = 'Deposited'
            `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var dataJson string
				server.Raise(result.Scan(&dataJson))
				if e, n, amount, ok := parseDepositedEvent(dataJson); ok && e == epoch && n == noId {
					total.Add(total, amount)
				}
			}
		})
	})
	return total
}

// GetStHighWaterBlock returns the next block the event sync should scan
// from (0 when never synced).
func GetStHighWaterBlock(ctx context.Context) uint64 {
	var highWaterBlock uint64
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT high_water_block FROM st_chain_sync WHERE singleton_id = 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var block int64
				server.Raise(result.Scan(&block))
				highWaterBlock = uint64(block)
			}
		})
	})
	return highWaterBlock
}

// SetStHighWaterBlock advances the event sync high-water mark. The mark
// never moves backward (re-scans are idempotent but pointless).
func SetStHighWaterBlock(ctx context.Context, block uint64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_chain_sync (singleton_id, high_water_block, update_time)
                VALUES (1, $1, $2)
                ON CONFLICT (singleton_id) DO UPDATE
                SET
                    high_water_block = GREATEST(st_chain_sync.high_water_block, $1),
                    update_time = $2
            `,
			int64(block),
			server.NowUtc(),
		))
	})
}

// StNetworkUsage is one network's summed provider payout bytes in an epoch
// window, from `transfer_escrow_sweep` (written by `settleEscrowInTx`).
type StNetworkUsage struct {
	NetworkId       server.Id
	PayoutByteCount int64
}

// GetStEpochNetworkUsage sums `transfer_escrow_sweep.payout_byte_count` per
// provider network with `sweep_time` in [startTime, endTime).
func GetStEpochNetworkUsage(ctx context.Context, startTime time.Time, endTime time.Time) []*StNetworkUsage {
	usages := []*StNetworkUsage{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    network_id,
                    SUM(payout_byte_count) AS payout_byte_count
                FROM transfer_escrow_sweep
                WHERE $1 <= sweep_time AND sweep_time < $2
                GROUP BY network_id
            `,
			startTime,
			endTime,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				usage := &StNetworkUsage{}
				server.Raise(result.Scan(&usage.NetworkId, &usage.PayoutByteCount))
				usages = append(usages, usage)
			}
		})
	})
	return usages
}

// StClientReliability is one provider client's verification counters summed
// over the stat periods that start inside an epoch window, joined to its
// network.
type StClientReliability struct {
	ClientId      server.Id
	NetworkId     server.Id
	Assignments   int64
	Confirmations int64
}

// GetStEpochClientReliability reads the `verify_provider_stats` rollup
// (written by the verify subsystem) for periods overlapping
// [startTime, endTime), summed per client and joined to `network_client`
// for the network attribution.
func GetStEpochClientReliability(ctx context.Context, startTime time.Time, endTime time.Time) []*StClientReliability {
	reliabilities := []*StClientReliability{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    verify_provider_stats.client_id,
                    network_client.network_id,
                    SUM(verify_provider_stats.assignments) AS assignments,
                    SUM(verify_provider_stats.confirmations) AS confirmations
                FROM verify_provider_stats
                INNER JOIN network_client ON
                    network_client.client_id = verify_provider_stats.client_id
                WHERE $1 < verify_provider_stats.period_end AND verify_provider_stats.period_start < $2
                GROUP BY verify_provider_stats.client_id, network_client.network_id
            `,
			startTime,
			endTime,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				reliability := &StClientReliability{}
				server.Raise(result.Scan(
					&reliability.ClientId,
					&reliability.NetworkId,
					&reliability.Assignments,
					&reliability.Confirmations,
				))
				reliabilities = append(reliabilities, reliability)
			}
		})
	})
	return reliabilities
}

// StHeadBinding mirrors one on-chain head-binding registry entry
// (WHITEPAPER §8.4/§11.4): a provider promoted to the head tier, keyed by its
// client public key (ckey — the 32-byte Ed25519 key GetClientPublicKey
// returns, the contract's `clientId`) and bound to a head-tier hotkey/uid.
// `Active` is false once a HeadUnbound supersedes the bind. `UpdateBlock` is
// the contract block of the last transition; it guards out-of-order replays.
type StHeadBinding struct {
	Ckey        [32]byte
	Hotkey      [32]byte
	Uid         uint64
	Active      bool
	UpdateBlock uint64
	UpdateTime  time.Time
}

// UpsertStHeadBinding records a head-binding transition mirrored from a
// HeadBound (active) or HeadUnbound (inactive) event. The sync task drives
// these in block/log order; the update_block guard makes a conservative
// re-scan idempotent — an older event never regresses a newer state, and a
// same-block later log (applied last) wins.
func UpsertStHeadBinding(ctx context.Context, ckey [32]byte, hotkey [32]byte, uid uint64, active bool, updateBlock uint64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_head_binding (
                    ckey,
                    hotkey,
                    uid,
                    active,
                    update_block,
                    update_time
                )
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (ckey) DO UPDATE
                SET
                    hotkey = $2,
                    uid = $3,
                    active = $4,
                    update_block = $5,
                    update_time = $6
                WHERE st_head_binding.update_block <= $5
            `,
			ckey[:],
			hotkey[:],
			int64(uid),
			active,
			int64(updateBlock),
			server.NowUtc(),
		))
	})
}

// parseHeadEventCkey extracts the 32-byte ckey from a HeadBound/HeadUnbound
// `st_event.data_json` (the "0x…"-hex `ckey` field written by the event
// decoder in st_controller.go). Returns ok=false on any malformed row.
func parseHeadEventCkey(dataJson string) ([32]byte, bool) {
	var data struct {
		Ckey string `json:"ckey"`
	}
	if err := json.Unmarshal([]byte(dataJson), &data); err != nil {
		return [32]byte{}, false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(data.Ckey, "0x"))
	if err != nil || len(raw) != 32 {
		return [32]byte{}, false
	}
	var ckey [32]byte
	copy(ckey[:], raw)
	return ckey, true
}

// GetHeadBoundCkeysInEpoch returns every ckey that was in a head-BOUND state at
// any block in the epoch window [startBlock, closeBlock], reconstructed from the
// synced HeadBound/HeadUnbound event log (`st_event`). This is the head-tier
// pool-exclusion set.
//
// It deliberately does NOT use the point-in-time `active` flag: the validator
// pays a head provider native emission per tempo across the whole epoch, so a
// provider that held a top-level UID at ANY point in the epoch earned head
// emission for those tempos and must be dropped from the pool payout for the
// whole epoch (never paid twice). Excluding only the providers still bound at
// close would let a provider dodge the exclusion by calling `unbindHead` one
// block before close while keeping ~all its head emission. The event-log
// interval reconstruction is fully correct, including multiple bind/unbind
// cycles within one epoch.
func GetHeadBoundCkeysInEpoch(ctx context.Context, startBlock uint64, closeBlock uint64) map[[32]byte]bool {
	events := []StHeadEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT kind, data_json, block_number
                FROM st_event
                WHERE kind IN ('HeadBound', 'HeadUnbound') AND block_number <= $1
                ORDER BY block_number ASC, log_index ASC
            `,
			int64(closeBlock),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var kind, dataJson string
				var blockNumber int64
				server.Raise(result.Scan(&kind, &dataJson, &blockNumber))
				if ckey, ok := parseHeadEventCkey(dataJson); ok {
					events = append(events, StHeadEvent{Ckey: ckey, Bound: kind == "HeadBound", Block: uint64(blockNumber)})
				}
			}
		})
	})
	return StHeadBoundCkeysFromEvents(events, startBlock, closeBlock)
}

// StHeadEvent is one parsed HeadBound (Bound=true) / HeadUnbound (Bound=false)
// transition from the mirrored contract event log.
type StHeadEvent struct {
	Ckey  [32]byte
	Bound bool
	Block uint64
}

// StHeadBoundCkeysFromEvents is the pure replay half of
// GetHeadBoundCkeysInEpoch (unit-testable without pg): given the FULL
// (block, log)-ordered head-binding history up to closeBlock, it returns
// every ckey whose bound interval [since, unbindBlock] (or [since, ∞) if
// still bound) overlaps [startBlock, closeBlock] — i.e. since ≤ closeBlock
// && end ≥ startBlock. History must start at the contract deploy (not at
// startBlock) so a ckey bound before the window opens is still excluded.
func StHeadBoundCkeysFromEvents(events []StHeadEvent, startBlock uint64, closeBlock uint64) map[[32]byte]bool {
	type ckeyState struct {
		bound    bool
		since    uint64
		inWindow bool
	}
	states := map[[32]byte]*ckeyState{}
	for _, ev := range events {
		s := states[ev.Ckey]
		if s == nil {
			s = &ckeyState{}
			states[ev.Ckey] = s
		}
		if ev.Bound {
			if !s.bound {
				s.bound = true
				s.since = ev.Block
			}
		} else if s.bound {
			if s.since <= closeBlock && ev.Block >= startBlock {
				s.inWindow = true
			}
			s.bound = false
		}
	}
	ckeys := map[[32]byte]bool{}
	for ckey, s := range states {
		in := s.inWindow
		if s.bound && s.since <= closeBlock {
			// still bound through closeBlock -> bound at closeBlock (in window)
			in = true
		}
		if in {
			ckeys[ckey] = true
		}
	}
	return ckeys
}

// GetStContributingClientCkeys batch-reads the client public key (ckey) for
// each client_id in one Redis MGET (reusing the ckey_<clientId> layout of
// network_client_key_model.go), so the epoch-close head-tier exclusion can
// resolve client_id -> ckey without a per-client round trip. Clients with no
// published key, or a key that is not 32 bytes, are omitted.
func GetStContributingClientCkeys(ctx context.Context, clientIds []server.Id) map[server.Id][32]byte {
	ckeys := map[server.Id][32]byte{}
	if len(clientIds) == 0 {
		return ckeys
	}
	keys := make([]string, len(clientIds))
	for i, clientId := range clientIds {
		keys[i] = clientPublicKeyRedisKey(clientId)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.MGet(ctx, keys...).Result()
		server.Raise(err)
		for i, value := range values {
			// go-redis returns a string on hit, nil on miss
			raw, ok := value.(string)
			if !ok || len(raw) != 32 {
				continue
			}
			var ckey [32]byte
			copy(ckey[:], raw)
			ckeys[clientIds[i]] = ckey
		}
	})
	return ckeys
}
