// Synthetic transaction boundaries prove diagnostics do not replace the
// original statement contexts, replies, causes or retry policy.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Unused transaction methods deliberately retain the embedded nil interface;
// a newly introduced call cannot silently succeed through this fixture.
type competitionHeartbeatDiagnosticTx struct {
	server.PgTx
	exec     func(context.Context, string, ...any) (pgconn.CommandTag, error)
	queryRow func(context.Context, string, ...any) pgx.Row
}

func (self competitionHeartbeatDiagnosticTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return self.exec(ctx, sql, args...)
}

func (self competitionHeartbeatDiagnosticTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return self.queryRow(ctx, sql, args...)
}

type competitionHeartbeatDiagnosticRow func(...any) error

func (self competitionHeartbeatDiagnosticRow) Scan(dest ...any) error { return self(dest...) }

// Query text and arguments reach only the original driver, while the same
// typed PostgreSQL error and its exact digest pass through the new wrapper.
func TestCompetitionHeartbeatDatabaseExecPreservesCallAndCause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const secretSql = "synthetic-private-sql"
	argument := &struct{ value string }{"synthetic-private-argument"}
	cause := &pgconn.PgError{Code: "55P03", Message: "synthetic-private-postgres-detail"}
	result := pgconn.NewCommandTag("UPDATE 1")
	err := captureCompetitionHeartbeatDatabase(ctx, func(observation *competitionHeartbeatDatabaseObservation) {
		tx := observation.beginCallback(competitionHeartbeatDiagnosticTx{exec: func(actual context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			if actual != ctx || sql != secretSql || len(args) != 1 || args[0] != argument {
				t.Fatal("diagnostic decorator changed the original call")
			}
			return result, cause
		}})
		observation.enterStep(competitionHeartbeatDatabaseSlotUpdate)
		actual, err := tx.Exec(ctx, secretSql, argument)
		if actual != result || err != cause {
			t.Fatal("diagnostic decorator replaced the driver reply")
		}
		panic(err)
	})
	var actual *pgconn.PgError
	if !errors.As(err, &actual) || actual != cause || err.Error() != cause.Error() {
		t.Fatal("diagnostic wrapper changed PostgreSQL identity or bytes")
	}
	detail := competitionHeartbeatFailureDetails(err)
	digest := sha256.Sum256([]byte(cause.Error()))
	if detail.SqlState != "55P03" || detail.DetailSha256 != hex.EncodeToString(digest[:]) || detail.HeartbeatDatabase == nil {
		t.Fatal("finite diagnostic or original digest missing")
	}
	snapshot := detail.HeartbeatDatabase
	if snapshot.Step != competitionHeartbeatDatabaseSlotUpdate || snapshot.CallbackEntries != 1 || len(snapshot.Statements) != 1 {
		t.Fatalf("statement boundary lost: %+v", snapshot)
	}
	statement := snapshot.Statements[0]
	if statement.Step != competitionHeartbeatDatabaseSlotUpdate || statement.Ordinal != 1 || statement.Kind != "exec" || statement.Count != 1 ||
		!statement.Returned || statement.Succeeded || statement.AtReturn == nil || statement.Entered.State != "live" || !statement.Entered.HasDeadline {
		t.Fatalf("statement terminal state lost: %+v", statement)
	}
	encoded, marshalErr := json.Marshal(competitionHandBackEvent{Reason: "heartbeat_failed", competitionHeartbeatFailure: detail})
	if marshalErr != nil || len(encoded) > 16384 {
		t.Fatal("finite diagnostic was not bounded JSON")
	}
	for _, secret := range []string{secretSql, argument.value, cause.Message} {
		if strings.Contains(string(encoded), secret) || strings.Contains(safeCompetitionHeartbeatError(err).Error(), secret) {
			t.Fatal("private driver detail escaped through diagnostics")
		}
	}
}

// QueryRow is lazy: only the exact Scan reply closes its observed statement.
func TestCompetitionHeartbeatDatabaseRowWaitsForScan(t *testing.T) {
	ctx := context.Background()
	observation := &competitionHeartbeatDatabaseObservation{}
	calls := 0
	destination := 7
	cause := errors.New("synthetic-private-row-refusal")
	tx := observation.beginCallback(competitionHeartbeatDiagnosticTx{queryRow: func(actual context.Context, sql string, args ...any) pgx.Row {
		if actual != ctx || sql != "synthetic-private-row" || len(args) != 1 || args[0] != 12 {
			t.Fatal("row observation changed query arguments")
		}
		return competitionHeartbeatDiagnosticRow(func(dest ...any) error {
			calls++
			if len(dest) != 1 || dest[0] != &destination {
				t.Fatal("row observation replaced Scan destinations")
			}
			return cause
		})
	}})
	observation.enterStep(competitionHeartbeatDatabaseSlotLock)
	row := tx.QueryRow(ctx, "synthetic-private-row", 12)
	statement := &observation.statements[competitionHeartbeatDatabaseSlotLock][0]
	if calls != 0 || statement.Returned || statement.AtReturn != nil || statement.Kind != "row_scan" {
		t.Fatal("lazy row was acknowledged before Scan")
	}
	if err := row.Scan(&destination); err != cause || calls != 1 || !statement.Returned || statement.Succeeded || statement.AtReturn == nil {
		t.Fatal("actual row refusal was not observed transparently")
	}
}

// An owned virtual deadline crossing a statement remains visible at return;
// no timer, timeout or cancellation policy is supplied by the observation.
func TestCompetitionHeartbeatDatabaseObservesOwnedDeadlineAtReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := captureCompetitionHeartbeatDatabase(ctx, func(observation *competitionHeartbeatDatabaseObservation) {
			tx := observation.beginCallback(competitionHeartbeatDiagnosticTx{exec: func(actual context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
				if actual != ctx {
					t.Fatal("statement no longer uses its owned context")
				}
				<-actual.Done()
				return pgconn.CommandTag{}, actual.Err()
			}})
			observation.enterStep(competitionHeartbeatDatabaseJobUpdate)
			_, err := tx.Exec(ctx, "synthetic-update")
			panic(err)
		})
		detail := competitionHeartbeatFailureDetails(err)
		if !errors.Is(err, context.DeadlineExceeded) || detail.HeartbeatDatabase == nil {
			t.Fatal("owned deadline was altered or lost")
		}
		statement := detail.HeartbeatDatabase.Statements[0]
		if statement.Entered.State != "live" || statement.Entered.RemainingNs != int64(5*time.Second) || statement.AtReturn == nil ||
			statement.AtReturn.State != "deadline" || statement.AtReturn.RemainingNs != 0 || !statement.Returned || statement.Succeeded || detail.HeartbeatDatabase.AtReturn.State != "deadline" {
			t.Fatalf("entry/return budget boundary lost: %+v", statement)
		}
	})
}

// Repeated callbacks retain finite counts and the final statement, not an
// unbounded trace. A copied failure snapshot cannot be revised by later writes.
func TestCompetitionHeartbeatDatabaseKeepsBoundedRerunHistory(t *testing.T) {
	ctx := context.Background()
	var retained *competitionHeartbeatDatabaseObservation
	err := captureCompetitionHeartbeatDatabase(ctx, func(observation *competitionHeartbeatDatabaseObservation) {
		retained = observation
		for range 2 {
			tx := observation.beginCallback(competitionHeartbeatDiagnosticTx{exec: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
				return pgconn.NewCommandTag("SELECT 1"), nil
			}})
			observation.enterStep(competitionHeartbeatDatabaseSupervisorAuthority)
			for range 5 {
				_, _ = tx.Exec(ctx, "synthetic-private-sql")
			}
		}
		panic(ErrLeaseLost)
	})
	snapshot := competitionHeartbeatFailureDetails(err).HeartbeatDatabase
	if snapshot == nil || snapshot.CallbackEntries != 2 || snapshot.UnknownStatements != 4 || len(snapshot.Statements) != 3 {
		t.Fatalf("diagnostic history was reset or grew without a bound: %+v", snapshot)
	}
	for _, statement := range snapshot.Statements {
		if statement.Count != 2 || !statement.Returned || !statement.Succeeded {
			t.Fatal("earlier statement invocation was erased")
		}
	}
	retained.enterStep(competitionHeartbeatDatabaseSupervisorAuthority)
	retained.beginStatement(ctx, "exec")
	if snapshot.Statements[0].Count != 2 || !snapshot.Statements[0].Returned || snapshot.Statements[0].AtReturn == nil {
		t.Fatal("completed diagnostic snapshot retained mutable statement state")
	}
}

// Driver panics still unwind normally and never become a fabricated returned
// statement. Successful calls likewise remain nil, including a stopped owner.
func TestCompetitionHeartbeatDatabaseCaptureDoesNotFabricateReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := captureCompetitionHeartbeatDatabase(ctx, func(*competitionHeartbeatDatabaseObservation) {}); err != nil {
		t.Fatal("observing a successful call manufactured cancellation")
	}
	cause := errors.New("synthetic-private-driver-panic")
	err := captureCompetitionHeartbeatDatabase(ctx, func(observation *competitionHeartbeatDatabaseObservation) {
		tx := observation.beginCallback(competitionHeartbeatDiagnosticTx{exec: func(context.Context, string, ...any) (pgconn.CommandTag, error) { panic(cause) }})
		observation.enterStep(competitionHeartbeatDatabaseSlotUpdate)
		_, _ = tx.Exec(ctx, "synthetic-private-sql")
	})
	snapshot := competitionHeartbeatFailureDetails(err).HeartbeatDatabase
	if !errors.Is(err, cause) || snapshot == nil || len(snapshot.Statements) != 1 || snapshot.Statements[0].Returned || snapshot.Statements[0].AtReturn != nil {
		t.Fatal("panic identity or unreturned statement boundary lost")
	}
}

// New observational wrappers preserve the exact pre-existing complete-55P03
// retry rule; deadline and mixed causes remain terminal after one call.
func TestCompetitionHeartbeatDatabasePreservesLeaseRetryRule(t *testing.T) {
	for _, cause := range []error{&pgconn.PgError{Code: "55P03"}, context.DeadlineExceeded, errors.Join(&pgconn.PgError{Code: "55P03"}, context.Canceled)} {
		now := time.Now()
		attempts, waits := 0, 0
		settings := competitionLeaseRetrySettings{Timeout: time.Minute, Interval: time.Second, MaximumAttempts: 2, Now: func() time.Time { return now },
			Wait: func(context.Context, time.Duration) error { waits++; return nil }}
		_, err := retryCompetitionLeaseOperation(context.Background(), now.Add(time.Hour), settings, func(ctx context.Context) error {
			attempts++
			if attempts == 2 {
				return nil
			}
			return captureCompetitionHeartbeatDatabase(ctx, func(*competitionHeartbeatDatabaseObservation) { panic(cause) })
		})
		if cause == context.DeadlineExceeded || errors.Is(cause, context.Canceled) {
			if attempts != 1 || waits != 0 || !errors.Is(err, cause) {
				t.Fatal("diagnostics broadened a terminal cause into retry authority")
			}
		} else if attempts != 2 || waits != 1 || err != nil {
			t.Fatal("diagnostics suppressed the unchanged completed-lock retry")
		}
	}
}
