// Database collection owns one read-only repeatable-read snapshot per source.
// It never calls the status-filtered live recovery query or edits historical rows.
package strecovery

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Each call owns and joins one connection selected by a private pinned URL.
// No production vault, process-global database pool or fallback is selected.
type PostgresReader struct {
	ReadTimeout time.Duration
	// A private deterministic barrier may refuse, never manufacture a connection.
	beforeConnect func(context.Context, *pgx.ConnConfig) error
}

// Credentials stay in the connection file and are never returned in errors.
func (self PostgresReader) Snapshot(ctx context.Context, source DatabaseSource, limits Limits) (result *DatabaseSnapshot, resultErr error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	ctx, cancel, err := censusReadOwner(ctx, self.ReadTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	raw, err := readPrivateFile(ctx, source.Connection.Path, 16*1024)
	if err != nil {
		return nil, &CensusReadError{Source: source.Id, Stage: "connection input", Cause: errors.Join(err, ctx.Err())}
	}
	if digest(raw) != source.Connection.Sha256 {
		return nil, &Refusal{Source: source.Id, Cause: "connection file differs from its exact pin"}
	}
	connectionUrl, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || connectionUrl.Scheme != "postgres" && connectionUrl.Scheme != "postgresql" || connectionUrl.Hostname() == "" ||
		connectionUrl.User == nil || connectionUrl.User.Username() == "" || strings.Trim(connectionUrl.Path, "/") == "" || connectionUrl.Query().Get("sslmode") == "" {
		return nil, &Refusal{Source: source.Id, Cause: "connection file needs an explicit PostgreSQL URL, user, database and sslmode"}
	}
	config, err := pgx.ParseConfig(connectionUrl.String())
	if err != nil || config.Host != connectionUrl.Hostname() || config.User != connectionUrl.User.Username() || config.Database != strings.TrimPrefix(connectionUrl.Path, "/") {
		return nil, &Refusal{Source: source.Id, Cause: "connection configuration changes its explicit database identity"}
	}
	config.Password, _ = connectionUrl.User.Password()
	deadline, _ := ctx.Deadline()
	config.Fallbacks, config.ConnectTimeout = nil, time.Until(deadline)
	config.RuntimeParams = map[string]string{"application_name": "urnetwork-operator-census", "timezone": "UTC"}
	if self.beforeConnect != nil {
		if err := self.beforeConnect(ctx, config); err != nil {
			return nil, &CensusReadError{Source: source.Id, Stage: "connection admission", Cause: errors.Join(err, ctx.Err())}
		}
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, &CensusReadError{Source: source.Id, Stage: "connection", Cause: errors.Join(err, ctx.Err())}
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := connection.Close(closeCtx); err != nil {
			result, resultErr = nil, errors.Join(resultErr, &CensusReadError{Source: source.Id, Stage: "connection close", Cause: err})
		}
		<-connection.PgConn().CleanupDone()
		if err := ctx.Err(); err != nil {
			result, resultErr = nil, errors.Join(resultErr, err)
		}
	}()
	result, err = readDatabaseSnapshot(ctx, connection, limits, nil)
	if err != nil {
		return nil, &CensusReadError{Source: source.Id, Stage: "complete read-only snapshot", Cause: errors.Join(err, ctx.Err())}
	}
	return result, nil
}

// The scoped hook exposes the established snapshot to deterministic database
// tests. It is absent from the public collector and receives no write authority.
func readDatabaseSnapshot(ctx context.Context, connection *pgx.Conn, limits Limits, afterSnapshot func(pgx.Tx) error) (result *DatabaseSnapshot, resultErr error) {
	if ctx == nil || connection == nil {
		return nil, errors.New("database snapshot context or connection is absent")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(closeCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			result, resultErr = nil, errors.Join(resultErr, err)
		}
	}()
	var intentCount, attemptCount, totalBytes, maximumBytes int64
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(calldata)),0), COALESCE(max(octet_length(calldata)),0) FROM public.st_transaction_intent`).Scan(&intentCount, &totalBytes, &maximumBytes); err != nil {
		return nil, err
	}
	var attemptBytes, maximumAttemptBytes int64
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(raw_transaction)),0), COALESCE(max(octet_length(raw_transaction)),0) FROM public.st_transaction_attempt`).Scan(&attemptCount, &attemptBytes, &maximumAttemptBytes); err != nil {
		return nil, err
	}
	if intentCount > int64(limits.MaximumIntents) || attemptCount > int64(limits.MaximumAttempts) || maximumBytes > int64(limits.MaximumTransactionBytes) ||
		maximumAttemptBytes > int64(limits.MaximumTransactionBytes) || totalBytes+attemptBytes > int64(limits.MaximumTotalBytes) {
		return nil, errors.New("complete database snapshot exceeds its explicit bounds")
	}
	if afterSnapshot != nil {
		if err := afterSnapshot(tx); err != nil {
			return nil, err
		}
	}
	result = &DatabaseSnapshot{Intents: []Intent{}, Attempts: []Attempt{}}
	rows, err := tx.Query(ctx, `SELECT intent_id::text, intent_key, logical_key, generation, profile, deployment_id, deployment_key,
		chain_id, genesis_hash, from_address, to_address, calldata_hash, calldata, nonce, status, current_tx_hash, attempt_count, error, create_time, update_time
		FROM public.st_transaction_intent ORDER BY intent_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var intent Intent
		if err := rows.Scan(&intent.Id, &intent.IntentKey, &intent.LogicalKey, &intent.Generation, &intent.Profile, &intent.DeploymentId, &intent.DeploymentKey,
			&intent.ChainId, &intent.Genesis, &intent.From, &intent.To, &intent.CalldataHash, &intent.Calldata, &intent.Nonce, &intent.Status, &intent.CurrentHash,
			&intent.AttemptCount, &intent.Error, &intent.CreateTime, &intent.UpdateTime); err != nil {
			rows.Close()
			return nil, err
		}
		result.Intents = append(result.Intents, intent)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT intent_id::text, attempt, kind, tx_hash, raw_transaction, gas_limit, gas_price, gas_tip_cap, gas_fee_cap, status,
		inclusion_block, inclusion_hash, finalized_block, finalized_hash, error, create_time, update_time
		FROM public.st_transaction_attempt ORDER BY intent_id, attempt`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var attempt Attempt
		if err := rows.Scan(&attempt.IntentId, &attempt.Number, &attempt.Kind, &attempt.Hash, &attempt.Raw, &attempt.GasLimit, &attempt.GasPrice, &attempt.GasTipCap,
			&attempt.GasFeeCap, &attempt.Status, &attempt.InclusionBlock, &attempt.InclusionHash, &attempt.FinalizedBlock, &attempt.FinalizedHash,
			&attempt.Error, &attempt.CreateTime, &attempt.UpdateTime); err != nil {
			rows.Close()
			return nil, err
		}
		result.Attempts = append(result.Attempts, attempt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result.Intents) != int(intentCount) || len(result.Attempts) != int(attemptCount) {
		return nil, errors.New("database snapshot row counts changed or were truncated")
	}
	if err := errors.Join(ctx.Err(), tx.Commit(ctx)); err != nil {
		return nil, err
	}
	return result, nil
}
