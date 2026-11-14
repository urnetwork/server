//go:build sidecar_pending

package controller

// A fake of Stripe's meters for FS-43 (QUOTA-M6-04), behind stripeMeterClient, on the test's clock:
// events with their identifiers, the 24 h dedup of an identifier, pre-aggregated daily ingestion
// (a day keeps its most recently received event per customer), an invoice's quantity at
// finalization, summaries that lag, and a plan of faults for the sends to come. Its errors are
// stripe-go's, placed by the controller's own stripeMeterErrorOutcome.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server/model"
)

type fakeStripeMeterFault int

const (
	// Stripe takes the event and answers 200
	fakeStripeMeterAnswer fakeStripeMeterFault = iota
	// Stripe takes the event and the answer is lost: the caller sees a timeout
	fakeStripeMeterApplyThenDrop
	// the request never arrives
	fakeStripeMeterTimeout
	// 429 and 500: nothing taken
	fakeStripeMeterRateLimited
	fakeStripeMeterServerError
	// a 400 that names no duplicate: nothing taken
	fakeStripeMeterRefused
)

type fakeStripeMeterReceipt struct {
	event      stripeMeterEvent
	receivedAt time.Time
}

type fakeStripeMeters struct {
	stateLock sync.Mutex
	now       func() time.Time
	// how long an identifier is refused as a duplicate
	dedupWindow time.Duration
	// how far behind the summaries run
	summaryLag time.Duration
	// every event taken, in the order received
	receipts    []*fakeStripeMeterReceipt
	identifiers map[string]time.Time
	// the faults of the sends to come, first first; past the plan Stripe answers
	plan []fakeStripeMeterFault
	// a fault by the event itself, ahead of the plan when it says so
	faultOf func(event *stripeMeterEvent) (fakeStripeMeterFault, bool)
	// every send made, whatever became of it
	sends []stripeMeterEvent
	// sends in flight by (customer, event name), and the most any pair saw
	inFlight    map[string]int
	maxInFlight int
	// called at the start of a send, outside the lock; a test blocks a send here
	sendHook func(event *stripeMeterEvent)
}

func newFakeStripeMeters(now func() time.Time) *fakeStripeMeters {
	return &fakeStripeMeters{
		now:         now,
		dedupWindow: 24 * time.Hour,
		identifiers: map[string]time.Time{},
		inFlight:    map[string]int{},
	}
}

func (self *fakeStripeMeters) setPlan(faults ...fakeStripeMeterFault) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.plan = faults
}

func (self *fakeStripeMeters) sendCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.sends)
}

func (self *fakeStripeMeters) sentEvents() []stripeMeterEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]stripeMeterEvent{}, self.sends...)
}

func (self *fakeStripeMeters) SendMeterEvent(ctx context.Context, event *stripeMeterEvent) model.StripeMeterOutcome {
	pair := event.CustomerId + "/" + event.EventName
	self.stateLock.Lock()
	self.inFlight[pair] += 1
	self.maxInFlight = max(self.maxInFlight, self.inFlight[pair])
	hook := self.sendHook
	self.stateLock.Unlock()
	if hook != nil {
		hook(event)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.inFlight[pair] -= 1
	self.sends = append(self.sends, *event)
	fault := fakeStripeMeterAnswer
	planned := true
	if self.faultOf != nil {
		if eventFault, ok := self.faultOf(event); ok {
			fault = eventFault
			planned = false
		}
	}
	if planned && 0 < len(self.plan) {
		fault = self.plan[0]
		self.plan = self.plan[1:]
	}
	var err error
	switch fault {
	case fakeStripeMeterTimeout:
		err = context.DeadlineExceeded
	case fakeStripeMeterRateLimited:
		err = &stripe.Error{HTTPStatusCode: 429, Type: stripe.ErrorTypeInvalidRequest, Code: stripe.ErrorCodeRateLimit, Msg: "Too many requests."}
	case fakeStripeMeterServerError:
		err = &stripe.Error{HTTPStatusCode: 500, Type: stripe.ErrorTypeAPI, Msg: "An error occurred."}
	case fakeStripeMeterRefused:
		err = &stripe.Error{HTTPStatusCode: 400, Type: stripe.ErrorTypeInvalidRequest, Code: stripe.ErrorCodeParameterInvalidInteger, Param: "payload", Msg: "Invalid payload."}
	default:
		now := self.now()
		if receivedAt, ok := self.identifiers[event.Identifier]; ok && now.Sub(receivedAt) < self.dedupWindow {
			err = &stripe.Error{HTTPStatusCode: 400, Type: stripe.ErrorTypeInvalidRequest, Code: stripe.ErrorCodeResourceAlreadyExists, Msg: "An event already exists with this identifier."}
			break
		}
		self.identifiers[event.Identifier] = now
		self.receipts = append(self.receipts, &fakeStripeMeterReceipt{event: *event, receivedAt: now})
		if fault == fakeStripeMeterApplyThenDrop {
			err = errors.New("the answer was lost")
		}
	}
	if err != nil {
		return stripeMeterErrorOutcome(err, event.Identifier, DefaultStripeMeterSettings().DuplicateIdentifierCodes)
	}
	return model.StripeMeterOutcomeSent
}

// The event the customer's meter keeps for the UTC day as of asOf: the most recently received one
// stamped inside it.
func (self *fakeStripeMeters) dayEvent(customerId string, eventName string, day time.Time, asOf time.Time) *fakeStripeMeterReceipt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var kept *fakeStripeMeterReceipt
	for _, receipt := range self.receipts {
		if receipt.event.CustomerId != customerId || receipt.event.EventName != eventName || asOf.Before(receipt.receivedAt) {
			continue
		}
		if receipt.event.Timestamp.Before(day) || !receipt.event.Timestamp.Before(day.AddDate(0, 0, 1)) {
			continue
		}
		kept = receipt
	}
	return kept
}

// Every event the customer's meter was sent for the UTC day, in the order received.
func (self *fakeStripeMeters) dayReceipts(customerId string, eventName string, day time.Time) (receipts []*fakeStripeMeterReceipt) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, receipt := range self.receipts {
		if receipt.event.CustomerId != customerId || receipt.event.EventName != eventName {
			continue
		}
		if receipt.event.Timestamp.Before(day) || !receipt.event.Timestamp.Before(day.AddDate(0, 0, 1)) {
			continue
		}
		receipts = append(receipts, receipt)
	}
	return
}

// What an invoice finalized at finalizedAt bills the customer for a sum meter over the period
// [periodStart, periodEnd): each day's kept event, added.
func (self *fakeStripeMeters) billedSum(customerId string, eventName string, periodStart time.Time, periodEnd time.Time, finalizedAt time.Time) (billed int64) {
	for day := time.Date(periodStart.Year(), periodStart.Month(), periodStart.Day(), 0, 0, 0, 0, time.UTC); day.Before(periodEnd); day = day.AddDate(0, 0, 1) {
		receipt := self.dayEvent(customerId, eventName, day, finalizedAt)
		if receipt == nil || receipt.event.Timestamp.Before(periodStart) || !receipt.event.Timestamp.Before(periodEnd) {
			continue
		}
		billed += receipt.event.Value
	}
	return
}

func (self *fakeStripeMeters) ListMeterSummaries(ctx context.Context, meterId string, customerId string, from time.Time, to time.Time) ([]*stripeMeterSummary, error) {
	// the fake's meter id is its event name
	asOf := self.now().Add(-self.summaryLag)
	summaries := []*stripeMeterSummary{}
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		if receipt := self.dayEvent(customerId, meterId, day, asOf); receipt != nil {
			summaries = append(summaries, &stripeMeterSummary{Day: day, Value: receipt.event.Value})
		}
	}
	return summaries, nil
}
