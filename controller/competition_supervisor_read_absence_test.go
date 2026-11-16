// Supervisor reads exercise real disposable PostgreSQL transactions, retries,
// and the composed snapshot. Subprocesses observe only their own log window;
// no global logger hook or production error policy is changed for the tests.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Expected absence preserves exact sentinels and commits all five read-only
// transaction paths, including an absent round and the source-target lookup.
func TestCompetitionSupervisorReadAbsenceCommitsWithoutPanic(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		foreign := *fixture.settings
		foreign.AutomationPolicy.SeasonId = "synthetic-unpublished-season"
		trace := &competitionSealPollTracer{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		missingRoundId := server.NewId()
		for range 3 {
			for _, roundId := range []server.Id{fixture.round.RoundId, missingRoundId} {
				if result, err := fixture.store.AutomationSeal(ctx, fixture.settings, roundId); result != nil || err != ErrNotFound {
					t.Fatalf("absent seal changed its sentinel: %+v %v", result, err)
				}
			}
			if result, err := fixture.store.SourceActivation(ctx, fixture.settings, 99); result != nil || err != ErrNotFound {
				t.Fatalf("absent source generation changed its sentinel: %+v %v", result, err)
			}
			if result, err := fixture.store.CurrentSourceActivation(ctx, &foreign); result != nil || err != ErrNotFound {
				t.Fatalf("absent source pointer changed its sentinel: %+v %v", result, err)
			}
			if result, err := fixture.store.SourceActivationForTarget(ctx, fixture.settings, 2); result != nil || err != ErrNotFound {
				t.Fatalf("absent target source changed its sentinel: %+v %v", result, err)
			}
			if result, err := fixture.store.SupervisorTransition(ctx, fixture.settings, 2); result != nil || err != ErrNotFound {
				t.Fatalf("absent transition changed its sentinel: %+v %v", result, err)
			}
			if result, err := fixture.store.CurrentAutomationRound(ctx, &foreign); result != nil || err != nil {
				t.Fatalf("empty current-round lookup changed its existing nil contract: %+v %v", result, err)
			}
		}
		trace.stateLock.Lock()
		commits, rollbacks := trace.commits, trace.rollbacks
		trace.stateLock.Unlock()
		if commits != 15 || rollbacks != 0 {
			t.Fatalf("ordinary absence unwound a read transaction: commits=%d rollbacks=%d", commits, rollbacks)
		}
	})
}

// Present immutable documents and the reconstructed snapshot retain the exact
// data selected by the database, not a synthesized default for missing state.
func TestCompetitionSupervisorReadPresentDocumentsAndSnapshot(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt
		published, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		seal, err := fixture.store.AutomationSeal(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || !reflect.DeepEqual(seal, &published.Seal) {
			t.Fatalf("stored seal changed: %+v %v", seal, err)
		}
		current, err := fixture.store.CurrentSourceActivation(ctx, fixture.settings)
		if err != nil || current == nil || current.Generation != 1 || current.TargetRoundId != fixture.round.RoundId || current.State != "active" {
			t.Fatalf("active source changed: %+v %v", current, err)
		}
		generation, err := fixture.store.SourceActivation(ctx, fixture.settings, 1)
		if err != nil || !reflect.DeepEqual(generation, current) {
			t.Fatalf("source-generation read changed: %+v %v", generation, err)
		}
		target, err := fixture.store.SourceActivationForTarget(ctx, fixture.settings, 1)
		if err != nil || !reflect.DeepEqual(target, current) {
			t.Fatalf("source-target read changed: %+v %v", target, err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-read-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		transition, err := fixture.store.RecordSupervisorTransition(ctx, fixture.settings, *lease, model.CompetitionSupervisorTransition{
			TargetEpoch: 2, State: "waiting", SourceActivationGeneration: 1, Reason: "synthetic durable checkpoint"})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := fixture.store.SupervisorTransition(ctx, fixture.settings, 2)
		if err != nil || !reflect.DeepEqual(stored, transition) {
			t.Fatalf("supervisor checkpoint changed: %+v %v", stored, err)
		}
		driver := &PostgresCompetitionSupervisorDriver{Store: fixture.store, Settings: fixture.settings}
		snapshot, err := driver.Snapshot(ctx, *lease)
		if err != nil || snapshot == nil || snapshot.Round == nil || snapshot.Round.RoundId != fixture.round.RoundId ||
			!reflect.DeepEqual(snapshot.Seal, seal) || !reflect.DeepEqual(snapshot.CurrentSource, current) || !reflect.DeepEqual(snapshot.Transition, transition) || !reflect.DeepEqual(snapshot.Intent, current) {
			t.Fatalf("composed snapshot lost durable read state: %+v %v", snapshot, err)
		}
	})
}

// An existing generation in the wrong frozen mode is a conflict, not missing
// state. Its transaction must retain the original error/rollback behavior.
func TestCompetitionSupervisorReadKeepsSourceModeConflict(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		conflicting := *fixture.settings
		conflicting.AutomationPolicy.Staging = true
		trace := &competitionSealPollTracer{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		for _, read := range []func() (*model.CompetitionSourceActivation, error){
			func() (*model.CompetitionSourceActivation, error) {
				return fixture.store.SourceActivation(ctx, &conflicting, 1)
			},
			func() (*model.CompetitionSourceActivation, error) {
				return fixture.store.CurrentSourceActivation(ctx, &conflicting)
			},
			func() (*model.CompetitionSourceActivation, error) {
				return fixture.store.SourceActivationForTarget(ctx, &conflicting, 1)
			},
		} {
			if result, err := read(); result != nil || err != ErrConflict {
				t.Fatalf("source conflict became ordinary absence: %+v %v", result, err)
			}
		}
		trace.stateLock.Lock()
		commits, rollbacks := trace.commits, trace.rollbacks
		trace.stateLock.Unlock()
		if commits != 0 || rollbacks != 3 {
			t.Fatalf("source conflict lost its failure path: commits=%d rollbacks=%d", commits, rollbacks)
		}
	})
}

// A one-shot deferred constraint fails the first actual COMMIT, not a mocked
// method return. The touched table and trigger exist only inside this TestEnv.
func prepareCompetitionSupervisorReadCommitFault(ctx context.Context, code string) {
	if code != "40001" && code != "23514" {
		panic("unsupported synthetic read-commit fault")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE synthetic_supervisor_read_commit (value integer NOT NULL)`))
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
		 CREATE SEQUENCE synthetic_supervisor_read_commit_sequence;
		 CREATE FUNCTION synthetic_supervisor_read_commit_fault() RETURNS trigger AS $fault$
		 BEGIN
		  IF nextval('synthetic_supervisor_read_commit_sequence')=1 THEN
		   RAISE EXCEPTION 'synthetic read commit fault' USING ERRCODE='%s';
		  END IF;
		  RETURN NULL;
		 END $fault$ LANGUAGE plpgsql;
		 CREATE CONSTRAINT TRIGGER synthetic_supervisor_read_commit_fault AFTER INSERT ON synthetic_supervisor_read_commit
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION synthetic_supervisor_read_commit_fault();`, code)))
	})
}

// The tracer reuses the already-owned pgx connection. It never acquires a
// nested connection or changes application SQL, and injection occurs once.
type competitionSupervisorReadCommitFault struct {
	once           sync.Once
	commitFailures atomic.Int64
}

// Arm the deferred fault immediately before the actual first commit request.
func (self *competitionSupervisorReadCommitFault) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		self.once.Do(func() {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO synthetic_supervisor_read_commit(value) VALUES(1)`))
		})
	}
	return ctx
}

// Actual server error responses prove that the test crossed the commit path.
func (self *competitionSupervisorReadCommitFault) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	var pgErr *pgconn.PgError
	if errors.As(data.Err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "23514") {
		self.commitFailures.Add(1)
	}
}

// A missing seal observed by a failed attempt cannot survive a retry that
// reads the now-published seal. The intervening publication has its own owner.
func TestCompetitionSupervisorReadRetryClearsEarlierAbsence(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		prepareCompetitionSupervisorReadCommitFault(ctx, "40001")
		trace := &competitionSupervisorReadCommitFault{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		reruns := 0
		var publication *model.CompetitionV2RoundResult
		var publicationErr error
		ctx = server.Testing_WithTxRerunHook(ctx, func() {
			reruns++
			fixture.now = fixture.round.RevealAt
			publication, publicationErr = fixture.store.SealAndElect(context.Background(), fixture.settings, fixture.round.RoundId)
		})
		seal, err := fixture.store.AutomationSeal(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || seal == nil || reruns != 1 || publicationErr != nil || publication == nil || !reflect.DeepEqual(seal, &publication.Seal) || trace.commitFailures.Load() != 1 {
			t.Fatalf("read retry retained stale absence: seal=%+v error=%v reruns=%d publication_error=%v commit_failures=%d", seal, err, reruns, publicationErr, trace.commitFailures.Load())
		}
	})
}

// A genuine failed commit takes precedence over the callback's missing-state
// sentinel; returning ErrNotFound here would hide the database failure.
func TestCompetitionSupervisorReadCommitFailurePrecedesAbsence(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		prepareCompetitionSupervisorReadCommitFault(ctx, "23514")
		trace := &competitionSupervisorReadCommitFault{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		seal, err := fixture.store.AutomationSeal(ctx, fixture.settings, fixture.round.RoundId)
		var pgErr *pgconn.PgError
		if seal != nil || errors.Is(err, ErrNotFound) || !errors.As(err, &pgErr) || pgErr.Code != "23514" || trace.commitFailures.Load() != 1 {
			t.Fatalf("missing state hid a failed commit: seal=%+v error=%v commit_failures=%d", seal, err, trace.commitFailures.Load())
		}
	})
}

// Only this test process changes its own cloned session's search path, and
// resets it after the actual SELECT fails. No schema or role is modified.
type competitionSupervisorReadQueryFault struct{ once sync.Once }
type competitionSupervisorReadQueryFaultKey struct{}

// Raw pgx control reuses the same connection without recursively tracing it.
func (self *competitionSupervisorReadQueryFault) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT transition_json FROM competition_supervisor_transition") {
		changed := false
		self.once.Do(func() {
			_, err := conn.PgConn().Exec(ctx, `SET search_path TO pg_catalog`).ReadAll()
			server.Raise(err)
			changed = true
		})
		if changed {
			return context.WithValue(ctx, competitionSupervisorReadQueryFaultKey{}, true)
		}
	}
	return ctx
}

// Query completion is physically synchronized before the session is restored.
func (self *competitionSupervisorReadQueryFault) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if changed, _ := ctx.Value(competitionSupervisorReadQueryFaultKey{}).(bool); changed {
		resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer resetCancel()
		_, err := conn.PgConn().Exec(resetCtx, `RESET search_path`).ReadAll()
		server.Raise(err)
	}
}

// The binary inherits only the independently configured test environment.
// Fixed markers bound observation to the read, not fixture startup/teardown.
func competitionSupervisorReadLogOutput(t *testing.T, mode string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCompetitionSupervisorReadLogChild$", "-test.count=1", "-test.v", "-logtostderr=true")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "URNETWORK_TEST_SUPERVISOR_READ_CHILD=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "URNETWORK_TEST_SUPERVISOR_READ_CHILD="+mode)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("supervisor read log child %s failed: %v", mode, err)
	}
	start := []byte("synthetic-supervisor-read-capture-start\n")
	end := []byte("synthetic-supervisor-read-capture-end\n")
	begin := bytes.Index(output, start)
	finish := bytes.Index(output, end)
	if begin < 0 || finish < begin+len(start) || bytes.Count(output, start) != 1 || bytes.Count(output, end) != 1 {
		t.Fatalf("supervisor read log child %s did not complete its exact capture window", mode)
	}
	return output[begin+len(start) : finish]
}

// Snapshot tolerates an absent seal/checkpoint and an entirely uninitialized
// season. The untransactional transition lookup must also remain quiet.
func TestCompetitionSupervisorSnapshotAbsenceDoesNotLogUnexpectedError(t *testing.T) {
	output := competitionSupervisorReadLogOutput(t, "missing")
	if bytes.Contains(output, []byte("Unexpected error:")) {
		t.Fatal("expected supervisor absence reached unexpected-error logging")
	}
}

// Real query and corrupt stored-data failures still reach the existing logger;
// quiet expected state is not a blanket catch or global sentinel suppression.
func TestCompetitionSupervisorReadGenuineFailuresStillLog(t *testing.T) {
	for _, mode := range []string{"query-error", "corrupt-data"} {
		output := competitionSupervisorReadLogOutput(t, mode)
		if !bytes.Contains(output, []byte("Unexpected error:")) {
			t.Fatalf("genuine %s was silently classified as ordinary absence", mode)
		}
	}
}

// A normal suite skips this closed helper. The parent invokes this exact test
// in a subprocess, so log observation cannot race another test's global sink.
func TestCompetitionSupervisorReadLogChild(t *testing.T) {
	mode := os.Getenv("URNETWORK_TEST_SUPERVISOR_READ_CHILD")
	if mode == "" {
		t.Skip("subprocess-only supervisor read observation")
	}
	if mode != "missing" && mode != "query-error" && mode != "corrupt-data" {
		t.Fatal("invalid supervisor read subprocess mode")
	}
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		if mode == "query-error" {
			scope, err := server.NewTestPgQueryScope(ctx, &competitionSupervisorReadQueryFault{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := scope.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		if mode == "corrupt-data" {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_supervisor_transition(environment,competition_id,season_id,target_epoch,revision,state,transition_json,updated_at)
				 VALUES($1,$2,$3,2,1,'waiting','"synthetic corrupt transition"'::jsonb,$4)`,
					fixture.settings.AutomationPolicy.Environment, fixture.settings.CompetitionId, fixture.settings.AutomationPolicy.SeasonId, fixture.now))
			})
		}
		glog.Flush()
		fmt.Fprintln(os.Stderr, "synthetic-supervisor-read-capture-start")
		switch mode {
		case "missing":
			for _, foreign := range []bool{false, true} {
				settings := *fixture.settings
				if foreign {
					settings.AutomationPolicy.SeasonId = "synthetic-unpublished-season"
				}
				driver := &PostgresCompetitionSupervisorDriver{Store: fixture.store, Settings: &settings}
				for range 3 {
					snapshot, err := driver.Snapshot(ctx, model.CompetitionSupervisorLease{})
					if err != nil || snapshot == nil || snapshot.Seal != nil || snapshot.Transition != nil ||
						!foreign && (snapshot.Round == nil || snapshot.CurrentSource == nil) || foreign && (snapshot.Round != nil || snapshot.CurrentSource != nil) {
						t.Fatalf("snapshot rejected ordinary not-created state: %+v %v", snapshot, err)
					}
				}
			}
		case "query-error":
			result, err := fixture.store.SupervisorTransition(ctx, fixture.settings, 2)
			var pgErr *pgconn.PgError
			if result != nil || !errors.As(err, &pgErr) || pgErr.Code != "42P01" || errors.Is(err, ErrNotFound) {
				t.Fatalf("genuine query error lost its identity: %+v %v", result, err)
			}
		case "corrupt-data":
			_, err := fixture.store.SupervisorTransition(ctx, fixture.settings, 2)
			var corruptErr *json.UnmarshalTypeError
			if !errors.As(err, &corruptErr) || errors.Is(err, ErrNotFound) {
				t.Fatalf("corrupt stored data lost its identity: %v", err)
			}
		}
		glog.Flush()
		fmt.Fprintln(os.Stderr, "synthetic-supervisor-read-capture-end")
	})
}
