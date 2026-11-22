// A live observation proves an unresolved contract through one exact boundary.
// It shares settlement's row fence and never assigns an earlier SQL snapshot.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

const providerWorkOpenBatchSize = 64
const providerWorkOpenTimeout = 300 * time.Second

// The finite observation owner preserves any earlier caller deadline. Its
// expected database reads use the same approved 300-second recovery budget.
func providerWorkOpenContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(providerWorkSessionContext(ctx), providerWorkOpenTimeout)
}

// Capture only actual unresolved rows after their original reservation. The
// dedicated configured source owns signing; the artifact publisher supplies no
// key. First retained bytes are replayed even when the contract later closes.
func RetainProviderWorkOpenObservations(ctx context.Context, contractIds []server.Id, epoch, block uint64, blockHash [32]byte, boundary time.Time) (originals [][]byte, resultErr error) {
	if ctx == nil {
		return nil, protocol.ErrProviderWorkUnavailable
	}
	if len(contractIds) > 32768 {
		return nil, protocol.ErrProviderWorkCapacity
	}
	if block == 0 || blockHash == ([32]byte{}) || boundary.UnixMicro() <= 0 || !boundary.Equal(boundary.Truncate(time.Microsecond)) {
		return nil, protocol.ErrProviderWorkIntegrity
	}
	ids := slices.Clone(contractIds)
	slices.SortFunc(ids, func(a, b server.Id) int { return a.Cmp(b) })
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return [][]byte{}, ctx.Err()
	}
	if ids[0] == (server.Id{}) {
		return nil, protocol.ErrProviderWorkIntegrity
	}
	ctx, cancel := providerWorkOpenContext(ctx)
	defer cancel()
	epochText := strconv.FormatUint(epoch, 10)
	blockText := strconv.FormatUint(block, 10)
	source := providerWorkSessionSourceFromContext(ctx)
	server.HandleError(func() {
		if source != nil {
			for start := 0; start < len(ids); start += providerWorkOpenBatchSize {
				server.Raise(ctx.Err())
				batch := ids[start:min(start+providerWorkOpenBatchSize, len(ids))]
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(retainProviderWorkOpenBatchInTx(ctx, tx, source, batch, epoch, block, epochText, blockText, blockHash, boundary))
				}, server.TxReadCommitted)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT contract_id,receipt_hash,original FROM provider_work_open_original
 WHERE contract_id=ANY($1) AND epoch=$2::numeric AND block_hash=$3 ORDER BY contract_id LIMIT 32769`, ids, epochText, blockHash[:])
			server.Raise(err)
			defer rows.Close()
			used := 0
			originals = [][]byte{}
			for rows.Next() {
				var id server.Id
				var retainedHash, raw []byte
				server.Raise(rows.Scan(&id, &retainedHash, &raw))
				if len(originals) >= 32768 || len(raw) > 8*1024*1024-used {
					server.Raise(protocol.ErrProviderWorkCapacity)
				}
				hash := sha256.Sum256(raw)
				original, err := protocol.DecodeProviderWorkReceipt(ctx, raw)
				server.Raise(err)
				open := original.Open
				if !bytes.Equal(hash[:], retainedHash) || open == nil || open.ContractId != id.String() || open.Epoch != epoch || open.Block != block || open.BlockHash != blockHash || open.BoundaryUnixMicro != boundary.UnixMicro() {
					server.Raise(protocol.ErrProviderWorkIntegrity)
				}
				originals = append(originals, bytes.Clone(raw))
				used += len(raw)
			}
			server.Raise(rows.Err())
		})
	}, func(err error) { resultErr = err })
	if resultErr = errors.Join(resultErr, ctx.Err()); resultErr != nil {
		return nil, resultErr
	}
	slices.SortFunc(originals, bytes.Compare)
	return originals, nil
}

// ReadCommitted plus the exact settlement row lock makes the post-lock clock
// authoritative. The immutable terminal guard forbids reopening a closed row.
func retainProviderWorkOpenBatchInTx(ctx context.Context, tx server.PgTx, source *ProviderWorkSessionSource, ids []server.Id, epoch, block uint64, epochText, blockText string, blockHash [32]byte, boundary time.Time) error {
	// Keep the outcome check opaque to legacy false-zero global partial
	// indexes; this owner is bounded by the requested primary-key cohort.
	rows, err := tx.Query(ctx, `/* provider-work-open-owner */ SELECT c.contract_id,r.receipt_hash,r.original
 FROM transfer_contract c JOIN provider_work_reservation_original r USING(contract_id)
 WHERE c.contract_id=ANY($1) AND (CASE WHEN c.outcome IS NULL THEN true ELSE false END)
 ORDER BY c.contract_id FOR UPDATE OF c`, ids)
	if err != nil {
		return err
	}
	type candidate struct {
		id           server.Id
		reservation  []byte
		retainedHash []byte
	}
	candidates := []candidate{}
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.id, &value.retainedHash, &value.reservation); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(candidates) == 0 {
		return err
	}
	// A separate statement reads time only after every returned row is locked.
	var observed time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&observed); err != nil {
		return err
	}
	at := observed.UnixMicro()
	if observed.Before(boundary) || at < source.authority.FromUnixMicro || at >= source.authority.ThroughUnixMicro {
		return nil
	}
	// A contender can retain the first original while this transaction waits
	// for its row fence. Refresh this existence read after acquiring that fence.
	retained := map[server.Id]bool{}
	rows, err = tx.Query(ctx, `SELECT contract_id FROM provider_work_open_original WHERE contract_id=ANY($1) AND epoch=$2::numeric AND block_hash=$3`, ids, epochText, blockHash[:])
	if err != nil {
		return err
	}
	for rows.Next() {
		var id server.Id
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		retained[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, value := range candidates {
		if retained[value.id] {
			continue
		}
		hash := sha256.Sum256(value.reservation)
		original, err := protocol.DecodeProviderWorkReceipt(ctx, value.reservation)
		if err != nil || !bytes.Equal(hash[:], value.retainedHash) {
			return errors.Join(protocol.ErrProviderWorkIntegrity, err)
		}
		reservation := original.Reservation
		if reservation == nil || reservation.ContractId != value.id.String() {
			return protocol.ErrProviderWorkIntegrity
		}
		if original.DomainHash != source.authority.DomainHash || reservation.RequestFrameHash == nil || reservation.UsageOriginIsSource == nil || reservation.CreatedAtUnixMicro >= boundary.UnixMicro() {
			continue
		}
		body := protocol.ProviderWorkOpenObservation{ContractId: value.id.String(), ReservationHash: hash, Epoch: epoch, Block: block, BlockHash: blockHash, BoundaryUnixMicro: boundary.UnixMicro(), ObservedAtUnixMicro: at}
		_, raw, receiptHash, err := source.sign(ctx, protocol.ProviderWorkReceipt{Open: &body}, at)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO provider_work_open_original
 (contract_id,epoch,block_number,block_hash,boundary_time,observed_at,reservation_hash,receipt_hash,original)
 VALUES($1,$2::numeric,$3::numeric,$4,$5,$6,$7,$8,$9) ON CONFLICT(contract_id,epoch,block_hash) DO NOTHING`, value.id, epochText, blockText, blockHash[:], boundary.UTC(), observed.UTC(), hash[:], receiptHash[:], raw); err != nil {
			return err
		}
	}
	return nil
}
