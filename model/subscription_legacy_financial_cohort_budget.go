// Cohort admission ends at completed protocol boundaries, before its hard
// timeout can cancel an in-flight response and destroy transaction ownership.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const legacyFinancialCohortAdmissionTime = legacyFinancialCohortTimeout / 2

var errLegacyFinancialCohortBudget = errors.New("legacy financial cohort admission budget exhausted")

type legacyFinancialCohortBudgetKey struct{}
type legacyFinancialCohortClockKey struct{}

type legacyFinancialCohortBudget struct {
	now         func() time.Time
	admitBefore time.Time
	written     bool
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

// Reserve half the unchanged 500 ms hard budget for the admitted exchange and
// confirmed cleanup. A slow or canceled exchange still fails normally; this
// guard neither detaches financial work nor reports an unknown release as known.
func checkLegacyFinancialCohortBudget(ctx context.Context, stage ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget, _ := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget); budget != nil && !budget.now().Before(budget.admitBefore) {
		if len(stage) > 0 {
			return fmt.Errorf("%w before %s", errLegacyFinancialCohortBudget, stage[0])
		}
		return errLegacyFinancialCohortBudget
	}
	return nil
}
