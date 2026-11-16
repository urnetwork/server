package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/urnetwork/server"
)

// GetStDemandState is the demand deposit funder's state and its version; nil at
// version 0 before its first write.
func GetStDemandState(ctx context.Context, deploymentKey StDeploymentKey) (state []byte, version uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state, version FROM st_demand_state WHERE deployment_key = $1`, key)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var stored int64
				server.Raise(result.Scan(&state, &stored))
				version = uint64(stored)
			}
		})
	})
	return state, version
}

// SaveStDemandState replaces the state if it is still at version and returns the
// new version. ok is false when another writer got there first, which leaves the
// row as that writer left it.
func SaveStDemandState(ctx context.Context, deploymentKey StDeploymentKey, state []byte, version uint64) (next uint64, ok bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		var affected int64
		if version == 0 {
			tag, err := tx.Exec(ctx, `
				INSERT INTO st_demand_state (deployment_key, version, state, update_time)
				VALUES ($1, 1, $2, $3)
				ON CONFLICT (deployment_key) DO NOTHING
			`, key, state, now)
			server.Raise(err)
			affected = tag.RowsAffected()
		} else {
			tag, err := tx.Exec(ctx, `
				UPDATE st_demand_state SET version = $3, state = $4, update_time = $5
				WHERE deployment_key = $1 AND version = $2
			`, key, int64(version), int64(version+1), state, now)
			server.Raise(err)
			affected = tag.RowsAffected()
		}
		ok = affected == 1
	})
	return version + 1, ok
}

// AddStDemandEvent appends one step of a conversion, for accounting.
func AddStDemandEvent(ctx context.Context, deploymentKey StDeploymentKey, eventTime time.Time, epoch uint64, kind string, data map[string]any) {
	key := requireStDeploymentKey(deploymentKey)
	encoded, err := json.Marshal(data)
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_demand_event (event_id, deployment_key, event_time, epoch, kind, data)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, server.NewId(), key, eventTime, int64(epoch), kind, encoded))
	})
}

// GetStDemandPriceSheet is the last price sheet that verified for a deployment,
// exactly as it was published; nil before the first.
func GetStDemandPriceSheet(ctx context.Context, deploymentKey StDeploymentKey) []byte {
	key := requireStDeploymentKey(deploymentKey)
	var raw []byte
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT raw FROM st_demand_price_sheet WHERE deployment_key = $1`, key)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&raw))
			}
		})
	})
	return raw
}

// SaveStDemandPriceSheet replaces a deployment's verified price sheet.
func SaveStDemandPriceSheet(ctx context.Context, deploymentKey StDeploymentKey, raw []byte) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_demand_price_sheet (deployment_key, raw, update_time)
			VALUES ($1, $2, $3)
			ON CONFLICT (deployment_key) DO UPDATE SET raw = EXCLUDED.raw, update_time = EXCLUDED.update_time
		`, key, raw, server.NowUtc()))
	})
}

// ErrStLedgerV2EpochUsageFixed refuses a usage statement that differs from the
// epoch's recorded one.
var ErrStLedgerV2EpochUsageFixed = errors.New("st ledger v2: the epoch's usage statement is already recorded")

// StLedgerV2EpochUsage is a settlement v2 epoch's usage record. Statement is nil
// until it is built, after the epoch's root commit is mirrored.
type StLedgerV2EpochUsage struct {
	NoId            uint64
	Epoch           uint64
	TotalUsers      uint64
	UsersTime       time.Time
	TotalUsageBytes uint64
	ProviderRows    uint64
	ContentHash     string
	Statement       []byte
	StatementTime   *time.Time
}

const stLedgerV2EpochUsageColumns = `no_id, epoch, total_users, users_time, total_usage_bytes::text, provider_rows, content_hash, statement, statement_time`

func scanStLedgerV2EpochUsage(scan func(dest ...any) error) *StLedgerV2EpochUsage {
	var noId, epoch, users int64
	var totalBytes, contentHash *string
	var rows *int64
	usage := &StLedgerV2EpochUsage{}
	server.Raise(scan(&noId, &epoch, &users, &usage.UsersTime, &totalBytes, &rows, &contentHash, &usage.Statement, &usage.StatementTime))
	usage.NoId, usage.Epoch, usage.TotalUsers = uint64(noId), uint64(epoch), uint64(users)
	if totalBytes != nil {
		parsed, err := strconv.ParseUint(*totalBytes, 10, 64)
		server.Raise(err)
		usage.TotalUsageBytes = parsed
	}
	if rows != nil {
		usage.ProviderRows = uint64(*rows)
	}
	if contentHash != nil {
		usage.ContentHash = *contentHash
	}
	return usage
}

// GetStLedgerV2EpochUsage is an epoch's usage record, nil before its users are
// captured.
func GetStLedgerV2EpochUsage(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *StLedgerV2EpochUsage {
	key := requireStDeploymentKey(deploymentKey)
	var usage *StLedgerV2EpochUsage
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT `+stLedgerV2EpochUsageColumns+` FROM st_ledger_v2_epoch_usage WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3`, key, int64(noId), int64(epoch))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				usage = scanStLedgerV2EpochUsage(result.Scan)
			}
		})
	})
	return usage
}

// GetLatestStLedgerV2EpochStatement is the pool's newest recorded usage statement,
// nil before the first.
func GetLatestStLedgerV2EpochStatement(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) *StLedgerV2EpochUsage {
	key := requireStDeploymentKey(deploymentKey)
	var usage *StLedgerV2EpochUsage
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT `+stLedgerV2EpochUsageColumns+`
			FROM st_ledger_v2_epoch_usage
			WHERE deployment_key = $1 AND no_id = $2 AND statement IS NOT NULL
			ORDER BY epoch DESC
			LIMIT 1
		`, key, int64(noId))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				usage = scanStLedgerV2EpochUsage(result.Scan)
			}
		})
	})
	return usage
}

// CaptureStLedgerV2EpochUsers records an epoch's user count once and returns the
// recorded count. A later count, which can only be larger, never replaces it:
// the deposit's amount derives from it, and its calldata is immutable once retained.
func CaptureStLedgerV2EpochUsers(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, users uint64) uint64 {
	key := requireStDeploymentKey(deploymentKey)
	var recorded int64
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_ledger_v2_epoch_usage (deployment_key, no_id, epoch, total_users, users_time)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
		`, key, int64(noId), int64(epoch), int64(users), server.NowUtc()))
		server.Raise(tx.QueryRow(ctx, `
			SELECT total_users FROM st_ledger_v2_epoch_usage
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
		`, key, int64(noId), int64(epoch)).Scan(&recorded))
	})
	return uint64(recorded)
}

// RecordStLedgerV2EpochStatement records an epoch's usage statement once.
// Recording the same statement again is a no-op; any other is
// ErrStLedgerV2EpochUsageFixed, and an epoch without its users captured is an error.
func RecordStLedgerV2EpochStatement(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, totalUsageBytes uint64, providerRows uint64, contentHash string, statement []byte) error {
	key := requireStDeploymentKey(deploymentKey)
	var refused error
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_ledger_v2_epoch_usage
			SET total_usage_bytes = $4::text::numeric, provider_rows = $5, content_hash = $6, statement = $7, statement_time = $8
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3 AND statement IS NULL
		`, key, int64(noId), int64(epoch), strconv.FormatUint(totalUsageBytes, 10), int64(providerRows), contentHash, statement, server.NowUtc()))
		if tag.RowsAffected() == 1 {
			return
		}
		var stored []byte
		var storedHash *string
		err := tx.QueryRow(ctx, `
			SELECT statement, content_hash FROM st_ledger_v2_epoch_usage
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
		`, key, int64(noId), int64(epoch)).Scan(&stored, &storedHash)
		if err != nil {
			refused = fmt.Errorf("st ledger v2: epoch %d has no captured users: %w", epoch, err)
			return
		}
		if !bytes.Equal(stored, statement) || storedHash == nil || *storedHash != contentHash {
			refused = fmt.Errorf("%w: epoch %d", ErrStLedgerV2EpochUsageFixed, epoch)
		}
	})
	return refused
}
