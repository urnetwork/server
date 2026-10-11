package model

// The route rejection registry of a real notification owner, driven through
// its synthetic subscription transport and virtual time. No Redis, PostgreSQL
// or wall-clock wait is involved.

import (
	"encoding/hex"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

func testDestinationRejection() ContractRouteRejection {
	return ContractRouteRejection{
		Cause:          ContractRouteDestinationInactive,
		ResolutionPath: "rejected",
		Relationship: ProvideRelationshipDetails{
			Mode:                 ProvideModePublic,
			SourceLifecycle:      NetworkClientLifecycleActiveTop,
			DestinationLifecycle: NetworkClientLifecycleInactiveDerived,
			SourceOwner:          NetworkClientSourceOwnerOther,
			DestinationOwner:     NetworkClientSourceOwnerEgressProber,
		},
	}
}

// Returns a route whose reverse pair falls in a different bucket than avoid.
func testRouteOutsideBucket(avoid int) (sourceId server.Id, destinationId server.Id) {
	for {
		sourceId, destinationId = server.NewId(), server.NewId()
		key := contractOriginPairHash(destinationId, sourceId)
		if int(key[0])%contractOriginBucketCount != avoid {
			return
		}
	}
}

// Arms the bucket of a route's reverse pair through a real watch and returns
// the watch with its acknowledged synthetic subscription.
func testWatchRouteBucket(t *testing.T, owner *ContractOriginNotifications, opened chan testContractOriginOpen, sourceId server.Id, destinationId server.Id) (*ContractOriginWatch, testContractOriginOpen) {
	t.Helper()
	watch := owner.Watch(destinationId, sourceId)
	synctest.Wait()
	connection := <-opened
	connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
	synctest.Wait()
	return watch, connection
}

func TestContractRouteRejectionReplaysOnlyIdenticalRequestsUntilTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		rejection := testDestinationRejection()
		if !owner.RecordRouteRejection(sourceId, destinationId, false, rejection, nil) {
			t.Fatal("refusal was not stored")
		}
		if replayed, ok := owner.RouteRejection(sourceId, destinationId, false); !ok || replayed != rejection {
			t.Fatalf("identical request replay = %+v, %t", replayed, ok)
		}
		if _, ok := owner.RouteRejection(sourceId, destinationId, true); ok {
			t.Fatal("a companion request replayed a non-companion refusal")
		}
		if _, ok := owner.RouteRejection(destinationId, sourceId, false); ok {
			t.Fatal("the reverse route replayed the refusal")
		}
		time.Sleep(settings.RouteRejectionTimeout - time.Millisecond)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("refusal expired before its timeout")
		}
		time.Sleep(time.Millisecond)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("refusal was replayed at its timeout")
		}
	})
}

func TestContractRouteRejectionBoundEvictsOldest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		settings.MaxRouteRejections = 2
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		destinationId := server.NewId()
		sourceIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		for _, sourceId := range sourceIds {
			owner.RecordRouteRejection(sourceId, destinationId, false, testDestinationRejection(), nil)
		}
		if _, ok := owner.RouteRejection(sourceIds[0], destinationId, false); ok {
			t.Fatal("the oldest refusal survived past the bound")
		}
		for _, sourceId := range sourceIds[1:] {
			if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
				t.Fatal("a newer refusal was evicted")
			}
		}
		owner.routeRejections.stateLock.Lock()
		entries, slots := len(owner.routeRejections.entries), owner.routeRejections.count
		owner.routeRejections.stateLock.Unlock()
		if entries != 2 || slots != 2 {
			t.Fatalf("bound of 2 holds entries=%d slots=%d", entries, slots)
		}
	})
}

// A new origin for the reverse pair, published by any owner, opens the route.
func TestContractRouteRejectionOriginEdgeRemovesRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch, connection := testWatchRouteBucket(t, owner, opened, sourceId, destinationId)
		watch.Close()
		otherSourceId := server.NewId()
		missing := ContractRouteRejection{Cause: ContractRouteMissingOrigin, ResolutionPath: "stream_fallback"}
		for _, companion := range []bool{false, true} {
			owner.RecordRouteRejection(sourceId, destinationId, companion, missing, nil)
		}
		owner.RecordRouteRejection(otherSourceId, destinationId, true, missing, nil)
		key := contractOriginPairHash(destinationId, sourceId)
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
		synctest.Wait()
		for _, companion := range []bool{false, true} {
			if _, ok := owner.RouteRejection(sourceId, destinationId, companion); ok {
				t.Fatalf("an origin edge left the route refused (companion=%t)", companion)
			}
		}
		if _, ok := owner.RouteRejection(otherSourceId, destinationId, true); !ok {
			t.Fatal("an origin edge for another pair removed this route")
		}
	})
}

// A fresh acknowledgement can follow missed edges, so it opens its bucket.
func TestContractRouteRejectionAcknowledgementClearsItsBucket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch, connection := testWatchRouteBucket(t, owner, opened, sourceId, destinationId)
		defer watch.Close()
		key := contractOriginPairHash(destinationId, sourceId)
		otherSourceId, otherDestinationId := testRouteOutsideBucket(int(key[0]) % contractOriginBucketCount)
		owner.RecordRouteRejection(sourceId, destinationId, false, testDestinationRejection(), nil)
		owner.RecordRouteRejection(otherSourceId, otherDestinationId, false, testDestinationRejection(), nil)
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("a reacknowledged bucket kept a refusal that may have missed an edge")
		}
		if _, ok := owner.RouteRejection(otherSourceId, otherDestinationId, false); !ok {
			t.Fatal("an acknowledgement cleared another bucket")
		}
	})
}

// A verdict whose read overlapped an origin edge is not stored: the edge
// closed the fence its read armed, before the edge removed entries.
func TestContractRouteRejectionClosedFenceRefusesStaleVerdict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch, connection := testWatchRouteBucket(t, owner, opened, sourceId, destinationId)
		defer watch.Close()
		missing := ContractRouteRejection{Cause: ContractRouteMissingOrigin, ResolutionPath: "requested_companion"}

		fence := watch.Update()
		key := contractOriginPairHash(destinationId, sourceId)
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
		synctest.Wait()
		if owner.RecordRouteRejection(sourceId, destinationId, true, missing, fence) {
			t.Fatal("a verdict read across an origin edge was stored")
		}
		if _, ok := owner.RouteRejection(sourceId, destinationId, true); ok {
			t.Fatal("a fenced verdict was replayed")
		}

		if !owner.RecordRouteRejection(sourceId, destinationId, true, missing, watch.Update()) {
			t.Fatal("a verdict with an open fence was not stored")
		}
	})
}

// This owner's own commit opens the route without waiting for its publication.
func TestContractRouteRejectionLocalCommitRemovesRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(sourceId, destinationId, true, ContractRouteRejection{Cause: ContractRouteMissingOrigin}, nil)
		notifyCommittedContractOrigin(WithContractOriginNotifications(t.Context(), owner), destinationId, sourceId)
		if _, ok := owner.RouteRejection(sourceId, destinationId, true); ok {
			t.Fatal("a local origin commit left the route refused")
		}
	})
}

func TestContractRouteRejectionClosedOwnerKeepsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		sourceId, destinationId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(sourceId, destinationId, false, testDestinationRejection(), nil)
		owner.Close()
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("a closed owner replayed a refusal no edge can remove")
		}
		if owner.RecordRouteRejection(sourceId, destinationId, false, testDestinationRejection(), nil) {
			t.Fatal("a closed owner stored a refusal")
		}
		var unowned *ContractOriginNotifications
		if unowned.RecordRouteRejection(sourceId, destinationId, false, testDestinationRejection(), nil) {
			t.Fatal("a nil owner stored a refusal")
		}
		if _, ok := unowned.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("a nil owner replayed a refusal")
		}
	})
}

func TestContractRouteRejectionZeroBoundDisables(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		settings.MaxRouteRejections = 0
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		if owner.RecordRouteRejection(sourceId, destinationId, false, testDestinationRejection(), nil) {
			t.Fatal("a zero bound stored a refusal")
		}
	})
}
