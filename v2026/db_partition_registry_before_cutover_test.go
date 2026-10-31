// The registry's pre-cutover hook, without a database.
package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The registry runs BeforeCutover on each runway pass while a family is not
// partitioned, with the pass's clock, and reports its failure.
func TestPartitionRegistryRunsBeforeCutoverUntilPartitioned(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	var calls []time.Time
	family := testPartitionDailyFamily()
	family.Partitioned = func(ctx context.Context) bool { return false }
	family.BeforeCutover = func(ctx context.Context, at time.Time) error {
		calls = append(calls, at)
		return errors.New("synthetic refusal")
	}
	report := family.EnsureRunway(ctx, now)
	if len(calls) != 1 || !calls[0].Equal(now) {
		t.Fatalf("hook calls %v", calls)
	}
	if report.Partitioned || len(report.Created) != 0 || len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "synthetic refusal") {
		t.Fatalf("report %+v", report)
	}
	family.BeforeCutover = func(ctx context.Context, at time.Time) error {
		calls = append(calls, at)
		return nil
	}
	if report := family.EnsureRunway(ctx, now.Add(time.Hour)); len(report.Errors) != 0 || len(calls) != 2 {
		t.Fatalf("report %+v after %v", report, calls)
	}
}
