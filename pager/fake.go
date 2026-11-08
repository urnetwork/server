package pager

// FakeSink stands in for the pager service, so FS-50 runs whichever service
// plan-OPS-pager-service names. It models §19.8's rota on a caller's clock: a
// page reaches the first person, the second is paged once the first has not
// acknowledged in EscalateAfter, a page marked BothAtOnce reaches both at once,
// and a repeated trigger of an open key changes nothing, its clock included.
// Time moves only when a test moves the clock and calls Tick.
//
// It also serves WebhookSink's events over http, so a test can put the real
// client in front of it.
//
// FakeSink's methods are safe for concurrent use.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"
)

// The rota's two people.
const (
	FakeFirstPerson  = "first"
	FakeSecondPerson = "second"
)

// Settings for NewFakeSink. Zero values take the defaults.
type FakeSinkSettings struct {
	// how long the first person has before the second is paged
	EscalateAfter time.Duration
	Now           func() time.Time
}

// §19.8's 30 minutes, on the wall clock.
func DefaultFakeSinkSettings() FakeSinkSettings {
	return FakeSinkSettings{
		EscalateAfter: 30 * time.Minute,
		Now:           time.Now,
	}
}

// One person paged.
type FakeNotification struct {
	Person string
	Page   Page
	Time   time.Time
}

// One call that reached the sink, taken or refused.
type FakeAttempt struct {
	Resolve  bool
	DedupKey string
	Refused  bool
}

// One open page.
type fakeOpenPage struct {
	page         Page
	openTime     time.Time
	acknowledged bool
	secondPaged  bool
}

// The pager service, on a test's clock (see the file comment).
type FakeSink struct {
	settings FakeSinkSettings

	stateLock sync.Mutex
	// refuses every call while set
	failure       error
	keyOpenPages  map[string]*fakeOpenPage
	notifications []FakeNotification
	attempts      []FakeAttempt
}

func NewFakeSink(settings FakeSinkSettings) *FakeSink {
	defaults := DefaultFakeSinkSettings()
	if settings.EscalateAfter <= 0 {
		settings.EscalateAfter = defaults.EscalateAfter
	}
	if settings.Now == nil {
		settings.Now = defaults.Now
	}
	return &FakeSink{
		settings:     settings,
		keyOpenPages: map[string]*fakeOpenPage{},
	}
}

func (self *FakeSink) Trigger(_ context.Context, page Page) error {
	if err := page.Validate(); err != nil {
		return err
	}
	now := self.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attempts = append(self.attempts, FakeAttempt{DedupKey: page.DedupKey, Refused: self.failure != nil})
	if self.failure != nil {
		return self.failure
	}
	if _, ok := self.keyOpenPages[page.DedupKey]; ok {
		return nil
	}
	self.keyOpenPages[page.DedupKey] = &fakeOpenPage{page: page, openTime: now, secondPaged: page.BothAtOnce}
	self.notifications = append(self.notifications, FakeNotification{Person: FakeFirstPerson, Page: page, Time: now})
	if page.BothAtOnce {
		self.notifications = append(self.notifications, FakeNotification{Person: FakeSecondPerson, Page: page, Time: now})
	}
	return nil
}

func (self *FakeSink) Resolve(_ context.Context, dedupKey string) error {
	if err := ValidateDedupKey(dedupKey); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attempts = append(self.attempts, FakeAttempt{Resolve: true, DedupKey: dedupKey, Refused: self.failure != nil})
	if self.failure != nil {
		return self.failure
	}
	delete(self.keyOpenPages, dedupKey)
	return nil
}

// A person acknowledges the key's page; false when it is not open.
func (self *FakeSink) Acknowledge(dedupKey string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	open, ok := self.keyOpenPages[dedupKey]
	if ok {
		open.acknowledged = true
	}
	return ok
}

// Runs the rota's policy at the clock's time, in the order the pages opened.
func (self *FakeSink) Tick() {
	now := self.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	due := []*fakeOpenPage{}
	for _, open := range self.keyOpenPages {
		if !open.acknowledged && !open.secondPaged && self.settings.EscalateAfter <= now.Sub(open.openTime) {
			due = append(due, open)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].openTime.Equal(due[j].openTime) {
			return due[i].openTime.Before(due[j].openTime)
		}
		return due[i].page.DedupKey < due[j].page.DedupKey
	})
	for _, open := range due {
		open.secondPaged = true
		self.notifications = append(self.notifications, FakeNotification{
			Person: FakeSecondPerson,
			Page:   open.page,
			Time:   open.openTime.Add(self.settings.EscalateAfter),
		})
	}
}

// Refuses every call with err, or takes them again when err is nil.
func (self *FakeSink) Fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failure = err
}

// Everyone paged so far, in order.
func (self *FakeSink) Notifications() []FakeNotification {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]FakeNotification(nil), self.notifications...)
}

// Every call so far, in order.
func (self *FakeSink) Attempts() []FakeAttempt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]FakeAttempt(nil), self.attempts...)
}

// The keys of the open pages, sorted.
func (self *FakeSink) OpenKeys() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	dedupKeys := make([]string, 0, len(self.keyOpenPages))
	for dedupKey := range self.keyOpenPages {
		dedupKeys = append(dedupKeys, dedupKey)
	}
	sort.Strings(dedupKeys)
	return dedupKeys
}

// Takes one WebhookSink post. A refusal answers 503, and a body that does not
// decode 400.
func (self *FakeSink) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	var event webhookEvent
	if request.Method != http.MethodPost || json.NewDecoder(request.Body).Decode(&event) != nil {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	err := errors.New("pager: an unknown webhook action")
	switch event.Action {
	case webhookActionTrigger:
		page := Page{DedupKey: event.DedupKey, Class: event.Class, Summary: event.Summary, BothAtOnce: event.BothAtOnce}
		page.Time, err = time.Parse(time.RFC3339, event.Time)
		if err == nil {
			err = self.Trigger(request.Context(), page)
		}
	case webhookActionResolve:
		err = self.Resolve(request.Context(), event.DedupKey)
	}
	if err != nil {
		response.WriteHeader(http.StatusServiceUnavailable)
	}
}
