// Subprocess-only observation never replaces a global logger or echoes private
// incident logs. These windows contain only this test's synthetic disposable DB.
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

func competitionCompletionLogOutput(t *testing.T, mode string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCompetitionCompletionLogChild$", "-test.count=1", "-test.v", "-logtostderr=true")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "URNETWORK_TEST_COMPLETION_POLL_CHILD=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "URNETWORK_TEST_COMPLETION_POLL_CHILD="+mode)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("completion log child %s failed: %v", mode, err)
	}
	start, end := []byte("synthetic-completion-capture-start\n"), []byte("synthetic-completion-capture-end\n")
	begin, finish := bytes.Index(output, start), bytes.Index(output, end)
	if begin < 0 || finish < begin+len(start) || bytes.Count(output, start) != 1 || bytes.Count(output, end) != 1 {
		t.Fatal("completion child did not finish its exact synthetic capture window")
	}
	return output[begin+len(start) : finish]
}

func TestCompetitionCompletionExpectedHoldDoesNotLogUnexpectedStacks(t *testing.T) {
	if bytes.Contains(competitionCompletionLogOutput(t, "held"), []byte("Unexpected error:")) {
		t.Fatal("expected held worker/store control flow emitted an unexpected stack")
	}
}

func TestCompetitionCompletionGenuineDatabaseFailureStillLogs(t *testing.T) {
	if !bytes.Contains(competitionCompletionLogOutput(t, "query-error"), []byte("Unexpected error:")) {
		t.Fatal("genuine completion poll database failure was silently suppressed")
	}
}

// This fault affects the actual SELECT on this already-owned test connection.
// Raw PgConn calls avoid a recursive tracer or a nested pool acquisition.
type competitionCompletionQueryFault struct{ once sync.Once }
type competitionCompletionQueryFaultKey struct{}

func (self *competitionCompletionQueryFault) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT r.closes_at,r.automation_sealed_at,r.canceled,") {
		changed := false
		self.once.Do(func() {
			_, err := conn.PgConn().Exec(ctx, `SET search_path TO pg_catalog`).ReadAll()
			server.Raise(err)
			changed = true
		})
		if changed {
			return context.WithValue(ctx, competitionCompletionQueryFaultKey{}, true)
		}
	}
	return ctx
}
func (self *competitionCompletionQueryFault) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if changed, _ := ctx.Value(competitionCompletionQueryFaultKey{}).(bool); changed {
		reset, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := conn.PgConn().Exec(reset, `RESET search_path`).ReadAll()
		server.Raise(err)
	}
}

func TestCompetitionCompletionLogChild(t *testing.T) {
	mode := os.Getenv("URNETWORK_TEST_COMPLETION_POLL_CHILD")
	if mode == "" {
		t.Skip("subprocess-only synthetic completion logging observation")
	}
	if mode != "held" && mode != "query-error" {
		t.Fatal("invalid completion child mode")
	}
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		var scope interface{ Close() error }
		if mode == "query-error" {
			var err error
			scope, err = server.NewTestPgQueryScope(tb.Context(), &competitionCompletionQueryFault{})
			if err != nil {
				tb.Fatal(err)
			}
			defer scope.Close()
		}
		glog.Flush()
		fmt.Fprintln(os.Stderr, "synthetic-completion-capture-start")
		if mode == "query-error" {
			ready, err := store.PostgresStore.automationEpochSealReady(tb.Context(), fixture.settings, fixture.round.RoundId)
			var postgres *pgconn.PgError
			if ready || !errors.As(err, &postgres) || postgres.Code != "42P01" || errors.Is(err, ErrAutomationHeld) {
				tb.Fatal("real query failure lost its original identity", err)
			}
		} else {
			store.clock.Store(fixture.round.ClosesAt.UnixNano())
			var once sync.Once
			store.beforeSeal = func(ctx context.Context) (err error) {
				once.Do(func() {
					_, err = store.SetAutomationHold(ctx, fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-captured-late-hold", true, 0))
				})
				return err
			}
			cancel, done, poll, heartbeat, evaluator := startCompetitionCompletionWorkerPg(tb, fixture, store)
			_ = receiveCompetitionCompletion(tb, store.registered, done)
			if result := receiveCompetitionCompletion(tb, store.sealed, done); result.Result != nil || result.Err != ErrAutomationHeld {
				tb.Fatal("late held seal did not return idle", result)
			}
			_ = receiveCompetitionCompletion(tb, store.polls, done)
			for range 3 {
				tickCompetitionCompletion(tb, poll, done)
				if result := receiveCompetitionCompletion(tb, store.polls, done); result.Ready || result.Err != ErrAutomationHeld {
					tb.Fatal("captured held worker was not idle", result)
				}
				if result, err := store.PostgresStore.SealAndElect(tb.Context(), fixture.settings, fixture.round.RoundId); result != nil || err != ErrAutomationHeld {
					tb.Fatal("held direct seal changed its exact sentinel", err)
				}
			}
			tickCompetitionCompletion(tb, heartbeat, done)
			_ = receiveCompetitionCompletion(tb, store.registered, done)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) || evaluator.forbidden.Load() != 0 {
				tb.Fatal("captured held worker did not join without execution", err)
			}
		}
		glog.Flush()
		fmt.Fprintln(os.Stderr, "synthetic-completion-capture-end")
	})
}
