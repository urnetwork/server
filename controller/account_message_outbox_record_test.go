// The account message outbox records each outcome on a context of its own, made
// when the record starts (QD-1220): after the send, or after the withheld check
// for a message withheld unsent, so the record has the whole record timeout
// however long the send took. The record contexts run on a test clock that each
// send moves on, so a send is held past the record timeout without real time
// passing.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A clock for a delivery's run and its record contexts. A record context ends,
// as context.WithTimeout's does at its deadline, once the clock reaches its
// deadline. Safe for concurrent use.
type outboxRecordTestClock struct {
	stateLock   sync.Mutex
	currentTime time.Time
	records     []*outboxRecordTestContext
}

// A clock reading `startTime`.
func newOutboxRecordTestClock(startTime time.Time) *outboxRecordTestClock {
	return &outboxRecordTestClock{
		currentTime: startTime,
	}
}

// The clock's time, which the run reads as its own.
func (self *outboxRecordTestClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.currentTime
}

// Makes a context that ends with context.DeadlineExceeded once the clock
// reaches its time now plus `timeout`, and with context.Canceled when its cancel
// is called first: the delivery's contextWithTimeout.
func (self *outboxRecordTestClock) withTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	record := &outboxRecordTestContext{
		Context: ctx,
		cancel:  cancel,
	}
	expired := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		record.startTime = self.currentTime
		record.deadline = self.currentTime.Add(timeout)
		self.records = append(self.records, record)
		return !self.currentTime.Before(record.deadline)
	}()
	if expired {
		// a non-positive timeout ends at once, as context.WithTimeout's does
		cancel(context.DeadlineExceeded)
	}
	return record, func() {
		cancel(context.Canceled)
	}
}

// Moves the clock on by `duration`, ending each record context whose deadline
// it reaches. A context already canceled keeps its first cause.
func (self *outboxRecordTestClock) advance(duration time.Duration) {
	expiredRecords := func() []*outboxRecordTestContext {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.currentTime = self.currentTime.Add(duration)
		expiredRecords := []*outboxRecordTestContext{}
		for _, record := range self.records {
			if !self.currentTime.Before(record.deadline) {
				expiredRecords = append(expiredRecords, record)
			}
		}
		return expiredRecords
	}()
	for _, record := range expiredRecords {
		record.cancel(context.DeadlineExceeded)
	}
}

// The record contexts made so far, in the order they were made.
func (self *outboxRecordTestClock) made() []*outboxRecordTestContext {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*outboxRecordTestContext{}, self.records...)
}

// A record context the test clock made. It reports no deadline, since its
// deadline is on the test clock and real time must not end it; its start and
// deadline are fixed when it is made.
type outboxRecordTestContext struct {
	context.Context
	cancel    context.CancelCauseFunc
	startTime time.Time
	deadline  time.Time
}

// context.DeadlineExceeded once the clock reached the deadline,
// context.Canceled once canceled first, as context.WithTimeout's.
func (self *outboxRecordTestContext) Err() error {
	if self.Context.Err() == nil {
		return nil
	}
	return context.Cause(self.Context)
}

// Whether the clock reached the context's deadline before it was canceled.
func (self *outboxRecordTestContext) expired() bool {
	return errors.Is(self.Err(), context.DeadlineExceeded)
}

// Whether the context was canceled before the clock reached its deadline.
func (self *outboxRecordTestContext) released() bool {
	return errors.Is(self.Err(), context.Canceled)
}

// The context's start, its deadline, and how it ended.
func (self *outboxRecordTestContext) String() string {
	state := "live"
	switch {
	case self.expired():
		state = "expired"
	case self.released():
		state = "released"
	}
	return fmt.Sprintf("{made %s, deadline %s, %s}", self.startTime.Format(time.RFC3339Nano), self.deadline.Format(time.RFC3339Nano), state)
}

// A `MessageSender` whose sends each take `sendDuration` on the record clock,
// moving it on before they return, and fail with `err` when it is set. Not safe
// for concurrent use: its sends run on the delivery's goroutine.
type outboxSlowTestSender struct {
	clock        *outboxRecordTestClock
	sendDuration time.Duration
	err          error
	sendCount    int
}

// A `MessageSender`.
func (self *outboxSlowTestSender) SendAccountMessageTemplate(string, Template, ...any) error {
	self.sendCount += 1
	self.clock.advance(self.sendDuration)
	return self.err
}

// A delivery that reads time from the clock and makes its record contexts on it.
func newOutboxRecordTestDelivery(
	ctx context.Context,
	sender MessageSender,
	clock *outboxRecordTestClock,
	settings *accountMessageDeliverySettings,
) *accountMessageDelivery {
	delivery := newAccountMessageDelivery(ctx, sender, clock.now, settings)
	delivery.contextWithTimeout = clock.withTimeout
	return delivery
}

// Runs the delivery and returns what it raised, nil when nothing did. A record
// on an ended context raises out of the run, as its transaction does.
func runOutboxRecordTestDelivery(delivery *accountMessageDelivery) (raised any) {
	defer func() {
		raised = recover()
	}()
	delivery.Run()
	return
}

// Whether the record contexts were made one at each of `startTimes`, in order,
// each with the whole record timeout and released by its record before the
// clock reached its deadline.
func outboxRecordsHadTheirWholeBudget(
	records []*outboxRecordTestContext,
	startTimes []time.Time,
	settings *accountMessageDeliverySettings,
) bool {
	if len(records) != len(startTimes) {
		return false
	}
	for i, record := range records {
		if !record.startTime.Equal(startTimes[i]) || !record.deadline.Equal(startTimes[i].Add(settings.recordTimeout)) || !record.released() {
			return false
		}
	}
	return true
}

// A send slower than the record timeout that succeeds is recorded once, on a
// context made after the send with the whole timeout, and is not sent again
// when its claim's lease ends (QD-1220). With the record's context made before
// the send, the slow send left it expired: the record raised, the message
// stayed claimed, and the first claim after the lease sent it a second time.
func TestAccountMessageOutboxRecordsASendSlowerThanTheRecordTimeoutOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const key = "synthetic-record-slow-send"
		addOutboxTestMessage(ctx, key, "outbox-record-slow-send@synthetic.example")
		settings := defaultAccountMessageDeliverySettings()
		claimTime := server.NowUtc()
		clock := newOutboxRecordTestClock(claimTime)
		// the send takes twice the record timeout
		sender := &outboxSlowTestSender{
			clock:        clock,
			sendDuration: 2 * settings.recordTimeout,
		}

		delivery := newOutboxRecordTestDelivery(ctx, sender, clock, settings)
		if raised := runOutboxRecordTestDelivery(delivery); raised != nil {
			t.Errorf("the record after a slow send raised: %v", raised)
		}
		sendEndTime := claimTime.Add(2 * settings.recordTimeout)
		message := outboxTestMessage(t, ctx, key)
		if delivery.sentCount != 1 || message.SentTime == nil || !message.SentTime.Equal(sendEndTime) ||
			message.ClaimId != nil || message.AttemptCount != 1 {
			t.Errorf("after a send slower than the record timeout: %+v (counted %d sent), want recorded sent at the send's end after one attempt", message, delivery.sentCount)
		}
		if records := clock.made(); !outboxRecordsHadTheirWholeBudget(records, []time.Time{sendEndTime}, settings) {
			t.Errorf("record contexts = %v, want one made at the send's end %s with the whole %s, released by its record", records, sendEndTime.Format(time.RFC3339Nano), settings.recordTimeout)
		}

		// once the claim's lease ends, a delivery finds nothing to send
		deliverAccountMessagesAt(ctx, sender, claimTime.Add(2*settings.claimLease))
		if sender.sendCount != 1 {
			t.Fatalf("sent %d times, want once", sender.sendCount)
		}
	})
}

// A send slower than the record timeout that fails is recorded as before, each
// time on a context made after the send with the whole timeout: due again after
// the backoff with its error, then abandoned at the most attempts.
func TestAccountMessageOutboxRecordsASlowFailedSendAsBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const key = "synthetic-record-failed-send"
		addOutboxTestMessage(ctx, key, "outbox-record-failed-send@synthetic.example")
		settings := defaultAccountMessageDeliverySettings()
		settings.maxAttemptCount = 2
		firstTime := server.NowUtc()
		clock := newOutboxRecordTestClock(firstTime)
		sender := &outboxSlowTestSender{
			clock:        clock,
			sendDuration: 2 * settings.recordTimeout,
			err:          errors.New("synthetic slow send failure"),
		}

		delivery := newOutboxRecordTestDelivery(ctx, sender, clock, settings)
		if raised := runOutboxRecordTestDelivery(delivery); raised != nil {
			t.Errorf("the record after the first slow failed send raised: %v", raised)
		}
		firstEndTime := firstTime.Add(2 * settings.recordTimeout)
		message := outboxTestMessage(t, ctx, key)
		if delivery.retryCount != 1 || message.SentTime != nil || message.AbandonTime != nil || message.ClaimId != nil ||
			message.AttemptCount != 1 || message.DeliverTime == nil || !message.DeliverTime.Equal(firstEndTime.Add(settings.retryBaseDelay)) ||
			!strings.Contains(message.LastError, "synthetic slow send failure") {
			t.Fatalf("after a slow failed send: %+v (retried %d), want due again %s after the send's end with the error", message, delivery.retryCount, settings.retryBaseDelay)
		}

		// due again; the last attempt, as slow, gives the message up
		clock.advance(settings.retryBaseDelay)
		secondTime := clock.now()
		delivery = newOutboxRecordTestDelivery(ctx, sender, clock, settings)
		if raised := runOutboxRecordTestDelivery(delivery); raised != nil {
			t.Errorf("the record after the last slow failed send raised: %v", raised)
		}
		secondEndTime := secondTime.Add(2 * settings.recordTimeout)
		message = outboxTestMessage(t, ctx, key)
		if delivery.abandonCount != 1 || message.AbandonTime == nil || !message.AbandonTime.Equal(secondEndTime) ||
			message.SentTime != nil || message.ClaimId != nil || message.AttemptCount != 2 ||
			!strings.Contains(message.LastError, "synthetic slow send failure") {
			t.Fatalf("after the last slow failed send: %+v (abandoned %d), want abandoned at the send's end with the error", message, delivery.abandonCount)
		}
		if records := clock.made(); !outboxRecordsHadTheirWholeBudget(records, []time.Time{firstEndTime, secondEndTime}, settings) {
			t.Errorf("record contexts = %v, want one made at each send's end with the whole %s, released by its record", records, settings.recordTimeout)
		}
		if sender.sendCount != 2 {
			t.Fatalf("sent %d times, want 2", sender.sendCount)
		}
	})
}

// A withheld message is abandoned unsent on a record context of its own, made
// after the withheld check with the whole record timeout, here after another
// message's send slower than the timeout in the same run.
func TestAccountMessageOutboxRecordsAWithheldMessageOnItsOwnBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usdcSwitchTestDeclare(t, "wallet_requests: off\n")
		const sentKey = "synthetic-record-before-withheld"
		addOutboxTestMessage(ctx, sentKey, "outbox-record-before-withheld@synthetic.example")
		const withheldKey = "synthetic-record-withheld"
		withheldNetworkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			model.AddAccountMessageInTx(ctx, tx, &model.AccountMessageArgs{
				Key:          withheldKey,
				NetworkId:    &withheldNetworkId,
				UserAuth:     "outbox-record-withheld@synthetic.example",
				TemplateName: (&MissingWalletTemplate{}).Name(),
				TemplateJson: "{}",
				// claimed after the other message
				DeliverAfter: time.Second,
			})
		})
		settings := defaultAccountMessageDeliverySettings()
		startTime := server.NowUtc().Add(time.Minute)
		clock := newOutboxRecordTestClock(startTime)
		sender := &outboxSlowTestSender{
			clock:        clock,
			sendDuration: 2 * settings.recordTimeout,
		}

		delivery := newOutboxRecordTestDelivery(ctx, sender, clock, settings)
		if raised := runOutboxRecordTestDelivery(delivery); raised != nil {
			t.Errorf("a record raised: %v", raised)
		}
		if delivery.sentCount != 1 || delivery.withheldCount != 1 || sender.sendCount != 1 {
			t.Fatalf("sent %d (counted %d) and withheld %d, want the first sent and the notice withheld", sender.sendCount, delivery.sentCount, delivery.withheldCount)
		}
		sendEndTime := startTime.Add(2 * settings.recordTimeout)
		withheld := model.GetAccountMessageByKey(ctx, (&MissingWalletTemplate{}).Name(), withheldKey)
		if withheld == nil || withheld.AbandonTime == nil || !withheld.AbandonTime.Equal(sendEndTime) || withheld.SentTime != nil ||
			withheld.ClaimId != nil || withheld.LastError != "withheld: wallet requests are off (usdc_payout.yml)" {
			t.Fatalf("withheld notice = %+v, want abandoned unsent with the reason at the other send's end", withheld)
		}
		// the sent message's record, then the withheld one's, each its own
		if records := clock.made(); !outboxRecordsHadTheirWholeBudget(records, []time.Time{sendEndTime, sendEndTime}, settings) {
			t.Errorf("record contexts = %v, want two made at the send's end with the whole %s, each released by its record", records, settings.recordTimeout)
		}
	})
}

// The claim's lease outlasts the longest send email.yml allows and the whole
// record timeout after it, so a record that starts after the slowest send still
// lands while the claim holds.
func TestAccountMessageClaimLeaseOutlastsTheLongestSendAndItsRecord(t *testing.T) {
	deliverySettings := defaultAccountMessageDeliverySettings()
	sendSettings := defaultAccountMessageSendSettings()
	if deliverySettings.claimLease <= sendSettings.maxSendTimeout+deliverySettings.recordTimeout {
		t.Fatalf(
			"claim lease %s does not outlast the longest send %s and its record %s",
			deliverySettings.claimLease,
			sendSettings.maxSendTimeout,
			deliverySettings.recordTimeout,
		)
	}
}
