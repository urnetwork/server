package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A socket for a superseded resident generation must fail at the generation
// boundary. Holding it for ExchangeResidentWaitTimeout delays the caller's
// model refresh and can stall every packet queued behind that forward.
func TestExchangeRejectsChangedResidentGenerationWithoutWaiting(t *testing.T) {
	exchangeCtx, exchangeCancel := context.WithCancel(context.Background())
	defer exchangeCancel()
	settings := DefaultExchangeSettings()
	settings.ExchangeResidentWaitTimeout = time.Hour
	settings.ExchangeResidentPollTimeout = time.Hour
	settings.ExchangeReadHeaderTimeout = time.Second
	settings.ExchangeWriteHeaderTimeout = time.Second

	clientId := server.NewId()
	requestedResidentId := server.NewId()
	currentResidentId := server.NewId()
	exchange := &Exchange{
		ctx:             exchangeCtx,
		cancel:          exchangeCancel,
		settings:        settings,
		residentChanges: map[server.Id]chan struct{}{},
		residents: map[server.Id]*Resident{
			clientId: {residentId: currentResidentId},
		},
		connections: map[server.Id]map[server.Id]context.CancelFunc{},
	}

	peerConn, exchangeConn := net.Pipe()
	defer peerConn.Close()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		exchange.handleExchangeConnection(exchangeConn)
	}()

	header := &ExchangeHeader{
		Version:    1,
		ClientId:   clientId,
		ResidentId: requestedResidentId,
		Op:         ExchangeOpForward,
	}
	buffer := NewDefaultExchangeBuffer(settings)
	if err := buffer.WriteHeader(context.Background(), peerConn, header); err != nil {
		t.Fatal(err)
	}

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("changed resident generation was retained for the resident wait timeout")
	}
}

// An accepted header can arrive in the short interval after its nomination is
// published but before the local resident is installed. A later installation
// must wake that handshake immediately so a different generation is rejected
// without waiting for the periodic poll.
func TestExchangeMissingHandshakeWakesOnResidentGenerationChange(t *testing.T) {
	exchangeCtx, exchangeCancel := context.WithCancel(context.Background())
	defer exchangeCancel()
	settings := DefaultExchangeSettings()
	settings.ExchangeResidentWaitTimeout = time.Hour
	settings.ExchangeResidentPollTimeout = time.Hour
	settings.ExchangeReadHeaderTimeout = time.Second
	settings.ExchangeWriteHeaderTimeout = time.Second

	clientId := server.NewId()
	requestedResidentId := server.NewId()
	currentResidentId := server.NewId()
	missingObserved := make(chan struct{})
	var missingOnce sync.Once
	exchange := &Exchange{
		ctx:             exchangeCtx,
		cancel:          exchangeCancel,
		settings:        settings,
		residents:       map[server.Id]*Resident{},
		residentChanges: map[server.Id]chan struct{}{},
		connections:     map[server.Id]map[server.Id]context.CancelFunc{},
		afterResidentMissingForTest: func() {
			missingOnce.Do(func() { close(missingObserved) })
		},
	}

	peerConn, exchangeConn := net.Pipe()
	defer peerConn.Close()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		exchange.handleExchangeConnection(exchangeConn)
	}()

	header := &ExchangeHeader{
		Version:    1,
		ClientId:   clientId,
		ResidentId: requestedResidentId,
		Op:         ExchangeOpForward,
	}
	buffer := NewDefaultExchangeBuffer(settings)
	if err := buffer.WriteHeader(context.Background(), peerConn, header); err != nil {
		t.Fatal(err)
	}
	select {
	case <-missingObserved:
	case <-time.After(time.Second):
		t.Fatal("accepted header did not reach the missing-resident wait boundary")
	}

	exchange.stateLock.Lock()
	exchange.residents[clientId] = &Resident{residentId: currentResidentId}
	exchange.notifyResidentChangedLocked(clientId)
	exchange.stateLock.Unlock()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("resident generation change did not wake the accepted handshake")
	}
}

// The lookup distinguishes a resident that has not reached the local map yet
// from a different generation, preserving the bounded cold-start wait.
func TestExchangeResidentGenerationLookupKeepsMissingDistinct(t *testing.T) {
	clientId := server.NewId()
	residentId := server.NewId()
	exchange := &Exchange{residents: map[server.Id]*Resident{}}

	resident, generation, residentChanged := exchange.matchResidentGeneration(clientId, residentId)
	if resident != nil || generation != exchangeResidentGenerationMissing {
		t.Fatalf("missing lookup = %p/%d, expected nil/%d", resident, generation, exchangeResidentGenerationMissing)
	}
	if residentChanged == nil {
		t.Fatal("missing lookup did not return a resident-change notification")
	}

	current := &Resident{residentId: residentId}
	exchange.stateLock.Lock()
	exchange.residents[clientId] = current
	exchange.notifyResidentChangedLocked(clientId)
	exchange.stateLock.Unlock()
	select {
	case <-residentChanged:
	default:
		t.Fatal("resident installation did not close the change notification")
	}
	resident, generation, residentChanged = exchange.matchResidentGeneration(clientId, residentId)
	if resident != current || generation != exchangeResidentGenerationCurrent {
		t.Fatalf("current lookup = %p/%d, expected %p/%d", resident, generation, current, exchangeResidentGenerationCurrent)
	}
	if residentChanged != nil {
		t.Fatal("current lookup returned a missing-resident notification")
	}
}
