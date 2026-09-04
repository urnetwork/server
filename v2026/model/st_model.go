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
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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

// StDeploymentKey is the durable chain identity for coordinator-owned mirror
// and settlement rows. It is deliberately based on chain id + coordinator
// address rather than a reusable deployment label, so a replacement testnet
// contract can never inherit another contract's epochs, checkpoints, events,
// artifacts, or task decisions.
type StDeploymentKey string

func requireStDeploymentKey(deploymentKey StDeploymentKey) string {
	if deploymentKey == "" {
		panic("st deployment key is empty")
	}
	return string(deploymentKey)
}

// The Redis key is deployment-scoped for the same reason as PostgreSQL. The
// hash tag keeps each deployment's summary stable under Redis clustering.
func stEpochSummaryRedisKey(deploymentKey StDeploymentKey) string {
	return fmt.Sprintf("{st_epoch:%s}state", requireStDeploymentKey(deploymentKey))
}

// StWallet is a network's subtensor claim wallet (ss58 coldkey).
type StWallet struct {
	NetworkId     server.Id
	ColdkeySs58   string
	ColdkeyPubkey [32]byte
	SetTime       time.Time
}

// StProviderWallet is the epoch-snapshotted payout coldkey for one logical
// provider client. Wallet changes are prospective: settlement selects the
// newest row whose SetTime is not after the epoch boundary.
type StProviderWallet struct {
	ClientId      server.Id
	NetworkId     server.Id
	ColdkeySs58   string
	ColdkeyPubkey [32]byte
	SetTime       time.Time
}

func SetStProviderWallet(ctx context.Context, clientId server.Id, networkId server.Id, coldkeySs58 string, coldkeyPubkey [32]byte) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
            INSERT INTO st_provider_wallet_history (
                client_id, network_id, coldkey_ss58, coldkey_pubkey, set_time
            ) VALUES ($1, $2, $3, $4, $5)
        `, clientId, networkId, coldkeySs58, coldkeyPubkey[:], server.NowUtc()))
	})
}

// GetStProviderWalletsAt returns the most recent wallet for every provider as
// of boundaryTime. A provider without a prospective wallet is intentionally
// absent and excluded with an auditable reason; its weight is never silently
// redistributed by pretending the network wallet belongs to it.
func GetStProviderWalletsAt(ctx context.Context, boundaryTime time.Time) map[server.Id]*StProviderWallet {
	wallets := map[server.Id]*StProviderWallet{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT DISTINCT ON (client_id)
                client_id, network_id, coldkey_ss58, coldkey_pubkey, set_time
            FROM st_provider_wallet_history
            WHERE set_time <= $1
            ORDER BY client_id, set_time DESC, wallet_version DESC
        `, boundaryTime)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				wallet := &StProviderWallet{}
				var coldkey []byte
				server.Raise(result.Scan(&wallet.ClientId, &wallet.NetworkId, &wallet.ColdkeySs58, &coldkey, &wallet.SetTime))
				copy(wallet.ColdkeyPubkey[:], coldkey)
				wallets[wallet.ClientId] = wallet
			}
		})
	})
	return wallets
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
func UpsertStEpoch(ctx context.Context, deploymentKey StDeploymentKey, epoch *StEpoch) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_epoch (
					deployment_key,
                    epoch,
                    start_block,
                    commit_deadline_block,
                    trails_deadline_block,
                    finalize_block,
                    status
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (deployment_key, epoch) DO UPDATE
                SET
                    start_block = EXCLUDED.start_block,
                    commit_deadline_block = EXCLUDED.commit_deadline_block,
                    trails_deadline_block = EXCLUDED.trails_deadline_block,
                    finalize_block = EXCLUDED.finalize_block,
                    status = CASE
                        WHEN array_position(ARRAY['open','closed','committed','finalized'], st_epoch.status) <
                             array_position(ARRAY['open','closed','committed','finalized'], EXCLUDED.status)
                            THEN EXCLUDED.status
                        ELSE st_epoch.status
                    END
            `,
			key,
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
func SetStEpochStatus(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, status string) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE st_epoch
                SET
                    status = $3,
                    finalized_time = CASE
                        WHEN $3 = 'finalized' AND finalized_time IS NULL THEN $4
                        ELSE finalized_time
                    END
                WHERE
					deployment_key = $1 AND epoch = $2 AND
                    array_position(ARRAY['open','closed','committed','finalized'], status) <
					array_position(ARRAY['open','closed','committed','finalized'], $3)
            `,
			key,
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
func GetStEpoch(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64) *StEpoch {
	key := requireStDeploymentKey(deploymentKey)
	var stEpoch *StEpoch
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch WHERE deployment_key = $1 AND epoch = $2`,
			key,
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
func GetLatestStEpoch(ctx context.Context, deploymentKey StDeploymentKey) *StEpoch {
	key := requireStDeploymentKey(deploymentKey)
	var stEpoch *StEpoch
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch WHERE deployment_key = $1 ORDER BY epoch DESC LIMIT 1`,
			key,
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
func GetLatestFinalizedStEpoch(ctx context.Context, deploymentKey StDeploymentKey) *StEpoch {
	key := requireStDeploymentKey(deploymentKey)
	var stEpoch *StEpoch
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT `+stEpochSelectColumns+`
                FROM st_epoch
				WHERE deployment_key = $1 AND status = 'finalized'
                ORDER BY epoch DESC
                LIMIT 1
            `,
			key,
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
func GetStEpochsWithStatus(ctx context.Context, deploymentKey StDeploymentKey, status string) []*StEpoch {
	key := requireStDeploymentKey(deploymentKey)
	stEpochs := []*StEpoch{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch WHERE deployment_key = $1 AND status = $2 ORDER BY epoch ASC`,
			key,
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
	// release chain settings for direct claims (SDK/web overlay; empty or
	// zero means unset and the client default wins)
	SettlementVaultAddress string `json:"settlement_vault_address,omitempty"`
	NoId                   string `json:"no_id,omitempty"`
	Netuid                 uint64 `json:"netuid,omitempty"`
	RpcUrl                 string `json:"rpc_url,omitempty"`
}

// SetStEpochSummaryCache writes the hot epoch summary to Redis with a ttl.
// The sync task refreshes it about every minute; the ttl only bounds
// staleness if the task stalls.
func SetStEpochSummaryCache(ctx context.Context, deploymentKey StDeploymentKey, summary *StEpochSummary, ttl time.Duration) {
	summaryJson, err := json.Marshal(summary)
	if err != nil {
		panic(err)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, stEpochSummaryRedisKey(deploymentKey), string(summaryJson), ttl).Err())
	})
}

// GetStEpochSummaryCache reads the hot epoch summary from Redis, or nil on
// miss.
func GetStEpochSummaryCache(ctx context.Context, deploymentKey StDeploymentKey) *StEpochSummary {
	var summary *StEpochSummary
	server.Redis(ctx, func(r server.RedisClient) {
		summaryJson, err := r.Get(ctx, stEpochSummaryRedisKey(deploymentKey)).Result()
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
	ClientId  *server.Id
	NetworkId server.Id
	Coldkey   [32]byte
	ShareBps  int
	LeafIndex int
}

// SetStPayoutLeaves replaces the full leaf set for (epoch, noId). The
// replace makes epoch-close recomputation idempotent before the root is
// committed on chain.
func SetStPayoutLeaves(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64, leaves []*StPayoutLeaf) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_payout_leaf WHERE deployment_key = $1 AND epoch = $2 AND no_id = $3`,
			key,
			int64(epoch),
			int64(noId),
		))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, leaf := range leaves {
				batch.Queue(
					`
                        INSERT INTO st_payout_leaf (
							deployment_key,
                            epoch,
                            no_id,
							client_id,
                            network_id,
                            coldkey,
                            share_bps,
                            leaf_index
                        )
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                    `,
					key,
					int64(epoch),
					int64(noId),
					leaf.ClientId,
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
func GetStPayoutLeaves(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64) []*StPayoutLeaf {
	key := requireStDeploymentKey(deploymentKey)
	leaves := []*StPayoutLeaf{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    network_id,
					client_id,
                    coldkey,
                    share_bps,
                    leaf_index
                FROM st_payout_leaf
				WHERE deployment_key = $1 AND epoch = $2 AND no_id = $3
                ORDER BY leaf_index ASC
            `,
			key,
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
					&leaf.ClientId,
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

func GetStPayoutLeafForClient(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, clientId server.Id) *StPayoutLeaf {
	key := requireStDeploymentKey(deploymentKey)
	var leaf *StPayoutLeaf
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT no_id, network_id, coldkey, share_bps, leaf_index
			FROM st_payout_leaf
			WHERE deployment_key = $1 AND epoch = $2 AND client_id = $3
			ORDER BY no_id ASC LIMIT 1
		`, key, int64(epoch), clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				leaf = &StPayoutLeaf{Epoch: epoch, ClientId: &clientId}
				var noId int64
				var coldkey []byte
				server.Raise(result.Scan(&noId, &leaf.NetworkId, &coldkey, &leaf.ShareBps, &leaf.LeafIndex))
				leaf.NoId = uint64(noId)
				copy(leaf.Coldkey[:], coldkey)
			}
		})
	})
	return leaf
}

type StPayoutArtifact struct {
	Epoch       uint64
	NoId        uint64
	ContentHash string
	ContentKey  string
	HistoryKey  string
	PayoutRoot  [32]byte
	CreateTime  time.Time
}

func AddStPayoutArtifact(ctx context.Context, deploymentKey StDeploymentKey, artifact *StPayoutArtifact) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_payout_artifact (
				deployment_key, epoch, no_id, content_hash, content_key, history_key, payout_root, create_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (deployment_key, epoch, no_id) DO NOTHING
		`, key, int64(artifact.Epoch), int64(artifact.NoId), artifact.ContentHash, artifact.ContentKey,
			artifact.HistoryKey, artifact.PayoutRoot[:], artifact.CreateTime))
	})
}

func GetStPayoutArtifact(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64) *StPayoutArtifact {
	key := requireStDeploymentKey(deploymentKey)
	var artifact *StPayoutArtifact
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT content_hash, content_key, history_key, payout_root, create_time
			FROM st_payout_artifact WHERE deployment_key = $1 AND epoch = $2 AND no_id = $3
		`, key, int64(epoch), int64(noId))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				artifact = &StPayoutArtifact{Epoch: epoch, NoId: noId}
				var root []byte
				server.Raise(result.Scan(&artifact.ContentHash, &artifact.ContentKey, &artifact.HistoryKey, &root, &artifact.CreateTime))
				copy(artifact.PayoutRoot[:], root)
			}
		})
	})
	return artifact
}

// GetStPayoutLeafForColdkey returns the single leaf for a coldkey in
// (epoch, noId), or nil. This is the claim-proof lookup for one network's
// wallet.
func GetStPayoutLeafForColdkey(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64, coldkey [32]byte) *StPayoutLeaf {
	key := requireStDeploymentKey(deploymentKey)
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
				WHERE deployment_key = $1 AND epoch = $2 AND no_id = $3 AND coldkey = $4
            `,
			key,
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
func AddStPublish(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, kind string) server.Id {
	key := requireStDeploymentKey(deploymentKey)
	publishId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_publish (
                    publish_id,
					deployment_key,
                    epoch,
                    kind,
                    status,
                    create_time,
                    update_time
                )
				VALUES ($1, $2, $3, $4, $5, $6, $6)
            `,
			publishId,
			key,
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
func GetStPublishes(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64) []*StPublish {
	key := requireStDeploymentKey(deploymentKey)
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
				WHERE deployment_key = $1 AND epoch = $2
                ORDER BY create_time ASC
            `,
			key,
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

// Durable transaction intent statuses.  An intent is the logical operation;
// fee replacements are attempts beneath the same intent and nonce.
const (
	StTxPrepared   = "prepared"
	StTxSigned     = "signed"
	StTxBroadcast  = "broadcast"
	StTxMined      = "mined"
	StTxFinalized  = "finalized"
	StTxReverted   = "reverted"
	StTxInvalid    = "invalid"
	StTxCanceled   = "canceled"
	StTxSuperseded = "superseded"
	// Legacy-only ambiguous terminal state. New code records a canonical EVM
	// revert separately and leaves local corruption nonterminal for repair.
	StTxFailed    = "failed"
	StTxUncertain = "uncertain"

	StTxAttemptReplaced = "replaced"

	StTxAttemptExecution    = "execution"
	StTxAttemptCancellation = "cancellation"
)

type StTransactionIntent struct {
	IntentId      server.Id
	IntentKey     string
	LogicalKey    string
	Generation    int
	Profile       string
	DeploymentId  string
	DeploymentKey StDeploymentKey
	ChainId       uint64
	GenesisHash   string
	FromAddress   string
	ToAddress     string
	CalldataHash  string
	Calldata      []byte
	Nonce         uint64
	Status        string
	CurrentTxHash *string
	AttemptCount  int
	Error         *string
	CreateTime    time.Time
	UpdateTime    time.Time
}

type StTransactionAttempt struct {
	IntentId       server.Id
	Attempt        int
	Kind           string
	TxHash         string
	RawTransaction []byte
	GasLimit       uint64
	GasPrice       *string
	GasTipCap      *string
	GasFeeCap      *string
	Status         string
	InclusionBlock *uint64
	InclusionHash  *string
	FinalizedBlock *uint64
	FinalizedHash  *string
	Error          *string
	CreateTime     time.Time
	UpdateTime     time.Time
}

const stTransactionAttemptColumns = `
	intent_id, attempt, kind, tx_hash, raw_transaction, gas_limit,
	gas_price, gas_tip_cap, gas_fee_cap, status,
	inclusion_block, inclusion_hash, finalized_block, finalized_hash,
	error, create_time, update_time`

// Decodes one durable signed attempt and preserves nullable chain observations.
func scanStTransactionAttempt(row interface{ Scan(...any) error }) (*StTransactionAttempt, error) {
	var attempt StTransactionAttempt
	var gasLimit int64
	var inclusionBlock, finalizedBlock *int64
	err := row.Scan(
		&attempt.IntentId, &attempt.Attempt, &attempt.Kind, &attempt.TxHash, &attempt.RawTransaction, &gasLimit,
		&attempt.GasPrice, &attempt.GasTipCap, &attempt.GasFeeCap, &attempt.Status,
		&inclusionBlock, &attempt.InclusionHash, &finalizedBlock, &attempt.FinalizedHash,
		&attempt.Error, &attempt.CreateTime, &attempt.UpdateTime,
	)
	if err != nil {
		return nil, err
	}
	if gasLimit <= 0 {
		return nil, fmt.Errorf("invalid stored st transaction gas limit: %d", gasLimit)
	}
	attempt.GasLimit = uint64(gasLimit)
	if inclusionBlock != nil {
		block := uint64(*inclusionBlock)
		attempt.InclusionBlock = &block
	}
	if finalizedBlock != nil {
		block := uint64(*finalizedBlock)
		attempt.FinalizedBlock = &block
	}
	return &attempt, nil
}

func scanStTransactionIntent(row interface{ Scan(...any) error }) *StTransactionIntent {
	var v StTransactionIntent
	var chainId, nonce int64
	server.Raise(row.Scan(
		&v.IntentId, &v.IntentKey, &v.LogicalKey, &v.Generation,
		&v.Profile, &v.DeploymentId, &v.DeploymentKey, &chainId, &v.GenesisHash,
		&v.FromAddress, &v.ToAddress, &v.CalldataHash, &v.Calldata, &nonce,
		&v.Status, &v.CurrentTxHash, &v.AttemptCount, &v.Error,
		&v.CreateTime, &v.UpdateTime,
	))
	if chainId <= 0 || nonce < 0 {
		panic(fmt.Errorf("invalid stored st transaction chain/nonce: %d/%d", chainId, nonce))
	}
	v.ChainId, v.Nonce = uint64(chainId), uint64(nonce)
	return &v
}

const stTransactionIntentColumns = `
	intent_id, intent_key, logical_key, generation,
	profile, deployment_id, deployment_key, chain_id, genesis_hash,
	from_address, to_address, calldata_hash, calldata, nonce,
	status, current_tx_hash, attempt_count, error, create_time, update_time`

// The lock follows Ethereum's actual nonce namespace. Human environment and
// deployment labels are metadata and must never create another nonce lane.
func stTransactionAdvisoryLockKey(chainId uint64, genesisHash string, fromAddress string) string {
	return fmt.Sprintf("st-transaction-v2:%d:%s:%s", chainId, strings.ToLower(genesisHash), strings.ToLower(fromAddress))
}

// A logical operation can have another account nonce only after a canonical
// revert consumed the preceding generation without changing contract state.
func stTransactionIntentKey(logicalKey string, generation int) string {
	intentKey := fmt.Sprintf("%s:%d", logicalKey, generation)
	if logicalKey == "" || len(intentKey) > 255 || generation < 0 {
		panic("invalid st transaction logical key or generation")
	}
	return intentKey
}

// ReserveStTransactionIntent atomically reserves the next usable nonce for an
// account and persists the immutable operation before any signing occurs. A
// repeated logical key returns the active generation after byte-for-byte
// validation. Only a canonically reverted generation may create one successor.
// The database, not a process or human deployment label, owns account nonces.
func ReserveStTransactionIntent(
	ctx context.Context,
	logicalKey string,
	profile string,
	deploymentId string,
	deploymentKey StDeploymentKey,
	chainId uint64,
	genesisHash string,
	fromAddress string,
	toAddress string,
	calldataHash string,
	calldata []byte,
	pendingNonce uint64,
) *StTransactionIntent {
	if chainId > uint64(^uint64(0)>>1) || pendingNonce > uint64(^uint64(0)>>1) {
		panic(fmt.Errorf("st transaction chain id or nonce exceeds postgres bigint"))
	}
	key := requireStDeploymentKey(deploymentKey)
	genesisBytes, genesisErr := hex.DecodeString(strings.TrimPrefix(genesisHash, "0x"))
	if fromAddress != strings.ToLower(fromAddress) || toAddress != strings.ToLower(toAddress) ||
		genesisHash != strings.ToLower(genesisHash) || !strings.HasPrefix(genesisHash, "0x") ||
		genesisErr != nil || len(genesisBytes) != 32 {
		panic("st transaction chain identity and addresses must be canonical lowercase")
	}
	var intent *StTransactionIntent
	server.Tx(ctx, func(tx server.PgTx) {
		var ignored any
		server.Raise(tx.QueryRow(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			stTransactionAdvisoryLockKey(chainId, genesisHash, fromAddress),
		).Scan(&ignored))

		row := tx.QueryRow(ctx,
			`SELECT `+stTransactionIntentColumns+`
			 FROM st_transaction_intent WHERE logical_key = $1
			 ORDER BY generation DESC LIMIT 1`,
			logicalKey,
		)
		var existing StTransactionIntent
		var existingChain, existingNonce int64
		err := row.Scan(
			&existing.IntentId, &existing.IntentKey, &existing.LogicalKey, &existing.Generation,
			&existing.Profile, &existing.DeploymentId, &existing.DeploymentKey, &existingChain, &existing.GenesisHash,
			&existing.FromAddress, &existing.ToAddress, &existing.CalldataHash,
			&existing.Calldata, &existingNonce, &existing.Status, &existing.CurrentTxHash,
			&existing.AttemptCount, &existing.Error, &existing.CreateTime, &existing.UpdateTime,
		)
		generation := 0
		if err == nil {
			existing.ChainId, existing.Nonce = uint64(existingChain), uint64(existingNonce)
			if existing.DeploymentKey != deploymentKey || existing.ChainId != chainId || existing.GenesisHash != genesisHash ||
				existing.FromAddress != fromAddress || existing.ToAddress != toAddress ||
				existing.CalldataHash != calldataHash || !bytes.Equal(existing.Calldata, calldata) {
				panic(fmt.Errorf("st transaction intent %q was reused with different immutable data", logicalKey))
			}
			if existing.Status != StTxReverted {
				intent = &existing
				return
			}
			generation = existing.Generation + 1
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			if err != nil {
				server.Raise(err)
			}
		}

		var nonce int64
		server.Raise(tx.QueryRow(ctx, `
			SELECT GREATEST(
				$4::bigint,
				COALESCE(MAX(nonce) + 1, $4::bigint)
			)
			FROM st_transaction_intent
			WHERE chain_id = $1 AND genesis_hash = $2 AND from_address = $3
		`, int64(chainId), genesisHash, fromAddress, int64(pendingNonce)).Scan(&nonce))

		now := server.NowUtc()
		intentId := server.NewId()
		intentKey := stTransactionIntentKey(logicalKey, generation)
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_transaction_intent (
				intent_id, intent_key, logical_key, generation,
				profile, deployment_id, deployment_key, chain_id, genesis_hash,
				from_address, to_address, calldata_hash, calldata, nonce,
				status, create_time, update_time
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
		`, intentId, intentKey, logicalKey, generation, profile, deploymentId, key,
			int64(chainId), genesisHash, fromAddress, toAddress, calldataHash, calldata, nonce, StTxPrepared, now))
		intent = &StTransactionIntent{
			IntentId: intentId, IntentKey: intentKey, LogicalKey: logicalKey, Generation: generation,
			Profile: profile, DeploymentId: deploymentId, DeploymentKey: deploymentKey,
			ChainId: chainId, GenesisHash: genesisHash, FromAddress: fromAddress,
			ToAddress: toAddress, CalldataHash: calldataHash, Calldata: append([]byte(nil), calldata...),
			Nonce: uint64(nonce), Status: StTxPrepared, CreateTime: now, UpdateTime: now,
		}
	})
	return intent
}

// Returns the latest generation, or nil when no such logical operation exists.
func GetStTransactionIntent(ctx context.Context, logicalKey string) *StTransactionIntent {
	var intent *StTransactionIntent
	server.Db(ctx, func(conn server.PgConn) {
		row := conn.QueryRow(ctx,
			`SELECT `+stTransactionIntentColumns+`
			 FROM st_transaction_intent WHERE logical_key = $1
			 ORDER BY generation DESC LIMIT 1`,
			logicalKey,
		)
		var v StTransactionIntent
		var chainId, nonce int64
		err := row.Scan(&v.IntentId, &v.IntentKey, &v.LogicalKey, &v.Generation,
			&v.Profile, &v.DeploymentId, &v.DeploymentKey, &chainId, &v.GenesisHash,
			&v.FromAddress, &v.ToAddress, &v.CalldataHash, &v.Calldata, &nonce,
			&v.Status, &v.CurrentTxHash, &v.AttemptCount, &v.Error, &v.CreateTime, &v.UpdateTime)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		v.ChainId, v.Nonce = uint64(chainId), uint64(nonce)
		intent = &v
	})
	return intent
}

// Returns every nonce whose canonical outcome is still unresolved, ordered so
// callers drain gaps before allocating or broadcasting later account nonces.
func GetUnresolvedStTransactionIntents(ctx context.Context, chainId uint64, genesisHash string, fromAddress string) []*StTransactionIntent {
	intents := []*StTransactionIntent{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT `+stTransactionIntentColumns+`
			FROM st_transaction_intent
			WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3
				AND status NOT IN ($4,$5,$6,$7)
			ORDER BY nonce, generation
		`, int64(chainId), genesisHash, fromAddress,
			StTxFinalized, StTxReverted, StTxCanceled, StTxSuperseded)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				intents = append(intents, scanStTransactionIntent(rows))
			}
		})
	})
	return intents
}

// Atomically elects one signed candidate for the next attempt number. A stale
// concurrent caller receives the already-persisted winner and must broadcast
// those bytes. No database lock is held while callers perform RPC or signing.
func AddStTransactionAttempt(ctx context.Context, candidate *StTransactionAttempt) *StTransactionAttempt {
	if candidate == nil || candidate.Attempt <= 0 ||
		(candidate.Kind != StTxAttemptExecution && candidate.Kind != StTxAttemptCancellation) {
		panic("invalid st transaction attempt candidate")
	}
	var stored *StTransactionAttempt
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		var attemptCount int
		server.Raise(tx.QueryRow(ctx, `
			SELECT status, attempt_count FROM st_transaction_intent
			WHERE intent_id=$1 FOR UPDATE
		`, candidate.IntentId).Scan(&intentStatus, &attemptCount))

		if candidate.Attempt <= attemptCount || stTransactionIntentTerminal(intentStatus) || intentStatus == StTxMined {
			if attemptCount == 0 {
				return
			}
			var err error
			stored, err = scanStTransactionAttempt(tx.QueryRow(ctx, `
				SELECT `+stTransactionAttemptColumns+` FROM st_transaction_attempt
				WHERE intent_id=$1 AND attempt=$2
			`, candidate.IntentId, attemptCount))
			server.Raise(err)
			return
		}
		if candidate.Attempt != attemptCount+1 {
			panic(fmt.Errorf("st transaction attempt %d skips durable predecessor %d", candidate.Attempt, attemptCount))
		}
		if intentStatus != StTxPrepared && intentStatus != StTxSigned && intentStatus != StTxBroadcast && intentStatus != StTxUncertain {
			panic(fmt.Errorf("st transaction intent has invalid signing state %q", intentStatus))
		}
		now := server.NowUtc()
		if attemptCount > 0 {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE st_transaction_attempt SET status = $3, update_time = $4
				WHERE intent_id = $1 AND attempt = $2
					AND status IN ($5,$6,$7)
			`, candidate.IntentId, attemptCount, StTxAttemptReplaced, now,
				StTxSigned, StTxBroadcast, StTxUncertain))
		}
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_transaction_attempt (
				intent_id, attempt, kind, tx_hash, raw_transaction, gas_limit,
				gas_price, gas_tip_cap, gas_fee_cap, status, create_time, update_time
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
		`, candidate.IntentId, candidate.Attempt, candidate.Kind, candidate.TxHash, candidate.RawTransaction,
			int64(candidate.GasLimit), candidate.GasPrice, candidate.GasTipCap, candidate.GasFeeCap,
			StTxSigned, now))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_intent SET status=$2, current_tx_hash=$3,
				attempt_count=$4, error=NULL, update_time=$5 WHERE intent_id=$1
		`, candidate.IntentId, StTxSigned, candidate.TxHash, candidate.Attempt, now))
		stored = &StTransactionAttempt{
			IntentId: candidate.IntentId, Attempt: candidate.Attempt, Kind: candidate.Kind, TxHash: candidate.TxHash,
			RawTransaction: append([]byte(nil), candidate.RawTransaction...), GasLimit: candidate.GasLimit,
			GasPrice: candidate.GasPrice, GasTipCap: candidate.GasTipCap, GasFeeCap: candidate.GasFeeCap,
			Status: StTxSigned, CreateTime: now, UpdateTime: now,
		}
	})
	return stored
}

// Chain-terminal outcomes absorb every weaker or late observation.
func stTransactionIntentTerminal(status string) bool {
	return status == StTxFinalized || status == StTxReverted || status == StTxCanceled || status == StTxSuperseded || status == StTxFailed
}

func GetCurrentStTransactionAttempt(ctx context.Context, intentId server.Id) *StTransactionAttempt {
	attempts := GetStTransactionAttempts(ctx, intentId)
	if len(attempts) == 0 {
		return nil
	}
	return attempts[0]
}

// GetStTransactionAttempts returns newest first so reconciliation checks a
// replacement before its predecessors while still accepting whichever one
// became canonical for the shared nonce.
func GetStTransactionAttempts(ctx context.Context, intentId server.Id) []*StTransactionAttempt {
	attempts := []*StTransactionAttempt{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT `+stTransactionAttemptColumns+`
			FROM st_transaction_attempt WHERE intent_id=$1 ORDER BY attempt DESC
		`, intentId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				attempt, scanErr := scanStTransactionAttempt(rows)
				server.Raise(scanErr)
				attempts = append(attempts, attempt)
			}
		})
	})
	return attempts
}

func updateStTransactionState(ctx context.Context, intentId server.Id, attempt int, status string, errorMessage *string) {
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		var attemptCount int
		server.Raise(tx.QueryRow(ctx, `
			SELECT status, attempt_count FROM st_transaction_intent
			WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus, &attemptCount))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_attempt SET status=$3, error=$4, update_time=$5
			WHERE intent_id=$1 AND attempt=$2
				AND status IN ($6,$7,$8)
		`, intentId, attempt, status, errorMessage, now,
			StTxSigned, StTxBroadcast, StTxUncertain))
		if attempt == attemptCount && intentStatus != StTxMined {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE st_transaction_intent SET status=$2, error=$3, update_time=$4
				WHERE intent_id=$1
			`, intentId, status, errorMessage, now))
		}
	})
}

func MarkStTransactionBroadcast(ctx context.Context, intentId server.Id, attempt int) {
	updateStTransactionState(ctx, intentId, attempt, StTxBroadcast, nil)
}

func MarkStTransactionUncertain(ctx context.Context, intentId server.Id, attempt int, err error) {
	message := "uncertain"
	if err != nil {
		message = err.Error()
	}
	updateStTransactionState(ctx, intentId, attempt, StTxUncertain, &message)
}

func MarkStTransactionMined(ctx context.Context, intentId server.Id, attempt int, txHash string, block uint64, hash string) {
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		server.Raise(tx.QueryRow(ctx, `
			SELECT status FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_attempt SET status=$3, inclusion_block=$4,
				inclusion_hash=$5, error=NULL, update_time=$6
			WHERE intent_id=$1 AND attempt=$2 AND status NOT IN ($7,$8)
		`, intentId, attempt, StTxMined, int64(block), hash, now,
			StTxFinalized, StTxFailed))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_intent SET status=$2, current_tx_hash=$3, error=NULL, update_time=$4
			WHERE intent_id=$1
		`, intentId, StTxMined, txHash, now))
	})
}

// Invalidates only the exact non-final inclusion previously observed. A late
// timeout cannot use this transition to erase another goroutine's newer mined
// or finalized evidence.
func MarkStTransactionOrphaned(ctx context.Context, intentId server.Id, attempt int, txHash string, block uint64, hash string, err error) {
	message := "canonical inclusion was orphaned"
	if err != nil {
		message = err.Error()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		var currentTxHash *string
		server.Raise(tx.QueryRow(ctx, `
			SELECT status, current_tx_hash FROM st_transaction_intent
			WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus, &currentTxHash))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		result, updateErr := tx.Exec(ctx, `
			UPDATE st_transaction_attempt
			SET status=$6, inclusion_block=NULL, inclusion_hash=NULL, error=$7, update_time=$8
			WHERE intent_id=$1 AND attempt=$2 AND status=$3
				AND inclusion_block=$4 AND inclusion_hash=$5
		`, intentId, attempt, StTxMined, int64(block), hash, StTxUncertain, message, now)
		server.Raise(updateErr)
		if result.RowsAffected() == 1 && currentTxHash != nil && *currentTxHash == txHash {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE st_transaction_intent SET status=$2, error=$3, update_time=$4
				WHERE intent_id=$1
			`, intentId, StTxUncertain, message, now))
		}
	})
}

func MarkStTransactionFinalized(ctx context.Context, intentId server.Id, attempt int, txHash string, block uint64, hash string) {
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		server.Raise(tx.QueryRow(ctx, `
			SELECT status FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_attempt SET status=$3, finalized_block=$4,
				finalized_hash=$5, error=NULL, update_time=$6
			WHERE intent_id=$1 AND attempt=$2 AND status <> $7
		`, intentId, attempt, StTxFinalized, int64(block), hash, now, StTxReverted))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_intent SET status=$2, current_tx_hash=$3, error=NULL, update_time=$4
			WHERE intent_id=$1
		`, intentId, StTxFinalized, txHash, now))
	})
}

func MarkStTransactionReverted(ctx context.Context, intentId server.Id, attempt int, err error) {
	message := "transaction reverted"
	if err != nil {
		message = err.Error()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		server.Raise(tx.QueryRow(ctx, `
			SELECT status FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_attempt SET status=$3, error=$4, update_time=$5
			WHERE intent_id=$1 AND attempt=$2 AND status <> $6
		`, intentId, attempt, StTxReverted, message, now, StTxFinalized))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_intent SET status=$2, error=$3, update_time=$4
			WHERE intent_id=$1
		`, intentId, StTxReverted, message, now))
	})
}

// Records that a finalized self-transaction consumed the obsolete nonce
// without executing the intent's coordinator calldata.
func MarkStTransactionCanceled(ctx context.Context, intentId server.Id, attempt int, txHash string, block uint64, hash string, err error) {
	var errorMessage *string
	if err != nil {
		message := err.Error()
		errorMessage = &message
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		server.Raise(tx.QueryRow(ctx, `
			SELECT status FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_attempt SET status=$3, finalized_block=$4,
				finalized_hash=$5, error=$6, update_time=$7
			WHERE intent_id=$1 AND attempt=$2
		`, intentId, attempt, StTxCanceled, int64(block), hash, errorMessage, now))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_intent SET status=$2, current_tx_hash=$3,
				error=$4, update_time=$5 WHERE intent_id=$1
		`, intentId, StTxCanceled, txHash, errorMessage, now))
	})
}

// Records finalized account-nonce consumption for which none of the durable
// attempt hashes is canonical. The unknown transaction cannot be overwritten.
func MarkStTransactionSuperseded(ctx context.Context, intentId server.Id, err error) {
	message := "account nonce was consumed by an unknown transaction"
	if err != nil {
		message = err.Error()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var intentStatus string
		server.Raise(tx.QueryRow(ctx, `
			SELECT status FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE
		`, intentId).Scan(&intentStatus))
		if stTransactionIntentTerminal(intentStatus) {
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_attempt SET status=$2, error=$3, update_time=$4
			WHERE intent_id=$1 AND status NOT IN ($5,$6)
		`, intentId, StTxSuperseded, message, now, StTxFinalized, StTxReverted))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_transaction_intent SET status=$2, error=$3, update_time=$4
			WHERE intent_id=$1
		`, intentId, StTxSuperseded, message, now))
	})
}

// StChainEvent is one mirrored contract log, unique on
// (block_number, log_index).
type StChainEvent struct {
	BlockNumber uint64
	BlockHash   string
	LogIndex    int
	TxHash      string
	Kind        string
	DataJson    string
}

// UpsertStEvents inserts events, ignoring rows already mirrored (log ranges
// are re-scanned conservatively, so duplicates are expected).
func UpsertStEvents(ctx context.Context, deploymentKey StDeploymentKey, events []*StChainEvent) {
	if len(events) == 0 {
		return
	}
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, event := range events {
				batch.Queue(
					`
                        INSERT INTO st_event (
							deployment_key,
                            block_number,
							block_hash,
                            log_index,
                            tx_hash,
                            kind,
                            data_json
                        )
						VALUES ($1, $2, $3, $4, $5, $6, $7)
						ON CONFLICT (deployment_key, block_number, log_index) DO NOTHING
                    `,
					key,
					int64(event.BlockNumber),
					event.BlockHash,
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
func GetStEvents(ctx context.Context, deploymentKey StDeploymentKey, minBlock uint64, maxBlock uint64) []*StChainEvent {
	key := requireStDeploymentKey(deploymentKey)
	events := []*StChainEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    block_number,
					block_hash,
                    log_index,
                    tx_hash,
                    kind,
                    data_json
                FROM st_event
				WHERE deployment_key = $1 AND $2 <= block_number AND block_number <= $3
                ORDER BY block_number ASC, log_index ASC
            `,
			key,
			int64(minBlock),
			int64(maxBlock),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				event := &StChainEvent{}
				var blockNumber int64
				server.Raise(result.Scan(
					&blockNumber,
					&event.BlockHash,
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

type StChainCheckpoint struct {
	NextBlock uint64
	BlockHash string // canonical hash of NextBlock-1; empty only before first scan
}

func GetStChainCheckpoint(ctx context.Context, deploymentKey StDeploymentKey) StChainCheckpoint {
	key := requireStDeploymentKey(deploymentKey)
	checkpoint := StChainCheckpoint{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT high_water_block, block_hash FROM st_chain_sync WHERE deployment_key = $1 AND singleton_id = 1`, key)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var block int64
				server.Raise(result.Scan(&block, &checkpoint.BlockHash))
				checkpoint.NextBlock = uint64(block)
			}
		})
	})
	return checkpoint
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
func SumStDepositedRao(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64) *big.Int {
	key := requireStDeploymentKey(deploymentKey)
	total := big.NewInt(0)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT data_json
                FROM st_event
				WHERE deployment_key = $1 AND kind = 'Deposited'
            `,
			key,
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

// SumStDepositedInBlockRangeRao sums the α (rao) of every mirrored
// Deposited event with block_number in [minBlock, maxBlock), across epochs
// and NOs — the demand deposits inside a wall-clock window mapped to chain
// blocks (the public stats collector, controller/stats_collector.go). The
// kind+block index covers the scan.
func SumStDepositedInBlockRangeRao(ctx context.Context, deploymentKey StDeploymentKey, minBlock uint64, maxBlock uint64) *big.Int {
	key := requireStDeploymentKey(deploymentKey)
	total := big.NewInt(0)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT data_json
                FROM st_event
				WHERE deployment_key = $1 AND kind = 'Deposited' AND $2 <= block_number AND block_number < $3
            `,
			key,
			int64(minBlock),
			int64(maxBlock),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var dataJson string
				server.Raise(result.Scan(&dataJson))
				if _, _, amount, ok := parseDepositedEvent(dataJson); ok {
					total.Add(total, amount)
				}
			}
		})
	})
	return total
}

// parsePoolSweptEvent extracts the measured α (rao) from a PoolSwept
// `st_event.data_json` (the decimal-string fields written by the event
// decoder in st_controller.go: `no_id`, `measured`, `swept`, `move_ok`).
// Returns ok=false on any malformed row.
func parsePoolSweptEvent(dataJson string) (measured *big.Int, ok bool) {
	var data struct {
		Measured string `json:"measured"`
	}
	if err := json.Unmarshal([]byte(dataJson), &data); err != nil {
		return nil, false
	}
	m, mOk := new(big.Int).SetString(data.Measured, 10)
	if !mOk {
		return nil, false
	}
	return m, true
}

// SumStPoolSweptMeasuredInBlockRangeRao sums the measured α (rao) of every
// mirrored PoolSwept event with block_number in [minBlock, maxBlock) — the
// miner emission captured by the D-4 sweeps inside a wall-clock window
// mapped to chain blocks (the public stats collector,
// controller/stats_collector.go).
func SumStPoolSweptMeasuredInBlockRangeRao(ctx context.Context, deploymentKey StDeploymentKey, minBlock uint64, maxBlock uint64) *big.Int {
	key := requireStDeploymentKey(deploymentKey)
	total := big.NewInt(0)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT data_json
                FROM st_event
				WHERE deployment_key = $1 AND kind = 'PoolSwept' AND $2 <= block_number AND block_number < $3
            `,
			key,
			int64(minBlock),
			int64(maxBlock),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var dataJson string
				server.Raise(result.Scan(&dataJson))
				if measured, ok := parsePoolSweptEvent(dataJson); ok {
					total.Add(total, measured)
				}
			}
		})
	})
	return total
}

// parseMinerClaimedEvent extracts the claiming coldkey and the claimed α
// (rao) from a MinerClaimed `st_event.data_json` (the fields written by the
// event decoder in st_controller.go: `e`, `no_id`, `coldkey`, `share_bps`,
// `amount`, `caller`). Returns ok=false on any malformed row.
func parseMinerClaimedEvent(dataJson string) (coldkey string, amount *big.Int, ok bool) {
	var data struct {
		Coldkey string `json:"coldkey"`
		Amount  string `json:"amount"`
	}
	if err := json.Unmarshal([]byte(dataJson), &data); err != nil {
		return "", nil, false
	}
	if data.Coldkey == "" {
		return "", nil, false
	}
	amt, amtOk := new(big.Int).SetString(data.Amount, 10)
	if !amtOk {
		return "", nil, false
	}
	return data.Coldkey, amt, true
}

// SumStMinerClaimedInBlockRange sums the α (rao) of every mirrored
// MinerClaimed event with block_number in [minBlock, maxBlock) and counts
// the distinct claiming coldkeys — the miner payouts claimed inside a
// wall-clock window mapped to chain blocks (the public stats collector,
// controller/stats_collector.go). The kind+block index covers the scan.
func SumStMinerClaimedInBlockRange(ctx context.Context, deploymentKey StDeploymentKey, minBlock uint64, maxBlock uint64) (amountRao *big.Int, minerCount int64) {
	key := requireStDeploymentKey(deploymentKey)
	amountRao = big.NewInt(0)
	coldkeys := map[string]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT data_json
                FROM st_event
				WHERE deployment_key = $1 AND kind = 'MinerClaimed' AND $2 <= block_number AND block_number < $3
            `,
			key,
			int64(minBlock),
			int64(maxBlock),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var dataJson string
				server.Raise(result.Scan(&dataJson))
				if coldkey, amount, ok := parseMinerClaimedEvent(dataJson); ok {
					amountRao.Add(amountRao, amount)
					coldkeys[coldkey] = true
				}
			}
		})
	})
	return amountRao, int64(len(coldkeys))
}

// GetStHighWaterBlock returns the next block the event sync should scan
// from (0 when never synced).
func GetStHighWaterBlock(ctx context.Context, deploymentKey StDeploymentKey) uint64 {
	return GetStChainCheckpoint(ctx, deploymentKey).NextBlock
}

// SetStHighWaterBlock advances the event sync high-water mark. The mark
// never moves backward (re-scans are idempotent but pointless).
func SetStHighWaterBlock(ctx context.Context, deploymentKey StDeploymentKey, block uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_chain_sync (deployment_key, singleton_id, high_water_block, block_hash, update_time)
				VALUES ($1, 1, $2, '', $3)
				ON CONFLICT (deployment_key, singleton_id) DO UPDATE
                SET
					high_water_block = EXCLUDED.high_water_block,
					block_hash = EXCLUDED.block_hash,
					update_time = EXCLUDED.update_time
				WHERE st_chain_sync.high_water_block < EXCLUDED.high_water_block
            `,
			key,
			int64(block),
			server.NowUtc(),
		))
	})
}

// SetStChainCheckpoint records the exact canonical boundary. It intentionally
// does not use GREATEST: a reconciler may explicitly rewind after operator
// review, while normal callers verify monotonicity and parent hashes first.
func SetStChainCheckpoint(ctx context.Context, deploymentKey StDeploymentKey, checkpoint StChainCheckpoint) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_chain_sync (deployment_key, singleton_id, high_water_block, block_hash, update_time)
				VALUES ($1, 1, $2, $3, $4)
				ON CONFLICT (deployment_key, singleton_id) DO UPDATE
                SET
					high_water_block = EXCLUDED.high_water_block,
					block_hash = EXCLUDED.block_hash,
					update_time = EXCLUDED.update_time
            `,
			key,
			int64(checkpoint.NextBlock),
			checkpoint.BlockHash,
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

// StProviderUsage is settled payout traffic attributed to the actual
// destination/provider client. destination_id is denormalized onto the sweep
// row at settlement time, so later network membership changes cannot rewrite
// an old epoch.
type StProviderUsage struct {
	ClientId        server.Id
	NetworkId       server.Id
	PayoutByteCount int64
}

func GetStEpochProviderUsage(ctx context.Context, startTime time.Time, endTime time.Time) []*StProviderUsage {
	usages := []*StProviderUsage{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT destination_id, network_id, SUM(payout_byte_count)::bigint
            FROM transfer_escrow_sweep
            WHERE $1 <= sweep_time AND sweep_time < $2 AND destination_id IS NOT NULL
            GROUP BY destination_id, network_id
        `, startTime, endTime)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				usage := &StProviderUsage{}
				server.Raise(result.Scan(&usage.ClientId, &usage.NetworkId, &usage.PayoutByteCount))
				usages = append(usages, usage)
			}
		})
	})
	return usages
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
func UpsertStHeadBinding(ctx context.Context, deploymentKey StDeploymentKey, ckey [32]byte, hotkey [32]byte, uid uint64, active bool, updateBlock uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO st_head_binding (
					deployment_key,
                    ckey,
                    hotkey,
                    uid,
                    active,
                    update_block,
                    update_time
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (deployment_key, ckey) DO UPDATE
                SET
					hotkey = EXCLUDED.hotkey,
					uid = EXCLUDED.uid,
					active = EXCLUDED.active,
					update_block = EXCLUDED.update_block,
					update_time = EXCLUDED.update_time
				WHERE st_head_binding.update_block <= EXCLUDED.update_block
            `,
			key,
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
func GetHeadBoundCkeysInEpoch(ctx context.Context, deploymentKey StDeploymentKey, startBlock uint64, closeBlock uint64) map[[32]byte]bool {
	key := requireStDeploymentKey(deploymentKey)
	events := []StHeadEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT kind, data_json, block_number
                FROM st_event
				WHERE deployment_key = $1 AND kind IN ('HeadBound', 'HeadUnbound') AND block_number <= $2
                ORDER BY block_number ASC, log_index ASC
            `,
			key,
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
// each client_id (reusing the ckey_<clientId> layout of
// network_client_key_model.go), so the epoch-close head-tier exclusion can
// resolve client_id -> ckey without a per-client round trip. Clients with no
// published key, or a key that is not 32 bytes, are omitted.
// The ckey_ keys carry no shared hash tag, so a single MGET cannot span them
// on the cluster (CROSSSLOT); a plain pipeline auto-routes each GET per slot.
func GetStContributingClientCkeys(ctx context.Context, clientIds []server.Id) map[server.Id][32]byte {
	ckeys := map[server.Id][32]byte{}
	if len(clientIds) == 0 {
		return ckeys
	}
	server.Redis(ctx, func(r server.RedisClient) {
		cmds := make([]*redis.StringCmd, len(clientIds))
		// the aggregate error is ignored deliberately: it is the FIRST
		// per-command error in command order, which redis.Nil (an expected
		// miss) can mask — errors must be classified per command below. A
		// real error must raise: silently skipping it would omit the
		// client→ckey mapping and let a head-bound provider through the
		// head-tier exclusion into a double payout, immutable once the
		// epoch root is committed
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i, clientId := range clientIds {
				cmds[i] = pipe.Get(ctx, clientPublicKeyRedisKey(clientId))
			}
			return nil
		})
		for i, cmd := range cmds {
			raw, err := cmd.Result()
			if errors.Is(err, redis.Nil) {
				// no published ckey for this client; expected
				continue
			}
			server.Raise(err)
			if len(raw) != 32 {
				// malformed value; treat as unpublished
				continue
			}
			var ckey [32]byte
			copy(ckey[:], raw)
			ckeys[clientIds[i]] = ckey
		}
	})
	return ckeys
}
