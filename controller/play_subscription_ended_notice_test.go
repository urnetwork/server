// The subscription-ended notice of a lapsed Play subscription goes out only
// after the finish that stops the renewal has committed. The tests run the
// renewal poll through a task worker registered like taskworker's target,
// against the fake Play API, and fail the finish's commit at the exact boundary
// with a deferred constraint trigger on the task's finished row: a
// serialization failure makes the finish rerun its callback, a plain error
// rolls it back.
package controller

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One lapsed Play subscription whose renewal poll is due, the worker that runs
// it, and the message sender that records each notice and whether the finish
// that sent it had committed. The finished row of the task is visible to a
// separate pooled connection only after that commit.
type playEndedNoticeFixture struct {
	ctx            context.Context
	taskId         server.Id
	recipient      string
	worker         *task.TaskWorker
	previousSender MessageSender
	sendCount      atomic.Int32
	openSendCount  atomic.Int32
	wrongSendCount atomic.Int32
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

	const purchaseToken = "synthetic-ended-notice-token"
	paidThrough := server.NowUtc().Truncate(time.Second).Add(-2 * SubscriptionGracePeriod)
	subscription := playTestSubscription(
		networkId,
		playTerminalTestSubscriptionId,
		paidThrough.Add(-30*24*time.Hour),
		paidThrough,
	)
	subscription.SubscriptionState = "SUBSCRIPTION_STATE_ON_HOLD"
	env.subscriptions[purchaseToken] = subscription

	args := playTerminalTestArgs(networkId, env.packageName, purchaseToken)
	args.CheckTime = server.NowUtc().Add(-time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		SchedulePlaySubscriptionRenewal(clientSession, tx, args)
	})
	var taskId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT task_id FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("play_subscription_renewal", purchaseToken).String(),
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
	worker.AddTargets(task.NewTaskTargetWithCommitPost(
		PlaySubscriptionRenewal,
		PlaySubscriptionRenewalPost,
	))

	fixture := &playEndedNoticeFixture{
		ctx:            ctx,
		taskId:         taskId,
		recipient:      recipient,
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

// A `MessageSender` that records the notice.
func (self *playEndedNoticeFixture) SendAccountMessageTemplate(userAuth string, template Template, _ ...any) error {
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

// The notice of a lapsed subscription goes out exactly once, after the finish
// that stops the renewal has committed.
func TestPlaySubscriptionEndedNoticeIsSentOnceAfterTheFinishCommits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newPlayEndedNoticeFixture(t, ctx)
		defer fixture.close()

		finishedTaskIds, finishPanic, err := fixture.evalRenewal()
		if finishPanic != nil || err != nil || len(finishedTaskIds) != 1 || finishedTaskIds[0] != fixture.taskId {
			t.Fatalf("finish = %v err=%v panic=%v, want %s finished", finishedTaskIds, err, finishPanic, fixture.taskId)
		}
		if sendCount := fixture.sendCount.Load(); sendCount != 1 {
			t.Fatalf("sent %d notices, want one", sendCount)
		}
		if openSendCount := fixture.openSendCount.Load(); openSendCount != 0 {
			t.Fatalf("sent %d notices while the finish was open", openSendCount)
		}
		if wrongSendCount := fixture.wrongSendCount.Load(); wrongSendCount != 0 {
			t.Fatalf("sent %d notices with the wrong recipient or template", wrongSendCount)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, "synthetic-ended-notice-token"); count != 0 {
			t.Fatalf("the stopped renewal scheduled %d more polls, want none", count)
		}
	})
}

// A finish that rolls back sends nothing; the poll stays pending, so a later
// finish can still send the notice.
func TestPlaySubscriptionEndedNoticeIsNotSentWhenTheFinishRollsBack(t *testing.T) {
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
// the post twice and sends the notice once, after the commit.
func TestPlaySubscriptionEndedNoticeIsSentOnceWhenTheFinishReruns(t *testing.T) {
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
		if sendCount := fixture.sendCount.Load(); sendCount != 1 {
			t.Fatalf("sent %d notices, want one", sendCount)
		}
		if openSendCount := fixture.openSendCount.Load(); openSendCount != 0 {
			t.Fatalf("sent %d notices while the finish was open", openSendCount)
		}
	})
}
