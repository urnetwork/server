// Snapshot and offer-pool tasks must publish durable effects before handback.
package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Run serialized production code with real claim, result persistence and Post.
func runControllerAuditTask[A, R any](t testing.TB, owner *session.ClientSession,
	body func(A, *session.ClientSession) (R, error), post func(A, R, *session.ClientSession, server.PgTx) error, args A,
) R {
	ctx := owner.Ctx
	target := task.NewTaskTargetWithPost(body, post)
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(target)
	id := task.ScheduleTask(body, args, owner, task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
		t.Fatal(target.TargetFunctionName(), "task did not finish", finished, retried, posts, err)
	}
	var result R
	server.Db(ctx, func(conn server.PgConn) {
		var wire string
		var completed, future bool
		var pending int
		server.Raise(conn.QueryRow(ctx, `SELECT result_json,post_completed FROM finished_task WHERE task_id=$1`, id).Scan(&wire, &completed))
		server.Raise(json.Unmarshal([]byte(wire), &result))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*),bool_and(run_at>$2) FROM pending_task WHERE function_name=$1`,
			target.TargetFunctionName(), server.NowUtc()).Scan(&pending, &future))
		if !completed || pending != 1 || !future {
			t.Fatal(target.TargetFunctionName(), "missing recurrence", completed, pending, future)
		}
	})
	return result
}

// A scheduled rebuild must publish the account's points without inventing
// unavailable epoch metrics. Replaying the rebuild keeps the same ranking.
func TestPointsLeaderboardTaskPublishesAndRearms(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-task-ranking", server.NewId())
		model.Testing_InsertAccountPoint(ctx, networkId, model.PointsToNanoPoints(2), server.NowUtc())
		previous := pointsLeaderboardOperatorWindowFunc
		pointsLeaderboardOperatorWindowFunc = func(time.Time) []model.PointsEpochWindow { return nil }
		defer func() { pointsLeaderboardOperatorWindowFunc = previous }()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		for range 2 {
			result := runControllerAuditTask(t, owner, RebuildPointsLeaderboard, RebuildPointsLeaderboardPost, &RebuildPointsLeaderboardArgs{})
			rows, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Sort: model.PointsLeaderboardSortPoints}, owner)
			if result.TotalRanked != 1 || result.EpochMetricsAvailable || err != nil || rows.Error != nil ||
				len(rows.Rows) != 1 || rows.Rows[0].TotalPoints != 2 {
				t.Fatal("leaderboard task lost its source or invented epoch metrics", result, rows, err)
			}
		}
	})
}

// Rebuild the same source twice through each task. Rows and gauges must describe
// the source exactly once, and a malformed window must retain an explicit error.
func TestAsyncOnboardingAndSubscriptionSnapshots(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		now := server.NowUtc()
		day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-snapshot", server.NewId())
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: day,
			Email: true, TimeZone: "UTC", Platform: "ios", Locale: "en", Country: "US"}) {
			t.Fatal("synthetic cohort rejected")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			model.AddNetworkOnboardingEmailInTx(tx, ctx, &model.NetworkOnboardingEmail{MessageId: "synthetic-snapshot-email",
				NetworkId: networkId, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect,
				Variant: onboarding.VariantDefault, SentAt: day.Add(8 * time.Hour)})
		})
		server.Raise(model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{NetworkId: networkId,
			SubscriptionType: model.SubscriptionTypeSupporter, SubscriptionMarket: model.SubscriptionMarketApple,
			StartTime: day, EndTime: now.Add(24 * time.Hour), NetRevenue: model.UsdToNanoCents(5), TransactionId: "synthetic-paid-renewal"}))
		for range 2 {
			tracker := runControllerAuditTask(t, owner, OnboardingEmailTrackerSync, OnboardingEmailTrackerSyncPost,
				&OnboardingEmailTrackerSyncArgs{From: day, To: day})
			if tracker.Rows == 0 || tracker.SnapshotUnixTime == 0 ||
				testutil.ToFloat64(onboardingEmailTrackerNetworks.WithLabelValues(onboarding.StepE1, onboarding.EmailOutcomeSent)) != 1 {
				t.Fatal("email tracker failed to publish its source", tracker)
			}
			rollup := runControllerAuditTask(t, owner, OnboardingResultsRollup, OnboardingResultsRollupPost,
				&OnboardingResultsRollupArgs{From: day, To: day})
			if rollup.CohortNetworks != 1 || rollup.Rows == 0 {
				t.Fatal("cohort rollup lost or duplicated a network", rollup)
			}
			metrics := runControllerAuditTask(t, owner, SubscriptionMetricsSync, SubscriptionMetricsSyncPost, &SubscriptionMetricsSyncArgs{})
			families := gatherSubscriptionMetricFamilies(t, subscriptionMetrics)
			if metrics.SnapshotUnixTime == 0 || subscriptionMetricValue(t, families, "urnetwork_subscription_active_accounts",
				map[string]string{"store": model.SubscriptionMarketApple}) != 1 {
				t.Fatal("subscription task did not publish its paid account", metrics)
			}
		}
		if _, err := OnboardingEmailTrackerSync(&OnboardingEmailTrackerSyncArgs{From: now, To: day}, owner); err == nil {
			t.Fatal("invalid tracker window acknowledged")
		}
		if _, err := OnboardingResultsRollup(&OnboardingResultsRollupArgs{From: now, To: day}, owner); err == nil {
			t.Fatal("invalid cohort window acknowledged")
		}
	})
}

// All external calls terminate at a private HTTP fixture. Provider failures
// cannot create pool entries; replay stops once the configured floor is met.
func TestAppleOfferCodeTaskFailureRecoveryAndPoolReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := model.Onboarding()
		previousConfig := *config
		defer func() { *config = previousConfig }()
		config.Enabled = true
		config.Offer.Apple = model.OnboardingAppleOfferCodesConfig{OfferCodeId: "synthetic-offer", BatchSize: 500, MinAvailable: 2, ExpiryDays: 5}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		server.Raise(err)
		der, err := x509.MarshalECPrivateKey(key)
		server.Raise(err)
		credentials := &appStoreConnectCredentials{KeyId: "synthetic-key", IssuerId: "synthetic-issuer",
			PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))}
		previousUrl, previousCredentials := appStoreConnectBaseUrl, appStoreConnectCredentialsFunc
		defer func() { appStoreConnectBaseUrl, appStoreConnectCredentialsFunc = previousUrl, previousCredentials }()
		appStoreConnectCredentialsFunc = func() *appStoreConnectCredentials { return credentials }
		var fail atomic.Bool
		fail.Store(true)
		var requests atomic.Int64
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.Header.Get("Authorization") == "" {
				t.Error("offer request missing authorization")
			}
			if r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				if fail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"errors":[{"code":"synthetic_unavailable"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"id":"synthetic-batch"}}`))
				return
			}
			if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptionOfferCodeOneTimeUseCodes/synthetic-batch/values" {
				t.Error("unexpected offer request", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte("Code\nSYNTHETIC-ONE\nSYNTHETIC-TWO\n"))
		}))
		defer provider.Close()
		appStoreConnectBaseUrl = provider.URL
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		if _, err := AppleOfferCodeTopUp(&AppleOfferCodeTopUpArgs{}, owner); err == nil || model.CountAvailableAppleOfferCodes(ctx, server.NowUtc()) != 0 {
			t.Fatal("failed offer creation acknowledged or populated codes", err)
		}
		fail.Store(false)
		result := runControllerAuditTask(t, owner, AppleOfferCodeTopUp, AppleOfferCodeTopUpPost, &AppleOfferCodeTopUpArgs{})
		if result.Added != 2 || model.CountAvailableAppleOfferCodes(ctx, server.NowUtc()) != 2 {
			t.Fatal("top-up did not persist downloaded codes", result)
		}
		before := requests.Load()
		result = runControllerAuditTask(t, owner, AppleOfferCodeTopUp, AppleOfferCodeTopUpPost, &AppleOfferCodeTopUpArgs{})
		if result.Added != 0 || requests.Load() != before {
			t.Fatal("full-pool replay created another batch", result, requests.Load(), before)
		}
	})
}

// The task preserves the model's exact next-check time and stops missing-client
// chains without creating a successor; an existing live chain remains intact.
func TestProviderIntentTaskRearmsLiveStateAndStopsMissingState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		networkId, clientId := server.NewId(), server.NewId()
		next := server.NowUtc().Add(time.Hour).Truncate(time.Millisecond)
		model.Testing_SetProviderIntentState(ctx, networkId, clientId, &model.ProviderIntentState{
			Status: model.ProviderIntentStatusPending, AttemptTime: next.Add(-2 * time.Hour), GraceEndTime: next, CheckTime: next,
		}, 2*time.Hour)
		result := runControllerAuditTask(t, owner, ProviderIntentCheck, ProviderIntentCheckPost,
			&ProviderIntentCheckArgs{NetworkId: networkId, ClientId: clientId})
		if result.CheckTime == nil || !result.CheckTime.Equal(next) {
			t.Fatal("task changed the model's next check", result)
		}
		result = runControllerAuditTask(t, owner, ProviderIntentCheck, ProviderIntentCheckPost,
			&ProviderIntentCheckArgs{NetworkId: networkId, ClientId: server.NewId()})
		if result.CheckTime != nil {
			t.Fatal("missing state revived a stopped chain", result)
		}
	})
}
