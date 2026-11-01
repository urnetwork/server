package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The extender activates again, from `ip` with its tcp carrier on `tcpPort`,
// under a new client, which it returns: the same identity key, so the same
// extender and the same id.
func reactivateTestReopenExtender(t testing.TB, ctx context.Context, index int, ip string, tcpPort int) server.Id {
	t.Helper()
	clientId := server.NewId()
	addr := netip.MustParseAddr(ip)
	ipVersion := 6
	if addr.Is4() {
		ipVersion = 4
	}
	sign := func(*model.NetworkExtender, []*model.NetworkExtenderAddress, time.Time) ([]byte, error) {
		return []byte("record"), nil
	}
	if model.ActivateNetworkExtender(ctx, &model.NetworkExtenderActivation{
		NetworkId:   server.NewId(),
		ClientId:    clientId,
		PublicKey:   testMeasurementExtenderPublicKey(index),
		TcpPort:     tcpPort,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "de",
		IpVersion:   ipVersion,
		Ip:          addr,
		Carriers:    []string{connect.ExtenderCarrierTcp},
	}, sign) == nil {
		t.Fatalf("the extender was not activated")
	}
	return clientId
}

// Amendment 24 end to end. Two extenders deactivated past the away grace are
// withdrawn and revoked, and nothing is asked again for either while it is not
// active. Both come back with activations that carry no request. The one that
// may be measured is asked again by the next issuance pass with the request it
// holds: it is issued a certificate for that request's key, outside the spent
// weekly cap since its name has an unexpired one, with no new request stored,
// and the dns pass publishes its name again. The one that came back with its
// tcp carrier on 8443 may not be measured, so it is not asked again: its
// request stays withdrawn, and A8 answers it withdrawn, so it drops the
// revoked certificate it holds (C9). An extender whose owner opted out is
// active throughout and is never asked again.
func TestExtenderMeasurementAsksAgainForAnExtenderBackAfterTheGrace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 3})

		returning := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		key, request := requestTestMeasurementCert(t, ctx, returning, server.NowUtc())
		optedOut := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		requestTestMeasurementCert(t, ctx, optedOut, server.NowUtc())
		moved := createTestMeasurementExtender(t, ctx, 3, "203.0.113.3")
		requestTestMeasurementCert(t, ctx, moved, server.NowUtc())
		// the week's three new certificates
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 3)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Names, 3)

		// two extenders' probes failed past the grace: they left. The third's
		// owner opts out, as an activation with the flag does
		for _, extenderId := range []server.Id{returning, moved} {
			deactivateTestMeasurementExtender(t, ctx, extenderId, server.NowUtc().Add(-controller.ExtenderMeasurementAwayGrace-time.Minute))
		}
		connect.AssertEqual(t, model.OptOutNetworkExtenderMeasurement(ctx, testMeasurementExtenderPublicKey(2), controller.ExtenderMeasurementOptOutReason, server.NowUtc()), 1)
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Withdrawn, 2)
		connect.AssertEqual(t, dnsResult.Names, 0)
		withdrawn := model.Testing_GetNetworkExtenderMeasurement(ctx, returning)
		connect.AssertEqual(t, withdrawn.State, model.NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, withdrawn.Reason, controller.ExtenderMeasurementInactiveReason)

		// all three certificates are revoked, and nothing is asked again for
		// the extenders that are not active
		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Revoked, 3)
		connect.AssertEqual(t, result.Issued, 0)
		for _, extenderId := range []server.Id{returning, moved} {
			connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateWithdrawn)
		}

		// both come back, with activations that carry no request, one with its
		// tcp carrier where no browser connects (decision 4)
		reactivateTestReopenExtender(t, ctx, 1, "203.0.113.1", controller.ExtenderMeasurementPort)
		movedClientId := reactivateTestReopenExtender(t, ctx, 3, "203.0.113.3", 8443)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, returning).State, model.NetworkExtenderMeasurementStateWithdrawn)

		// the next issuance pass asks again, with the request it holds, only
		// for the one that may be measured
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Waiting, 0)
		connect.AssertEqual(t, result.Refused, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 4)
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

		// the one on 8443 stays withdrawn, through this pass and the ones
		// after, and A8 answers it withdrawn, so it drops the revoked
		// certificate
		for range 2 {
			result = runTestMeasurementIssue(t, ctx)
			connect.AssertEqual(t, result.Issued+result.Refused, 0)
		}
		movedMeasurement := model.Testing_GetNetworkExtenderMeasurement(ctx, moved)
		connect.AssertEqual(t, movedMeasurement.State, model.NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, movedMeasurement.Reason, controller.ExtenderMeasurementInactiveReason)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, moved)), 1)
		answeredId, answered, answeredCert := model.GetNetworkExtenderMeasurementForClient(ctx, movedClientId, testMeasurementExtenderPublicKey(3), server.NowUtc())
		if answeredId == nil || *answeredId != moved || answered == nil {
			t.Fatalf("A8 answers %v for the extender on 8443", answeredId)
		}
		connect.AssertEqual(t, answered.State, model.NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, answeredCert == nil, true)

		// the one asked for is published again
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
