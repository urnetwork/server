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
//     Connection: close; each tcp server's Shutdown, off the drain's
//     goroutine, turns keepalives off, closes its listeners, sends each h2
//     connection one GOAWAY and closes idle connections; each h3 api
//     connection is shut down gracefully, one GOAWAY each and new streams
//     refused. Requests in flight finish;
//  3. alongside, the exchange drains its residents;
//  4. hijacked H1 connections, connect's websockets and H1+ tunnels and the
//     forwarded websockets, are tracked until they close or the drain's
//     deadline passes: the drain timeout, as nginx's worker_shutdown_timeout,
//     less DrainStopReserve, since warpctl's stop grace is the same 60 m and
//     the steps below must finish inside it;
//  5. the forced close, bounded by ForcedCloseTimeout, cuts what is left: the
//     tcp sockets before anything TLS would send on them, and the h3 servers
//     always before their transports;
//  6. the serving context is canceled last.
//
// A GOAWAY needs a transport that can send. alt once started a graceful
// HTTP/3 shutdown while its transports were being destroyed and got 700k
// goroutines (LBALT.md §3.3), which is why step 2 runs first and step 5 closes
// the h3 servers before any transport. The accept loop that spun there spins
// for any connection that closes during a graceful shutdown, a client closing
// after its GOAWAY included, so the vendored http3 ends it instead, one GOAWAY
// per connection (quic-go's URNETWORK.md, ../quic-go).

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
	deadline := publicFront.waitDrained()
	select {
	case <-exchangeDrained:
	case <-deadline:
	}
	publicFront.close()
	cancel()
	// Cancellation releases resident work still in progress at the deadline.
	<-exchangeDrained
}

// Starts the front's drain, step 2 of drainConnect. A front that never
// started, or that already drains or closes, is left as it is.
//
// The drain's context outlives the serving context: the graceful h3
// shutdowns it bounds must not end on their own while close tears the
// sockets down, since only close knows to close each h3 server before its
// transport. close releases it last.
func (self *frontServer) beginDrain() {
	quicRuns, drainCtx, ok := func() ([]*frontQuicRun, context.Context, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.started || self.draining || self.closing {
			return nil, nil, false
		}
		self.draining = true
		drainTimeout := self.settings.drainDeadlineTimeout()
		drainParentCtx := context.WithoutCancel(self.ctx)
		if 0 < drainTimeout {
			self.drainCtx, self.drainCancel = context.WithTimeout(drainParentCtx, drainTimeout)
		} else {
			self.drainCtx, self.drainCancel = context.WithCancel(drainParentCtx)
		}
		close(self.drainStarted)
		quicRuns := []*frontQuicRun{}
		for _, quicGroup := range self.quicGroups {
			if quicGroup.run != nil {
				quicRuns = append(quicRuns, quicGroup.run)
			}
		}
		return quicRuns, self.drainCtx, true
	}()
	if !ok {
		return
	}
	self.front.BeginDrain()
	for _, httpServer := range self.httpServers {
		// Shutdown turns keepalives off, so each later http/1 response
		// carries Connection: close; closes the listeners, which no new flow
		// reaches after the switch; sends each h2 connection one GOAWAY; and
		// closes idle keepalive connections. It runs off the drain's
		// goroutine, since closing an idle TLS connection sends close_notify
		// under the server's lock, which can wait on a client that stopped
		// reading: the exchange drain and the h3 GOAWAYs below must not wait
		// behind it. Hijacked connections are the front's to track.
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
// timeout passes. The same deadline bounds the subsequent resident wait.
func (self *frontServer) waitDrained() <-chan struct{} {
	drainCtx := func() context.Context {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.drainCtx
	}()
	if drainCtx == nil {
		return nil
	}
	for {
		openCount, changed := self.openCount.Get()
		if openCount == 0 {
			return drainCtx.Done()
		}
		select {
		case <-changed:
		case <-drainCtx.Done():
			glog.Infof("[front]the drain timeout passed with %d connections open\n", openCount)
			return drainCtx.Done()
		}
	}
}

// The forced close, step 5 of drainConnect: everything the front holds is
// released, each step bounded by ForcedCloseTimeout. Every udp group's
// supervisor closes its transport in the run's order, so an h3 server always
// closes before its transport and the transport before the group's sockets,
// and every socket's supervisor then releases its socket. Safe to call more
// than once, and on a front that never started.
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

		// every serve loop ends: QUIC accepts return, and each transport's
		// supervisor closes it
		self.cancel()
		// every tcp connection's raw socket closes first, hijacked or not.
		// http.Server.Close below closes each connection it still owns one
		// after another under the server's lock, and over TLS each close
		// first sends close_notify, which could wait on a client that stopped
		// reading, or behind a blocked write that holds TLS's write lock,
		// such as the answer to a client's KeyUpdate request, until that
		// write's send_timeout (frontTrackedConn), by default as long as
		// this close's whole bound. A drain's Shutdown stuck the same way
		// closing an idle connection holds the server's lock, which Close
		// needs, until this cut.
		self.closeTcpConns()
		// the tcp servers close their listeners and forget their connections
		for _, httpServer := range self.httpServers {
			httpServer.Close()
		}

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
			// a step already done is never reported, even once the budget
			// is spent and the timer below would be due at once
			select {
			case <-step.done:
				continue
			default:
			}
			select {
			case <-step.done:
			case <-time.After(max(0, forcedCloseTimeout-time.Since(closeTime))):
				glog.Infof("[front]the %s did not close within %s\n", step.name, forcedCloseTimeout)
				if self.settings.forcedCloseReportObserverForTest != nil {
					self.settings.forcedCloseReportObserverForTest(step.name)
				}
			}
		}
		// the drain's shutdowns are all past their transports' close
		if drainCancel != nil {
			drainCancel()
		}
	})
}
