package controller

// Database-backed tests of the hourly dunning sweep
// (subscription_stripe_dunning_sweep.go), with the fake Stripe API of
// subscription_stripe_payment_failed_db_test.go and an injected clock
// (stripeDunningNow). The sweep runs through its real task target (claim,
// result, post): a run owes each notice a retrying subscription is due once, a
// re-run owes nothing more, a run beside a hand send owes one notice, and each
// run schedules the next one interval out. Needs the test database and redis
// (server.DefaultTestEnv).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Stripe credentials in the vault, so the sweep reads the fake Stripe API
// instead of skipping the env.
func setStripeDunningSweepTestCredentials(t testing.TB) {
	t.Cleanup(server.Vault.PushSimpleResource("stripe.yml", []byte("api:\n  token: sk_test_synthetic_sweep\n")))
}

// The payment-failed template of a notice the outbox sent.
func stripeDunningSweepTestTemplate(t testing.TB, send outboxTestSend) *SubscriptionPaymentFailedTemplate {
	template, ok := send.template.(*SubscriptionPaymentFailedTemplate)
	if !ok {
		t.Fatalf("sent %T, want a payment-failed notice", send.template)
	}
	return template
}

// The first run owes the first notice of a declined renewal and of a renewal
// that waits on the customer's bank at attempt 0 (3D Secure), and leaves the
// next run one interval out. A second run owes nothing more. Nine days on, when
// Stripe's next attempt on the declined renewal is its last, a run owes its
// final notice, and the run after owes nothing.
func TestStripeDunningSweepOwesEachNoticeOnceAndReschedules(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		stripeEnv := newStripePaymentFailedTestEnv(t)
		setStripeDunningSweepTestCredentials(t)
		const declinedEmail = "dunning-sweep-declined@synthetic.example"
		const confirmEmail = "dunning-sweep-confirm@synthetic.example"
		declinedNetworkId, _ := newStripePaymentFailedTestNetwork(t, ctx, "dunningsweepdeclined", declinedEmail)
		confirmNetworkId, _ := newStripePaymentFailedTestNetwork(t, ctx, "dunningsweepconfirm", confirmEmail)

		day := 24 * time.Hour
		now := server.NowUtc().Truncate(time.Second)
		setStripeDunningTestClock(t, now)
		created := now.Add(-time.Hour)
		next := created.Add(2 * day)
		stripeEnv.setSubscription("sub_synthetic_sweep_declined", "cus_synthetic_sweep_declined", &declinedNetworkId, "past_due")
		stripeEnv.setInvoice("in_synthetic_sweep_declined", "sub_synthetic_sweep_declined", "cus_synthetic_sweep_declined", "open", 1, created, &next, 4000)
		stripeEnv.setLatestInvoice("sub_synthetic_sweep_declined", "in_synthetic_sweep_declined")
		stripeEnv.setSubscription("sub_synthetic_sweep_confirm", "cus_synthetic_sweep_confirm", &confirmNetworkId, "past_due")
		stripeEnv.setInvoice("in_synthetic_sweep_confirm", "sub_synthetic_sweep_confirm", "cus_synthetic_sweep_confirm", "open", 0, created, &next, 4000)
		stripeEnv.setInvoicePaymentIntentStatus("in_synthetic_sweep_confirm", stripePaymentIntentStatusRequiresAction)
		stripeEnv.setLatestInvoice("sub_synthetic_sweep_confirm", "in_synthetic_sweep_confirm")

		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		sweep := func() *SweepStripeDunningResult {
			return runControllerAuditTask(t, owner, SweepStripeDunning, SweepStripeDunningPost, &SweepStripeDunningArgs{})
		}
		sender := newOutboxTestSender()
		settings := defaultStripeDunningSettings()

		beforeRun := server.NowUtc()
		result := sweep()
		afterRun := server.NowUtc()
		if result.Skip != "" || result.SubscriptionCount != 2 || result.FirstNoticeCount != 2 || result.FinalNoticeCount != 0 ||
			result.NoEmailCount != 0 || result.ErrorCount != 0 {
			t.Fatalf("first run: %+v", result)
		}
		var runAt time.Time
		var maxTimeSeconds int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT run_at, run_max_time_seconds FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce(stripeDunningSweepTaskKey).String(),
			).Scan(&runAt, &maxTimeSeconds))
		})
		if runAt.Before(beforeRun.Add(settings.SweepInterval)) || afterRun.Add(settings.SweepInterval).Before(runAt) {
			t.Fatalf("next run at %s, want one interval (%s) after the run (%s to %s)", runAt, settings.SweepInterval, beforeRun, afterRun)
		}
		if time.Duration(maxTimeSeconds)*time.Second != settings.SweepMaxTime {
			t.Fatalf("next run's max time %ds, want %s", maxTimeSeconds, settings.SweepMaxTime)
		}

		declined := deliverStripePaymentFailedTestNotices(ctx, sender, declinedEmail)
		confirm := deliverStripePaymentFailedTestNotices(ctx, sender, confirmEmail)
		if len(declined) != 1 || len(confirm) != 1 {
			t.Fatalf("the first run must email each account once: declined %d, confirm %d", len(declined), len(confirm))
		}
		if template := stripeDunningSweepTestTemplate(t, declined[0]); template.Final || template.ActionRequired {
			t.Fatalf("the declined renewal's first notice: %+v", template)
		}
		if template := stripeDunningSweepTestTemplate(t, confirm[0]); template.Final || !template.ActionRequired {
			t.Fatalf("the renewal that waits on the bank must ask for the confirmation: %+v", template)
		}

		result = sweep()
		if result.SubscriptionCount != 2 || result.FirstNoticeCount != 0 || result.FinalNoticeCount != 0 || result.ErrorCount != 0 {
			t.Fatalf("a re-run owed notices again: %+v", result)
		}
		declined = deliverStripePaymentFailedTestNotices(ctx, sender, declinedEmail)
		confirm = deliverStripePaymentFailedTestNotices(ctx, sender, confirmEmail)
		if len(declined) != 1 || len(confirm) != 1 {
			t.Fatalf("a re-run emailed again: declined %d, confirm %d", len(declined), len(confirm))
		}

		// Stripe's next attempt on the declined renewal is its last
		finalNext := created.Add(14 * day)
		stripeEnv.setInvoice("in_synthetic_sweep_declined", "sub_synthetic_sweep_declined", "cus_synthetic_sweep_declined", "open", 8, created, &finalNext, 4000)
		setStripeDunningTestClock(t, now.Add(9*day))
		result = sweep()
		if result.SubscriptionCount != 2 || result.FirstNoticeCount != 0 || result.FinalNoticeCount != 1 || result.ErrorCount != 0 {
			t.Fatalf("the run at the last attempt: %+v", result)
		}
		declined = deliverStripePaymentFailedTestNotices(ctx, sender, declinedEmail)
		if len(declined) != 2 || !stripeDunningSweepTestTemplate(t, declined[1]).Final {
			t.Fatalf("the last attempt must owe the final notice once: %d notices", len(declined))
		}

		result = sweep()
		if result.FirstNoticeCount != 0 || result.FinalNoticeCount != 0 || result.ErrorCount != 0 {
			t.Fatalf("a re-run after the final notice owed notices: %+v", result)
		}
		declined = deliverStripePaymentFailedTestNotices(ctx, sender, declinedEmail)
		confirm = deliverStripePaymentFailedTestNotices(ctx, sender, confirmEmail)
		if len(declined) != 2 || len(confirm) != 1 {
			t.Fatalf("a re-run after the final notice emailed again: declined %d, confirm %d", len(declined), len(confirm))
		}
		failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_sweep_declined")
		if failure == nil || failure.FirstNoticeTime == nil || !failure.FirstNoticeTime.Equal(now) ||
			failure.FinalNoticeTime == nil || !failure.FinalNoticeTime.Equal(now.Add(9*day)) {
			t.Fatalf("the ledger must hold each notice once, at the run that owed it: %+v", failure)
		}
	})
}

// The sweep and a hand send (`bringyourctl payments stripe-dunning --send`, as
// on 2026-11-13) running at once decide one after the other on the invoice's
// ledger row: one notice is owed and one email is sent.
func TestStripeDunningSweepAndHandSendOweOneNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stripeEnv := newStripePaymentFailedTestEnv(t)
		setStripeDunningSweepTestCredentials(t)
		const email = "dunning-sweep-concurrent@synthetic.example"
		networkId, _ := newStripePaymentFailedTestNetwork(t, ctx, "dunningsweepconcurrent", email)

		now := server.NowUtc().Truncate(time.Second)
		setStripeDunningTestClock(t, now)
		created := now.Add(-time.Hour)
		next := created.Add(2 * 24 * time.Hour)
		stripeEnv.setSubscription("sub_synthetic_sweep_concurrent", "cus_synthetic_sweep_concurrent", &networkId, "past_due")
		stripeEnv.setInvoice("in_synthetic_sweep_concurrent", "sub_synthetic_sweep_concurrent", "cus_synthetic_sweep_concurrent", "open", 1, created, &next, 4000)
		stripeEnv.setLatestInvoice("sub_synthetic_sweep_concurrent", "in_synthetic_sweep_concurrent")

		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		const runCount = 8
		var wait sync.WaitGroup
		errs := make(chan error, runCount)
		for i := range runCount {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if i%2 == 0 {
					_, err := SweepStripeDunning(&SweepStripeDunningArgs{}, owner)
					errs <- err
				} else {
					_, err := StripeDunningReport(ctx, true)
					errs <- err
				}
			}()
		}
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}

		sender := newOutboxTestSender()
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 1 {
			t.Fatalf("%d concurrent sweeps and hand sends sent %d notices, want 1", runCount, len(notices))
		}
		failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_sweep_concurrent")
		if failure == nil || failure.FirstNoticeTime == nil || failure.FinalNoticeTime != nil {
			t.Fatalf("the ledger must hold the first notice once: %+v", failure)
		}
	})
}
