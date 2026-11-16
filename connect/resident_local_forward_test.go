// Local forward tests (resident_local_forward.go). The pump fixture attaches a
// resident's forward to its destination resident through the production
// attach, over a forward queue the test owns in place of the resident's, so
// the test reads as the resident's forward consumer and every boundary is a
// barrier the test controls. Clocks are synctest virtual time wherever a bound
// is measured.
package connect

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Production bounds, with in-process forwards allowed for every block.
func localForwardTestSettings() *ExchangeSettings {
	settings := DefaultExchangeSettings()
	settings.EnableLocalForward = true
	settings.LocalForwardDefault = model.ConnectLocalTransportAll
	settings.EnableMove = false
	return settings
}

// Sets the control's `localForward`, and no other field.
func (self *localTransportTestControl) setForward(value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fields = map[string]string{model.ConnectMoveControlFieldLocalForward: value}
	self.err = nil
}

// One attach of the pump fixture: its link, the queue standing in for the
// destination resident's forward queue, and what its runLocal returned.
type localForwardTestLink struct {
	link *localTransportLink
	// the destination's forward queue: the test reads it as the resident's
	// forward consumer
	queue        chan []byte
	closeForward atomic.Int64
	done         chan localTransportDetachReason
	// the pending message runLocal left with the loop, if any; read after done
	pending    []byte
	hasPending bool
}

// A forward and its destination's resident in one exchange. The resident's
// internal client is real but never sees an attach: each attach publishes a
// test queue.
type localForwardPumpFixture struct {
	t              testing.TB
	settings       *ExchangeSettings
	control        *localTransportTestControl
	exchange       *Exchange
	exchangeCancel context.CancelFunc
	// the destination's resident
	resident *Resident
	// a source resident's forward to the destination
	forward   *ResidentForward
	header    ExchangeHeader
	queueSize int
	links     []*localForwardTestLink
	// called by each link's queue close, so a test can hold a detach there
	queueClose func()
}

func newLocalForwardPumpFixture(
	t testing.TB,
	ctx context.Context,
	settings *ExchangeSettings,
	queueSize int,
) *localForwardPumpFixture {
	t.Helper()
	exchangeCtx, exchangeCancel := context.WithCancel(ctx)
	exchange := &Exchange{
		ctx:             exchangeCtx,
		cancel:          exchangeCancel,
		settings:        settings,
		block:           "test-block",
		residents:       map[server.Id]*Resident{},
		residentChanges: map[server.Id]chan struct{}{},
		connections:     map[server.Id]map[server.Id]context.CancelFunc{},
	}
	control := &localTransportTestControl{}
	exchange.localForward = newExchangeLocalOpWithStore(exchangeCtx, settings, exchange.block, ExchangeOpForward, control.read)
	resident := newResidentCallbackLifecycleFixture(t, exchangeCtx, settings)
	resident.exchange = exchange
	resident.residentId = server.NewId()
	resident.instanceId = server.NewId()
	// as resident construction does
	resident.lastActivityNanos.Store(time.Now().UnixNano())
	exchange.residents[resident.clientId] = resident
	header := ExchangeHeader{
		Op:         ExchangeOpForward,
		ClientId:   resident.clientId,
		ResidentId: resident.residentId,
	}
	return &localForwardPumpFixture{
		t:              t,
		settings:       settings,
		control:        control,
		exchange:       exchange,
		exchangeCancel: exchangeCancel,
		resident:       resident,
		forward:        NewResidentForward(exchangeCtx, exchange, resident.clientId),
		header:         header,
		queueSize:      queueSize,
	}
}

// Enqueues a message to the forward as forward admission does, charging the
// forward output stage when the exchange keeps a payload ledger.
func (self *localForwardPumpFixture) enqueue(message []byte) {
	if ledger := self.exchange.residentPayloadLedger(); ledger != nil {
		ledger.update(residentPayloadForwardOutput, self.forward.clientId[15], payloadCharge(message), true)
	}
	self.forward.send <- message
}

// Attaches the forward through the production attach after its checks.
func (self *localForwardPumpFixture) attach() *localForwardTestLink {
	self.t.Helper()
	testLink := &localForwardTestLink{
		queue: make(chan []byte, self.queueSize),
		done:  make(chan localTransportDetachReason, 1),
	}
	testLink.link = self.exchange.attachLocalTransportRoutes(
		self.forward.ctx,
		self.exchange.localForward,
		self.resident,
		self.header,
		func() (chan []byte, chan []byte, func(), error) {
			return nil, testLink.queue, func() {
				testLink.closeForward.Add(1)
				if queueClose := self.queueClose; queueClose != nil {
					queueClose()
				}
			}, nil
		},
	)
	if testLink.link == nil {
		self.t.Fatal("attach refused")
	}
	self.links = append(self.links, testLink)
	return testLink
}

// Runs the link's owner as ResidentForward.Run does, with the loop's pending
// message when hasPending. A raise that escapes runLocal fails the test
// instead of the process.
func (self *localForwardPumpFixture) run(testLink *localForwardTestLink, pending []byte, hasPending bool) {
	go func() {
		reason := localTransportDetachNone
		defer func() {
			if r := recover(); r != nil {
				self.t.Errorf("a raise escaped runLocal: %v", r)
			}
			testLink.pending, testLink.hasPending = pending, hasPending
			testLink.done <- reason
		}()
		reason = self.forward.runLocal(testLink.link, &pending, &hasPending)
	}()
}

// Ends the forward and the resident and joins every owner. A pending message
// a link left with the loop and the messages left in the forward's queue are
// released, as the forward's Run releases them.
func (self *localForwardPumpFixture) close() {
	self.forward.Cancel()
	for _, testLink := range self.links {
		reason := <-testLink.done
		// keep the reason readable for later checks
		testLink.done <- reason
		if testLink.hasPending {
			self.forward.releasePayload(testLink.pending)
			testLink.pending, testLink.hasPending = nil, false
		}
	}
	self.forward.Close()
	if err := self.resident.CloseAndWait(context.Background()); err != nil {
		self.t.Error(err)
	}
	self.exchangeCancel()
}

// Reads the reason the link detached with.
func (self *localForwardTestLink) reason(t testing.TB) localTransportDetachReason {
	t.Helper()
	select {
	case reason := <-self.done:
		self.done <- reason
		return reason
	default:
		t.Fatal("link has not detached")
		return localTransportDetachNone
	}
}

// Receives the next message the pump delivers, failing if the link detaches
// before the delivery.
func (self *localForwardTestLink) receive(t testing.TB, description string) []byte {
	t.Helper()
	select {
	case message := <-self.queue:
		return message
	case <-self.link.ctx.Done():
		select {
		case message := <-self.queue:
			return message
		default:
		}
		t.Fatalf("%s: the link detached before the delivery", description)
		return nil
	}
}

// Waits a bounded time for the link to detach and returns why.
func (self *localForwardTestLink) waitDetach(t testing.TB, description string) localTransportDetachReason {
	t.Helper()
	select {
	case reason := <-self.done:
		self.done <- reason
		return reason
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the link did not detach", description)
		return localTransportDetachNone
	}
}

// Whether the link is still attached.
func (self *localForwardTestLink) attached() bool {
	select {
	case <-self.link.ctx.Done():
		return false
	default:
		return true
	}
}

func snapshotLocalForwardIO(direction string, kind string) localTransportIOSnapshot {
	return localTransportIOSnapshot{
		frames: testutil.ToFloat64(exchangeLocalFramesCounter.WithLabelValues("forward", direction, kind)),
		bytes:  testutil.ToFloat64(exchangeLocalBytesCounter.WithLabelValues("forward", direction, kind)),
	}
}

func snapshotLocalForwardDetaches(reason localTransportDetachReason) float64 {
	return testutil.ToFloat64(exchangeLocalDetachesCounter.WithLabelValues("forward", localTransportDetachReasonLabels[reason]))
}

// Fails unless the forward output stage holds nothing and released every
// message it admitted.
func requireLocalForwardLedgerBalanced(t testing.TB, ledger *residentPayloadLedger, description string) {
	t.Helper()
	snapshot := ledger.snapshot()
	group := snapshot.Groups[residentPayloadForwardOutput]
	if !snapshot.Complete || group.Messages != 0 || group.LogicalBytes != 0 || group.BackingCharge != 0 || group.Admitted != group.Released {
		t.Errorf("%s: forward output ledger %+v, want empty and balanced", description, group)
	}
}

// The pump delivers the loop's pending message first and then the forward's
// queue in FIFO order, gathers batches bounded by ExchangeWriteBatchCount with
// the pending message the first of the first batch, counts each delivered
// frame once sent and once received with its frame header under op forward,
// and releases each message's forward output charge as it hands it on.
func TestLocalForwardPumpKeepsFrameOrderAndBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	violations := watchLocalTransportPoolViolations(t)
	settings := localForwardTestSettings()
	settings.ExchangeWriteBatchCount = 7
	settings.ForwardBufferSize = 4096
	f := newLocalForwardPumpFixture(t, ctx, settings, 4096)
	ledger := &residentPayloadLedger{}
	f.exchange.payloadOwnerLedger = ledger
	defer f.close()
	var batchLock sync.Mutex
	batchCounts := []int{}
	f.exchange.afterLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection, count int) {
		batchLock.Lock()
		defer batchLock.Unlock()
		if direction != localTransportDirectionForward {
			t.Errorf("a forward pump gathered as direction %d", direction)
		}
		batchCounts = append(batchCounts, count)
	}
	testLink := f.attach()

	const messageCount = 1000
	byteCount := func(index int) int { return 20 + index%300 }
	var witnesses [][]byte
	wantBytes := 0
	messages := make([][]byte, messageCount)
	for messageIndex := range messageCount {
		messages[messageIndex] = newLocalTransportTestMessage(messageIndex, byteCount(messageIndex))
		witnesses = append(witnesses, retainResidentPoolWitness(messages[messageIndex]))
		wantBytes += exchangeIOFrameHeaderByteCount + byteCount(messageIndex)
	}
	// the loop took the first from the queue before its lookup; the rest are
	// queued before the pump starts, so it gathers full batches
	pending := messages[0]
	ledger.update(residentPayloadForwardOutput, f.forward.clientId[15], payloadCharge(pending), true)
	for _, message := range messages[1:] {
		f.enqueue(message)
	}
	sentBefore := snapshotLocalForwardIO("sent", "data")
	receivedBefore := snapshotLocalForwardIO("received", "data")
	transportSentBefore := snapshotLocalTransportIO("sent", "data")
	f.run(testLink, pending, true)

	for messageIndex := range messageCount {
		select {
		case message := <-testLink.queue:
			if got := localTransportTestMessageIndex(message); got != messageIndex || len(message) != byteCount(messageIndex) {
				t.Fatalf("message %d = index %d len %d, want len %d", messageIndex, got, len(message), byteCount(messageIndex))
			}
			clientconnect.MessagePoolReturn(message)
		case <-ctx.Done():
			t.Fatalf("delivered %d of %d", messageIndex, messageCount)
		}
	}

	batchLock.Lock()
	total := 0
	for _, count := range batchCounts {
		if count < 1 || settings.ExchangeWriteBatchCount < count {
			t.Errorf("gathered a batch of %d", count)
		}
		total += count
	}
	if total != messageCount || len(batchCounts) == 0 || batchCounts[0] != settings.ExchangeWriteBatchCount {
		t.Errorf("batches %v, want %d messages starting with a full batch", batchCounts, messageCount)
	}
	batchLock.Unlock()

	sent := snapshotLocalForwardIO("sent", "data")
	received := snapshotLocalForwardIO("received", "data")
	if sent.frames-sentBefore.frames != messageCount || received.frames-receivedBefore.frames != messageCount {
		t.Errorf("local forward frames sent %v received %v, want %d each", sent.frames-sentBefore.frames, received.frames-receivedBefore.frames, messageCount)
	}
	if sent.bytes-sentBefore.bytes != float64(wantBytes) || received.bytes-receivedBefore.bytes != float64(wantBytes) {
		t.Errorf("local forward bytes sent %v received %v, want %d each", sent.bytes-sentBefore.bytes, received.bytes-receivedBefore.bytes, wantBytes)
	}
	if snapshotLocalTransportIO("sent", "data") != transportSentBefore {
		t.Error("a forward link counted its frames as a transport's")
	}

	forwardDoneBefore := snapshotLocalForwardDetaches(localTransportDetachForwardDone)
	f.forward.Cancel()
	if reason := testLink.waitDetach(t, "the forward's end"); reason != localTransportDetachForwardDone {
		t.Errorf("detach reason = %d, want forward_done", reason)
	}
	if snapshotLocalForwardDetaches(localTransportDetachForwardDone)-forwardDoneBefore != 1 {
		t.Error("forward_done detach not counted under op forward")
	}
	requireResidentPoolOwnersReturned(t, witnesses, "delivered local forward frames")
	requireLocalForwardLedgerBalanced(t, ledger, "delivered local forward frames")
	if testLink.hasPending {
		t.Error("the link left the pending message it delivered with the loop")
	}
	if testLink.closeForward.Load() != 1 {
		t.Errorf("forward queue close ran %d times, want once", testLink.closeForward.Load())
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// A batch has WriteTimeout from its start: a delivery 1 ns inside it keeps the
// link, a batch blocked for all of it detaches at exactly WriteTimeout with
// write_timeout, two blocked deliveries of one batch share the one budget,
// and an idle link's next batch has the whole budget. The destination's
// forward consumer stalling is modeled as a reader that stops. Catches a
// per-delivery re-arm (which loosens the bound) and a deadline from before the
// batch (which shortens a healthy batch).
func TestLocalForwardBatchDeadlineStallsDetach(t *testing.T) {
	warmLocalTransportPool()

	// healthy: delivered 1 ns before the deadline, the pending message's batch
	// first, and the next batch has a whole budget of its own
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardPumpFixture(t, context.Background(), localForwardTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		f.run(testLink, newLocalTransportTestMessage(1, 64), true)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		clientconnect.MessagePoolReturn(testLink.receive(t, "a batch 1 ns inside its budget"))
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("a batch delivered inside its budget detached the link")
		}
		f.enqueue(newLocalTransportTestMessage(2, 64))
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		clientconnect.MessagePoolReturn(testLink.receive(t, "the next batch 1 ns inside its own budget"))
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("the next batch did not get a whole budget of its own")
		}
	})

	// stalled: detaches at exactly WriteTimeout, the batch returned and counted
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardPumpFixture(t, context.Background(), localForwardTestSettings(), 0)
		ledger := &residentPayloadLedger{}
		f.exchange.payloadOwnerLedger = ledger
		defer f.close()
		testLink := f.attach()
		message := newLocalTransportTestMessage(1, 64)
		witness := retainResidentPoolWitness(message)
		f.enqueue(message)
		dropsBefore, dropBytesBefore := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend)
		detachesBefore := snapshotLocalForwardDetaches(localTransportDetachWriteTimeout)
		f.run(testLink, nil, false)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("the link detached before the batch's budget ended")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if testLink.attached() || testLink.reason(t) != localTransportDetachWriteTimeout {
			t.Fatal("a batch blocked for its whole budget did not detach the link with write_timeout")
		}
		requireResidentPoolOwnerReturned(t, witness, "stalled batch")
		requireLocalForwardLedgerBalanced(t, ledger, "stalled batch")
		drops, dropBytes := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend)
		if drops-dropsBefore != 1 || dropBytes-dropBytesBefore != 64 {
			t.Errorf("stalled batch drops = %v (%v bytes), want 1 (64 bytes)", drops-dropsBefore, dropBytes-dropBytesBefore)
		}
		if snapshotLocalForwardDetaches(localTransportDetachWriteTimeout)-detachesBefore != 1 {
			t.Error("write_timeout detach not counted under op forward")
		}
	})

	// two blocked deliveries of one batch share its budget
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardPumpFixture(t, context.Background(), localForwardTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		f.enqueue(newLocalTransportTestMessage(2, 64))
		f.run(testLink, newLocalTransportTestMessage(1, 64), true)
		synctest.Wait()
		time.Sleep(10 * time.Second)
		clientconnect.MessagePoolReturn(testLink.receive(t, "the first message of a shared budget"))
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - 10*time.Second - time.Nanosecond)
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("the link detached before the batch's budget ended")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if testLink.attached() {
			t.Fatal("the second blocked delivery of a batch got a budget of its own")
		}
	})

	// an idle link's next batch has the whole budget
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardPumpFixture(t, context.Background(), localForwardTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		f.run(testLink, nil, false)
		time.Sleep(time.Hour)
		f.enqueue(newLocalTransportTestMessage(1, 64))
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		clientconnect.MessagePoolReturn(testLink.receive(t, "a batch after an idle hour"))
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("a batch after an idle hour did not get its whole budget")
		}
	})
}

// An oversized message fails its whole batch before any delivery, as a failed
// exchange write does: the batch is returned and counted, the link detaches
// with max_message_len, and the next link carries the rest of the queue in
// order. Both framer limits apply: MaxMessageLen and the uint16 frame length,
// including an oversized pending message.
func TestLocalForwardMaxMessageLenTearsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	violations := watchLocalTransportPoolViolations(t)

	cases := []struct {
		name          string
		maxMessageLen int
		oversizeLen   int
		// the oversized message is the loop's pending message instead of the
		// second of the batch
		pendingOversize bool
	}{
		{name: "MaxMessageLen", maxMessageLen: DefaultExchangeSettings().FramerSettings.MaxMessageLen, oversizeLen: DefaultExchangeSettings().FramerSettings.MaxMessageLen + 1},
		{name: "uint16 frame length", maxMessageLen: 2 * math.MaxUint16, oversizeLen: math.MaxUint16 + 1},
		{name: "an oversized pending message", maxMessageLen: DefaultExchangeSettings().FramerSettings.MaxMessageLen, oversizeLen: DefaultExchangeSettings().FramerSettings.MaxMessageLen + 1, pendingOversize: true},
	}
	for _, c := range cases {
		settings := localForwardTestSettings()
		framerSettings := *settings.FramerSettings
		framerSettings.MaxMessageLen = c.maxMessageLen
		settings.FramerSettings = &framerSettings
		settings.ExchangeWriteBatchCount = 2
		f := newLocalForwardPumpFixture(t, ctx, settings, 16)
		ledger := &residentPayloadLedger{}
		f.exchange.payloadOwnerLedger = ledger

		first := f.attach()
		var failed [][]byte
		var pending []byte
		if c.pendingOversize {
			pending = clientconnect.MessagePoolGet(c.oversizeLen)
			ledger.update(residentPayloadForwardOutput, f.forward.clientId[15], payloadCharge(pending), true)
			failed = [][]byte{pending, newLocalTransportTestMessage(1, 64)}
			f.enqueue(failed[1])
		} else {
			failed = [][]byte{newLocalTransportTestMessage(1, 64), clientconnect.MessagePoolGet(c.oversizeLen)}
			for _, message := range failed {
				f.enqueue(message)
			}
		}
		witnesses := retainLocalTransportPooledWitnesses(failed)
		f.enqueue(newLocalTransportTestMessage(3, 64))
		f.enqueue(newLocalTransportTestMessage(4, 64))
		dropsBefore, dropBytesBefore := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend)
		detachesBefore := snapshotLocalForwardDetaches(localTransportDetachMaxMessageLen)
		f.run(first, pending, pending != nil)
		if reason := first.waitDetach(t, c.name+" oversized batch"); reason != localTransportDetachMaxMessageLen {
			t.Fatalf("%s: detach reason = %d, want max_message_len", c.name, reason)
		}
		if 0 < len(first.queue) {
			t.Errorf("%s: a message of the failed batch was delivered", c.name)
		}
		requireResidentPoolOwnersReturned(t, witnesses, c.name+" failed batch")
		drops, dropBytes := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend)
		if drops-dropsBefore != 2 || dropBytes-dropBytesBefore != float64(64+c.oversizeLen) {
			t.Errorf("%s: failed batch drops = %v (%v bytes), want 2 (%d bytes)", c.name, drops-dropsBefore, dropBytes-dropBytesBefore, 64+c.oversizeLen)
		}
		if snapshotLocalForwardDetaches(localTransportDetachMaxMessageLen)-detachesBefore != 1 {
			t.Errorf("%s: max_message_len detach not counted under op forward", c.name)
		}
		second := f.attach()
		f.run(second, nil, false)
		for _, wantIndex := range []int{3, 4} {
			select {
			case message := <-second.queue:
				if localTransportTestMessageIndex(message) != wantIndex {
					t.Errorf("%s: the next link delivered %d, want %d", c.name, localTransportTestMessageIndex(message), wantIndex)
				}
				clientconnect.MessagePoolReturn(message)
			case <-ctx.Done():
				t.Fatalf("%s: the next link did not carry the rest of the queue", c.name)
			}
		}
		f.close()
		requireLocalForwardLedgerBalanced(t, ledger, c.name)
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// A zero-length message is a ping, as on a forward socket: it is returned, not
// delivered, counts as an exchange ping under op forward, and is the
// destination resident's activity.
func TestLocalForwardZeroLengthIsPing(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardPumpFixture(t, context.Background(), localForwardTestSettings(), 16)
		ledger := &residentPayloadLedger{}
		f.exchange.payloadOwnerLedger = ledger
		defer f.close()
		testLink := f.attach()
		staleActivityNanos := time.Now().Add(-time.Hour).UnixNano()
		f.resident.lastActivityNanos.Store(staleActivityNanos)
		ping := clientconnect.MessagePoolGet(0)
		witness := retainResidentPoolWitness(ping)
		f.enqueue(ping)
		f.enqueue(newLocalTransportTestMessage(1, 64))
		pingSentBefore := snapshotLocalForwardIO("sent", "ping")
		pingReceivedBefore := snapshotLocalForwardIO("received", "ping")
		f.run(testLink, nil, false)
		synctest.Wait()

		if len(testLink.queue) != 1 {
			t.Fatalf("delivered %d to the resident, want only the data message", len(testLink.queue))
		}
		if message := <-testLink.queue; localTransportTestMessageIndex(message) != 1 {
			t.Error("the data message after the ping was not delivered")
		} else {
			clientconnect.MessagePoolReturn(message)
		}
		requireResidentPoolOwnerReturned(t, witness, "a local forward ping")
		pingSent := snapshotLocalForwardIO("sent", "ping")
		pingReceived := snapshotLocalForwardIO("received", "ping")
		if pingSent.frames-pingSentBefore.frames != 1 || pingReceived.frames-pingReceivedBefore.frames != 1 ||
			pingSent.bytes-pingSentBefore.bytes != exchangeIOFrameHeaderByteCount {
			t.Errorf("ping frames sent %v received %v bytes %v, want 1, 1, %d", pingSent.frames-pingSentBefore.frames, pingReceived.frames-pingReceivedBefore.frames, pingSent.bytes-pingSentBefore.bytes, exchangeIOFrameHeaderByteCount)
		}
		if f.resident.lastActivityNanos.Load() <= staleActivityNanos {
			t.Error("a ping was not the destination resident's activity")
		}
		if !testLink.attached() {
			t.Error("a ping detached the link")
		}
		f.forward.Cancel()
		synctest.Wait()
		requireLocalForwardLedgerBalanced(t, ledger, "a ping and a delivered message")
	})
}

// Idle and attached past the resident ttl, the destination resident stays: an
// attached forward link stands in for the forward socket's pings. Detached, it
// retires exactly one ttl after the detach, as after an idle socket's last
// ping. The forward's own idle lifetime is unchanged: the link never renews
// it.
func TestLocalForwardAttachmentKeepsResidentActive(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		settings := localForwardTestSettings()
		// longer than the resident ttl, so the forward outlives the window
		settings.ForwardIdleTimeout = time.Hour
		f := newLocalForwardPumpFixture(t, context.Background(), settings, 16)
		defer f.close()
		ttl := f.settings.ExchangeResidentTtl
		if ttl != 300*time.Second {
			t.Fatalf("resident ttl = %s, want the production 300s", ttl)
		}
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			f.resident.runIdleWatcher()
		}()
		forwardActivityNanos := f.forward.lastActivityNanos.Load()

		testLink := f.attach()
		f.run(testLink, nil, false)
		time.Sleep(2*ttl + ttl/3)
		synctest.Wait()
		if f.resident.IsDone() {
			t.Fatal("a destination resident with an idle attached forward retired")
		}
		if f.forward.lastActivityNanos.Load() != forwardActivityNanos {
			t.Fatal("an attached link renewed the forward's own idle lifetime")
		}

		f.forward.Cancel()
		synctest.Wait()
		if reason := testLink.reason(t); reason != localTransportDetachForwardDone {
			t.Fatalf("detach reason = %d, want forward_done", reason)
		}
		time.Sleep(ttl - time.Nanosecond)
		synctest.Wait()
		if f.resident.IsDone() {
			t.Fatal("the resident retired before one ttl after its last link detached")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.resident.IsDone() {
			t.Fatal("the resident did not retire one ttl after its last link detached")
		}
		<-watcherDone
	})
}

// Detaching at each barrier leaves every message with exactly one final owner,
// no pool violation and a balanced forward output ledger: after a gather with
// the batch delivered, after a gather with the delivery waiting, at a blocked
// delivery when the destination resident closes, with a pending message when
// the resident closed before the pump started (the message stays the loop's),
// and with messages left in the forward queue.
func TestLocalForwardPoolOwnershipUnderTeardown(t *testing.T) {
	warmLocalTransportPool()
	violations := watchLocalTransportPoolViolations(t)
	messages := func(firstIndex int, count int) ([][]byte, [][]byte) {
		messages := make([][]byte, count)
		witnesses := make([][]byte, count)
		for i := range count {
			messages[i] = newLocalTransportTestMessage(firstIndex+i, 100+i)
			witnesses[i] = retainResidentPoolWitness(messages[i])
		}
		return messages, witnesses
	}

	cases := []struct {
		name       string
		queueSize  int
		wantReason localTransportDetachReason
		// the pending message the loop still owns after the link
		wantPending bool
		arm         func(f *localForwardPumpFixture, testLink *localForwardTestLink)
	}{
		{
			name:       "after a gather, the batch delivered",
			queueSize:  16,
			wantReason: localTransportDetachForwardDone,
			arm: func(f *localForwardPumpFixture, _ *localForwardTestLink) {
				f.exchange.afterLocalTransportGatherForTest = func(*localTransportLink, localTransportDirection, int) {
					f.forward.Cancel()
				}
			},
		},
		{
			name:       "after a gather, the delivery waiting",
			queueSize:  0,
			wantReason: localTransportDetachForwardDone,
			arm: func(f *localForwardPumpFixture, _ *localForwardTestLink) {
				f.exchange.afterLocalTransportGatherForTest = func(*localTransportLink, localTransportDirection, int) {
					f.forward.Cancel()
				}
			},
		},
		{
			name:       "a blocked delivery when the destination resident closes",
			queueSize:  1,
			wantReason: localTransportDetachResidentDone,
			arm: func(f *localForwardPumpFixture, _ *localForwardTestLink) {
				f.exchange.beforeLocalTransportBlockForTest = func(*localTransportLink, localTransportDirection) {
					f.resident.Cancel()
				}
			},
		},
		{
			name:        "the resident closed before the pump took the pending message",
			queueSize:   16,
			wantReason:  localTransportDetachResidentDone,
			wantPending: true,
			arm: func(f *localForwardPumpFixture, testLink *localForwardTestLink) {
				f.resident.Cancel()
				<-testLink.link.ctx.Done()
			},
		},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			settings := localForwardTestSettings()
			f := newLocalForwardPumpFixture(t, context.Background(), settings, c.queueSize)
			ledger := &residentPayloadLedger{}
			f.exchange.payloadOwnerLedger = ledger
			testLink := f.attach()
			batch, witnesses := messages(0, 6)
			pending := batch[0]
			ledger.update(residentPayloadForwardOutput, f.forward.clientId[15], payloadCharge(pending), true)
			for _, message := range batch[1:] {
				f.enqueue(message)
			}
			c.arm(f, testLink)
			f.run(testLink, pending, true)
			synctest.Wait()
			if testLink.attached() {
				t.Fatalf("%s: the link did not detach", c.name)
			}
			if reason := testLink.reason(t); reason != c.wantReason {
				t.Errorf("%s: detach reason = %d, want %d", c.name, reason, c.wantReason)
			}
			if testLink.hasPending != c.wantPending {
				t.Errorf("%s: the loop owns a pending message = %t, want %t", c.name, testLink.hasPending, c.wantPending)
			}
			if c.wantPending && testLink.hasPending && localTransportTestMessageIndex(testLink.pending) != 0 {
				t.Errorf("%s: the loop's pending message changed", c.name)
			}
			// whatever reached the stand-in forward queue is the consumer's
			returnReadyPooledMessages(testLink.queue)
			f.close()
			requireResidentPoolOwnersReturned(t, witnesses, c.name)
			requireLocalForwardLedgerBalanced(t, ledger, c.name)
		})
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// Attach does the accept path's work in its order and detach undoes it: one
// accepted exchange connection, one forward attachment and one link while
// attached, none after, with the forward queue closed once. The transport
// switch and its gauge are untouched.
func TestLocalForwardAttachAccountingBalances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newLocalForwardPumpFixture(t, ctx, localForwardTestSettings(), 16)
	defer f.close()
	forwardAttachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward])
	transportAttachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpTransport])
	testLink := f.attach()
	clientId := f.resident.clientId
	if count := f.exchange.exchangeConnectionCounts[clientId]; count != 1 {
		t.Fatalf("accepted exchange connections = %d, want 1", count)
	}
	if f.resident.localAttachments.Load() != 1 || f.exchange.localForward.linkCount() != 1 ||
		testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) != forwardAttachmentsBefore+1 {
		t.Fatal("attach did not count its attachment and link")
	}
	if testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpTransport]) != transportAttachmentsBefore {
		t.Fatal("a forward attach counted a transport attachment")
	}
	f.run(testLink, nil, false)
	f.forward.Cancel()
	if reason := testLink.waitDetach(t, "the forward's end"); reason != localTransportDetachForwardDone {
		t.Fatalf("detach reason = %d, want forward_done", reason)
	}
	if _, ok := f.exchange.exchangeConnectionCounts[clientId]; ok {
		t.Error("detach left an accepted exchange connection")
	}
	if f.resident.localAttachments.Load() != 0 || f.exchange.localForward.linkCount() != 0 ||
		testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) != forwardAttachmentsBefore {
		t.Error("detach left its attachment or link")
	}
	if testLink.closeForward.Load() != 1 {
		t.Errorf("forward queue close ran %d times, want once", testLink.closeForward.Load())
	}
}

// The production attach refuses what the accept path refuses and what the
// forward switch excludes: the ceiling off (even with transports allowed), the
// switch off for this block, a generation that is not current, a missing
// resident and a done resident. A refusal leaves no accounting behind. A
// current resident attaches into its real forward queue.
func TestLocalForwardAttachRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := localForwardTestSettings()
	settings.EnableLocalTransport = true
	settings.LocalTransportDefault = model.ConnectLocalTransportAll
	settings.ExchangeBufferSize = 4
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	defer resident.CloseAndWait(context.Background())
	exchange := &Exchange{
		ctx:       ctx,
		settings:  settings,
		block:     "test-block",
		residents: map[server.Id]*Resident{},
	}
	resident.exchange = exchange
	resident.residentId = server.NewId()
	exchange.residents[resident.clientId] = resident
	control := &localTransportTestControl{}
	header := ExchangeHeader{Op: ExchangeOpForward, ClientId: resident.clientId, ResidentId: resident.residentId}

	// the transport switch allows every block; the forward ceiling is off
	exchange.localTransport = newExchangeLocalTransportWithStore(ctx, settings, exchange.block, control.read)
	if exchange.attachLocalForward(ctx, header) != nil {
		t.Error("attached a forward with its ceiling off")
	}
	exchange.localForward = newExchangeLocalOpWithStore(ctx, settings, exchange.block, ExchangeOpForward, control.read)
	control.setForward("other-block")
	exchange.localForward.refresh()
	if exchange.attachLocalForward(ctx, header) != nil {
		t.Error("attached with the forward switch off for this block")
	}
	if !exchange.localTransport.allows() {
		t.Error("the forward field turned the transport switch off")
	}
	control.setForward("test-block")
	exchange.localForward.refresh()
	stale := header
	stale.ResidentId = server.NewId()
	if exchange.attachLocalForward(ctx, stale) != nil {
		t.Error("attached to a generation that is not current")
	}
	missing := header
	missing.ClientId = server.NewId()
	if exchange.attachLocalForward(ctx, missing) != nil {
		t.Error("attached to a missing resident")
	}
	if len(exchange.residentChanges) != 0 || len(exchange.exchangeConnectionCounts) != 0 {
		t.Error("a refused attach left wait or connection state")
	}
	// the real forward queue: one message reaches the resident's consumer
	var received atomic.Int64
	resident.forwardReceiveForTest = func(_ context.Context, message []byte) bool {
		received.Add(1)
		clientconnect.MessagePoolReturn(message)
		return true
	}
	forward := NewResidentForward(ctx, exchange, resident.clientId)
	link := exchange.attachLocalForward(forward.ctx, header)
	if link == nil {
		t.Fatal("refused a current resident with the switch on")
	}
	if exchange.localTransport.linkCount() != 0 || exchange.localForward.linkCount() != 1 {
		t.Error("a forward link joined the transport switch's links")
	}
	runDone := make(chan localTransportDetachReason, 1)
	go func() {
		var pending []byte
		hasPending := false
		runDone <- forward.runLocal(link, &pending, &hasPending)
	}()
	forward.send <- newLocalTransportTestMessage(1, 64)
	for received.Load() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("the attached forward delivered nothing")
		case <-time.After(time.Millisecond):
		}
	}
	forward.Cancel()
	<-runDone
	forward.Close()
	resident.Cancel()
	if exchange.attachLocalForward(ctx, header) != nil {
		t.Error("attached to a done resident")
	}
	if exchange.localForward.linkCount() != 0 || len(exchange.exchangeConnectionCounts) != 0 || resident.localAttachments.Load() != 0 {
		t.Error("refusals or the detach left accounting behind")
	}
}

// Each switch reads only its own field: one control store with localTransport
// all and localForward off lets transports attach and refuses forwards, the
// reverse the reverse, and each gauge follows its own switch.
func TestLocalForwardSwitchIsIndependentOfTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := localForwardTestSettings()
	settings.EnableLocalTransport = true
	settings.LocalTransportDefault = model.ConnectLocalTransportOff
	settings.LocalForwardDefault = model.ConnectLocalTransportOff
	control := &localTransportTestControl{}
	localTransport := newExchangeLocalOpWithStore(ctx, settings, "g1", ExchangeOpTransport, control.read)
	localForward := newExchangeLocalOpWithStore(ctx, settings, "g1", ExchangeOpForward, control.read)
	if localTransport.allows() || localForward.allows() {
		t.Fatal("a dark default allowed attaching before the first read")
	}
	set := func(fields map[string]string) {
		control.stateLock.Lock()
		defer control.stateLock.Unlock()
		control.fields = fields
	}
	for _, c := range []struct {
		fields    map[string]string
		transport bool
		forward   bool
	}{
		{fields: map[string]string{"localTransport": "all", "localForward": "off"}, transport: true, forward: false},
		{fields: map[string]string{"localTransport": "off", "localForward": "g1"}, transport: false, forward: true},
		{fields: map[string]string{"localTransport": "g1", "localForward": "beta"}, transport: true, forward: false},
		{fields: map[string]string{"localForward": "all"}, transport: false, forward: true},
		{fields: map[string]string{}, transport: false, forward: false},
	} {
		set(c.fields)
		localTransport.refresh()
		localForward.refresh()
		if localTransport.allows() != c.transport || localForward.allows() != c.forward {
			t.Errorf("control %v: transport=%t forward=%t, want %t and %t", c.fields, localTransport.allows(), localForward.allows(), c.transport, c.forward)
		}
		if (testutil.ToFloat64(exchangeLocalTransportEnabledGauge) == 1) != c.transport || (testutil.ToFloat64(exchangeLocalForwardEnabledGauge) == 1) != c.forward {
			t.Errorf("control %v: gauges transport=%v forward=%v", c.fields, testutil.ToFloat64(exchangeLocalTransportEnabledGauge), testutil.ToFloat64(exchangeLocalForwardEnabledGauge))
		}
	}
	ceilingOff := localForwardTestSettings()
	ceilingOff.EnableLocalForward = false
	if newExchangeLocalTransport(ctx, ceilingOff, "g1", ExchangeOpForward) != nil {
		t.Error("the forward ceiling off still built a switch that reads the control")
	}
}

// Narrowing switches new forward attaches to TCP at once and detaches live
// forward links at LocalForwardDisableDetachRate: one at the change, then one
// per interval. A message still in a forward's queue survives for its next
// link or connection, while the batch its pump held counts as dropped. Turning
// the switch on again stops the pacer.
func TestLocalForwardKillSwitchPacedDetach(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := localForwardTestSettings()
		settings.ExchangeWriteBatchCount = 1
		settings.LocalForwardDisableDetachRate = 10
		// the transport rate must not pace forwards
		settings.LocalTransportDisableDetachRate = 1
		interval := time.Second / 10
		base := newLocalForwardPumpFixture(t, ctx, settings, 0)
		defer base.close()
		var pacerStarts atomic.Int64
		base.exchange.localForward.afterPacerStartForTest = func() { pacerStarts.Add(1) }

		// five forwards to five residents in one exchange
		const linkCount = 5
		fixtures := []*localForwardPumpFixture{base}
		for range linkCount - 1 {
			f := newLocalForwardPumpFixture(t, ctx, settings, 0)
			f.exchange = base.exchange
			f.resident.exchange = base.exchange
			base.exchange.residents[f.resident.clientId] = f.resident
			f.forward.exchange = base.exchange
			fixtures = append(fixtures, f)
			defer f.close()
		}
		testLinks := []*localForwardTestLink{}
		for _, f := range fixtures {
			testLink := f.attach()
			f.run(testLink, nil, false)
			testLinks = append(testLinks, testLink)
		}
		// the first forward's pump holds one message; three more wait in its
		// queue
		held := newLocalTransportTestMessage(0, 64)
		heldWitness := retainResidentPoolWitness(held)
		base.enqueue(held)
		synctest.Wait()
		for messageIndex := 1; messageIndex <= 3; messageIndex++ {
			base.enqueue(newLocalTransportTestMessage(messageIndex, 64))
		}
		dropsBefore, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend)
		disabledBefore := snapshotLocalForwardDetaches(localTransportDetachDisabled)
		attachedCount := func() int {
			count := 0
			for _, testLink := range testLinks {
				if testLink.attached() {
					count += 1
				}
			}
			return count
		}

		base.control.setForward("other-block")
		base.exchange.localForward.refresh()
		synctest.Wait()
		if count := attachedCount(); count != linkCount-1 {
			t.Fatalf("attached at the change = %d, want %d", count, linkCount-1)
		}
		if base.exchange.attachLocalForward(ctx, base.header) != nil {
			t.Fatal("a new attach was allowed after the switch turned off")
		}
		for detached := 2; detached <= 3; detached++ {
			time.Sleep(interval)
			synctest.Wait()
			if count := attachedCount(); count != linkCount-detached {
				t.Fatalf("attached after %d intervals = %d, want %d", detached-1, count, linkCount-detached)
			}
		}

		// on again: the pacer stops with links still attached
		base.control.setForward("all")
		base.exchange.localForward.refresh()
		time.Sleep(10 * interval)
		synctest.Wait()
		if count := attachedCount(); count != linkCount-3 {
			t.Fatalf("attached after the switch turned on again = %d, want %d", count, linkCount-3)
		}
		// off again: the rest detach at the pace
		base.control.setForward("off")
		base.exchange.localForward.refresh()
		time.Sleep(interval)
		synctest.Wait()
		if count := attachedCount(); count != 0 {
			t.Fatalf("attached after the second change = %d, want 0", count)
		}
		for _, testLink := range testLinks {
			if testLink.reason(t) != localTransportDetachDisabled {
				t.Errorf("a link detached with %d, want disabled", testLink.reason(t))
			}
		}
		if count := snapshotLocalForwardDetaches(localTransportDetachDisabled) - disabledBefore; count != linkCount {
			t.Errorf("disabled detaches = %v, want %d", count, linkCount)
		}
		requireResidentPoolOwnerReturned(t, heldWitness, "the batch a disabled link held")
		if drops, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend); drops-dropsBefore != 1 {
			t.Errorf("drops = %v, want the one held message", drops-dropsBefore)
		}
		if len(base.forward.send) != 3 {
			t.Fatalf("the forward queue holds %d, want the 3 messages for the next link", len(base.forward.send))
		}
		for messageIndex := 1; messageIndex <= 3; messageIndex++ {
			message := <-base.forward.send
			if localTransportTestMessageIndex(message) != messageIndex {
				t.Errorf("forward queue message %d out of order", messageIndex)
			}
			clientconnect.MessagePoolReturn(message)
		}
		// one pacer per turn off, each ending when it had nothing left to do
		if pacerStarts.Load() != 2 || base.exchange.localForward.isPacing() {
			t.Errorf("pacer starts = %d pacing=%t, want 2 and none left", pacerStarts.Load(), base.exchange.localForward.isPacing())
		}
		cancel()
	})
}

// A rate of zero detaches none: the switch then reaches only new attaches.
func TestLocalForwardKillSwitchRateZeroKeepsLiveLinks(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		settings := localForwardTestSettings()
		settings.LocalForwardDisableDetachRate = 0
		f := newLocalForwardPumpFixture(t, context.Background(), settings, 16)
		defer f.close()
		f.exchange.localForward.afterPacerStartForTest = func() { t.Error("a zero rate started a pacer") }
		testLink := f.attach()
		f.run(testLink, nil, false)
		f.control.setForward("off")
		f.exchange.localForward.refresh()
		time.Sleep(time.Minute)
		synctest.Wait()
		if !testLink.attached() {
			t.Error("a zero rate detached a live link")
		}
		if f.exchange.attachLocalForward(context.Background(), f.header) != nil {
			t.Error("a zero rate allowed a new attach")
		}
	})
}

// The connect service ships the forward ceiling on and its switch dark, read
// on the transport switch's interval and paced at 500/s; every other exchange
// keeps the ceiling off.
func TestLocalForwardShipsDark(t *testing.T) {
	defaults := DefaultExchangeSettings()
	if defaults.EnableLocalForward {
		t.Error("the default exchange settings allow local forwards")
	}
	runSettings := exchangeSettingsForRun(RunOptions{Port: 443})
	if !runSettings.EnableLocalForward {
		t.Error("the connect service does not allow local forwards")
	}
	if runSettings.LocalForwardDefault != model.ConnectLocalTransportOff {
		t.Errorf("the connect service's forward default = %q, want off", runSettings.LocalForwardDefault)
	}
	if runSettings.localControlReadForTest != nil {
		t.Error("the connect service reads a test control")
	}
	control := model.NarrowConnectMoveControl(nil, model.ConnectMoveControlCeiling{
		EnableLocalForward:  runSettings.EnableLocalForward,
		LocalForwardDefault: runSettings.LocalForwardDefault,
	})
	for _, block := range []string{"beta", "g1", "g2", "g3", "g4"} {
		if control.LocalForwardAllows(block) {
			t.Errorf("block %s attaches forwards with no control", block)
		}
	}
	if runSettings.LocalForwardDisableDetachRate != 500 || runSettings.LocalTransportControlRefreshInterval != 10*time.Second {
		t.Errorf("pacing %d/s refresh %s, want 500/s and 10s", runSettings.LocalForwardDisableDetachRate, runSettings.LocalTransportControlRefreshInterval)
	}
}

// A raise in the pump detaches the link with raised after the pump returned
// and counted the batch it held, and releases its ledger charges.
func TestLocalForwardPumpRaiseDetachesAndReturnsTheBatch(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		settings := localForwardTestSettings()
		f := newLocalForwardPumpFixture(t, context.Background(), settings, 0)
		ledger := &residentPayloadLedger{}
		f.exchange.payloadOwnerLedger = ledger
		defer f.close()
		testLink := f.attach()
		f.exchange.beforeLocalTransportBlockForTest = func(*localTransportLink, localTransportDirection) {
			panic("synthetic local forward pump fault")
		}
		batch := [][]byte{newLocalTransportTestMessage(1, 64), newLocalTransportTestMessage(2, 64)}
		witnesses := retainLocalTransportPooledWitnesses(batch)
		for _, message := range batch {
			f.enqueue(message)
		}
		dropsBefore, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend)
		f.run(testLink, nil, false)
		synctest.Wait()
		if testLink.attached() || testLink.reason(t) != localTransportDetachRaised {
			t.Fatal("a raise did not detach the link with raised")
		}
		requireResidentPoolOwnersReturned(t, witnesses, "the batch a raised pump held")
		requireLocalForwardLedgerBalanced(t, ledger, "the batch a raised pump held")
		if drops, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalForwardSend); drops-dropsBefore != 2 {
			t.Errorf("drops = %v, want the two held messages", drops-dropsBefore)
		}
	})
}
