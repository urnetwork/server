package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
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

func TestReadinessCheckFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// a canceled ctx forces both checks to fail deterministically; the
		// error must surface as a value (for the /status latch), never a
		// panic or a crash
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := ReadinessCheck(ctx)
		if err == nil {
			t.Fatalf("expected a readiness error with a canceled ctx")
		}
		// the first failing check (pg) names itself so the latched /status
		// reads `error not ready: pg: ...`
		if !strings.HasPrefix(err.Error(), "pg:") {
			t.Fatalf("expected the failing check to name itself: %s", err)
		}
	})
}
