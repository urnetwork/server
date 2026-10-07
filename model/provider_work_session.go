// Live session owners sign only mutations from their current transaction.
// An unsigned original event is permanent evidence of incomplete coverage.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// Optional provenance uses a savepoint so missing rollout tables or a bounded
// evidence refusal cannot abort ordinary connection and accounting traffic.
func providerWorkOptionalInTx(ctx context.Context, tx server.PgTx, fn func(server.PgTx) error) bool {
	if providerWorkSessionSourceFromContext(ctx) == nil {
		return false
	}
	return providerWorkOptionalSchemaInTx(ctx, tx, fn)
}

// Fences also apply to unsigned current callers. Before the optional migration
// exists, a savepoint preserves their original operation and compatibility.
// A savepoint that cannot be created raises: the transaction is aborted, its
// context canceled or its connection lost, so the caller cannot go on either.
func providerWorkOptionalSchemaInTx(ctx context.Context, tx server.PgTx, fn func(server.PgTx) error) bool {
	optional := server.RaisePgResult(tx.Begin(ctx))
	if err := fn(optional); err != nil {
		// Cancellation can close pgx before savepoint cleanup. Preserve the
		// refused operation and caller stop when cleanup reports only conn closed.
		rollbackErr := optional.Rollback(ctx)
		if rollbackErr != nil || ctx.Err() != nil {
			server.Raise(errors.Join(err, rollbackErr, ctx.Err()))
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40001" {
			server.Raise(err)
		}
		return false
	}
	server.Raise(optional.Commit(ctx))
	return true
}

// Lock keys, rather than UUID order, define the order because advisory hashes
// may collide. Both request directions and connection owners use this function.
func providerWorkLockEndpointsInTx(ctx context.Context, tx server.PgTx, clientIds ...server.Id) {
	providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error {
		if _, err := optional.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(-776::bigint)`); err != nil {
			return err
		}
		return providerWorkLockEndpointReadRowsInTx(ctx, optional, clientIds)
	})
}

// Current writers prelock every endpoint. The shared bridge supports databases
// that still have the original v776 functions; the repair removes its exclusive
// holder and makes conflicting rolling writers retry before waiting on a fence.
func providerWorkLockSessionMutationInTx(ctx context.Context, tx server.PgTx, clientIds ...server.Id) bool {
	if len(clientIds) == 0 {
		return false
	}
	return providerWorkOptionalSchemaInTx(ctx, tx, func(optional server.PgTx) error {
		if _, err := optional.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(-776::bigint)`); err != nil {
			return err
		}
		if err := providerWorkLockEndpointRowsInTx(ctx, optional, clientIds); err != nil {
			return err
		}
		_, err := optional.Exec(ctx, `SELECT set_config('urnetwork.provider_work_cooperating','1',true)`)
		return err
	})
}

var providerWorkEndpointWriteLockSQL = server.TaggedDatabaseStatement(`SELECT pg_advisory_xact_lock(776,lock_key) FROM
   (SELECT DISTINCT ('x'||substr(md5(client_id::text),1,8))::bit(32)::int AS lock_key
    FROM unnest($1::uuid[]) AS client_id ORDER BY lock_key) AS locks`)
var providerWorkEndpointWriteHeadSQL = server.TaggedDatabaseStatement(`SELECT client_id FROM provider_work_session_head WHERE client_id=ANY($1) ORDER BY client_id FOR UPDATE`)

// A head updated after a repeatable-read snapshot must retry the transaction;
// otherwise an advisory wait alone could certify a stale connected set.
func providerWorkLockEndpointRowsInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id) error {
	_, err := tx.Exec(ctx, providerWorkEndpointWriteLockSQL, clientIds)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, providerWorkEndpointWriteHeadSQL, clientIds)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	return err
}

// Contract publication reads a stable endpoint cut without changing its head.
// Shared fences allow concurrent contract readers; connection mutations retain
// exclusive fences on the same keys. The row lock still rejects stale snapshots.
func providerWorkLockEndpointReadRowsInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(776,lock_key) FROM
   (SELECT DISTINCT ('x'||substr(md5(client_id::text),1,8))::bit(32)::int AS lock_key
    FROM unnest($1::uuid[]) AS client_id ORDER BY lock_key) AS locks`, clientIds)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT client_id FROM provider_work_session_head WHERE client_id=ANY($1) ORDER BY client_id FOR SHARE`, clientIds)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	return err
}

// A failed optional lock acquisition cannot later become a complete receipt.
// PostgreSQL represents the second signed advisory key as an unsigned oid.
func providerWorkRequireEndpointFencesInTx(ctx context.Context, tx server.PgTx, clientIds ...server.Id) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT NOT EXISTS(
	 SELECT 1 FROM unnest($1::uuid[]) AS client_id WHERE NOT EXISTS(
	  SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted
	   AND classid=776 AND objsubid=2
	   AND objid::bigint=(('x'||substr(md5(client_id::text),1,8))::bit(32)::int::bigint & 4294967295)
	 ))`, clientIds).Scan(&held)
	if err != nil {
		return err
	}
	if !held {
		return errors.New("provider work original endpoint fence is absent")
	}
	return nil
}

// Only a live first admission may observe an empty genesis. An existing head
// permanently prevents rebasing after a gap, restart, cleanup, or key rotation.
func providerWorkSessionGenesisInTx(ctx context.Context, tx server.PgTx, clientId server.Id) {
	providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error {
		if err := providerWorkRequireEndpointFencesInTx(ctx, optional, clientId); err != nil {
			return err
		}
		var empty bool
		if err := optional.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM provider_work_session_head WHERE client_id=$1)
   AND NOT EXISTS(SELECT 1 FROM network_client_connection WHERE client_id=$1 AND connected)`, clientId).Scan(&empty); err != nil {
			return err
		}
		if !empty {
			return nil
		}
		_, err := optional.Exec(ctx, `SELECT provider_work_session_append($1,NULL,'baseline',NULL)`, clientId)
		if err != nil {
			return err
		}
		return providerWorkSignSessionEventsInTx(ctx, optional, []server.Id{clientId})
	})
}

// Sign only events issued by this transaction; old SQL rows are never promoted
// to originals when a signer later becomes available.
func providerWorkSignSessionEventsInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id) error {
	source := providerWorkSessionSourceFromContext(ctx)
	if source == nil {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT e.client_id,e.network_id,e.connection_id,e.sequence,e.kind,e.observed_at,e.extender_id
  FROM provider_work_session_event e LEFT JOIN provider_work_session_receipt r USING(client_id,sequence)
  WHERE e.transaction_id=txid_current() AND e.client_id=ANY($1) AND r.client_id IS NULL
  ORDER BY e.client_id,e.sequence LIMIT $2`, clientIds, int64(source.authority.MaxEndpointEvents)+1)
	if err != nil {
		return err
	}
	type event struct {
		clientId     server.Id
		networkId    *server.Id
		connectionId *server.Id
		sequence     uint64
		kind         string
		at           time.Time
		extenderId   *server.Id
	}
	events := []event{}
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.clientId, &e.networkId, &e.connectionId, &e.sequence, &e.kind, &e.at, &e.extenderId); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(events) > int(source.authority.MaxEndpointEvents) {
		return errors.New("provider work live event batch exceeds authority")
	}
	for _, e := range events {
		if e.networkId == nil {
			continue
		}
		body := protocol.ProviderWorkSessionEvent{ClientId: e.clientId.String(), NetworkId: e.networkId.String(), Sequence: e.sequence, Kind: e.kind, ObservedAtUnixMicro: e.at.UnixMicro()}
		if e.extenderId != nil {
			body.ExtenderId = e.extenderId.String()
		}
		if e.connectionId != nil {
			body.ConnectionId = e.connectionId.String()
		}
		if e.sequence > 1 {
			var previous []byte
			if err := tx.QueryRow(ctx, `SELECT receipt_hash FROM provider_work_session_receipt WHERE client_id=$1 AND sequence=$2`, e.clientId, e.sequence-1).Scan(&previous); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return err
			}
			if len(previous) != 32 {
				return errors.New("provider work preceding event hash is invalid")
			}
			copy(body.PreviousHash[:], previous)
		}
		_, raw, hash, err := source.sign(ctx, protocol.ProviderWorkReceipt{Session: &body}, body.ObservedAtUnixMicro)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO provider_work_session_receipt(client_id,sequence,receipt_hash,original) VALUES($1,$2,$3,$4)`, e.clientId, e.sequence, hash[:], raw); err != nil {
			return err
		}
	}
	return nil
}

// A retirement event keeps the admitted identity, rather than reading the
// directory after a later cleanup. All original event bytes are append-only.
func providerWorkRetainSessionEventsInTx(ctx context.Context, tx server.PgTx, clientIds ...server.Id) {
	providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error { return providerWorkSignSessionEventsInTx(ctx, optional, clientIds) })
}

// Read a bounded exact chain at the reservation fence. Missing originals,
// unknown genesis and active extenders all preserve incomplete attribution.
func providerWorkEndpointHeadInTx(ctx context.Context, tx server.PgTx, clientId, networkId server.Id) (protocol.ProviderWorkEndpointHead, bool, error) {
	source := providerWorkSessionSourceFromContext(ctx)
	head := protocol.ProviderWorkEndpointHead{ClientId: clientId.String(), NetworkId: networkId.String()}
	var sequence uint64
	if err := tx.QueryRow(ctx, `SELECT sequence FROM provider_work_session_head WHERE client_id=$1`, clientId).Scan(&sequence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return head, false, nil
		}
		return head, false, err
	}
	if sequence > uint64(source.authority.MaxEndpointEvents) {
		return head, false, nil
	}
	rows, err := tx.Query(ctx, `SELECT original FROM provider_work_session_receipt WHERE client_id=$1 AND sequence<=$2 ORDER BY sequence`, clientId, sequence)
	if err != nil {
		return head, false, err
	}
	var previous [32]byte
	active := map[string]bool{}
	activeExtenders := map[string]bool{}
	var count uint64
	complete := true
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return head, false, err
		}
		receipt, err := protocol.DecodeProviderWorkReceipt(ctx, raw)
		if err != nil {
			rows.Close()
			return head, false, err
		}
		count++
		e := receipt.Session
		if e == nil || e.ClientId != head.ClientId || e.NetworkId != head.NetworkId || e.Sequence != count || e.PreviousHash != previous || receipt.SourceId != source.authority.SourceId || receipt.Generation != source.authority.Generation || receipt.PublicKey != source.authority.PublicKey || receipt.DomainHash != source.authority.DomainHash {
			complete = false
			continue
		}
		switch e.Kind {
		case "baseline":
			if count != 1 || e.ConnectionId != "" {
				complete = false
			}
		case "admit":
			if count == 1 || active[e.ConnectionId] {
				complete = false
			}
			active[e.ConnectionId] = true
			if e.Extender != nil || e.ExtenderId != "" {
				activeExtenders[e.ConnectionId] = true
			}
		case "retire":
			if !active[e.ConnectionId] {
				complete = false
			}
			delete(active, e.ConnectionId)
			delete(activeExtenders, e.ConnectionId)
		default:
			complete = false
		}
		previous, err = receipt.ContentHash(ctx)
		if err != nil {
			rows.Close()
			return head, false, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return head, false, err
	}
	if count != sequence || previous == ([32]byte{}) {
		return head, false, nil
	}
	// The independently replayed active set must match the actual original
	// reservation census. A legacy writer or unsupported extender cannot vanish.
	rows, err = tx.Query(ctx, `SELECT connection_id,extender_id FROM network_client_connection WHERE client_id=$1 AND connected ORDER BY connection_id LIMIT $2`, clientId, int64(source.authority.MaxEndpointEvents)+1)
	if err != nil {
		return head, false, err
	}
	for rows.Next() {
		var connectionId server.Id
		var extenderId *server.Id
		if err := rows.Scan(&connectionId, &extenderId); err != nil {
			rows.Close()
			return head, false, err
		}
		if !active[connectionId.String()] || extenderId != nil {
			complete = false
		}
		delete(active, connectionId.String())
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return head, false, err
	}
	if len(active) != 0 || len(activeExtenders) != 0 {
		complete = false
	}
	head.Sequence = sequence
	head.HeadHash = previous
	return head, complete, nil
}

// Preserve the original reservation identity and both complete endpoint cuts
// immediately after the immutable contract/extender insertion, under its fence.
func providerWorkRetainReservationInTx(ctx context.Context, tx server.PgTx, contractId server.Id) {
	providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error {
		var sourceId, sourceNetworkId, destinationId, destinationNetworkId server.Id
		var at time.Time
		var capacity int64
		var originIsSource *bool
		if err := optional.QueryRow(ctx, `SELECT source_id,source_network_id,destination_id,destination_network_id,create_time,transfer_byte_count,usage_origin_is_source FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&sourceId, &sourceNetworkId, &destinationId, &destinationNetworkId, &at, &capacity, &originIsSource); err != nil {
			return err
		}
		if capacity < 0 {
			return errors.New("provider work reservation capacity is negative")
		}
		if err := providerWorkRequireEndpointFencesInTx(ctx, optional, sourceId, destinationId); err != nil {
			return err
		}
		sourceHead, sourceComplete, err := providerWorkEndpointHeadInTx(ctx, optional, sourceId, sourceNetworkId)
		if err != nil {
			return err
		}
		destinationHead, destinationComplete, err := providerWorkEndpointHeadInTx(ctx, optional, destinationId, destinationNetworkId)
		if err != nil {
			return err
		}
		body := protocol.ProviderWorkReservation{ContractId: contractId.String(), SourceId: sourceId.String(), SourceNetworkId: sourceNetworkId.String(), DestinationId: destinationId.String(), DestinationNetworkId: destinationNetworkId.String(), CreatedAtUnixMicro: at.UnixMicro(), Capacity: uint64(capacity), SourceHead: sourceHead, DestinationHead: destinationHead, Complete: sourceComplete && destinationComplete}
		body.UsageOriginIsSource = originIsSource
		requestHash, _ := ctx.Value(providerWorkRequestFrameContextKey{}).([32]byte)
		if requestHash != ([32]byte{}) {
			body.RequestFrameHash = &requestHash
		}
		body.Complete = body.Complete && body.UsageOriginIsSource != nil && body.RequestFrameHash != nil
		_, raw, hash, err := providerWorkSessionSourceFromContext(ctx).sign(ctx, protocol.ProviderWorkReceipt{Reservation: &body}, body.CreatedAtUnixMicro)
		if err != nil {
			return err
		}
		_, err = optional.Exec(ctx, `INSERT INTO provider_work_reservation_original(contract_id,source_id,source_sequence,destination_id,destination_sequence,receipt_hash,original) VALUES($1,$2,$3,$4,$5,$6,$7)`, contractId, sourceId, sourceHead.Sequence, destinationId, destinationHead.Sequence, hash[:], raw)
		return err
	})
}

// Return only retained originals; payout reconstruction never signs a mutable
// SQL projection. Pooling deduplicates shared endpoint histories and streams.
func ListProviderWorkOriginals(ctx context.Context, contractIds []server.Id) ([][]byte, error) {
	if len(contractIds) == 0 {
		return [][]byte{}, nil
	}
	if len(contractIds) > 32768 {
		return nil, errors.New("provider work original contract census exceeds capacity")
	}
	originals := [][]byte{}
	var resultErr error
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `WITH contract_ids AS MATERIALIZED (
	   SELECT unnest($1::uuid[]) AS contract_id
	   UNION SELECT s.origin_contract_id FROM provider_work_stream_contract c
	    JOIN provider_work_stream_original s USING(stream_id) WHERE c.contract_id=ANY($1)
	  ), selected AS MATERIALIZED (
	   SELECT r.* FROM provider_work_reservation_original r JOIN contract_ids USING(contract_id)
  ), cuts AS (
   SELECT source_id AS client_id,source_sequence AS sequence FROM selected
   UNION ALL SELECT destination_id,destination_sequence FROM selected
	  ), heads AS (SELECT client_id,max(sequence) AS sequence FROM cuts GROUP BY client_id), originals AS (
	   SELECT receipt_hash,original FROM selected
	   UNION ALL SELECT o.receipt_hash,o.original FROM provider_work_outcome_original o WHERE contract_id=ANY($1)
	   UNION ALL SELECT s.receipt_hash,s.original FROM provider_work_stream_original s
	    WHERE EXISTS(SELECT 1 FROM provider_work_stream_contract c WHERE c.stream_id=s.stream_id AND c.contract_id=ANY($1))
	   UNION ALL SELECT r.receipt_hash,r.original FROM provider_work_session_receipt r JOIN heads h ON r.client_id=h.client_id AND r.sequence<=h.sequence
	  ) SELECT receipt_hash,original FROM originals LIMIT 32769`, contractIds)
		if err != nil {
			resultErr = err
			return
		}
		defer rows.Close()
		used := 0
		seen := map[[32]byte]bool{}
		count := 0
		for rows.Next() {
			var raw, retainedHash []byte
			if err := rows.Scan(&retainedHash, &raw); err != nil {
				resultErr = err
				return
			}
			if count >= 32768 || len(raw) > 8*1024*1024-used {
				resultErr = errors.New("provider work originals exceed capacity")
				return
			}
			count++
			hash := sha256.Sum256(raw)
			if !bytes.Equal(hash[:], retainedHash) {
				resultErr = errors.New("provider work retained original hash differs")
				return
			}
			if seen[hash] {
				continue
			}
			seen[hash] = true
			originals = append(originals, bytes.Clone(raw))
			used += len(raw)
		}
		resultErr = rows.Err()
	})
	if resultErr != nil {
		return nil, resultErr
	}
	slices.SortFunc(originals, bytes.Compare)
	return originals, nil
}
