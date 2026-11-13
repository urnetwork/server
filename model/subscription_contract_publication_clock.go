// A shard's existing final publication statement supplies its completion clock.
package model

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// This exact local signal aborts provisional writes before COMMIT. Other
// statement, cancellation, rollback and unknown-commit failures stay unchanged.
type contractPublicationDeadlineAbort struct{}

// Preserve the domain cause through the transaction's normal panic unwinding.
func (self contractPublicationDeadlineAbort) Error() string {
	return ErrProberShardRetired.Error()
}

// Internal transaction controls can identify the ordinary shard refusal.
func (self contractPublicationDeadlineAbort) Unwrap() error {
	return ErrProberShardRetired
}

// Only an outer creation owner may recover this signal, after its transaction
// has unwound. Recovery does not assert that a rollback reply was acknowledged.
func recoverContractPublicationDeadline(escrow **TransferEscrow, returnErr *error) {
	value := recover()
	if value == nil {
		return
	}
	if _, aborted := value.(contractPublicationDeadlineAbort); !aborted {
		panic(value)
	}
	*escrow, *returnErr = nil, ErrProberShardRetired
}

// Consuming the insert's returning rows makes the aggregate clock follow its
// actual write, including a zero-row insertion. It occupies the same batch slot.
const contractExtenderInsertCompletionClockSql = `WITH inserted AS (` + contractExtenderInsertSql + ` RETURNING 1)
SELECT count(*), clock_timestamp() AT TIME ZONE 'UTC' FROM inserted`

// Legacy positive admission already writes this projection after its extenders.
const netEscrowPublishAdmissionCacheCompletionClockSql = `WITH inserted AS (` + netEscrowPublishAdmissionCacheSQL + ` RETURNING 1)
SELECT count(*), clock_timestamp() AT TIME ZONE 'UTC' FROM inserted`

// Ordinary accounts keep the byte-identical statement and response. A private
// shard reads its completion clock in that existing statement, without another
// acquisition, statement, round trip or additional database lock.
func queueContractExtenderPublication(batch server.PgBatch, contractId, sourceId, destinationId server.Id, deadline *time.Time) *time.Time {
	return queueContractPublicationClock(batch, contractExtenderInsertSql, contractExtenderInsertCompletionClockSql, deadline,
		contractId, sourceId, destinationId, ContractPartySource, ContractPartyDestination)
}

// The last required publication write supplies a clock in its existing reply.
// An ordinary account queues exactly its previous statement and result policy.
func queueContractPublicationClock(batch server.PgBatch, sql, clockSql string, deadline *time.Time, args ...any) *time.Time {
	if deadline == nil {
		batch.Queue(sql, args...)
		return nil
	}
	var completedAt time.Time
	var inserted int64
	batch.Queue(clockSql, args...).QueryRow(func(row pgx.Row) error {
		return row.Scan(&inserted, &completedAt)
	})
	return &completedAt
}

// Provisional rows must roll back when the final financial publication waits
// past the shard's lifetime. Returning a policy error here would commit them.
func requireContractPublicationDeadline(deadline, completedAt *time.Time) {
	if deadline == nil {
		return
	}
	if completedAt == nil || completedAt.IsZero() {
		panic(errors.New("contract publication has no completion clock"))
	}
	if !completedAt.Before(*deadline) {
		panic(contractPublicationDeadlineAbort{})
	}
}
