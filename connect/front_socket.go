package connect

// Supervision of the front's public sockets and of its udp groups' transports
// (D13 and D22, as decided 2026-11-18 with (b) from 2026-11-19; LBALT-IMPL.md
// §2.8, §8).
//
// Each socket has its own supervisor goroutine, so losing one touches no
// sibling. A socket is lost when its serve returns while the front is still
// serving: a udp socket whose reads fail leaves its group's transport, and a
// tcp listener whose accept fails ends its Serve. The supervisor then releases
// the run, which reads as missing at once, and re-creates it: at once after a
// healthy run, and otherwise after a wait that doubles from
// SocketRestartInitialDelay up to SocketRestartMaxDelay, so a socket that keeps
// failing can't spin. A re-created udp socket joins its group's transport. It
// changes the size of its SO_REUSEPORT group twice, which moves most of the
// group's flows between its sockets, but every socket of the group feeds the
// one transport, so the moved flows keep their connections (D22 (b)). The
// socket reads as up again once it serves.
//
// Each udp group's transport has its own supervisor too. A transport that
// ends while the front serves, as whodis's does when its translation ends, is
// replaced at once after a healthy run, else after the same backoff: the new
// one is installed, so re-created sockets join it, and the old one is closed in
// the drain's order, which closes its sockets, to be re-created into the new
// one. Its connections end with it.
//
// Nothing here ends the process: a panic in a run ends only that run. Once
// the drain begins or the front closes, nothing is re-created or replaced.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// One incarnation of a bound public socket, from its bind to its release.
type frontSocketRun interface {
	// serves until the socket is lost or the front stops it
	serve() error
	// starts the run's part of the drain (D12) while it can still send
	beginDrain(drainCtx context.Context)
	// releases everything the run holds, in the order the drain needs
	close()
}

// Serves one socket for the front's life, re-creating it whenever it is lost.
// run is its first incarnation, already up.
func (self *frontServer) superviseSocket(socket *frontSocket, run frontSocketRun) {
	defer self.supervisors.Done()
	// the wait before the next re-creation: none until a re-creation fails or
	// a re-created socket is lost before its run counted as healthy
	wait := time.Duration(0)
	bindErrText := ""
	for {
		serveStartTime := time.Now()
		err := runFrontSocketStep(run.serve)
		if self.settings.socketServeEndObserverForTest != nil {
			self.settings.socketServeEndObserverForTest(socket.key)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.setSocketRunWithLock(socket, nil)
		}()
		run.close()
		if self.socketsStopped() {
			return
		}
		healthyRunTimeout := self.settings.SocketHealthyRunTimeout
		if 0 < healthyRunTimeout && healthyRunTimeout <= time.Since(serveStartTime) {
			wait = 0
		}
		if err == nil {
			err = fmt.Errorf("the socket stopped serving")
		}
		frontSocketRestartsCounter.WithLabelValues(socket.key.labels()...).Inc()
		glog.Errorf("[front]socket %s lost, re-creating: %s\n", socket.key, err)

		for {
			if self.settings.restartWaitObserverForTest != nil {
				self.settings.restartWaitObserverForTest(socket.key, wait)
			}
			if 0 < wait {
				select {
				case <-self.ctx.Done():
					return
				case <-self.drainStarted:
					return
				case <-time.After(wait):
				}
			}
			if self.socketsStopped() {
				return
			}
			wait = self.settings.nextRestartWait(wait)
			var openErr error
			run, openErr = self.openSocket(socket)
			if openErr == nil {
				break
			}
			frontSocketBindFailuresCounter.WithLabelValues(socket.key.labels()...).Inc()
			// each new reason once, not every retry
			if bindErrText != openErr.Error() {
				bindErrText = openErr.Error()
				glog.Errorf("[front]socket %s did not bind again: %s\n", socket.key, openErr)
			}
		}
		bindErrText = ""
		installed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.socketsStoppedWithLock() {
				return false
			}
			self.setSocketRunWithLock(socket, run)
			return true
		}()
		if !installed {
			run.close()
			return
		}
		glog.Infof("[front]socket %s serving again\n", socket.key)
	}
}

// Serves one udp group's transport for the front's life, replacing it whenever
// it ends while the front serves. run is its first incarnation, already
// installed. The transport is closed here, in the drain's order, once the
// front stops: its accept loop returns when the serving context ends.
func (self *frontServer) superviseQuicTransport(quicGroup *frontQuicGroup, run *frontQuicRun) {
	defer self.supervisors.Done()
	// the wait before the next replacement, as a socket's
	wait := time.Duration(0)
	openErrText := ""
	for {
		serveStartTime := time.Now()
		err := runFrontSocketStep(run.serve)
		if self.settings.quicTransportServeEndObserverForTest != nil {
			self.settings.quicTransportServeEndObserverForTest(quicGroup.key)
		}
		if self.socketsStopped() {
			run.close()
			return
		}
		healthyRunTimeout := self.settings.SocketHealthyRunTimeout
		if 0 < healthyRunTimeout && healthyRunTimeout <= time.Since(serveStartTime) {
			wait = 0
		}
		if err == nil {
			err = fmt.Errorf("the transport stopped serving")
		}
		glog.Errorf("[front]quic transport %s lost, replacing it: %s\n", quicGroup, err)

		var next *frontQuicRun
		for {
			if 0 < wait {
				select {
				case <-self.ctx.Done():
					run.close()
					return
				case <-self.drainStarted:
					run.close()
					return
				case <-time.After(wait):
				}
			}
			if self.socketsStopped() {
				run.close()
				return
			}
			wait = self.settings.nextRestartWait(wait)
			var openErr error
			next, openErr = self.openQuicTransport(quicGroup)
			if openErr == nil {
				break
			}
			// each new reason once, not every retry
			if openErrText != openErr.Error() {
				openErrText = openErr.Error()
				glog.Errorf("[front]quic transport %s did not open again: %s\n", quicGroup, openErr)
			}
		}
		openErrText = ""
		installed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.socketsStoppedWithLock() {
				return false
			}
			quicGroup.run = next
			return true
		}()
		if !installed {
			next.close()
			run.close()
			return
		}
		// the old transport's sockets are closed with it, and re-created into
		// the new one
		run.close()
		run = next
		glog.Infof("[front]quic transport %s serving again\n", quicGroup)
	}
}

// Runs one step of a socket's supervision, reading a panic as what ended it,
// so nothing a socket does reaches its siblings or the process. HandleError
// logs the panic's stack.
func runFrontSocketStep(step func() error) (err error) {
	if r := server.HandleError(func() {
		err = step()
	}); r != nil {
		return fmt.Errorf("panic: %v", r)
	}
	return err
}
