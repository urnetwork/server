package proxy

import (
	"context"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A replacement may accept a customer-driven lazy open as soon as Warp flips
// public traffic, while the old instance is still draining. That open must
// mint a fresh window identity: restoring the old instance's live identity
// runs one logical Connect client in two processes and makes return routing
// nondeterministic. Writes stay buffered too, then the replacement publishes
// its newest fresh snapshot once the drain-complete handoff releases the gate.
func TestWindowIdentityRestoreGateProtectsLazyOpen(t *testing.T) {
	ctx := context.Background()
	proxyId := server.NewId()
	oldIdentity := testWindowIdentity(server.NewId(), "old")
	freshIdentity := testWindowIdentity(server.NewId(), "fresh")
	persisted := []*model.ProxyWindowClientIdentity{oldIdentity}
	loadCount := 0
	storeCount := 0

	gate := newWindowIdentityRestoreGate(true)
	store := &windowIdentityStore{
		ctx:         ctx,
		proxyId:     proxyId,
		restoreGate: gate,
		loadWindowIdentities: func(context.Context, server.Id) []*model.ProxyWindowClientIdentity {
			loadCount += 1
			return persisted
		},
		storeWindowIdentities: func(_ context.Context, _ server.Id, identities []*model.ProxyWindowClientIdentity) {
			storeCount += 1
			persisted = identities
		},
	}

	if identities := store.LoadWindowClientIdentities(); len(identities) != 0 {
		t.Fatalf("held replacement restored %d old identities, want none", len(identities))
	}
	if loadCount != 0 {
		t.Fatalf("held replacement reached the persistence load %d times, want 0", loadCount)
	}

	store.StoreWindowClientIdentities([]*connect.WindowClientIdentity{
		connectWindowIdentity(freshIdentity),
	})
	if storeCount != 0 {
		t.Fatalf("replacement published %d identity snapshots before drain completion", storeCount)
	}
	if persisted[0].ClientId != oldIdentity.ClientId {
		t.Fatal("replacement changed the old instance's persisted snapshot while the gate was held")
	}

	gate.Release()
	if storeCount != 1 {
		t.Fatalf("gate release published %d snapshots, want 1", storeCount)
	}
	if len(persisted) != 1 || persisted[0].ClientId != freshIdentity.ClientId {
		t.Fatal("gate release did not publish the replacement's fresh identity")
	}
	identities := store.LoadWindowClientIdentities()
	if loadCount != 1 || len(identities) != 1 || server.Id(identities[0].ClientId) != freshIdentity.ClientId {
		t.Fatal("released gate did not restore from the replacement's published snapshot")
	}
}

// Release must drain writes that arrive while an earlier buffered snapshot is
// being published. The barriers force that ordering without sleeps: otherwise
// a late window mutation could be stranded forever behind an already-open gate.
func TestWindowIdentityRestoreGateJoinsConcurrentReleaseWrite(t *testing.T) {
	ctx := context.Background()
	proxyId := server.NewId()
	firstIdentity := testWindowIdentity(server.NewId(), "first")
	secondIdentity := testWindowIdentity(server.NewId(), "second")
	thirdIdentity := testWindowIdentity(server.NewId(), "third")

	firstStoreStarted := make(chan struct{})
	releaseFirstStore := make(chan struct{})
	var stateLock sync.Mutex
	storeCount := 0
	var persisted []*model.ProxyWindowClientIdentity

	gate := newWindowIdentityRestoreGate(true)
	store := &windowIdentityStore{
		ctx:         ctx,
		proxyId:     proxyId,
		restoreGate: gate,
		loadWindowIdentities: func(context.Context, server.Id) []*model.ProxyWindowClientIdentity {
			return nil
		},
		storeWindowIdentities: func(_ context.Context, _ server.Id, identities []*model.ProxyWindowClientIdentity) {
			stateLock.Lock()
			storeCount += 1
			callCount := storeCount
			stateLock.Unlock()
			if callCount == 1 {
				close(firstStoreStarted)
				<-releaseFirstStore
			}
			stateLock.Lock()
			persisted = identities
			stateLock.Unlock()
		},
	}

	store.StoreWindowClientIdentities([]*connect.WindowClientIdentity{
		connectWindowIdentity(firstIdentity),
	})
	releaseDone := make(chan struct{})
	go func() {
		defer close(releaseDone)
		gate.Release()
	}()
	<-firstStoreStarted
	store.StoreWindowClientIdentities([]*connect.WindowClientIdentity{
		connectWindowIdentity(secondIdentity),
	})
	close(releaseFirstStore)
	<-releaseDone

	stateLock.Lock()
	if storeCount != 2 || len(persisted) != 1 || persisted[0].ClientId != secondIdentity.ClientId {
		t.Fatalf("release result count=%d persisted=%v, want the second buffered snapshot", storeCount, persisted)
	}
	stateLock.Unlock()

	store.StoreWindowClientIdentities([]*connect.WindowClientIdentity{
		connectWindowIdentity(thirdIdentity),
	})
	stateLock.Lock()
	defer stateLock.Unlock()
	if storeCount != 3 || len(persisted) != 1 || persisted[0].ClientId != thirdIdentity.ClientId {
		t.Fatalf("post-release result count=%d persisted=%v, want direct third snapshot", storeCount, persisted)
	}
}

// Device churn during a long serialized host drain can create more than one
// store object for the same proxy ID. Only the latest mutation for that proxy
// may cross the gate; flushing both store generations leaves Redis at the
// mercy of map iteration order and can resurrect the retired device's IDs.
func TestWindowIdentityRestoreGateKeepsNewestDeviceGeneration(t *testing.T) {
	ctx := context.Background()
	proxyId := server.NewId()
	retiredIdentity := testWindowIdentity(server.NewId(), "retired")
	currentIdentity := testWindowIdentity(server.NewId(), "current")

	gate := newWindowIdentityRestoreGate(true)
	storeCount := 0
	var persisted []*model.ProxyWindowClientIdentity
	newStore := func() *windowIdentityStore {
		return &windowIdentityStore{
			ctx:         ctx,
			proxyId:     proxyId,
			restoreGate: gate,
			loadWindowIdentities: func(context.Context, server.Id) []*model.ProxyWindowClientIdentity {
				return nil
			},
			storeWindowIdentities: func(_ context.Context, _ server.Id, identities []*model.ProxyWindowClientIdentity) {
				storeCount += 1
				persisted = identities
			},
		}
	}
	retiredStore := newStore()
	currentStore := newStore()
	retiredStore.StoreWindowClientIdentities([]*connect.WindowClientIdentity{
		connectWindowIdentity(retiredIdentity),
	})
	currentStore.StoreWindowClientIdentities([]*connect.WindowClientIdentity{
		connectWindowIdentity(currentIdentity),
	})

	gate.Release()
	if storeCount != 1 {
		t.Fatalf("gate published %d generations for one proxy ID, want only the latest", storeCount)
	}
	if len(persisted) != 1 || persisted[0].ClientId != currentIdentity.ClientId {
		t.Fatal("gate did not preserve the current device generation")
	}
}

func testWindowIdentity(clientId server.Id, byJwt string) *model.ProxyWindowClientIdentity {
	return &model.ProxyWindowClientIdentity{
		ClientId:       clientId,
		ByJwt:          byJwt,
		InstanceId:     server.NewId(),
		DestinationIds: []server.Id{server.NewId()},
	}
}

func connectWindowIdentity(identity *model.ProxyWindowClientIdentity) *connect.WindowClientIdentity {
	destinationIds := make([]connect.Id, 0, len(identity.DestinationIds))
	for _, destinationId := range identity.DestinationIds {
		destinationIds = append(destinationIds, connect.Id(destinationId))
	}
	return &connect.WindowClientIdentity{
		ClientId:    connect.Id(identity.ClientId),
		ByJwt:       identity.ByJwt,
		InstanceId:  connect.Id(identity.InstanceId),
		Destination: connect.RequireMultiHopId(destinationIds...),
	}
}
