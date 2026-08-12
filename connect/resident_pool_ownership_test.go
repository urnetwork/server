// Resident exchange ownership tests isolate cancellation, timeout, delivery,
// and queued teardown without database-backed resident discovery.
package connect

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Retains one test-owned reference that becomes the final owner only after the
// production owner has returned its exact pooled message.
func retainResidentPoolWitness(message []byte) []byte {
	return clientconnect.MessagePoolShareReadOnly(message)
}

// Verifies that joined production teardown released the other exact reference.
func requireResidentPoolOwnerReturned(t *testing.T, witness []byte, description string) {
	t.Helper()
	if !clientconnect.MessagePoolReturn(witness) {
		t.Fatalf("%s did not leave the retained witness as final pool owner", description)
	}
}

// Verifies a collection of independently retained exact-message references.
func requireResidentPoolOwnersReturned(t *testing.T, witnesses [][]byte, description string) {
	t.Helper()
	for witnessIndex, witness := range witnesses {
		requireResidentPoolOwnerReturned(
			t,
			witness,
			fmt.Sprintf("%s message %d", description, witnessIndex),
		)
	}
}

// newTestingExchangeConnection creates one socket-backed connection without
// database-backed resident discovery.
func newTestingExchangeConnection(
	t *testing.T,
	op ExchangeOp,
) (*ExchangeConnection, net.Conn) {
	localConn, peerConn := net.Pipe()
	settings := DefaultExchangeSettings()
	ctx, cancel := context.WithCancel(context.Background())
	connection := &ExchangeConnection{
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		conn:          localConn,
		sendBuffer:    NewDefaultExchangeBuffer(settings),
		receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
		send:          make(chan []byte, 8),
		receive:       make(chan []byte, 8),
		settings:      settings,
		header:        ExchangeHeader{Op: op},
	}
	t.Cleanup(func() {
		connection.Close()
		peerConn.Close()
	})
	return connection, peerConn
}

// finishPausedAcceptSideWrite joins a failed test setup before returning the
// message from either its caller or the accept-side queue.
func finishPausedAcceptSideWrite(
	t *testing.T,
	resumeWriter func(),
	writeDone <-chan writerResult,
	message []byte,
	send <-chan []byte,
) {
	t.Helper()
	resumeWriter()
	select {
	case result := <-writeDone:
		if result.err != nil || !result.success {
			clientconnect.MessagePoolReturn(message)
			return
		}
		returnReadyPooledMessages(send)
	case <-time.After(time.Second):
		t.Error("accept-side writer did not stop during failure cleanup")
	}
}

// An already-canceled lifecycle owns priority over a writable destination, so
// the message is returned instead of being stranded in a dead peer queue.
func TestSendPooledMessageReturnsOnCanceledLifecycle(t *testing.T) {
	for caseIndex := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		peerCtx, peerCancel := context.WithCancel(context.Background())
		if caseIndex == 0 {
			cancel()
		} else {
			peerCancel()
		}
		destination := make(chan []byte, 1)
		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			<-timer.C
		}
		message := clientconnect.MessagePoolGet(128)
		witness := retainResidentPoolWitness(message)
		result := sendPooledMessage(
			ctx.Done(),
			peerCtx.Done(),
			destination,
			message,
			timer,
			time.Second,
		)
		timer.Stop()
		cancel()
		peerCancel()
		if result != pooledMessageSendDone {
			t.Fatalf("case %d canceled send result=%d", caseIndex, result)
		}
		if len(destination) != 0 {
			t.Fatalf("case %d canceled send transferred a message", caseIndex)
		}
		requireResidentPoolOwnerReturned(
			t,
			witness,
			fmt.Sprintf("case %d canceled send", caseIndex),
		)
	}
}

// A backpressure timeout drops and returns the held message, while a
// successful offer transfers exactly one reference to the destination.
func TestSendPooledMessageTimeoutAndDeliveryOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peerCtx, peerCancel := context.WithCancel(context.Background())
	defer peerCancel()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	blockedDestination := make(chan []byte)
	timedOutMessage := clientconnect.MessagePoolGet(128)
	timedOutWitness := retainResidentPoolWitness(timedOutMessage)
	result := sendPooledMessage(
		ctx.Done(),
		peerCtx.Done(),
		blockedDestination,
		timedOutMessage,
		timer,
		time.Millisecond,
	)
	if result != pooledMessageSendDropped {
		t.Fatalf("timeout send result=%d", result)
	}
	requireResidentPoolOwnerReturned(t, timedOutWitness, "timed-out send")

	destination := make(chan []byte, 1)
	deliveredMessage := clientconnect.MessagePoolGet(128)
	deliveredWitness := retainResidentPoolWitness(deliveredMessage)
	result = sendPooledMessage(
		ctx.Done(),
		peerCtx.Done(),
		destination,
		deliveredMessage,
		timer,
		time.Second,
	)
	if result != pooledMessageSendDelivered {
		t.Fatalf("delivery send result=%d", result)
	}
	if clientconnect.MessagePoolReturn(<-destination) {
		t.Fatal("destination return bypassed the retained delivery witness")
	}
	requireResidentPoolOwnerReturned(t, deliveredWitness, "delivered send")
}

// Teardown returns every queued message for both an open channel and a closed
// channel, matching cancellation-driven and explicit-close paths.
func TestReturnReadyPooledMessagesDrainsQueuedOwnership(t *testing.T) {
	openMessages := make(chan []byte, 4)
	closedMessages := make(chan []byte, 4)
	openWitnesses := make([][]byte, 0, 3)
	closedWitnesses := make([][]byte, 0, 3)
	for range 3 {
		openMessage := clientconnect.MessagePoolGet(128)
		openWitnesses = append(openWitnesses, retainResidentPoolWitness(openMessage))
		openMessages <- openMessage
		closedMessage := clientconnect.MessagePoolGet(128)
		closedWitnesses = append(closedWitnesses, retainResidentPoolWitness(closedMessage))
		closedMessages <- closedMessage
	}
	close(closedMessages)
	returnReadyPooledMessages(openMessages)
	returnReadyPooledMessages(closedMessages)
	if len(openMessages) != 0 || len(closedMessages) != 0 {
		t.Fatalf("queued messages remain open=%d closed=%d", len(openMessages), len(closedMessages))
	}
	requireResidentPoolOwnersReturned(t, openWitnesses, "open queue drain")
	requireResidentPoolOwnersReturned(t, closedWitnesses, "closed queue drain")
}

// Explicit resident close drains messages accepted immediately before
// cancellation, without requiring the exchange run loop to receive them.
func TestResidentTransportCloseDrainsQueuedSendOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &ResidentTransport{
		ctx:     ctx,
		cancel:  cancel,
		send:    make(chan []byte, 4),
		receive: make(chan []byte, 4),
	}
	witnesses := make([][]byte, 0, 3)
	for range 3 {
		message := clientconnect.MessagePoolGet(128)
		witnesses = append(witnesses, retainResidentPoolWitness(message))
		transport.send <- message
	}
	transport.Close()
	requireResidentPoolOwnersReturned(t, witnesses, "resident transport close")
}

// Connection close joins its framed reader before draining the receive queue.
// This reproduces the exchange-to-resident teardown path that previously left
// each unread Framer.Read buffer checked out.
func TestExchangeConnectionCloseDrainsQueuedReceiveOwnership(t *testing.T) {
	connection, peerConn := newTestingExchangeConnection(t, ExchangeOpTransport)
	receiveWitnesses := make(chan []byte, 3)
	connection.afterReceiveEnqueueForTest = func(message []byte) {
		receiveWitnesses <- retainResidentPoolWitness(message)
	}
	go connection.Run()
	writeDone := make(chan error, 1)
	go func() {
		framer := clientconnect.NewFramer(connection.settings.FramerSettings)
		for range 3 {
			if err := framer.Write(peerConn, make([]byte, 128)); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()
	witnesses := make([][]byte, 0, 3)
	for messageIndex := range 3 {
		select {
		case witness := <-receiveWitnesses:
			witnesses = append(witnesses, witness)
		case <-time.After(time.Second):
			t.Fatalf("exchange receive worker transferred %d messages, want 3", messageIndex)
		}
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if len(connection.receive) != 0 {
		t.Fatalf("exchange close left %d receive messages", len(connection.receive))
	}
	requireResidentPoolOwnersReturned(t, witnesses, "exchange receive close")
}

// Connection close also joins a writer blocked in the socket and returns both
// its in-flight batch and every message still queued behind that batch.
func TestExchangeConnectionCloseDrainsQueuedSendOwnership(t *testing.T) {
	connection, _ := newTestingExchangeConnection(t, ExchangeOpTransport)
	sendDequeued := make(chan struct{}, 1)
	connection.afterSendDequeueForTest = func() {
		select {
		case sendDequeued <- struct{}{}:
		default:
		}
	}
	witnesses := make([][]byte, 0, 3)
	for range 3 {
		message := clientconnect.MessagePoolGet(128)
		witnesses = append(witnesses, retainResidentPoolWitness(message))
		connection.send <- message
	}
	go connection.Run()
	select {
	case <-sendDequeued:
	case <-time.After(time.Second):
		t.Fatal("exchange writer did not take its queued batch")
	}
	connection.Close()
	if len(connection.send) != 0 {
		t.Fatalf("exchange close left %d send messages", len(connection.send))
	}
	requireResidentPoolOwnersReturned(t, witnesses, "exchange send close")
}

// A producer admitted before connection cancellation may resume after Close
// starts. Close waits for that producer and drains its handoff without closing
// the channel underneath it.
func TestExchangeConnectionCloseJoinsDelayedProducer(t *testing.T) {
	connection, _ := newTestingExchangeConnection(t, ExchangeOpTransport)
	go connection.Run()
	if !connection.sendAdmission.start() {
		t.Fatal("exchange producer was not admitted")
	}
	message := clientconnect.MessagePoolGet(128)
	witness := retainResidentPoolWitness(message)
	closeDone := make(chan struct{})
	go func() {
		connection.Close()
		close(closeDone)
	}()
	select {
	case <-connection.Done():
	case <-time.After(time.Second):
		t.Fatal("exchange close did not cancel the connection")
	}
	connection.send <- message
	connection.sendAdmission.done()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("exchange close did not join the delayed producer")
	}
	if len(connection.send) != 0 {
		t.Fatalf("exchange close left %d delayed messages", len(connection.send))
	}
	requireResidentPoolOwnerReturned(t, witness, "exchange delayed producer")
}

// Resident transport and forward close use the same admission barrier. An
// already-admitted producer can finish after cancellation, and close returns
// only after its queued reference has been reclaimed.
func TestResidentCloseJoinsDelayedProducer(t *testing.T) {
	for caseIndex := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		send := make(chan []byte, 1)
		var admission *pooledMessageSendAdmission
		var closeOwner func()
		if caseIndex == 0 {
			transport := &ResidentTransport{
				ctx:     ctx,
				cancel:  cancel,
				send:    send,
				receive: make(chan []byte, 1),
			}
			admission = &transport.sendAdmission
			closeOwner = transport.Close
		} else {
			forward := &ResidentForward{
				ctx:    ctx,
				cancel: cancel,
				send:   send,
			}
			admission = &forward.sendAdmission
			closeOwner = forward.Close
		}

		if !admission.start() {
			t.Fatalf("case %d producer was not admitted", caseIndex)
		}
		message := clientconnect.MessagePoolGet(128)
		witness := retainResidentPoolWitness(message)
		closeDone := make(chan struct{})
		go func() {
			closeOwner()
			close(closeDone)
		}()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatalf("case %d close did not cancel the resident owner", caseIndex)
		}
		send <- message
		admission.done()
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatalf("case %d close did not join the delayed producer", caseIndex)
		}
		if len(send) != 0 {
			t.Fatalf("case %d close left %d delayed messages", caseIndex, len(send))
		}
		requireResidentPoolOwnerReturned(
			t,
			witness,
			fmt.Sprintf("case %d delayed resident producer", caseIndex),
		)
	}
}

// The accept-side forward queue can hold a framed message while its resident
// consumer is still processing an earlier message. Close waits for that
// consumer, then returns both the in-flight rejection and the queued message.
func TestResidentAddForwardCloseJoinsConsumerAndDrainsQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ExchangeBufferSize = 2
	resident := &Resident{
		ctx:      ctx,
		exchange: &Exchange{settings: settings},
	}
	closeJoinEntered := make(chan struct{})
	resident.beforeForwardCloseJoinForTest = func() {
		close(closeJoinEntered)
	}

	consumerEntered := make(chan struct{})
	releaseConsumer := make(chan struct{})
	var enterOnce sync.Once
	forward, closeForward, err := resident.addForwardWithReceive(func([]byte) bool {
		enterOnce.Do(func() {
			close(consumerEntered)
		})
		<-releaseConsumer
		return false
	})
	if err != nil {
		t.Fatal(err)
	}

	firstMessage := clientconnect.MessagePoolGet(128)
	firstWitness := retainResidentPoolWitness(firstMessage)
	forward <- firstMessage
	select {
	case <-consumerEntered:
	case <-time.After(time.Second):
		t.Fatal("forward consumer did not receive the first message")
	}
	secondMessage := clientconnect.MessagePoolGet(128)
	secondWitness := retainResidentPoolWitness(secondMessage)
	forward <- secondMessage

	closeDone := make(chan struct{})
	go func() {
		closeForward()
		close(closeDone)
	}()
	select {
	case <-closeJoinEntered:
	case <-time.After(time.Second):
		t.Fatal("forward close did not enter its consumer join")
	}
	select {
	case <-closeDone:
		t.Fatal("forward close returned from its join before the consumer")
	default:
	}
	close(releaseConsumer)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("forward close did not join its consumer")
	}

	if len(forward) != 0 {
		t.Fatalf("forward close left %d queued messages", len(forward))
	}
	requireResidentPoolOwnerReturned(t, firstWitness, "in-flight forward close")
	requireResidentPoolOwnerReturned(t, secondWitness, "queued forward close")
}

// writerResult captures one paused WriteDetailed outcome.
type writerResult struct {
	success bool
	err     error
}

// Accept-side transport teardown must not drain before RouteManager has joined
// a real WriteDetailed call admitted to the old route snapshot. The paused
// writer resumes after route withdrawal and performs its final old-route send;
// cleanup must reclaim that handoff before reporting completion.
func TestCloseExchangeTransportQueuesDrainsJoinedOldSnapshotWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeManager := clientconnect.NewRouteManager(ctx, "accept-side-test")
	writer := routeManager.OpenMultiRouteWriter(clientconnect.DestinationId(clientconnect.NewId()))
	defer routeManager.CloseMultiRouteWriter(writer)
	send := make(chan []byte, 1)
	receive := make(chan []byte, 1)
	transport := clientconnect.NewSendGatewayTransport()
	routeManager.UpdateTransport(transport, []clientconnect.Route{send})
	defer routeManager.RemoveTransport(transport)

	message := clientconnect.MessagePoolGet(128)
	witness := retainResidentPoolWitness(message)
	snapshotAcquired, removalWaiting, resumeWriter := clientconnect.TestingPauseMultiRouteWriterSnapshot(writer)
	defer resumeWriter()
	writeDone := make(chan writerResult, 1)
	go func() {
		success, err := writer.WriteDetailed(ctx, message, time.Second)
		writeDone <- writerResult{success: success, err: err}
	}()
	select {
	case <-snapshotAcquired:
	case <-time.After(time.Second):
		finishPausedAcceptSideWrite(t, resumeWriter, writeDone, message, send)
		t.Fatal("writer did not acquire the accept-side route snapshot")
	}

	workersStopped := make(chan struct{})
	var workers sync.WaitGroup
	cleanupDone := make(chan struct{})
	go func() {
		closeExchangeTransportQueues(
			func() {
				routeManager.RemoveTransport(transport)
			},
			func() {
				close(workersStopped)
			},
			&workers,
			send,
			receive,
		)
		close(cleanupDone)
	}()
	select {
	case <-removalWaiting:
	case <-time.After(time.Second):
		finishPausedAcceptSideWrite(t, resumeWriter, writeDone, message, send)
		t.Fatal("accept-side cleanup did not reach the old-writer join")
	}
	select {
	case <-workersStopped:
		finishPausedAcceptSideWrite(t, resumeWriter, writeDone, message, send)
		t.Fatal("socket workers stopped before old route writers were joined")
	case <-cleanupDone:
		finishPausedAcceptSideWrite(t, resumeWriter, writeDone, message, send)
		t.Fatal("accept-side cleanup returned before old route writers were joined")
	default:
	}
	resumeWriter()
	select {
	case result := <-writeDone:
		if result.err != nil || !result.success {
			clientconnect.MessagePoolReturn(message)
			t.Fatalf("paused accept-side write success=%t err=%v", result.success, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("accept-side old-snapshot writer did not resume")
	}
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("accept-side cleanup did not finish after old writer release")
	}
	if len(send) != 0 {
		t.Fatalf("accept-side cleanup left %d old-snapshot messages", len(send))
	}
	requireResidentPoolOwnerReturned(t, witness, "accept-side old-snapshot cleanup")
}

// The real exchange accept loop admits each socket into WaitForIdle before its
// handler can retain a pooled owner.
func TestExchangeWaitForIdleJoinsAcceptedConnectionOwnership(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	exchangeCtx, exchangeCancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	exchange := &Exchange{
		ctx:                  exchangeCtx,
		cancel:               exchangeCancel,
		hostToServicePorts:   map[int]int{port: port},
		settings:             DefaultExchangeSettings(),
		servicePortListeners: map[int]net.Listener{port: listener},
		residents:            map[server.Id]*Resident{},
	}
	message := clientconnect.MessagePoolGet(2 * 1024)
	witnessBeforeRelease := clientconnect.MessagePoolShareReadOnly(message)
	witnessAfterJoin := clientconnect.MessagePoolShareReadOnly(message)
	workerEntered := make(chan struct{})
	releaseWorker := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseWorker) })
	exchange.handleExchangeConnectionForTest = func(net.Conn) {
		close(workerEntered)
		<-releaseWorker
		clientconnect.MessagePoolReturn(message)
	}
	go exchange.Run()
	peerConn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		exchange.Close()
		clientconnect.MessagePoolReturn(witnessBeforeRelease)
		clientconnect.MessagePoolReturn(witnessAfterJoin)
		clientconnect.MessagePoolReturn(message)
		t.Fatal(err)
	}
	defer peerConn.Close()
	select {
	case <-workerEntered:
	case <-testCtx.Done():
		t.Fatalf("accepted connection did not reach ownership barrier: %v", testCtx.Err())
	}

	exchange.Close()
	waitResult := make(chan bool, 1)
	go func() {
		waitResult <- exchange.WaitForIdle(testCtx)
	}()
	select {
	case result := <-waitResult:
		t.Fatalf("exchange idle returned before accepted ownership release: %t", result)
	default:
	}
	if clientconnect.MessagePoolReturn(witnessBeforeRelease) {
		t.Fatal("accepted connection lost its pooled owner before worker release")
	}

	releaseOnce.Do(func() { close(releaseWorker) })
	select {
	case result := <-waitResult:
		if !result {
			t.Fatal("exchange idle deadline expired after accepted ownership release")
		}
	case <-testCtx.Done():
		t.Fatalf("exchange did not join accepted connection: %v", testCtx.Err())
	}
	if !clientconnect.MessagePoolReturn(witnessAfterJoin) {
		t.Fatal("accepted connection retained its pooled owner after exchange idle")
	}
}

// Resident teardown joins the internal connect client before the exchange
// publishes idle, including a reliable send held in its Ack callback.
func TestExchangeWaitForIdleJoinsResidentInternalClientOwnership(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	exchangeCtx, exchangeCancel := context.WithCancel(context.Background())
	settings := DefaultExchangeSettings()
	exchange := &Exchange{
		ctx:            exchangeCtx,
		cancel:         exchangeCancel,
		settings:       settings,
		residents:      map[server.Id]*Resident{},
		connections:    map[server.Id]map[server.Id]context.CancelFunc{},
		drainedClients: map[server.Id]struct{}{},
	}
	clientSettings := clientconnect.DefaultClientSettingsWithBufferSize(settings.ExchangeBufferSize)
	clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	clientSettings.ControlPingTimeout = 0
	clientSettings.Log = clientconnect.NewNoopLogger()
	client := clientconnect.NewClient(
		exchangeCtx,
		clientconnect.ControlId,
		clientconnect.NewNoContractClientOob(),
		clientSettings,
	)
	clientId := server.NewId()
	client.ContractManager().AddNoContractPeer(clientconnect.Id(clientId))
	residentCtx, residentCancel := context.WithCancel(exchangeCtx)
	resident := &Resident{
		ctx:                residentCtx,
		cancel:             residentCancel,
		exchange:           exchange,
		clientId:           clientId,
		instanceId:         server.NewId(),
		residentId:         server.NewId(),
		client:             client,
		transports:         map[*clientTransport]bool{},
		forwards:           map[server.Id]*ResidentForward{},
		clientReceiveUnsub: func() {},
		clientForwardUnsub: func() {},
	}
	exchange.residents[clientId] = resident
	send, _, closeTransport, err := resident.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()

	ackEntered := make(chan struct{})
	releaseAck := make(chan struct{})
	var ackEnteredOnce sync.Once
	var releaseAckOnce sync.Once
	defer releaseAckOnce.Do(func() { close(releaseAck) })
	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "resident close ownership"},
	)
	if !client.SendWithTimeout(
		frame,
		clientconnect.Id(clientId),
		func(error) {
			ackEnteredOnce.Do(func() { close(ackEntered) })
			<-releaseAck
		},
		time.Second,
	) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		t.Fatal("resident internal client did not admit the reliable send")
	}

	var transferFrameBytes []byte
	select {
	case transferFrameBytes = <-send:
	case <-testCtx.Done():
		t.Fatalf("resident internal client did not write its transfer frame: %v", testCtx.Err())
	}
	witnessBeforeRelease := clientconnect.MessagePoolShareReadOnly(transferFrameBytes)
	witnessAfterJoin := clientconnect.MessagePoolShareReadOnly(transferFrameBytes)
	clientconnect.MessagePoolReturn(transferFrameBytes)

	exchange.residentWorkers.Add(1)
	go func() {
		defer exchange.residentWorkers.Done()
		<-resident.Done()
		exchange.closeResidentAndWait(resident)
	}()
	exchange.Close()
	waitResult := make(chan bool, 1)
	go func() {
		waitResult <- exchange.WaitForIdle(testCtx)
	}()
	select {
	case <-ackEntered:
	case <-testCtx.Done():
		t.Fatalf("resident send cleanup did not reach Ack callback: %v", testCtx.Err())
	}
	select {
	case result := <-waitResult:
		t.Fatalf("exchange idle returned before resident client ownership release: %t", result)
	default:
	}
	if clientconnect.MessagePoolReturn(witnessBeforeRelease) {
		t.Fatal("resident client lost its pooled owner before Ack cleanup release")
	}

	releaseAckOnce.Do(func() { close(releaseAck) })
	select {
	case result := <-waitResult:
		if !result {
			t.Fatal("exchange idle deadline expired after resident client release")
		}
	case <-testCtx.Done():
		t.Fatalf("exchange did not join resident internal client: %v", testCtx.Err())
	}
	if !clientconnect.MessagePoolReturn(witnessAfterJoin) {
		t.Fatal("resident client retained its pooled transfer frame after exchange idle")
	}
}

// An admitted resident controller callback retains a live database context
// until the internal client joins it, then closes before exchange idle.
func TestExchangeResidentControllerContextOutlivesTransportCancellation(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	exchangeCtx, exchangeCancel := context.WithCancel(context.Background())
	settings := DefaultExchangeSettings()
	exchange := &Exchange{
		ctx:            exchangeCtx,
		cancel:         exchangeCancel,
		settings:       settings,
		residents:      map[server.Id]*Resident{},
		connections:    map[server.Id]map[server.Id]context.CancelFunc{},
		drainedClients: map[server.Id]struct{}{},
	}
	residentCtx, residentCancel := context.WithCancel(exchangeCtx)
	clientSettings := clientconnect.DefaultClientSettingsWithBufferSize(settings.ExchangeBufferSize)
	clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	clientSettings.ControlPingTimeout = 0
	clientSettings.Log = clientconnect.NewNoopLogger()
	residentClient := clientconnect.NewClient(
		residentCtx,
		clientconnect.ControlId,
		clientconnect.NewNoContractClientOob(),
		clientSettings,
	)
	clientId := server.NewId()
	residentClient.ContractManager().AddNoContractPeer(clientconnect.Id(clientId))
	residentController := newResidentController(
		residentCtx,
		clientId,
		nil,
		settings,
	)
	resident := &Resident{
		ctx:                residentCtx,
		cancel:             residentCancel,
		exchange:           exchange,
		clientId:           clientId,
		instanceId:         server.NewId(),
		residentId:         server.NewId(),
		client:             residentClient,
		residentController: residentController,
		transports:         map[*clientTransport]bool{},
		forwards:           map[server.Id]*ResidentForward{},
		controlLimiter:     newLimiter(residentCtx, 0),
		clientForwardUnsub: func() {},
	}
	resident.clientReceiveUnsub = residentClient.AddReceiveCallback(resident.handleClientReceive)
	exchange.residents[clientId] = resident

	residentSend, residentReceive, closeTransport, err := resident.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	sourceCtx, sourceCancel := context.WithCancel(context.Background())
	sourceClient := clientconnect.NewClient(
		sourceCtx,
		clientconnect.Id(clientId),
		clientconnect.NewNoContractClientOob(),
		clientSettings,
	)
	sourceClient.ContractManager().AddNoContractPeer(clientconnect.ControlId)
	sourceSendTransport := clientconnect.NewSendGatewayTransport()
	sourceReceiveTransport := clientconnect.NewReceiveGatewayTransport()
	sourceClient.RouteManager().UpdateTransport(
		sourceSendTransport,
		[]clientconnect.Route{residentReceive},
	)
	sourceClient.RouteManager().UpdateTransport(
		sourceReceiveTransport,
		[]clientconnect.Route{residentSend},
	)

	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	callbackReturned := make(chan struct{})
	clientJoinEntered := make(chan struct{})
	var callbackOnce sync.Once
	var releaseOnce sync.Once
	var clientJoinOnce sync.Once
	residentController.beforeHandleControlFramesForTest = func() {
		callbackOnce.Do(func() {
			close(callbackEntered)
			<-releaseCallback
			close(callbackReturned)
		})
	}
	resident.beforeClientCloseJoinForTest = func() {
		clientJoinOnce.Do(func() { close(clientJoinEntered) })
	}

	exchange.residentWorkers.Add(1)
	go func() {
		defer exchange.residentWorkers.Done()
		<-resident.Done()
		exchange.closeResidentAndWait(resident)
	}()
	defer func() {
		releaseOnce.Do(func() { close(releaseCallback) })
		sourceClient.RouteManager().RemoveTransport(sourceSendTransport)
		sourceClient.RouteManager().RemoveTransport(sourceReceiveTransport)
		sourceCancel()
		sourceClient.CloseAndWait(testCtx)
		closeTransport()
		exchange.Close()
		residentController.Close()
		exchange.WaitForIdle(testCtx)
	}()

	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "resident controller close ordering"},
	)
	if !sourceClient.SendWithTimeout(
		frame,
		clientconnect.ControlId,
		nil,
		time.Second,
	) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		t.Fatal("source client did not admit resident control frame")
	}
	select {
	case <-callbackEntered:
	case <-testCtx.Done():
		t.Fatalf("resident control callback did not enter: %v", testCtx.Err())
	}

	exchange.Close()
	idleResult := make(chan bool, 1)
	go func() {
		idleResult <- exchange.WaitForIdle(testCtx)
	}()
	select {
	case <-clientJoinEntered:
	case <-testCtx.Done():
		t.Fatalf("resident close did not reach client join: %v", testCtx.Err())
	}
	if err := residentController.ctx.Err(); err != nil {
		t.Fatalf("controller context canceled before admitted callback joined: %v", err)
	}
	select {
	case result := <-idleResult:
		t.Fatalf("exchange idle returned before controller callback release: %t", result)
	default:
	}

	releaseOnce.Do(func() { close(releaseCallback) })
	select {
	case <-callbackReturned:
	case <-testCtx.Done():
		t.Fatalf("resident control callback did not return: %v", testCtx.Err())
	}
	select {
	case result := <-idleResult:
		if !result {
			t.Fatal("exchange idle deadline expired after controller callback release")
		}
	case <-testCtx.Done():
		t.Fatalf("exchange did not join resident controller callback: %v", testCtx.Err())
	}
	if err := residentController.ctx.Err(); err == nil {
		t.Fatal("controller context remained live after resident client join")
	}
}
