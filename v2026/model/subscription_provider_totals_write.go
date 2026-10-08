// Validated provider credits and their durable markers share bounded transports.
package model

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

const legacyProviderTotalsWriteBatchLimit = 64

const legacyProviderTotalWriteSql = `INSERT INTO account_balance
    (network_id,provided_byte_count,provided_net_revenue_nano_cents) VALUES($1,$2,$3)
    ON CONFLICT(network_id) DO UPDATE SET
    provided_byte_count=account_balance.provided_byte_count+EXCLUDED.provided_byte_count,
    provided_net_revenue_nano_cents=account_balance.provided_net_revenue_nano_cents+EXCLUDED.provided_net_revenue_nano_cents`

// The locked payload read already validated every allocation and queue identity.
// Keep all credits and markers in that same transaction; at most 64 credits and
// its final marker statement are in flight, including larger multi-provider work.
func writeLegacyProviderTotalsAndMarkersInTx(ctx context.Context, tx server.PgTx, totals []legacyProviderTotal, taskIds []server.Id) error {
	if len(totals) == 0 || len(taskIds) == 0 {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("legacy provider total projection has no allocation or owner"))
	}
	for offset := 0; offset < len(totals); offset += legacyProviderTotalsWriteBatchLimit {
		end := min(offset+legacyProviderTotalsWriteBatchLimit, len(totals))
		page := totals[offset:end]
		var markers []server.Id
		if end == len(totals) {
			markers = taskIds
		}
		if err := writeLegacyProviderTotalsBatchInTx(ctx, tx, page, markers); err != nil {
			return err
		}
		// Close the transport before a test holds the actual transaction or
		// issues another statement. Every production reply is already known.
		for _, total := range page {
			observeAccountBalanceWriteForTest(ctx, tx, total.NetworkId)
		}
	}
	return nil
}

// Consume each statement reply and the final synchronization before returning.
// A failed or missing reply leaves the transaction owner to roll back; the batch
// never survives into commit and no error retries an account write independently.
func writeLegacyProviderTotalsBatchInTx(ctx context.Context, tx server.PgTx, totals []legacyProviderTotal, taskIds []server.Id) (returnErr error) {
	batch := &pgx.Batch{}
	for _, total := range totals {
		batch.Queue(legacyProviderTotalWriteSql, total.NetworkId, total.Bytes, total.Revenue)
	}
	if len(taskIds) != 0 {
		batch.Queue(`UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{applied}','true'::jsonb)::text WHERE task_id=ANY($1)`, taskIds)
	}
	results := tx.SendBatch(ctx, batch)
	phase := legacyProviderTotalsAccountWrite
	defer func() {
		if err := results.Close(); err != nil && !errors.Is(returnErr, err) {
			closeErr := withLegacyProviderTotalsPhase(phase, err)
			if returnErr == nil {
				returnErr = closeErr
			} else {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	for range totals {
		if _, err := results.Exec(); err != nil {
			return withLegacyProviderTotalsPhase(phase, err)
		}
	}
	if len(taskIds) != 0 {
		phase = legacyProviderTotalsAppliedMarker
		tag, err := results.Exec()
		if err != nil {
			return withLegacyProviderTotalsPhase(phase, err)
		}
		if tag.RowsAffected() != int64(len(taskIds)) {
			return withLegacyProviderTotalsPhase(phase, errors.New("legacy provider total marker ownership missing"))
		}
	}
	return nil
}
