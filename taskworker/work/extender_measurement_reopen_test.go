package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Amendment 24 end to end. An extender deactivated past the away grace is
// withdrawn and revoked, and nothing is asked again for it while it is not
// active. It comes back with an activation that carries no request, and the
// next issuance pass asks again with the request it holds: it is issued a
// certificate for that request's key, outside the spent weekly cap since its
// name has an unexpired one, with no new request stored, and the dns pass
// publishes its name again. An extender whose owner opted out is active
// throughout and is never asked again.
func TestExtenderMeasurementAsksAgainForAnExtenderBackAfterTheGrace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 2})

		returning := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		key, request := requestTestMeasurementCert(t, ctx, returning, server.NowUtc())
		optedOut := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		requestTestMeasurementCert(t, ctx, optedOut, server.NowUtc())
		// the week's two new certificates
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Names, 2)

		// one extender's probes failed past the grace: it left. The other's
		// owner opts out, as an activation with the flag does
		deactivateTestMeasurementExtender(t, ctx, returning, server.NowUtc().Add(-controller.ExtenderMeasurementAwayGrace-time.Minute))
		connect.AssertEqual(t, model.OptOutNetworkExtenderMeasurement(ctx, testMeasurementExtenderPublicKey(2), controller.ExtenderMeasurementOptOutReason, server.NowUtc()), 1)
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Withdrawn, 1)
		connect.AssertEqual(t, dnsResult.Names, 0)
		withdrawn := model.Testing_GetNetworkExtenderMeasurement(ctx, returning)
		connect.AssertEqual(t, withdrawn.State, model.NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, withdrawn.Reason, controller.ExtenderMeasurementInactiveReason)

		// both certificates are revoked, and nothing is asked again for the
		// extender that is not active
		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Revoked, 2)
		connect.AssertEqual(t, result.Issued, 0)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, returning).State, model.NetworkExtenderMeasurementStateWithdrawn)

		// it comes back, with an activation that carries no request
		reactivateTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, returning).State, model.NetworkExtenderMeasurementStateWithdrawn)

		// the next issuance pass asks again with the request it holds
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Waiting, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 3)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, returning)
		connect.AssertEqual(t, len(certs), 2)
		connect.AssertEqual(t, certs[1].WithdrawTime == nil, true)
		connect.AssertEqual(t, certs[1].CapCounted, false)
		connect.AssertEqual(t, bytes.Equal(certs[1].Csr, request.Csr), true)
		leaf := verifyTestMeasurementChain(t, env.ca, certs[1].ChainPem, testMeasurementName(returning))
		connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), true)
		// no new request was stored: the one it held is issued
		measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, returning)
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, request.Csr), true)
		connect.AssertEqual(t, measurement.RequestTime.UTC(), withdrawn.RequestTime.UTC())
		// the one whose owner opted out stays out
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, optedOut).State, model.NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, optedOut)), 1)

		// its name is published again
		dnsResult = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Withdrawn, 0)
		connect.AssertEqual(t, dnsResult.Names, 1)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(returning) + " A": {"203.0.113.1"},
		})
		assertTestMeasurementForeignRecordSets(t, env, testMeasurementForeignApexRecordSets())
		assertTestMeasurementBatches(t, env)
	})
}
