package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Amendment 24: a postponement moves a due certificate's renewal time and
// nothing else -- it counts no failure -- and the renewal read leaves the
// certificate out until then. One that was renewed, withdrawn or expired
// meanwhile is not postponed, and keeps its renewal time.
func TestPostponeNetworkExtenderMeasurementCertRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		renewTime := now.Add(time.Hour)

		// a certificate of a new extender, issued `age` ago for ninety days
		// and due sixty days after its issue
		addCert := func(index int, age time.Duration) *NetworkExtenderMeasurementCert {
			extenderId := testMeasurementActivate(t, ctx, index).Extender.ExtenderId
			cert := testMeasurementCert(extenderId, []byte(fmt.Sprintf("key-%d", index)), now.Add(-age))
			connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, cert), true)
			return cert
		}
		storedCert := func(cert *NetworkExtenderMeasurementCert) *NetworkExtenderMeasurementCert {
			for _, stored := range Testing_GetNetworkExtenderMeasurementCerts(ctx, cert.ExtenderId) {
				if stored.CertId == cert.CertId {
					return stored
				}
			}
			t.Fatalf("the certificate %s is gone", cert.CertId)
			return nil
		}

		// due, after a renewal that failed
		postponed := addCert(1, 61*24*time.Hour)
		DeferNetworkExtenderMeasurementCertRenewal(ctx, postponed.CertId, now.Add(-time.Minute))
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsForRenewal(ctx, now, 10)), 1)
		connect.AssertEqual(t, PostponeNetworkExtenderMeasurementCertRenewal(ctx, postponed.CertId, renewTime, now), true)
		stored := storedCert(postponed)
		connect.AssertEqual(t, stored.RenewTime.UTC(), renewTime)
		connect.AssertEqual(t, stored.RenewFailureCount, 1)
		connect.AssertEqual(t, stored.RenewedTime == nil, true)
		connect.AssertEqual(t, stored.WithdrawTime == nil, true)
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsForRenewal(ctx, now, 10)), 0)
		due := GetNetworkExtenderMeasurementCertsForRenewal(ctx, renewTime, 10)
		connect.AssertEqual(t, len(due), 1)
		connect.AssertEqual(t, due[0].Cert.CertId, postponed.CertId)
		connect.AssertEqual(t, due[0].Cert.RenewFailureCount, 1)

		// replaced by a renewal meanwhile
		renewed := addCert(2, 61*24*time.Hour)
		renewal := testMeasurementCert(renewed.ExtenderId, []byte("key-2"), now)
		renewal.CapCounted = false
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, renewal), true)
		// withdrawn meanwhile
		withdrawn := addCert(3, 61*24*time.Hour)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCert(ctx, withdrawn.CertId, "the extender moved to the gated tier", now), true)
		// expired
		expired := addCert(4, 100*24*time.Hour)
		for _, cert := range []*NetworkExtenderMeasurementCert{renewed, withdrawn, expired} {
			connect.AssertEqual(t, PostponeNetworkExtenderMeasurementCertRenewal(ctx, cert.CertId, renewTime, now), false)
			connect.AssertEqual(t, storedCert(cert).RenewTime.UTC(), cert.RenewTime.UTC())
		}
	})
}
