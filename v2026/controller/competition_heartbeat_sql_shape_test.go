// A complete private pgx surface makes every bypass observable without a
// database. Production deliberately embeds no underlying transaction method.
package controller

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// All methods delegate once so instrumentation cannot silently refuse/change
// existing pgx contracts while invalidating continuation evidence.
type competitionHeartbeatSurfaceTx struct{ calls int }

func (self *competitionHeartbeatSurfaceTx) Begin(context.Context) (pgx.Tx, error) {
	self.calls++
	return nil, nil
}
func (self *competitionHeartbeatSurfaceTx) Commit(context.Context) error {
	self.calls++
	return nil
}
func (self *competitionHeartbeatSurfaceTx) Rollback(context.Context) error {
	self.calls++
	return nil
}
func (self *competitionHeartbeatSurfaceTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	self.calls++
	return nil, nil
}
func (self *competitionHeartbeatSurfaceTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	self.calls++
	return nil
}
func (self *competitionHeartbeatSurfaceTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	self.calls++
	return 0, nil
}
func (self *competitionHeartbeatSurfaceTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	self.calls++
	return nil, nil
}
func (self *competitionHeartbeatSurfaceTx) Conn() *pgx.Conn {
	self.calls++
	return nil
}
func (self *competitionHeartbeatSurfaceTx) LargeObjects() pgx.LargeObjects {
	self.calls++
	return pgx.LargeObjects{}
}
func (self *competitionHeartbeatSurfaceTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	self.calls++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (self *competitionHeartbeatSurfaceTx) QueryRow(context.Context, string, ...any) pgx.Row {
	self.calls++
	return competitionHeartbeatDiagnosticRow(func(...any) error { return nil })
}

// All unreviewed methods invalidate proof BEFORE returning raw/nested access.
func TestCompetitionHeartbeatClosedTxInterceptsEveryOtherPgxMethod(t *testing.T) {
	for _, invoke := range []func(*competitionHeartbeatObservedTx){
		func(tx *competitionHeartbeatObservedTx) { _, _ = tx.Begin(t.Context()) },
		func(tx *competitionHeartbeatObservedTx) { _ = tx.Commit(t.Context()) },
		func(tx *competitionHeartbeatObservedTx) { _ = tx.Rollback(t.Context()) },
		func(tx *competitionHeartbeatObservedTx) { _, _ = tx.Query(t.Context(), "synthetic query") },
		func(tx *competitionHeartbeatObservedTx) { _ = tx.SendBatch(t.Context(), &pgx.Batch{}) },
		func(tx *competitionHeartbeatObservedTx) { _, _ = tx.CopyFrom(t.Context(), nil, nil, nil) },
		func(tx *competitionHeartbeatObservedTx) {
			_, _ = tx.Prepare(t.Context(), "synthetic", "synthetic query")
		},
		func(tx *competitionHeartbeatObservedTx) { _ = tx.Conn() },
		func(tx *competitionHeartbeatObservedTx) { _ = tx.LargeObjects() },
	} {
		original := &competitionHeartbeatSurfaceTx{}
		observation := &competitionHeartbeatDatabaseObservation{}
		wrapped := observation.beginCallback(original)
		invoke(wrapped)
		if original.calls != 1 || observation.unknown != 1 {
			t.Fatal("another pgx method escaped the closed proof or changed delegation", original.calls, observation.unknown)
		}
	}
}

// The exact current-main prefix passes; transaction-control SQL, a reordered
// known query or a trailing command cannot masquerade under semantic labels.
func TestCompetitionHeartbeatSqlShapeRejectsChangedReorderedAndManualCommitSql(t *testing.T) {
	for _, mode := range []string{"exact", "manual commit", "trailing commit", "reordered", "another table"} {
		original := &competitionHeartbeatSurfaceTx{}
		observation := &competitionHeartbeatDatabaseObservation{sqlShape: competitionHeartbeatLegacySlotShape, sqlValid: true}
		wrapped := observation.beginCallback(original)
		previous := competitionHeartbeatDatabaseUnknown
		for index, statement := range competitionHeartbeatLegacySlotStatements() {
			if statement.step != previous {
				observation.enterStep(statement.step)
				previous = statement.step
			}
			sql := statement.sql
			if index == 8 {
				switch mode {
				case "manual commit":
					sql = "COMMIT"
				case "trailing commit":
					sql += "; COMMIT"
				case "reordered":
					sql = competitionHeartbeatLegacySlotStatements()[0].sql
				case "another table":
					sql = "UPDATE synthetic_other_table SET value=1"
				}
			}
			if statement.kind == "exec" {
				_, _ = wrapped.Exec(t.Context(), sql)
			} else {
				_ = wrapped.QueryRow(t.Context(), sql).Scan()
			}
		}
		if observation.sqlValid != (mode == "exact") || observation.sqlCount != 9 || original.calls != 9 {
			t.Fatal("closed SQL shape changed delegation or accepted another command", mode, observation.sqlValid, observation.sqlCount, original.calls)
		}
	}
}

// Repeated Scan on one lazy row cannot overwrite a prior terminal boundary
// while leaving the continuation's statement count at one.
func TestCompetitionHeartbeatSqlShapeRejectsRepeatedLazyRowScan(t *testing.T) {
	observation := &competitionHeartbeatDatabaseObservation{}
	wrapped := observation.beginCallback(&competitionHeartbeatSurfaceTx{})
	observation.enterStep(competitionHeartbeatDatabaseSlotLock)
	row := wrapped.QueryRow(t.Context(), "synthetic row")
	_ = row.Scan()
	_ = row.Scan()
	if observation.unknown != 1 {
		t.Fatal("repeated lazy-row completion erased earlier history")
	}
}
