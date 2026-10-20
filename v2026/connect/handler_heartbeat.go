// A connect process's handler heartbeat, and its decision to shut the process
// down when handler expiry retired the handler.
//
// Handler expiry (model.CloseExpiredNetworkClientHandlers) deletes a handler
// whose heartbeat stopped and retires its connection rows. The process cannot
// restore those rows: nothing marks a retired row connected again while its
// transport lives (the announcement's per-connection status check is off, and
// the reconcile only retires rows, transport_announce_reconcile.go). Writing
// the handler row again would leave every provider it holds retired while
// connected, unselectable until it happened to reconnect, so a retired process
// still restarts for its providers to register again.
//
// Restarting at once is what turned the 2026-10-19 pooler livelock into a
// provider outage: the heartbeat path failed for every process together,
// expiry deleted every handler, each process exited on its next heartbeat
// that got through, and the replacements could not pass their one-shot
// readiness while the pools were starved, so warp redeployed them in a loop.
// So a retired process keeps its transports until the database can take its
// restart and its providers' reconnects: a separate read must find the row
// still gone in RetirementConfirmations consecutive rounds, each answered
// within ConfirmationTimeout, and a round the database does not answer starts
// the count again. It then shuts down after a random delay within
// ShutdownJitter, so a fleet retired together does not restart together.
//
// Not safe for concurrent use: one loop owns the state.
package connect

import (
	"context"
	mathrand "math/rand"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Events of a handler's retirement, as the metric labels them.
const (
	// a heartbeat found the handler row gone
	handlerRetirementDetected = "detected"
	// a confirmation read the database did not answer; the count starts again
	handlerRetirementUnanswered = "unanswered"
	// the handler row was back; heartbeats resumed
	handlerRetirementRestored = "restored"
	// confirmed: the process shuts down
	handlerRetirementShutdown = "shutdown"
)

var handlerRetirementCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "handler_retirement_total",
		Help:      "Retirement of this process's handler by handler expiry, by event: detected, a heartbeat found the handler row gone; unanswered, a confirmation read the database did not answer, which starts the confirmation again; restored, the row was back and heartbeats resumed; shutdown, confirmed, and the process shuts down",
	},
	[]string{"event"},
)

// Registers the retirement metric with the default registry, every event at
// zero.
func init() {
	prometheus.MustRegister(handlerRetirementCounter)
	for _, event := range []string{
		handlerRetirementDetected,
		handlerRetirementUnanswered,
		handlerRetirementRestored,
		handlerRetirementShutdown,
	} {
		handlerRetirementCounter.WithLabelValues(event)
	}
}

// The pace of the heartbeat and of the confirmation of a retirement.
type HandlerHeartbeatSettings struct {
	// between heartbeats, and between confirmation reads once retired
	HeartbeatInterval time.Duration
	// consecutive answered reads that must find the handler row gone before
	// the process shuts down
	RetirementConfirmations int
	// bounds each confirmation read
	ConfirmationTimeout time.Duration
	// the most the shutdown waits, at random, after the last confirmation
	ShutdownJitter time.Duration
}

// A heartbeat at least twice per heartbeat timeout; three confirmations,
// about 15 s of a database that answers; a shutdown spread over one heartbeat
// timeout.
func DefaultHandlerHeartbeatSettings() *HandlerHeartbeatSettings {
	return &HandlerHeartbeatSettings{
		HeartbeatInterval:       min(5*time.Second, model.NetworkClientHandlerHeartbeatTimeout/2),
		RetirementConfirmations: 3,
		ConfirmationTimeout:     2 * time.Second,
		ShutdownJitter:          model.NetworkClientHandlerHeartbeatTimeout,
	}
}

// The heartbeat of one handler. See the file header.
type handlerHeartbeat struct {
	ctx        context.Context
	handlerId  server.Id
	settings   *HandlerHeartbeatSettings
	heartbeat  func(ctx context.Context, handlerId server.Id) error
	readExists func(ctx context.Context, handlerId server.Id) (bool, error)
	jitter     func(maxDelay time.Duration) time.Duration
	shutdown   func()

	retired           bool
	confirmationCount int
	// zero until the last confirmation picks it
	shutdownTime time.Time
	// set by the round that confirms the retirement and shuts the process down
	confirmed bool
}

// Starts the heartbeat loop for handlerId, which runs until ctx ends. shutdown
// is called by the round that confirms a retirement, and when the loop ends;
// the connect router passes its cancel, so either ends the process.
func newHandlerHeartbeat(
	ctx context.Context,
	handlerId server.Id,
	settings *HandlerHeartbeatSettings,
	shutdown func(),
) *handlerHeartbeat {
	heartbeat := newHandlerHeartbeatWithStore(
		ctx,
		handlerId,
		settings,
		model.HeartbeatNetworkClientHandler,
		model.NetworkClientHandlerExists,
		func(maxDelay time.Duration) time.Duration {
			if maxDelay <= 0 {
				return 0
			}
			return time.Duration(mathrand.Int63n(int64(maxDelay)))
		},
		shutdown,
	)
	go server.HandleError(heartbeat.run)
	return heartbeat
}

// The heartbeat state over an explicit store, without its loop. Tests drive
// its rounds directly.
func newHandlerHeartbeatWithStore(
	ctx context.Context,
	handlerId server.Id,
	settings *HandlerHeartbeatSettings,
	heartbeat func(ctx context.Context, handlerId server.Id) error,
	readExists func(ctx context.Context, handlerId server.Id) (bool, error),
	jitter func(maxDelay time.Duration) time.Duration,
	shutdown func(),
) *handlerHeartbeat {
	if settings == nil || settings.HeartbeatInterval <= 0 || settings.RetirementConfirmations <= 0 || settings.ConfirmationTimeout <= 0 {
		// unset settings would spin the loop, or shut down on no evidence
		settings = DefaultHandlerHeartbeatSettings()
	}
	return &handlerHeartbeat{
		ctx:        ctx,
		handlerId:  handlerId,
		settings:   settings,
		heartbeat:  heartbeat,
		readExists: readExists,
		jitter:     jitter,
		shutdown:   shutdown,
	}
}

// One round every interval until ctx ends. Whatever ends the loop shuts the
// process down: a process whose heartbeat stopped would otherwise keep serving
// under a handler that expiry retires.
func (self *handlerHeartbeat) run() {
	defer self.shutdown()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.HeartbeatInterval):
		}
		self.round(time.Now())
	}
}

// One heartbeat, or once retired one confirmation read, at now. The round that
// confirms the retirement shuts the process down; rounds after it do nothing.
func (self *handlerHeartbeat) round(now time.Time) {
	if self.confirmed {
		return
	}
	if !self.retired {
		var err error
		if recovered := server.HandleError(func() {
			err = self.heartbeat(self.ctx, self.handlerId)
		}); recovered != nil {
			// a raised failure is transient, such as database load: try again
			return
		}
		if err == nil {
			return
		}
		self.retired = true
		self.confirmationCount = 0
		self.shutdownTime = time.Time{}
		handlerRetirementCounter.WithLabelValues(handlerRetirementDetected).Inc()
		glog.Infof("[c]handler retired by handler expiry; holding transports until the database confirms (%s)\n", err)
		return
	}

	exists, err := func() (bool, error) {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.ConfirmationTimeout)
		defer readCancel()
		return self.readExists(readCtx, self.handlerId)
	}()
	if err != nil {
		// the database cannot take the restart yet
		self.confirmationCount = 0
		self.shutdownTime = time.Time{}
		handlerRetirementCounter.WithLabelValues(handlerRetirementUnanswered).Inc()
		return
	}
	if exists {
		self.retired = false
		self.confirmationCount = 0
		self.shutdownTime = time.Time{}
		handlerRetirementCounter.WithLabelValues(handlerRetirementRestored).Inc()
		glog.Infof("[c]handler row is back; heartbeats resume\n")
		return
	}
	self.confirmationCount += 1
	if self.confirmationCount < self.settings.RetirementConfirmations {
		return
	}
	if self.shutdownTime.IsZero() {
		self.shutdownTime = now.Add(self.jitter(self.settings.ShutdownJitter))
	}
	if now.Before(self.shutdownTime) {
		return
	}
	self.confirmed = true
	handlerRetirementCounter.WithLabelValues(handlerRetirementShutdown).Inc()
	glog.Infof("[c]handler retirement confirmed by %d answered reads; shutting down so the providers register again\n", self.confirmationCount)
	self.shutdown()
}
