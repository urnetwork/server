package model

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The family-pinned names in the certificate records (lag.dev/DRIFTTEST.md
// K6): a certificate for several names is published once all of them are,
// and stops being published when any one of them goes; a certificate for the
// measurement name alone answers a request, under the same key, that adds the
// family-pinned names, so the lag.dev sample serves the extender while that
// request waits; and only a certificate for exactly the request's names is the
// request's own.

// The extender's three names, as the list these tables keep: its measurement
// name, its IPv4 name and its IPv6 name.
func testMeasurementFamilyNames(extenderId server.Id) []string {
	return connect.ExtenderMeasurementNames(connect.Id(extenderId), testMeasurementZone)
}

func TestNetworkExtenderMeasurementPublishesAllOfACertificatesNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		familyId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		plainId := testMeasurementActivate(t, ctx, 2).Extender.ExtenderId
		familyNames := testMeasurementFamilyNames(familyId)

		SetNetworkExtenderMeasurementRequest(ctx, familyId, []byte("csr-a"), []byte("key-a"), familyNames, now)
		familyCert := testMeasurementCert(familyId, []byte("key-a"), now)
		familyCert.Names = familyNames
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, familyCert), true)
		SetNetworkExtenderMeasurementRequest(ctx, plainId, []byte("csr-b"), []byte("key-b"), testMeasurementNames(plainId), now)
		plainCert := testMeasurementCert(plainId, []byte("key-b"), now)
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, plainCert), true)

		published := func(extenderId server.Id) bool {
			t.Helper()
			return Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].PublishTime != nil
		}

		// the measurement name alone, or two of the three, publish nothing of
		// a certificate for all three
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, familyNames[:1], now), 0)
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, familyNames[:2], now), 0)
		connect.AssertEqual(t, published(familyId), false)
		// all three, in any order and case, beside another extender's name
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(
			ctx,
			[]string{strings.ToUpper(familyNames[2]), familyNames[0], familyNames[1], plainCert.Names[0]},
			now,
		), 2)
		connect.AssertEqual(t, published(familyId), true)
		connect.AssertEqual(t, published(plainId), true)

		// any one of its names gone unpublishes it, and nothing else
		connect.AssertEqual(t, UnpublishNetworkExtenderMeasurementNames(ctx, familyNames[2:]), 1)
		connect.AssertEqual(t, published(familyId), false)
		connect.AssertEqual(t, published(plainId), true)
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, familyNames, now), 1)

		// kept only while all of its names are kept
		connect.AssertEqual(t, UnpublishNetworkExtenderMeasurementNamesExcept(ctx, append(slices.Clone(familyNames[:2]), plainCert.Names...)), 1)
		connect.AssertEqual(t, published(familyId), false)
		connect.AssertEqual(t, published(plainId), true)
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, familyNames, now), 1)
		connect.AssertEqual(t, UnpublishNetworkExtenderMeasurementNamesExcept(ctx, familyNames), 1)
		connect.AssertEqual(t, published(familyId), true)
		connect.AssertEqual(t, published(plainId), false)
	})
}

// The lag.dev sample's read of a request (A4) while it adds the family-pinned
// names: the certificate for the measurement name alone, under the same key,
// published and delivered, serves it, though it is not the request's own; one
// under another key does not. Once the certificate for all three names is
// published and delivered, it is the request's own. A request for the
// measurement name alone again is answered by that name's certificate, never
// by the one for all three.
func TestLagExtenderMeasurementServesWhileTheFamilyNamesWait(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		extenderId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		plainNames := testMeasurementNames(extenderId)
		familyNames := testMeasurementFamilyNames(extenderId)
		read := func() *LagExtenderMeasurement {
			t.Helper()
			measurement := GetLagExtenderMeasurements(ctx, []server.Id{extenderId}, now.Add(time.Hour))[extenderId]
			if measurement == nil {
				t.Fatal("the request was not read")
			}
			return measurement
		}

		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), plainNames, now)
		plainCert := testMeasurementCert(extenderId, []byte("key-a"), now)
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, plainCert), true)
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, plainNames, now), 1)
		connect.AssertEqual(t, MarkNetworkExtenderMeasurementCertDelivered(ctx, plainCert.CertId, now), true)
		measurement := read()
		connect.AssertEqual(t, measurement.Serving(), true)
		connect.AssertEqual(t, measurement.NamesDelivered, true)

		// the request adds the family-pinned names: pending, and served by the
		// measurement name's certificate, which is not its own
		connect.AssertEqual(
			t,
			SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a3"), []byte("key-a"), familyNames, now),
			NetworkExtenderMeasurementStatePending,
		)
		measurement = read()
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, measurement.Delivered, true)
		connect.AssertEqual(t, measurement.NamesDelivered, false)
		connect.AssertEqual(t, measurement.Serving(), true)

		// another key's request for the three names: nothing held is for it
		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-b3"), []byte("key-b"), familyNames, now)
		connect.AssertEqual(t, read().Serving(), false)

		// the first key's request again, and the certificate for all three:
		// its own once published and delivered
		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a3"), []byte("key-a"), familyNames, now)
		connect.AssertEqual(t, read().Serving(), true)
		familyCert := testMeasurementCert(extenderId, []byte("key-a"), now.Add(time.Minute))
		familyCert.Names = familyNames
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, familyCert), true)
		measurement = read()
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, measurement.Serving(), true)
		connect.AssertEqual(t, measurement.NamesDelivered, false)
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, familyNames, now), 1)
		connect.AssertEqual(t, MarkNetworkExtenderMeasurementCertDelivered(ctx, familyCert.CertId, now), true)
		measurement = read()
		connect.AssertEqual(t, measurement.Serving(), true)
		connect.AssertEqual(t, measurement.NamesDelivered, true)

		// the measurement name alone again: answered at once by its
		// certificate, still live, never by the one for all three
		connect.AssertEqual(
			t,
			SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), plainNames, now),
			NetworkExtenderMeasurementStateIssued,
		)
		measurement = read()
		connect.AssertEqual(t, measurement.Serving(), true)
		if !slices.Equal(measurement.Names, plainNames) {
			t.Fatalf("names = %v", measurement.Names)
		}
	})
}
