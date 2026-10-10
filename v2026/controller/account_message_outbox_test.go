// The account message outbox: a message exists exactly when the transaction
// that adds it commits, and the delivery task sends each message once, retries
// a failed send, and never sends a message twice while its claim lasts. The
// delivery runs with an explicit clock so retries and leases are reached
// without waiting.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// One send the outbox test sender saw.
type outboxTestSend struct {
	userAuth string
	template Template
}

// A message sender that records each send. It can fail the next sends, and can
// hold each send until the test releases it. Safe for concurrent use.
type outboxTestSender struct {
	stateLock sync.Mutex
	sends     []outboxTestSend
	// failed sends are not recorded in sends
	failedSendCount int
	failNextCount   int
	failAll         bool
	// when set, each send signals entered and waits for release
	entered chan struct{}
	release chan struct{}
}

// A sender that sends at once.
func newOutboxTestSender() *outboxTestSender {
	return &outboxTestSender{}
}

// A sender whose sends wait for `releaseSend`.
func newHoldingOutboxTestSender() *outboxTestSender {
	return &outboxTestSender{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

// A `MessageSender`.
func (self *outboxTestSender) SendAccountMessageTemplate(userAuth string, template Template, _ ...any) error {
	if self.entered != nil {
		self.entered <- struct{}{}
		<-self.release
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failAll || 0 < self.failNextCount {
		if 0 < self.failNextCount {
			self.failNextCount -= 1
		}
		self.failedSendCount += 1
		return errors.New("synthetic send failure")
	}
	self.sends = append(self.sends, outboxTestSend{
		userAuth: userAuth,
		template: template,
	})
	return nil
}

// Fails the next `count` sends.
func (self *outboxTestSender) failNext(count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failNextCount = count
}

// Fails every send.
func (self *outboxTestSender) failEvery() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failAll = true
}

// The successful sends so far.
func (self *outboxTestSender) sent() []outboxTestSend {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]outboxTestSend{}, self.sends...)
}

// The failed sends so far.
func (self *outboxTestSender) failed() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.failedSendCount
}

// Lets the held sends finish.
func (self *outboxTestSender) releaseSend() {
	close(self.release)
}

// Runs one delivery at `now` with the production settings.
func deliverAccountMessagesAt(ctx context.Context, sender MessageSender, now time.Time) *accountMessageDelivery {
	return deliverAccountMessagesWithSettings(ctx, sender, now, defaultAccountMessageDeliverySettings())
}

// Runs one delivery at `now` with the given settings.
func deliverAccountMessagesWithSettings(
	ctx context.Context,
	sender MessageSender,
	now time.Time,
	settings *accountMessageDeliverySettings,
) *accountMessageDelivery {
	delivery := newAccountMessageDelivery(ctx, sender, func() time.Time { return now }, settings)
	delivery.Run()
	return delivery
}

// Adds a subscription-ended notice for a synthetic recipient in its own
// committed transaction.
func addOutboxTestMessage(ctx context.Context, key string, userAuth string) (added bool) {
	networkId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		added = addAccountMessageInTx(ctx, tx, &accountMessage{
			key:       key,
			networkId: &networkId,
			userAuth:  userAuth,
			template:  &SubscriptionEndedTemplate{},
		})
	})
	return
}

// The stored message of a subscription-ended notice key.
func outboxTestMessage(t testing.TB, ctx context.Context, key string) *model.AccountMessage {
	return model.GetAccountMessageByKey(ctx, (&SubscriptionEndedTemplate{}).Name(), key)
}

// The number of messages the outbox holds for the network.
func outboxMessageCount(t testing.TB, ctx context.Context, networkId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`,
			networkId,
		).Scan(&count))
	})
	return
}

// A message added in a committed transaction is sent once, after the commit,
// by a later delivery run; a second run sends nothing.
func TestAccountMessageOutboxDeliversACommittedMessageOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const recipient = "outbox-once@synthetic.example"
		if !addOutboxTestMessage(ctx, "synthetic-once", recipient) {
			t.Fatal("the message was not added")
		}
		before := testutil.ToFloat64(accountMessageDeliveryCounter.WithLabelValues("subscription_ended", "sent"))

		sender := newOutboxTestSender()
		delivery := deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sends := sender.sent()
		if len(sends) != 1 || delivery.sentCount != 1 {
			t.Fatalf("sent %d messages (counted %d), want 1", len(sends), delivery.sentCount)
		}
		if _, ok := sends[0].template.(*SubscriptionEndedTemplate); !ok || sends[0].userAuth != recipient {
			t.Fatalf("sent %T to %q, want the subscription-ended notice to %q", sends[0].template, sends[0].userAuth, recipient)
		}
		message := outboxTestMessage(t, ctx, "synthetic-once")
		if message == nil || message.SentTime == nil || message.ClaimId != nil || message.AttemptCount != 1 {
			t.Fatalf("delivered message = %+v, want sent after one attempt with no claim", message)
		}
		if delta := testutil.ToFloat64(accountMessageDeliveryCounter.WithLabelValues("subscription_ended", "sent")) - before; delta != 1 {
			t.Fatalf("counted %v deliveries, want 1", delta)
		}

		deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Hour))
		if sends := sender.sent(); len(sends) != 1 {
			t.Fatalf("a second delivery sent %d messages in all, want 1", len(sends))
		}
	})
}

// A transaction that adds a message and rolls back leaves nothing to deliver.
func TestAccountMessageOutboxRolledBackTransactionLeavesNoMessage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rollbackErr := errors.New("synthetic rollback")
		func() {
			defer func() {
				if value := recover(); value != rollbackErr {
					panic(value)
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				networkId := server.NewId()
				addAccountMessageInTx(ctx, tx, &accountMessage{
					key:       "synthetic-rollback",
					networkId: &networkId,
					userAuth:  "outbox-rollback@synthetic.example",
					template:  &SubscriptionEndedTemplate{},
				})
				panic(rollbackErr)
			})
		}()
		if message := outboxTestMessage(t, ctx, "synthetic-rollback"); message != nil {
			t.Fatalf("a rolled-back transaction left message %+v", message)
		}
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		if sends := sender.sent(); len(sends) != 0 {
			t.Fatalf("sent %d messages for a rolled-back transaction", len(sends))
		}
	})
}

// A writer that adds the same key again (a rerun, a retried task or webhook)
// owes one message.
func TestAccountMessageOutboxAddsAKeyOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if !addOutboxTestMessage(ctx, "synthetic-key", "outbox-key@synthetic.example") {
			t.Fatal("the first add was refused")
		}
		if addOutboxTestMessage(ctx, "synthetic-key", "outbox-key@synthetic.example") {
			t.Fatal("the same key was added twice")
		}
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		if sends := sender.sent(); len(sends) != 1 {
			t.Fatalf("sent %d messages for one key, want 1", len(sends))
		}
	})
}

// A failed send leaves the message due again after the backoff, which doubles
// with each failure; the message is sent at a later run, once.
func TestAccountMessageOutboxRetriesAFailedSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-retry", "outbox-retry@synthetic.example")
		sender := newOutboxTestSender()
		sender.failNext(2)

		firstTime := server.NowUtc()
		delivery := deliverAccountMessagesAt(ctx, sender, firstTime)
		message := outboxTestMessage(t, ctx, "synthetic-retry")
		if delivery.retryCount != 1 || message.SentTime != nil || message.AttemptCount != 1 ||
			message.DeliverTime == nil || !message.DeliverTime.Equal(firstTime.Add(accountMessageRetryBaseDelay)) ||
			!strings.Contains(message.LastError, "synthetic send failure") || message.ClaimId != nil {
			t.Fatalf("after one failed send: %+v (retried %d), want due again in %s with the error", message, delivery.retryCount, accountMessageRetryBaseDelay)
		}

		// not due before its backoff ends
		deliverAccountMessagesAt(ctx, sender, firstTime.Add(accountMessageRetryBaseDelay-time.Second))
		if failed := sender.failed(); failed != 1 {
			t.Fatalf("attempted %d sends before the backoff ended, want 1", failed)
		}

		secondTime := firstTime.Add(accountMessageRetryBaseDelay)
		deliverAccountMessagesAt(ctx, sender, secondTime)
		message = outboxTestMessage(t, ctx, "synthetic-retry")
		if message.AttemptCount != 2 || message.DeliverTime == nil || !message.DeliverTime.Equal(secondTime.Add(2*accountMessageRetryBaseDelay)) {
			t.Fatalf("after two failed sends: %+v, want due again in %s", message, 2*accountMessageRetryBaseDelay)
		}

		deliverAccountMessagesAt(ctx, sender, secondTime.Add(2*accountMessageRetryBaseDelay))
		message = outboxTestMessage(t, ctx, "synthetic-retry")
		if sends := sender.sent(); len(sends) != 1 || message.SentTime == nil || message.AttemptCount != 3 || message.LastError != "" {
			t.Fatalf("after the retry: sent %d, message %+v, want sent once on the third attempt", len(sends), message)
		}
	})
}

// A delivery that runs while another holds a message's claim sends nothing; the
// message goes out once, from the attempt that claimed it.
func TestAccountMessageOutboxConcurrentDeliveriesSendOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-concurrent", "outbox-concurrent@synthetic.example")
		now := server.NowUtc()

		holdingSender := newHoldingOutboxTestSender()
		firstDone := make(chan *accountMessageDelivery)
		go func() {
			firstDone <- deliverAccountMessagesAt(ctx, holdingSender, now)
		}()
		// the first delivery has claimed the message and is sending it
		<-holdingSender.entered

		secondSender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, secondSender, now)
		deliverAccountMessagesAt(ctx, secondSender, now.Add(accountMessageClaimLease-time.Second))
		if sends := secondSender.sent(); len(sends) != 0 {
			t.Fatalf("a delivery sent %d messages while another held the claim", len(sends))
		}

		holdingSender.releaseSend()
		first := <-firstDone
		if sends := holdingSender.sent(); len(sends) != 1 || first.sentCount != 1 {
			t.Fatalf("the claiming delivery sent %d messages, want 1", len(sends))
		}
		deliverAccountMessagesAt(ctx, secondSender, now.Add(2*accountMessageClaimLease))
		if sends := secondSender.sent(); len(sends) != 0 {
			t.Fatalf("a delivered message was sent again (%d)", len(sends))
		}
	})
}

// An attempt that outlives its claim (a crash between the send and its record)
// does not lose the message: once the lease ends it is claimed and sent again,
// and the late record of the first attempt is counted as a lost claim.
func TestAccountMessageOutboxClaimEndsAfterItsLease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-lease", "outbox-lease@synthetic.example")
		now := server.NowUtc()
		before := testutil.ToFloat64(accountMessageDeliveryCounter.WithLabelValues("subscription_ended", "lease_lost"))

		holdingSender := newHoldingOutboxTestSender()
		firstDone := make(chan *accountMessageDelivery)
		go func() {
			firstDone <- deliverAccountMessagesAt(ctx, holdingSender, now)
		}()
		<-holdingSender.entered

		secondSender := newOutboxTestSender()
		second := deliverAccountMessagesAt(ctx, secondSender, now.Add(accountMessageClaimLease))
		if sends := secondSender.sent(); len(sends) != 1 || second.sentCount != 1 {
			t.Fatalf("after the lease ended a delivery sent %d messages, want 1", len(sends))
		}

		holdingSender.releaseSend()
		first := <-firstDone
		if first.sentCount != 0 {
			t.Fatalf("the attempt that lost its claim recorded %d sends", first.sentCount)
		}
		if delta := testutil.ToFloat64(accountMessageDeliveryCounter.WithLabelValues("subscription_ended", "lease_lost")) - before; delta != 1 {
			t.Fatalf("counted %v lost claims, want 1", delta)
		}
		message := outboxTestMessage(t, ctx, "synthetic-lease")
		if message.SentTime == nil || message.AttemptCount != 2 {
			t.Fatalf("message = %+v, want sent on its second attempt", message)
		}
	})
}

// A message that keeps failing is abandoned after the most attempts and is not
// claimed again.
func TestAccountMessageOutboxAbandonsAfterTheMostAttempts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-abandon", "outbox-abandon@synthetic.example")
		settings := defaultAccountMessageDeliverySettings()
		settings.maxAttemptCount = 3
		sender := newOutboxTestSender()
		sender.failEvery()

		now := server.NowUtc()
		for range 3 {
			deliverAccountMessagesWithSettings(ctx, sender, now, settings)
			now = now.Add(accountMessageRetryMaxDelay)
		}
		message := outboxTestMessage(t, ctx, "synthetic-abandon")
		if message.AbandonTime == nil || message.SentTime != nil || message.AttemptCount != 3 || message.LastError == "" {
			t.Fatalf("message = %+v, want abandoned after 3 attempts with the error", message)
		}
		deliverAccountMessagesWithSettings(ctx, sender, now.Add(accountMessageRetryMaxDelay), settings)
		if failed := sender.failed(); failed != 3 {
			t.Fatalf("attempted %d sends, want 3 (an abandoned message is not claimed)", failed)
		}
	})
}

// A run stops after failed sends in a row (an outage) and leaves the other due
// messages for the next run.
func TestAccountMessageOutboxStopsARunAfterFailedSendsInARow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for i := range accountMessageMaxConsecutiveFailureCount + 2 {
			addOutboxTestMessage(ctx, fmt.Sprintf("synthetic-outage-%d", i), fmt.Sprintf("outbox-outage-%d@synthetic.example", i))
		}
		sender := newOutboxTestSender()
		sender.failEvery()

		now := server.NowUtc()
		delivery := newAccountMessageDelivery(ctx, sender, func() time.Time { return now }, defaultAccountMessageDeliverySettings())
		if more := delivery.Run(); more {
			t.Fatal("a run stopped by failed sends asked to run again at once")
		}
		if failed := sender.failed(); failed != accountMessageMaxConsecutiveFailureCount {
			t.Fatalf("attempted %d sends, want %d", failed, accountMessageMaxConsecutiveFailureCount)
		}
		untouchedCount := 0
		for i := range accountMessageMaxConsecutiveFailureCount + 2 {
			if message := outboxTestMessage(t, ctx, fmt.Sprintf("synthetic-outage-%d", i)); message.AttemptCount == 0 {
				untouchedCount += 1
			}
		}
		if untouchedCount != 2 {
			t.Fatalf("%d messages were left for the next run, want 2", untouchedCount)
		}
	})
}

// A message whose template this build does not know stays due, with a backoff,
// for a build that does, and does not stop the run like a failed send.
func TestAccountMessageOutboxKeepsAnUnregisteredTemplateDue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Tx(ctx, func(tx server.PgTx) {
			for i := range accountMessageMaxConsecutiveFailureCount {
				model.AddAccountMessageInTx(ctx, tx, &model.AccountMessageArgs{
					Key:          fmt.Sprintf("synthetic-unregistered-%d", i),
					UserAuth:     fmt.Sprintf("outbox-unregistered-%d@synthetic.example", i),
					TemplateName: "synthetic_future_template",
					TemplateJson: "{}",
				})
			}
		})
		addOutboxTestMessage(ctx, "synthetic-registered", "outbox-registered@synthetic.example")

		sender := newOutboxTestSender()
		now := server.NowUtc()
		delivery := deliverAccountMessagesAt(ctx, sender, now)
		if sends := sender.sent(); len(sends) != 1 || delivery.retryCount != accountMessageMaxConsecutiveFailureCount {
			t.Fatalf("sent %d and retried %d, want the registered message sent and the others due again", len(sends), delivery.retryCount)
		}
		message := model.GetAccountMessageByKey(ctx, "synthetic_future_template", "synthetic-unregistered-0")
		if message.AbandonTime != nil || message.SentTime != nil || message.DeliverTime == nil ||
			!message.DeliverTime.Equal(now.Add(accountMessageRetryBaseDelay)) {
			t.Fatalf("unregistered message = %+v, want due again after the backoff", message)
		}
	})
}

// A held message is not delivered until it is released.
func TestAccountMessageOutboxDeliversAHeldMessageOnlyAfterItsRelease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			addAccountMessageInTx(ctx, tx, &accountMessage{
				key:       "synthetic-held",
				networkId: &networkId,
				userAuth:  "outbox-held@synthetic.example",
				template:  &SubscriptionEndedTemplate{},
				held:      true,
			})
		})
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Hour))
		if sends := sender.sent(); len(sends) != 0 {
			t.Fatalf("sent %d held messages", len(sends))
		}

		releaseTime := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			held := model.GetHeldAccountMessagesForUpdateInTx(ctx, tx, "subscription_ended", releaseTime)
			if len(held) != 1 {
				t.Fatalf("held messages = %d, want 1", len(held))
			}
			model.ReleaseAccountMessageInTx(ctx, tx, held[0].MessageId, held[0].TemplateJson, releaseTime)
		})
		deliverAccountMessagesAt(ctx, sender, releaseTime)
		if sends := sender.sent(); len(sends) != 1 {
			t.Fatalf("sent %d released messages, want 1", len(sends))
		}
	})
}

// Delivered and abandoned messages are removed after the retention period; due
// messages stay.
func TestAccountMessageOutboxRemovesFinishedMessagesAfterRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-finished", "outbox-finished@synthetic.example")
		deliverAccountMessagesAt(ctx, newOutboxTestSender(), server.NowUtc())
		addOutboxTestMessage(ctx, "synthetic-due", "outbox-due@synthetic.example")

		if removedCount := model.RemoveFinishedAccountMessages(ctx, server.NowUtc().Add(-accountMessageRetention), accountMessageRemoveLimit); removedCount != 0 {
			t.Fatalf("removed %d messages inside the retention period", removedCount)
		}
		if removedCount := model.RemoveFinishedAccountMessages(ctx, server.NowUtc().Add(time.Second), accountMessageRemoveLimit); removedCount != 1 {
			t.Fatalf("removed %d messages past the retention period, want the delivered one", removedCount)
		}
		if message := outboxTestMessage(t, ctx, "synthetic-finished"); message != nil {
			t.Fatalf("the delivered message is still stored: %+v", message)
		}
		if message := outboxTestMessage(t, ctx, "synthetic-due"); message == nil {
			t.Fatal("the due message was removed")
		}
	})
}

// Every template that goes through the outbox keeps its fields through the
// stored json and renders, and the claim lease outlasts two of the longest
// sends.
func TestAccountMessageTemplatesDecodeAndRender(t *testing.T) {
	for templateName := range accountMessageTemplates {
		template, err := decodeAccountMessageTemplate(templateName, "{}")
		if err != nil {
			t.Fatalf("%s: %v", templateName, err)
		}
		if template.Name() != templateName {
			t.Fatalf("template registered as %s is named %s", templateName, template.Name())
		}
		if _, _, _, err := RenderEmailTemplate(template); err != nil {
			t.Fatalf("%s: email: %v", templateName, err)
		}
		if _, err := RenderSmsTemplate(template); err != nil {
			t.Fatalf("%s: sms: %v", templateName, err)
		}
	}
	if accountMessageClaimLease <= 2*maxAccountMessageSendTimeout {
		t.Fatalf("claim lease %s does not outlast two of the longest sends (%s)", accountMessageClaimLease, maxAccountMessageSendTimeout)
	}
}

// The templates the model writes itself are registered under the names it
// writes.
func TestModelAccountMessageTemplatesAreRegistered(t *testing.T) {
	for _, templateName := range []string{model.AuthPasswordSetTemplateName, model.NetworkWelcomeTemplateName} {
		template, err := decodeAccountMessageTemplate(templateName, "{}")
		if err != nil {
			t.Fatalf("%s: %v", templateName, err)
		}
		if template.Name() != templateName {
			t.Fatalf("model template %s decodes as %s", templateName, template.Name())
		}
	}
}

// The delivery task delivers what is due through the message sender and
// schedules its next run after the interval; a run whose context ends early
// asks for the next run at once.
func TestDeliverAccountMessagesTaskDeliversAndSchedulesTheNextRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-task", "outbox-task@synthetic.example")
		sender := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(sender)
		defer SetMessageSender(previousSender)

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		result, err := DeliverAccountMessages(&DeliverAccountMessagesArgs{}, clientSession)
		if err != nil || result.SentCount != 1 || result.More {
			t.Fatalf("delivery run = %+v err=%v, want one sent and nothing left", result, err)
		}
		if sends := sender.sent(); len(sends) != 1 {
			t.Fatalf("delivered %d messages, want 1", len(sends))
		}

		beforePost := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := DeliverAccountMessagesPost(&DeliverAccountMessagesArgs{}, result, clientSession, tx); err != nil {
				t.Fatal(err)
			}
		})
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT run_at FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("deliver_account_messages").String(),
			).Scan(&runAt))
		})
		if runAt.Before(beforePost.Add(accountMessageDeliverInterval - time.Second)) {
			t.Fatalf("next run at %s, want about %s after %s", runAt, accountMessageDeliverInterval, beforePost)
		}

		// a drain or the max time ends a run before it claims more
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		addOutboxTestMessage(ctx, "synthetic-task-canceled", "outbox-task-canceled@synthetic.example")
		delivery := newAccountMessageDelivery(canceledCtx, sender, server.NowUtc, defaultAccountMessageDeliverySettings())
		if more := delivery.Run(); !more || delivery.sentCount != 0 {
			t.Fatalf("canceled run sent %d and asked for more %t, want none sent and the next run at once", delivery.sentCount, more)
		}
	})
}

// A `MessageSender` that runs a hook inside each send and counts the sends.
type outboxHookSender struct {
	onSend    func()
	sendCount int
}

// A `MessageSender`.
func (self *outboxHookSender) SendAccountMessageTemplate(string, Template, ...any) error {
	self.sendCount += 1
	self.onSend()
	return nil
}

// A send that went out while the run was being canceled (a drain, the task's
// max time) is still recorded, so it is not sent again when its lease ends.
func TestAccountMessageOutboxRecordsASendWhenTheRunIsCanceledDuringIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-canceled-send", "outbox-canceled-send@synthetic.example")
		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		sender := &outboxHookSender{onSend: runCancel}

		now := server.NowUtc()
		delivery := newAccountMessageDelivery(runCtx, sender, func() time.Time { return now }, defaultAccountMessageDeliverySettings())
		if more := delivery.Run(); !more || delivery.sentCount != 1 {
			t.Fatalf("canceled run recorded %d sends and asked for more %t, want the send recorded", delivery.sentCount, more)
		}
		message := outboxTestMessage(t, ctx, "synthetic-canceled-send")
		if message.SentTime == nil || message.ClaimId != nil {
			t.Fatalf("message = %+v, want recorded as sent", message)
		}
		deliverAccountMessagesAt(ctx, sender, now.Add(2*accountMessageClaimLease))
		if sender.sendCount != 1 {
			t.Fatalf("sent %d times, want once", sender.sendCount)
		}
	})
}

// A message claimed more often than the most attempts, each time without a
// recorded outcome (a process that keeps stopping in the send), is abandoned
// without another send.
func TestAccountMessageOutboxAbandonsAMessageClaimedTooOftenWithoutAnOutcome(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addOutboxTestMessage(ctx, "synthetic-crash-loop", "outbox-crash-loop@synthetic.example")
		settings := defaultAccountMessageDeliverySettings()
		settings.maxAttemptCount = 3

		now := server.NowUtc()
		for range settings.maxAttemptCount {
			if message := model.ClaimAccountMessage(ctx, now, settings.claimLease); message == nil {
				t.Fatal("the message was not claimable after its lease ended")
			}
			now = now.Add(settings.claimLease)
		}
		sender := newOutboxTestSender()
		delivery := deliverAccountMessagesWithSettings(ctx, sender, now, settings)
		message := outboxTestMessage(t, ctx, "synthetic-crash-loop")
		if len(sender.sent())+sender.failed() != 0 || delivery.abandonCount != 1 || message.AbandonTime == nil ||
			!strings.Contains(message.LastError, "without a recorded outcome") {
			t.Fatalf("message = %+v (abandoned %d, sends %d), want abandoned unsent", message, delivery.abandonCount, len(sender.sent()))
		}
	})
}
