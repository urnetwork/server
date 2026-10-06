package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestSetGeolocationSourcePinStoresAndReplacesPerHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		observedAt := server.NowUtc().Truncate(time.Millisecond)

		// first observation of a host: nothing replaced
		previous := SetGeolocationSourcePin(ctx, &GeolocationSourcePin{
			Host:             "ipinfo.io",
			LeafSpki:         "leaf-ipinfo-1",
			IntermediateSpki: "int-ipinfo-1",
			ObservedAt:       observedAt,
		})
		if previous != nil {
			t.Fatalf("expected no previous pin on first observation, got %+v", previous)
		}

		// a second host is stored independently
		SetGeolocationSourcePin(ctx, &GeolocationSourcePin{
			Host:             "api.i.pn",
			LeafSpki:         "leaf-ipn-1",
			IntermediateSpki: "int-ipn-1",
			ObservedAt:       observedAt,
		})

		stored := GetGeolocationSourcePin(ctx, "ipinfo.io")
		if stored == nil {
			t.Fatal("expected a stored pin for ipinfo.io, got nil")
		}
		connect.AssertEqual(t, stored.Host, "ipinfo.io")
		connect.AssertEqual(t, stored.LeafSpki, "leaf-ipinfo-1")
		connect.AssertEqual(t, stored.IntermediateSpki, "int-ipinfo-1")
		connect.AssertEqual(t, stored.ObservedAt.UTC().Equal(observedAt.UTC()), true)

		// re-observing one host REPLACES that host's row and returns what it
		// replaced -- the return value is what makes a rotation loggable with
		// both old and new
		rotatedAt := observedAt.Add(6 * time.Hour)
		previous = SetGeolocationSourcePin(ctx, &GeolocationSourcePin{
			Host:             "ipinfo.io",
			LeafSpki:         "leaf-ipinfo-2",
			IntermediateSpki: "int-ipinfo-2",
			ObservedAt:       rotatedAt,
		})
		if previous == nil {
			t.Fatal("expected the replaced pin to be returned, got nil")
		}
		connect.AssertEqual(t, previous.LeafSpki, "leaf-ipinfo-1")
		connect.AssertEqual(t, previous.IntermediateSpki, "int-ipinfo-1")

		pins := GetGeolocationSourcePins(ctx)
		connect.AssertEqual(t, len(pins), 2)
		connect.AssertEqual(t, pins["ipinfo.io"].LeafSpki, "leaf-ipinfo-2")
		connect.AssertEqual(t, pins["ipinfo.io"].IntermediateSpki, "int-ipinfo-2")
		// replacing one host must not touch another: a per-host write that
		// disturbed its neighbours would be the same class of bug as a failing
		// host blanking the whole set
		connect.AssertEqual(t, pins["api.i.pn"].LeafSpki, "leaf-ipn-1")
		connect.AssertEqual(t, pins["api.i.pn"].IntermediateSpki, "int-ipn-1")

		// a host that has never been observed reads as absent, not as an empty
		// pin -- the consumer's correct response to absent is to refuse to
		// probe, and a zero-valued row would take that decision away from it
		if GetGeolocationSourcePin(ctx, "free.freeipapi.com") != nil {
			t.Fatal("expected nil for a host that has never been observed")
		}
		if _, ok := pins["free.freeipapi.com"]; ok {
			t.Fatal("expected an unobserved host to be absent from the pin map")
		}
	})
}
