package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestReadinessCheckDelegatesAndPropagatesFailure(t *testing.T) {
	wantErr := errors.New("database migration head 590 is below binary-required head 597")
	calls := 0
	err := readinessCheckWith(context.Background(), func(context.Context) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("readiness error = %v, want migration error", err)
	}
	if calls != 1 {
		t.Fatalf("shared readiness calls = %d, want 1", calls)
	}
}

func TestReadinessCheck(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// healthy pg + redis: ready
		if err := ReadinessCheck(ctx); err != nil {
			t.Fatalf("expected ready against a healthy env: %s", err)
		}
	})
}

// A failing check surfaces as a value (for the /status latch), never a panic
// or a crash. A canceled ctx no longer reaches a check: since 27242d349 a
// stopped startup returns its context's cause before any check runs
// (router.TestStartupReadinessPreservesStoppedContextCause), and no dependency
// failed, so none is named. A database without a migration history fails the
// first check for real: pg's read of migration_audit.
func TestReadinessCheckFailure(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := ReadinessCheck(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("a stopped startup = %v, want its context's cause", err)
		}

		err := ReadinessCheck(context.Background())
		if err == nil {
			t.Fatalf("expected a readiness error against a database without migrations")
		}
		// the first failing check (pg) names itself so the latched /status
		// reads `error not ready: pg: ...`
		if !strings.HasPrefix(err.Error(), "pg:") {
			t.Fatalf("expected the failing check to name itself: %s", err)
		}
	})
}
