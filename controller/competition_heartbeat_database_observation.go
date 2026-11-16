// Heartbeat database diagnostics retain fixed source boundaries, never SQL or
// arguments. They observe a completed invocation without granting retry authority.
package controller

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Statement ordinals are local to one step. SupervisorAuthority consists of
// the lease EXISTS scan, instrument-approval setup and supervisor-fence setup,
// in that order. JobAuthority also has two prospective executor-fence statements
// for a distributed host; every other SQL-bearing step has one statement.
type competitionHeartbeatDatabaseStep uint8

const (
	competitionHeartbeatDatabaseUnknown competitionHeartbeatDatabaseStep = iota
	competitionHeartbeatDatabaseScopeLock
	competitionHeartbeatDatabaseSlotLock
	competitionHeartbeatDatabaseJobLock
	competitionHeartbeatDatabaseJobAuthority
	competitionHeartbeatDatabaseSupervisorAuthority
	competitionHeartbeatDatabaseWorkerMode
	competitionHeartbeatDatabaseClockCheck
	competitionHeartbeatDatabaseSlotUpdate
	competitionHeartbeatDatabaseJobUpdate
	competitionHeartbeatDatabaseCallbackReturn
	competitionHeartbeatDatabaseStepCount
)

// Only the latest occurrence of each fixed statement is retained; counts keep
// earlier callback attempts visible. A row statement returns only after Scan,
// not when pgx hands back its lazy Row value. No error object is retained here.
type competitionHeartbeatStatementObservation struct {
	Step      competitionHeartbeatDatabaseStep `json:"step"`
	Ordinal   uint8                            `json:"ordinal"`
	Kind      string                           `json:"kind"`
	Count     uint64                           `json:"count"`
	Entered   server.DbContextBudget           `json:"entered"`
	Returned  bool                             `json:"returned"`
	Succeeded bool                             `json:"succeeded"`
	AtReturn  *server.DbContextBudget          `json:"at_return,omitempty"`
}

// Fixed-size invocation evidence is copied only after the database call and
// all of its cleanup unwind. Step is the last business-callback step, possibly
// from an earlier inner attempt if a later acquire/BEGIN failed. The separate
// database phase and cumulative counts retain that distinction. Statement
// entry/return is a client call boundary, not proof of server receipt or timing.
type competitionHeartbeatDatabaseSnapshot struct {
	AtEntry           server.DbContextBudget                     `json:"at_entry"`
	AtReturn          server.DbContextBudget                     `json:"at_return"`
	Step              competitionHeartbeatDatabaseStep           `json:"step"`
	CallbackEntries   uint64                                     `json:"callback_entries"`
	UnknownStatements uint64                                     `json:"unknown_statements"`
	Acquire           server.DbAcquireObservation                `json:"acquire"`
	Transaction       server.DbTxObservationSnapshot             `json:"transaction"`
	Statements        []competitionHeartbeatStatementObservation `json:"statements"`
	SqlShape          string                                     `json:"sql_shape,omitempty"`
	SqlStatements     uint64                                     `json:"sql_statements,omitempty"`
	SqlShapeValid     bool                                       `json:"sql_shape_valid,omitempty"`
}

// One sequential owner supplies this observation to the original transaction.
// It cannot interrupt, replace, replay or acknowledge any database operation.
type competitionHeartbeatDatabaseObservation struct {
	phase       server.DbPhaseObservation
	timing      server.DbTiming
	acquire     server.DbAcquireObservation
	transaction server.DbTxObservation
	step        competitionHeartbeatDatabaseStep
	ordinal     uint8
	callbacks   uint64
	unknown     uint64
	statements  [competitionHeartbeatDatabaseStepCount][3]competitionHeartbeatStatementObservation
	sqlShape    string
	sqlCount    uint64
	sqlValid    bool
}

// The original error bytes and Is/As graph remain unchanged by diagnostics.
type competitionHeartbeatDatabaseError struct {
	cause    error
	snapshot competitionHeartbeatDatabaseSnapshot
}

func (self competitionHeartbeatDatabaseError) Error() string { return self.cause.Error() }
func (self competitionHeartbeatDatabaseError) Unwrap() error { return self.cause }

// A fresh observation covers every inner transaction retry in this invocation.
func captureCompetitionHeartbeatDatabase(ctx context.Context, run func(*competitionHeartbeatDatabaseObservation)) error {
	observation := &competitionHeartbeatDatabaseObservation{}
	entered := server.ObserveDbContextBudget(ctx)
	err := captureDatabaseError(func() { run(observation) })
	completed := competitionHeartbeatCompletedDatabaseFromContext(ctx)
	if err == nil && completed == nil {
		return nil
	}
	snapshot := competitionHeartbeatDatabaseSnapshot{AtEntry: entered, AtReturn: server.ObserveDbContextBudget(ctx),
		Step: observation.step, CallbackEntries: observation.callbacks, UnknownStatements: observation.unknown,
		Acquire: observation.acquire, Transaction: observation.transaction.Snapshot(),
		SqlShape: observation.sqlShape, SqlStatements: observation.sqlCount, SqlShapeValid: observation.sqlValid}
	for _, statements := range observation.statements {
		for _, statement := range statements {
			if statement.Count != 0 {
				snapshot.Statements = append(snapshot.Statements, statement)
			}
		}
	}
	completed.capture(snapshot)
	if err == nil {
		return nil
	}
	return competitionHeartbeatDatabaseError{cause: competitionDatabasePhaseError{cause: err, phase: observation.phase.Phase()}, snapshot: snapshot}
}

// Each callback rerun starts a new local sequence, without clearing history.
func (self *competitionHeartbeatDatabaseObservation) beginCallback(tx server.PgTx) *competitionHeartbeatObservedTx {
	self.callbacks++
	self.step, self.ordinal = competitionHeartbeatDatabaseUnknown, 0
	return &competitionHeartbeatObservedTx{tx: tx, observation: self}
}

// A semantic boundary is explicitly chosen by trusted source, not inferred
// from query text. Steps without SQL still identify local authority refusals.
func (self *competitionHeartbeatDatabaseObservation) enterStep(step competitionHeartbeatDatabaseStep) {
	self.step, self.ordinal = step, 0
}

func (self *competitionHeartbeatDatabaseObservation) beginStatement(ctx context.Context, kind string) *competitionHeartbeatStatementObservation {
	if self.ordinal < 255 {
		self.ordinal++
	}
	if self.step <= competitionHeartbeatDatabaseUnknown || self.step >= competitionHeartbeatDatabaseStepCount || self.ordinal > 3 {
		self.unknown++
		return nil
	}
	statement := &self.statements[self.step][self.ordinal-1]
	*statement = competitionHeartbeatStatementObservation{Step: self.step, Ordinal: self.ordinal, Kind: kind,
		Count: statement.Count + 1, Entered: server.ObserveDbContextBudget(ctx)}
	return statement
}

func finishCompetitionHeartbeatStatement(ctx context.Context, statement *competitionHeartbeatStatementObservation, err error) {
	if statement != nil {
		budget := server.ObserveDbContextBudget(ctx)
		statement.Returned, statement.Succeeded, statement.AtReturn = true, err == nil, &budget
	}
}

// No embedding can silently bypass the closed proof when pgx adds a method.
// Every other current method explicitly invalidates proof before delegating.
type competitionHeartbeatObservedTx struct {
	tx          server.PgTx
	observation *competitionHeartbeatDatabaseObservation
}

var _ server.PgTx = (*competitionHeartbeatObservedTx)(nil)

func (self *competitionHeartbeatObservedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	statement := self.observation.beginStatement(ctx, "exec")
	self.observation.observeSql(statement, sql)
	result, err := self.tx.Exec(ctx, sql, args...)
	finishCompetitionHeartbeatStatement(ctx, statement, err)
	return result, err
}

func (self *competitionHeartbeatObservedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	statement := self.observation.beginStatement(ctx, "row_scan")
	self.observation.observeSql(statement, sql)
	return competitionHeartbeatObservedRow{Row: self.tx.QueryRow(ctx, sql, args...), ctx: ctx, statement: statement, observation: self.observation}
}

// Row.Scan is the actual terminal reply/error boundary for pgx.QueryRow.
type competitionHeartbeatObservedRow struct {
	pgx.Row
	ctx         context.Context
	statement   *competitionHeartbeatStatementObservation
	observation *competitionHeartbeatDatabaseObservation
}

func (self competitionHeartbeatObservedRow) Scan(dest ...any) error {
	if self.statement != nil && self.statement.Returned {
		self.observation.unknown++
	}
	err := self.Row.Scan(dest...)
	finishCompetitionHeartbeatStatement(self.ctx, self.statement, err)
	return err
}
