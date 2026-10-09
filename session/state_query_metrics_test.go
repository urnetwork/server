// Pin metric cardinality and error propagation independently of PostgreSQL.
package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Reads a preallocated bounded counter without any I/O.
func stateQueryCount(source StateQuerySource, client bool, outcome stateQueryOutcome) float64 {
	credential := 0
	if client {
		credential = 1
	}
	return testutil.ToFloat64(stateQueryCounters[source][credential][outcome])
}

// Every source has one fixed caller/operation pair and both statement shapes.
// Unknown and invalid contexts cannot create another label child.
func TestStateQueryMetricSourcesAreBounded(t *testing.T) {
	seen := map[[2]string]bool{}
	for source, labels := range stateQuerySourceLabels {
		if labels[0] == "" || labels[1] == "" || seen[labels] {
			t.Fatal("source labels are empty or duplicate")
		}
		seen[labels] = true
		for _, client := range []bool{false, true} {
			ctx, cancel := context.WithCancel(WithStateQuerySource(t.Context(), StateQuerySource(source)))
			before := stateQueryCount(StateQuerySource(source), client, stateQueryValid)
			query := beginStateQuery(ctx, client)
			query.complete(true, time.Unix(2, 0), time.Unix(1, 0))
			query.finish(ctx)
			cancel()
			if stateQueryCount(StateQuerySource(source), client, stateQueryValid) != before+1 {
				t.Fatal("child context lost its trusted source")
			}
		}
	}
	for _, ctx := range []context.Context{
		t.Context(),
		WithStateQuerySource(t.Context(), StateQuerySource(255)),
		context.WithValue(t.Context(), stateQuerySourceKey{}, "synthetic-client-supplied-label"),
	} {
		before := stateQueryCount(StateQueryUnknown, true, stateQueryNoActiveRow)
		query := beginStateQuery(ctx, true)
		query.complete(false, time.Time{}, time.Time{})
		query.finish(ctx)
		if stateQueryCount(StateQueryUnknown, true, stateQueryNoActiveRow) != before+1 {
			t.Fatal("unannotated or malformed source escaped unknown coverage")
		}
	}
	if count := testutil.CollectAndCount(stateQueryCounter); count != int(stateQuerySourceCount)*2*int(stateQueryOutcomeCount) {
		t.Fatalf("unbounded metric cardinality: %d", count)
	}
}

// Observing query failure must never consume a panic or translate it into an
// authorization refusal. Real caller contexts determine cancellation labels.
func TestStateQueryMetricFailurePreservesPanic(t *testing.T) {
	for _, outcome := range []stateQueryOutcome{stateQueryError, stateQueryCanceled, stateQueryDeadline} {
		ctx := t.Context()
		cancel := func() {}
		if outcome == stateQueryCanceled {
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		} else if outcome == stateQueryDeadline {
			ctx, cancel = context.WithDeadline(ctx, time.Unix(1, 0))
		}
		ctx = WithStateQuerySource(ctx, StateQueryConnectH1)
		before := stateQueryCount(StateQueryConnectH1, true, outcome)
		want := errors.New("synthetic query failure")
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			query := beginStateQuery(ctx, true)
			defer query.finish(ctx)
			panic(want)
		}()
		cancel()
		if recovered != want || stateQueryCount(StateQueryConnectH1, true, outcome) != before+1 {
			t.Fatal("metric changed query failure or counted the wrong outcome")
		}
	}
}

// A real acquisition refusal occurs before the SQL observation, even though
// the caller carried a trusted source. Preserve its panic and zero query count.
func TestStateQueryMetricAcquisitionRefusalIsNotSqlWork(t *testing.T) {
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	ctx := WithStateQuerySource(t.Context(), StateQueryHostedControl)
	claims := NewByJwt(server.NewId(), server.NewId(), "synthetic", false, false).Client(server.NewId(), server.NewId())
	before := 0.0
	for outcome := stateQueryOutcome(0); outcome < stateQueryOutcomeCount; outcome++ {
		before += stateQueryCount(StateQueryHostedControl, true, outcome)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = ValidateByJwtState(ctx, claims, true)
	}()
	after := 0.0
	for outcome := stateQueryOutcome(0); outcome < stateQueryOutcomeCount; outcome++ {
		after += stateQueryCount(StateQueryHostedControl, true, outcome)
	}
	if recovered != server.ErrPacketPostgres || attempts() != 1 || after != before {
		t.Fatal("acquisition refusal changed its panic or counted an unstarted query")
	}
}

// Isolates counter overhead; the separate request/PGSS fixture measures the
// complete authentication operation under PostgreSQL concurrency.
func BenchmarkStateQueryObservation(b *testing.B) {
	ctx := WithStateQuerySource(b.Context(), StateQueryProberControl)
	created, changed := time.Unix(2, 0), time.Unix(1, 0)
	b.ReportAllocs()
	for b.Loop() {
		query := beginStateQuery(ctx, true)
		query.complete(true, created, changed)
		query.finish(ctx)
	}
}
