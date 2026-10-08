package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLegacyPayerDueIndexReadinessOutcomes(t *testing.T) {
	for _, test := range []struct {
		name  string
		ready bool
		err   error
	}{
		{name: "ready", ready: true},
		{name: "catalog_invalid"},
		{name: "deadline", err: fmt.Errorf("private read detail: %w", context.DeadlineExceeded)},
		{name: "canceled", err: fmt.Errorf("private read detail: %w", context.Canceled)},
		{name: "read_error", err: errors.New("private database detail")},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, clocks := 0, 0
			observed := observeLegacySettlementPayerDueIndex(t.Context(), func(ctx context.Context) (bool, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || deadline.After(time.Now().Add(legacySettlementPayerIndexBudget)) {
					t.Fatal("probe lost its original bounded deadline")
				}
				return test.ready, test.err
			}, func() time.Time {
				clocks++
				return time.Unix(1, 0).Add(time.Duration(clocks) * 37 * time.Millisecond)
			})
			if observed.Outcome != test.name || observed.ElapsedMs != 37 || calls != 1 || clocks != 2 {
				t.Fatal("probe outcome, elapsed or single-read boundary changed", observed, calls, clocks)
			}
			encoded, err := json.Marshal(observed)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 || fields["outcome"] != test.name || fields["elapsed_ms"] != float64(37) {
				t.Fatal("readiness retained fields beyond its fixed outcome and elapsed", string(encoded), err)
			}
		})
	}
}

func TestLegacyPayerDueIndexDeadlineIsNotCatalogInvalid(t *testing.T) {
	if legacySettlementPayerIndexBudget != 250*time.Millisecond {
		t.Fatal("readiness changed the production probe budget")
	}
	observed := observeLegacySettlementPayerDueIndex(t.Context(), func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		// Even an uncooperative reader's late valid row cannot qualify a probe
		// whose original context already expired.
		return true, nil
	}, time.Now)
	if observed.Outcome != "deadline" || observed.ElapsedMs < legacySettlementPayerIndexBudget.Milliseconds() {
		t.Fatal("expired probe was reported as catalog state", observed)
	}
}

func TestLegacyPayerDueIndexParentCancellationIsNotCatalogInvalid(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	observed := observeLegacySettlementPayerDueIndex(ctx, func(readCtx context.Context) (bool, error) {
		cancel()
		if !errors.Is(readCtx.Err(), context.Canceled) {
			t.Fatal("probe lost parent cancellation")
		}
		return true, nil
	}, time.Now)
	if observed.Outcome != "canceled" {
		t.Fatal("parent cancellation was reported as catalog state", observed)
	}
}
