package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

type recordingReliabilityRunningCheckpoint struct {
	statements []string
	arguments  [][]any
}

func (r *recordingReliabilityRunningCheckpoint) Exec(_ context.Context, sql string, arguments ...any) (server.PgTag, error) {
	r.statements = append(r.statements, sql)
	r.arguments = append(r.arguments, append([]any(nil), arguments...))
	return server.PgTag{}, nil
}

// A hard worker-host loss used to remove the task evaluator's context while
// PostgreSQL kept a long rolling UPDATE and its transaction alive. The
// replacement claim then blocked behind that orphan. Keep the task's existing
// two-hour ceiling as a transaction-local PostgreSQL guard, rather than a
// session/global setting or an unbounded client-only deadline.
func TestReliabilityRunningCheckpointConfiguresServerSideTimeout(t *testing.T) {
	recorder := &recordingReliabilityRunningCheckpoint{}
	configureReliabilityRunningCheckpoint(context.Background(), recorder)

	if len(recorder.statements) != 1 {
		t.Fatalf("reliability checkpoint statements = %d, want 1", len(recorder.statements))
	}
	if got, want := recorder.statements[0], `SELECT set_config('statement_timeout', $1, true)`; got != want {
		t.Fatalf("reliability checkpoint statement = %q, want %q", got, want)
	}
	if len(recorder.arguments) != 1 || len(recorder.arguments[0]) != 1 {
		t.Fatalf("reliability checkpoint argument shape = %v, want one argument", recorder.arguments)
	}
	if got, want := recorder.arguments[0][0], "7200000ms"; got != want {
		t.Fatalf("reliability checkpoint timeout = %v, want %s", got, want)
	}
}
