// Resident routing ownership must exist before its lifecycle can retire it.
// The native nomination control forces cleanup across that publication seam.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// A canceled resident may finish its routing cleanup immediately after its
// workers launch. Nomination must not subsequently put that retired owner,
// including its client and queues, back into the process-long resident map.
func TestResidentNominationDoesNotRepublishRetiredOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		defer func() {
			exchange.Close()
			if !exchange.WaitForIdle(ctx) {
				t.Error("resident publication fixture did not join")
			}
		}()
		clientId := server.NewId()
		joinEntered := make(chan struct{})
		var joinOnce sync.Once
		var retired *Resident
		exchange.beforeResidentProfileForTest = func(resident *Resident) {
			retired = resident
			resident.beforeClientCloseJoinForTest = func() {
				joinOnce.Do(func() { close(joinEntered) })
			}
		}
		exchange.afterResidentWorkersStartedForTest = func(resident *Resident) {
			resident.Cancel()
			// closeResidentAndWait removes the routing entry before entering
			// the owned client join. This barrier forces the old cleanup-first
			// ordering without a sleep, poll, or short negative timeout.
			select {
			case <-joinEntered:
			case <-ctx.Done():
				t.Fatal("resident did not reach routing retirement before client join")
			}
		}
		if !exchange.NominateLocalResidentWithContext(ctx, clientId, server.NewId(), nil) {
			t.Fatal("synthetic resident did not reach lifecycle publication")
		}
		if retired == nil {
			t.Fatal("synthetic resident construction was not observed")
		}
		exchange.Close()
		if !exchange.WaitForIdle(ctx) {
			t.Fatal("retired resident lifecycle did not join")
		}
		exchange.stateLock.Lock()
		retained := exchange.residents[clientId]
		retainedCount := len(exchange.residents)
		exchange.stateLock.Unlock()
		if retained != nil || retainedCount != 0 {
			t.Fatalf("nomination republished a retired resident after cleanup: retained=%t count=%d", retained == retired, retainedCount)
		}
		if !retired.IsDone() {
			t.Fatal("retirement barrier did not cancel the resident")
		}
	})
}

// A forward has the same publication boundary as a nominated resident. Force
// the real empty forward worker to retire before construction returns; its
// completed queue must not remain as a historical destination map entry.
func TestResidentForwardDoesNotRepublishRetiredOwner(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		settings := DefaultExchangeSettings()
		settings.ForwardEnforceActiveContracts = false
		resident := &Resident{
			ctx:      ctx,
			cancel:   cancel,
			exchange: &Exchange{ctx: ctx, cancel: cancel, settings: settings},
			clientId: server.NewId(),
			forwards: map[server.Id]*ResidentForward{},
		}
		defer resident.cancelForwards()
		var retired *ResidentForward
		resident.afterForwardWorkersStartedForTest = func(forward *ResidentForward) {
			retired = forward
			forward.Cancel()
			if err := waitForWorkerGroup(ctx, &resident.forwardWorkers, "synthetic forward retirement"); err != nil {
				t.Fatal(err)
			}
		}
		destinationId := server.NewId()
		message := clientconnect.MessagePoolGet(32)
		witness := retainResidentPoolWitness(message)
		resident.processClientForward(clientconnect.TransferPath{
			SourceId:      clientconnect.Id(resident.clientId),
			DestinationId: clientconnect.Id(destinationId),
		}, message)
		synctest.Wait()
		requireResidentPoolOwnerReturned(t, witness, "retired forward publication offer")
		resident.stateLock.RLock()
		retained := resident.forwards[destinationId]
		retainedCount := len(resident.forwards)
		resident.stateLock.RUnlock()
		if retired == nil || !retired.IsDone() {
			t.Fatal("forward retirement boundary was not reached")
		}
		if retained != nil || retainedCount != 0 {
			t.Fatalf("forward construction republished a retired owner after cleanup: retained=%t count=%d", retained == retired, retainedCount)
		}
	})
}

// Publishing before launch must still reclaim the candidate when shutdown
// has closed worker admission, including the exact offered pooled reference.
func TestResidentForwardPublicationRefusedAfterWorkerAdmissionCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ForwardEnforceActiveContracts = false
	resident := &Resident{
		ctx:                  ctx,
		cancel:               cancel,
		exchange:             &Exchange{ctx: ctx, cancel: cancel, settings: settings},
		clientId:             server.NewId(),
		forwards:             map[server.Id]*ResidentForward{},
		forwardWorkersClosed: true,
	}
	defer resident.cancelForwards()
	message := clientconnect.MessagePoolGet(32)
	witness := retainResidentPoolWitness(message)
	resident.processClientForward(clientconnect.TransferPath{
		SourceId:      clientconnect.Id(resident.clientId),
		DestinationId: clientconnect.Id(server.NewId()),
	}, message)
	requireResidentPoolOwnerReturned(t, witness, "refused forward publication offer")
	if len(resident.forwards) != 0 {
		t.Fatal("closed worker admission retained the unpublished forward candidate")
	}
	if ctx.Err() != nil {
		t.Fatal("refusing a forward candidate canceled the independent resident lifetime")
	}
}
