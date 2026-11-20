// Composed local forward tests: the real ResidentForward loop from a source
// resident's forward queue to its destination's resident, delivered in process
// or over the real exchange accept path into the same AddForward queue and
// consumer. Only resident discovery and the consumer's client forward are held:
// the loop's lookup returns the test's record, and the destination's forward
// consumer records what reaches it.
package connect

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The message indices one destination resident's forward consumer received,
// in order.
type localForwardReceived struct {
	stateLock    sync.Mutex
	indices      []int
	byteCounts   []int
	changed      chan struct{}
	receiveCount atomic.Int64
}

func newLocalForwardReceived() *localForwardReceived {
	return &localForwardReceived{changed: make(chan struct{}, 1)}
}

// A forward consumer that records and takes each message, as
// Client.ForwardWithTimeout takes one it accepts.
func (self *localForwardReceived) receive(_ context.Context, message []byte) bool {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		index := -1
		if 4 <= len(message) {
			index = localTransportTestMessageIndex(message)
		}
		self.indices = append(self.indices, index)
		self.byteCounts = append(self.byteCounts, len(message))
	}()
	self.receiveCount.Add(1)
	clientconnect.MessagePoolReturn(message)
	select {
	case self.changed <- struct{}{}:
	default:
	}
	return true
}

func (self *localForwardReceived) snapshot() ([]int, []int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]int(nil), self.indices...), append([]int(nil), self.byteCounts...)
}

// Waits until count messages arrived and returns their indices.
func (self *localForwardReceived) wait(t testing.TB, ctx context.Context, count int, description string) []int {
	t.Helper()
	for {
		indices, _ := self.snapshot()
		if count <= len(indices) {
			return indices
		}
		select {
		case <-self.changed:
		case <-ctx.Done():
			t.Fatalf("%s received %d of %d", description, len(indices), count)
		}
	}
}

// One destination's resident and a source resident's forward to it.
type localForwardComposedFixture struct {
	t        testing.TB
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ExchangeSettings
	control  *localTransportTestControl
	exchange *Exchange

	stateLock sync.Mutex
	// the resident the record names
	recordResident *Resident
	residents      []*Resident
	received       map[*Resident]*localForwardReceived

	forward *ResidentForward
	runDone chan struct{}

	dials         atomic.Int64
	dialTimes     chan time.Time
	socketWorkers sync.WaitGroup
	// a closed run has joined every owner
	closed bool
}

// A fixture whose exchange delivers forwards in process when local is true,
// and over the exchange accept path otherwise. mutate adjusts the settings
// before the exchange reads them.
func newLocalForwardComposedFixture(t testing.TB, local bool, mutate func(*ExchangeSettings)) *localForwardComposedFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := localForwardTestSettings()
	// no exchange pings in a test window
	settings.ExchangePingTimeout = time.Hour
	settings.ExchangeReadTimeout = 3 * time.Hour
	settings.ForwardIdleTimeout = 3 * time.Hour
	if mutate != nil {
		mutate(settings)
	}
	exchange := &Exchange{
		ctx:             ctx,
		cancel:          cancel,
		settings:        settings,
		block:           "test-block",
		residents:       map[server.Id]*Resident{},
		residentChanges: map[server.Id]chan struct{}{},
		connections:     map[server.Id]map[server.Id]context.CancelFunc{},
	}
	f := &localForwardComposedFixture{
		t:         t,
		ctx:       ctx,
		cancel:    cancel,
		settings:  settings,
		control:   &localTransportTestControl{},
		exchange:  exchange,
		received:  map[*Resident]*localForwardReceived{},
		dialTimes: make(chan time.Time, 64),
		runDone:   make(chan struct{}),
	}
	if local {
		exchange.localForward = newExchangeLocalOpWithStore(ctx, settings, exchange.block, ExchangeOpForward, f.control.read)
	}
	settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		f.dials.Add(1)
		select {
		case f.dialTimes <- time.Now():
		default:
		}
		local, remote := net.Pipe()
		f.socketWorkers.Add(1)
		go func() {
			defer f.socketWorkers.Done()
			exchange.handleExchangeConnection(remote)
		}()
		return local, nil
	}
	resident := f.newResident(server.NewId())
	f.recordResident = resident
	exchange.residents[resident.clientId] = resident
	f.forward = NewResidentForward(ctx, exchange, resident.clientId)
	return f
}

// A resident of the destination client whose forward consumer records what it
// receives.
func (self *localForwardComposedFixture) newResident(clientId server.Id) *Resident {
	resident := newResidentCallbackLifecycleFixture(self.t, self.ctx, self.settings)
	resident.exchange = self.exchange
	if self.recordResident != nil {
		resident.clientId = self.recordResident.clientId
		resident.instanceId = self.recordResident.instanceId
	} else {
		resident.clientId = clientId
		resident.instanceId = server.NewId()
	}
	resident.residentId = server.NewId()
	resident.lastActivityNanos.Store(time.Now().UnixNano())
	received := newLocalForwardReceived()
	resident.forwardReceiveForTest = received.receive
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.residents = append(self.residents, resident)
	self.received[resident] = received
	return resident
}

// The record the loop resolves: the resident the test last named.
func (self *localForwardComposedFixture) lookup(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &model.NetworkClientResident{
		ClientId:              self.recordResident.clientId,
		InstanceId:            self.recordResident.instanceId,
		ResidentId:            self.recordResident.residentId,
		ResidentHost:          "local-forward.invalid",
		ResidentInternalPorts: []int{1},
	}
}

// Replaces the record and the exchange's resident, as a nomination does.
func (self *localForwardComposedFixture) replaceResident(replacement *Resident) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.recordResident = replacement
	}()
	var replaced *Resident
	func() {
		self.exchange.stateLock.Lock()
		defer self.exchange.stateLock.Unlock()
		replaced = self.exchange.residents[replacement.clientId]
		self.exchange.residents[replacement.clientId] = replacement
		self.exchange.notifyResidentChangedLocked(replacement.clientId)
	}()
	if replaced != nil {
		replaced.Cancel()
	}
}

func (self *localForwardComposedFixture) receivedBy(resident *Resident) *localForwardReceived {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.received[resident]
}

func (self *localForwardComposedFixture) run() {
	go func() {
		defer close(self.runDone)
		self.forward.runWithResidentLookup(self.lookup)
	}()
}

// The forward admits count messages, of sizes that vary across batch and pool
// boundaries, as forward admission does.
func (self *localForwardComposedFixture) send(firstIndex int, count int) {
	for index := firstIndex; index < firstIndex+count; index++ {
		message := newLocalTransportTestMessage(index, localForwardComposedByteCount(index))
		select {
		case self.forward.send <- message:
			self.forward.UpdateActivity()
		case <-self.ctx.Done():
			clientconnect.MessagePoolReturn(message)
			self.t.Fatalf("message %d refused", index)
		}
	}
}

func localForwardComposedByteCount(index int) int {
	return 8 + (index*37)%1400
}

// Ends the forward, then the residents, and joins every owner.
func (self *localForwardComposedFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.forward.Close()
	<-self.runDone
	self.stateLock.Lock()
	residents := append([]*Resident(nil), self.residents...)
	self.stateLock.Unlock()
	for _, resident := range residents {
		if err := resident.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	self.cancel()
	self.socketWorkers.Wait()
}

// Fails unless indices are firstIndex, firstIndex+1, ... in order.
func requireLocalForwardIndices(t testing.TB, indices []int, firstIndex int, description string) {
	t.Helper()
	for offset, index := range indices {
		if index != firstIndex+offset {
			t.Fatalf("%s received %d at %d, want %d", description, index, offset, firstIndex+offset)
		}
	}
}

func localForwardDropSnapshot() map[string]float64 {
	drops := map[string]float64{}
	for _, boundary := range []string{
		receiveQueueBoundaryExchangeAcceptForward,
		receiveQueueBoundaryExchangeForwardSend,
		receiveQueueBoundaryLocalForwardSend,
	} {
		drops[boundary] = testutil.ToFloat64(receiveQueueDroppedMessagesCounter.WithLabelValues(boundary))
	}
	drops["forward_receive_dropped"] = testutil.ToFloat64(forwardReceiveDroppedCounter)
	return drops
}

// A forward whose destination's resident is in this process opens no exchange
// socket, and its messages reach the resident's forward consumer in order
// through the link. Catches removing the branch.
func TestLocalForwardDialsNoExchangeSocket(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardComposedFixture(t, true, nil)
		defer f.close()
		attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward])
		f.run()
		f.send(0, 1)
		synctest.Wait()
		if f.exchange.localForward.linkCount() != 1 || testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) != attachmentsBefore+1 {
			t.Fatal("the forward did not attach in process")
		}
		const messageCount = 300
		f.send(1, messageCount-1)
		waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
		defer waitCancel()
		requireLocalForwardIndices(t, f.receivedBy(f.recordResident).wait(t, waitCtx, messageCount, "resident"), 0, "resident")
		if dials := f.dials.Load(); dials != 0 {
			t.Fatalf("a local forward dialed %d exchange sockets", dials)
		}
		f.close()
		if testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) != attachmentsBefore {
			t.Error("the forward's end left its attachment")
		}
	})
}

// One script through the in-process link and through the exchange socket
// gives the destination's forward consumer the same messages in the same
// order, the same drop counters (none), and the same exchange accounting: each
// admitted message is one data frame sent and one received, with its 4-byte
// header, in exchange_local on one path and exchange_io on the other, both
// under op forward. Catches any semantic drift between the paths.
func TestLocalAndTcpForwardsDeliverIdenticalSequences(t *testing.T) {
	warmLocalTransportPool()
	type pathResult struct {
		indices    []int
		byteCounts []int
		drops      map[string]float64
		dials      int64
		sent       localTransportIOSnapshot
		received   localTransportIOSnapshot
	}
	const messageCount = 400
	wantBytes := 0
	for index := range messageCount {
		wantBytes += exchangeIOFrameHeaderByteCount + localForwardComposedByteCount(index)
	}
	run := func(local bool) pathResult {
		var result pathResult
		synctest.Test(t, func(t *testing.T) {
			f := newLocalForwardComposedFixture(t, local, nil)
			defer f.close()
			dropsBefore := localForwardDropSnapshot()
			snapshot := func(direction string) localTransportIOSnapshot {
				if local {
					return snapshotLocalForwardIO(direction, "data")
				}
				ioSnapshot := snapshotExchangeIOOp("forward", direction, "data")
				return localTransportIOSnapshot{frames: ioSnapshot.frames, bytes: ioSnapshot.bytes}
			}
			sentBefore := snapshot("sent")
			receivedBefore := snapshot("received")
			f.run()
			f.send(0, messageCount)
			waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
			defer waitCancel()
			f.receivedBy(f.recordResident).wait(t, waitCtx, messageCount, "resident")
			synctest.Wait()
			result.indices, result.byteCounts = f.receivedBy(f.recordResident).snapshot()
			sent := snapshot("sent")
			received := snapshot("received")
			result.sent = localTransportIOSnapshot{frames: sent.frames - sentBefore.frames, bytes: sent.bytes - sentBefore.bytes}
			result.received = localTransportIOSnapshot{frames: received.frames - receivedBefore.frames, bytes: received.bytes - receivedBefore.bytes}
			result.dials = f.dials.Load()
			result.drops = map[string]float64{}
			for boundary, count := range localForwardDropSnapshot() {
				result.drops[boundary] = count - dropsBefore[boundary]
			}
		})
		return result
	}
	local := run(true)
	tcp := run(false)

	if local.dials != 0 || tcp.dials != 1 {
		t.Errorf("dials local=%d tcp=%d, want 0 and 1", local.dials, tcp.dials)
	}
	if len(local.indices) != messageCount || len(tcp.indices) != messageCount {
		t.Fatalf("received local=%d tcp=%d, want %d each", len(local.indices), len(tcp.indices), messageCount)
	}
	for index := range messageCount {
		if local.indices[index] != index || tcp.indices[index] != index || local.byteCounts[index] != tcp.byteCounts[index] {
			t.Fatalf("message %d differs: local %d (%d bytes) tcp %d (%d bytes)", index, local.indices[index], local.byteCounts[index], tcp.indices[index], tcp.byteCounts[index])
		}
	}
	for boundary, count := range local.drops {
		if count != 0 || tcp.drops[boundary] != 0 {
			t.Errorf("drops at %s: local=%v tcp=%v, want none", boundary, count, tcp.drops[boundary])
		}
	}
	for _, path := range []struct {
		name   string
		result pathResult
	}{{name: "local", result: local}, {name: "tcp", result: tcp}} {
		t.Logf("%s: dials=%d data_frames_sent=%v received=%v bytes_sent=%v received=%v", path.name, path.result.dials, path.result.sent.frames, path.result.received.frames, path.result.sent.bytes, path.result.received.bytes)
		if path.result.sent.frames != messageCount || path.result.received.frames != messageCount ||
			path.result.sent.bytes != float64(wantBytes) || path.result.received.bytes != float64(wantBytes) {
			t.Errorf("%s accounting: sent %+v received %+v, want %d frames and %d bytes each way", path.name, path.result.sent, path.result.received, messageCount, wantBytes)
		}
	}
}

// A replacement between the generation check and the forward queue's
// publication: the link to the replaced resident detaches at once and takes
// nothing, the pending message included, the loop attaches to the new
// generation after its reconnect wait, and every message reaches the new
// resident in order. Catches a missing resident edge and a pending message
// taken by a link that had already ended.
func TestLocalForwardGenerationReplacementRace(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardComposedFixture(t, true, nil)
		defer f.close()
		old := f.recordResident
		replacement := f.newResident(old.clientId)
		replaced := false
		f.exchange.beforeLocalForwardAttachForTest = func(resident *Resident) {
			if resident == old && !replaced {
				replaced = true
				f.replaceResident(replacement)
			}
		}
		var gatheredFromOld atomic.Int64
		f.exchange.afterLocalTransportGatherForTest = func(link *localTransportLink, _ localTransportDirection, count int) {
			if link.resident == old {
				gatheredFromOld.Add(int64(count))
			}
		}
		residentDoneBefore := snapshotLocalForwardDetaches(localTransportDetachResidentDone)
		// the messages wait in the forward before the loop starts
		const messageCount = 100
		f.send(0, messageCount)
		f.run()
		waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
		defer waitCancel()
		requireLocalForwardIndices(t, f.receivedBy(replacement).wait(t, waitCtx, messageCount, "replacement resident"), 0, "replacement resident")
		if !replaced {
			t.Fatal("the attach did not reach the replacement barrier")
		}
		if count := gatheredFromOld.Load(); count != 0 {
			t.Fatalf("the link to the replaced resident took %d messages", count)
		}
		if indices, _ := f.receivedBy(old).snapshot(); len(indices) != 0 {
			t.Fatalf("the replaced resident received %d messages", len(indices))
		}
		if snapshotLocalForwardDetaches(localTransportDetachResidentDone)-residentDoneBefore != 1 {
			t.Error("the replaced resident's link did not detach with resident_done")
		}
		if f.exchange.localForward.linkCount() != 1 || f.dials.Load() != 0 {
			t.Errorf("links=%d dials=%d, want the new generation attached in process", f.exchange.localForward.linkCount(), f.dials.Load())
		}
	})
}

// A destination resident replaced while its link carries traffic: the link
// detaches with resident_done, the loop takes its reconnect wait as after a
// socket's end (the unchanged loop's jittered wait within
// ExchangeReconnectAfterErrorTimeout of the attempt's start), and attaches to
// the replacement, which receives the later messages in order.
func TestLocalForwardResidentReplacedMidFlight(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardComposedFixture(t, true, nil)
		defer f.close()
		var attachTimes []time.Time
		f.exchange.beforeLocalForwardAttachForTest = func(*Resident) {
			attachTimes = append(attachTimes, time.Now())
		}
		f.run()
		waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Hour)
		defer waitCancel()
		first := f.recordResident
		const phaseCount = 100
		f.send(0, phaseCount)
		requireLocalForwardIndices(t, f.receivedBy(first).wait(t, waitCtx, phaseCount, "the first resident"), 0, "the first resident")
		residentDoneBefore := snapshotLocalForwardDetaches(localTransportDetachResidentDone)
		second := f.newResident(first.clientId)
		f.replaceResident(second)
		synctest.Wait()
		if snapshotLocalForwardDetaches(localTransportDetachResidentDone)-residentDoneBefore != 1 {
			t.Fatal("the replaced resident's link did not detach with resident_done")
		}
		f.send(phaseCount, phaseCount)
		requireLocalForwardIndices(t, f.receivedBy(second).wait(t, waitCtx, phaseCount, "the second resident"), phaseCount, "the second resident")
		if len(attachTimes) != 2 {
			t.Fatalf("attaches = %d, want the first resident's and the replacement's", len(attachTimes))
		}
		if indices, _ := f.receivedBy(first).snapshot(); len(indices) != phaseCount {
			t.Fatalf("the first resident received %d messages, want only its %d", len(indices), phaseCount)
		}
		if f.exchange.localForward.linkCount() != 1 || f.dials.Load() != 0 {
			t.Fatalf("links=%d dials=%d, want the replacement attached in process", f.exchange.localForward.linkCount(), f.dials.Load())
		}
	})
}

// The switch turning off detaches the live link, and the forward's next
// message dials the same resident over the exchange at once: no reconnect
// wait. The forward's messages continue in order across the switch.
func TestLocalForwardKillSwitchRedialsTcp(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalForwardComposedFixture(t, true, func(settings *ExchangeSettings) {
			// any reconnect wait would be visible
			settings.ExchangeReconnectAfterErrorTimeout = time.Hour
		})
		defer f.close()
		f.run()
		const messageCount = 200
		f.send(0, messageCount/2)
		waitCtx, waitCancel := context.WithTimeout(f.ctx, 2*time.Hour)
		defer waitCancel()
		f.receivedBy(f.recordResident).wait(t, waitCtx, messageCount/2, "resident before the switch")

		disabledBefore := snapshotLocalForwardDetaches(localTransportDetachDisabled)
		f.control.setForward("off")
		f.exchange.localForward.refresh()
		synctest.Wait()
		if f.exchange.localForward.linkCount() != 0 || snapshotLocalForwardDetaches(localTransportDetachDisabled)-disabledBefore != 1 {
			t.Fatal("the switch did not detach the live link as disabled")
		}
		if f.dials.Load() != 0 {
			t.Fatal("the forward dialed with nothing to send")
		}
		sendTime := time.Now()
		f.send(messageCount/2, messageCount/2)
		synctest.Wait()
		if f.dials.Load() != 1 {
			t.Fatalf("dials after the switch = %d, want one", f.dials.Load())
		}
		if dialTime := <-f.dialTimes; !dialTime.Equal(sendTime) {
			t.Fatalf("the dial waited %s after the next message", dialTime.Sub(sendTime))
		}
		requireLocalForwardIndices(t, f.receivedBy(f.recordResident).wait(t, waitCtx, messageCount, "resident"), 0, "resident")
		if f.recordResident.IsDone() || len(f.residents) != 1 {
			t.Fatal("the switch replaced the resident")
		}
		if f.exchange.localForward.isPacing() {
			t.Error("the pacer outlived the last live link")
		}
	})
}

// Every destination that is not this process's current resident takes the
// exchange as before: a resident of another process, a record of another
// generation, a resident that is done, the switch off for this block, and the
// ceiling off. Each dials once with the forward header, and its messages
// reach the far side's reader in order; nothing attaches. Catches an attach
// that skips one of its checks.
func TestLocalForwardFallsBackToTcpWhenResidentNotLocal(t *testing.T) {
	warmLocalTransportPool()
	cases := []struct {
		name string
		arm  func(f *localForwardComposedFixture)
	}{
		{
			name: "a resident of another process",
			arm: func(f *localForwardComposedFixture) {
				func() {
					f.exchange.stateLock.Lock()
					defer f.exchange.stateLock.Unlock()
					delete(f.exchange.residents, f.recordResident.clientId)
				}()
			},
		},
		{
			name: "a record of another generation",
			arm: func(f *localForwardComposedFixture) {
				func() {
					f.stateLock.Lock()
					defer f.stateLock.Unlock()
					f.recordResident = &Resident{
						clientId:   f.recordResident.clientId,
						instanceId: f.recordResident.instanceId,
						residentId: server.NewId(),
					}
				}()
			},
		},
		{
			name: "a resident that is done",
			arm: func(f *localForwardComposedFixture) {
				f.recordResident.Cancel()
			},
		},
		{
			name: "the switch off for this block",
			arm: func(f *localForwardComposedFixture) {
				f.control.setForward("other-block")
				f.exchange.localForward.refresh()
			},
		},
		{
			name: "the ceiling off",
			arm: func(f *localForwardComposedFixture) {
				f.exchange.localForward = nil
			},
		},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			f := newLocalForwardComposedFixture(t, true, nil)
			defer f.close()
			// the far side of every dial: a resident's accept path elsewhere,
			// which echoes the header and reads the forward's frames
			farHeaders := make(chan ExchangeHeader, 4)
			var farLock sync.Mutex
			farIndices := []int{}
			f.settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
				f.dials.Add(1)
				local, remote := net.Pipe()
				f.socketWorkers.Add(1)
				go func() {
					defer f.socketWorkers.Done()
					defer remote.Close()
					buffer := NewDefaultExchangeBuffer(f.settings)
					header, err := buffer.ReadHeader(context.Background(), remote)
					if err != nil {
						return
					}
					farHeaders <- *header
					if err := buffer.WriteHeader(context.Background(), remote, header); err != nil {
						return
					}
					for {
						message, err := buffer.ReadMessage(remote)
						if err != nil {
							return
						}
						if 4 <= len(message) {
							farLock.Lock()
							farIndices = append(farIndices, localTransportTestMessageIndex(message))
							farLock.Unlock()
						}
						clientconnect.MessagePoolReturn(message)
					}
				}()
				return local, nil
			}
			var dialHeaders []ExchangeHeader
			f.exchange.beforeForwardDialForTest = func(header ExchangeHeader) {
				dialHeaders = append(dialHeaders, header)
			}
			attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward])
			c.arm(f)
			f.run()
			const messageCount = 20
			f.send(0, messageCount)
			synctest.Wait()
			if f.dials.Load() != 1 || len(dialHeaders) != 1 {
				t.Fatalf("%s: dials=%d observed=%d, want one", c.name, f.dials.Load(), len(dialHeaders))
			}
			if header := <-farHeaders; header.Op != ExchangeOpForward || header.ClientId != f.recordResident.clientId {
				t.Errorf("%s: the far side read header %+v", c.name, header)
			}
			farLock.Lock()
			got := append([]int(nil), farIndices...)
			farLock.Unlock()
			if len(got) != messageCount {
				t.Fatalf("%s: the far side read %d of %d messages", c.name, len(got), messageCount)
			}
			requireLocalForwardIndices(t, got, 0, c.name)
			if testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) != attachmentsBefore ||
				(f.exchange.localForward != nil && f.exchange.localForward.linkCount() != 0) {
				t.Errorf("%s: a forward attached in process", c.name)
			}
			for _, resident := range f.residents {
				if indices, _ := f.receivedBy(resident).snapshot(); len(indices) != 0 {
					t.Errorf("%s: a local resident received %d messages", c.name, len(indices))
				}
			}
		})
	}
}
