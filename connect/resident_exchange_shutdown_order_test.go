// Owns the delayed cancellation publisher used to force one legal sibling
// shutdown ordering through the real native Transport constructor and Run loop.
package connect

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// This control supplies a legal asynchronous cancellation parent only to the
// real Transport constructor. It deliberately exposes no inherited values so
// context.WithCancel uses the documented AfterFunc path, not a private runtime
// parent shortcut. Root Done/Err/Deadline remain those of the real fixture.
type exchangeQueueRefusalDelayedCancelContext struct {
	context.Context
	release             chan struct{}
	joined              chan struct{}
	releaseOnce         sync.Once
	joinOnce            sync.Once
	registered          atomic.Bool
	stoppedBeforeParent atomic.Bool
}

func (self *exchangeQueueRefusalDelayedCancelContext) Value(any) any {
	return nil
}

// The exact registered callback either stops before starting, or publishes the
// child's cancellation after the explicit gate. Both outcomes publish joined.
func (self *exchangeQueueRefusalDelayedCancelContext) AfterFunc(run func()) func() bool {
	if !self.registered.CompareAndSwap(false, true) {
		panic("shutdown-order control registered more than one native child")
	}
	stop := context.AfterFunc(self.Context, func() {
		<-self.release
		run()
		self.joinOnce.Do(func() { close(self.joined) })
	})
	return func() bool {
		stopped := stop()
		if stopped {
			self.stoppedBeforeParent.Store(self.Context.Err() == nil)
			self.joinOnce.Do(func() { close(self.joined) })
		}
		return stopped
	}
}

// Only an explicit positive extra-dial event or owner cancellation releases
// this control. There is no sleep, shortened reconnect clock or queue fill.
func TestExchangeQueueRefusalCleanupCancelsTransportBeforeSiblings(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		q := newExchangeQueueRefusalFixture(t, ExchangeOpTransport, exchangeQueueRefusalNoAck)
		f := q.origin
		parent := &exchangeQueueRefusalDelayedCancelContext{
			Context: f.ctx, release: make(chan struct{}), joined: make(chan struct{}),
		}
		q.transportParent = parent
		defer parent.releaseOnce.Do(func() { close(parent.release) })
		defer f.close()
		var joinCtx context.Context
		var joinCancel context.CancelFunc
		q.beforeRootCancel = func() {
			// The same ten-second cleanup guard covers both cancellation phases.
			// No product timeout or retry setting changes.
			joinCtx, joinCancel = context.WithTimeout(context.WithoutCancel(f.ctx), 10*time.Second)
			if q.connection == nil {
				return
			}
			select {
			case <-q.closeObserved:
			case <-joinCtx.Done():
				f.fail(fmt.Errorf("native close observer did not join before root cancellation: %w", joinCtx.Err()))
				return
			}
			proof := q.firstCloseProof.Load()
			if f.ctx.Err() != nil || q.transport.ctx.Err() == nil ||
				uint32((proof>>2)&3) != exchangeQueueRefusalPhaseTeardown || proof&16 == 0 ||
				proof&3 != 1 || proof>>5 != 0 {
				f.fail(fmt.Errorf("shutdown control did not observe the canceled child's teardown close while root remained live: proof=%d root=%v child=%v", proof, f.ctx.Err(), q.transport.ctx.Err()))
			}
		}
		q.afterRootCancel = func() {
			defer joinCancel()
			if q.transport != nil && q.transport.ctx.Err() == nil {
				select {
				case <-q.extraDial:
					state := q.extraDialState.Load()
					want := exchangeQueueRefusalDialObserved |
						exchangeQueueRefusalPhaseTeardown<<2 | exchangeQueueRefusalDialRootDone
					if state != want {
						f.fail(fmt.Errorf("controlled sibling interval did not reach root-done/caller-live extra dial: %s", exchangeQueueRefusalDialDescription(state)))
					}
				case <-joinCtx.Done():
					f.fail(fmt.Errorf("controlled live child did not reach an actual extra dial: %w", joinCtx.Err()))
				}
			}
			parent.releaseOnce.Do(func() { close(parent.release) })
			if parent.registered.Load() {
				select {
				case <-parent.joined:
				case <-joinCtx.Done():
					f.fail(fmt.Errorf("delayed child-cancellation publisher did not join: %w", joinCtx.Err()))
				}
			}
		}
		q.configure()
		f.afterJoined = func(*p2OriginState) {
			// No queue refusal is offered by this separate lifecycle control.
			// Every dial is retained: no canceled/teardown attempt is filtered.
			if q.dials.Load() != 1 || q.publications.Load() != 1 || q.refused.Load() != 0 {
				f.t.Errorf("joined shutdown-control dial/generation/refusal census=%d/%d/%d want=1/1/0; first={%s} extra={%s}",
					q.dials.Load(), q.publications.Load(), q.refused.Load(),
					exchangeQueueRefusalDialDescription(q.firstDialState.Load()),
					exchangeQueueRefusalDialDescription(q.extraDialState.Load()))
			}
			if !parent.stoppedBeforeParent.Load() {
				f.t.Error("native transport cancellation was not published before shared root cancellation")
			}
			if q.outcomeOverflow.Load() || q.socketTimeout.Load() {
				f.t.Error("shutdown control overflowed observations or expired a socket deadline")
			}
		}
		f.setup()
		if f.settings.ExchangeReconnectAfterErrorTimeout != time.Second ||
			f.settings.ExchangeBufferSize != exchangeQueueRefusalCapacity ||
			f.settings.WriteTimeout != 15*time.Second ||
			f.settings.ExchangeReadTimeout != 30*time.Second {
			t.Fatal("shutdown control changed a production retry, queue or deadline")
		}
		if !parent.registered.Load() {
			t.Fatal("actual constructor did not register the delayed native child")
		}
		q.connection = q.takePublished()
		q.requireSameGeneration()
		if q.transport.ctx.Err() != nil || f.ctx.Err() != nil {
			t.Fatal("shutdown control did not establish two live owners")
		}
	})
}
