package egresshealth

import (
	"context"
	"net/http"
	"sync"
)

// The tunnel a run rides, and the single-flight re-opening of it when it dies
// part-way through the run.

// The tunnel a run's requests ride, for a caller that can replace it.
//
// A run spans minutes, and a provider tunnel can die part-way through one --
// the provider disconnects, the multi-client loses its path, the packet pump
// fails. The loads the dead tunnel had not finished would otherwise fail
// against a tunnel that is no longer there, which says nothing about the
// sites or the exit, and a provider that merely reconnected would be charged
// with every site the run had not reached yet. With a Path, the next attempt
// of every pending load first re-opens the tunnel to the same provider and
// carries on through the new one; the loads keep their remaining attempts and
// their spacing (see run.load).
type Path interface {
	// The client to send through now, and a context that ends
	// when that client's tunnel is lost for good (nil when it cannot be).
	Current() (*http.Client, context.Context)
	// Replaces a lost tunnel with a new one to the same provider and
	// makes it Current. An error means no tunnel could be opened, and the
	// attempt that needed one is not measured. The run calls it from one load
	// at a time, only after Current's tunnel was lost, and at most
	// Options.TunnelRecreateAttempts times.
	Reopen(ctx context.Context) error
}

// The path of a caller that passed a client and no Path: one
// client, never lost, never replaced.
type staticPath struct {
	client *http.Client
}

// Implements Path: the one client, never lost.
func (self staticPath) Current() (*http.Client, context.Context) {
	return self.client, nil
}

// Implements Path: a fixed client cannot be replaced.
func (self staticPath) Reopen(context.Context) error {
	return errStaticPath
}

// Never returned in practice -- a static path is never lost,
// so nothing asks it to re-open -- and exists so the method cannot silently
// succeed if that ever changes.
var errStaticPath = pathError("egresshealth: a fixed client cannot be re-opened")

// The string error type of this file's sentinels.
type pathError string

// Implements error.
func (self pathError) Error() string { return string(self) }

// Reports whether a tunnel's loss signal has fired.
func lost(signal context.Context) bool {
	return signal != nil && signal.Err() != nil
}

// A run's view of its Path: every load asks it for a live
// client before each attempt, and at most one re-open runs at a time, its
// outcome shared by every load that was waiting on it.
//
// Single-flight because a dead tunnel is noticed by every pending load at
// once. Each opening its own tunnel would open dozens to one provider -- each a
// new client device and a new contract -- where one is needed; and a load
// that arrives while a re-open is running is better served by its result than
// by starting another.
//
// The Path is an external object, so it is never called with stateLock held.
// Its state is read between two locked scopes, and finishedReopenCount is what
// lets the second scope tell whether a re-open started or finished in between.
// Safe for concurrent use by every load of the run.
type pathTracker struct {
	path Path
	// Runs on every re-opened client before any load uses it: a new
	// tunnel is as cold as the first one, and the cold start is the warm-up's
	// to pay, not a scored load's (see warmUp).
	warm func(ctx context.Context, client *http.Client)
	// How many re-creations the run may ask for; reopenCount is how many it
	// has. Every call counts, a failed one too: whether or not it came up,
	// it may have cost the provider a device and a contract.
	reopenLimit int

	stateLock   sync.Mutex
	inflight    *reopenCall
	reopenCount int
	// How many re-opens have finished, their warm-ups included.
	finishedReopenCount int
}

// Why an attempt has no tunnel once the run has used up
// its re-creations.
var errRecreateLimit = pathError("egresshealth: the tunnel was lost again and this run has used up its re-creations")

// One re-open in flight. err is written before done closes, and read only
// after.
type reopenCall struct {
	done chan struct{}
	err  error
}

// Returns the client an attempt should use and that client's loss
// signal, re-opening the tunnel first if it is lost. An error means the
// tunnel is lost and could not be re-opened.
//
// While a re-open is in flight every caller waits for it, even one whose
// Current already looks live: the new tunnel is Current before its warm-up
// has run, and a load let onto it then would pay its cold start. A caller
// that read Current while a re-open started or finished does not trust what
// it read: it waits for the one in flight, or takes the tunnel the finished
// one left, rather than open another.
//
// It re-opens at most once per call: a tunnel that dies again straight after
// being re-opened is handed back as it is, and the attempt that uses it comes
// out not measured, rather than this looping on a provider that cannot hold a
// tunnel up.
func (self *pathTracker) live(ctx context.Context) (*http.Client, context.Context, error) {
	var call *reopenCall
	var finishedReopenCount int
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		call = self.inflight
		finishedReopenCount = self.finishedReopenCount
	}()
	if call != nil {
		return self.await(ctx, call)
	}

	client, signal := self.path.Current()
	isLost := lost(signal)

	started := false
	reopened := false
	var err error
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		switch {
		case self.inflight != nil:
			// a re-open started while Current was read
			call = self.inflight
		case self.finishedReopenCount != finishedReopenCount:
			// a re-open started and finished while Current was read
			reopened = true
		case !isLost:
		case self.reopenLimit <= self.reopenCount:
			err = errRecreateLimit
		default:
			self.reopenCount++
			call = &reopenCall{done: make(chan struct{})}
			self.inflight = call
			started = true
		}
	}()
	switch {
	case err != nil:
		return nil, nil, err
	case reopened:
		client, signal := self.path.Current()
		return client, signal, nil
	case call != nil && !started:
		return self.await(ctx, call)
	case !started:
		return client, signal, nil
	}

	call.err = self.path.Reopen(ctx)
	if call.err == nil && self.warm != nil {
		reopenedClient, reopenedSignal := self.path.Current()
		warmCtx, stop := bound(ctx, reopenedSignal)
		self.warm(warmCtx, reopenedClient)
		stop()
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.inflight = nil
		self.finishedReopenCount++
	}()
	close(call.done)
	return self.outcome(call)
}

// Waits out a re-open another load started, and shares its outcome.
func (self *pathTracker) await(ctx context.Context, call *reopenCall) (*http.Client, context.Context, error) {
	select {
	case <-call.done:
	case <-ctx.Done():
		return nil, nil, context.Cause(ctx)
	}
	return self.outcome(call)
}

// Returns what a finished re-open left: its error, or the path's current
// client and that client's loss signal.
func (self *pathTracker) outcome(call *reopenCall) (*http.Client, context.Context, error) {
	if call.err != nil {
		return nil, nil, call.err
	}
	client, signal := self.path.Current()
	return client, signal, nil
}
