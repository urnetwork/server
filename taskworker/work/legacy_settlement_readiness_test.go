package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/server/model"
)

func TestLegacyDispatcherReadinessSurvivesFinancialFallback(t *testing.T) {
	for _, outcome := range []string{"ready", "catalog_invalid", "deadline", "canceled", "read_error"} {
		args := &FlushLegacySettlementsArgs{Shard: 13,
			Cursor: &model.LegacySettlementCursor{}, PayerCursor: &model.LegacySettlementPayerCursor{}}
		readiness := &model.LegacySettlementPayerIndexReadiness{Outcome: outcome, ElapsedMs: 37}
		dispatchCalls, legacyCalls := 0, 0
		result, err := flushLegacySettlements(args, t.Context(),
			func(ctx context.Context, shard int, cursor *model.LegacySettlementCursor, payerCursor *model.LegacySettlementPayerCursor) (model.LegacySettlementDispatchResult, *model.LegacySettlementPayerIndexReadiness, error) {
				dispatchCalls++
				if ctx != t.Context() || shard != args.Shard || cursor != args.Cursor || payerCursor != args.PayerCursor {
					t.Fatal("dispatch scope changed")
				}
				var err error
				if outcome != "ready" {
					err = fmt.Errorf("synthetic probe: %w", model.ErrLegacySettlementPayerIndexUnavailable)
				}
				return model.LegacySettlementDispatchResult{Private: true, Probes: 1}, readiness, err
			},
			func(ctx context.Context, shard int, cursor *model.LegacySettlementCursor, payerCursor *model.LegacySettlementPayerCursor, limit int) (model.LegacySettlementShardResult, error) {
				legacyCalls++
				if ctx != t.Context() || shard != args.Shard || cursor != args.Cursor || payerCursor != args.PayerCursor || limit != model.LegacySettlementPageLimit {
					t.Fatal("financial fallback scope or bound changed")
				}
				return model.LegacySettlementShardResult{LegacySettlementFlushResult: model.LegacySettlementFlushResult{Visited: 18, Completed: 18}}, nil
			})
		if err != nil || dispatchCalls != 1 || result.IndexReadiness != readiness {
			t.Fatal("readiness was lost at the task boundary", result, err, dispatchCalls)
		}
		if outcome == "ready" {
			if legacyCalls != 0 || result.Dispatch == nil || result.Dispatch.Probes != 1 {
				t.Fatal("ready dispatch entered financial fallback", result, legacyCalls)
			}
		} else if legacyCalls != 1 || result.Dispatch != nil || result.Completed != 18 {
			t.Fatal("unavailable dispatch lost its bounded fallback result", result, legacyCalls)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var stored FlushLegacySettlementsResult
		if err := json.Unmarshal(encoded, &stored); err != nil || stored.IndexReadiness == nil || *stored.IndexReadiness != *readiness {
			t.Fatal("persisted task result lost the exact readiness outcome", err)
		}
	}
}

func TestLegacyDispatcherReadinessPreservesTaskErrors(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		args := &FlushLegacySettlementsArgs{Shard: 13}
		readiness := &model.LegacySettlementPayerIndexReadiness{Outcome: "read_error", ElapsedMs: 37}
		want := errors.New("synthetic task error")
		legacyCalls := 0
		result, err := flushLegacySettlements(args, t.Context(),
			func(context.Context, int, *model.LegacySettlementCursor, *model.LegacySettlementPayerCursor) (model.LegacySettlementDispatchResult, *model.LegacySettlementPayerIndexReadiness, error) {
				if fallback {
					return model.LegacySettlementDispatchResult{}, readiness, model.ErrLegacySettlementPayerIndexUnavailable
				}
				return model.LegacySettlementDispatchResult{}, readiness, want
			},
			func(context.Context, int, *model.LegacySettlementCursor, *model.LegacySettlementPayerCursor, int) (model.LegacySettlementShardResult, error) {
				legacyCalls++
				return model.LegacySettlementShardResult{}, want
			})
		if !errors.Is(err, want) || result.IndexReadiness != readiness || legacyCalls != map[bool]int{false: 0, true: 1}[fallback] {
			t.Fatal("readiness rewrote the error or expanded fallback", fallback, result, err, legacyCalls)
		}
	}
}
