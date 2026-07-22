package api

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

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
