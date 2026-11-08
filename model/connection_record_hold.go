package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// What the hourly connection-record families share, tls-3's
// tls_connection_record and mx-2's mail_connection_record: the batch insert
// that makes a missing hour and stores once more, and the §21.9 hold check.
//
// The §21.9 hold check of the hourly connection-record families, tls-3's
// tls_connection_record and mx-2's mail_connection_record: a range drops
// whole at 24 h only once every hold has copied out its network's rows (owner
// decision 133; §2.8's header). copied_rows, the copy that lets a held range
// drop, is ABUSE-M5-15's (model.AbuseHoldCopyPending), which replaces this
// check's body when it lands. Until then no hold's copy is known to be done,
// so every hold that covers rows of a range keeps the range: the closed
// direction, which may keep a range past its 24 h while a hold lives and never
// loses a row a hold covers. With no hold placed, which holds_enabled keeps
// true until counsel confirms, every range drops on time.
//
// A hold covers a network's server rows whatever its scope
// (plan-ABUSE-hold-format #1), and only the rows that existed at its
// placement: a row written later is new collection, which a hold never makes.
// A row's time column stands for when it was written, which it precedes by at
// most the writer's batch, so a range that began after the placement holds no
// row the hold covers.

// Why range r, whose leaf of the family's first table is leaf, must stay: a
// live hold, placed after r began, whose network has a row in leaf from before
// the placement. "" when none does. A part-0 row with no placement or expiry
// reads as live and covering, since a hold that cannot say what it spares
// spares nothing. timeColumn is the column the family partitions on. leaf may
// be attached or, inside the drop's transaction, just detached.
func connectionRecordRangeHold(ctx context.Context, conn server.PgQuerier, leaf string, timeColumn string, r server.PartitionRange, now time.Time) string {
	if r.Lower.IsZero() {
		// the registry gives these families neither a legacy nor a DEFAULT
		// partition, so an open range is nothing this check can judge
		return fmt.Sprintf("%s has no lower bound to judge holds against", leaf)
	}
	var caseId server.Id
	var found bool
	result, err := conn.Query(
		ctx,
		fmt.Sprintf(
			`
			SELECT hold.case_id
			FROM abuse_legal_hold hold
			WHERE
				hold.part = 0 AND
				(hold.place_time IS NULL OR $1 < hold.place_time) AND
				(hold.expire_time IS NULL OR $2 < hold.expire_time) AND
				EXISTS (
					SELECT 1
					FROM public.%[1]s record
					WHERE
						record.network_id = hold.network_id AND
						(hold.place_time IS NULL OR record.%[2]s < hold.place_time)
				)
			ORDER BY hold.case_id
			LIMIT 1
			`,
			pgx.Identifier{leaf}.Sanitize(),
			pgx.Identifier{timeColumn}.Sanitize(),
		),
		r.Lower.UTC(),
		now.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&caseId))
			found = true
		}
	})
	if !found {
		return ""
	}
	// the case id is an opaque id, which a log line may carry (I-13)
	return fmt.Sprintf("hold of case %s has not copied its network's rows of %s", caseId, leaf)
}

// Runs insert in one transaction, without retries, and returns what it raised.
// When the insert found no partition for a row, it creates each of hours that
// has none (the registry's EnsureRangeFor) and runs insert once more, so a
// batch never waits for the runway. insert is the whole batch, so a retry
// stores nothing twice that its statement does not already keep once.
func insertConnectionRecordsCreatingHours(ctx context.Context, family *server.PartitionFamily, table string, hours map[time.Time]bool, insert func(tx server.PgTx)) error {
	run := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if server.IsDoneError(r) {
					panic(r)
				}
				if recoveredErr, ok := r.(error); ok {
					err = recoveredErr
				} else {
					err = fmt.Errorf("%v", r)
				}
			}
		}()
		server.Tx(ctx, insert, server.OptNoRetry())
		return nil
	}
	err := run()
	if !connectionRecordPartitionMissing(err, table) {
		return err
	}
	for hour := range hours {
		if _, createErr := family.EnsureRangeFor(ctx, hour); createErr != nil {
			return createErr
		}
	}
	return run()
}

// Whether err is Postgres finding no partition of table for a row, which is a
// check violation on the parent with no constraint named.
func connectionRecordPartitionMissing(err error, table string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.CheckViolation &&
		pgErr.TableName == table &&
		pgErr.ConstraintName == ""
}
