package work

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A run counts what it newly discounted by outcome, once, records when it ran,
// and reports its buckets.
func TestProviderEgressUrlOverloadJudgementIsCountedOnce(t *testing.T) {
	successBefore := testutil.ToFloat64(providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("success"))
	errorBefore := testutil.ToFloat64(providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("error"))
	now := time.Date(2026, 11, 25, 0, 6, 0, 0, time.UTC)
	entered := &model.ProviderEgressUrlFleetBucket{
		BucketStart:  now.Truncate(5 * time.Minute),
		BucketEnd:    now.Truncate(5 * time.Minute).Add(5 * time.Minute),
		OutcomeCount: 4800, ErrorCount: 4224,
		CountryOutcomeCount: 900, CountryErrorCount: 810,
		NetworkOutcomeCount: 60, NetworkErrorCount: 54,
		BaselineOutcomeCount: 380000, BaselineErrorCount: 125400,
		Discounted: true, DiscountedOutcomeCount: 4800, DiscountedErrorCount: 4224,
	}
	scored := &model.ProviderEgressUrlFleetBucket{BucketStart: entered.BucketStart.Add(-5 * time.Minute), BucketEnd: entered.BucketStart, OutcomeCount: 4000, ErrorCount: 1300}
	result := exportProviderEgressUrlOverloadJudgement(now, &model.ProviderEgressUrlOverloadJudgement{
		Buckets:                []*model.ProviderEgressUrlFleetBucket{scored, entered},
		Entered:                []*model.ProviderEgressUrlFleetBucket{entered},
		DiscountedOutcomeCount: 4800,
		DiscountedErrorCount:   4224,
		RemovedCount:           2,
	})
	if *result != (JudgeProviderEgressUrlOverloadResult{Buckets: 2, Discounted: 1, Entered: 1, DiscountedOutcomes: 4800, DiscountedErrors: 4224, Removed: 2}) {
		t.Fatalf("the run reported %+v", result)
	}
	if got := testutil.ToFloat64(providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("success")) - successBefore; got != 576 {
		t.Errorf("discounted successes counted %v, want 576", got)
	}
	if got := testutil.ToFloat64(providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("error")) - errorBefore; got != 4224 {
		t.Errorf("discounted failures counted %v, want 4224", got)
	}
	if got := testutil.ToFloat64(providerEgressUrlOverloadJudgeLastRunTimestamp); got != float64(now.Unix()) {
		t.Errorf("the last run timestamp is %v", got)
	}

	// a run that found nothing new counts nothing
	exportProviderEgressUrlOverloadJudgement(now.Add(time.Minute), &model.ProviderEgressUrlOverloadJudgement{
		Buckets: []*model.ProviderEgressUrlFleetBucket{scored, entered},
	})
	if got := testutil.ToFloat64(providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("error")) - errorBefore; got != 4224 {
		t.Errorf("a run with nothing new counted failures again: %v", got)
	}
}

// A run against the database with no outcomes judges nothing and still
// records that it ran; its Post schedules the next run one interval out, and
// the startup schedule merges into it.
func TestJudgeProviderEgressUrlOverloadRunsAndReschedules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		settings := model.DefaultProviderEgressUrlOverloadSettings()

		now := server.NowUtc()
		result := judgeProviderEgressUrlOverload(ctx, now, settings)
		if *result != (JudgeProviderEgressUrlOverloadResult{}) {
			t.Fatalf("a run over no outcomes reported %+v", result)
		}
		if got := testutil.ToFloat64(providerEgressUrlOverloadJudgeLastRunTimestamp); got != float64(now.UnixMilli())/1000 {
			t.Fatalf("the last run timestamp is %v", got)
		}

		readRunAt := func() (runAt time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE run_once_key = '["judge_provider_egress_url_overload"]'`).Scan(&runAt))
			})
			return
		}
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(JudgeProviderEgressUrlOverloadPost(&JudgeProviderEgressUrlOverloadArgs{}, result, clientSession, tx))
		})
		runAt := readRunAt()
		if delay := runAt.Sub(before); delay < settings.JudgeInterval || settings.JudgeInterval+time.Minute < delay {
			t.Fatalf("the next run is %s out, want %s", delay, settings.JudgeInterval)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleJudgeProviderEgressUrlOverload(clientSession, tx)
		})
		if rescheduledAt := readRunAt(); !rescheduledAt.Before(runAt) {
			t.Fatalf("the startup schedule did not bring the run forward: %s, was %s", rescheduledAt, runAt)
		}
	})
}
