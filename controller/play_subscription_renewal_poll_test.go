// The scheduled Play renewal poll after another owner credited the purchase,
// and after a real lapse. The store's notification credits a renewal the
// moment Play charges it and asks for a poll at the new expiry; that request
// merges into the purchase's pending poll (task.RunOnce keeps the earlier run
// time), so the poll that runs next finds the expiry already credited and
// reports it not renewed. The tests run the poll through a task worker
// registered like taskworker's target, against the fake Play API
// (play_webhook_test.go), with the notification sent through the webhook.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The Play products of these tests: UR Pro, and a data plan, a product that
// credits data only and never Pro.
const (
	playPollTestProProductId  = "synthetic_pro"
	playPollTestDataProductId = "synthetic_data_plan"
)

// The fake Play API, selling both products.
func newPlayPollTestEnv(t testing.TB) *playWebhookTestEnv {
	return newPlayWebhookTestEnv(t, map[string]*Sku{
		playPollTestProProductId: {
			FeeFraction:    0.3,
			PriceAmountUsd: 5.0,
			Supporter:      true,
		},
		playPollTestDataProductId: {
			FeeFraction:                   0.3,
			BalanceByteCountHumanReadable: "10GiB",
		},
	})
}

// A network whose admin signs in by email, so a post that owes the
// subscription-ended notice adds it to the outbox.
func newPlayPollTestNetwork(ctx context.Context, name string) server.Id {
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-"+name, userId)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`UPDATE network_user SET user_auth = $1, auth_type = $2 WHERE user_id = $3`,
			name+"@synthetic.example", model.AuthTypePassword, userId))
	})
	return networkId
}

// A worker that claims only the Play renewal poll, registered like
// taskworker's target.
func newPlayPollTestWorker(ctx context.Context) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(task.NewTaskTargetWithPost(
		PlaySubscriptionRenewal,
		PlaySubscriptionRenewalPost,
	))
	return worker
}

// The purchase's poll, due a minute ago.
func playPollTestArgs(
	networkId server.Id,
	packageName string,
	productId string,
	purchaseToken string,
) *PlaySubscriptionRenewalArgs {
	return &PlaySubscriptionRenewalArgs{
		NetworkId:      networkId,
		PackageName:    packageName,
		SubscriptionId: productId,
		PurchaseToken:  purchaseToken,
		CheckTime:      server.NowUtc().Add(-time.Minute),
	}
}

// Claims, runs and finishes the purchase's due poll, and returns its result.
func evalPlayPollTestRenewal(
	t testing.TB,
	ctx context.Context,
	worker *task.TaskWorker,
	purchaseToken string,
) *PlaySubscriptionRenewalResult {
	t.Helper()
	finishedTaskIds, _, _, err := worker.EvalTasks(1)
	if err != nil || len(finishedTaskIds) != 1 {
		t.Fatalf("finished %v err=%v, want the due poll", finishedTaskIds, err)
	}
	finishedTask, ok := task.GetFinishedTasks(ctx, finishedTaskIds[0])[finishedTaskIds[0]]
	if !ok || finishedTask.RunOnceKey != task.RunOnce("play_subscription_renewal", purchaseToken).String() {
		t.Fatalf("finished task = %+v, want the poll of %s", finishedTask, purchaseToken)
	}
	if finishedTask.PostError != "" {
		t.Fatalf("the poll's post failed: %s", finishedTask.PostError)
	}
	result := &PlaySubscriptionRenewalResult{}
	if err := json.Unmarshal([]byte(finishedTask.ResultJson), result); err != nil {
		t.Fatalf("poll result %q: %v", finishedTask.ResultJson, err)
	}
	return result
}

// The notification credits the renewal Play just charged, and its request for
// a poll at the new expiry merges into the purchase's poll, which is due. That
// poll finds the expiry credited: not renewed by it, paid through a month from
// now. Nothing ended, so the post owes no notice and polls again at the
// expiry. Before the fix the post owed the subscription-ended notice and
// scheduled nothing, so a paid purchase was told it ended and lost its polls.
func TestPlaySubscriptionRenewalPollAfterTheNotificationCreditPollsAtTheExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		worker := newPlayPollTestWorker(ctx)
		defer worker.Close()

		for _, productId := range []string{playPollTestProProductId, playPollTestDataProductId} {
			networkId := newPlayPollTestNetwork(ctx, "credited-"+productId)
			purchaseToken := "synthetic-credited-" + productId
			now := server.NowUtc().Truncate(time.Second)
			expiryTime := now.Add(30 * 24 * time.Hour)
			env.subscriptions[purchaseToken] = playTestSubscription(
				networkId,
				productId,
				now.Add(-60*24*time.Hour),
				expiryTime,
			)

			// the poll an earlier post scheduled, now due
			args := playPollTestArgs(networkId, env.packageName, productId, purchaseToken)
			server.Tx(ctx, func(tx server.PgTx) {
				SchedulePlaySubscriptionRenewal(clientSession, tx, args)
			})

			// the notification credits the renewal and asks for a poll at its
			// expiry, which merges into the due poll
			if _, err := PlayWebhook(env.webhookArgs(t, productId, purchaseToken), clientSession); err != nil {
				t.Fatalf("%s notification: %v", productId, err)
			}
			if count := countActivePlayBalancesThrough(t, ctx, networkId, purchaseToken, expiryTime); count != 1 {
				t.Fatalf("%s: %d balances cover the expiry after the notification, want its credit", productId, count)
			}
			if count, runAt := countScheduledPlayRenewals(t, ctx, purchaseToken); count != 1 || !runAt.Before(now) {
				t.Fatalf("%s: %d polls pending at %s, want the one due poll", productId, count, runAt)
			}

			result := evalPlayPollTestRenewal(t, ctx, worker, purchaseToken)
			if result.Renewed || result.Canceled || result.Terminal || result.InGrace || !result.ExpiryTime.Equal(expiryTime) {
				t.Fatalf("%s poll result = %+v, want not renewed by the poll, paid through %s", productId, result, expiryTime)
			}
			if count := outboxMessageCount(t, ctx, networkId); count != 0 {
				t.Errorf("%s: a purchase paid through %s owes %d subscription-ended notices, want none", productId, expiryTime, count)
			}
			if count, runAt := countScheduledPlayRenewals(t, ctx, purchaseToken); count != 1 || !runAt.Equal(expiryTime) {
				t.Errorf("%s: %d polls scheduled, the first at %s, want one at the expiry %s", productId, count, runAt, expiryTime)
			}
			if count := countActivePlayBalancesThrough(t, ctx, networkId, purchaseToken, expiryTime); count != 1 {
				t.Errorf("%s: %d balances cover the expiry after the poll, want the notification's one credit", productId, count)
			}
		}
	})
}

// A purchase Play stopped renewing (on hold) is not told it ended while its
// grace period lasts; the poll checks again later. Once the grace period has
// passed with nothing renewed, the poll owes exactly one subscription-ended
// notice and schedules nothing, and another poll of the same lapse owes no
// second one.
func TestPlaySubscriptionRenewalPollOfALapseOwesOneNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		worker := newPlayPollTestWorker(ctx)
		defer worker.Close()

		networkId := newPlayPollTestNetwork(ctx, "lapse")
		purchaseToken := "synthetic-lapse"
		now := server.NowUtc().Truncate(time.Second)
		subscription := playTestSubscription(
			networkId,
			playPollTestProProductId,
			now.Add(-60*24*time.Hour),
			now.Add(-time.Hour),
		)
		subscription.SubscriptionState = "SUBSCRIPTION_STATE_ON_HOLD"
		env.subscriptions[purchaseToken] = subscription
		args := playPollTestArgs(networkId, env.packageName, playPollTestProProductId, purchaseToken)

		// an hour past the expiry, inside the grace period
		server.Tx(ctx, func(tx server.PgTx) {
			SchedulePlaySubscriptionRenewal(clientSession, tx, args)
		})
		result := evalPlayPollTestRenewal(t, ctx, worker, purchaseToken)
		afterPoll := server.NowUtc()
		if result.Renewed || result.Canceled || result.Terminal || !result.ExpiryTime.Equal(now.Add(-time.Hour)) {
			t.Fatalf("poll result inside the grace period = %+v", result)
		}
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("a lapse inside its grace period owes %d notices, want none", count)
		}
		if count, runAt := countScheduledPlayRenewals(t, ctx, purchaseToken); count != 1 || !runAt.After(afterPoll) {
			t.Fatalf("%d polls scheduled, the first at %s, want one later than %s", count, runAt, afterPoll)
		}

		// the grace period has passed and Play still reports the lapse; the
		// due request merges into the pending poll
		lapsedExpiryTime := now.Add(-2 * SubscriptionGracePeriod)
		subscription.LineItems[0].ExpiryTime = lapsedExpiryTime.Format(time.RFC3339)
		for range 2 {
			args = playPollTestArgs(networkId, env.packageName, playPollTestProProductId, purchaseToken)
			server.Tx(ctx, func(tx server.PgTx) {
				SchedulePlaySubscriptionRenewal(clientSession, tx, args)
			})
			result = evalPlayPollTestRenewal(t, ctx, worker, purchaseToken)
			if result.Renewed || result.Canceled || result.Terminal || !result.ExpiryTime.Equal(lapsedExpiryTime) {
				t.Fatalf("poll result after the grace period = %+v", result)
			}
			if count := outboxMessageCount(t, ctx, networkId); count != 1 {
				t.Fatalf("the lapse owes %d notices, want one", count)
			}
			if count, _ := countScheduledPlayRenewals(t, ctx, purchaseToken); count != 0 {
				t.Fatalf("the ended purchase has %d polls scheduled, want none", count)
			}
		}
		key := fmt.Sprintf("%s/%s/%d", networkId, purchaseToken, lapsedExpiryTime.Unix())
		if notice := outboxTestMessage(t, ctx, key); notice == nil || notice.UserAuth != "lapse@synthetic.example" {
			t.Fatalf("owed notice = %+v, want the lapse's to the admin", notice)
		}
	})
}

// The subscription-ended notice of a data plan, a Play product that credits
// data only, never Pro, does not say UR Pro; the notice of a UR Pro
// subscription still does. Before the fix both said UR Pro.
func TestPlaySubscriptionEndedNoticeOfADataPlanDoesNotSayPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		worker := newPlayPollTestWorker(ctx)
		defer worker.Close()

		cases := []struct {
			productId string
			pro       bool
		}{
			{productId: playPollTestProProductId, pro: true},
			{productId: playPollTestDataProductId, pro: false},
		}
		for _, c := range cases {
			networkId := newPlayPollTestNetwork(ctx, "ended-"+c.productId)
			purchaseToken := "synthetic-ended-" + c.productId
			now := server.NowUtc().Truncate(time.Second)
			lapsedExpiryTime := now.Add(-2 * SubscriptionGracePeriod)
			subscription := playTestSubscription(
				networkId,
				c.productId,
				now.Add(-60*24*time.Hour),
				lapsedExpiryTime,
			)
			subscription.SubscriptionState = "SUBSCRIPTION_STATE_ON_HOLD"
			env.subscriptions[purchaseToken] = subscription
			args := playPollTestArgs(networkId, env.packageName, c.productId, purchaseToken)
			server.Tx(ctx, func(tx server.PgTx) {
				SchedulePlaySubscriptionRenewal(clientSession, tx, args)
			})
			evalPlayPollTestRenewal(t, ctx, worker, purchaseToken)

			// the owed notice, decoded and rendered the way the delivery does
			notice := outboxTestMessage(t, ctx, fmt.Sprintf("%s/%s/%d", networkId, purchaseToken, lapsedExpiryTime.Unix()))
			if notice == nil {
				t.Fatalf("%s: the lapse owes no notice", c.productId)
			}
			template, err := decodeAccountMessageTemplate(notice.TemplateName, notice.TemplateJson)
			if err != nil {
				t.Fatalf("%s: %v", c.productId, err)
			}
			subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
			if err != nil {
				t.Fatalf("%s: render: %v", c.productId, err)
			}
			sms, err := RenderSmsTemplate(template)
			if err != nil {
				t.Fatalf("%s: sms: %v", c.productId, err)
			}
			for part, text := range map[string]string{
				"subject": subject,
				"html":    bodyHtml,
				"text":    bodyText,
				"sms":     sms,
			} {
				// case matters: every email's footer names the UR protocol
				saysPro := strings.Contains(text, "UR Pro") || strings.Contains(text, "UR PRO")
				if saysPro != c.pro {
					t.Errorf("%s: the %s part says UR Pro: %t, want %t", c.productId, part, saysPro, c.pro)
				}
			}
		}
	})
}

// The purchase's balances whose window covers expiryTime: the credit gate's
// test (model.GetOverlappingTransferBalance), without its grace hold exception.
func countActivePlayBalancesThrough(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
	expiryTime time.Time,
) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT count(*)
			FROM transfer_balance
			WHERE network_id = $1
			  AND purchase_token = $2
			  AND start_time <= $3
			  AND $3 < end_time
			`,
			networkId,
			purchaseToken,
			expiryTime,
		).Scan(&count))
	})
	return
}
