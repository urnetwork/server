package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The wait the account's activation budget asks of an account whose hosts
// never pass (research extender-health-2026-11-11 §1.2 and §5 item 3), against
// the database. The jitter is off, so every wait is exact.

// The words every refusal by the account's budget over ipv4 starts with, which
// is all an activator that predates the longer wait shows.
const testAccountRefusalStart = "You have reached the maximum number of attempts for activating your account's extenders over IPv4 in the last hour."

// An account none of whose hosts has passed activation on the family is told
// to wait longer the more of its floor it has spent on probes that failed: an
// hour for its first hour's worth over the last day, two hours for two, four
// for three, and six from four on, which is the most the activator holds a
// family. Before, every account was told to come back when its floor had room,
// under an hour, so a fleet that never passes came back every hour all day:
// two such accounts were refused about 96,000 posts a day, each spending
// exactly its 60 probes an hour. The refusal is counted as unproven_account
// and says why.
func TestExtenderActivateHoldsAnAccountWhoseHostsNeverPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		settings := DefaultExtenderActivateSettings()
		settings.BudgetRetryAfterJitter = 0
		metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
		settings.metrics = metrics
		service := newExtenderActivateService(settings)
		service.testProbes = testFakeExtenderProbes(errors.New("connection refused"))

		const accountAction = model.AccountActionExtenderActivateAccountV4
		for i, c := range []struct {
			name string
			// the hours before this one in which the account also spent its
			// floor
			earlierHourCount int
			wantRetryAfter   time.Duration
		}{
			{name: "its first hour", earlierHourCount: 0, wantRetryAfter: time.Hour},
			{name: "its second hour", earlierHourCount: 1, wantRetryAfter: 2 * time.Hour},
			{name: "its third hour", earlierHourCount: 2, wantRetryAfter: 4 * time.Hour},
			{name: "its fourth hour", earlierHourCount: 3, wantRetryAfter: 6 * time.Hour},
			{name: "all day", earlierHourCount: 23, wantRetryAfter: 6 * time.Hour},
		} {
			account := newTestExtenderAccount(ctx)
			now := server.NowUtc()
			// the floor of this hour, spent a minute ago
			seedTestAccountActionAttempts(t, ctx, account.userId, accountAction, settings.AccountRateLimit, now.Add(-time.Minute))
			for hour := 1; hour <= c.earlierHourCount; hour += 1 {
				seedTestAccountActionAttempts(
					t,
					ctx,
					account.userId,
					accountAction,
					settings.AccountRateLimit,
					now.Add(-time.Duration(hour)*time.Hour-30*time.Minute),
				)
			}

			refused := testActivateHost(t, ctx, service, account.newHost(t, ctx), testExtenderFleetIp(t, i, 4))
			if refused.Activated || !strings.HasPrefix(refused.Error, testAccountRefusalStart) {
				t.Fatalf("%s: the post was not refused by the account's budget: activated %t, %q", c.name, refused.Activated, refused.Error)
			}
			if !strings.Contains(refused.Error, "None of your account's extenders has passed activation over IPv4 in the last 7 days") {
				t.Errorf("%s: the refusal does not say why it waits longer: %q", c.name, refused.Error)
			}
			if want := int(c.wantRetryAfter / time.Second); refused.RetryAfterSeconds != want {
				t.Errorf("%s: the refusal asks for %d seconds, want %d", c.name, refused.RetryAfterSeconds, want)
			}
		}
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("budget", "unproven_account", "ipv4")), float64(5))
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("budget", "account", "ipv4")), float64(0))
	})
}

// An account any of whose hosts has passed keeps the wait until its budget has
// room, and is counted as account. That holds for an account whose only host
// passed ten minutes ago, which is no proven host yet and leaves the account at
// its floor (ProvenHostMinAge), so a fleet that is ramping up is not held for
// hours, and for an account at the limit its proven hosts raised it to.
func TestExtenderActivateKeepsTheWaitOfAnAccountWhoseHostsPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		settings := DefaultExtenderActivateSettings()
		settings.BudgetRetryAfterJitter = 0
		metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
		settings.metrics = metrics
		service := newExtenderActivateService(settings)
		service.testProbes = testFakeExtenderProbes(nil)

		const accountAction = model.AccountActionExtenderActivateAccountV4
		requireTheBudgetsWait := func(name string, refused *ExtenderActivateResult) {
			t.Helper()
			if refused.Activated || !strings.HasPrefix(refused.Error, testAccountRefusalStart) {
				t.Fatalf("%s: the post was not refused by the account's budget: activated %t, %q", name, refused.Activated, refused.Error)
			}
			if strings.Contains(refused.Error, "None of your account's extenders") {
				t.Errorf("%s: an account whose hosts pass was told none has: %q", name, refused.Error)
			}
			// the oldest attempt of the hour is from ten minutes or a minute
			// ago, so the budget has room again within the hour
			if refused.RetryAfterSeconds <= 45*60 || 60*60 < refused.RetryAfterSeconds {
				t.Errorf("%s: the refusal asks for %d seconds, want the rest of the hour", name, refused.RetryAfterSeconds)
			}
		}

		// a host passed ten minutes ago, and the floor is spent
		rampingAccount := testRampingExtenderAccount(t, ctx, service, settings, accountAction)
		requireTheBudgetsWait(
			"a ramping account",
			testActivateHost(t, ctx, service, rampingAccount.newHost(t, ctx), testExtenderFleetIp(t, 1, 4)),
		)

		// ten hosts passed two hours ago, raising the limit to 60 + 6 × 10,
		// and the limit is spent
		provenAccount := newTestExtenderAccount(ctx)
		for i := range 10 {
			seedTestProvenExtenderHost(t, ctx, provenAccount.newHost(t, ctx), map[int]string{
				4: testExtenderFleetIp(t, 10+i, 4),
			}, server.NowUtc().Add(-2*time.Hour))
		}
		seedTestAccountActionAttempts(t, ctx, provenAccount.userId, accountAction, settings.accountRateLimit(10), server.NowUtc().Add(-time.Minute))
		requireTheBudgetsWait(
			"a proven account",
			testActivateHost(t, ctx, service, provenAccount.newHost(t, ctx), testExtenderFleetIp(t, 30, 4)),
		)

		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("budget", "account", "ipv4")), float64(2))
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("budget", "unproven_account", "ipv4")), float64(0))
	})
}

// An account whose first host passed activation ten minutes ago, and whose
// floor has been spent since, the rest of it a minute ago.
func testRampingExtenderAccount(
	t testing.TB,
	ctx context.Context,
	service *extenderActivateService,
	settings *ExtenderActivateSettings,
	accountAction string,
) *testExtenderAccount {
	t.Helper()
	account := newTestExtenderAccount(ctx)
	if result := testActivateHost(t, ctx, service, account.newHost(t, ctx), testExtenderFleetIp(t, 0, 4)); !result.Activated {
		t.Fatalf("the first host did not activate: %q", result.Error)
	}
	shiftTestExtenderActivationsBack(t, ctx, 10*time.Minute)
	seedTestAccountActionAttempts(t, ctx, account.userId, accountAction, settings.AccountRateLimit-1, server.NowUtc().Add(-time.Minute))
	return account
}
