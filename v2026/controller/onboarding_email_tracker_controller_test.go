package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func TestOnboardingEmailTrackerWindowAndGauges(t *testing.T) {
	now := time.Date(2026, 1, 28, 12, 0, 0, 0, time.UTC)
	from, to, err := onboardingEmailTrackerWindow(now, time.Time{}, time.Time{})
	connect.AssertEqual(t, nil, err)
	// The rebuild includes 31 send days so a correction just inside the
	// 30-day provider window is not missed.
	connect.AssertEqual(t, "2025-12-29", from.Format("2006-01-02"))
	connect.AssertEqual(t, "2026-01-28", to.Format("2006-01-02"))
	metricFrom, metricTo := onboardingEmailTrackerMetricWindow(now)
	connect.AssertEqual(t, "2026-01-01", metricFrom.Format("2006-01-02"))
	connect.AssertEqual(t, "2026-01-28", metricTo.Format("2006-01-02"))

	setOnboardingEmailTrackerGauges(map[string]*model.OnboardingEmailTrackerTotals{
		onboarding.StepE1: {Step: onboarding.StepE1, Sent: 12, Opened: 7, Engaged: 4},
	}, now)
	connect.AssertEqual(t, float64(12), testutil.ToFloat64(onboardingEmailTrackerNetworks.WithLabelValues(onboarding.StepE1, onboarding.EmailOutcomeSent)))
	connect.AssertEqual(t, float64(4), testutil.ToFloat64(onboardingEmailTrackerNetworks.WithLabelValues(onboarding.StepE1, onboarding.EmailOutcomeEngaged)))
	// A complete snapshot exports known zeroes for every step/outcome; an
	// absent exporter is represented by the absent snapshot timestamp instead.
	connect.AssertEqual(t, float64(0), testutil.ToFloat64(onboardingEmailTrackerNetworks.WithLabelValues(onboarding.StepE5, onboarding.EmailOutcomeSent)))
	connect.AssertEqual(t, float64(now.Unix()), testutil.ToFloat64(onboardingEmailTrackerSnapshotTimestamp))

	_, _, err = onboardingEmailTrackerWindow(now, now, now.Add(-24*time.Hour))
	connect.AssertEqual(t, true, err != nil)
}

func TestScheduleOnboardingEmailTrackerSyncIsFleetWideRunOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// Task scheduling hashes the local synthetic client address. Keep this
		// fixture self-contained and independent of any developer Vault.
		t.Cleanup(server.Vault.PushSimpleResource(
			"client.yml",
			[]byte("client_ip_hash_pepper: synthetic-onboarding-tracker-test-only\n"),
		))
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleOnboardingEmailTrackerSync(clientSession, tx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			ScheduleOnboardingEmailTrackerSync(clientSession, tx, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
		})

		functionName := task.NewTaskTarget(OnboardingEmailTrackerSync).TargetFunctionName()
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT COUNT(*) FROM pending_task WHERE function_name = $1`, functionName)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, true, result.Next())
				server.Raise(result.Scan(&count))
			})
		})
		connect.AssertEqual(t, 1, count)
	})
}
