// The subscription-ended notice of a lapsed Play subscription is owed exactly
// when the finish that stops the renewal commits, and is delivered after it.
// The tests run the renewal poll through a task worker registered like
// taskworker's target, against the fake Play API, and fail the finish's commit
// at the exact boundary with a deferred constraint trigger on the task's
// finished row: a serialization failure makes the finish rerun its callback, a
// plain error rolls it back. The notice is in the account message outbox, so a
// process that stops right after the commit loses nothing: a later delivery run
// (any process) sends it once, and retries a send that fails.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The purchase token of the lapsed subscription.
const playEndedNoticePurchaseToken = "synthetic-ended-notice-token"

// Stands for the process stopping in the middle of a send.
var errPlayEndedNoticeCrash = errors.New("synthetic crash during the send")

// One lapsed Play subscription whose renewal poll is due, the worker that runs
// it, and the message sender that records each notice and whether the finish
// that owed it had committed. The finished row of the task is visible to a
// separate pooled connection only after that commit.
type playEndedNoticeFixture struct {
	ctx            context.Context
	taskId         server.Id
	networkId      server.Id
	recipient      string
	paidThrough    time.Time
	worker         *task.TaskWorker
	previousSender MessageSender
	sendCount      atomic.Int32
	openSendCount  atomic.Int32
	wrongSendCount atomic.Int32
	// fail the next sends
	failSendCount atomic.Int32
	// stop the process inside the next send, after it reached SES
	crashNextSend atomic.Bool
}

// A network whose admin signs in by email, its Play subscription on hold and
// paid through two grace periods ago, and the renewal poll due, scheduled the
// way the store paths schedule it. The fixture replaces the message sender
// until `close`.
func newPlayEndedNoticeFixture(t testing.TB, ctx context.Context) *playEndedNoticeFixture {
	env := newPlayWebhookTestEnv(t, map[string]*Sku{})
	clientSession := session.Testing_CreateClientSession(ctx, nil)
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-ended-notice", userId)
	const recipient = "ended-notice@synthetic.example"
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`UPDATE network_user SET user_auth = $1, auth_type = $2 WHERE user_id = $3`,
			recipient, model.AuthTypePassword, userId))
	})

	paidThrough := server.NowUtc().Truncate(time.Second).Add(-2 * SubscriptionGracePeriod)
	subscription := playTestSubscription(
		networkId,
		playTerminalTestSubscriptionId,
		paidThrough.Add(-30*24*time.Hour),
		paidThrough,
	)
	subscription.SubscriptionState = "SUBSCRIPTION_STATE_ON_HOLD"
	env.subscriptions[playEndedNoticePurchaseToken] = subscription

	args := playTerminalTestArgs(networkId, env.packageName, playEndedNoticePurchaseToken)
	args.CheckTime = server.NowUtc().Add(-time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		SchedulePlaySubscriptionRenewal(clientSession, tx, args)
	})
	var taskId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT task_id FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("play_subscription_renewal", playEndedNoticePurchaseToken).String(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&taskId))
			}
		})
	})
	if taskId == (server.Id{}) {
		t.Fatal("the renewal poll was not scheduled")
	}

	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(task.NewTaskTargetWithPost(
		PlaySubscriptionRenewal,
		PlaySubscriptionRenewalPost,
	))

	fixture := &playEndedNoticeFixture{
		ctx:            ctx,
		taskId:         taskId,
		networkId:      networkId,
		recipient:      recipient,
		paidThrough:    paidThrough,
		worker:         worker,
		previousSender: GetAWSMessageSender(),
	}
	SetMessageSender(fixture)
	return fixture
}

// Restores the message sender and stops the worker.
func (self *playEndedNoticeFixture) close() {
	SetMessageSender(self.previousSender)
	self.worker.Close()
}

// A `MessageSender` that records the notice, or fails it while failures are
// asked for.
func (self *playEndedNoticeFixture) SendAccountMessageTemplate(userAuth string, template Template, _ ...any) error {
	if self.crashNextSend.CompareAndSwap(true, false) {
		panic(errPlayEndedNoticeCrash)
	}
	if 0 < self.failSendCount.Load() {
		self.failSendCount.Add(-1)
		return errors.New("synthetic SES failure")
	}
	self.sendCount.Add(1)
	if _, ok := template.(*SubscriptionEndedTemplate); !ok || userAuth != self.recipient {
		self.wrongSendCount.Add(1)
	}
	committed := false
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`SELECT EXISTS (SELECT 1 FROM finished_task WHERE task_id = $1)`,
			self.taskId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&committed))
			}
		})
	})
	if !committed {
		self.openSendCount.Add(1)
	}
	return nil
}

// Fails the commit of each finish that writes the task's finished row: the
// first `transientCount` with a serialization failure, then, with `rollback`,
// every later one with a plain error.
func (self *playEndedNoticeFixture) failFinishCommits(transientCount int, rollback bool) {
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, fmt.Sprintf(`
			CREATE SEQUENCE synthetic_play_notice_commit;
			CREATE FUNCTION synthetic_play_notice_fault() RETURNS trigger AS $fault$
			BEGIN
				IF nextval('synthetic_play_notice_commit') <= %d THEN
					RAISE EXCEPTION 'synthetic serialization failure at commit' USING ERRCODE = '40001';
				END IF;
				IF %t THEN
					RAISE EXCEPTION 'synthetic failure at commit';
				END IF;
				RETURN NULL;
			END
			$fault$ LANGUAGE plpgsql;
			CREATE CONSTRAINT TRIGGER synthetic_play_notice_fault
				AFTER INSERT ON finished_task
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW WHEN (NEW.task_id = '%s')
				EXECUTE FUNCTION synthetic_play_notice_fault();
		`, transientCount, rollback, self.taskId)))
	})
}

// The finish commits the fault has failed or let through.
func (self *playEndedNoticeFixture) finishCommitCount() (commitCount int64) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM synthetic_play_notice_commit`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&commitCount))
			}
		})
	})
	return
}

// Claims and runs the due poll, and finishes it. A finish that rolls back
// panics with its commit failure, which is returned.
func (self *playEndedNoticeFixture) evalRenewal() (finishedTaskIds []server.Id, finishPanic any, returnErr error) {
	defer func() {
		finishPanic = recover()
	}()
	finishedTaskIds, _, _, returnErr = self.worker.EvalTasks(1)
	return
}

// The notice the outbox holds for the lapse, or nil.
func (self *playEndedNoticeFixture) owedNotice() *model.AccountMessage {
	return model.GetAccountMessageByKey(
		self.ctx,
		(&SubscriptionEndedTemplate{}).Name(),
		fmt.Sprintf("%s/%s/%d", self.networkId, playEndedNoticePurchaseToken, self.paidThrough.Unix()),
	)
}

// Runs a delivery at `now`, as the delivery task of any process would.
func (self *playEndedNoticeFixture) deliverAt(now time.Time) {
	deliverAccountMessagesAt(self.ctx, self, now)
}

// The finish that stops the renewal owes the notice and sends nothing itself;
// the process can stop right after the commit, and a later delivery sends the
// notice once, after the commit, to the admin.
func TestPlaySubscriptionEndedNoticeIsDeliveredOnceAfterTheFinishCommits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newPlayEndedNoticeFixture(t, ctx)
		defer fixture.close()

		finishedTaskIds, finishPanic, err := fixture.evalRenewal()
		if finishPanic != nil || err != nil || len(finishedTaskIds) != 1 || finishedTaskIds[0] != fixture.taskId {
			t.Fatalf("finish = %v err=%v panic=%v, want %s finished", finishedTaskIds, err, finishPanic, fixture.taskId)
		}
		// the worker is done with the task: the notice now lives only in the
		// committed outbox row, as it would if the process stopped here
		fixture.worker.Close()
		if sendCount := fixture.sendCount.Load(); sendCount != 0 {
			t.Fatalf("the finish sent %d notices itself, want none", sendCount)
		}
		notice := fixture.owedNotice()
		if notice == nil || notice.UserAuth != fixture.recipient || notice.SentTime != nil || notice.DeliverTime == nil {
			t.Fatalf("owed notice = %+v, want one due for %s", notice, fixture.recipient)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, playEndedNoticePurchaseToken); count != 0 {
			t.Fatalf("the stopped renewal scheduled %d more polls, want none", count)
		}

		fixture.deliverAt(server.NowUtc())
		fixture.deliverAt(server.NowUtc().Add(time.Hour))
		if sendCount := fixture.sendCount.Load(); sendCount != 1 {
			t.Fatalf("delivered %d notices, want one", sendCount)
		}
		if openSendCount := fixture.openSendCount.Load(); openSendCount != 0 {
			t.Fatalf("sent %d notices while the finish was open", openSendCount)
		}
		if wrongSendCount := fixture.wrongSendCount.Load(); wrongSendCount != 0 {
			t.Fatalf("sent %d notices with the wrong recipient or template", wrongSendCount)
		}
		if notice := fixture.owedNotice(); notice == nil || notice.SentTime == nil {
			t.Fatalf("delivered notice = %+v, want sent", notice)
		}
	})
}

// A finish that rolls back owes no notice, and nothing is delivered; the poll
// stays pending, so a later finish can still owe it.
func TestPlaySubscriptionEndedNoticeIsNotOwedWhenTheFinishRollsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newPlayEndedNoticeFixture(t, ctx)
		defer fixture.close()
		fixture.failFinishCommits(0, true)

		_, finishPanic, _ := fixture.evalRenewal()
		if err, ok := finishPanic.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("finish panic = %v, want the commit failure", finishPanic)
		}
		if commitCount := fixture.finishCommitCount(); commitCount != 1 {
			t.Fatalf("the finish tried %d commits, want one", commitCount)
		}
		if notice := fixture.owedNotice(); notice != nil {
			t.Fatalf("a rolled-back finish owes notice %+v", notice)
		}
		fixture.deliverAt(server.NowUtc())
		if sendCount := fixture.sendCount.Load(); sendCount != 0 {
			t.Fatalf("sent %d notices for a finish that rolled back", sendCount)
		}
		if _, ok := task.GetTasks(ctx, fixture.taskId)[fixture.taskId]; !ok {
			t.Fatal("the rolled-back finish removed the pending poll")
		}
		if _, ok := task.GetFinishedTasks(ctx, fixture.taskId)[fixture.taskId]; ok {
			t.Fatal("the rolled-back finish left a finished poll")
		}
	})
}

// A finish whose callback reruns after a serialization failure at commit runs
// the post twice and owes one notice, delivered once after the commit.
func TestPlaySubscriptionEndedNoticeIsDeliveredOnceWhenTheFinishReruns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newPlayEndedNoticeFixture(t, ctx)
		defer fixture.close()
		fixture.failFinishCommits(1, false)

		finishedTaskIds, finishPanic, err := fixture.evalRenewal()
		if finishPanic != nil || err != nil || len(finishedTaskIds) != 1 || finishedTaskIds[0] != fixture.taskId {
			t.Fatalf("finish = %v err=%v panic=%v, want %s finished", finishedTaskIds, err, finishPanic, fixture.taskId)
		}
		if commitCount := fixture.finishCommitCount(); commitCount != 2 {
			t.Fatalf("the finish tried %d commits, want 2 (it did not rerun)", commitCount)
		}
		if count := outboxMessageCount(t, ctx, fixture.networkId); count != 1 {
			t.Fatalf("the rerun finish owes %d notices, want one", count)
		}
		fixture.deliverAt(server.NowUtc())
		if sendCount := fixture.sendCount.Load(); sendCount != 1 {
			t.Fatalf("delivered %d notices, want one", sendCount)
		}
		if openSendCount := fixture.openSendCount.Load(); openSendCount != 0 {
			t.Fatalf("sent %d notices while the finish was open", openSendCount)
		}
	})
}

// A notice whose send fails (an SES error or timeout) stays owed and is
// delivered by a later run, once.
func TestPlaySubscriptionEndedNoticeIsRetriedAfterAFailedSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newPlayEndedNoticeFixture(t, ctx)
		defer fixture.close()

		if _, finishPanic, err := fixture.evalRenewal(); finishPanic != nil || err != nil {
			t.Fatalf("finish err=%v panic=%v", err, finishPanic)
		}
		fixture.failSendCount.Store(1)
		firstTime := server.NowUtc()
		fixture.deliverAt(firstTime)
		if sendCount := fixture.sendCount.Load(); sendCount != 0 {
			t.Fatalf("delivered %d notices through a failing send", sendCount)
		}
		notice := fixture.owedNotice()
		if notice == nil || notice.SentTime != nil || notice.AttemptCount != 1 || !strings.Contains(notice.LastError, "synthetic SES failure") {
			t.Fatalf("notice after the failed send = %+v, want still owed with the error", notice)
		}

		fixture.deliverAt(firstTime.Add(accountMessageRetryBaseDelay))
		fixture.deliverAt(firstTime.Add(time.Hour))
		if sendCount := fixture.sendCount.Load(); sendCount != 1 {
			t.Fatalf("delivered %d notices after the retry, want one", sendCount)
		}
		if wrongSendCount := fixture.wrongSendCount.Load(); wrongSendCount != 0 {
			t.Fatalf("sent %d notices with the wrong recipient or template", wrongSendCount)
		}
	})
}

// A process that stops in the middle of the send leaves the notice claimed but
// not delivered; once the claim's lease ends, a delivery in any process sends
// it, once.
func TestPlaySubscriptionEndedNoticeSurvivesACrashDuringTheSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newPlayEndedNoticeFixture(t, ctx)
		defer fixture.close()

		if _, finishPanic, err := fixture.evalRenewal(); finishPanic != nil || err != nil {
			t.Fatalf("finish err=%v panic=%v", err, finishPanic)
		}
		fixture.crashNextSend.Store(true)
		crashTime := server.NowUtc()
		func() {
			defer func() {
				if value := recover(); value != errPlayEndedNoticeCrash {
					panic(value)
				}
			}()
			fixture.deliverAt(crashTime)
		}()
		notice := fixture.owedNotice()
		if notice == nil || notice.SentTime != nil || notice.ClaimId == nil {
			t.Fatalf("notice after the crash = %+v, want claimed and not delivered", notice)
		}

		// another process, while the dead claim lasts, does not send it twice
		fixture.deliverAt(crashTime.Add(accountMessageClaimLease - time.Second))
		if sendCount := fixture.sendCount.Load(); sendCount != 0 {
			t.Fatalf("delivered %d notices while the dead claim lasted", sendCount)
		}
		fixture.deliverAt(crashTime.Add(accountMessageClaimLease))
		fixture.deliverAt(crashTime.Add(2 * accountMessageClaimLease))
		if sendCount := fixture.sendCount.Load(); sendCount != 1 {
			t.Fatalf("delivered %d notices after the crash, want one", sendCount)
		}
		if notice := fixture.owedNotice(); notice == nil || notice.SentTime == nil || notice.AttemptCount != 2 {
			t.Fatalf("notice = %+v, want delivered on the second attempt", notice)
		}
	})
}
