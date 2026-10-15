package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Tests of the extender activation history.

// The activation history keeps the genesis accuracy radius of the lookup that
// placed the activating address (connect/GEOMAP.md §5.1), and a lookup
// without one stores NULL rather than a zero that would read as an address
// placed exactly.
func TestExtenderActivationStoresTheAccuracyRadius(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")

		publicKey := []byte("extender-public-key-accuracy-001")
		located := testExtenderActivation(publicKey, 4, "192.0.2.60")
		accuracyKm := float32(25)
		located.AccuracyKm = &accuracyKm
		activated := ActivateNetworkExtender(ctx, located.WithLocation(sydney), sign)
		if activated == nil {
			t.Fatal("the activation was not stored")
		}

		// a second activation whose lookup gave no radius
		unplaced := testExtenderActivation(publicKey, 6, "2001:db8::60")
		ActivateNetworkExtender(ctx, unplaced, sign)

		activations := GetNetworkExtenderActivations(ctx, activated.Extender.ExtenderId, time.Time{})
		connect.AssertEqual(t, len(activations), 2)
		if activations[0].AccuracyKm == nil || *activations[0].AccuracyKm != accuracyKm {
			t.Fatalf("the located activation stored radius %v, want %f", activations[0].AccuracyKm, accuracyKm)
		}
		connect.AssertEqual(t, activations[0].CityLocationId != nil, true)
		if activations[1].AccuracyKm != nil {
			t.Fatalf("the unplaced activation stored radius %f", *activations[1].AccuracyKm)
		}
	})
}
