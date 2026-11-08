package pager

// Delivery that outlives a sink outage. A dropped page is the worst failure
// here, so a RetrySink hands each event to the sink it wraps again and again
// until the sink takes it; nothing but Close's deadline ends a retry.
//
// Trigger and Resolve never wait on the network: they record the event and
// return, and one sender goroutine does every hand-off. They wait only for
// stateLock, which is never held across a hand-off, so a caller may hold a
// lock of its own across them to keep its events in order.
//
// The queue holds one slot per dedup key, and a slot holds at most one trigger
// and one resolve, in that order. It is therefore bounded by the incidents
// that open during an outage, however long the outage or often they flap, and
// a full queue cannot refuse a page. A resolve queued behind a trigger still
// goes out after it, so the service sees every page, even one that ended
// during the outage. A trigger arriving while a resolve waits cancels that
// resolve, since the page the service holds is still the open one; a trigger
// arriving while a resolve is in flight goes out after it, since that resolve
// may land.
//
// A hand-off the sink fails sends its slot to the back of the queue, so one
// page the sink keeps refusing cannot hold the others behind it. Failures back
// off from RetryDelay to MaxRetryDelay, shared across pages, and a success
// resets the wait. DeliveryFailing says when hand-offs start failing and when
// they recover.
//
// State is process-local: what Close's deadline abandons is logged and gone.
//
// RetrySink's methods are safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Trigger or Resolve on a closed RetrySink.
var ErrClosed = errors.New("pager: the sink is closed")

// Settings for NewRetrySink. Zero values take the defaults.
type RetrySinkSettings struct {
	// limit on one hand-off; past it the hand-off has failed and is tried again
	SendTimeout time.Duration
	// wait after the first failed hand-off, doubling up to MaxRetryDelay
	RetryDelay    time.Duration
	MaxRetryDelay time.Duration
	// told true when a hand-off fails after none was failing, false when one
	// next succeeds; runs on the sender, so it must not wait on the network
	DeliveryFailing func(failing bool)
	// waits before a retry; false when ctx ends first
	RetryWait func(ctx context.Context, delay time.Duration) bool
	// one line per call, without a trailing newline
	Logf func(format string, args ...any)
}

// The production limits. A retry costs one small post, and a page held back is
// a person not woken, so the wait stays short. Logf writes to stderr.
func DefaultRetrySinkSettings() RetrySinkSettings {
	return RetrySinkSettings{
		SendTimeout:     10 * time.Second,
		RetryDelay:      time.Second,
		MaxRetryDelay:   30 * time.Second,
		DeliveryFailing: func(bool) {},
		RetryWait: func(ctx context.Context, delay time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(delay):
				return true
			}
		},
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
}

// Fills unset limits and seams from DefaultRetrySinkSettings.
func (self RetrySinkSettings) withDefaults() RetrySinkSettings {
	defaults := DefaultRetrySinkSettings()
	if self.SendTimeout <= 0 {
		self.SendTimeout = defaults.SendTimeout
	}
	if self.MaxRetryDelay <= 0 {
		self.MaxRetryDelay = defaults.MaxRetryDelay
	}
	if self.RetryDelay <= 0 {
		self.RetryDelay = defaults.RetryDelay
	}
	self.RetryDelay = min(self.RetryDelay, self.MaxRetryDelay)
	if self.DeliveryFailing == nil {
		self.DeliveryFailing = defaults.DeliveryFailing
	}
	if self.RetryWait == nil {
		self.RetryWait = defaults.RetryWait
	}
	if self.Logf == nil {
		self.Logf = defaults.Logf
	}
	return self
}

// What one hand-off asks of the sink.
type retryAction int

const (
	retryNothing retryAction = iota
	retryTrigger
	retryResolve
)

// What waits for one dedup key.
type retrySlot struct {
	page    Page
	trigger bool
	resolve bool
	// in queuedKeys
	queued bool
	// the hand-off the sender has in flight
	sending retryAction
}

// One hand-off, taken from its slot.
type retryEvent struct {
	action   retryAction
	dedupKey string
	page     Page
}

// A Sink that queues and retries (see the file comment).
type RetrySink struct {
	sink     Sink
	settings RetrySinkSettings
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	// wakes an idle sender; holds at most one wake
	wake chan struct{}
	// notified when the last slot empties
	idle *connect.Monitor

	stateLock sync.Mutex
	closed    bool
	keySlots  map[string]*retrySlot
	// slots waiting for the sender, oldest first
	queuedKeys []string

	// owned by the sender goroutine
	failing        bool
	failedHandOffs int
}

// The sender is running when it returns.
func NewRetrySink(sink Sink, settings RetrySinkSettings) *RetrySink {
	ctx, cancel := context.WithCancel(context.Background())
	retrySink := &RetrySink{
		sink:     sink,
		settings: settings.withDefaults(),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		idle:     connect.NewMonitor(),
		keySlots: map[string]*retrySlot{},
	}
	go retrySink.run()
	return retrySink
}

// Queues the page. A nil return means the sender owns it, not that the service
// took it; see Flush.
func (self *RetrySink) Trigger(_ context.Context, page Page) error {
	if err := page.Validate(); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return ErrClosed
	}
	slot := self.slotWithLock(page.DedupKey)
	// a resolve in flight may land, so only a page the service keeps open can
	// stand on a canceled resolve
	canceled := slot.resolve && slot.sending != retryResolve
	slot.resolve = false
	if !canceled {
		slot.trigger = true
		slot.page = page
	}
	self.queueWithLock(page.DedupKey, slot)
	return nil
}

// Queues the resolve, behind any trigger of the key.
func (self *RetrySink) Resolve(_ context.Context, dedupKey string) error {
	if err := ValidateDedupKey(dedupKey); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return ErrClosed
	}
	slot := self.slotWithLock(dedupKey)
	slot.resolve = true
	self.queueWithLock(dedupKey, slot)
	return nil
}

// Waits until the sink has taken every event queued so far, or ctx ends. A
// caller that must know a page landed, such as a task that can retry, waits
// here.
func (self *RetrySink) Flush(ctx context.Context) error {
	for {
		var idle <-chan struct{}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if 0 < len(self.keySlots) {
				idle = self.idle.NotifyChannel()
			}
		}()
		if idle == nil {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Stops taking events and waits for the sender to hand over what it holds,
// retries included. When ctx ends first the rest is abandoned, and the first
// Close logs how much.
func (self *RetrySink) Close(ctx context.Context) {
	first := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		first = !self.closed
		self.closed = true
	}()
	self.wakeSender()
	select {
	case <-self.done:
	case <-ctx.Done():
		self.cancel()
		<-self.done
	}
	self.cancel()
	if !first {
		return
	}
	abandonedCount := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		abandonedCount = len(self.keySlots)
	}()
	if 0 < abandonedCount {
		self.settings.Logf("pager: %d page(s) still held events the sink had not taken at close", abandonedCount)
	}
}

func (self *RetrySink) slotWithLock(dedupKey string) *retrySlot {
	slot, ok := self.keySlots[dedupKey]
	if !ok {
		slot = &retrySlot{}
		self.keySlots[dedupKey] = slot
	}
	return slot
}

// A slot in flight is queued again when its hand-off ends.
func (self *RetrySink) queueWithLock(dedupKey string, slot *retrySlot) {
	if !slot.queued && slot.sending == retryNothing {
		slot.queued = true
		self.queuedKeys = append(self.queuedKeys, dedupKey)
	}
	self.wakeSender()
}

func (self *RetrySink) wakeSender() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

func (self *RetrySink) dropSlotWithLock(dedupKey string) {
	delete(self.keySlots, dedupKey)
	if len(self.keySlots) == 0 {
		self.idle.NotifyAll()
	}
}

// The oldest waiting hand-off, now in flight, and whether the sender is done:
// closed with nothing left. A slot a later trigger emptied falls out here.
func (self *RetrySink) take() (event retryEvent, ok bool, finished bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for 0 < len(self.queuedKeys) {
		dedupKey := self.queuedKeys[0]
		self.queuedKeys = self.queuedKeys[1:]
		slot := self.keySlots[dedupKey]
		slot.queued = false
		switch {
		case slot.trigger:
			slot.trigger = false
			slot.sending = retryTrigger
			return retryEvent{action: retryTrigger, dedupKey: dedupKey, page: slot.page}, true, false
		case slot.resolve:
			slot.resolve = false
			slot.sending = retryResolve
			return retryEvent{action: retryResolve, dedupKey: dedupKey}, true, false
		default:
			self.dropSlotWithLock(dedupKey)
		}
	}
	return retryEvent{}, false, self.closed
}

// Settles a hand-off. A failed trigger waits again, ahead of any resolve. A
// failed resolve waits again unless a trigger arrived meanwhile: the page is
// wanted open then, and the trigger alone leaves it so.
func (self *RetrySink) finish(event retryEvent, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	slot := self.keySlots[event.dedupKey]
	slot.sending = retryNothing
	if err != nil {
		if event.action == retryTrigger {
			slot.trigger = true
		} else if !slot.trigger {
			slot.resolve = true
		}
	}
	if slot.trigger || slot.resolve {
		self.queueWithLock(event.dedupKey, slot)
		return
	}
	self.dropSlotWithLock(event.dedupKey)
}

func (self *RetrySink) handOff(event retryEvent) error {
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.SendTimeout)
	defer cancel()
	if event.action == retryTrigger {
		return self.sink.Trigger(ctx, event.page)
	}
	return self.sink.Resolve(ctx, event.dedupKey)
}

// Logs the first failure of a run of failures; the rest are counted.
func (self *RetrySink) noteFailed(err error) {
	self.failedHandOffs++
	if self.failing {
		return
	}
	self.failing = true
	self.settings.Logf("pager: the sink did not take a page (%v); retrying until it does, and counting later failures", err)
	self.settings.DeliveryFailing(true)
}

func (self *RetrySink) noteTaken() {
	if !self.failing {
		return
	}
	self.settings.Logf("pager: the sink takes pages again after %d failed hand-off(s)", self.failedHandOffs)
	self.failing = false
	self.failedHandOffs = 0
	self.settings.DeliveryFailing(false)
}

// The sender: hands queued events to the sink, one at a time, until the sink
// has taken each. It returns once closed with nothing left, or at Close's
// deadline.
func (self *RetrySink) run() {
	defer close(self.done)
	delay := self.settings.RetryDelay
	for {
		event, ok, finished := self.take()
		if finished {
			return
		}
		if !ok {
			select {
			case <-self.ctx.Done():
				return
			case <-self.wake:
			}
			continue
		}
		err := self.handOff(event)
		self.finish(event, err)
		if err == nil {
			delay = self.settings.RetryDelay
			self.noteTaken()
			continue
		}
		if self.ctx.Err() != nil {
			// Close's deadline cut the hand-off short; the sink did not refuse it
			return
		}
		self.noteFailed(err)
		if !self.settings.RetryWait(self.ctx, delay) {
			return
		}
		delay = min(2*delay, self.settings.MaxRetryDelay)
	}
}
