package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/route53"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The lag.dev measurement certificate tasks end to end (lag.dev/PLAN.md T2):
// issuance against an in-process ACME CA with dns-01 in a fake Route 53 zone,
// the weekly cap, renewal at two thirds, withdrawal and revocation, the zone
// reconcile, and the record field the publish drip signs. The fake zone holds
// the scoped IAM policy, so a change batch that mixed address and challenge
// records, or touched the zone's apex, would fail whichever test sent it.

const (
	testMeasurementZone         = "extender.ur.example"
	testMeasurementHostedZoneId = "Z0MEASUREMENTZONE"
	testMeasurementParentZoneId = "Z0PARENTZONE"
	testMeasurementOtherZoneId  = "Z0OTHERZONE"
)

// What a test changes about the environment.
type testMeasurementEnvSettings struct {
	// the hosted zone the block names; the measurement zone's own by default
	hostedZoneId string
	// 0 leaves the default
	issueCapPerWeek int
}

// One measurement environment: the fake zone, the CA, and an `extender.yml`
// enabling the measurement block against both.
type testMeasurementEnv struct {
	zone          *testMeasurementRoute53
	ca            *testAcmeCa
	rootPublicKey ed25519.PublicKey
	hostedZoneId  string
}

// The record sets that are not the reconcile's, as the measurement zone holds
// them in production: the zone's own at the apex, the CAA record, the geo dns
// sets at the apex, and the Public Suffix List's proof.
func testMeasurementForeignApexRecordSets() []*route53.ResourceRecordSet {
	apex := testMeasurementZone + "."
	return []*route53.ResourceRecordSet{
		{
			Name:            aws.String(apex),
			Type:            aws.String(route53.RRTypeSoa),
			TTL:             aws.Int64(900),
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("ns-1.ur.example. hostmaster.ur.example. 1 7200 900 1209600 60")}},
		},
		{
			Name:            aws.String(apex),
			Type:            aws.String(route53.RRTypeNs),
			TTL:             aws.Int64(172800),
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("ns-1.ur.example.")}},
		},
		{
			Name:            aws.String(apex),
			Type:            aws.String(route53.RRTypeCaa),
			TTL:             aws.Int64(3600),
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String(`0 issue "ca.example; validationmethods=dns-01"`)}},
		},
		{
			Name:            aws.String(apex),
			Type:            aws.String(route53.RRTypeA),
			TTL:             aws.Int64(60),
			SetIdentifier:   aws.String("extender-EU-A"),
			GeoLocation:     &route53.GeoLocation{ContinentCode: aws.String("EU")},
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("198.51.100.200")}},
		},
		{
			Name:            aws.String(apex),
			Type:            aws.String(route53.RRTypeAaaa),
			TTL:             aws.Int64(60),
			SetIdentifier:   aws.String("extender-default-AAAA"),
			GeoLocation:     &route53.GeoLocation{CountryCode: aws.String("*")},
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("2001:db8:ffff::200")}},
		},
		{
			Name:            aws.String(apex),
			Type:            aws.String(route53.RRTypeTxt),
			TTL:             aws.Int64(60),
			SetIdentifier:   aws.String("extender-EU-TXT"),
			GeoLocation:     &route53.GeoLocation{ContinentCode: aws.String("EU")},
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String(`"signed-record"`)}},
		},
		{
			Name:            aws.String("_psl." + apex),
			Type:            aws.String(route53.RRTypeTxt),
			TTL:             aws.Int64(3600),
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String(`"https://psl.example/pull/1"`)}},
		},
	}
}

func newTestMeasurementEnv(t testing.TB, settings testMeasurementEnvSettings) *testMeasurementEnv {
	t.Helper()
	if settings.hostedZoneId == "" {
		settings.hostedZoneId = testMeasurementHostedZoneId
	}
	zone := newTestMeasurementRoute53(
		testMeasurementZone,
		testRoute53HostedZone(testMeasurementZone, testMeasurementHostedZoneId),
		testRoute53HostedZone("ur.example", testMeasurementParentZoneId),
		testRoute53HostedZone("other.example", testMeasurementOtherZoneId),
	)
	for _, recordSet := range testMeasurementForeignApexRecordSets() {
		zone.seed(settings.hostedZoneId, recordSet)
	}

	ca := newTestAcmeCa(t, zone)

	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	accountKeyDer, err := x509.MarshalPKCS8PrivateKey(accountKey)
	if err != nil {
		t.Fatal(err)
	}
	accountKeyPem := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: accountKeyDer}))
	yamlLines := []string{
		"measurement:",
		"  enabled: true",
		"  zone_name: " + testMeasurementZone,
		"  hosted_zone_id: " + settings.hostedZoneId,
		"  acme_directory_url: " + ca.directoryUrl(),
		"  acme_account_key: |",
	}
	for _, line := range strings.Split(strings.TrimSpace(accountKeyPem), "\n") {
		yamlLines = append(yamlLines, "    "+line)
	}
	if 0 < settings.issueCapPerWeek {
		yamlLines = append(yamlLines, fmt.Sprintf("  issue_cap_per_week: %d", settings.issueCapPerWeek))
	}
	rootPublicKey := installTestExtenderWorkConfig(t, yamlLines...)

	previousApi := newExtenderMeasurementRoute53Api
	newExtenderMeasurementRoute53Api = func(_ *controller.ExtenderConfig) (extenderMeasurementRoute53Api, error) {
		return zone, nil
	}
	previousPollInterval := extenderMeasurementInsyncPollInterval
	extenderMeasurementInsyncPollInterval = time.Millisecond
	Testing_ResetExtenderMeasurementAcme()
	Testing_ResetExtenderMeasurementZones()
	Testing_ResetExtenderDnsHostedZoneIds()
	t.Cleanup(func() {
		newExtenderMeasurementRoute53Api = previousApi
		extenderMeasurementInsyncPollInterval = previousPollInterval
		Testing_ResetExtenderMeasurementAcme()
		Testing_ResetExtenderMeasurementZones()
		Testing_ResetExtenderDnsHostedZoneIds()
	})
	return &testMeasurementEnv{
		zone:          zone,
		ca:            ca,
		rootPublicKey: rootPublicKey,
		hostedZoneId:  settings.hostedZoneId,
	}
}

func testMeasurementName(extenderId server.Id) string {
	return controller.ExtenderMeasurementName(extenderId, testMeasurementZone)
}

func testMeasurementExtenderPublicKey(index int) []byte {
	return []byte(fmt.Sprintf("extender-public-key-measure-%04d", index))
}

// One open tier extender serving tcp on 443 at each address given.
func createTestMeasurementExtender(t testing.TB, ctx context.Context, index int, ips ...string) server.Id {
	t.Helper()
	return createTestMeasurementExtenderWith(t, ctx, index, controller.ExtenderMeasurementPort, connect.ExtenderCarrierTcp, ips...)
}

func createTestMeasurementExtenderWith(
	t testing.TB,
	ctx context.Context,
	index int,
	tcpPort int,
	carrier string,
	ips ...string,
) server.Id {
	t.Helper()
	extenderId := server.NewId()
	createTime := server.NowUtc()
	addresses := []*model.NetworkExtenderAddress{}
	for _, ip := range ips {
		addr := netip.MustParseAddr(ip)
		ipVersion := 6
		if addr.Is4() {
			ipVersion = 4
		}
		addresses = append(addresses, &model.NetworkExtenderAddress{
			IpVersion:    ipVersion,
			Ip:           addr,
			Carriers:     []string{carrier},
			ActivateTime: createTime,
			Active:       true,
		})
	}
	model.Testing_CreateNetworkExtender(
		ctx,
		&model.NetworkExtender{
			ExtenderId:  extenderId,
			NetworkId:   server.NewId(),
			ClientId:    server.NewId(),
			PublicKey:   testMeasurementExtenderPublicKey(index),
			CreateTime:  createTime,
			TcpPort:     tcpPort,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: "de",
			Active:      true,
		},
		addresses,
	)
	return extenderId
}

// The extender activates again, from `ip`: the same identity key, so the same
// extender and the same id, with its address of that family replaced.
func reactivateTestMeasurementExtender(t testing.TB, ctx context.Context, index int, ip string) {
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
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   testMeasurementExtenderPublicKey(index),
		TcpPort:     controller.ExtenderMeasurementPort,
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

// A certificate request for `name` under a fresh measurement key.
func newTestMeasurementCsr(t testing.TB, name string) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{name}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, csr
}

// A fresh measurement key and the extender's request under it, stored at
// `requestTime` as an activation stores it (A10).
func requestTestMeasurementCert(
	t testing.TB,
	ctx context.Context,
	extenderId server.Id,
	requestTime time.Time,
) (*ecdsa.PrivateKey, *controller.ExtenderMeasurementRequest) {
	t.Helper()
	key, csr := newTestMeasurementCsr(t, testMeasurementName(extenderId))
	request, err := controller.ParseExtenderMeasurementCsr(base64.StdEncoding.EncodeToString(csr), extenderId, testMeasurementZone)
	if err != nil {
		t.Fatal(err)
	}
	model.SetNetworkExtenderMeasurementRequest(ctx, extenderId, request.Csr, request.PublicKey, []string{request.Name}, requestTime)
	return key, request
}

func runTestMeasurementIssue(t testing.TB, ctx context.Context) *ExtenderMeasurementIssueResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderMeasurementIssue(&ExtenderMeasurementIssueArgs{}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runTestMeasurementDns(t testing.TB, ctx context.Context) *ExtenderMeasurementDnsResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderMeasurementDns(&ExtenderMeasurementDnsArgs{}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The leaf of a stored chain, verified to the CA's root for its name.
func verifyTestMeasurementChain(t testing.TB, ca *testAcmeCa, chainPem string, name string) *x509.Certificate {
	t.Helper()
	leaf, err := extenderMeasurementLeaf(chainPem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:   name,
		Roots:     ca.roots(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("the chain does not verify for %s: %v", name, err)
	}
	return leaf
}

// The address records below the zone's apex, as `<name> <type>` to their
// sorted values, each checked to be a plain record at the default ttl.
func testMeasurementAddressRecords(t testing.TB, env *testMeasurementEnv) map[string][]string {
	t.Helper()
	records := map[string][]string{}
	for _, recordSet := range env.zone.listedRecordSets(env.hostedZoneId) {
		recordType := aws.StringValue(recordSet.Type)
		name := strings.TrimSuffix(aws.StringValue(recordSet.Name), ".")
		if recordType != route53.RRTypeA && recordType != route53.RRTypeAaaa {
			continue
		}
		if _, ok := controller.ParseExtenderMeasurementName(name, testMeasurementZone); !ok {
			continue
		}
		if recordSet.SetIdentifier != nil || aws.Int64Value(recordSet.TTL) != controller.ExtenderMeasurementDefaultDnsTtl {
			t.Fatalf("%s %s is not a plain record at the ttl", name, recordType)
		}
		records[name+" "+recordType] = testMeasurementRecordSetValues(recordSet)
	}
	return records
}

func assertTestMeasurementAddressRecords(t testing.TB, env *testMeasurementEnv, want map[string][]string) {
	t.Helper()
	got := testMeasurementAddressRecords(t, env)
	if len(got) != len(want) {
		t.Fatalf("address records = %v, want %v", got, want)
	}
	for key, values := range want {
		if !slices.Equal(got[key], values) {
			t.Fatalf("address records = %v, want %v", got, want)
		}
	}
}

// Every record set a test seeded as someone else's is still there, exactly.
func assertTestMeasurementForeignRecordSets(t testing.TB, env *testMeasurementEnv, foreign []*route53.ResourceRecordSet) {
	t.Helper()
	listed := env.zone.listedRecordSets(env.hostedZoneId)
	for _, recordSet := range foreign {
		if !slices.ContainsFunc(listed, func(existing *route53.ResourceRecordSet) bool {
			return testMeasurementRecordSetsEqual(existing, recordSet)
		}) {
			t.Fatalf("%s was changed or deleted", testMeasurementRecordSetKey(recordSet))
		}
	}
}

// No batch was refused by the policy, and every batch sent was of one kind.
func assertTestMeasurementBatches(t testing.TB, env *testMeasurementEnv) {
	t.Helper()
	allowed, denied := env.zone.batches()
	if 0 < len(denied) {
		keys := []string{}
		for _, change := range denied[0].ChangeBatch.Changes {
			keys = append(keys, aws.StringValue(change.Action)+" "+testMeasurementRecordSetKey(change.ResourceRecordSet))
		}
		t.Fatalf("the policy refused %d batches, the first %v", len(denied), keys)
	}
	for _, input := range allowed {
		types := []string{}
		for _, change := range input.ChangeBatch.Changes {
			recordType := aws.StringValue(change.ResourceRecordSet.Type)
			if recordType == route53.RRTypeAaaa {
				recordType = route53.RRTypeA
			}
			if !slices.Contains(types, recordType) {
				types = append(types, recordType)
			}
		}
		if len(types) != 1 {
			t.Fatalf("a batch mixes %v", types)
		}
	}
}

// C5 end to end: a pending request of an eligible extender is ordered for its
// one name, its challenge answered in the zone only once the change is in
// sync, the chain stored and checked, the flag set, the request issued and
// the challenge record removed. The dns pass then gives the name its
// extender's addresses of both families (C8, N3).
func TestExtenderMeasurementIssuesAgainstTheCa(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		// two polls before a change is in sync: a CA that looked at once
		// would find nothing
		env.zone.pollsUntilInsync = 2

		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7", "2001:db8::7")
		name := testMeasurementName(extenderId)
		key, request := requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Failed, 0)
		connect.AssertEqual(t, env.ca.validationFailures(), 0)
		connect.AssertEqual(t, env.ca.validityOrderTotal(), 0)
		if env.zone.getChangeCount < 2 {
			t.Fatalf("the change was polled %d times, so the order did not wait for sync", env.zone.getChangeCount)
		}

		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 1)
		cert := certs[0]
		if !slices.Equal(cert.Names, []string{name}) {
			t.Fatalf("names = %v", cert.Names)
		}
		connect.AssertEqual(t, bytes.Equal(cert.Csr, request.Csr), true)
		leaf := verifyTestMeasurementChain(t, env.ca, cert.ChainPem, name)
		leafKey, ok := leaf.PublicKey.(*ecdsa.PublicKey)
		if !ok || !leafKey.Equal(&key.PublicKey) {
			t.Fatalf("the certificate is not for the extender's key")
		}
		connect.AssertEqual(t, cert.NotAfter.UTC(), leaf.NotAfter.UTC())
		connect.AssertEqual(t, cert.CapCounted, true)
		// the renewal is at two thirds of the lifetime (C6)
		connect.AssertEqual(t, cert.RenewTime.UTC(), leaf.NotBefore.Add(2*leaf.NotAfter.Sub(leaf.NotBefore)/3).UTC())

		everIssued, _ := model.Testing_GetNetworkExtenderMeasurementExposure(ctx, extenderId)
		connect.AssertEqual(t, everIssued, true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)

		// the challenge record was at `_acme-challenge.<id>` and is gone again
		allowed, _ := env.zone.batches()
		connect.AssertEqual(t, len(allowed), 2)
		for i, action := range []string{route53.ChangeActionUpsert, route53.ChangeActionDelete} {
			changes := allowed[i].ChangeBatch.Changes
			connect.AssertEqual(t, len(changes), 1)
			connect.AssertEqual(t, aws.StringValue(changes[0].Action), action)
			connect.AssertEqual(t, aws.StringValue(changes[0].ResourceRecordSet.Type), route53.RRTypeTxt)
			connect.AssertEqual(t, aws.StringValue(changes[0].ResourceRecordSet.Name), "_acme-challenge."+name+".")
		}
		for _, recordSet := range env.zone.listedRecordSets(env.hostedZoneId) {
			if strings.HasPrefix(aws.StringValue(recordSet.Name), "_acme-challenge.") {
				t.Fatalf("a challenge record was left: %s", aws.StringValue(recordSet.Name))
			}
		}

		// a second pass has nothing to do
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 1)

		// the name resolves to the extender's addresses of both families
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Names, 1)
		connect.AssertEqual(t, dnsResult.Withdrawn, 0)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			name + " A":    {"203.0.113.7"},
			name + " AAAA": {"2001:db8::7"},
		})
		assertTestMeasurementForeignRecordSets(t, env, testMeasurementForeignApexRecordSets())
		assertTestMeasurementBatches(t, env)
	})
}

// A request is refused when its extender may not be measured, or when it is
// not a request for exactly its extender's own name (C5). A refused request
// keeps its key, which A8 names (key_sha256), so the extender knows the
// refusal is of its own request.
func TestExtenderMeasurementRefusesWhatMayNotBeIssued(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		now := server.NowUtc()
		publicKeys := map[server.Id][]byte{}
		request := func(extenderId server.Id) {
			_, request := requestTestMeasurementCert(t, ctx, extenderId, now)
			publicKeys[extenderId] = request.PublicKey
		}

		gated := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		if err := model.SetNetworkExtenderDirectoryTier(ctx, gated, connect.ExtenderDirectoryTierGated, false); err != nil {
			t.Fatal(err)
		}
		request(gated)

		canary := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		if err := model.SetNetworkExtenderCanaryChannel(ctx, canary, connect.ExtenderChannelDns, false); err != nil {
			t.Fatal(err)
		}
		request(canary)

		// no public inbound address (decision 14)
		webRtcOnly := createTestMeasurementExtenderWith(t, ctx, 3, controller.ExtenderMeasurementPort, connect.ExtenderCarrierWebRtc, "203.0.113.3")
		request(webRtcOnly)

		// tcp where no browser connects (decision 4)
		otherPort := createTestMeasurementExtenderWith(t, ctx, 4, 8443, connect.ExtenderCarrierTcp, "203.0.113.4")
		request(otherPort)

		// a stored request for another extender's name
		otherName := createTestMeasurementExtender(t, ctx, 5, "203.0.113.5")
		otherKey, otherCsr := newTestMeasurementCsr(t, testMeasurementName(gated))
		otherPublicKey, err := x509.MarshalPKIXPublicKey(&otherKey.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		model.SetNetworkExtenderMeasurementRequest(ctx, otherName, otherCsr, otherPublicKey, []string{testMeasurementName(gated)}, now)
		publicKeys[otherName] = otherPublicKey

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 0)
		connect.AssertEqual(t, result.Refused, 5)
		connect.AssertEqual(t, env.ca.orderTotal(), 0)
		for _, extenderId := range []server.Id{gated, canary, webRtcOnly, otherPort, otherName} {
			measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStateRefused)
			if measurement.Reason == "" {
				t.Fatalf("a refusal carries no reason")
			}
			connect.AssertEqual(t, bytes.Equal(measurement.PublicKey, publicKeys[extenderId]), true)
			everIssued, _ := model.Testing_GetNetworkExtenderMeasurementExposure(ctx, extenderId)
			connect.AssertEqual(t, everIssued, false)
		}
		allowed, denied := env.zone.batches()
		connect.AssertEqual(t, len(allowed)+len(denied), 0)
	})
}

// C7: new certificates stop at the weekly cap, longest waiting first, and the
// rest wait. Only a new extender spends the cap: a rotated key asks for a
// name that already has a certificate, which is a renewal to the CA.
func TestExtenderMeasurementWaitsForTheWeeklyCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 2})
		now := server.NowUtc()

		extenderIds := []server.Id{}
		for i := range 3 {
			extenderId := createTestMeasurementExtender(t, ctx, 1+i, fmt.Sprintf("203.0.113.%d", 1+i))
			requestTestMeasurementCert(t, ctx, extenderId, now.Add(time.Duration(i-3)*time.Second))
			extenderIds = append(extenderIds, extenderId)
		}

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 2)
		connect.AssertEqual(t, result.Waiting, 1)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderIds[0])), 1)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderIds[1])), 1)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderIds[2])), 0)
		waiting := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderIds[2])
		connect.AssertEqual(t, waiting.State, model.NetworkExtenderMeasurementStatePending)
		if !strings.Contains(waiting.Reason, "weekly cap") {
			t.Fatalf("the waiting request says %q", waiting.Reason)
		}

		// the first extender rotates its key; the cap is spent and it is
		// served anyway
		requestTestMeasurementCert(t, ctx, extenderIds[0], server.NowUtc())
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Waiting, 1)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderIds[0])
		connect.AssertEqual(t, len(certs), 2)
		connect.AssertEqual(t, certs[1].CapCounted, false)
		connect.AssertEqual(t, env.ca.issuedCount(), 3)
		assertTestMeasurementBatches(t, env)
	})
}

// C7 behind a backlog: while the weekly cap is spent, a pass leaves every
// new extender's request pending, so the same longest waiting ones come back
// each pass. A request the cap does not hold, a rotated key's here, is still
// served at the next pass, however many capped requests wait ahead of it, and
// more than one pass reads.
func TestExtenderMeasurementServesAnUncappedRequestBehindACappedBacklog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 1})
		now := server.NowUtc()

		// the week's one new certificate
		rotating := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, rotating, now.Add(-2*time.Hour))
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)

		// new extenders, waiting longer than the rotated key will, and more of
		// them than a pass reads
		for i := range ExtenderMeasurementPendingLimit + 1 {
			extenderId := createTestMeasurementExtender(t, ctx, 2+i, fmt.Sprintf("198.51.100.%d", 1+i%250))
			requestTestMeasurementCert(t, ctx, extenderId, now.Add(-time.Hour).Add(time.Duration(i)*time.Second))
		}
		// the first extender rotates its key: a renewal to the CA
		requestTestMeasurementCert(t, ctx, rotating, now)

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Waiting, ExtenderMeasurementPendingLimit-1)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, rotating)
		connect.AssertEqual(t, len(certs), 2)
		connect.AssertEqual(t, certs[1].CapCounted, false)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, rotating).State, model.NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, env.ca.issuedCount(), 2)
	})
}

// The zone with every change refused.
type testRefusingMeasurementRoute53 struct {
	*testMeasurementRoute53
}

func (self *testRefusingMeasurementRoute53) ChangeResourceRecordSetsWithContext(
	_ aws.Context,
	_ *route53.ChangeResourceRecordSetsInput,
	_ ...request.Option,
) (*route53.ChangeResourceRecordSetsOutput, error) {
	return nil, fmt.Errorf("throttled")
}

// A failed order backs off on its own request rather than failing the pass,
// and the next attempt waits for the backoff. The flag is already set: it is
// set before the order (C5).
func TestExtenderMeasurementBacksOffAFailedOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())

		// the zone refuses every change, so no challenge can be answered
		previousApi := newExtenderMeasurementRoute53Api
		newExtenderMeasurementRoute53Api = func(_ *controller.ExtenderConfig) (extenderMeasurementRoute53Api, error) {
			return &testRefusingMeasurementRoute53{testMeasurementRoute53: env.zone}, nil
		}
		t.Cleanup(func() { newExtenderMeasurementRoute53Api = previousApi })

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Failed, 1)
		connect.AssertEqual(t, result.Issued, 0)
		measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, measurement.FailureCount, 1)
		if measurement.RetryTime == nil || !measurement.RetryTime.After(server.NowUtc()) {
			t.Fatalf("the failed order did not back off")
		}
		everIssued, _ := model.Testing_GetNetworkExtenderMeasurementExposure(ctx, extenderId)
		connect.AssertEqual(t, everIssued, true)

		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Failed, 0)
		connect.AssertEqual(t, result.Issued, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 0)
	})
}

// C6: a certificate renews at two thirds of its lifetime with the stored
// request, the new one replaces it, and the renewal spends nothing of the
// cap. An authorization the CA still holds is not answered again. A renewal
// follows a key the extender rotated.
func TestExtenderMeasurementRenewsAtTwoThirds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		name := testMeasurementName(extenderId)
		key, _ := requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)

		// not yet due
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Renewed, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 1)

		// two thirds of the lifetime later, with the authorization still
		// valid at the CA: no challenge is written
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, certs[0].CertId, server.NowUtc().Add(-time.Second))
		env.ca.setReuseAuthorizations(true)
		batchesBefore, _ := env.zone.batches()
		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Renewed, 1)
		connect.AssertEqual(t, result.Issued, 0)
		batchesAfter, _ := env.zone.batches()
		connect.AssertEqual(t, len(batchesAfter), len(batchesBefore))
		env.ca.setReuseAuthorizations(false)

		certs = model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 2)
		if certs[0].RenewedTime == nil {
			t.Fatalf("the old certificate was not marked renewed")
		}
		connect.AssertEqual(t, certs[1].RenewedTime == nil, true)
		connect.AssertEqual(t, certs[1].CapCounted, false)
		leaf := verifyTestMeasurementChain(t, env.ca, certs[1].ChainPem, name)
		connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
		// the renewed one is no longer due, and the new one not yet
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Renewed, 0)

		// the extender rotates its key as the renewal comes due: one order,
		// for the new key
		rotatedKey, _ := requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, certs[1].CertId, server.NowUtc().Add(-time.Second))
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Renewed+result.Issued, 1)
		connect.AssertEqual(t, env.ca.issuedCount(), 3)
		certs = model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 3)
		leaf = verifyTestMeasurementChain(t, env.ca, certs[2].ChainPem, name)
		connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&rotatedKey.PublicKey), true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
		assertTestMeasurementBatches(t, env)
	})
}

// A certificate that ran out un-renewed does not leave its request issued
// with nothing behind it: the pass puts the request back in the queue and
// issues for it again.
func TestExtenderMeasurementReissuesACertificateThatRanOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)

		config, err := controller.EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		// the pass as it runs after the certificate's ninety days
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		result := &ExtenderMeasurementIssueResult{}
		issueExtenderMeasurementCerts(clientSession, config, server.NowUtc().Add(100*24*time.Hour), result)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Renewed, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 2)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)), 2)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
	})
}

// C9 end to end. A deactivated extender and one an operator moved to the
// gated tier are withdrawn, leave the zone in the same dns pass, and are
// revoked by the next issuance pass for the cessation of operation. An
// address change withdraws nothing and orders nothing: the name's record
// moves (C8). A deactivated extender that comes back asks again and is issued
// again, outside the cap; a gated one that asks again is refused.
func TestExtenderMeasurementWithdrawsAndRevokes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		now := server.NowUtc()

		deactivated := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		moved := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		gated := createTestMeasurementExtender(t, ctx, 3, "203.0.113.3")
		staying := createTestMeasurementExtender(t, ctx, 4, "203.0.113.4")
		requests := map[server.Id]*controller.ExtenderMeasurementRequest{}
		for _, extenderId := range []server.Id{deactivated, moved, gated, staying} {
			_, requests[extenderId] = requestTestMeasurementCert(t, ctx, extenderId, now)
		}
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 4)
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Names, 4)
		connect.AssertEqual(t, dnsResult.Withdrawn, 0)

		model.RecordNetworkExtenderProbeResult(ctx, deactivated, 4, false, server.NowUtc(), 1, func(*model.NetworkExtender, time.Time) ([]byte, error) {
			return []byte("revocation"), nil
		})
		reactivateTestMeasurementExtender(t, ctx, 2, "198.51.100.2")
		if err := model.SetNetworkExtenderDirectoryTier(ctx, gated, connect.ExtenderDirectoryTierGated, true); err != nil {
			t.Fatal(err)
		}

		dnsResult = runTestMeasurementDns(t, ctx)
		// the override withdrew its own at once; the dns pass the deactivated
		connect.AssertEqual(t, dnsResult.Withdrawn, 1)
		connect.AssertEqual(t, dnsResult.Names, 2)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(moved) + " A":   {"198.51.100.2"},
			testMeasurementName(staying) + " A": {"203.0.113.4"},
		})

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Revoked, 2)
		connect.AssertEqual(t, result.Issued+result.Renewed, 0)
		reasons := env.ca.revokedReasons()
		connect.AssertEqual(t, len(reasons), 2)
		for _, reason := range reasons {
			// cessationOfOperation
			connect.AssertEqual(t, reason, 5)
		}
		for _, extenderId := range []server.Id{deactivated, gated} {
			certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
			if certs[0].WithdrawTime == nil || certs[0].RevokeTime == nil {
				t.Fatalf("%s was not withdrawn and revoked", extenderId)
			}
			measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStateWithdrawn)
			if !slices.Equal(measurement.Names, []string{testMeasurementName(extenderId)}) || measurement.Reason == "" {
				t.Fatalf("the withdrawn request reads %v %q", measurement.Names, measurement.Reason)
			}
		}
		for _, extenderId := range []server.Id{moved, staying} {
			certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
			connect.AssertEqual(t, len(certs), 1)
			connect.AssertEqual(t, certs[0].WithdrawTime == nil, true)
			connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
		}
		connect.AssertEqual(t, env.ca.issuedCount(), 4)
		// nothing is revoked twice
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Revoked, 0)

		// both ask again with the request they hold. The one that came back
		// is issued again; the gated one is refused
		reactivateTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		for _, extenderId := range []server.Id{deactivated, gated} {
			request := requests[extenderId]
			state := model.SetNetworkExtenderMeasurementRequest(ctx, extenderId, request.Csr, request.PublicKey, []string{request.Name}, server.NowUtc())
			connect.AssertEqual(t, state, model.NetworkExtenderMeasurementStatePending)
		}
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Refused, 1)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, deactivated)
		connect.AssertEqual(t, len(certs), 2)
		connect.AssertEqual(t, certs[1].CapCounted, false)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, gated).State, model.NetworkExtenderMeasurementStateRefused)

		dnsResult = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Names, 3)
		connect.AssertEqual(t, dnsResult.Withdrawn, 0)
		assertTestMeasurementForeignRecordSets(t, env, testMeasurementForeignApexRecordSets())
		assertTestMeasurementBatches(t, env)
	})
}

// C8: the reconcile owns the A and AAAA records at measurement names and
// nothing else. It writes each serving extender's records, replaces one that
// differs, deletes one no extender serves, and follows an address change. It
// leaves alone the zone's apex, where the geo dns sets are, every other type,
// every other name, and an order's open challenge record. A pass over a zone
// that already matches sends nothing.
func TestExtenderMeasurementDnsReconcilesOnlyWhatItOwns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		now := server.NowUtc()

		dualStack := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1", "2001:db8::1")
		requestTestMeasurementCert(t, ctx, dualStack, now)
		single := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		requestTestMeasurementCert(t, ctx, single, now)
		// an extender with no certificate is in no record
		withoutCert := createTestMeasurementExtender(t, ctx, 3, "203.0.113.3")
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)

		gone := server.NewId()
		foreign := append(testMeasurementForeignApexRecordSets(),
			// an order's open challenge
			&route53.ResourceRecordSet{
				Name:            aws.String("_acme-challenge." + testMeasurementName(withoutCert) + "."),
				Type:            aws.String(route53.RRTypeTxt),
				TTL:             aws.Int64(60),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String(`"open-order"`)}},
			},
			// another type at a measurement name
			&route53.ResourceRecordSet{
				Name:            aws.String(testMeasurementName(dualStack) + "."),
				Type:            aws.String(route53.RRTypeTxt),
				TTL:             aws.Int64(300),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String(`"someone's note"`)}},
			},
			// address records at names that are not measurement names
			&route53.ResourceRecordSet{
				Name:            aws.String("www." + testMeasurementZone + "."),
				Type:            aws.String(route53.RRTypeA),
				TTL:             aws.Int64(300),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("198.51.100.80")}},
			},
			&route53.ResourceRecordSet{
				Name:            aws.String("203-0-113-9." + testMeasurementZone + "."),
				Type:            aws.String(route53.RRTypeA),
				TTL:             aws.Int64(300),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("203.0.113.9")}},
			},
		)
		for _, recordSet := range foreign[len(testMeasurementForeignApexRecordSets()):] {
			env.zone.seed(env.hostedZoneId, recordSet)
		}
		for _, recordSet := range []*route53.ResourceRecordSet{
			// the name of an extender that no longer serves
			{
				Name:            aws.String(testMeasurementName(gone) + "."),
				Type:            aws.String(route53.RRTypeA),
				TTL:             aws.Int64(controller.ExtenderMeasurementDefaultDnsTtl),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("203.0.113.90")}},
			},
			// a serving name at an address it no longer has
			{
				Name:            aws.String(testMeasurementName(dualStack) + "."),
				Type:            aws.String(route53.RRTypeA),
				TTL:             aws.Int64(controller.ExtenderMeasurementDefaultDnsTtl),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("203.0.113.91")}},
			},
			// a serving name with a routing policy nobody asked for
			{
				Name:            aws.String(testMeasurementName(single) + "."),
				Type:            aws.String(route53.RRTypeA),
				TTL:             aws.Int64(controller.ExtenderMeasurementDefaultDnsTtl),
				SetIdentifier:   aws.String("weighted"),
				Weight:          aws.Int64(1),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("203.0.113.2")}},
			},
			// a family the serving extender has no address of
			{
				Name:            aws.String(testMeasurementName(single) + "."),
				Type:            aws.String(route53.RRTypeAaaa),
				TTL:             aws.Int64(controller.ExtenderMeasurementDefaultDnsTtl),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("2001:db8::92")}},
			},
		} {
			env.zone.seed(env.hostedZoneId, recordSet)
		}

		result := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, result.Names, 2)
		connect.AssertEqual(t, result.Deleted, 3)
		connect.AssertEqual(t, result.Upserted, 3)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(dualStack) + " A":    {"203.0.113.1"},
			testMeasurementName(dualStack) + " AAAA": {"2001:db8::1"},
			testMeasurementName(single) + " A":       {"203.0.113.2"},
		})
		assertTestMeasurementForeignRecordSets(t, env, foreign)
		assertTestMeasurementBatches(t, env)

		// a zone that already matches is left as it is
		allowedBefore, _ := env.zone.batches()
		result = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, result.Upserted, 0)
		connect.AssertEqual(t, result.Deleted, 0)
		allowedAfter, _ := env.zone.batches()
		connect.AssertEqual(t, len(allowedAfter), len(allowedBefore))

		// an address change is only a record update (N3): the v4 address
		// moves and the v6 address goes
		reactivateTestMeasurementExtender(t, ctx, 1, "198.51.100.1")
		model.Testing_DeactivateNetworkExtenderAddress(ctx, dualStack, 6)
		result = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, result.Withdrawn, 0)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(dualStack) + " A": {"198.51.100.1"},
			testMeasurementName(single) + " A":    {"203.0.113.2"},
		})
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, dualStack)), 1)
		connect.AssertEqual(t, env.ca.issuedCount(), 2)
		assertTestMeasurementForeignRecordSets(t, env, foreign)
		assertTestMeasurementBatches(t, env)
	})
}

// A measurement name that holds a cname, which nothing can share a name with
// and which is not the reconcile's to delete, is left out; every other name
// is still written.
func TestExtenderMeasurementDnsLeavesANameThatHoldsACname(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		now := server.NowUtc()
		blocked := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, blocked, now)
		served := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		requestTestMeasurementCert(t, ctx, served, now)
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)

		cname := &route53.ResourceRecordSet{
			Name:            aws.String(testMeasurementName(blocked) + "."),
			Type:            aws.String(route53.RRTypeCname),
			TTL:             aws.Int64(300),
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("elsewhere.example.")}},
		}
		env.zone.seed(env.hostedZoneId, cname)

		result := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, result.Names, 2)
		connect.AssertEqual(t, result.Upserted, 1)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(served) + " A": {"203.0.113.2"},
		})
		assertTestMeasurementForeignRecordSets(t, env, []*route53.ResourceRecordSet{cname})
		assertTestMeasurementBatches(t, env)
	})
}

// The names may live in a hosted zone above the measurement zone. The
// reconcile then reads only the measurement zone's part of it, which Route 53
// keeps together, and changes nothing outside.
func TestExtenderMeasurementDnsReadsOnlyItsPartOfAParentZone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{hostedZoneId: testMeasurementParentZoneId})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)

		outside := []*route53.ResourceRecordSet{}
		// `extender-x` lists before the measurement zone and the others after
		for _, label := range []string{"api", "extender-x", "extenderz", "www", "zzz"} {
			recordSet := &route53.ResourceRecordSet{
				Name:            aws.String(label + ".ur.example."),
				Type:            aws.String(route53.RRTypeA),
				TTL:             aws.Int64(300),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("198.51.100.10")}},
			}
			outside = append(outside, recordSet)
			env.zone.seed(env.hostedZoneId, recordSet)
		}
		gone := server.NewId()
		env.zone.seed(env.hostedZoneId, &route53.ResourceRecordSet{
			Name:            aws.String(testMeasurementName(gone) + "."),
			Type:            aws.String(route53.RRTypeA),
			TTL:             aws.Int64(controller.ExtenderMeasurementDefaultDnsTtl),
			ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("203.0.113.90")}},
		})

		result := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, result.Upserted, 1)
		connect.AssertEqual(t, result.Deleted, 1)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(extenderId) + " A": {"203.0.113.1"},
		})
		assertTestMeasurementForeignRecordSets(t, env, append(outside, testMeasurementForeignApexRecordSets()...))
		assertTestMeasurementBatches(t, env)

		// the listing began at the measurement zone and ended at the first
		// name past it
		listedNames := slices.Clone(env.zone.listedNames)
		if slices.Contains(listedNames, "api.ur.example.") || slices.Contains(listedNames, "extender-x.ur.example.") {
			t.Fatalf("the listing began before the measurement zone: %v", listedNames)
		}
		if slices.Contains(listedNames, "zzz.ur.example.") {
			t.Fatalf("the listing ran past the measurement zone: %v", listedNames)
		}
	})
}

// A hosted zone that does not hold the measurement zone is refused before
// any change, and says so.
func TestExtenderMeasurementDnsRefusesAZoneThatDoesNotHoldTheNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{hostedZoneId: testMeasurementOtherZoneId})
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())

		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 0)
		runTestMeasurementDns(t, ctx)
		allowed, denied := env.zone.batches()
		connect.AssertEqual(t, len(allowed)+len(denied), 0)
		connect.AssertEqual(t, env.ca.orderTotal(), 0)

		config, err := controller.EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := openExtenderMeasurementZone(ctx, config); err == nil || !strings.Contains(err.Error(), "does not hold the measurement zone") {
			t.Fatalf("another zone was opened as the measurement zone: %v", err)
		}
	})
}

// Both chains are inert while the block is disabled: nothing is ordered,
// nothing in any zone is read or written, and a stored request keeps waiting.
func TestExtenderMeasurementTasksAreInertWhenDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, "measurement:", "  enabled: false", "  zone_name: "+testMeasurementZone)
		previousApi := newExtenderMeasurementRoute53Api
		newExtenderMeasurementRoute53Api = func(_ *controller.ExtenderConfig) (extenderMeasurementRoute53Api, error) {
			t.Fatalf("a disabled block reached Route 53")
			return nil, nil
		}
		t.Cleanup(func() { newExtenderMeasurementRoute53Api = previousApi })

		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		connect.AssertEqual(t, *runTestMeasurementIssue(t, ctx), ExtenderMeasurementIssueResult{})
		connect.AssertEqual(t, *runTestMeasurementDns(t, ctx), ExtenderMeasurementDnsResult{})
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStatePending)
	})
}

// Records that the extender's poll (A8) read each of its live certificates,
// as the first answer with a certificate's chain does.
func deliverTestMeasurementCerts(t testing.TB, ctx context.Context, extenderId server.Id) {
	t.Helper()
	now := server.NowUtc()
	for _, cert := range model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId) {
		if cert.WithdrawTime == nil && now.Before(cert.NotAfter) {
			model.MarkNetworkExtenderMeasurementCertDelivered(ctx, cert.CertId, now)
		}
	}
}

// Runs the publish drip and reads the MeasurementId of the record it signed
// last for each extender (C10). The drip's smallest batch is larger than the
// extenders a test makes, so each run signs every one of them again.
func runTestMeasurementDrip(t testing.TB, ctx context.Context, env *testMeasurementEnv) map[server.Id][]byte {
	t.Helper()
	runTestExtenderPublish(t, ctx)
	measurementIds := map[server.Id][]byte{}
	for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx) {
		message := &protocol.ExtenderGossipMessage{}
		if err := proto.Unmarshal(publish.Message, message); err != nil {
			t.Fatal(err)
		}
		body, err := connect.NewExtenderRootKeySet(env.rootPublicKey).VerifyRecord(message.GetRecord())
		if err != nil {
			t.Fatal(err)
		}
		measurementIds[publish.ExtenderId] = body.MeasurementId
	}
	return measurementIds
}

// C10 through the drip: a record the publish tick signs for an extender whose
// certificate's name is published (C8), and whose certificate its poll has
// read (A8), carries its id as MeasurementId, from which a reader computes
// its name. One signed while the name is not yet published carries none, and
// so does one signed after its records went missing until a pass publishes
// them again, so no reader looks a name up before it resolves; so does one
// signed before the extender read its certificate, which it serves only from
// then; nor does one for an extender without a certificate.
func TestExtenderPublishSignsTheMeasurementId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		withCert := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, withCert, server.NowUtc())
		withoutCert := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)

		// issued, and its name not yet published: none
		measurementIds := runTestMeasurementDrip(t, ctx, env)
		connect.AssertEqual(t, len(measurementIds[withCert]), 0)
		connect.AssertEqual(t, len(measurementIds[withoutCert]), 0)

		// published, and not yet read by the extender: none
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Published, 1)
		measurementIds = runTestMeasurementDrip(t, ctx, env)
		connect.AssertEqual(t, len(measurementIds[withCert]), 0)

		// read: its id
		deliverTestMeasurementCerts(t, ctx, withCert)
		measurementIds = runTestMeasurementDrip(t, ctx, env)
		connect.AssertEqual(t, bytes.Equal(measurementIds[withCert], withCert.Bytes()), true)
		connect.AssertEqual(t, len(measurementIds[withCert]), 16)
		connect.AssertEqual(t, len(measurementIds[withoutCert]), 0)

		// a reader computes the name the certificate is for
		measurementId, err := connect.IdFromBytes(measurementIds[withCert])
		if err != nil {
			t.Fatal(err)
		}
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, withCert)
		connect.AssertEqual(t, connect.ExtenderMeasurementName(measurementId, testMeasurementZone), certs[0].Names[0])

		// its records go missing: none from the pass that finds them gone
		// until a pass has published them again
		env.zone.remove(env.hostedZoneId, testMeasurementName(withCert))
		useTestMeasurementRoute53(t, &testUnsyncedMeasurementRoute53{testMeasurementRoute53: env.zone})
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Published, 0)
		connect.AssertEqual(t, len(runTestMeasurementDrip(t, ctx, env)[withCert]), 0)
		useTestMeasurementRoute53(t, env.zone)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Published, 1)
		connect.AssertEqual(t, bytes.Equal(runTestMeasurementDrip(t, ctx, env)[withCert], withCert.Bytes()), true)
	})
}

// A renewal is at two thirds of the lifetime (C6), and a failed order or
// revocation backs off from five minutes, doubling, to three days.
func TestExtenderMeasurementRenewTimeAndBackoff(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, extenderMeasurementRenewTime(now, now.Add(90*24*time.Hour)), now.Add(60*24*time.Hour))
	connect.AssertEqual(t, extenderMeasurementRenewTime(now, now.Add(45*24*time.Hour)), now.Add(30*24*time.Hour))

	connect.AssertEqual(t, extenderMeasurementRetryTimeout(0), ExtenderMeasurementFirstRetryTimeout)
	connect.AssertEqual(t, extenderMeasurementRetryTimeout(1), 2*ExtenderMeasurementFirstRetryTimeout)
	connect.AssertEqual(t, extenderMeasurementRetryTimeout(64), ExtenderMeasurementMaxRetryTimeout)
	// a name that keeps failing is tried four times in its first hour, which
	// is under the CA's five failed validations an hour
	firstHourAttempts := 1
	elapsed := time.Duration(0)
	for failureCount := 0; ; failureCount += 1 {
		elapsed += extenderMeasurementRetryTimeout(failureCount)
		if time.Hour <= elapsed {
			break
		}
		firstHourAttempts += 1
	}
	connect.AssertEqual(t, firstHourAttempts, 4)
}

// An owner's opt-out end to end (lag.dev/PLAN.md C4, C9, decision 18). The
// activation withdraws the extender; the next dns pass takes its records out,
// the lag.dev sample (A4) no longer offers it, the next issuance pass revokes
// its certificate, and records the drip signs carry no measurement id. While
// the flag keeps coming nothing is issued for it. A request without the flag
// puts it back, and its certificate, for the same name, is a renewal: it is
// issued past a spent weekly cap.
func TestExtenderMeasurementOptOutAndBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 2})
		now := server.NowUtc()

		optedOut := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		_, request := requestTestMeasurementCert(t, ctx, optedOut, now)
		staying := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		requestTestMeasurementCert(t, ctx, staying, now)
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Names, 2)
		deliverTestMeasurementCerts(t, ctx, optedOut)
		deliverTestMeasurementCerts(t, ctx, staying)

		// the owner opts out, as an activation with the flag does
		connect.AssertEqual(t, model.OptOutNetworkExtenderMeasurement(ctx, testMeasurementExtenderPublicKey(1), controller.ExtenderMeasurementOptOutReason, server.NowUtc()), 1)

		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Names, 1)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(staying) + " A": {"203.0.113.2"},
		})
		config, err := controller.EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		hosts := controller.GetExtenderMeasurementHosts(ctx, config, server.NowUtc())
		connect.AssertEqual(t, len(hosts), 1)
		connect.AssertEqual(t, hosts[0].ExtenderId, staying)

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Revoked, 1)
		connect.AssertEqual(t, result.Issued, 0)
		for _, reason := range env.ca.revokedReasons() {
			connect.AssertEqual(t, reason, 5)
		}

		runTestExtenderPublish(t, ctx)
		measurementIds := map[server.Id][]byte{}
		for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx) {
			message := &protocol.ExtenderGossipMessage{}
			if err := proto.Unmarshal(publish.Message, message); err != nil {
				t.Fatal(err)
			}
			body, err := connect.NewExtenderRootKeySet(env.rootPublicKey).VerifyRecord(message.GetRecord())
			if err != nil {
				t.Fatal(err)
			}
			measurementIds[publish.ExtenderId] = body.MeasurementId
		}
		connect.AssertEqual(t, len(measurementIds[optedOut]), 0)
		connect.AssertEqual(t, bytes.Equal(measurementIds[staying], staying.Bytes()), true)

		// the flag keeps coming: nothing is issued
		connect.AssertEqual(t, model.OptOutNetworkExtenderMeasurement(ctx, testMeasurementExtenderPublicKey(1), controller.ExtenderMeasurementOptOutReason, server.NowUtc()), 0)
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued+result.Renewed+result.Revoked, 0)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, optedOut).State, model.NetworkExtenderMeasurementStateWithdrawn)

		// back in: the same request, without the flag, past the spent cap
		state := model.SetNetworkExtenderMeasurementRequest(ctx, optedOut, request.Csr, request.PublicKey, []string{request.Name}, server.NowUtc())
		connect.AssertEqual(t, state, model.NetworkExtenderMeasurementStatePending)
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Waiting, 0)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, optedOut)
		connect.AssertEqual(t, len(certs), 2)
		connect.AssertEqual(t, certs[1].CapCounted, false)
		connect.AssertEqual(t, certs[1].WithdrawTime == nil, true)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Names, 2)
		assertTestMeasurementForeignRecordSets(t, env, testMeasurementForeignApexRecordSets())
		assertTestMeasurementBatches(t, env)
	})
}

// A8's replacement (lag.dev/PLAN.md A8): a request for the same name under
// another key replaces a pending one in its place in the queue, and no order
// is placed for the old key. With room for one certificate, the extender that
// asked first is still issued first, for its new key.
func TestExtenderMeasurementReplacesAPendingRequestsKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{issueCapPerWeek: 1})
		now := server.NowUtc()

		first := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, first, now.Add(-2*time.Minute))
		second := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		requestTestMeasurementCert(t, ctx, second, now.Add(-time.Minute))

		// the first extender's key is replaced while it waits
		newKey, _ := requestTestMeasurementCert(t, ctx, first, now)

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Waiting, 1)
		connect.AssertEqual(t, env.ca.orderTotal(), 1)
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, first)
		connect.AssertEqual(t, len(certs), 1)
		leaf := verifyTestMeasurementChain(t, env.ca, certs[0].ChainPem, testMeasurementName(first))
		connect.AssertEqual(t, leaf.PublicKey.(*ecdsa.PublicKey).Equal(&newKey.PublicKey), true)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurementCerts(ctx, second)), 0)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, second).State, model.NetworkExtenderMeasurementStatePending)
	})
}

// The zone with no change ever known to be in sync, as when Route 53 throttles
// the reads of a change: a change is made, and dns may not answer with it yet.
type testUnsyncedMeasurementRoute53 struct {
	*testMeasurementRoute53
}

func (self *testUnsyncedMeasurementRoute53) GetChangeWithContext(
	_ aws.Context,
	_ *route53.GetChangeInput,
	_ ...request.Option,
) (*route53.GetChangeOutput, error) {
	return nil, fmt.Errorf("throttled")
}

// Puts `api` in place of the zone for what follows.
func useTestMeasurementRoute53(t testing.TB, api extenderMeasurementRoute53Api) {
	t.Helper()
	previousApi := newExtenderMeasurementRoute53Api
	newExtenderMeasurementRoute53Api = func(_ *controller.ExtenderConfig) (extenderMeasurementRoute53Api, error) {
		return api, nil
	}
	t.Cleanup(func() { newExtenderMeasurementRoute53Api = previousApi })
}

// The extenders the lag.dev sample may offer now (A4).
func testMeasurementOfferedIds(t testing.TB, ctx context.Context) []server.Id {
	t.Helper()
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	extenderIds := []server.Id{}
	for _, host := range controller.GetExtenderMeasurementHosts(ctx, config, server.NowUtc()) {
		if host.PublishTime == nil {
			t.Fatalf("%s is offered unpublished", host.Name)
		}
		extenderIds = append(extenderIds, host.ExtenderId)
	}
	return extenderIds
}

// The dns chain's pending pass.
type testMeasurementDnsTask struct {
	taskId         server.Id
	runAt          time.Time
	maxTimeSeconds int
}

func getTestMeasurementDnsTask(t testing.TB, ctx context.Context) *testMeasurementDnsTask {
	t.Helper()
	var dnsTask *testMeasurementDnsTask
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT task_id, run_at, run_max_time_seconds FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("extender_measurement_dns").String(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				dnsTask = &testMeasurementDnsTask{}
				server.Raise(result.Scan(&dnsTask.taskId, &dnsTask.runAt, &dnsTask.maxTimeSeconds))
			}
		})
	})
	if dnsTask == nil {
		t.Fatalf("no dns pass is pending")
	}
	return dnsTask
}

// Runs the dns chain's pending pass as a worker does, post included.
func runTestMeasurementPendingDnsTask(t testing.TB, ctx context.Context) {
	t.Helper()
	dnsTask := getTestMeasurementDnsTask(t, ctx)
	// a claim admits a run a whole second after its run_at; the test moves
	// the run back rather than sleeping
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE pending_task SET run_at = $2 WHERE task_id = $1`,
			dnsTask.taskId,
			time.Unix(1, 0).UTC(),
		))
	})
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(task.NewTaskTargetWithPost(ExtenderMeasurementDns, ExtenderMeasurementDnsPost))
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != dnsTask.taskId || 0 < len(retried)+len(posts) {
		t.Fatalf("the dns pass did not run: %v %v %v %v", finished, retried, posts, err)
	}
}

// An extender whose certificate was just issued is not offered until its name
// is published (lag.dev/PLAN.md C8, A4): a browser that looked the name up
// before every authoritative server answered for it would be told the name
// does not exist, and its resolver would keep that answer. A name whose
// records are written but not known to be in sync is not offered either, nor
// one whose records went another way until they are written again and in
// sync, nor one that is going.
func TestExtenderMeasurementOffersOnlyPublishedNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		config, err := controller.EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7", "2001:db8::7")
		name := testMeasurementName(extenderId)
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())

		// issued, and not yet published: the reconcile is to publish it, and
		// the sample does not offer it
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
		toPublish := controller.GetExtenderMeasurementHostsToPublish(ctx, config, server.NowUtc())
		connect.AssertEqual(t, len(toPublish), 1)
		connect.AssertEqual(t, toPublish[0].Name, name)
		connect.AssertEqual(t, toPublish[0].PublishTime == nil, true)
		connect.AssertEqual(t, len(testMeasurementOfferedIds(t, ctx)), 0)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].PublishTime == nil, true)

		// written, but Route 53 cannot say the change is in sync, and dns
		// does not answer with it yet: still not offered
		useTestMeasurementRoute53(t, &testUnsyncedMeasurementRoute53{testMeasurementRoute53: env.zone})
		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Upserted, 2)
		connect.AssertEqual(t, dnsResult.Published, 0)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			name + " A":    {"203.0.113.7"},
			name + " AAAA": {"2001:db8::7"},
		})
		connect.AssertEqual(t, len(env.zone.servedAddresses(name)), 0)
		connect.AssertEqual(t, len(testMeasurementOfferedIds(t, ctx)), 0)

		// the next pass writes the records again and waits for that change
		// to be in sync: published, answered everywhere, and offered
		useTestMeasurementRoute53(t, env.zone)
		dnsResult = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Upserted, 2)
		connect.AssertEqual(t, dnsResult.Published, 1)
		connect.AssertEqual(t, env.zone.servedAddresses(name), map[string][]string{
			route53.RRTypeA:    {"203.0.113.7"},
			route53.RRTypeAaaa: {"2001:db8::7"},
		})
		connect.AssertEqual(t, testMeasurementOfferedIds(t, ctx), []server.Id{extenderId})
		publishTime := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].PublishTime
		if publishTime == nil {
			t.Fatalf("the certificate does not say its name is published")
		}

		// a published name that matches is left as it is
		allowedBefore, _ := env.zone.batches()
		dnsResult = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Upserted+dnsResult.Deleted+dnsResult.Published, 0)
		allowedAfter, _ := env.zone.batches()
		connect.AssertEqual(t, len(allowedAfter), len(allowedBefore))
		connect.AssertEqual(t, *model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].PublishTime, *publishTime)

		// its records go another way: the next pass stops offering it before
		// writing them again, and offers it again once that is in sync
		env.zone.remove(env.hostedZoneId, name)
		useTestMeasurementRoute53(t, &testUnsyncedMeasurementRoute53{testMeasurementRoute53: env.zone})
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Upserted, 2)
		connect.AssertEqual(t, len(testMeasurementOfferedIds(t, ctx)), 0)
		useTestMeasurementRoute53(t, env.zone)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Published, 1)
		connect.AssertEqual(t, testMeasurementOfferedIds(t, ctx), []server.Id{extenderId})

		// the owner opts out: the name stops being published, and its records go
		connect.AssertEqual(t, model.OptOutNetworkExtenderMeasurement(ctx, testMeasurementExtenderPublicKey(1), controller.ExtenderMeasurementOptOutReason, server.NowUtc()), 1)
		dnsResult = runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Deleted, 2)
		connect.AssertEqual(t, dnsResult.Names, 0)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].PublishTime == nil, true)
		connect.AssertEqual(t, len(testMeasurementOfferedIds(t, ctx)), 0)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{})
		assertTestMeasurementForeignRecordSets(t, env, testMeasurementForeignApexRecordSets())
		assertTestMeasurementBatches(t, env)
	})
}

// An issuance wakes the dns chain (lag.dev/PLAN.md C8): the pass that
// publishes the new name is due at once rather than at the chain's next
// minute, the sample offers the extender as soon as it has run, and its post
// arms the next pass a minute out as usual. A renewal keeps the name offered
// throughout, wakes the chain too, and that pass marks the renewal published.
func TestExtenderMeasurementPublishesRightAfterIssuance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		env.zone.pollsUntilInsync = 2
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		name := testMeasurementName(extenderId)
		requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())

		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)
		dnsTask := getTestMeasurementDnsTask(t, ctx)
		if server.NowUtc().Before(dnsTask.runAt) {
			t.Fatalf("the dns pass runs at %s, after the issuance", dnsTask.runAt)
		}
		if dnsTask.maxTimeSeconds < int(ExtenderMeasurementInsyncTimeout/time.Second) {
			t.Fatalf("the dns pass may run %d s, less than a wait for sync", dnsTask.maxTimeSeconds)
		}
		connect.AssertEqual(t, len(testMeasurementOfferedIds(t, ctx)), 0)

		runTestMeasurementPendingDnsTask(t, ctx)
		connect.AssertEqual(t, testMeasurementOfferedIds(t, ctx), []server.Id{extenderId})
		connect.AssertEqual(t, env.zone.servedAddresses(name), map[string][]string{
			route53.RRTypeA: {"203.0.113.7"},
		})
		nextTask := getTestMeasurementDnsTask(t, ctx)
		if nextTask.runAt.Before(server.NowUtc().Add(ExtenderMeasurementDnsTimeout / 2)) {
			t.Fatalf("the next dns pass runs at %s, not a minute out", nextTask.runAt)
		}

		// a renewal: offered throughout, since the name stays published
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		publishTime := certs[0].PublishTime
		if publishTime == nil {
			t.Fatalf("the certificate does not say its name is published")
		}
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, certs[0].CertId, server.NowUtc().Add(-time.Second))
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Renewed, 1)
		connect.AssertEqual(t, testMeasurementOfferedIds(t, ctx), []server.Id{extenderId})
		dnsTask = getTestMeasurementDnsTask(t, ctx)
		if server.NowUtc().Before(dnsTask.runAt) {
			t.Fatalf("the dns pass runs at %s, after the renewal", dnsTask.runAt)
		}
		runTestMeasurementPendingDnsTask(t, ctx)
		certs = model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 2)
		connect.AssertEqual(t, *certs[0].PublishTime, *publishTime)
		if certs[1].PublishTime == nil {
			t.Fatalf("the renewal does not say its name is published")
		}
		connect.AssertEqual(t, testMeasurementOfferedIds(t, ctx), []server.Id{extenderId})
		assertTestMeasurementBatches(t, env)
	})
}
