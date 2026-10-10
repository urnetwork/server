package controller

// A non-companion request whose destination does not advertise the
// relationship mode falls back to a Stream companion. With no reverse origin,
// the companion origin wait used to poll it for its whole window, each poll a
// read-write origin transaction. These tests drive the real CreateContract
// resolution against the test database and count authoritative lookups.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Sums authoritative origin lookups across every wake source.
func companionOriginLookupTotal() float64 {
	total := 0.0
	for _, source := range []string{"initial", "event", "fallback", "deadline"} {
		total += testutil.ToFloat64(companionOriginLookupCounter.WithLabelValues(source))
	}
	return total
}

// Creates a source and a destination in different networks, so the
// relationship is Public, and registers the destination like a client that
// is not providing: it advertises only Stream for its return traffic.
func newCompanionFallbackPair(t testing.TB, ctx context.Context) (sourceId server.Id, destinationId server.Id) {
	t.Helper()
	sourceId, destinationId = server.NewId(), server.NewId()
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), sourceId, "synthetic-source", "synthetic")
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), destinationId, "synthetic-destination", "synthetic")
	model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{
		model.ProvideModeStream: []byte("synthetic-provide-stream-key-000"),
	})
	return
}

// Requests one contract and returns its single result.
func createCompanionFallbackContract(t testing.TB, ctx context.Context, sourceId server.Id, destinationId server.Id, companion bool) *protocol.CreateContractResult {
	t.Helper()
	frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: uint64(1024 * 1024),
		Companion:         companion,
	}, connect.DefaultContractManagerSettings())
	if err != nil || len(frames) != 1 {
		t.Fatalf("contract request failed: frames=%d err=%v", len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	result, ok := message.(*protocol.CreateContractResult)
	if err != nil || !ok {
		t.Fatalf("contract result was not decoded: %v", err)
	}
	return result
}

// A non-companion request to a destination that is not providing has no
// reverse origin to ride. It must cost one origin lookup and still report the
// route as unreliable, instead of polling the origin for the whole wait.
func TestCreateContractStreamFallbackWithoutOriginLooksUpOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sourceId, destinationId := newCompanionFallbackPair(t, ctx)
		before := companionOriginLookupTotal()
		result := createCompanionFallbackContract(t, ctx, sourceId, destinationId, false)
		lookups := companionOriginLookupTotal() - before
		if result.Contract != nil || result.Error == nil || *result.Error != protocol.ContractError_Reliability {
			t.Fatalf("fallback without an origin did not report an unreliable route: %+v", result)
		}
		if lookups != 1 {
			t.Fatalf("fallback without an origin used %v origin lookups, want 1", lookups)
		}
	})
}

// A real companion request keeps its bounded wait for an origin that can
// still be racing it at session setup.
func TestCreateContractCompanionWithoutOriginStillWaits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sourceId, destinationId := newCompanionFallbackPair(t, ctx)
		before := companionOriginLookupTotal()
		result := createCompanionFallbackContract(t, ctx, sourceId, destinationId, true)
		lookups := companionOriginLookupTotal() - before
		if result.Contract != nil || result.Error == nil || *result.Error != protocol.ContractError_Reliability {
			t.Fatalf("companion without an origin did not report an unreliable route: %+v", result)
		}
		if lookups < 2 {
			t.Fatalf("companion request stopped waiting for its origin: lookups=%v", lookups)
		}
	})
}
