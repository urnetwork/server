// The soft boundary admits a financial write phase with cleanup time left.
// Acknowledged writes finish under the original hard transaction deadline.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const legacyFinancialCohortAdmissionTime = 500 * time.Millisecond

var errLegacyFinancialCohortBudget = errors.New("legacy financial cohort admission budget exhausted")

type legacyFinancialCohortBudgetKey struct{}
type legacyFinancialCohortClockKey struct{}

type legacyFinancialCohortBudget struct {
	now         func() time.Time
	admitBefore time.Time
	written     bool
	diagnostic  *legacyFinancialDiagnostic
}

// The optional clock belongs to one fixture context. Production uses monotonic
// elapsed time; neither clock can extend the existing hard query deadline.
func withLegacyFinancialCohortBudget(ctx context.Context) context.Context {
	now, _ := ctx.Value(legacyFinancialCohortClockKey{}).(func() time.Time)
	if now == nil {
		now = time.Now
	}
	deadline, _ := ctx.Deadline()
	return context.WithValue(ctx, legacyFinancialCohortBudgetKey{}, &legacyFinancialCohortBudget{
		now: now, admitBefore: deadline.Add(-legacyFinancialCohortAdmissionTime),
	})
}

// Reserve 500 ms of the bounded body budget before the first write phase.
// Once that phase has acknowledged writes, finish it under the same hard
// context. A soft boundary must not roll back and repeat admitted financial work.
// Cancellation and in-flight errors still retain their ordinary custody.
func checkLegacyFinancialCohortBudget(ctx context.Context, stage ...string) error {
	if err := ctx.Err(); err != nil {
		observeLegacyFinancialBudgetBoundary(ctx, stage, "context_error")
		return err
	}
	if budget, _ := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget); budget != nil && !budget.written && !budget.now().Before(budget.admitBefore) {
		observeLegacyFinancialBudgetBoundary(ctx, stage, "soft_refusal")
		if len(stage) > 0 {
			return fmt.Errorf("%w before %s", errLegacyFinancialCohortBudget, stage[0])
		}
		return errLegacyFinancialCohortBudget
	}
	observeLegacyFinancialBudgetBoundary(ctx, stage, "ready")
	return nil
}
