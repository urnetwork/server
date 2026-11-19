package connect

// The drain of a block whose connect fronts its interfaces (D12 as amended by
// the review; LBALT-IMPL.md §2.8).
//
// After warpctl switches the interfaces' new flows to the next generation, it
// stops this one, and connect drains in this order:
//
//  1. the status latch reads draining, so /status is never ready again;
//  2. while every transport can still send, the HTTP front drains: the nginx
//     front stops admitting new HTTP work and marks its responses
//     Connection: close; the tcp servers turn keepalives off, which closes
//     idle connections, close their listeners and send each h2 connection one
//     GOAWAY; each h3 api connection is shut down gracefully, one GOAWAY each
//     and new streams refused. Requests in flight finish;
//  3. alongside, the exchange drains its residents;
//  4. hijacked H1 connections, connect's websockets and H1+ tunnels and the
//     forwarded websockets, are tracked until they close or the drain timeout
//     passes, as nginx's worker_shutdown_timeout;
//  5. the forced close, each step bounded by ForcedCloseTimeout, cuts what is
//     left, the h3 servers always before their transports;
//  6. the serving context is canceled last.
//
// A GOAWAY needs a transport that can send. alt once started a graceful
// HTTP/3 shutdown while its transports were being destroyed and got 700k
// goroutines (LBALT.md §3.3), which is why step 2 runs first and step 5 closes
// the h3 servers before any transport.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Drains connect in D12's order, then cancels. Without a front it is
// connect's drain as before: draining, the exchange drain, then cancel.
func drainConnect(
	warpStatus *router.WarpStatusState,
	publicFront *frontServer,
	exchangeDrain func(),
	cancel context.CancelFunc,
) {
	warpStatus.SetDrainingIfReady()
	if publicFront == nil {
		exchangeDrain()
		cancel()
		return
	}
	publicFront.beginDrain()
	exchangeDrained := make(chan struct{})
	go func() {
		defer close(exchangeDrained)
		server.HandleError(exchangeDrain)
	}()
	publicFront.waitDrained()
	<-exchangeDrained
	publicFront.close()
	cancel()
}

// Starts the front's drain, steps 2 of drainConnect. A front that never
// started, or that already drains or closes, is left as it is.
func (self *frontServer) beginDrain() {
	quicRuns, drainCtx, ok := func() ([]*frontQuicRun, context.Context, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.started || self.draining || self.closing {
			return nil, nil, false
		}
		self.draining = true
		drainTimeout := self.settings.Front.Http.DrainTimeout
		if 0 < drainTimeout {
			self.drainCtx, self.drainCancel = context.WithTimeout(self.ctx, drainTimeout)
		} else {
			self.drainCtx, self.drainCancel = context.WithCancel(self.ctx)
		}
		close(self.drainStarted)
		quicRuns := []*frontQuicRun{}
		for _, socket := range self.sockets {
			if quicRun, ok := socket.run.(*frontQuicRun); ok {
				quicRuns = append(quicRuns, quicRun)
			}
		}
		return quicRuns, self.drainCtx, true
	}()
	if !ok {
		return
	}
	self.front.BeginDrain()
	for _, httpServer := range self.httpServers {
		// idle keepalive connections close now, and each later http/1
		// response carries Connection: close
		httpServer.SetKeepAlivesEnabled(false)
		// the listeners close, which no new flow reaches after the switch,
		// and each h2 connection gets one GOAWAY; hijacked connections are the
		// front's to track
		go server.HandleError(func() {
			httpServer.Shutdown(drainCtx)
		})
	}
	for _, quicRun := range quicRuns {
		quicRun.beginDrain(drainCtx)
	}
	if glog.V(1) {
		openCount := self.openCount.Value()
		glog.Infof("[front]drain began with %d connections open\n", openCount)
	}
}

// Waits until no tcp connection or h3 api connection is open, or the drain
// timeout passes: step 4 of drainConnect. Returns at once without a drain.
func (self *frontServer) waitDrained() {
	drainCtx := func() context.Context {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.drainCtx
	}()
	if drainCtx == nil {
		return
	}
	for {
		openCount, changed := self.openCount.Get()
		if openCount == 0 {
			return
		}
		select {
		case <-changed:
		case <-drainCtx.Done():
			glog.Infof("[front]the drain timeout passed with %d connections open\n", openCount)
			return
		}
	}
}

// The forced close, step 5 of drainConnect: everything the front holds is
// released, each step bounded by ForcedCloseTimeout. Every supervisor
// releases its socket in its run's order, so an h3 server always closes
// before its transport. Safe to call more than once, and on a front that
// never started.
func (self *frontServer) close() {
	if !self.isStarted() {
		return
	}
	self.closeOnce.Do(func() {
		drainCancel := func() context.CancelFunc {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.closing = true
			return self.drainCancel
		}()
		forcedCloseTimeout := self.settings.Front.Http.ForcedCloseTimeout
		closeTime := time.Now()

		// the nginx front releases its pools, spill files and the upstream
		// sides of forwarded websockets alongside
		frontClosed := make(chan struct{})
		go func() {
			defer close(frontClosed)
			server.HandleError(func() {
				self.front.Close()
			})
		}()

		// every serve loop ends: QUIC accepts return, and the tcp servers
		// close their listeners and the connections they still own
		self.cancel()
		if drainCancel != nil {
			drainCancel()
		}
		for _, httpServer := range self.httpServers {
			httpServer.Close()
		}
		// the hijacked connections http.Server forgot
		self.closeTcpConns()

		supervised := make(chan struct{})
		go func() {
			defer close(supervised)
			self.supervisors.Wait()
		}()
		for _, step := range []struct {
			name string
			done chan struct{}
		}{
			{name: "sockets", done: supervised},
			{name: "nginx front", done: frontClosed},
		} {
			if forcedCloseTimeout <= 0 {
				break
			}
			select {
			case <-step.done:
			case <-time.After(forcedCloseTimeout - time.Since(closeTime)):
				glog.Infof("[front]the %s did not close within %s\n", step.name, forcedCloseTimeout)
			}
		}
	})
}
