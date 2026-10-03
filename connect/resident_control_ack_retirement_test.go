package connect

import (
	"context"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A real sender ACK is not proof of controller application if teardown drops
// a queued pack. The parked first operation is an exact dependency barrier;
// no sleeps, database, customer traffic or remote contact are used.
func TestResidentAcknowledgedControlQueueSurvivesTransportClose(t *testing.T) {
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
	resident.startClientCallbackWorkers()
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
	var handled atomic.Int32
	var callbackOnce sync.Once
	var releaseOnce sync.Once
	var clientJoinOnce sync.Once
	residentController.beforeHandleControlFramesForTest = func() {
		handled.Add(1)
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

	// The second native transfer is ACKed while the first controller operation
	// is held. Its callback has completed admission into the resident queue.
	secondAck := make(chan error, 1)
	second := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.ControlPing{})
	if !sourceClient.SendWithTimeout(second, clientconnect.ControlId,
		func(err error) { secondAck <- err }, time.Second) {
		clientconnect.MessagePoolReturn(second.MessageBytes)
		t.Fatal("second control was not admitted")
	}
	select {
	case err := <-secondAck:
		if err != nil {
			t.Fatal("second control did not receive successful ACK", err)
		}
	case <-testCtx.Done():
		t.Fatal("second control did not ACK before teardown")
	}
	if handled.Load() != 1 {
		t.Fatal("blocked first operation did not preserve ordered queue")
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
	if got := handled.Load(); got != 2 {
		t.Fatalf("successful transfer ACK lost queued control during retirement: applied_batches=%d want=2", got)
	}

	if err := residentController.ctx.Err(); err == nil {
		t.Fatal("controller context remained live after resident client join")
	}
}
