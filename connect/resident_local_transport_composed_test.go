// Composed local transport tests: the real ResidentTransport loop between a
// real device client on the transport's queues and a resident's real internal
// client, delivered in process or over the real exchange accept path. Only
// resident discovery is held: the loop's lookup returns the test's record, and
// the exchange has closed nominations, which then end before any model work.
package connect

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The messages one side of a composed fixture received, in order.
type localTransportReceived struct {
	stateLock sync.Mutex
	contents  []string
	changed   chan struct{}
}

func newLocalTransportReceived() *localTransportReceived {
	return &localTransportReceived{changed: make(chan struct{}, 1)}
}

// A receive callback that records simple messages from source.
func (self *localTransportReceived) callback(source clientconnect.Id) clientconnect.ReceiveFunction {
	return func(path clientconnect.TransferPath, frames []*protocol.Frame, _ clientconnect.Peer) {
		if path.SourceId != source {
			return
		}
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage {
				continue
			}
			message, err := clientconnect.FromFrame(frame)
			if err != nil {
				continue
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.contents = append(self.contents, message.(*protocol.SimpleMessage).Content)
			}()
			select {
			case self.changed <- struct{}{}:
			default:
			}
		}
	}
}

func (self *localTransportReceived) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.contents...)
}

// Waits until count messages arrived.
func (self *localTransportReceived) wait(t testing.TB, ctx context.Context, count int, description string) []string {
	t.Helper()
	for {
		contents := self.snapshot()
		if count <= len(contents) {
			return contents
		}
		select {
		case <-self.changed:
		case <-ctx.Done():
			t.Fatalf("%s received %d of %d", description, len(contents), count)
		}
	}
}

// One device client, one resident and one resident transport between them.
type localTransportComposedFixture struct {
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
	// what each resident received from the device
	residentReceived map[*Resident]*localTransportReceived

	transport *ResidentTransport
	device    *clientconnect.Client
	// what the device received from its resident
	deviceReceived *localTransportReceived

	dials         atomic.Int64
	dialTimes     chan time.Time
	socketWorkers sync.WaitGroup
	runDone       chan struct{}

	// messages and bytes that entered the transport's queues, counted at the
	// device's side of each
	queueMessages atomic.Int64
	queueBytes    atomic.Int64
	forwarders    sync.WaitGroup
	removeDevice  func()
}

// A fixture whose exchange delivers in process when local is true, and over
// the exchange accept path otherwise.
func newLocalTransportComposedFixture(t testing.TB, local bool, mutate func(*ExchangeSettings)) *localTransportComposedFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := localTransportTestSettings()
	// no exchange pings or resident registration polls in a test window
	settings.ExchangePingTimeout = time.Hour
	settings.ExchangeReadTimeout = 3 * time.Hour
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
		// Nominations end at admission, before any model work. Every loop
		// iteration after a link or connection ends reaches one.
		residentWorkersClosed: true,
	}
	f := &localTransportComposedFixture{
		t:                t,
		ctx:              ctx,
		cancel:           cancel,
		settings:         settings,
		control:          &localTransportTestControl{},
		exchange:         exchange,
		residentReceived: map[*Resident]*localTransportReceived{},
		deviceReceived:   newLocalTransportReceived(),
		dialTimes:        make(chan time.Time, 64),
		runDone:          make(chan struct{}),
	}
	if local {
		exchange.localTransport = newExchangeLocalTransportWithStore(ctx, settings, exchange.block, f.control.read)
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
	f.transport = NewResidentTransportWithEdge(
		ctx,
		exchange,
		resident.clientId,
		resident.instanceId,
		clientconnect.TransferCarrierProperties{},
		ResidentTransportAdmission{},
		ResidentTransportEdge{},
	)

	deviceSettings := clientconnect.DefaultClientSettingsWithBufferSize(settings.ExchangeBufferSize)
	deviceSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	deviceSettings.ControlPingTimeout = 0
	deviceSettings.Log = clientconnect.NewNoopLogger()
	f.device = clientconnect.NewClient(ctx, clientconnect.Id(resident.clientId), clientconnect.NewNoContractClientOob(), deviceSettings)
	f.device.ContractManager().AddNoContractPeer(clientconnect.ControlId)
	f.device.AddReceiveCallback(f.deviceReceived.callback(clientconnect.ControlId))
	// The device's routes count what enters and leaves the transport's queues
	// and otherwise hand messages straight through, as an edge carrier does.
	deviceSend := make(chan []byte, settings.ExchangeBufferSize)
	deviceReceive := make(chan []byte, settings.ExchangeBufferSize)
	f.forwarders.Add(2)
	go func() {
		defer f.forwarders.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-deviceSend:
				f.queueMessages.Add(1)
				f.queueBytes.Add(int64(len(message)))
				if sendPooledReceive(ctx.Done(), nil, f.transport.send, message, clientconnect.CarrierReliabilityReliable) != pooledMessageSendDelivered {
					return
				}
			}
		}
	}()
	go func() {
		defer f.forwarders.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-f.transport.receive:
				if !ok {
					return
				}
				f.queueMessages.Add(1)
				f.queueBytes.Add(int64(len(message)))
				if sendPooledReceive(ctx.Done(), nil, deviceReceive, message, clientconnect.CarrierReliabilityReliable) != pooledMessageSendDelivered {
					return
				}
			}
		}
	}()
	sendTransport := clientconnect.NewSendGatewayTransportWithType(clientconnect.TransportTypeH1)
	receiveTransport := clientconnect.NewReceiveGatewayTransport()
	f.device.RouteManager().UpdateTransport(sendTransport, []clientconnect.Route{deviceSend})
	f.device.RouteManager().UpdateTransportWithProperties(receiveTransport, []clientconnect.Route{deviceReceive}, clientconnect.TransferCarrierProperties{
		ReceiveReliability: clientconnect.CarrierReliabilityReliable,
	})
	f.removeDevice = func() {
		f.device.RouteManager().RemoveTransport(sendTransport)
		f.device.RouteManager().RemoveTransport(receiveTransport)
		returnReadyPooledMessages(deviceSend)
		returnReadyPooledMessages(deviceReceive)
	}
	return f
}

// A resident of the fixture's client with a real internal client that records
// what it receives from the device.
func (self *localTransportComposedFixture) newResident(clientId server.Id) *Resident {
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
	resident.client.ContractManager().AddNoContractPeer(clientconnect.Id(resident.clientId))
	received := newLocalTransportReceived()
	resident.client.AddReceiveCallback(received.callback(clientconnect.Id(resident.clientId)))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.residents = append(self.residents, resident)
	self.residentReceived[resident] = received
	return resident
}

// The record the loop resolves: the resident the test last named.
func (self *localTransportComposedFixture) lookup(
	context.Context,
	server.Id,
	server.Id,
	time.Duration,
) *model.NetworkClientResident {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &model.NetworkClientResident{
		ClientId:              self.recordResident.clientId,
		InstanceId:            self.recordResident.instanceId,
		ResidentId:            self.recordResident.residentId,
		ResidentHost:          "local-transport.invalid",
		ResidentInternalPorts: []int{1},
	}
}

// Replaces the record and the exchange's resident, as a nomination does.
func (self *localTransportComposedFixture) replaceResident(replacement *Resident) {
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

func (self *localTransportComposedFixture) received(resident *Resident) *localTransportReceived {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.residentReceived[resident]
}

func (self *localTransportComposedFixture) run() {
	go func() {
		defer close(self.runDone)
		self.transport.runWithResidentLookup(self.lookup)
	}()
}

// Ends the device, then the transport, then the residents, and joins them.
func (self *localTransportComposedFixture) close() {
	self.removeDevice()
	if err := self.device.CloseAndWait(context.Background()); err != nil {
		self.t.Error(err)
	}
	self.transport.Close()
	<-self.runDone
	self.cancel()
	self.forwarders.Wait()
	self.stateLock.Lock()
	residents := append([]*Resident(nil), self.residents...)
	self.stateLock.Unlock()
	for _, resident := range residents {
		if err := resident.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	self.socketWorkers.Wait()
}

// The device sends count messages to its resident, and the resident sends
// count to the device, of sizes that vary across batch and pool boundaries.
func (self *localTransportComposedFixture) exchangeMessages(t testing.TB, firstIndex int, count int, resident *Resident) []string {
	t.Helper()
	script := make([]string, 0, count)
	for index := firstIndex; index < firstIndex+count; index++ {
		content := fmt.Sprintf("%06d:%s", index, strings.Repeat("x", (index*37)%1400))
		script = append(script, content)
		send := func(client *clientconnect.Client, destination clientconnect.Id) {
			frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
			if !client.SendWithTimeout(frame, destination, func(error) {}, time.Minute) {
				clientconnect.MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("message %d refused", index)
			}
		}
		send(self.device, clientconnect.ControlId)
		send(resident.client, clientconnect.Id(resident.clientId))
	}
	return script
}

func localTransportDropSnapshot() map[string]float64 {
	drops := map[string]float64{}
	for _, boundary := range []string{
		receiveQueueBoundaryExchangeAcceptTransport,
		receiveQueueBoundaryExchangeOutboundSocket,
		receiveQueueBoundaryExchangeToResident,
		receiveQueueBoundaryExchangeTransportSend,
		receiveQueueBoundaryLocalTransportSend,
		receiveQueueBoundaryLocalTransportReceive,
	} {
		drops[boundary] = testutil.ToFloat64(receiveQueueDroppedMessagesCounter.WithLabelValues(boundary))
	}
	return drops
}

// A transport whose resident is in this process opens no exchange socket, and
// messages flow both ways in order through the link. Catches removing the
// branch.
func TestLocalTransportDialsNoExchangeSocket(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportComposedFixture(t, true, nil)
		defer f.close()
		attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachmentsGauge)
		f.run()
		synctest.Wait()
		if f.exchange.localTransport.linkCount() != 1 || testutil.ToFloat64(exchangeLocalAttachmentsGauge) != attachmentsBefore+1 {
			t.Fatal("the transport did not attach in process")
		}
		const messageCount = 300
		script := f.exchangeMessages(t, 0, messageCount, f.recordResident)
		waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
		defer waitCancel()
		deviceReceived := f.deviceReceived.wait(t, waitCtx, messageCount, "device")
		residentReceived := f.received(f.recordResident).wait(t, waitCtx, messageCount, "resident")
		for index := range messageCount {
			if deviceReceived[index] != script[index] || residentReceived[index] != script[index] {
				t.Fatalf("message %d out of order", index)
			}
		}
		if dials := f.dials.Load(); dials != 0 {
			t.Fatalf("a local transport dialed %d exchange sockets", dials)
		}
	})
}

// One script through the in-process link and through the exchange socket
// gives the device and the resident the same messages in the same order, the
// same drop counters (none), and the same exchange accounting: each message
// that entered the transport's queues is one data frame sent and one received,
// with its 4-byte header, in exchange_local on one path and exchange_io
// (op transport) on the other. Catches any semantic drift between the paths.
func TestLocalAndTcpPathsDeliverIdenticalSequences(t *testing.T) {
	warmLocalTransportPool()
	type pathResult struct {
		deviceReceived   []string
		residentReceived []string
		drops            map[string]float64
		dials            int64
		frames           localTransportIOSnapshot
		received         localTransportIOSnapshot
		queueMessages    int64
		queueBytes       int64
	}
	run := func(local bool) pathResult {
		var result pathResult
		synctest.Test(t, func(t *testing.T) {
			f := newLocalTransportComposedFixture(t, local, nil)
			defer f.close()
			dropsBefore := localTransportDropSnapshot()
			snapshot := func(direction string) localTransportIOSnapshot {
				if local {
					return snapshotLocalTransportIO(direction, "data")
				}
				ioSnapshot := snapshotExchangeIOOp("transport", direction, "data")
				return localTransportIOSnapshot{frames: ioSnapshot.frames, bytes: ioSnapshot.bytes}
			}
			sentBefore := snapshot("sent")
			receivedBefore := snapshot("received")
			unknownBefore := snapshotExchangeIOOp("unknown", "sent", "data")
			f.run()
			const messageCount = 400
			script := f.exchangeMessages(t, 0, messageCount, f.recordResident)
			waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
			defer waitCancel()
			result.deviceReceived = f.deviceReceived.wait(t, waitCtx, messageCount, "device")
			result.residentReceived = f.received(f.recordResident).wait(t, waitCtx, messageCount, "resident")
			for index := range messageCount {
				if result.deviceReceived[index] != script[index] || result.residentReceived[index] != script[index] {
					t.Fatalf("message %d differs from the script", index)
				}
			}
			// every acknowledgement has crossed once the bubble is idle
			synctest.Wait()
			sent := snapshot("sent")
			received := snapshot("received")
			result.frames = localTransportIOSnapshot{frames: sent.frames - sentBefore.frames, bytes: sent.bytes - sentBefore.bytes}
			result.received = localTransportIOSnapshot{frames: received.frames - receivedBefore.frames, bytes: received.bytes - receivedBefore.bytes}
			result.queueMessages = f.queueMessages.Load()
			result.queueBytes = f.queueBytes.Load()
			result.dials = f.dials.Load()
			result.drops = map[string]float64{}
			for boundary, count := range localTransportDropSnapshot() {
				result.drops[boundary] = count - dropsBefore[boundary]
			}
			if snapshotExchangeIOOp("unknown", "sent", "data").frames != unknownBefore.frames {
				t.Error("exchange frames were labeled with an unknown operation")
			}
		})
		return result
	}
	local := run(true)
	tcp := run(false)

	if local.dials != 0 || tcp.dials != 1 {
		t.Errorf("dials local=%d tcp=%d, want 0 and 1", local.dials, tcp.dials)
	}
	if strings.Join(local.deviceReceived, "\n") != strings.Join(tcp.deviceReceived, "\n") ||
		strings.Join(local.residentReceived, "\n") != strings.Join(tcp.residentReceived, "\n") {
		t.Error("the paths delivered different sequences")
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
		wantFrames := float64(path.result.queueMessages)
		wantBytes := float64(path.result.queueBytes + int64(exchangeIOFrameHeaderByteCount)*path.result.queueMessages)
		t.Logf("%s: dials=%d queue_messages=%d queue_bytes=%d data_frames_sent=%v received=%v bytes_sent=%v received=%v", path.name, path.result.dials, path.result.queueMessages, path.result.queueBytes, path.result.frames.frames, path.result.received.frames, path.result.frames.bytes, path.result.received.bytes)
		if path.result.frames.frames != wantFrames || path.result.received.frames != wantFrames ||
			path.result.frames.bytes != wantBytes || path.result.received.bytes != wantBytes {
			t.Errorf("%s accounting: sent %+v received %+v, want %v frames and %v bytes each way", path.name, path.result.frames, path.result.received, wantFrames, wantBytes)
		}
	}
}

// A replacement between the generation check and the route publication: the
// link to the replaced resident detaches at once and takes nothing from the
// transport, the loop attaches to the new generation, and every message
// reaches the new resident in order. Catches a missing resident edge.
func TestLocalTransportGenerationReplacementRace(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportComposedFixture(t, true, nil)
		defer f.close()
		old := f.recordResident
		replacement := f.newResident(old.clientId)
		replaced := false
		f.exchange.beforeLocalTransportAttachForTest = func(resident *Resident) {
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
		residentDoneBefore := snapshotLocalTransportDetaches(localTransportDetachResidentDone)
		// the device's messages wait in the transport before the loop starts
		const messageCount = 100
		script := make([]string, 0, messageCount)
		for index := range messageCount {
			content := fmt.Sprintf("%06d", index)
			script = append(script, content)
			frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
			if !f.device.SendWithTimeout(frame, clientconnect.ControlId, func(error) {}, time.Minute) {
				clientconnect.MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("message %d refused", index)
			}
		}
		synctest.Wait()
		f.run()
		waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
		defer waitCancel()
		residentReceived := f.received(replacement).wait(t, waitCtx, messageCount, "replacement resident")
		for index := range messageCount {
			if residentReceived[index] != script[index] {
				t.Fatalf("message %d out of order at the replacement", index)
			}
		}
		if !replaced {
			t.Fatal("the attach did not reach the replacement barrier")
		}
		if count := gatheredFromOld.Load(); count != 0 {
			t.Fatalf("the link to the replaced resident took %d messages", count)
		}
		if received := f.received(old).snapshot(); len(received) != 0 {
			t.Fatalf("the replaced resident received %d messages", len(received))
		}
		if snapshotLocalTransportDetaches(localTransportDetachResidentDone)-residentDoneBefore != 1 {
			t.Error("the replaced resident's link did not detach with resident_done")
		}
		if f.exchange.localTransport.linkCount() != 1 || f.dials.Load() != 0 {
			t.Errorf("links=%d dials=%d, want the new generation attached in process", f.exchange.localTransport.linkCount(), f.dials.Load())
		}
	})
}

// The switch turning off detaches the live link, and the loop redials the
// same resident over the exchange at once: no reconnect wait and no
// replacement nomination. The device's messages continue in order across the
// switch.
func TestLocalTransportKillSwitchRedialsTcp(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportComposedFixture(t, true, func(settings *ExchangeSettings) {
			// any reconnect wait would be visible
			settings.ExchangeReconnectAfterErrorTimeout = time.Hour
		})
		defer f.close()
		pacerDone := make(chan struct{})
		go func() {
			defer close(pacerDone)
			f.exchange.localTransport.runDisablePacer()
		}()
		f.run()
		synctest.Wait()
		const messageCount = 200
		script := f.exchangeMessages(t, 0, messageCount/2, f.recordResident)
		waitCtx, waitCancel := context.WithTimeout(f.ctx, 2*time.Hour)
		defer waitCancel()
		f.received(f.recordResident).wait(t, waitCtx, messageCount/2, "resident before the switch")

		disabledBefore := snapshotLocalTransportDetaches(localTransportDetachDisabled)
		switchTime := time.Now()
		f.control.set("off")
		f.exchange.localTransport.refresh()
		synctest.Wait()
		if f.dials.Load() != 1 {
			t.Fatalf("dials after the switch = %d, want one redial", f.dials.Load())
		}
		if dialTime := <-f.dialTimes; !dialTime.Equal(switchTime) {
			t.Fatalf("the redial waited %s after the detach", dialTime.Sub(switchTime))
		}
		if snapshotLocalTransportDetaches(localTransportDetachDisabled)-disabledBefore != 1 {
			t.Error("the detach was not counted as disabled")
		}
		script = append(script, f.exchangeMessages(t, messageCount/2, messageCount/2, f.recordResident)...)
		residentReceived := f.received(f.recordResident).wait(t, waitCtx, messageCount, "resident")
		deviceReceived := f.deviceReceived.wait(t, waitCtx, messageCount, "device")
		for index := range messageCount {
			if residentReceived[index] != script[index] || deviceReceived[index] != script[index] {
				t.Fatalf("message %d out of order across the switch", index)
			}
		}
		if f.recordResident.IsDone() || len(f.residents) != 1 {
			t.Fatal("the switch replaced the resident")
		}
		f.exchange.cancel()
		<-pacerDone
	})
}

// A resident replaced while its link carries traffic: the link detaches with
// resident_done, its pump takes nothing more, the loop attaches to the
// replacement, and the device's later messages reach the replacement in order.
// Replaced again with messages in flight, a batch can still cross in the
// instant before the resident's done edge ends the link, as on a socket; the
// device's transfer layer resends what the replaced resident had not
// acknowledged, and the two residents together receive every message, each in
// order.
func TestLocalTransportResidentReplacedMidFlight(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		f := newLocalTransportComposedFixture(t, true, nil)
		defer f.close()
		var stateLock sync.Mutex
		replaced := map[*Resident]bool{}
		gatheredAfterReplacement := 0
		f.exchange.afterLocalTransportGatherForTest = func(link *localTransportLink, direction localTransportDirection, count int) {
			stateLock.Lock()
			defer stateLock.Unlock()
			if direction == localTransportDirectionClient && replaced[link.resident] {
				gatheredAfterReplacement += count
			}
		}
		replace := func() *Resident {
			replacement := f.newResident(f.recordResident.clientId)
			f.replaceResident(replacement)
			return replacement
		}
		f.run()
		synctest.Wait()
		waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Hour)
		defer waitCancel()

		acked := make(chan struct{}, 1024)
		var ackCount atomic.Int64
		send := func(firstIndex int, count int) {
			for index := firstIndex; index < firstIndex+count; index++ {
				frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: fmt.Sprintf("%06d", index)})
				if !f.device.SendWithTimeout(frame, clientconnect.ControlId, func(err error) {
					if err == nil {
						ackCount.Add(1)
						acked <- struct{}{}
					}
				}, time.Hour) {
					clientconnect.MessagePoolReturn(frame.MessageBytes)
					t.Fatalf("message %d refused", index)
				}
			}
		}
		waitAcked := func(count int64) {
			for ackCount.Load() < count {
				select {
				case <-acked:
				case <-waitCtx.Done():
					t.Fatalf("acknowledged %d of %d", ackCount.Load(), count)
				}
			}
		}
		requireExactly := func(received []string, firstIndex int, description string) {
			for offset, content := range received {
				if want := fmt.Sprintf("%06d", firstIndex+offset); content != want {
					t.Fatalf("%s received %s at %d, want %s", description, content, offset, want)
				}
			}
		}
		const phaseCount = 100

		// replaced with every message acknowledged
		first := f.recordResident
		send(0, phaseCount)
		requireExactly(f.received(first).wait(t, waitCtx, phaseCount, "the first resident"), 0, "the first resident")
		waitAcked(phaseCount)
		residentDoneBefore := snapshotLocalTransportDetaches(localTransportDetachResidentDone)
		second := replace()
		synctest.Wait()
		if snapshotLocalTransportDetaches(localTransportDetachResidentDone)-residentDoneBefore != 1 {
			t.Fatal("the replaced resident's link did not detach with resident_done")
		}
		// nothing was in flight, so the first link's pump must take nothing
		// from here on
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			replaced[first] = true
		}()
		send(phaseCount, phaseCount)
		requireExactly(f.received(second).wait(t, waitCtx, phaseCount, "the second resident"), phaseCount, "the second resident")
		waitAcked(2 * phaseCount)
		if received := f.received(first).snapshot(); len(received) != phaseCount {
			t.Fatalf("the first resident received %d messages, want only its %d", len(received), phaseCount)
		}

		// replaced with messages in flight
		send(2*phaseCount, phaseCount)
		third := replace()
		// every message is acknowledged by whichever resident received it
		waitAcked(3 * phaseCount)
		synctest.Wait()
		lastIndex := 3*phaseCount - 1
		covered := map[int]bool{}
		for _, part := range []struct {
			name     string
			received []string
		}{{name: "the second resident", received: f.received(second).snapshot()[phaseCount:]}, {name: "the third resident", received: f.received(third).snapshot()}} {
			previous := -1
			for _, content := range part.received {
				var index int
				if _, err := fmt.Sscanf(content, "%06d", &index); err != nil {
					t.Fatalf("%s received %q", part.name, content)
				}
				if index <= previous {
					t.Fatalf("%s received %d after %d", part.name, index, previous)
				}
				previous = index
				covered[index] = true
			}
		}
		for index := 2 * phaseCount; index <= lastIndex; index++ {
			if !covered[index] {
				t.Fatalf("message %d reached neither resident", index)
			}
		}

		stateLock.Lock()
		gathered := gatheredAfterReplacement
		stateLock.Unlock()
		if gathered != 0 {
			t.Fatalf("the first resident's link took %d messages after it detached", gathered)
		}
		// the loop's reconnect wait after a resident ends, as after a socket
		time.Sleep(f.settings.ExchangeReconnectAfterErrorTimeout)
		synctest.Wait()
		if f.exchange.localTransport.linkCount() != 1 || f.dials.Load() != 0 {
			t.Fatalf("links=%d dials=%d, want the last replacement attached in process", f.exchange.localTransport.linkCount(), f.dials.Load())
		}
	})
}
