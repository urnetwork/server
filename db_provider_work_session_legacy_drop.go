// The early drop of the session journal's legacy partition (owner, 2026-11-15;
// DBDISK-STABILIZE I4): the cutover command's drop legacy step
// (ProviderWorkSessionPartitionMigrateOptions.DropLegacy).
//
// provider_work_session_event_legacy holds every event before the cutover
// week. The family retires it only at the contract horizon after that week,
// and only once no open connection's admission is in it
// (providerWorkSessionEventDropBlocker). Its only reader is the append's retire
// lookup, which reads the parent, and no event was ever signed. So the owner
// accepted that a connection admitted before the cutover and still open at the
// drop retires without its network id: the lookup finds no admit, and the
// retire is appended with a NULL network_id, which the journal allows.
//
// The step runs in two transactions, so the parent's ACCESS EXCLUSIVE lock is
// held for catalog work only:
//   - detach: the retention opt-in, the drop guard, which admits the legacy
//     partition once its upper bound is ProviderWorkSessionEventLegacyDropMargin
//     past by the database's clock, then DETACH PARTITION. Every connection
//     open and close waits behind the parent's lock, so the step first waits
//     until no transaction older than MaxTransactionAge holds a lock on the
//     parent or the partition, and its lock wait is LegacyDetachLockTimeout,
//     below a main-pool writer's own lock bound. It retries while busy.
//   - drop: DROP TABLE of the detached table, which nothing reads any more. Its
//     commit unlinks the files, seconds for a large partition, holding only the
//     detached table's lock.
//
// A run interrupted after the detach finds the detached table, which still
// carries the cutover's legacy bound check, and drops it. A dry run reports the
// guard's answer and changes nothing.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The age past its upper bound from which the drop guard admits the legacy
// partition (providerWorkSessionEventLegacyDropClauseSql). Every event takes the
// database's clock (provider_work_session_append), so none lands in the
// partition once its upper bound is past; the margin absorbs a clock step.
const ProviderWorkSessionEventLegacyDropMargin = time.Hour

// The legacy partition as the catalog has it, read in one session.
type providerWorkSessionLegacyState struct {
	// the database's clock
	now time.Time
	// a relation exists under the legacy name
	exists bool
	// it is the parent's legacy partition, with this upper bound
	attached bool
	upper    time.Time
	// a plain table that is no partition and still carries the cutover's
	// legacy bound check: detached by a run that did not drop it
	detached  bool
	byteCount int64
}

func readProviderWorkSessionLegacyState(ctx context.Context) (state providerWorkSessionLegacyState, err error) {
	legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var relkind *string
			var partition *bool
			var bound bool
			var byteCount *int64
			Raise(conn.QueryRow(
				ctx,
				`
				SELECT
					now() AT TIME ZONE 'UTC',
					(SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)),
					(SELECT relispartition FROM pg_class WHERE oid = to_regclass($1)),
					EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass($1) AND conname = $2 AND contype = 'c'),
					pg_total_relation_size(to_regclass($1))
				`,
				"public."+legacy,
				providerWorkSessionLegacyBound(),
			).Scan(&state.now, &relkind, &partition, &bound, &byteCount))
			if relkind == nil {
				return
			}
			state.exists = true
			if byteCount != nil {
				state.byteCount = *byteCount
			}
			leaves, listErr := providerWorkSessionEventPartitionFamily.ListLeaves(ctx, conn, providerWorkSessionEventTable)
			Raise(listErr)
			for _, leaf := range leaves {
				if leaf.Name == legacy && leaf.Range.Legacy && leaf.Canonical {
					state.attached = true
					state.upper = leaf.Range.Upper
				}
			}
			state.detached = *relkind == "r" && partition != nil && !*partition && bound
		})
	})
	return
}

// The statements of the detach transaction: its bounds, the opt-in, the guard
// and the detach. The guard reads the catalog before the detach locks anything.
func (self *providerWorkSessionCutover) legacyDetachSql() []string {
	table := providerWorkSessionEventTable
	legacy := PartitionLegacyLeafName(table)
	return []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LegacyDetachLockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL %s = 'on'", providerWorkSessionRetentionSetting),
		fmt.Sprintf("SELECT provider_work_session_event_drop_guard('%s')", legacy),
		fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(table), quoteIdentifier(legacy)),
	}
}

// Whether the drop guard admits the legacy partition's drop now, asked in a
// transaction that is rolled back: nil when it does, else its refusal.
func providerWorkSessionLegacyGuardAnswer(ctx context.Context) error {
	legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
	errAdmitted := errors.New("admitted")
	err := contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL %s = 'on'", providerWorkSessionRetentionSetting)))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SELECT provider_work_session_event_drop_guard('%s')", legacy)))
		panic(errAdmitted)
	})
	if errors.Is(err, errAdmitted) {
		return nil
	}
	return err
}

// Drops the detached legacy table under its own lock, after checking, under
// that lock, that it is still no partition and still carries the cutover's
// legacy bound check.
func (self *providerWorkSessionCutover) dropDetachedLegacy(ctx context.Context) error {
	legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LegacyDropLockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(legacy))))
		var partition bool
		var bound bool
		Raise(tx.QueryRow(
			ctx,
			`
			SELECT relispartition,
				EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = pg_class.oid AND conname = $2 AND contype = 'c')
			FROM pg_class WHERE oid = to_regclass($1) AND relkind = 'r'
			`,
			"public."+legacy,
			providerWorkSessionLegacyBound(),
		).Scan(&partition, &bound))
		if partition || !bound {
			panic(fmt.Errorf("%s is not the detached legacy partition (partition %t, legacy bound %t)", legacy, partition, bound))
		}
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(legacy))))
	})
}

func providerWorkSessionGibString(byteCount int64) string {
	return fmt.Sprintf("%.1f GiB", float64(byteCount)/float64(1<<30))
}

func (self *providerWorkSessionCutover) dropLegacy(ctx context.Context) error {
	if !isProviderWorkSessionEventPartitioned(ctx) {
		return errors.New("provider_work_session_event is not partitioned")
	}
	table := providerWorkSessionEventTable
	legacy := PartitionLegacyLeafName(table)
	state, err := readProviderWorkSessionLegacyState(ctx)
	if err != nil {
		return err
	}
	switch {
	case !state.exists:
		self.logf("%s is already dropped", legacy)
		return nil
	case state.attached:
		self.logf(
			"%s holds every event before %s (%s); the database's clock reads %s",
			legacy,
			state.upper.Format(time.RFC3339),
			providerWorkSessionGibString(state.byteCount),
			state.now.Format(time.RFC3339),
		)
		if err := providerWorkSessionLegacyGuardAnswer(ctx); err != nil {
			return fmt.Errorf("the drop guard refuses %s, which it admits from %s: %w", legacy, state.upper.Add(ProviderWorkSessionEventLegacyDropMargin).Format(time.RFC3339), err)
		}
		self.logf("the drop guard admits %s", legacy)
		if self.dryRun {
			self.logf("would detach, waiting at most %s for the locks, in one transaction:\n%s", self.settings.LegacyDetachLockTimeout, strings.Join(self.legacyDetachSql(), ";\n"))
		} else {
			detachStart := time.Now()
			relations := []string{"public." + table, "public." + legacy}
			if err := self.retryExclusiveOn(ctx, relations, func() error {
				return contractPartitionExec(ctx, self.legacyDetachSql())
			}); err != nil {
				return fmt.Errorf("detach %s: %w", legacy, err)
			}
			self.logf("detached %s in %s", legacy, time.Since(detachStart).Round(time.Millisecond))
		}
	case state.detached:
		self.logf("%s was detached by an earlier run (%s)", legacy, providerWorkSessionGibString(state.byteCount))
	default:
		return fmt.Errorf("%s is neither the journal's legacy partition nor its detached table; it is left alone", legacy)
	}

	if self.dryRun {
		self.logf("would then drop the detached table in a second transaction, under its own lock only:\nDROP TABLE public.%s", quoteIdentifier(legacy))
		self.logf("dry run: nothing changed")
		return nil
	}
	dropStart := time.Now()
	if err := self.retryExclusiveOn(ctx, nil, func() error {
		return self.dropDetachedLegacy(ctx)
	}); err != nil {
		return fmt.Errorf("drop the detached %s: %w", legacy, err)
	}
	self.logf("dropped %s (%s) in %s", legacy, providerWorkSessionGibString(state.byteCount), time.Since(dropStart).Round(time.Millisecond))
	return nil
}
