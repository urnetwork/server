package connect

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Client close (CLIENTCLOSE.md). The resident's listener delivers the close
// requests of its client, and the exchange closes the transports of a client
// removed through the api.

var (
	clientCloseSequencesSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_client_close_sequences_sent_total",
		Help: "CloseSequences frames residents sent to their clients",
	})
	clientCloseRequestsSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_client_close_requests_sent_total",
		Help: "close requests residents sent to their clients, by kind (contract, peer)",
	}, []string{"kind"})
	clientRemovedCloses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_client_removed_closes_total",
		Help: "transports closed because their client was removed through the api",
	}, []string{"kind"})
)

func init() {
	prometheus.MustRegister(
		clientCloseSequencesSent,
		clientCloseRequestsSent,
		clientRemovedCloses,
	)
}

// Delivers a resident client's close requests: each kick takes the pending
// requests and sends them as `CloseSequences`. Kicks coalesce while a
// delivery runs, so the key-event dispatch never blocks on it.
type clientCloseListener struct {
	ctx      context.Context
	cancel   context.CancelFunc
	clientId server.Id
	send     func(*protocol.Frame)
	kick     chan struct{}
	done     chan struct{}
}

func newClientCloseListener(ctx context.Context, clientId server.Id, send func(*protocol.Frame)) *clientCloseListener {
	cancelCtx, cancel := context.WithCancel(ctx)
	listener := &clientCloseListener{
		ctx:      cancelCtx,
		cancel:   cancel,
		clientId: clientId,
		send:     send,
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go server.HandleError(listener.run)
	return listener
}

func (self *clientCloseListener) Kick() {
	select {
	case self.kick <- struct{}{}:
	default:
	}
}

func (self *clientCloseListener) run() {
	defer close(self.done)
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.kick:
		}
		self.deliver()
	}
}

// Takes and sends until a take comes back short of the limit. A taken request
// whose send fails is gone; its contract closes at its deadline.
func (self *clientCloseListener) deliver() {
	for {
		var requests *model.ClientCloseRequests
		server.HandleError(func() {
			requests = model.TakeClientCloseRequests(self.ctx, self.clientId, model.ClientCloseTakeLimit)
		})
		if requests == nil || requests.Empty() {
			return
		}
		closeSequences := &protocol.CloseSequences{}
		for _, contractId := range requests.ContractIds {
			closeSequences.ContractIds = append(closeSequences.ContractIds, contractId.Bytes())
		}
		for _, peerId := range requests.PeerIds {
			closeSequences.PeerIds = append(closeSequences.PeerIds, peerId.Bytes())
		}
		self.send(connect.RequireToFrameWithDefaultProtocolVersion(closeSequences))
		clientCloseSequencesSent.Inc()
		clientCloseRequestsSent.WithLabelValues("contract").Add(float64(len(requests.ContractIds)))
		clientCloseRequestsSent.WithLabelValues("peer").Add(float64(len(requests.PeerIds)))
		if len(requests.ContractIds)+len(requests.PeerIds) < model.ClientCloseTakeLimit {
			return
		}
	}
}

func (self *clientCloseListener) CloseAndWait() {
	self.cancel()
	<-self.done
}

// Registers a transport's typed authorization close, which an api removal
// calls with the code the lease recheck would send. It must not block.
func (self *Exchange) registerConnectionAuthorizationClose(connectionId server.Id, closeAuthorization func(code int)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.connectionAuthorizationCloses == nil {
		self.connectionAuthorizationCloses = map[server.Id]func(int){}
	}
	self.connectionAuthorizationCloses[connectionId] = closeAuthorization
}

// The key-event dispatch of a client's removed flag. Only a process holding
// the client's transports does any work, off the dispatch.
func (self *Exchange) handleClientRemoved(clientId server.Id) {
	held := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return 0 < len(self.connections[clientId])
	}()
	if held {
		go server.HandleError(func() {
			self.closeRemovedClient(clientId)
		})
	}
}

// Closes the transports this process holds of a client removed through the api
// (CLIENTCLOSE.md section 4), each with the authorization close its lease
// recheck would send. A transport that registers after this is refused by its
// lease's final admission check.
//
// The resident is left to end at its idle ttl, as it did before the removal
// closes. Canceling it while peers still sent to the client left each of their
// forwards retrying the resident lookup every second until the forward idled
// out; at about 370 removals per second that saturated redis (2026-10-28).
func (self *Exchange) closeRemovedClient(clientId server.Id) {
	closeAuthorizations := []func(int){}
	handleCancels := []context.CancelFunc{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for connectionId, handleCancel := range self.connections[clientId] {
			if closeAuthorization, ok := self.connectionAuthorizationCloses[connectionId]; ok {
				closeAuthorizations = append(closeAuthorizations, closeAuthorization)
			} else {
				handleCancels = append(handleCancels, handleCancel)
			}
		}
	}()
	for _, closeAuthorization := range closeAuthorizations {
		closeAuthorization(session.AuthorizationRejectedCloseCode)
	}
	for _, handleCancel := range handleCancels {
		handleCancel()
	}
	clientRemovedCloses.WithLabelValues("transport").Add(float64(len(closeAuthorizations) + len(handleCancels)))
}
