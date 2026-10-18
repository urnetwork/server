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

// A missing destination row, which the ring keeps for its timeout.
func testDestinationRejection() ContractRouteRejection {
	return ContractRouteRejection{
		Cause:          ContractRouteDestinationInactive,
		ResolutionPath: "rejected",
		Relationship: ProvideRelationshipDetails{
			Mode:                 ProvideModePublic,
			SourceLifecycle:      NetworkClientLifecycleActiveTop,
			DestinationLifecycle: NetworkClientLifecycleMissing,
			SourceOwner:          NetworkClientSourceOwnerOther,
			DestinationOwner:     NetworkClientSourceOwnerUnknown,
		},
	}
}

// The production shape: an old provider replying to a retired prober window,
// whose row is present and inactive.
func testTerminalDestinationRejection() ContractRouteRejection {
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

// An old provider retries a reply to a retired window every 5s, and the load
// balancer spreads the retries over several api processes, so one process
// sees the route again only after the ring's timeout. A refusal of an
// inactive destination row is terminal and must still answer that retry.
func TestContractTerminalRouteRejectionOutlivesTheRingTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		rejection := testTerminalDestinationRejection()
		if !owner.RecordRouteRejection(sourceId, destinationId, false, rejection, nil) {
			t.Fatal("terminal refusal was not stored")
		}
		// the retry this process sees next, after the ring would have expired it
		time.Sleep(settings.RouteRejectionTimeout + 40*time.Second)
		if replayed, ok := owner.RouteRejection(sourceId, destinationId, false); !ok || replayed != rejection {
			t.Fatalf("retry after the ring timeout replay = %+v, %t", replayed, ok)
		}
		if _, ok := owner.RouteRejection(sourceId, destinationId, true); ok {
			t.Fatal("a companion request replayed a non-companion refusal")
		}
		if _, ok := owner.RouteRejection(destinationId, sourceId, false); ok {
			t.Fatal("the reverse route replayed the refusal")
		}
	})
}

// The timeout only bounds the cost of a mistake; a replay never extends it.
func TestContractTerminalRouteRejectionExpiresAtItsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(sourceId, destinationId, false, testTerminalDestinationRejection(), nil)
		time.Sleep(settings.TerminalRouteRejectionTimeout - time.Millisecond)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("terminal refusal expired before its timeout")
		}
		time.Sleep(time.Millisecond)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("terminal refusal was replayed at its timeout")
		}
	})
}

// An origin edge or a subscription acknowledgement cannot open a route whose
// destination row is inactive, so neither removes its refusal.
func TestContractTerminalRouteRejectionSurvivesOriginEdges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch, connection := testWatchRouteBucket(t, owner, opened, sourceId, destinationId)
		defer watch.Close()
		owner.RecordRouteRejection(sourceId, destinationId, false, testTerminalDestinationRejection(), nil)
		key := contractOriginPairHash(destinationId, sourceId)
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
		synctest.Wait()
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		notifyCommittedContractOrigin(WithContractOriginNotifications(t.Context(), owner), destinationId, sourceId)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("an origin edge removed the refusal of an inactive destination")
		}
	})
}

// Only a refusal that found the destination row present and inactive is
// terminal. A missing row, a write-boundary refusal (whose labels carry the
// earlier read's active lifecycle) and a missing origin stay in the ring:
// they expire at its timeout and an acknowledgement clears them.
func TestContractTerminalRouteRejectionOnlyForInactiveRows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		writeBoundary := testTerminalDestinationRejection()
		writeBoundary.Relationship.DestinationLifecycle = NetworkClientLifecycleActiveDerived
		timed := []ContractRouteRejection{
			testDestinationRejection(),
			writeBoundary,
			{Cause: ContractRouteMissingOrigin, ResolutionPath: "stream_fallback"},
		}
		terminal := []ContractRouteRejection{
			testTerminalDestinationRejection(),
			func() ContractRouteRejection {
				rejection := testTerminalDestinationRejection()
				rejection.Relationship.DestinationLifecycle = NetworkClientLifecycleInactiveTop
				rejection.Relationship.DestinationOwner = NetworkClientSourceOwnerOther
				return rejection
			}(),
		}
		destinationId := server.NewId()
		timedSourceIds := map[server.Id]ContractRouteRejection{}
		for _, rejection := range timed {
			sourceId := server.NewId()
			timedSourceIds[sourceId] = rejection
			if !owner.RecordRouteRejection(sourceId, destinationId, false, rejection, nil) {
				t.Fatalf("refusal %+v was not stored", rejection)
			}
		}
		terminalSourceIds := map[server.Id]ContractRouteRejection{}
		for _, rejection := range terminal {
			sourceId := server.NewId()
			terminalSourceIds[sourceId] = rejection
			if !owner.RecordRouteRejection(sourceId, destinationId, false, rejection, nil) {
				t.Fatalf("refusal %+v was not stored", rejection)
			}
		}
		time.Sleep(settings.RouteRejectionTimeout)
		for sourceId, rejection := range timedSourceIds {
			if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
				t.Fatalf("refusal %+v outlived the ring timeout", rejection)
			}
		}
		for sourceId, rejection := range terminalSourceIds {
			if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
				t.Fatalf("terminal refusal %+v expired at the ring timeout", rejection)
			}
		}
	})
}

// A route an old client keeps retrying survives the generation turnover; one
// it stopped retrying ages out with its generation.
func TestContractTerminalRouteRejectionKeepsRetriedRoutesAcrossGenerations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		settings.TerminalRouteRejectionCount = 2
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		destinationId := server.NewId()
		retriedId, abandonedId, thirdId, fourthId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		record := func(sourceId server.Id) {
			if !owner.RecordRouteRejection(sourceId, destinationId, false, testTerminalDestinationRejection(), nil) {
				t.Fatal("terminal refusal was not stored")
			}
		}
		replayed := func(sourceId server.Id) bool {
			_, ok := owner.RouteRejection(sourceId, destinationId, false)
			return ok
		}
		record(retriedId)
		record(abandonedId)
		// a full generation turns over: both move to the previous generation
		record(thirdId)
		if !replayed(retriedId) {
			t.Fatal("a refusal in the previous generation was not replayed")
		}
		// the retried route moved back, so this turnover drops only the abandoned one
		record(fourthId)
		if replayed(abandonedId) {
			t.Fatal("a route no longer retried survived two generations")
		}
		for _, sourceId := range []server.Id{retriedId, thirdId, fourthId} {
			if !replayed(sourceId) {
				t.Fatal("a retried or recent refusal was dropped")
			}
		}
		owner.terminalRouteRejections.stateLock.Lock()
		held := len(owner.terminalRouteRejections.currentKeyRejections) + len(owner.terminalRouteRejections.previousKeyRejections)
		owner.terminalRouteRejections.stateLock.Unlock()
		if 2*settings.TerminalRouteRejectionCount < held {
			t.Fatalf("generation bound of %d holds %d refusals", settings.TerminalRouteRejectionCount, held)
		}
	})
}

func TestContractTerminalRouteRejectionZeroBoundDisables(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		for _, settings := range []ContractOriginNotificationSettings{
			func() ContractOriginNotificationSettings {
				settings := DefaultContractOriginNotificationSettings()
				settings.TerminalRouteRejectionCount = 0
				return settings
			}(),
			func() ContractOriginNotificationSettings {
				settings := DefaultContractOriginNotificationSettings()
				settings.TerminalRouteRejectionTimeout = 0
				return settings
			}(),
		} {
			owner := testContractOriginOwner(t.Context(), settings, opened)
			sourceId, destinationId := server.NewId(), server.NewId()
			if owner.RecordRouteRejection(sourceId, destinationId, false, testTerminalDestinationRejection(), nil) {
				t.Fatalf("a disabled terminal registry (%d, %s) stored a refusal", settings.TerminalRouteRejectionCount, settings.TerminalRouteRejectionTimeout)
			}
			if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
				t.Fatal("a disabled terminal registry replayed a refusal")
			}
			owner.Close()
		}
	})
}

func TestContractTerminalRouteRejectionClosedOwnerKeepsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		sourceId, destinationId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(sourceId, destinationId, false, testTerminalDestinationRejection(), nil)
		owner.Close()
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("a closed owner replayed a terminal refusal")
		}
		if owner.RecordRouteRejection(sourceId, destinationId, false, testTerminalDestinationRejection(), nil) {
			t.Fatal("a closed owner stored a terminal refusal")
		}
	})
}

// Old sdks retry a route without an origin every 5s, and a load balancer
// spreads the retries so one api process sees the route only every 25-40s.
// The ring keeps the refusal for 60s.
func TestContractRouteRejectionMissingOriginLastsSixtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		if settings.RouteRejectionTimeout != 60*time.Second {
			t.Fatalf("the ring keeps a refusal for %s, want 60s", settings.RouteRejectionTimeout)
		}
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		missing := ContractRouteRejection{Cause: ContractRouteMissingOrigin, ResolutionPath: "stream_fallback"}
		if !owner.RecordRouteRejection(sourceId, destinationId, false, missing, nil) {
			t.Fatal("refusal was not stored")
		}
		time.Sleep(60*time.Second - time.Millisecond)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("a missing-origin refusal expired before 60s")
		}
		time.Sleep(time.Millisecond)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("a missing-origin refusal was replayed at 60s")
		}
	})
}

// A refusal kept past the retry another process brings is still removed at
// once by an origin edge for its pair.
func TestContractRouteRejectionOriginEdgeClearsWithinTheTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch, connection := testWatchRouteBucket(t, owner, opened, sourceId, destinationId)
		defer watch.Close()
		missing := ContractRouteRejection{Cause: ContractRouteMissingOrigin, ResolutionPath: "stream_fallback"}
		owner.RecordRouteRejection(sourceId, destinationId, false, missing, nil)
		time.Sleep(40 * time.Second)
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("a missing-origin refusal expired before the retry another process brings")
		}
		key := contractOriginPairHash(destinationId, sourceId)
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
		synctest.Wait()
		if _, ok := owner.RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("an origin edge left the route refused")
		}
	})
}
