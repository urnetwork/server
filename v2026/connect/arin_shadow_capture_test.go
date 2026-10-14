package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func shadowRegistryAnnounce(r *arinShadowOwnerRegistry, id server.Id, address string) *ConnectionAnnounce {
	ctx, cancel := context.WithCancel(context.Background())
	a := &ConnectionAnnounce{ctx: ctx, cancel: cancel, clientId: server.NewId(), handlerId: server.NewId(), clientAddress: address, connectionId: &id}
	a.shadowCaptureLease = r.add(id, a)
	return a
}

func TestArinShadowRegistryExactGenerationAndClose(t *testing.T) {
	r := newArinShadowOwnerRegistry(2)
	id := server.NewId()
	a := shadowRegistryAnnounce(r, id, "192.0.2.1:123")
	targets, stats, err := r.targets([]server.Id{id})
	if err != nil || stats.TrackedConnections != 1 {
		t.Fatal("registration missing")
	}
	snapshot, ok := targets[0].Owner.ArinShadowCurrentConnection()
	if !ok || snapshot.ConnectionId != id || snapshot.ClientId != a.clientId || snapshot.HandlerId != a.handlerId || snapshot.Address.String() != "192.0.2.1" {
		t.Fatal("incorrect owner binding")
	}
	a.cancel()
	if _, ok = targets[0].Owner.ArinShadowCurrentConnection(); ok {
		t.Fatal("canceled owner captured before cleanup")
	}
	a.releaseArinShadowOwner()
	a.releaseArinShadowOwner()
	b := shadowRegistryAnnounce(r, id, "192.0.2.2:123")
	defer b.cancel()
	defer b.releaseArinShadowOwner()
	if _, ok = targets[0].Owner.ArinShadowCurrentConnection(); ok {
		t.Fatal("old view accepted replacement in same mask")
	}
	targets, _, _ = r.targets([]server.Id{id})
	if _, ok = targets[0].Owner.ArinShadowCurrentConnection(); !ok {
		t.Fatal("replacement current control missing")
	}
}

func TestArinShadowRegistryCollisionCapacityAndUnknownTargets(t *testing.T) {
	r := newArinShadowOwnerRegistry(1)
	id := server.NewId()
	a := shadowRegistryAnnounce(r, id, "192.0.2.1:1")
	defer a.cancel()
	defer a.releaseArinShadowOwner()
	old, _, _ := r.targets([]server.Id{id})
	b := shadowRegistryAnnounce(r, id, "192.0.2.1:2")
	defer b.cancel()
	defer b.releaseArinShadowOwner()
	if _, ok := old[0].Owner.ArinShadowCurrentConnection(); ok {
		t.Fatal("duplicate durable identity accepted")
	}
	b.releaseArinShadowOwner()
	targets, _, _ := r.targets([]server.Id{id})
	if targets[0].Owner != nil {
		t.Fatal("ambiguous generation silently recovered")
	}
	id2 := server.NewId()
	c := shadowRegistryAnnounce(r, id2, "192.0.2.2:2")
	defer c.cancel()
	defer c.releaseArinShadowOwner()
	targets, stats, _ := r.targets([]server.Id{id2})
	if len(targets) != 1 || targets[0].Owner != nil || stats.OverflowConnections != 1 || stats.TrackedConnections != 1 {
		t.Fatal("capacity hid missing owner")
	}
	c.releaseArinShadowOwner()
	_, stats, _ = r.targets([]server.Id{id})
	if stats.OverflowConnections != 0 {
		t.Fatal("overflow lease leaked")
	}
	if _, _, err := r.targets([]server.Id{id, id}); err == nil {
		t.Fatal("duplicate request admitted")
	}
	if _, _, err := r.targets(make([]server.Id, 257)); err == nil {
		t.Fatal("oversized request admitted")
	}
}

func TestArinShadowRegistryConcurrentCaptureAndRelease(t *testing.T) {
	r := newArinShadowOwnerRegistry(1)
	id := server.NewId()
	a := shadowRegistryAnnounce(r, id, "192.0.2.1:1")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				targets, _, err := r.targets([]server.Id{id})
				if err == nil && targets[0].Owner != nil {
					targets[0].Owner.ArinShadowCurrentConnection()
				}
			}
		}()
	}
	a.cancel()
	a.releaseArinShadowOwner()
	wg.Wait()
	_, stats, _ := r.targets([]server.Id{id})
	if stats.TrackedConnections != 0 {
		t.Fatal("closed registry leaked")
	}
}

func TestArinShadowRegistryProductionSetAndJoinedRelease(t *testing.T) {
	id := server.NewId()
	ctx, cancel := context.WithCancel(context.Background())
	a := &ConnectionAnnounce{ctx: ctx, cancel: cancel, clientId: server.NewId(), handlerId: server.NewId(), clientAddress: "192.0.2.1:1"}
	a.setConnectionId(id)
	targets, _, err := CaptureArinShadowOwners([]server.Id{id})
	if err != nil || targets[0].Owner == nil {
		t.Fatal("production setter not registered")
	}
	// The registry's reader must not retain its mutex while waiting for state.
	a.stateLock.Lock()
	done := make(chan struct{})
	go func() { targets[0].Owner.ArinShadowCurrentConnection(); close(done) }()
	lookedUp := make(chan struct{})
	go func() { CaptureArinShadowOwners([]server.Id{id}); close(lookedUp) }()
	select {
	case <-lookedUp:
	case <-time.After(time.Second):
		a.stateLock.Unlock()
		t.Fatal("registry mutex held while waiting for owner")
	}
	a.stateLock.Unlock()
	<-done
	cancel()
	a.releaseArinShadowOwner()
	targets, _, _ = CaptureArinShadowOwners([]server.Id{id})
	if targets[0].Owner != nil {
		t.Fatal("joined release retained owner")
	}
}
