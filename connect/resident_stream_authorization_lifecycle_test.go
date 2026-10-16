// Stream authorization publishing remains owned through callback cancellation.
package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A callback may request cancellation but never joins its own worker. The
// external Run owner must retain it until the exact pooled frame is returned.
func TestResidentRunJoinsStreamAuthorizationWorkerAndFrame(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		residentCtx, residentCancel := context.WithCancel(ctx)
		settings := DefaultExchangeSettings()
		settings.StreamHopsPollInterval = time.Hour
		exchange := &Exchange{ctx: residentCtx, cancel: residentCancel, settings: settings}
		clientSettings := clientconnect.DefaultClientSettings()
		clientSettings.ControlPingTimeout = 0
		clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
		clientSettings.Log = clientconnect.NewNoopLogger()
		client := clientconnect.NewClient(residentCtx, clientconnect.ControlId, clientconnect.NewNoContractClientOob(), clientSettings)
		clientId := server.NewId()
		client.ContractManager().AddNoContractPeer(clientconnect.Id(clientId))
		resident := &Resident{ctx: residentCtx, cancel: residentCancel, exchange: exchange, clientId: clientId, client: client, transports: map[*clientTransport]bool{}, forwards: map[server.Id]*ResidentForward{}}

		callbackEntered, releaseCallback := make(chan struct{}), make(chan struct{})
		joinEntered := make(chan (<-chan struct{}), 1)
		var witnesses [][]byte
		var callbackOnce, releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(releaseCallback) })
		resident.afterStreamHopListenerStartForTest = func(listener *model.StreamHopListener) { listener.Resync() }
		resident.beforeListenerFrameSendForTest = func(frame *protocol.Frame) {
			witnesses = append(witnesses, retainResidentPoolWitness(frame.MessageBytes))
			callbackOnce.Do(func() {
				close(callbackEntered)
				<-releaseCallback
			})
		}
		var authorizationDone <-chan struct{}
		resident.beforeStreamAuthorizationJoinForTest = func(done <-chan struct{}) {
			authorizationDone = done
			joinEntered <- done
		}
		resident.afterStreamHopListenerCloseWaitForTest = func() {
			select {
			case <-authorizationDone:
			default:
				t.Error("listener owner completed before its publishing worker")
			}
		}
		runDone := make(chan struct{})
		go func() { defer close(runDone); resident.Run() }()
		defer func() {
			releaseOnce.Do(func() { close(releaseCallback) })
			resident.Cancel()
			select {
			case <-runDone:
			case <-time.After(10 * time.Second):
				t.Error("resident Run cleanup did not join")
			}
			if err := resident.CloseAndWait(ctx); err != nil {
				t.Error("resident cleanup failed", err)
			}
		}()
		select {
		case <-callbackEntered:
		case <-ctx.Done():
			t.Fatal("authorization callback did not enter")
		}
		resident.Cancel()
		select {
		case workerDone := <-joinEntered:
			if residentCtx.Err() == nil {
				t.Fatal("resident did not cancel the publishing worker before joining")
			}
			select {
			case <-workerDone:
				t.Fatal("publishing worker completed while its callback still owned a frame")
			default:
			}
			select {
			case <-runDone:
				t.Fatal("resident Run completed while its publisher still owned a frame")
			default:
			}
		case <-ctx.Done():
			t.Fatal("resident did not reach the publishing-worker join")
		}
		releaseOnce.Do(func() { close(releaseCallback) })
		select {
		case <-runDone:
		case <-ctx.Done():
			t.Fatal("resident did not join its released publishing worker")
		}
		if err := resident.CloseAndWait(ctx); err != nil {
			t.Fatal("resident did not join client queue owners", err)
		}
		if len(witnesses) != 1 {
			t.Fatalf("authorization publication retained %d frames, want one", len(witnesses))
		}
		requireResidentPoolOwnersReturned(t, witnesses, "joined authorization frame")
	})
}
