// Local transport tests (resident_local_transport.go). The pump fixture attaches
// a transport to a resident through the production attach, over queues the
// test owns in place of the resident's routes, so the test reads and writes as
// the resident and every boundary is a barrier the test controls. Clocks are
// synctest virtual time wherever a bound is measured.
package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"maps"
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

// The pool's process-wide statistics worker must start outside a bubble.
func warmLocalTransportPool() {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
}

// Production bounds, with in-process delivery allowed for every block.
func localTransportTestSettings() *ExchangeSettings {
	settings := DefaultExchangeSettings()
	settings.EnableLocalTransport = true
	settings.LocalTransportDefault = model.ConnectLocalTransportAll
	settings.EnableMove = false
	return settings
}

// The Redis control in memory, narrowed as the model narrows the real one.
type localTransportTestControl struct {
	stateLock sync.Mutex
	fields    map[string]string
	err       error
	readCount int
}

// Sets the control's `localTransport`.
func (self *localTransportTestControl) set(value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fields = map[string]string{model.ConnectMoveControlFieldLocalTransport: value}
	self.err = nil
}

// Makes every read fail.
func (self *localTransportTestControl) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

func (self *localTransportTestControl) read(
	ctx context.Context,
	ceiling model.ConnectMoveControlCeiling,
) (*model.ConnectMoveControl, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readCount += 1
	if self.err != nil {
		return nil, self.err
	}
	return model.NarrowConnectMoveControl(maps.Clone(self.fields), ceiling), nil
}

// A pooled message whose bytes identify it: its index in the first four bytes,
// then a pattern.
func newLocalTransportTestMessage(index int, byteCount int) []byte {
	message := clientconnect.MessagePoolGet(byteCount)
	for byteIndex := range message {
		message[byteIndex] = byte(index + byteIndex)
	}
	if 4 <= byteCount {
		binary.BigEndian.PutUint32(message, uint32(index))
	}
	return message
}

// The index newLocalTransportTestMessage wrote.
func localTransportTestMessageIndex(message []byte) int {
	return int(binary.BigEndian.Uint32(message))
}

// Witnesses of the pooled messages only: a message above the largest pool
// class is unpooled, and the collector owns it.
func retainLocalTransportPooledWitnesses(messages [][]byte) [][]byte {
	witnesses := [][]byte{}
	for _, message := range messages {
		if pooled, _ := clientconnect.MessagePoolCheck(message); pooled {
			witnesses = append(witnesses, retainResidentPoolWitness(message))
		}
	}
	return witnesses
}

// One attach of the pump fixture: its link, the queues standing in for the
// resident's routes, and the result of its runLocal.
type localTransportTestLink struct {
	link *localTransportLink
	// the resident's send route: the test writes it as the resident's writers
	routeSend chan []byte
	// the resident's receive route: the test reads it as the resident's reader
	routeReceive   chan []byte
	closeTransport atomic.Int64
	done           chan localTransportDetachReason
}

// A transport and a resident in one exchange. The resident's internal client
// is real but never sees an attach: each attach publishes test queues.
type localTransportPumpFixture struct {
	t              testing.TB
	settings       *ExchangeSettings
	control        *localTransportTestControl
	exchange       *Exchange
	exchangeCancel context.CancelFunc
	resident       *Resident
	transport      *ResidentTransport
	header         ExchangeHeader
	routeQueueSize int
	links          []*localTransportTestLink
	// called by each link's route removal, so a test can hold a detach there
	routeRemoval func()
}

func newLocalTransportPumpFixture(
	t testing.TB,
	ctx context.Context,
	settings *ExchangeSettings,
	routeQueueSize int,
) *localTransportPumpFixture {
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
	exchange.localTransport = newExchangeLocalTransportWithStore(exchangeCtx, settings, exchange.block, control.read)
	resident := newResidentCallbackLifecycleFixture(t, exchangeCtx, settings)
	resident.exchange = exchange
	resident.residentId = server.NewId()
	resident.instanceId = server.NewId()
	// as resident construction does
	resident.lastActivityNanos.Store(time.Now().UnixNano())
	exchange.residents[resident.clientId] = resident
	header := ExchangeHeader{
		Op:         ExchangeOpTransport,
		ClientId:   resident.clientId,
		ResidentId: resident.residentId,
	}
	transport := newResidentTransport(exchangeCtx, exchange, header, resident.clientId, resident.instanceId)
	return &localTransportPumpFixture{
		t:              t,
		settings:       settings,
		control:        control,
		exchange:       exchange,
		exchangeCancel: exchangeCancel,
		resident:       resident,
		transport:      transport,
		header:         header,
		routeQueueSize: routeQueueSize,
	}
}

// Attaches the transport through the production attach after its checks.
func (self *localTransportPumpFixture) attach() *localTransportTestLink {
	self.t.Helper()
	testLink := &localTransportTestLink{
		routeSend:    make(chan []byte, self.routeQueueSize),
		routeReceive: make(chan []byte, self.routeQueueSize),
		done:         make(chan localTransportDetachReason, 1),
	}
	testLink.link = self.exchange.attachLocalTransportRoutes(
		self.transport.ctx,
		self.exchange.localTransport,
		self.resident,
		self.header,
		func() (chan []byte, chan []byte, func(), error) {
			return testLink.routeSend, testLink.routeReceive, func() {
				testLink.closeTransport.Add(1)
				if routeRemoval := self.routeRemoval; routeRemoval != nil {
					routeRemoval()
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

// Runs the link's owner as ResidentTransport.Run does. A raise that escapes
// runLocal fails the test instead of the process.
func (self *localTransportPumpFixture) run(testLink *localTransportTestLink) {
	go func() {
		reason := localTransportDetachNone
		defer func() {
			if r := recover(); r != nil {
				self.t.Errorf("a raise escaped runLocal: %v", r)
			}
			testLink.done <- reason
		}()
		reason = self.transport.runLocal(testLink.link)
	}()
}

// Ends the transport and the resident and joins every owner. Messages left in
// the transport's queues are returned, as its Run returns them.
func (self *localTransportPumpFixture) close() {
	self.transport.Close()
	for _, testLink := range self.links {
		<-testLink.done
		// the done result is consumed; keep it readable for later checks
		testLink.done <- localTransportDetachNone
	}
	returnReadyPooledMessages(self.transport.send)
	returnReadyPooledMessages(self.transport.receive)
	if err := self.resident.CloseAndWait(context.Background()); err != nil {
		self.t.Error(err)
	}
	self.exchangeCancel()
}

// Reads the reason the link detached with.
func (self *localTransportTestLink) reason(t testing.TB) localTransportDetachReason {
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

// Receives the next message a pump delivers to queue, failing if the link
// detaches before the delivery.
func (self *localTransportTestLink) receive(t testing.TB, queue <-chan []byte, description string) []byte {
	t.Helper()
	select {
	case message := <-queue:
		return message
	case <-self.link.ctx.Done():
		select {
		case message := <-queue:
			return message
		default:
		}
		t.Fatalf("%s: the link detached before the delivery", description)
		return nil
	}
}

// Waits a bounded time for the link to detach and returns why.
func (self *localTransportTestLink) waitDetach(t testing.TB, description string) localTransportDetachReason {
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
func (self *localTransportTestLink) attached() bool {
	select {
	case <-self.link.ctx.Done():
		return false
	default:
		return true
	}
}

// The exchange_local and exchange_io totals of one label set, summed over
// operations for exchange_io.
type localTransportIOSnapshot struct {
	frames float64
	bytes  float64
}

func snapshotLocalTransportIO(direction string, kind string) localTransportIOSnapshot {
	return localTransportIOSnapshot{
		frames: testutil.ToFloat64(exchangeLocalFramesCounter.WithLabelValues(direction, kind)),
		bytes:  testutil.ToFloat64(exchangeLocalBytesCounter.WithLabelValues(direction, kind)),
	}
}

func snapshotLocalTransportDrops(boundary string) (float64, float64) {
	return testutil.ToFloat64(receiveQueueDroppedMessagesCounter.WithLabelValues(boundary)),
		testutil.ToFloat64(receiveQueueDroppedBytesCounter.WithLabelValues(boundary))
}

func snapshotLocalTransportDetaches(reason localTransportDetachReason) float64 {
	return testutil.ToFloat64(exchangeLocalDetachesCounter.WithLabelValues(localTransportDetachReasonLabels[reason]))
}

// Fails the test at any pool ownership violation and reports the count change.
func watchLocalTransportPoolViolations(t testing.TB) func() uint64 {
	t.Helper()
	before := clientconnect.MessagePoolViolationCount()
	previous := clientconnect.SetMessagePoolViolationHandler(func(err error, stack []byte) {
		t.Errorf("pool ownership violation: %v", err)
	})
	t.Cleanup(func() {
		clientconnect.SetMessagePoolViolationHandler(previous)
	})
	return func() uint64 {
		return clientconnect.MessagePoolViolationCount() - before
	}
}

// Both pumps deliver in FIFO order, gather batches bounded by
// ExchangeWriteBatchCount, and count each delivered frame once sent and once
// received with its frame header, as the socket counted it.
func TestLocalTransportPumpsKeepFrameOrderAndBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	violations := watchLocalTransportPoolViolations(t)
	settings := localTransportTestSettings()
	settings.ExchangeWriteBatchCount = 7
	f := newLocalTransportPumpFixture(t, ctx, settings, 4096)
	defer f.close()
	var batchLock sync.Mutex
	batchCounts := map[localTransportDirection][]int{}
	f.exchange.afterLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection, count int) {
		batchLock.Lock()
		defer batchLock.Unlock()
		batchCounts[direction] = append(batchCounts[direction], count)
	}
	testLink := f.attach()

	const messageCount = 1000
	var witnesses [][]byte
	wantBytes := 0
	// both directions are queued before the pumps start, so they gather full
	// batches
	for messageIndex := range messageCount {
		clientMessage := newLocalTransportTestMessage(messageIndex, 20+messageIndex%300)
		residentMessage := newLocalTransportTestMessage(messageIndex, 40+messageIndex%500)
		witnesses = append(witnesses, retainResidentPoolWitness(clientMessage), retainResidentPoolWitness(residentMessage))
		wantBytes += 2*exchangeIOFrameHeaderByteCount + len(clientMessage) + len(residentMessage)
		f.transport.send <- clientMessage
		testLink.routeSend <- residentMessage
	}
	sentBefore := snapshotLocalTransportIO("sent", "data")
	receivedBefore := snapshotLocalTransportIO("received", "data")
	f.run(testLink)

	read := func(queue <-chan []byte, wantByteCount func(int) int, description string) {
		for messageIndex := range messageCount {
			select {
			case message := <-queue:
				if got := localTransportTestMessageIndex(message); got != messageIndex || len(message) != wantByteCount(messageIndex) {
					t.Fatalf("%s message %d = index %d len %d, want len %d", description, messageIndex, got, len(message), wantByteCount(messageIndex))
				}
				clientconnect.MessagePoolReturn(message)
			case <-ctx.Done():
				t.Fatalf("%s delivered %d of %d", description, messageIndex, messageCount)
			}
		}
	}
	read(testLink.routeReceive, func(i int) int { return 20 + i%300 }, "client to resident")
	read(f.transport.receive, func(i int) int { return 40 + i%500 }, "resident to client")

	batchLock.Lock()
	for _, direction := range []localTransportDirection{localTransportDirectionClient, localTransportDirectionResident} {
		total := 0
		for _, count := range batchCounts[direction] {
			if count < 1 || settings.ExchangeWriteBatchCount < count {
				t.Errorf("direction %d gathered a batch of %d", direction, count)
			}
			total += count
		}
		if total != messageCount || len(batchCounts[direction]) == 0 || batchCounts[direction][0] != settings.ExchangeWriteBatchCount {
			t.Errorf("direction %d batches %v, want %d messages starting with a full batch", direction, batchCounts[direction], messageCount)
		}
	}
	batchLock.Unlock()

	sent := snapshotLocalTransportIO("sent", "data")
	received := snapshotLocalTransportIO("received", "data")
	if sent.frames-sentBefore.frames != 2*messageCount || received.frames-receivedBefore.frames != 2*messageCount {
		t.Errorf("local frames sent %v received %v, want %d each", sent.frames-sentBefore.frames, received.frames-receivedBefore.frames, 2*messageCount)
	}
	if sent.bytes-sentBefore.bytes != float64(wantBytes) || received.bytes-receivedBefore.bytes != float64(wantBytes) {
		t.Errorf("local bytes sent %v received %v, want %d each", sent.bytes-sentBefore.bytes, received.bytes-receivedBefore.bytes, wantBytes)
	}

	f.transport.Cancel()
	if reason := <-testLink.done; reason != localTransportDetachTransportDone {
		t.Errorf("detach reason = %d, want transport_done", reason)
	}
	testLink.done <- localTransportDetachTransportDone
	requireResidentPoolOwnersReturned(t, witnesses, "delivered local transport frames")
	if testLink.closeTransport.Load() != 1 {
		t.Errorf("route removal ran %d times, want once", testLink.closeTransport.Load())
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// A batch has WriteTimeout from its start: a delivery 1 ns inside it keeps
// the link, a batch blocked for all of it detaches at exactly WriteTimeout,
// two blocked deliveries of one batch share the one budget, and an idle link's
// next batch has the whole budget. Catches a per-delivery re-arm (which
// loosens the bound) and a deadline from before the batch (which shortens a
// healthy batch).
func TestLocalTransportBatchDeadlineStallsDetach(t *testing.T) {
	warmLocalTransportPool()

	// healthy: delivered 1 ns before the deadline, and the next batch has a
	// whole budget of its own
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), localTransportTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		f.transport.send <- newLocalTransportTestMessage(1, 64)
		f.run(testLink)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		clientconnect.MessagePoolReturn(testLink.receive(t, testLink.routeReceive, "a batch 1 ns inside its budget"))
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("a batch delivered inside its budget detached the link")
		}
		f.transport.send <- newLocalTransportTestMessage(2, 64)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		clientconnect.MessagePoolReturn(testLink.receive(t, testLink.routeReceive, "the next batch 1 ns inside its own budget"))
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("the next batch did not get a whole budget of its own")
		}
	})

	// stalled: detaches at exactly WriteTimeout, the batch returned and counted
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), localTransportTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		message := newLocalTransportTestMessage(1, 64)
		witness := retainResidentPoolWitness(message)
		f.transport.send <- message
		dropsBefore, dropBytesBefore := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend)
		detachesBefore := snapshotLocalTransportDetaches(localTransportDetachWriteTimeout)
		f.run(testLink)
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
		drops, dropBytes := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend)
		if drops-dropsBefore != 1 || dropBytes-dropBytesBefore != 64 {
			t.Errorf("stalled batch drops = %v (%v bytes), want 1 (64 bytes)", drops-dropsBefore, dropBytes-dropBytesBefore)
		}
		if snapshotLocalTransportDetaches(localTransportDetachWriteTimeout)-detachesBefore != 1 {
			t.Error("write_timeout detach not counted")
		}
	})

	// two blocked deliveries of one batch share its budget
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), localTransportTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		f.transport.send <- newLocalTransportTestMessage(1, 64)
		f.transport.send <- newLocalTransportTestMessage(2, 64)
		f.run(testLink)
		synctest.Wait()
		time.Sleep(10 * time.Second)
		clientconnect.MessagePoolReturn(testLink.receive(t, testLink.routeReceive, "the first message of a shared budget"))
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
		f := newLocalTransportPumpFixture(t, context.Background(), localTransportTestSettings(), 0)
		defer f.close()
		testLink := f.attach()
		f.run(testLink)
		time.Sleep(time.Hour)
		f.transport.send <- newLocalTransportTestMessage(1, 64)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		clientconnect.MessagePoolReturn(testLink.receive(t, testLink.routeReceive, "a batch after an idle hour"))
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("a batch after an idle hour did not get its whole budget")
		}
	})
}

// The resident pump has the same bound into the transport's receive queue,
// whose consumer is the client's writer. A stalled writer is modeled as a
// consumer that stops reading a full queue.
func TestLocalTransportResidentPumpDeadline(t *testing.T) {
	warmLocalTransportPool()
	settings := func() *ExchangeSettings {
		settings := localTransportTestSettings()
		settings.ExchangeBufferSize = 1
		return settings
	}

	// healthy: the writer resumes 1 ns before the deadline
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), settings(), 16)
		defer f.close()
		testLink := f.attach()
		f.transport.receive <- newLocalTransportTestMessage(0, 64)
		testLink.routeSend <- newLocalTransportTestMessage(1, 64)
		f.run(testLink)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		for messageIndex := range 2 {
			message := testLink.receive(t, f.transport.receive, "a resident batch 1 ns inside its budget")
			if localTransportTestMessageIndex(message) != messageIndex {
				t.Fatalf("resident message %d out of order", messageIndex)
			}
			clientconnect.MessagePoolReturn(message)
		}
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("a resident batch delivered inside its budget detached the link")
		}
	})

	// stalled: detaches at exactly WriteTimeout
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), settings(), 16)
		defer f.close()
		testLink := f.attach()
		f.transport.receive <- newLocalTransportTestMessage(0, 64)
		message := newLocalTransportTestMessage(1, 64)
		witness := retainResidentPoolWitness(message)
		testLink.routeSend <- message
		dropsBefore, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportReceive)
		f.run(testLink)
		synctest.Wait()
		time.Sleep(f.settings.WriteTimeout - time.Nanosecond)
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("the link detached before the resident batch's budget ended")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if testLink.attached() || testLink.reason(t) != localTransportDetachWriteTimeout {
			t.Fatal("a resident batch blocked for its whole budget did not detach with write_timeout")
		}
		requireResidentPoolOwnerReturned(t, witness, "stalled resident batch")
		if drops, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportReceive); drops-dropsBefore != 1 {
			t.Errorf("stalled resident batch drops = %v, want 1", drops-dropsBefore)
		}
	})
}

// An oversized message fails its whole batch before any delivery, as a failed
// exchange write does: the batch is returned and counted, the link detaches
// with max_message_len, and the next link carries the rest of the queue.
// Both framer limits apply: MaxMessageLen and the uint16 frame length.
func TestLocalTransportMaxMessageLenTearsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	violations := watchLocalTransportPoolViolations(t)

	cases := []struct {
		name          string
		maxMessageLen int
		oversizeLen   int
	}{
		{name: "MaxMessageLen", maxMessageLen: DefaultExchangeSettings().FramerSettings.MaxMessageLen, oversizeLen: DefaultExchangeSettings().FramerSettings.MaxMessageLen + 1},
		{name: "uint16 frame length", maxMessageLen: 2 * math.MaxUint16, oversizeLen: math.MaxUint16 + 1},
	}
	for _, c := range cases {
		settings := localTransportTestSettings()
		framerSettings := *settings.FramerSettings
		framerSettings.MaxMessageLen = c.maxMessageLen
		settings.FramerSettings = &framerSettings
		settings.ExchangeWriteBatchCount = 2
		f := newLocalTransportPumpFixture(t, ctx, settings, 16)

		// the client direction
		first := f.attach()
		failed := [][]byte{newLocalTransportTestMessage(1, 64), clientconnect.MessagePoolGet(c.oversizeLen)}
		witnesses := retainLocalTransportPooledWitnesses(failed)
		for _, message := range failed {
			f.transport.send <- message
		}
		f.transport.send <- newLocalTransportTestMessage(3, 64)
		f.transport.send <- newLocalTransportTestMessage(4, 64)
		dropsBefore, dropBytesBefore := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend)
		detachesBefore := snapshotLocalTransportDetaches(localTransportDetachMaxMessageLen)
		f.run(first)
		if reason := first.waitDetach(t, c.name+" oversized client batch"); reason != localTransportDetachMaxMessageLen {
			t.Fatalf("%s: client detach reason = %d, want max_message_len", c.name, reason)
		}
		if 0 < len(first.routeReceive) {
			t.Errorf("%s: a message of the failed batch was delivered", c.name)
		}
		requireResidentPoolOwnersReturned(t, witnesses, c.name+" failed client batch")
		drops, dropBytes := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend)
		if drops-dropsBefore != 2 || dropBytes-dropBytesBefore != float64(64+c.oversizeLen) {
			t.Errorf("%s: failed batch drops = %v (%v bytes), want 2 (%d bytes)", c.name, drops-dropsBefore, dropBytes-dropBytesBefore, 64+c.oversizeLen)
		}
		if snapshotLocalTransportDetaches(localTransportDetachMaxMessageLen)-detachesBefore != 1 {
			t.Errorf("%s: max_message_len detach not counted", c.name)
		}
		second := f.attach()
		f.run(second)
		for _, wantIndex := range []int{3, 4} {
			select {
			case message := <-second.routeReceive:
				if localTransportTestMessageIndex(message) != wantIndex {
					t.Errorf("%s: the next link delivered %d, want %d", c.name, localTransportTestMessageIndex(message), wantIndex)
				}
				clientconnect.MessagePoolReturn(message)
			case <-ctx.Done():
				t.Fatalf("%s: the next link did not carry the rest of the queue", c.name)
			}
		}

		// the resident direction
		f.transport.Cancel()
		<-second.done
		second.done <- localTransportDetachTransportDone
		f.close()
		f = newLocalTransportPumpFixture(t, ctx, settings, 16)
		third := f.attach()
		residentFailed := [][]byte{newLocalTransportTestMessage(5, 64), clientconnect.MessagePoolGet(c.oversizeLen)}
		residentWitnesses := retainLocalTransportPooledWitnesses(residentFailed)
		for _, message := range residentFailed {
			third.routeSend <- message
		}
		f.run(third)
		if reason := third.waitDetach(t, c.name+" oversized resident batch"); reason != localTransportDetachMaxMessageLen {
			t.Fatalf("%s: resident detach reason = %d, want max_message_len", c.name, reason)
		}
		if 0 < len(f.transport.receive) {
			t.Errorf("%s: a message of the failed resident batch was delivered", c.name)
		}
		requireResidentPoolOwnersReturned(t, residentWitnesses, c.name+" failed resident batch")
		f.close()
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// A zero-length message is a ping in both directions: it is returned, not
// delivered, counts as an exchange ping, and is resident activity.
func TestLocalTransportZeroLengthIsPing(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), localTransportTestSettings(), 16)
		defer f.close()
		testLink := f.attach()
		staleActivityNanos := time.Now().Add(-time.Hour).UnixNano()
		f.resident.lastActivityNanos.Store(staleActivityNanos)
		clientPing := clientconnect.MessagePoolGet(0)
		residentPing := clientconnect.MessagePoolGet(0)
		witnesses := [][]byte{retainResidentPoolWitness(clientPing), retainResidentPoolWitness(residentPing)}
		f.transport.send <- clientPing
		f.transport.send <- newLocalTransportTestMessage(1, 64)
		testLink.routeSend <- residentPing
		pingSentBefore := snapshotLocalTransportIO("sent", "ping")
		pingReceivedBefore := snapshotLocalTransportIO("received", "ping")
		f.run(testLink)
		synctest.Wait()

		if len(testLink.routeReceive) != 1 || len(f.transport.receive) != 0 {
			t.Fatalf("delivered %d to the resident and %d to the client, want only the data message", len(testLink.routeReceive), len(f.transport.receive))
		}
		if message := <-testLink.routeReceive; localTransportTestMessageIndex(message) != 1 {
			t.Error("the data message after the ping was not delivered")
		} else {
			clientconnect.MessagePoolReturn(message)
		}
		requireResidentPoolOwnersReturned(t, witnesses, "local pings")
		pingSent := snapshotLocalTransportIO("sent", "ping")
		pingReceived := snapshotLocalTransportIO("received", "ping")
		if pingSent.frames-pingSentBefore.frames != 2 || pingReceived.frames-pingReceivedBefore.frames != 2 ||
			pingSent.bytes-pingSentBefore.bytes != 2*exchangeIOFrameHeaderByteCount {
			t.Errorf("ping frames sent %v received %v bytes %v, want 2, 2, %d", pingSent.frames-pingSentBefore.frames, pingReceived.frames-pingReceivedBefore.frames, pingSent.bytes-pingSentBefore.bytes, 2*exchangeIOFrameHeaderByteCount)
		}
		if f.resident.lastActivityNanos.Load() <= staleActivityNanos {
			t.Error("a ping was not resident activity")
		}
		if !testLink.attached() {
			t.Error("a ping detached the link")
		}
	})
}

// Idle and attached past the resident ttl, the resident stays: an attached
// link stands in for the pings of a socket. Detached, it retires exactly one
// ttl after the detach, as after an idle socket's last ping.
func TestLocalAttachmentKeepsResidentActive(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportPumpFixture(t, context.Background(), localTransportTestSettings(), 16)
		defer f.close()
		ttl := f.settings.ExchangeResidentTtl
		if ttl != 300*time.Second {
			t.Fatalf("resident ttl = %s, want the production 300s", ttl)
		}
		// a resident with no link retires at exactly the ttl
		unattached := newResidentCallbackLifecycleFixture(t, f.exchange.ctx, f.settings)
		unattached.exchange = f.exchange
		unattached.lastActivityNanos.Store(time.Now().UnixNano())
		defer unattached.CloseAndWait(context.Background())
		go unattached.runIdleWatcher()
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			f.resident.runIdleWatcher()
		}()

		testLink := f.attach()
		f.run(testLink)
		time.Sleep(ttl - time.Nanosecond)
		synctest.Wait()
		if unattached.IsDone() {
			t.Fatal("the unattached control resident retired before the ttl")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !unattached.IsDone() {
			t.Fatal("the unattached control resident did not retire at the ttl")
		}
		time.Sleep(2*ttl + ttl/3)
		synctest.Wait()
		if f.resident.IsDone() {
			t.Fatal("an attached idle resident retired")
		}

		f.transport.Cancel()
		synctest.Wait()
		if reason := testLink.reason(t); reason != localTransportDetachTransportDone {
			t.Fatalf("detach reason = %d, want transport_done", reason)
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

// A slow resident fills its receive route and the pump waits, inside its
// budget, without dropping. The transport's send queue then fills: a reliable
// carrier message waits for it, and a datagram is refused at once, as before.
// Once the resident reads again everything arrives in order.
func TestLocalTransportBackpressure(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		settings := localTransportTestSettings()
		settings.ExchangeBufferSize = 4
		settings.ExchangeWriteBatchCount = 2
		f := newLocalTransportPumpFixture(t, context.Background(), settings, 2)
		defer f.close()
		testLink := f.attach()
		blocked := 0
		f.exchange.beforeLocalTransportBlockForTest = func(_ *localTransportLink, direction localTransportDirection) {
			if direction == localTransportDirectionClient {
				blocked += 1
			}
		}
		dropsBefore, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend)
		f.run(testLink)
		next := 0
		send := func() pooledMessageSendResult {
			result := f.transport.sendReceivedMessage(f.transport.ctx.Done(), newLocalTransportTestMessage(next, 64), clientconnect.CarrierReliabilityReliable)
			next += 1
			return result
		}
		// two fill the route; the pump takes one more and blocks on it; four
		// fill the send queue
		for range 2 + 1 + 4 {
			if result := send(); result != pooledMessageSendDelivered {
				t.Fatalf("message %d refused before the queues filled: %d", next-1, result)
			}
			synctest.Wait()
		}
		if blocked != 1 || len(testLink.routeReceive) != 2 || len(f.transport.send) != 4 {
			t.Fatalf("blocked=%d route=%d send=%d, want a blocked pump over full queues", blocked, len(testLink.routeReceive), len(f.transport.send))
		}
		waiting := make(chan struct{})
		f.transport.beforeReliableReceiveWaitForTest = func() { close(waiting) }
		reliableDone := make(chan pooledMessageSendResult, 1)
		go func() {
			reliableDone <- f.transport.sendReceivedMessage(f.transport.ctx.Done(), newLocalTransportTestMessage(7, 64), clientconnect.CarrierReliabilityReliable)
		}()
		<-waiting
		datagram := newLocalTransportTestMessage(99, 64)
		if result := f.transport.sendReceivedMessage(f.transport.ctx.Done(), datagram, clientconnect.CarrierReliabilityUnreliable); result != pooledMessageSendDropped {
			t.Fatalf("a datagram over a full send queue = %d, want refused at once", result)
		}
		if drops, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend); drops != dropsBefore {
			t.Fatal("the pump dropped under backpressure inside its budget")
		}

		for messageIndex := range 8 {
			message := <-testLink.routeReceive
			if localTransportTestMessageIndex(message) != messageIndex {
				t.Fatalf("resident read %d, want %d", localTransportTestMessageIndex(message), messageIndex)
			}
			clientconnect.MessagePoolReturn(message)
		}
		if result := <-reliableDone; result != pooledMessageSendDelivered {
			t.Fatalf("the waiting reliable message = %d, want delivered", result)
		}
		synctest.Wait()
		if !testLink.attached() {
			t.Fatal("backpressure inside the budget detached the link")
		}
	})
}

// Detaching at each barrier leaves every message with exactly one final
// owner and no pool violation: after a gather with the batch delivered, after
// a gather with the delivery waiting, at a blocked delivery when the resident
// closes, and with messages left on the routes and in a blocked resident pump.
func TestLocalTransportPoolOwnershipUnderTeardown(t *testing.T) {
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
		name           string
		routeQueueSize int
		bufferSize     int
		wantReason     localTransportDetachReason
		arm            func(f *localTransportPumpFixture)
		load           func(f *localTransportPumpFixture, testLink *localTransportTestLink) [][]byte
	}{
		{
			name:           "after a gather, the batch delivered",
			routeQueueSize: 16,
			bufferSize:     16,
			wantReason:     localTransportDetachTransportDone,
			arm: func(f *localTransportPumpFixture) {
				f.exchange.afterLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection, _ int) {
					if direction == localTransportDirectionClient {
						f.transport.Cancel()
					}
				}
			},
			load: func(f *localTransportPumpFixture, testLink *localTransportTestLink) [][]byte {
				batch, witnesses := messages(0, 5)
				for _, message := range batch {
					f.transport.send <- message
				}
				return witnesses
			},
		},
		{
			name:           "after a gather, the delivery waiting",
			routeQueueSize: 0,
			bufferSize:     16,
			wantReason:     localTransportDetachTransportDone,
			arm: func(f *localTransportPumpFixture) {
				f.exchange.afterLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection, _ int) {
					if direction == localTransportDirectionClient {
						f.transport.Cancel()
					}
				}
			},
			load: func(f *localTransportPumpFixture, testLink *localTransportTestLink) [][]byte {
				batch, witnesses := messages(0, 5)
				for _, message := range batch {
					f.transport.send <- message
				}
				return witnesses
			},
		},
		{
			name:           "a blocked delivery when the resident closes",
			routeQueueSize: 1,
			bufferSize:     16,
			wantReason:     localTransportDetachResidentDone,
			arm: func(f *localTransportPumpFixture) {
				f.exchange.beforeLocalTransportBlockForTest = func(_ *localTransportLink, direction localTransportDirection) {
					if direction == localTransportDirectionClient {
						f.resident.Cancel()
					}
				}
			},
			load: func(f *localTransportPumpFixture, testLink *localTransportTestLink) [][]byte {
				batch, witnesses := messages(0, 5)
				for _, message := range batch {
					f.transport.send <- message
				}
				return witnesses
			},
		},
		{
			name:           "queued routes and a blocked resident pump when the resident closes",
			routeQueueSize: 16,
			bufferSize:     1,
			wantReason:     localTransportDetachResidentDone,
			arm: func(f *localTransportPumpFixture) {
				f.settings.ExchangeWriteBatchCount = 2
				f.exchange.beforeLocalTransportBlockForTest = func(_ *localTransportLink, direction localTransportDirection) {
					if direction == localTransportDirectionResident {
						f.resident.Cancel()
					}
				}
			},
			load: func(f *localTransportPumpFixture, testLink *localTransportTestLink) [][]byte {
				queued, witnesses := messages(0, 1)
				f.transport.receive <- queued[0]
				resident, residentWitnesses := messages(1, 6)
				for _, message := range resident {
					testLink.routeSend <- message
				}
				return append(witnesses, residentWitnesses...)
			},
		},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			settings := localTransportTestSettings()
			settings.ExchangeBufferSize = c.bufferSize
			f := newLocalTransportPumpFixture(t, context.Background(), settings, c.routeQueueSize)
			testLink := f.attach()
			witnesses := c.load(f, testLink)
			c.arm(f)
			f.run(testLink)
			synctest.Wait()
			if testLink.attached() {
				t.Fatalf("%s: the link did not detach", c.name)
			}
			if reason := testLink.reason(t); reason != c.wantReason {
				t.Errorf("%s: detach reason = %d, want %d", c.name, reason, c.wantReason)
			}
			f.close()
			requireResidentPoolOwnersReturned(t, witnesses, c.name)
		})
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// Attach does the accept path's work in its order and detach undoes it: one
// accepted exchange connection, one attachment and one link while attached,
// none after, with route removal before the pumps are joined.
func TestLocalTransportAttachAccountingBalances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newLocalTransportPumpFixture(t, ctx, localTransportTestSettings(), 16)
	defer f.close()
	attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachmentsGauge)
	testLink := f.attach()
	clientId := f.resident.clientId
	if count := f.exchange.exchangeConnectionCounts[clientId]; count != 1 {
		t.Fatalf("accepted exchange connections = %d, want 1", count)
	}
	if f.resident.localAttachments.Load() != 1 || f.exchange.localTransport.linkCount() != 1 ||
		testutil.ToFloat64(exchangeLocalAttachmentsGauge) != attachmentsBefore+1 {
		t.Fatal("attach did not count its attachment and link")
	}
	f.run(testLink)
	f.transport.Cancel()
	if reason := <-testLink.done; reason != localTransportDetachTransportDone {
		t.Fatalf("detach reason = %d, want transport_done", reason)
	}
	testLink.done <- localTransportDetachTransportDone
	if _, ok := f.exchange.exchangeConnectionCounts[clientId]; ok {
		t.Error("detach left an accepted exchange connection")
	}
	if f.resident.localAttachments.Load() != 0 || f.exchange.localTransport.linkCount() != 0 ||
		testutil.ToFloat64(exchangeLocalAttachmentsGauge) != attachmentsBefore {
		t.Error("detach left its attachment or link")
	}
	if testLink.closeTransport.Load() != 1 {
		t.Errorf("route removal ran %d times, want once", testLink.closeTransport.Load())
	}
}

// Attach refuses what the accept path refuses and what the switch excludes:
// the switch off, a generation that is not current, a missing resident and a
// done resident. A refusal leaves no accounting behind.
func TestLocalTransportAttachRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := localTransportTestSettings()
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
	header := ExchangeHeader{Op: ExchangeOpTransport, ClientId: resident.clientId, ResidentId: resident.residentId}

	if exchange.attachLocalTransport(ctx, header) != nil {
		t.Error("attached with the ceiling off")
	}
	exchange.localTransport = newExchangeLocalTransportWithStore(ctx, settings, exchange.block, control.read)
	control.set("other-block")
	exchange.localTransport.refresh()
	if exchange.attachLocalTransport(ctx, header) != nil {
		t.Error("attached with the switch off for this block")
	}
	control.set("test-block")
	exchange.localTransport.refresh()
	stale := header
	stale.ResidentId = server.NewId()
	if exchange.attachLocalTransport(ctx, stale) != nil {
		t.Error("attached to a generation that is not current")
	}
	missing := header
	missing.ClientId = server.NewId()
	if exchange.attachLocalTransport(ctx, missing) != nil {
		t.Error("attached to a missing resident")
	}
	if len(exchange.residentChanges) != 0 || len(exchange.exchangeConnectionCounts) != 0 {
		t.Error("a refused attach left wait or connection state")
	}
	transport := newResidentTransport(ctx, exchange, header, resident.clientId, resident.instanceId)
	link := exchange.attachLocalTransport(transport.ctx, header)
	if link == nil {
		t.Fatal("refused a current resident with the switch on")
	}
	runDone := make(chan localTransportDetachReason, 1)
	go func() { runDone <- transport.runLocal(link) }()
	transport.Cancel()
	<-runDone
	resident.Cancel()
	if exchange.attachLocalTransport(ctx, header) != nil {
		t.Error("attached to a done resident")
	}
	if exchange.localTransport.linkCount() != 0 || len(exchange.exchangeConnectionCounts) != 0 || resident.localAttachments.Load() != 0 {
		t.Error("refusals or the detach left accounting behind")
	}
}

// The switch's control reads: the code default until the first read, a block
// list chooses blocks, a failed read keeps the last value, and the gauge
// follows.
func TestLocalTransportControlReadsTheSwitch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := localTransportTestSettings()
	settings.LocalTransportDefault = model.ConnectLocalTransportOff
	control := &localTransportTestControl{}
	localTransport := newExchangeLocalTransportWithStore(ctx, settings, "g1", control.read)
	if localTransport.allows() || testutil.ToFloat64(exchangeLocalTransportEnabledGauge) != 0 {
		t.Fatal("a dark default allowed attaching before the first read")
	}
	localTransport.refresh()
	if localTransport.allows() {
		t.Fatal("an absent control widened a dark default")
	}
	for _, c := range []struct {
		value string
		allow bool
	}{
		{value: "beta", allow: false},
		{value: "g1", allow: true},
		{value: "beta,g1", allow: true},
		{value: "off", allow: false},
		{value: "all", allow: true},
	} {
		control.set(c.value)
		localTransport.refresh()
		if localTransport.allows() != c.allow || (testutil.ToFloat64(exchangeLocalTransportEnabledGauge) == 1) != c.allow {
			t.Errorf("control %q: allows=%t, want %t", c.value, localTransport.allows(), c.allow)
		}
	}
	control.fail(errors.New("synthetic control read failure"))
	localTransport.refresh()
	if !localTransport.allows() {
		t.Error("a failed read dropped the last good value")
	}
	ceilingOff := localTransportTestSettings()
	ceilingOff.EnableLocalTransport = false
	if newExchangeLocalTransport(ctx, ceilingOff, "g1") != nil {
		t.Error("the ceiling off still built a switch that reads the control")
	}
}

// Narrowing switches new attaches to TCP at once and detaches live links at
// LocalTransportDisableDetachRate: one at the change, then one per interval.
// A message still in a transport's send queue survives for its next link,
// while the batch its pump held counts as dropped. Turning the switch on again
// stops the pacer.
func TestLocalTransportKillSwitchPacedDetach(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := localTransportTestSettings()
		settings.ExchangeWriteBatchCount = 1
		settings.LocalTransportDisableDetachRate = 10
		interval := time.Second / 10
		base := newLocalTransportPumpFixture(t, ctx, settings, 0)
		defer base.close()
		var pacerStarts atomic.Int64
		base.exchange.localTransport.afterPacerStartForTest = func() { pacerStarts.Add(1) }

		// five transports of five residents in one exchange
		const linkCount = 5
		fixtures := []*localTransportPumpFixture{base}
		for range linkCount - 1 {
			f := newLocalTransportPumpFixture(t, ctx, settings, 0)
			f.exchange = base.exchange
			f.resident.exchange = base.exchange
			base.exchange.residents[f.resident.clientId] = f.resident
			f.transport.exchange = base.exchange
			fixtures = append(fixtures, f)
			defer f.close()
		}
		testLinks := []*localTransportTestLink{}
		for _, f := range fixtures {
			testLink := f.attach()
			f.run(testLink)
			testLinks = append(testLinks, testLink)
		}
		// the first transport's pump holds one message; three more wait in
		// its send queue
		held := newLocalTransportTestMessage(0, 64)
		heldWitness := retainResidentPoolWitness(held)
		base.transport.send <- held
		synctest.Wait()
		for messageIndex := 1; messageIndex <= 3; messageIndex++ {
			base.transport.send <- newLocalTransportTestMessage(messageIndex, 64)
		}
		dropsBefore, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend)
		disabledBefore := snapshotLocalTransportDetaches(localTransportDetachDisabled)
		attachedCount := func() int {
			count := 0
			for _, testLink := range testLinks {
				if testLink.attached() {
					count += 1
				}
			}
			return count
		}

		base.control.set("other-block")
		base.exchange.localTransport.refresh()
		synctest.Wait()
		if count := attachedCount(); count != linkCount-1 {
			t.Fatalf("attached at the change = %d, want %d", count, linkCount-1)
		}
		if base.exchange.attachLocalTransport(ctx, base.header) != nil {
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
		base.control.set("all")
		base.exchange.localTransport.refresh()
		time.Sleep(10 * interval)
		synctest.Wait()
		if count := attachedCount(); count != linkCount-3 {
			t.Fatalf("attached after the switch turned on again = %d, want %d", count, linkCount-3)
		}
		// off again: the rest detach at the pace
		base.control.set("off")
		base.exchange.localTransport.refresh()
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
		if count := snapshotLocalTransportDetaches(localTransportDetachDisabled) - disabledBefore; count != linkCount {
			t.Errorf("disabled detaches = %v, want %d", count, linkCount)
		}
		requireResidentPoolOwnerReturned(t, heldWitness, "the batch a disabled link held")
		if drops, _ := snapshotLocalTransportDrops(receiveQueueBoundaryLocalTransportSend); drops-dropsBefore != 1 {
			t.Errorf("drops = %v, want the one held message", drops-dropsBefore)
		}
		if len(base.transport.send) != 3 {
			t.Fatalf("the send queue holds %d, want the 3 messages for the next link", len(base.transport.send))
		}
		for messageIndex := 1; messageIndex <= 3; messageIndex++ {
			message := <-base.transport.send
			if localTransportTestMessageIndex(message) != messageIndex {
				t.Errorf("send queue message %d out of order", messageIndex)
			}
			clientconnect.MessagePoolReturn(message)
		}
		// one pacer per turn off, each ending when it had nothing left to do
		if pacerStarts.Load() != 2 || base.exchange.localTransport.isPacing() {
			t.Errorf("pacer starts = %d pacing=%t, want 2 and none left", pacerStarts.Load(), base.exchange.localTransport.isPacing())
		}
		cancel()
	})
}

// A rate of zero is the alternative of detaching none: the switch then
// reaches only new attaches.
func TestLocalTransportKillSwitchRateZeroKeepsLiveLinks(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		settings := localTransportTestSettings()
		settings.LocalTransportDisableDetachRate = 0
		f := newLocalTransportPumpFixture(t, context.Background(), settings, 16)
		defer f.close()
		f.exchange.localTransport.afterPacerStartForTest = func() { t.Error("a zero rate started a pacer") }
		testLink := f.attach()
		f.run(testLink)
		f.control.set("off")
		f.exchange.localTransport.refresh()
		time.Sleep(time.Minute)
		synctest.Wait()
		if !testLink.attached() {
			t.Error("a zero rate detached a live link")
		}
		if f.exchange.attachLocalTransport(context.Background(), f.header) != nil {
			t.Error("a zero rate allowed a new attach")
		}
	})
}

// The connect service ships the ceiling on and the switch dark; every other
// exchange keeps the ceiling off.
func TestLocalTransportShipsDark(t *testing.T) {
	defaults := DefaultExchangeSettings()
	if defaults.EnableLocalTransport {
		t.Error("the default exchange settings allow local transport")
	}
	runSettings := exchangeSettingsForRun(RunOptions{Port: 443})
	if !runSettings.EnableLocalTransport {
		t.Error("the connect service does not allow local transport")
	}
	if runSettings.LocalTransportDefault != model.ConnectLocalTransportOff {
		t.Errorf("the connect service's default = %q, want off", runSettings.LocalTransportDefault)
	}
	control := model.NarrowConnectMoveControl(nil, model.ConnectMoveControlCeiling{
		EnableLocalTransport:  runSettings.EnableLocalTransport,
		LocalTransportDefault: runSettings.LocalTransportDefault,
	})
	for _, block := range []string{"beta", "g1", "g2", "g3", "g4"} {
		if control.LocalTransportAllows(block) {
			t.Errorf("block %s attaches with no control", block)
		}
	}
	if runSettings.LocalTransportDisableDetachRate != 500 || runSettings.LocalTransportControlRefreshInterval != 10*time.Second {
		t.Errorf("pacing %d/s refresh %s, want 500/s and 10s", runSettings.LocalTransportDisableDetachRate, runSettings.LocalTransportControlRefreshInterval)
	}
}

// The switch turning off between an attach's checks and its publication
// refuses the attach, so no link escapes a pacer that already ran. Once
// published, a link is the pacer's to detach.
func TestLocalTransportKillSwitchDuringAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newLocalTransportPumpFixture(t, ctx, localTransportTestSettings(), 16)
	defer f.close()
	f.exchange.beforeLocalTransportAttachForTest = func(*Resident) {
		f.control.set("off")
		f.exchange.localTransport.refresh()
	}
	transport := newResidentTransport(ctx, f.exchange, f.header, f.resident.clientId, f.resident.instanceId)
	defer transport.Close()
	if link := f.exchange.attachLocalTransport(transport.ctx, f.header); link != nil {
		t.Fatal("an attach published its link after the switch turned off")
	}
	if f.exchange.localTransport.linkCount() != 0 || len(f.exchange.exchangeConnectionCounts) != 0 || f.resident.localAttachments.Load() != 0 {
		t.Error("the refused attach left accounting behind")
	}
}

// Exchange.WaitForIdle covers the resident's side of a link as it covers an
// accepted socket: after Close it returns only once the link's detach has
// completed, so the pool and the exchange's accounting are final the moment
// it returns. The detach is held in its route removal to make the order
// deterministic.
func TestLocalTransportWaitForIdleJoinsLinkDetach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	violations := watchLocalTransportPoolViolations(t)
	f := newLocalTransportPumpFixture(t, ctx, localTransportTestSettings(), 16)
	defer f.close()
	attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachmentsGauge)
	routeRemovalEntered := make(chan struct{})
	routeRemovalRelease := make(chan struct{})
	// a failure must not leave the detach held under the fixture's close
	releaseRouteRemoval := sync.OnceFunc(func() { close(routeRemovalRelease) })
	defer releaseRouteRemoval()
	f.routeRemoval = func() {
		close(routeRemovalEntered)
		<-routeRemovalRelease
	}
	testLink := f.attach()
	// a delivered message the resident has not read yet: the detach returns it
	queued := newLocalTransportTestMessage(1, 64)
	witness := retainResidentPoolWitness(queued)
	testLink.routeReceive <- queued
	f.run(testLink)

	// the exchange's residents end with it, and the link detaches
	f.exchange.Close()
	select {
	case <-routeRemovalEntered:
	case <-ctx.Done():
		t.Fatal("the link did not detach when the exchange closed")
	}
	heldCtx, heldCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	idle := f.exchange.WaitForIdle(heldCtx)
	heldCancel()
	if idle {
		t.Fatal("WaitForIdle returned while a link's detach was still removing its routes")
	}
	releaseRouteRemoval()
	if !f.exchange.WaitForIdle(ctx) {
		t.Fatal("WaitForIdle did not return after the detach completed")
	}
	// final at once
	requireResidentPoolOwnerReturned(t, witness, "a message left on a route at the exchange's close")
	if len(f.exchange.exchangeConnectionCounts) != 0 || f.exchange.localTransport.linkCount() != 0 ||
		f.resident.localAttachments.Load() != 0 || testutil.ToFloat64(exchangeLocalAttachmentsGauge) != attachmentsBefore {
		t.Error("the exchange's accounting was not final when WaitForIdle returned")
	}
	// the close ends the resident and the transport together, and either end
	// may reach the link first
	if reason := testLink.reason(t); reason != localTransportDetachResidentDone && reason != localTransportDetachTransportDone {
		t.Errorf("detach reason = %d, want resident_done or transport_done", reason)
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// The control loop is an exchange worker: WaitForIdle after Close waits for a
// read in progress, and the loop reads nothing after.
func TestLocalTransportWaitForIdleJoinsTheControlLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := localTransportTestSettings()
	exchangeCtx, exchangeCancel := context.WithCancel(ctx)
	exchange := &Exchange{
		ctx:       exchangeCtx,
		cancel:    exchangeCancel,
		settings:  settings,
		block:     "test-block",
		residents: map[server.Id]*Resident{},
	}
	readEntered := make(chan struct{}, 16)
	readRelease := make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	defer releaseRead()
	var readCount atomic.Int64
	read := func(context.Context, model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
		readCount.Add(1)
		readEntered <- struct{}{}
		// a store slower than the cancellation
		<-readRelease
		return nil, errors.New("synthetic slow store")
	}
	exchange.localTransport = newExchangeLocalTransportWithStore(exchangeCtx, settings, exchange.block, read)
	exchange.localTransport.start()
	<-readEntered
	exchange.Close()
	heldCtx, heldCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	idle := exchange.WaitForIdle(heldCtx)
	heldCancel()
	if idle {
		t.Fatal("WaitForIdle returned while the control loop was in a read")
	}
	releaseRead()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("WaitForIdle did not return after the read")
	}
	if count := readCount.Load(); count != 1 {
		t.Errorf("the closed exchange's control loop read %d times, want the one in progress", count)
	}
}

// With the switch off and never turned off, an exchange runs only its control
// loop, which reads every interval on one reused timer. A pacer starts only
// when the switch turns off with live links, and ends when it has detached
// them.
func TestLocalTransportPacerStartsOnlyWhenTheSwitchTurnsOff(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		settings := localTransportTestSettings()
		settings.LocalTransportDefault = model.ConnectLocalTransportOff
		f := newLocalTransportPumpFixture(t, context.Background(), settings, 16)
		defer f.close()
		localTransport := f.exchange.localTransport
		var pacerStarts atomic.Int64
		localTransport.afterPacerStartForTest = func() { pacerStarts.Add(1) }
		var readCount atomic.Int64
		read := localTransport.read
		localTransport.read = func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
			readCount.Add(1)
			return read(ctx, ceiling)
		}

		// dark: the loop reads at the interval and nothing else runs
		f.control.set("off")
		localTransport.start()
		interval := settings.LocalTransportControlRefreshInterval
		time.Sleep(3*interval + interval/2)
		synctest.Wait()
		if readCount.Load() != 4 || pacerStarts.Load() != 0 || localTransport.isPacing() {
			t.Fatalf("dark: reads=%d pacer starts=%d pacing=%t, want 4, 0, false", readCount.Load(), pacerStarts.Load(), localTransport.isPacing())
		}

		// turned on, and off again with no live link: still no pacer
		f.control.set("all")
		time.Sleep(interval)
		synctest.Wait()
		f.control.set("off")
		time.Sleep(interval)
		synctest.Wait()
		if pacerStarts.Load() != 0 {
			t.Fatalf("a turn off with no live link started %d pacers", pacerStarts.Load())
		}

		// on with a live link, then off: one pacer, which ends with the link
		f.control.set("all")
		time.Sleep(interval)
		synctest.Wait()
		testLink := f.attach()
		f.run(testLink)
		f.control.set("off")
		time.Sleep(interval)
		synctest.Wait()
		if pacerStarts.Load() != 1 || localTransport.isPacing() || testLink.attached() {
			t.Fatalf("turn off with a link: pacer starts=%d pacing=%t attached=%t, want 1, false, false", pacerStarts.Load(), localTransport.isPacing(), testLink.attached())
		}
		// staying off starts nothing more
		time.Sleep(3 * interval)
		synctest.Wait()
		if pacerStarts.Load() != 1 {
			t.Fatalf("staying off started %d pacers, want the one", pacerStarts.Load())
		}
		f.exchange.Close()
		if !f.exchange.WaitForIdle(context.Background()) {
			t.Fatal("the control loop did not end with the exchange")
		}
	})
}

// A raise in either pump detaches the link with raised and returns the batch
// the pump held, counted as dropped, instead of leaking it or escaping the
// transport's Run.
func TestLocalTransportPumpRaiseDetachesAndReturnsTheBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	violations := watchLocalTransportPoolViolations(t)
	for _, c := range []struct {
		name      string
		direction localTransportDirection
		boundary  string
	}{
		{name: "client pump", direction: localTransportDirectionClient, boundary: receiveQueueBoundaryLocalTransportSend},
		{name: "resident pump", direction: localTransportDirectionResident, boundary: receiveQueueBoundaryLocalTransportReceive},
	} {
		f := newLocalTransportPumpFixture(t, ctx, localTransportTestSettings(), 16)
		testLink := f.attach()
		var witnesses [][]byte
		for index := range 3 {
			message := newLocalTransportTestMessage(index, 64)
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			if c.direction == localTransportDirectionClient {
				f.transport.send <- message
			} else {
				testLink.routeSend <- message
			}
		}
		f.exchange.afterLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection, _ int) {
			if direction == c.direction {
				panic("synthetic local transport pump raise")
			}
		}
		dropsBefore, _ := snapshotLocalTransportDrops(c.boundary)
		raisedBefore := snapshotLocalTransportDetaches(localTransportDetachRaised)
		f.run(testLink)
		if reason := testLink.waitDetach(t, c.name); reason != localTransportDetachRaised {
			t.Errorf("%s: detach reason = %d, want raised", c.name, reason)
		}
		requireResidentPoolOwnersReturned(t, witnesses, c.name+" batch at the raise")
		if drops, _ := snapshotLocalTransportDrops(c.boundary); drops-dropsBefore != 3 {
			t.Errorf("%s: drops = %v, want the 3 messages it held", c.name, drops-dropsBefore)
		}
		if snapshotLocalTransportDetaches(localTransportDetachRaised)-raisedBefore != 1 {
			t.Errorf("%s: raised detach not counted", c.name)
		}
		if 0 < len(testLink.routeReceive) || 0 < len(f.transport.receive) {
			t.Errorf("%s: a message of the raised batch was delivered", c.name)
		}
		f.close()
	}
	if count := violations(); count != 0 {
		t.Errorf("%d pool violations", count)
	}
}

// A pump whose link ends while it gathers takes nothing more from its source,
// so the transport's next link carries the rest of the queue.
func TestLocalTransportCanceledPumpStopsGathering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := localTransportTestSettings()
	if settings.ExchangeWriteBatchCount < 256 {
		t.Fatal("the production batch bound changed")
	}
	f := newLocalTransportPumpFixture(t, ctx, settings, 4096)
	defer f.close()
	testLink := f.attach()
	const queued = 300
	for index := range queued {
		f.transport.send <- newLocalTransportTestMessage(index, 64)
	}
	gathered := 0
	f.exchange.beforeLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection) {
		if direction == localTransportDirectionClient {
			f.transport.Cancel()
		}
	}
	f.exchange.afterLocalTransportGatherForTest = func(_ *localTransportLink, direction localTransportDirection, count int) {
		if direction == localTransportDirectionClient {
			gathered += count
		}
	}
	f.run(testLink)
	if reason := testLink.waitDetach(t, "a pump canceled mid-gather"); reason != localTransportDetachTransportDone {
		t.Fatalf("detach reason = %d, want transport_done", reason)
	}
	if gathered != 1 || len(f.transport.send) != queued-1 {
		t.Fatalf("gathered %d with %d left queued, want the one it held and %d left", gathered, len(f.transport.send), queued-1)
	}
}

// A detach removes the routes before it drains them, so the resident's own
// writer blocked on the detaching route keeps its frame for the routes that
// remain, as when a socket closes: the route update wakes it to retry. A
// drain first would take the blocked frame into the dying route and drop it
// after the writer counted it delivered.
func TestLocalTransportDetachRemovesRoutesBeforeTheDrain(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := localTransportTestSettings()
		settings.ExchangeBufferSize = 1
		// the pump holds exactly one frame
		settings.ExchangeWriteBatchCount = 1
		exchange := &Exchange{
			ctx:             ctx,
			cancel:          cancel,
			settings:        settings,
			block:           "test-block",
			residents:       map[server.Id]*Resident{},
			residentChanges: map[server.Id]chan struct{}{},
			connections:     map[server.Id]map[server.Id]context.CancelFunc{},
		}
		exchange.localTransport = newExchangeLocalTransportWithStore(ctx, settings, exchange.block, (&localTransportTestControl{}).read)
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		resident.exchange = exchange
		resident.residentId = server.NewId()
		resident.instanceId = server.NewId()
		resident.lastActivityNanos.Store(time.Now().UnixNano())
		exchange.residents[resident.clientId] = resident
		header := ExchangeHeader{Op: ExchangeOpTransport, ClientId: resident.clientId, ResidentId: resident.residentId}
		transport := newResidentTransport(ctx, exchange, header, resident.clientId, resident.instanceId)
		// the client's writer is stalled: its receive queue is full
		transport.receive <- newLocalTransportTestMessage(1000, 64)
		link := exchange.attachLocalTransport(transport.ctx, header)
		if link == nil {
			t.Fatal("attach refused")
		}
		runDone := make(chan localTransportDetachReason, 1)
		go func() { runDone <- transport.runLocal(link) }()
		// the client's second transport, whose send route the test reads
		otherSend, otherReceive, closeOther, err := resident.AddTransport()
		if err != nil {
			t.Fatal(err)
		}

		// the resident's real route writer fills the pump and both routes, then
		// blocks on both
		writer := resident.client.RouteManager().OpenMultiRouteWriter(clientconnect.DestinationId(clientconnect.Id(resident.clientId)))
		writerCtx, writerCancel := context.WithCancel(ctx)
		const blockedIndex = 3
		var writtenCount atomic.Int64
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			for index := 0; index <= blockedIndex; index++ {
				frame := newLocalTransportTestMessage(index, 64)
				if delivered, err := writer.WriteDetailed(writerCtx, frame, time.Hour); err != nil || !delivered {
					clientconnect.MessagePoolReturn(frame)
					return
				}
				writtenCount.Add(1)
			}
		}()
		synctest.Wait()
		if writtenCount.Load() != blockedIndex || len(link.routeSend) != 1 || len(otherSend) != 1 {
			t.Fatalf("written=%d routes %d and %d, want %d written over two full routes", writtenCount.Load(), len(link.routeSend), len(otherSend), blockedIndex)
		}

		transport.Cancel()
		if reason := <-runDone; reason != localTransportDetachTransportDone {
			t.Fatalf("detach reason = %d, want transport_done", reason)
		}
		// the remaining route frees one slot: the blocked frame must take it
		clientconnect.MessagePoolReturn(<-otherSend)
		synctest.Wait()
		select {
		case frame := <-otherSend:
			if index := localTransportTestMessageIndex(frame); index != blockedIndex {
				t.Errorf("the remaining route carried %d, want the blocked frame %d", index, blockedIndex)
			}
			clientconnect.MessagePoolReturn(frame)
		default:
			t.Error("the frame a resident writer had blocked on the detaching route was dropped with it")
		}
		if left := len(link.routeSend); left != 0 {
			t.Errorf("the detached route kept %d frames", left)
		}

		writerCancel()
		<-writerDone
		resident.client.RouteManager().CloseMultiRouteWriter(writer)
		closeOther()
		returnReadyPooledMessages(otherSend)
		returnReadyPooledMessages(otherReceive)
		returnReadyPooledMessages(link.routeSend)
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		returnReadyPooledMessages(transport.receive)
		returnReadyPooledMessages(transport.send)
	})
}
