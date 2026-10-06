// The connection side of provider intent (model/provider_intent_model.go). A
// connection declares intent with the H1 `X-UR-Provide-Intent: 1` header or the
// Auth frame's `provide_intent` field. Only such a connection runs the provider
// intent check: it starts or resumes the client's qualification when the
// connection opens, keeps the client's intent presence live, and observes the
// state the taskworker check writes. When the client is over the normal client
// limit and enforce_concurrent_clients is on, the connection closes with the
// client limit exceeded signal; while enforcement is dark the same decision is
// only counted (shadow mode).
//
// The client limit exceeded signal (the wire contract the client recognizes):
//   - H1 (WebSocket and H1+ framed): one 5 byte binary message, transport
//     control 3 followed by the big-endian uint32 close reason 1, then on
//     WebSocket a close frame with code 4001, then the socket closes.
//   - H3 (QUIC, also DNS-carried): the connection closes with application
//     error code 4001.
//
// The client backs off for at least 15 minutes on every transport.
package connect

import (
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// the H1 connect handshake header that declares provide intent
const HeaderProvideIntent = "X-UR-Provide-Intent"

// The transport control of the H1 close message. 1 and 2 are the speed test
// controls.
const transportControlClose connect.TransportControl = 3

// the close reason in the H1 close message
const closeReasonClientLimitExceeded uint32 = 1

// the WebSocket close code and QUIC application error code of the client
// limit exceeded close
const clientLimitExceededCloseCode = 4001

const clientLimitExceededCloseText = "client limit exceeded"

// the `transport` label values
const (
	connectTransportH1 = "h1"
	connectTransportH3 = "h3"
)

// the `cause` label values of urnetwork_connect_client_limit_kicks_total
const (
	clientLimitKickCauseProviderIntent        = "provider_intent"
	clientLimitKickCauseConcurrentClientLimit = "concurrent_client_limit"
)

var provideIntentConnectionsGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "provide_intent_connections",
		Help:      "Live connect transports by declared provide intent",
	},
	[]string{"transport", "intent"},
)

var clientLimitKicksCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "client_limit_kicks_total",
		Help:      "Client limit exceeded closes sent (enforced) or computed but not sent while enforce_concurrent_clients is false (shadow)",
	},
	[]string{"transport", "cause", "mode"},
)

var provideIntentCheckErrorsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "provide_intent_check_errors_total",
		Help:      "Provider intent check store errors; the check fails open and retries at its next observation",
	},
	[]string{"transport"},
)

// Every label value is exported from the start, so a rate is defined before
// the first event.
func init() {
	for _, transport := range []string{connectTransportH1, connectTransportH3} {
		for _, intent := range []string{"declared", "none"} {
			provideIntentConnectionsGauge.WithLabelValues(transport, intent)
		}
		for _, cause := range []string{clientLimitKickCauseProviderIntent, clientLimitKickCauseConcurrentClientLimit} {
			for _, mode := range []string{"enforced", "shadow"} {
				clientLimitKicksCounter.WithLabelValues(transport, cause, mode)
			}
		}
		provideIntentCheckErrorsCounter.WithLabelValues(transport)
	}
	prometheus.MustRegister(
		provideIntentConnectionsGauge,
		clientLimitKicksCounter,
		provideIntentCheckErrorsCounter,
	)
}

// Exactly `1` declares intent; anything else, or no header, is no intent.
func provideIntentFromHeader(header http.Header) bool {
	return strings.TrimSpace(header.Get(HeaderProvideIntent)) == "1"
}

// Holds the provide intent gauge of one live transport until the returned
// release.
func trackProvideIntentConnection(transport string, provideIntent bool) func() {
	intent := "none"
	if provideIntent {
		intent = "declared"
	}
	gauge := provideIntentConnectionsGauge.WithLabelValues(transport, intent)
	gauge.Inc()
	return gauge.Dec
}

// The H1 close message: transport control 3 and the close reason.
func clientLimitExceededControlMessage() []byte {
	message := make([]byte, 5)
	message[0] = transportControlClose
	binary.BigEndian.PutUint32(message[1:5], closeReasonClientLimitExceeded)
	return message
}

// Sends the client limit exceeded signal on an H1 connection. The caller must
// be the connection's only writer and closes the connection afterwards.
func writeConnectH1ClientLimitExceeded(ws connect.H1MessageConn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := ws.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, clientLimitExceededControlMessage()); err != nil {
		return err
	}
	if websocketConn, ok := ws.(*websocket.Conn); ok {
		// best effort: the control message above is the signal
		return websocketConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(clientLimitExceededCloseCode, clientLimitExceededCloseText),
			deadline,
		)
	}
	return nil
}

// Sends the client limit exceeded signal on an H3 connection, closing it.
func closeConnectQuicClientLimitExceeded(conn *quic.Conn) error {
	return conn.CloseWithError(quic.ApplicationErrorCode(clientLimitExceededCloseCode), clientLimitExceededCloseText)
}

// Counts a sent client limit exceeded close.
func countClientLimitKick(transport string, cause string) {
	clientLimitKicksCounter.WithLabelValues(transport, cause, "enforced").Inc()
}

// The timing of the provider intent check of one connection.
type ProviderIntentCheckSettings struct {
	// how often a connection observes its client's state and refreshes its
	// presence
	ObserveInterval time.Duration
	// how long the presence of a connection lasts without a refresh
	PresenceTimeout time.Duration
	// a check due longer ago than this means the client's check chain stopped
	// (it ran while the client was away), so the connection starts it again
	OverdueCheckTimeout time.Duration
	// a connection starts a stopped check chain at most once per interval
	MinScheduleInterval time.Duration
}

// Two minutes between observations bound the time to act on a decision of the
// taskworker check at one redis call per intent connection per interval, and
// the presence outlives two missed observations.
func DefaultProviderIntentCheckSettings() *ProviderIntentCheckSettings {
	return &ProviderIntentCheckSettings{
		ObserveInterval:     2 * time.Minute,
		PresenceTimeout:     6 * time.Minute,
		OverdueCheckTimeout: 5 * time.Minute,
		MinScheduleInterval: 15 * time.Minute,
	}
}

// The model calls of the provider intent check, a seam for deterministic tests.
type providerIntentStore interface {
	// starts or resumes the client's qualification for a new intent connection
	connect(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) model.ProviderIntentConnectResult
	// refreshes the client's presence and reads its live state
	observe(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) *model.ProviderIntentState
	// starts the client's check chain, or pulls a pending check forward
	scheduleCheck(ctx context.Context, networkId server.Id, clientId server.Id, runAt time.Time)
	// enforce_concurrent_clients
	enforced() bool
	now() time.Time
}

// The production store: redis state, the taskworker check and pro.yml.
type modelProviderIntentStore struct{}

// See ConnectProviderIntent.
func (self modelProviderIntentStore) connect(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) model.ProviderIntentConnectResult {
	return model.ConnectProviderIntent(ctx, networkId, clientId, presenceTimeout)
}

// See ObserveProviderIntent.
func (self modelProviderIntentStore) observe(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) *model.ProviderIntentState {
	return model.ObserveProviderIntent(ctx, networkId, clientId, presenceTimeout)
}

// See ScheduleProviderIntentCheck.
func (self modelProviderIntentStore) scheduleCheck(ctx context.Context, networkId server.Id, clientId server.Id, runAt time.Time) {
	controller.ScheduleProviderIntentCheck(ctx, networkId, clientId, runAt)
}

// pro.yml enforce_concurrent_clients, read live.
func (self modelProviderIntentStore) enforced() bool {
	return model.Pro().EnforceConcurrentClients
}

// The server clock.
func (self modelProviderIntentStore) now() time.Time {
	return server.NowUtc()
}

// The provider intent check of one connection that declared intent. Start runs
// on the handler goroutine before the connection serves; Run is the
// connection's observation loop, owned by the handler's workers. Not safe for
// concurrent use: Run continues on the state Start left.
type providerIntentCheck struct {
	ctx       context.Context
	networkId server.Id
	clientId  server.Id
	transport string
	settings  *ProviderIntentCheckSettings
	store     providerIntentStore

	// the check chain must be started (or pulled forward) by Run
	scheduleCheck bool
	// the last time this connection started the check chain
	scheduleTime time.Time
	// the current over limit decision was counted in shadow mode
	shadowCounted bool
}

// A check for one connection, on the production store. `ctx` is the
// connection's lifetime.
func newProviderIntentCheck(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	transport string,
	settings *ProviderIntentCheckSettings,
) *providerIntentCheck {
	return newProviderIntentCheckWithStore(ctx, networkId, clientId, transport, settings, modelProviderIntentStore{})
}

// A check on a given store, for tests.
func newProviderIntentCheckWithStore(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	transport string,
	settings *ProviderIntentCheckSettings,
	store providerIntentStore,
) *providerIntentCheck {
	if settings == nil || settings.ObserveInterval <= 0 {
		// unset settings would spin the observation loop
		settings = DefaultProviderIntentCheckSettings()
	}
	return &providerIntentCheck{
		ctx:       ctx,
		networkId: networkId,
		clientId:  clientId,
		transport: transport,
		settings:  settings,
		store:     store,
	}
}

// Runs one store call. A store error fails open (the connection keeps
// serving) and is counted; the next observation retries.
func (self *providerIntentCheck) guard(do func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			if server.IsDoneError(r) {
				return
			}
			provideIntentCheckErrorsCounter.WithLabelValues(self.transport).Inc()
			if glog.V(1) {
				glog.Infof("[pi]%s check error = %s\n", self.clientId, server.ErrorJsonNoStack(r))
			}
		}
	}()
	do()
	return true
}

// Starts or resumes the client's qualification for this connection. False
// means the client is over the limit with enforcement on: the caller closes
// the connection with the client limit exceeded signal.
func (self *providerIntentCheck) Start() bool {
	var result model.ProviderIntentConnectResult
	if !self.guard(func() {
		result = self.store.connect(self.ctx, self.networkId, self.clientId, self.settings.PresenceTimeout)
	}) {
		return true
	}
	if result.ScheduleCheck {
		self.scheduleCheck = true
	}
	return self.decide(result.State)
}

// Acts on an observed state: false when the connection must close. An over
// limit state is counted once per decision in shadow mode.
func (self *providerIntentCheck) decide(state *model.ProviderIntentState) bool {
	if state == nil || state.Status != model.ProviderIntentStatusOverLimit {
		self.shadowCounted = false
		return true
	}
	if self.store.enforced() {
		return false
	}
	if !self.shadowCounted {
		self.shadowCounted = true
		clientLimitKicksCounter.WithLabelValues(self.transport, clientLimitKickCauseProviderIntent, "shadow").Inc()
	}
	return true
}

// One observation: refreshes the presence and decides on the live state. A
// client whose record expired starts again; a check overdue past the stopped
// chain timeout starts the chain again.
func (self *providerIntentCheck) observe() bool {
	now := self.store.now()
	var state *model.ProviderIntentState
	if !self.guard(func() {
		state = self.store.observe(self.ctx, self.networkId, self.clientId, self.settings.PresenceTimeout)
	}) {
		return true
	}
	if state == nil {
		return self.Start()
	}
	if !now.Before(state.CheckTime.Add(self.settings.OverdueCheckTimeout)) {
		self.scheduleCheck = true
	}
	return self.decide(state)
}

// Starts the check chain when asked, at most once per interval.
func (self *providerIntentCheck) scheduleCheckIfNeeded() {
	if !self.scheduleCheck {
		return
	}
	now := self.store.now()
	if !self.scheduleTime.IsZero() && now.Before(self.scheduleTime.Add(self.settings.MinScheduleInterval)) {
		return
	}
	if self.guard(func() {
		self.store.scheduleCheck(self.ctx, self.networkId, self.clientId, now)
	}) {
		self.scheduleCheck = false
		self.scheduleTime = now
	}
}

// The observation loop. Returns false when the client went over the limit with
// enforcement on (the caller closes with the client limit exceeded signal), and
// true when the connection ended.
func (self *providerIntentCheck) Run() bool {
	for {
		self.scheduleCheckIfNeeded()
		select {
		case <-self.ctx.Done():
			return true
		case <-time.After(self.settings.ObserveInterval):
		}
		if !self.observe() {
			return false
		}
	}
}
