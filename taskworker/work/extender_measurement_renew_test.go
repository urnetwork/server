package work

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Amendment 24: the issuance chain puts off the renewal of a due certificate
// it may not renew -- its extender may not be measured now, or it is for
// another zone's name -- rather than leaving it due, where it would come back
// at the head of every renewal read until it expired.

// The zone the measurement names were under before a change of zone.
const testMeasurementRenewOldZone = "extender.old.example"

// One pass of the issuance chain at `now`, as the task runs it, so a test can
// place a pass an hour on without waiting for it.
func runTestMeasurementRenewPassAt(t testing.TB, ctx context.Context, now time.Time) *ExtenderMeasurementIssueResult {
	t.Helper()
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result := &ExtenderMeasurementIssueResult{}
	issueExtenderMeasurementCerts(clientSession, config, now, result)
	return result
}

// A live certificate for `name` stored as an issuance stores one, due at
// `renewTime`, and with no order at the CA: a renewal that is put off reads
// neither its request nor its chain.
func addTestMeasurementRenewCert(
	t testing.TB,
	ctx context.Context,
	extenderId server.Id,
	name string,
	renewTime time.Time,
) *model.NetworkExtenderMeasurementCert {
	t.Helper()
	issueTime := renewTime.Add(-60 * 24 * time.Hour)
	cert := &model.NetworkExtenderMeasurementCert{
		CertId:     server.NewId(),
		ExtenderId: extenderId,
		Names:      []string{name},
		Csr:        []byte(fmt.Sprintf("csr-%s", extenderId)),
		PublicKey:  []byte(fmt.Sprintf("key-%s", extenderId)),
		ChainPem:   "chain",
		SerialHex:  "1",
		NotBefore:  issueTime,
		NotAfter:   issueTime.Add(90 * 24 * time.Hour),
		IssueTime:  issueTime,
		CapCounted: true,
		RenewTime:  renewTime,
	}
	if !model.AddNetworkExtenderMeasurementCert(ctx, cert) {
		t.Fatalf("the certificate was not stored live")
	}
	return cert
}

// The extender's one certificate as stored is still live and not renewed, is
// due at `renewTime`, and has counted `failureCount` failed renewals.
func assertTestMeasurementRenewCert(
	t testing.TB,
	ctx context.Context,
	cert *model.NetworkExtenderMeasurementCert,
	renewTime time.Time,
	failureCount int,
) {
	t.Helper()
	certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, cert.ExtenderId)
	if len(certs) != 1 || certs[0].CertId != cert.CertId {
		t.Fatalf("%s holds %d certificates", cert.ExtenderId, len(certs))
	}
	stored := certs[0]
	if stored.WithdrawTime != nil || stored.RenewedTime != nil {
		t.Fatalf("%s was withdrawn or renewed", cert.CertId)
	}
	if !stored.RenewTime.Equal(renewTime) || stored.RenewFailureCount != failureCount {
		t.Fatalf(
			"%s is due at %s after %d failures, not at %s after %d",
			cert.CertId,
			stored.RenewTime.UTC(),
			stored.RenewFailureCount,
			renewTime.UTC(),
			failureCount,
		)
	}
}

// The extender activates again from `ip` serving tcp on `tcpPort`: the same
// identity key, so the same extender and the same id, with its port and its
// address of that family replaced.
func reactivateTestMeasurementRenewExtender(t testing.TB, ctx context.Context, index int, tcpPort int, ip string) {
	t.Helper()
	addr := netip.MustParseAddr(ip)
	ipVersion := 6
	if addr.Is4() {
		ipVersion = 4
	}
	sign := func(*model.NetworkExtender, []*model.NetworkExtenderAddress, time.Time) ([]byte, error) {
		return []byte("record"), nil
	}
	if model.ActivateNetworkExtender(ctx, &model.NetworkExtenderActivation{
		NetworkId:   testMeasurementExtenderNetworkId(index),
		ClientId:    server.NewId(),
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
}

// More due certificates than a pass reads, all due before a serving
// extender's, of active extenders that may not be measured -- tcp moved off
// 443, no public inbound address -- or for the zone before a change of zone.
// Nothing withdraws them (C9). The first pass puts each one it reads off by
// ExtenderMeasurementRenewalPostponeTimeout, counting no failure and ordering
// nothing, so the next pass reads the serving extender's certificate and
// renews it.
func TestExtenderMeasurementRenewalIsNotHeldByUnmeasurableExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		serving := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, serving, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)

		now := server.NowUtc().Truncate(time.Second)
		held := []*model.NetworkExtenderMeasurementCert{}
		failureCounts := map[server.Id]int{}
		for i := range ExtenderMeasurementRenewalLimit + 1 {
			index := 2 + i
			ip := fmt.Sprintf("198.51.100.%d", 1+i)
			zone := testMeasurementZone
			var extenderId server.Id
			switch i % 3 {
			case 0:
				// tcp where no browser connects (decision 4)
				extenderId = createTestMeasurementExtenderWith(t, ctx, index, 8443, connect.ExtenderCarrierTcp, ip)
			case 1:
				// no public inbound address (decision 14)
				extenderId = createTestMeasurementExtenderWith(t, ctx, index, controller.ExtenderMeasurementPort, connect.ExtenderCarrierWebRtc, ip)
			default:
				// may be measured, with a certificate for its name under the
				// zone before the change
				extenderId = createTestMeasurementExtender(t, ctx, index, ip)
				zone = testMeasurementRenewOldZone
			}
			renewTime := now.Add(-2 * time.Hour).Add(time.Duration(i) * time.Second)
			cert := addTestMeasurementRenewCert(t, ctx, extenderId, controller.ExtenderMeasurementName(extenderId, zone), renewTime)
			if i%5 == 0 {
				// a renewal that failed before
				model.DeferNetworkExtenderMeasurementCertRenewal(ctx, cert.CertId, renewTime)
				failureCounts[cert.CertId] = 1
			}
			held = append(held, cert)
		}
		servingCert := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, serving)[0]
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, servingCert.CertId, now.Add(-time.Hour))

		// their extenders are active, in the open tier and no canary: none is
		// withdrawn, and only the serving extender's name is in the zone
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Withdrawn, 0)
		connect.AssertEqual(t, dnsResult.Names, 1)
		orders := env.ca.orderTotal()

		// the first pass reads only certificates it may not renew, and puts
		// each off; the last of them is past what a pass reads
		result := runTestMeasurementRenewPassAt(t, ctx, now)
		connect.AssertEqual(t, result.Renewed, 0)
		connect.AssertEqual(t, result.Failed, 0)
		connect.AssertEqual(t, env.ca.orderTotal(), orders)
		last := held[ExtenderMeasurementRenewalLimit]
		assertTestMeasurementRenewCert(t, ctx, last, last.RenewTime, failureCounts[last.CertId])
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, serving)), 1)

		// the next pass, a minute on, reads the last of them and the serving
		// extender's certificate, puts off the one and renews the other
		next := now.Add(ExtenderMeasurementIssueTimeout)
		result = runTestMeasurementRenewPassAt(t, ctx, next)
		connect.AssertEqual(t, result.Renewed, 1)
		connect.AssertEqual(t, result.Failed, 0)
		connect.AssertEqual(t, env.ca.orderTotal(), orders+1)
		// each was put off by the interval from the pass that read it, with
		// no failure counted
		for i, cert := range held {
			renewTime := now.Add(ExtenderMeasurementRenewalPostponeTimeout)
			if i == ExtenderMeasurementRenewalLimit {
				renewTime = next.Add(ExtenderMeasurementRenewalPostponeTimeout)
			}
			assertTestMeasurementRenewCert(t, ctx, cert, renewTime, failureCounts[cert.CertId])
		}
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, serving)
		connect.AssertEqual(t, len(certs), 2)
		if certs[0].RenewedTime == nil {
			t.Fatalf("the serving extender's certificate was not marked renewed")
		}
		connect.AssertEqual(t, certs[1].CapCounted, false)
		verifyTestMeasurementChain(t, env.ca, certs[1].ChainPem, testMeasurementName(serving))
		assertTestMeasurementBatches(t, env)
	})
}

// A certificate put off while its extender serves tcp off 443 is put off
// again each time it comes due while that lasts, counting no failure. Back on
// 443, the extender waits out the interval, and the next pass renews it.
func TestExtenderMeasurementRenewsAPostponedCertificateOnceMeasurableAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		key, _ := requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)
		cert := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0]

		// it activates again serving tcp on 8443, which withdraws nothing
		// (C9), and its certificate comes due after a renewal that failed
		reactivateTestMeasurementRenewExtender(t, ctx, 1, 8443, "203.0.113.7")
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Withdrawn, 0)
		now := server.NowUtc().Truncate(time.Second)
		model.DeferNetworkExtenderMeasurementCertRenewal(ctx, cert.CertId, now.Add(-time.Minute))

		result := runTestMeasurementRenewPassAt(t, ctx, now)
		connect.AssertEqual(t, result.Renewed+result.Failed, 0)
		later := now.Add(ExtenderMeasurementRenewalPostponeTimeout)
		assertTestMeasurementRenewCert(t, ctx, cert, later, 1)

		// still on 8443 when it is due again: put off again
		result = runTestMeasurementRenewPassAt(t, ctx, later)
		connect.AssertEqual(t, result.Renewed+result.Failed, 0)
		renewAt := later.Add(ExtenderMeasurementRenewalPostponeTimeout)
		assertTestMeasurementRenewCert(t, ctx, cert, renewAt, 1)
		connect.AssertEqual(t, env.ca.issuedCount(), 1)

		// back on 443: not before the interval is out, then at the first pass
		reactivateTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		result = runTestMeasurementRenewPassAt(t, ctx, renewAt.Add(-time.Minute))
		connect.AssertEqual(t, result.Renewed+result.Failed, 0)
		assertTestMeasurementRenewCert(t, ctx, cert, renewAt, 1)
		result = runTestMeasurementRenewPassAt(t, ctx, renewAt)
		connect.AssertEqual(t, result.Renewed, 1)
		connect.AssertEqual(t, result.Failed, 0)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 2)
		if certs[0].RenewedTime == nil {
			t.Fatalf("the postponed certificate was not marked renewed")
		}
		connect.AssertEqual(t, certs[1].CapCounted, false)
		leaf := verifyTestMeasurementChain(t, env.ca, certs[1].ChainPem, testMeasurementName(extenderId))
		connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
		assertTestMeasurementBatches(t, env)
	})
}
