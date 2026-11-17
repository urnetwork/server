package work

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The family-pinned measurement names end to end (lag.dev/DRIFTTEST.md K6):
// one certificate for an extender's measurement name and its IPv4 and IPv6
// names, each answered with a dns-01 challenge of its own; the IPv4 name
// published with the extender's A records alone and the IPv6 name with its
// AAAA records alone; the weekly cap holding an extender's first certificate
// for the three names, as the CA counts it new; and the family-pinned names'
// records kept out of the measurement names' batches.

// The extender's request for `names` under `key`, a fresh key when nil,
// stored at `requestTime` as an activation stores it (A10).
func requestTestMeasurementNames(
	t testing.TB,
	ctx context.Context,
	extenderId server.Id,
	names []string,
	key *ecdsa.PrivateKey,
	requestTime time.Time,
) (*ecdsa.PrivateKey, *controller.ExtenderMeasurementRequest) {
	t.Helper()
	if key == nil {
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, key)
	if err != nil {
		t.Fatal(err)
	}
	request, err := controller.ParseExtenderMeasurementCsr(base64.StdEncoding.EncodeToString(csr), extenderId, testMeasurementZone)
	if err != nil {
		t.Fatal(err)
	}
	model.SetNetworkExtenderMeasurementRequest(ctx, extenderId, request.Csr, request.PublicKey, request.Names, requestTime)
	return key, request
}

// The extender's three names (K6): its measurement name, its IPv4 name and
// its IPv6 name.
func testMeasurementNames(extenderId server.Id) []string {
	return controller.ExtenderMeasurementNames(extenderId, testMeasurementZone)
}

func testMeasurementSameNames(a []string, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// No address batch, allowed or refused, carries both a measurement name's
// record and a family-pinned name's.
func assertTestMeasurementFamilyBatchesApart(t testing.TB, env *testMeasurementEnv) {
	t.Helper()
	allowed, denied := env.zone.batches()
	for _, input := range append(allowed, denied...) {
		kinds := map[bool]bool{}
		for _, change := range input.ChangeBatch.Changes {
			switch aws.StringValue(change.ResourceRecordSet.Type) {
			case route53.RRTypeA, route53.RRTypeAaaa:
			default:
				continue
			}
			name := strings.TrimSuffix(aws.StringValue(change.ResourceRecordSet.Name), ".")
			if _, ipFamily, ok := controller.ParseExtenderMeasurementFamilyName(name, testMeasurementZone); ok {
				kinds[ipFamily != 0] = true
			}
		}
		if 1 < len(kinds) {
			t.Fatalf("a batch carries both measurement names and family-pinned names")
		}
	}
}

// The hosts the reconcile publishes from, by extender.
func testMeasurementHosts(t testing.TB, ctx context.Context) map[server.Id]*controller.ExtenderMeasurementHost {
	t.Helper()
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[server.Id]*controller.ExtenderMeasurementHost{}
	for _, host := range controller.GetExtenderMeasurementHostsToPublish(ctx, config, server.NowUtc()) {
		hosts[host.ExtenderId] = host
	}
	return hosts
}

// A request for the three names is ordered as one certificate for all of
// them, each identifier answered with a dns-01 challenge of its own, written
// in one change and removed in one. The dns pass gives the measurement name
// the extender's addresses of both families, the IPv4 name its IPv4 addresses
// alone and the IPv6 name its IPv6 ones alone, in batches apart from the
// measurement names', and publishes the certificate once all its names are
// in sync. An IPv4 extender's IPv6 name gets no record, and its certificate is
// published all the same.
func TestExtenderMeasurementIssuesTheFamilyNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		env.zone.pollsUntilInsync = 2

		dualId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7", "2001:db8::7")
		ipv4Id := createTestMeasurementExtender(t, ctx, 2, "203.0.113.8")
		dualNames := testMeasurementNames(dualId)
		ipv4Names := testMeasurementNames(ipv4Id)
		dualKey, dualRequest := requestTestMeasurementNames(t, ctx, dualId, dualNames, nil, server.NowUtc())
		connect.AssertEqual(t, dualRequest.FamilyNames, true)
		requestTestMeasurementNames(t, ctx, ipv4Id, []string{ipv4Names[2], ipv4Names[0], ipv4Names[1]}, nil, server.NowUtc())

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 2)
		connect.AssertEqual(t, result.Failed, 0)
		connect.AssertEqual(t, env.ca.validationFailures(), 0)

		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, dualId)
		connect.AssertEqual(t, len(certs), 1)
		if !testMeasurementSameNames(certs[0].Names, dualNames) {
			t.Fatalf("names = %v, expected %v", certs[0].Names, dualNames)
		}
		connect.AssertEqual(t, certs[0].CapCounted, true)
		for _, name := range dualNames {
			leaf := verifyTestMeasurementChain(t, env.ca, certs[0].ChainPem, name)
			connect.AssertEqual(t, len(leaf.DNSNames), 3)
			connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&dualKey.PublicKey), true)
		}
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, dualId).State, model.NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, ipv4Id).State, model.NetworkExtenderMeasurementStateIssued)

		// one challenge per identifier, written in one change and removed in
		// one, and none left
		allowed, _ := env.zone.batches()
		challengeNames := map[string][]string{}
		for _, input := range allowed {
			changes := input.ChangeBatch.Changes
			if aws.StringValue(changes[0].ResourceRecordSet.Type) != route53.RRTypeTxt {
				continue
			}
			connect.AssertEqual(t, len(changes), 3)
			names := []string{}
			for _, change := range changes {
				names = append(names, strings.TrimSuffix(aws.StringValue(change.ResourceRecordSet.Name), "."))
			}
			action := aws.StringValue(changes[0].Action)
			challengeNames[action] = append(challengeNames[action], names...)
		}
		expectedChallengeNames := []string{}
		for _, name := range append(slices.Clone(dualNames), ipv4Names...) {
			expectedChallengeNames = append(expectedChallengeNames, "_acme-challenge."+name)
		}
		for _, action := range []string{route53.ChangeActionUpsert, route53.ChangeActionDelete} {
			if !testMeasurementSameNames(challengeNames[action], expectedChallengeNames) {
				t.Fatalf("%s challenge names = %v, expected %v", action, challengeNames[action], expectedChallengeNames)
			}
		}
		for _, recordSet := range env.zone.listedRecordSets(env.hostedZoneId) {
			if strings.HasPrefix(aws.StringValue(recordSet.Name), "_acme-challenge.") {
				t.Fatalf("a challenge record was left: %s", aws.StringValue(recordSet.Name))
			}
		}

		// each family's name holds that family's addresses alone
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Names, 2)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			dualNames[0] + " A":    {"203.0.113.7"},
			dualNames[0] + " AAAA": {"2001:db8::7"},
			dualNames[1] + " A":    {"203.0.113.7"},
			dualNames[2] + " AAAA": {"2001:db8::7"},
			ipv4Names[0] + " A":    {"203.0.113.8"},
			ipv4Names[1] + " A":    {"203.0.113.8"},
		})
		assertTestMeasurementForeignRecordSets(t, env, testMeasurementForeignApexRecordSets())
		assertTestMeasurementBatches(t, env)
		assertTestMeasurementFamilyBatchesApart(t, env)

		// published, the IPv4 extender's included
		for _, extenderId := range []server.Id{dualId, ipv4Id} {
			cert := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0]
			if cert.PublishTime == nil {
				t.Fatalf("the certificate of %s was not published", extenderId)
			}
		}
		hosts := testMeasurementHosts(t, ctx)
		for extenderId, names := range map[server.Id][]string{dualId: dualNames, ipv4Id: ipv4Names} {
			host := hosts[extenderId]
			if host == nil || host.PublishTime == nil || host.FamilyPublishTime == nil {
				t.Fatalf("the host of %s is not published: %+v", extenderId, host)
			}
			connect.AssertEqual(t, host.Name, names[0])
			connect.AssertEqual(t, host.Ipv4Name, names[1])
			connect.AssertEqual(t, host.Ipv6Name, names[2])
		}

		// a pass over a zone that matches sends nothing
		dnsResult = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Upserted+dnsResult.Deleted+dnsResult.Published, 0)

		// an address change moves each name's records, the family's with it
		reactivateTestMeasurementExtender(t, ctx, 1, "203.0.113.9")
		runTestMeasurementDns(t, ctx)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			dualNames[0] + " A":    {"203.0.113.9"},
			dualNames[0] + " AAAA": {"2001:db8::7"},
			dualNames[1] + " A":    {"203.0.113.9"},
			dualNames[2] + " AAAA": {"2001:db8::7"},
			ipv4Names[0] + " A":    {"203.0.113.8"},
			ipv4Names[1] + " A":    {"203.0.113.8"},
		})
		assertTestMeasurementFamilyBatchesApart(t, env)

		// the extender goes away: all of its names leave dns together, the
		// family-pinned names' deletions in batches of their own, and its
		// certificate stops being published
		deactivateTestMeasurementExtender(t, ctx, dualId, server.NowUtc())
		runTestMeasurementDns(t, ctx)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			ipv4Names[0] + " A": {"203.0.113.8"},
			ipv4Names[1] + " A": {"203.0.113.8"},
		})
		assertTestMeasurementBatches(t, env)
		assertTestMeasurementFamilyBatchesApart(t, env)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, dualId)[0].PublishTime == nil, true)
		if host := testMeasurementHosts(t, ctx)[dualId]; host != nil {
			t.Fatalf("an extender that went away is still published from: %+v", host)
		}
	})
}

// An extender whose certificate is for its measurement name alone asks for its
// family-pinned names too. The CA counts a certificate for a set of names it
// has none for as new, so the request is held by the weekly cap like a new
// extender's (C7), and the extender keeps its certificate for the measurement
// name, which is renewed for that name alone, outside the cap.
func TestExtenderMeasurementFamilyNamesWaitForTheCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 2})
		now := server.NowUtc()

		upgradingId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		otherId := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		key, _ := requestTestMeasurementNames(t, ctx, upgradingId, testMeasurementNames(upgradingId)[:1], nil, now.Add(-2*time.Second))
		requestTestMeasurementNames(t, ctx, otherId, testMeasurementNames(otherId)[:1], nil, now.Add(-time.Second))
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)

		// the cap is spent, and the request for all three names waits
		requestTestMeasurementNames(t, ctx, upgradingId, testMeasurementNames(upgradingId), key, server.NowUtc())
		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 0)
		connect.AssertEqual(t, result.Waiting, 1)
		waiting := model.Testing_GetNetworkExtenderMeasurement(ctx, upgradingId)
		connect.AssertEqual(t, waiting.State, model.NetworkExtenderMeasurementStatePending)
		if !strings.Contains(waiting.Reason, "weekly cap") {
			t.Fatalf("the waiting request says %q", waiting.Reason)
		}

		// its certificate for the measurement name comes due, and is renewed
		// for that name alone, outside the cap
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, upgradingId)
		connect.AssertEqual(t, len(certs), 1)
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, certs[0].CertId, server.NowUtc().Add(-time.Second))
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Renewed, 1)
		connect.AssertEqual(t, result.Issued, 0)
		connect.AssertEqual(t, result.Waiting, 1)
		certs = model.Testing_GetNetworkExtenderMeasurementCerts(ctx, upgradingId)
		connect.AssertEqual(t, len(certs), 2)
		if !slices.Equal(certs[1].Names, testMeasurementNames(upgradingId)[:1]) {
			t.Fatalf("the renewal names %v", certs[1].Names)
		}
		connect.AssertEqual(t, certs[1].CapCounted, false)
		verifyTestMeasurementChain(t, env.ca, certs[1].ChainPem, testMeasurementName(upgradingId))
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, upgradingId).State, model.NetworkExtenderMeasurementStatePending)
		assertTestMeasurementBatches(t, env)
	})
}

// With room under the cap, an extender whose certificate is for its
// measurement name alone is ordered one certificate for all three names,
// counted by the cap, which replaces the other: the other is marked renewed,
// and is renewed no more. While the measurement name stays published, the
// dns pass writes the family-pinned names, and publishes the new certificate
// once they are in sync. That certificate is renewed for all three names with
// the extender's request, outside the cap.
func TestExtenderMeasurementFamilyNamesUpgrade(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7", "2001:db8::7")
		names := testMeasurementNames(extenderId)

		key, _ := requestTestMeasurementNames(t, ctx, extenderId, names[:1], nil, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)
		runTestMeasurementDns(t, ctx)
		plainCert := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0]
		if plainCert.PublishTime == nil {
			t.Fatal("the measurement name was not published")
		}
		if host := testMeasurementHosts(t, ctx)[extenderId]; host == nil || host.Ipv4Name != "" || host.Ipv6Name != "" {
			t.Fatalf("the host of a certificate for the measurement name alone = %+v", host)
		}

		// the request for all three names is a new certificate to the CA
		requestTestMeasurementNames(t, ctx, extenderId, names, key, server.NowUtc())
		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 2)
		if !testMeasurementSameNames(certs[1].Names, names) {
			t.Fatalf("names = %v", certs[1].Names)
		}
		connect.AssertEqual(t, certs[1].CapCounted, true)
		if certs[0].RenewedTime == nil {
			t.Fatal("the certificate for the measurement name alone was not marked renewed")
		}
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)

		// the measurement name stays published; the family-pinned names are
		// written and published with the new certificate
		runTestMeasurementDns(t, ctx)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			names[0] + " A":    {"203.0.113.7"},
			names[0] + " AAAA": {"2001:db8::7"},
			names[1] + " A":    {"203.0.113.7"},
			names[2] + " AAAA": {"2001:db8::7"},
		})
		certs = model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		if certs[0].PublishTime == nil || !certs[0].PublishTime.Equal(*plainCert.PublishTime) {
			t.Fatalf("the measurement name's publication moved: %v, was %v", certs[0].PublishTime, plainCert.PublishTime)
		}
		if certs[1].PublishTime == nil {
			t.Fatal("the certificate for all three names was not published")
		}
		host := testMeasurementHosts(t, ctx)[extenderId]
		if host == nil || host.FamilyPublishTime == nil || host.Ipv4Name != names[1] || host.Ipv6Name != names[2] {
			t.Fatalf("the host = %+v", host)
		}
		assertTestMeasurementBatches(t, env)
		assertTestMeasurementFamilyBatchesApart(t, env)

		// only the certificate for all three names comes due again, and it is
		// renewed for all three, outside the cap
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, certs[0].CertId, server.NowUtc().Add(-time.Second))
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, certs[1].CertId, server.NowUtc().Add(-time.Second))
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Renewed, 1)
		connect.AssertEqual(t, result.Issued, 0)
		certs = model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 3)
		if !testMeasurementSameNames(certs[2].Names, names) {
			t.Fatalf("the renewal names %v", certs[2].Names)
		}
		connect.AssertEqual(t, certs[2].CapCounted, false)
		for _, name := range names {
			leaf := verifyTestMeasurementChain(t, env.ca, certs[2].ChainPem, name)
			connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), true)
		}
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
	})
}

// A scoped policy from before K6 lists no family-pinned name, and refuses
// their records. Their batches carry nothing else, so the measurement names
// are still written and published: an extender whose certificate is for its
// measurement name alone is published, and the measurement name of one whose
// certificate carries all three is written, though that certificate is
// published only once the policy lists the family-pinned names and the next
// pass writes them.
func TestExtenderMeasurementFamilyNamesPublishApart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		plainId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		familyId := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2", "2001:db8::2")
		plainNames := testMeasurementNames(plainId)
		familyNames := testMeasurementNames(familyId)

		// both certificates issued while the policy listed the family-pinned
		// names, which it no longer does
		requestTestMeasurementNames(t, ctx, plainId, plainNames[:1], nil, server.NowUtc())
		requestTestMeasurementNames(t, ctx, familyId, familyNames, nil, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)
		env.zone.setFamilyNamesDenied(true)

		runTestMeasurementDns(t, ctx)
		_, denied := env.zone.batches()
		if len(denied) == 0 {
			t.Fatal("the policy refused nothing")
		}
		assertTestMeasurementFamilyBatchesApart(t, env)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			plainNames[0] + " A":     {"203.0.113.1"},
			familyNames[0] + " A":    {"203.0.113.2"},
			familyNames[0] + " AAAA": {"2001:db8::2"},
		})
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, plainId)[0].PublishTime != nil, true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, familyId)[0].PublishTime == nil, true)

		// listed: the next pass writes and publishes them
		env.zone.setFamilyNamesDenied(false)
		runTestMeasurementDns(t, ctx)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			plainNames[0] + " A":     {"203.0.113.1"},
			familyNames[0] + " A":    {"203.0.113.2"},
			familyNames[0] + " AAAA": {"2001:db8::2"},
			familyNames[1] + " A":    {"203.0.113.2"},
			familyNames[2] + " AAAA": {"2001:db8::2"},
		})
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, familyId)[0].PublishTime != nil, true)
		assertTestMeasurementFamilyBatchesApart(t, env)
	})
}
