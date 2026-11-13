// In-process delivery of a resident's forward whose destination's resident
// lives in the same connect process (CPUOPT4, extending CPUOPT3 D1 from
// transports to forwards). ResidentForward.Run resolves the destination's
// resident record and, when it names this process's current resident and the
// switch allows this block, attaches the forward to that resident directly
// instead of dialing an exchange socket back into its own process. The rest of
// the loop is unchanged: the demand-driven lookup, the reconnect pacing, and
// the forward's idle lifetime, which payload admission alone owns.
//
// An attach does what the accept path does after it reads a forward header, in
// the same order: it refuses a generation that is not current or a resident
// that is done, counts as an accepted exchange connection, opens the
// resident's forward queue (Resident.AddForward, the queue and consumer an
// accepted forward socket delivers into), and ends when the resident ends. The
// link is the transport link of resident_local_transport.go with one pump:
// from the forward's send queue, starting with the loop's pending message,
// into the destination's forward queue, on the forward's Run goroutine. The
// pump keeps the exchange socket writer's contract (FIFO batches within
// ExchangeWriteBatchCount and ExchangeWriteBatchByteCount, the framer's limits
// over the whole batch, zero-length messages as pings, a WriteTimeout budget
// per batch from its start with a timer only while a delivery blocks, and
// resident activity per delivered batch). It releases each message's forward
// output charge when it delivers or returns it, as the socket path releases it
// at its admission. Every message it took and did not deliver is returned
// once and counted at the local_forward_send boundary.
//
// The bounds of today's forward socket, and what holds them here:
//   - the dialer's 15 s admission of each message into its connection's send
//     queue (drop and continue): that queue does not exist. The pump blocks
//     only on the destination's forward queue, within its batch's 15 s, and
//     no producer waits on it: production forward admission never blocks
//     (ForwardTimeout 0).
//   - the dialer's write deadline, 15 s per batch, which ends the socket into
//     the loop's reconnect: the batch's 15 s from its start, a stall ending
//     the link with write_timeout into the same loop and the same reconnect
//     wait. A healthy batch keeps its whole budget.
//   - the accept side's reader, which waits without a deadline on a full
//     forward queue so the dialer's write deadline fires: the pump waits on
//     that queue under the batch deadline that stands in for the write
//     deadline.
//   - the destination's forward consumer, Client.ForwardWithTimeout within
//     WriteTimeout per message: unchanged, the same queue and consumer.
//   - read deadlines (30 s), pings (10 s) and TCP keepalive at both ends: not
//     needed, since both ends are contexts of this process. The forward's
//     end, or the destination resident's, ends the link at once, where a
//     socket needed up to a read deadline.
//   - header deadlines and the accept side's resident wait: no handshake. The
//     generation is checked under the exchange's stateLock, and a generation
//     that is not current dials, so the accept path waits or refuses as
//     today.
//   - the destination resident's ExchangeResidentTtl, which an open forward
//     socket's pings keep from lapsing: an attached link is activity
//     (Resident.CancelIfIdle), and a wedged link detaches within WriteTimeout.
//   - the forward's ForwardIdleTimeout: unchanged, since the link never
//     touches the forward's activity, as exchange keepalives never did.
//   - MaxMessageLen and the uint16 frame length: the same checks, failing the
//     whole batch with max_message_len.
//   - the lookup's demand rules, contract enforcement, the per-resident
//     forward limit and revocation: untouched; they sit before the forward's
//     send queue or in the loop.
//
// The switch is the code ceiling EnableLocalForward and the Redis move
// control's `localForward`, read every LocalTransportControlRefreshInterval
// beside `localTransport` and never wider than the ceiling;
// LocalForwardDefault ships off. When the control turns this block off, new
// attaches dial, and live links detach LocalForwardDisableDetachRate per
// second, each dialing its destination with its next message.
package connect

import (
	"context"
	"sync"

	"github.com/urnetwork/server/v2026"
)

// Attaches the forward described by header to this process's resident of its
// destination, or returns nil for the caller to dial the resident as before:
// the switch is off, the record's generation is not this process's current
// resident, or that resident is done. A draining exchange still attaches to a
// present resident, as its accept path accepts one. A returned link may
// already have ended, and its owner runs it with ResidentForward.runLocal.
func (self *Exchange) attachLocalForward(ctx context.Context, header ExchangeHeader) *localTransportLink {
	localForward := self.localForward
	if localForward == nil || !localForward.allows() {
		return nil
	}
	resident := self.localResident(header.ClientId)
	if resident == nil || resident.residentId != header.ResidentId || resident.IsDone() {
		return nil
	}
	if beforeAttach := self.beforeLocalForwardAttachForTest; beforeAttach != nil {
		beforeAttach(resident)
	}
	return self.attachLocalTransportRoutes(ctx, localForward, resident, header, func() (chan []byte, chan []byte, func(), error) {
		forward, closeForward, err := resident.AddForward()
		return nil, forward, closeForward, err
	})
}

// Runs an attached link until it detaches, and returns why. The link's one
// pump runs here, on the Run goroutine: it delivers the loop's pending message
// first, then the forward's send queue. The pending message stays the loop's
// when the link ended before taking it, so the next link or connection
// carries it first. A raise detaches the link with raised after the pump
// returned what it held, and the loop continues as after a socket's end. On
// return the link is detached.
func (self *ResidentForward) runLocal(link *localTransportLink, pending *[]byte, hasPending *bool) (reason localTransportDetachReason) {
	// a forward link has no resident pump to join
	var pumps sync.WaitGroup
	defer func() {
		reason = link.close(&pumps)
	}()
	pump := &localTransportPump{
		link:         link,
		direction:    localTransportDirectionForward,
		source:       self.send,
		destination:  link.routeReceive,
		dropBoundary: receiveQueueBoundaryLocalForwardSend,
		sourceClosed: func() {
			// a closed send queue ends the forward, as it ends its socket's
			// writer
			self.cancel()
			link.detach(localTransportDetachForwardDone)
		},
		ledger:      self.exchange.residentPayloadLedger(),
		ledgerOwner: self.clientId[15],
	}
	server.HandleError(func() {
		if *hasPending {
			select {
			case <-link.ctx.Done():
				return
			default:
			}
			message := *pending
			*pending, *hasPending = nil, false
			if !pump.deliverBatch(message) {
				return
			}
		}
		pump.run()
	}, func(error) {
		link.detach(localTransportDetachRaised)
	})
	return
}
