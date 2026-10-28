// Client close (CLIENTCLOSE.md). Residents deliver their clients' close requests;
// the exchange closes transports of clients removed through the api.
package connect

import (
	"context"
	"math"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

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
	ctx       context.Context
	cancel    context.CancelFunc
	clientId  server.Id
	batchSize int
	send      func(*protocol.Frame)
	kick      chan struct{}
	done      chan struct{}
}

// The shared control sender can coalesce 3,300 payload bytes and 16 frame
// wrappers into one carrier. Its ordinary 4 KiB envelope is a compatibility
// floor: shrinking this listener's batches cannot constrain unrelated frames.
const clientCloseMinCarrierSize = 4 * 1024

// Starts the resident-owned worker with a batch that fits both exchange and
// client framing. The send callback takes ownership of the frame bytes.
func newClientCloseListener(ctx context.Context, clientId server.Id, send func(*protocol.Frame), settings *ExchangeSettings) *clientCloseListener {
	cancelCtx, cancel := context.WithCancel(ctx)
	listener := &clientCloseListener{
		ctx:       cancelCtx,
		cancel:    cancel,
		clientId:  clientId,
		batchSize: clientCloseBatchSize(settings),
		send:      send,
		kick:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	go server.HandleError(listener.run)
	return listener
}

// Measures the larger legacy control envelope, including maximum sequence
// and RTT tags. Control packs have no contract or peer-encryption envelope.
// Halving oversized batches also bounds scratch space for custom settings.
func clientCloseBatchSize(settings *ExchangeSettings) int {
	batchSize := settings.ClientCloseBatchSize
	if batchSize <= 0 {
		batchSize = DefaultExchangeSettings().ClientCloseBatchSize
	}
	messageLimit := min(settings.FramerSettings.MaxMessageLen, int(connect.DefaultClientSettings().MinimumMessageLenLimit()))
	if messageLimit < clientCloseMinCarrierSize {
		// Keep the Redis requests for another listener or the expiry backstop;
		// taking them cannot produce a safe carrier on this configuration.
		return 0
	}
	id := server.Id{}
	request := &protocol.CloseSequences{ContractIds: [][]byte{id.Bytes()}}
	batchSize = min(batchSize, messageLimit/proto.Size(request))
	for 0 < batchSize {
		requests := &protocol.CloseSequences{ContractIds: make([][]byte, batchSize)}
		for i := range requests.ContractIds {
			requests.ContractIds[i] = id.Bytes()
		}
		frame := connect.RequireToFrameWithDefaultProtocolVersion(requests)
		packFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.Pack{
			MessageId:      id.Bytes(),
			SequenceId:     id.Bytes(),
			SequenceNumber: math.MaxUint64,
			Head:           true,
			Frames:         []*protocol.Frame{frame},
			Tag:            &protocol.Tag{SendTime: math.MaxUint64},
		})
		carrierSize := proto.Size(&protocol.TransferFrame{
			TransferPath: &protocol.TransferPath{SourceId: connect.ControlId.Bytes(), DestinationId: id.Bytes()},
			Frame:        packFrame,
		})
		connect.MessagePoolReturn(packFrame.MessageBytes)
		connect.MessagePoolReturn(frame.MessageBytes)
		if carrierSize <= messageLimit {
			return batchSize
		}
		batchSize /= 2
	}
	return 0
}

// Coalesces hints without blocking the shared key-event dispatcher.
func (self *clientCloseListener) Kick() {
	select {
	case self.kick <- struct{}{}:
	default:
	}
}

// Serializes pending-set reads and blocking control sends for this resident.
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
	if self.batchSize <= 0 {
		return
	}
	for {
		if self.ctx.Err() != nil {
			return
		}
		var requests *model.ClientCloseRequests
		server.HandleError(func() {
			requests = model.TakeClientCloseRequests(self.ctx, self.clientId, self.batchSize)
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
		if len(requests.ContractIds)+len(requests.PeerIds) < self.batchSize {
			return
		}
	}
}

// Joins the worker after withdrawing its Redis and send context.
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
