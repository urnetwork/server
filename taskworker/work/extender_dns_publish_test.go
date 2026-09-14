package work

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The geo dns half of the publish tick (connect/EXTENDER.md C5).
//
// Two seams make this deterministic: the sample source is seeded, so the same
// rows produce the same sets on every run, and the publisher is a fake that
// keeps the zone in memory, so a test can assert the batch a tick sends and
// the state it leaves behind. The Route 53 implementation is driven through a
// fake of the two api calls it makes, which is where the set identifiers and
// the geolocation fields are pinned.

const (
	testExtenderDnsRecordName     = "extender." + testExtenderWorkNetworkHost
	testExtenderDnsHostedZoneId   = "Z0EXAMPLEZONEID"
	testExtenderDnsHostedZoneName = testExtenderWorkNetworkHost
	testExtenderDnsNamedZoneId    = "Z0EXAMPLENAMEDZONE"
	testExtenderDnsTtl            = 90
	testExtenderDnsSampleCount    = 3
)

// The dns block the tick tests run with.
func testExtenderDnsConfigYaml() []string {
	return []string{
		"dns:",
		"  enabled: true",
		"  hosted_zone_id: " + testExtenderDnsHostedZoneId,
		"  record_name: " + testExtenderDnsRecordName,
		fmt.Sprintf("  ttl: %d", testExtenderDnsTtl),
		fmt.Sprintf("  sample_count: %d", testExtenderDnsSampleCount),
	}
}

// One apply as the fake publisher saw it.
type testExtenderDnsBatch struct {
	recordName            string
	ttl                   int
	upsertSets            []*extenderDnsRecordSet
	deletedSetIdentifiers []string
}

func (self *testExtenderDnsBatch) setIdentifiers() []string {
	setIdentifiers := []string{}
	for _, upsertSet := range self.upsertSets {
		setIdentifiers = append(setIdentifiers, upsertSet.setIdentifier())
	}
	return setIdentifiers
}

func (self *testExtenderDnsBatch) upsertSet(setIdentifier string) *extenderDnsRecordSet {
	for _, upsertSet := range self.upsertSets {
		if upsertSet.setIdentifier() == setIdentifier {
			return upsertSet
		}
	}
	return nil
}

// A publisher that keeps the zone in memory.
//
// It decides what to delete with the same helper the Route 53 publisher uses,
// so the two cannot disagree about what a tick leaves behind. Used from the
// test goroutine only, as the tick is synchronous.
type testExtenderDnsPublisher struct {
	batches            []*testExtenderDnsBatch
	zoneSetIdentifiers []string
	zoneSets           map[string]*extenderDnsRecordSet
	err                error
}

func newTestExtenderDnsPublisher() *testExtenderDnsPublisher {
	return &testExtenderDnsPublisher{
		batches:            []*testExtenderDnsBatch{},
		zoneSetIdentifiers: []string{},
		zoneSets:           map[string]*extenderDnsRecordSet{},
	}
}

func (self *testExtenderDnsPublisher) publish(
	_ context.Context,
	recordName string,
	ttl int,
	desiredSets []*extenderDnsRecordSet,
) error {
	batch := &testExtenderDnsBatch{
		recordName:            recordName,
		ttl:                   ttl,
		upsertSets:            desiredSets,
		deletedSetIdentifiers: extenderDnsDeletedSetIdentifiers(self.zoneSetIdentifiers, desiredSets),
	}
	self.batches = append(self.batches, batch)
	if self.err != nil {
		// one route 53 batch is atomic, so a failed apply leaves the zone as
		// it was
		return self.err
	}

	for _, setIdentifier := range batch.deletedSetIdentifiers {
		delete(self.zoneSets, setIdentifier)
		self.zoneSetIdentifiers = slices.DeleteFunc(
			self.zoneSetIdentifiers,
			func(zoneSetIdentifier string) bool {
				return zoneSetIdentifier == setIdentifier
			},
		)
	}
	for _, desiredSet := range desiredSets {
		setIdentifier := desiredSet.setIdentifier()
		if _, ok := self.zoneSets[setIdentifier]; !ok {
			self.zoneSetIdentifiers = append(self.zoneSetIdentifiers, setIdentifier)
		}
		self.zoneSets[setIdentifier] = desiredSet
	}
	return nil
}

// The one batch a tick was expected to send.
func (self *testExtenderDnsPublisher) onlyBatch(t testing.TB) *testExtenderDnsBatch {
	t.Helper()
	if len(self.batches) != 1 {
		t.Fatalf("the tick sent %d batches, want exactly one", len(self.batches))
	}
	return self.batches[0]
}

// Replaces the publisher and the sample source for one test.
//
// The seed is fixed, so the same rows draw the same sets on every run; without
// it every assertion about which addresses a set holds would be a coin flip.
func stubExtenderDnsPublisher(t testing.TB) *testExtenderDnsPublisher {
	t.Helper()
	publisher := newTestExtenderDnsPublisher()
	previousPublisher := newExtenderDnsPublisher
	newExtenderDnsPublisher = func(_ *controller.ExtenderConfig) (extenderDnsPublisher, error) {
		return publisher, nil
	}
	previousRandom := extenderDnsRandom
	extenderDnsRandom = func() *mathrand.Rand {
		return mathrand.New(mathrand.NewPCG(7, 11))
	}
	t.Cleanup(func() {
		newExtenderDnsPublisher = previousPublisher
		extenderDnsRandom = previousRandom
	})
	return publisher
}

// Synthetic documentation addresses only (RFC 5737, RFC 3849), one per index.
func testExtenderDnsIp(ipVersion int, index int) string {
	if ipVersion == 6 {
		return fmt.Sprintf("2001:db8:1::%x", index)
	}
	return fmt.Sprintf("198.51.100.%d", index)
}

// The rows a sampler sees, without a database behind them.
func testExtenderDnsAddresses(
	countryCode string,
	ipVersion int,
	indexes ...int,
) []*model.NetworkExtenderDnsAddress {
	addresses := []*model.NetworkExtenderDnsAddress{}
	for _, index := range indexes {
		addresses = append(addresses, &model.NetworkExtenderDnsAddress{
			ExtenderId:  server.NewId(),
			IpVersion:   ipVersion,
			Ip:          netip.MustParseAddr(testExtenderDnsIp(ipVersion, index)),
			CountryCode: countryCode,
		})
	}
	return addresses
}

func testExtenderDnsIps(ipVersion int, indexes ...int) []string {
	ips := []string{}
	for _, index := range indexes {
		ips = append(ips, testExtenderDnsIp(ipVersion, index))
	}
	return ips
}

// Draws the sets of a sampler run with the fixed seed.
func sampleTestExtenderDnsRecordSets(
	addresses []*model.NetworkExtenderDnsAddress,
	sampleCount int,
) []*extenderDnsRecordSet {
	return sampleExtenderDnsRecordSets(
		addresses,
		sampleCount,
		mathrand.New(mathrand.NewPCG(7, 11)),
	)
}

// Fails unless the set holds sampleCount distinct addresses, every one of them
// from the pool given.
func assertTestExtenderDnsSet(
	t testing.TB,
	desiredSets []*extenderDnsRecordSet,
	setIdentifier string,
	count int,
	poolIps []string,
) *extenderDnsRecordSet {
	t.Helper()
	var found *extenderDnsRecordSet
	for _, desiredSet := range desiredSets {
		if desiredSet.setIdentifier() == setIdentifier {
			found = desiredSet
		}
	}
	if found == nil {
		t.Fatalf("there is no %s set", setIdentifier)
	}
	if len(found.ips) != count {
		t.Fatalf("%s holds %d addresses, want %d", setIdentifier, len(found.ips), count)
	}
	for i, ip := range found.ips {
		if slices.Contains(found.ips[0:i], ip) {
			t.Fatalf("%s holds %s twice", setIdentifier, ip)
		}
		if !slices.Contains(poolIps, ip) {
			t.Fatalf("%s holds %s, which is not in its pool", setIdentifier, ip)
		}
	}
	return found
}

// A continent with more addresses than the sample takes only its own: the
// whole point of the continent sets is that a client is answered with
// addresses near it whenever there are any.
func TestExtenderDnsSampleTakesTheContinentsOwnAddresses(t *testing.T) {
	addresses := slices.Concat(
		testExtenderDnsAddresses("DE", 4, 1, 2, 3, 4, 5),
		testExtenderDnsAddresses("US", 4, 6, 7, 8, 9),
	)
	desiredSets := sampleTestExtenderDnsRecordSets(addresses, testExtenderDnsSampleCount)

	connect.AssertEqual(t, len(desiredSets), 3)
	assertTestExtenderDnsSet(
		t,
		desiredSets,
		"extender-EU-A",
		testExtenderDnsSampleCount,
		testExtenderDnsIps(4, 1, 2, 3, 4, 5),
	)
	assertTestExtenderDnsSet(
		t,
		desiredSets,
		"extender-NA-A",
		testExtenderDnsSampleCount,
		testExtenderDnsIps(4, 6, 7, 8, 9),
	)
	assertTestExtenderDnsSet(
		t,
		desiredSets,
		"extender-default-A",
		testExtenderDnsSampleCount,
		testExtenderDnsIps(4, 1, 2, 3, 4, 5, 6, 7, 8, 9),
	)
}

// A continent short of the sample is filled from the global pool rather than
// published half empty: a set of one address is a single point of failure for
// the whole continent, and no address may appear in it twice.
func TestExtenderDnsSampleFillsAShortContinentFromTheGlobalPool(t *testing.T) {
	addresses := slices.Concat(
		testExtenderDnsAddresses("DE", 4, 1),
		testExtenderDnsAddresses("US", 4, 6, 7, 8, 9, 10),
	)
	desiredSets := sampleTestExtenderDnsRecordSets(addresses, testExtenderDnsSampleCount)

	europe := assertTestExtenderDnsSet(
		t,
		desiredSets,
		"extender-EU-A",
		testExtenderDnsSampleCount,
		testExtenderDnsIps(4, 1, 6, 7, 8, 9, 10),
	)
	if !slices.Contains(europe.ips, testExtenderDnsIp(4, 1)) {
		t.Fatalf("the filled set %v dropped the continent's own address", europe.ips)
	}
}

// A family with no active address anywhere gets no sets at all. An empty set
// resolves to nothing, which is worse than the absent record a client falls
// through on.
func TestExtenderDnsSampleSkipsAFamilyWithNoAddresses(t *testing.T) {
	desiredSets := sampleTestExtenderDnsRecordSets(
		testExtenderDnsAddresses("DE", 6, 1, 2),
		testExtenderDnsSampleCount,
	)

	connect.AssertEqual(t, len(desiredSets), 2)
	for _, desiredSet := range desiredSets {
		if desiredSet.recordType() != route53.RRTypeAaaa {
			t.Fatalf("the %s set has no addresses to hold", desiredSet.setIdentifier())
		}
	}
	assertTestExtenderDnsSet(t, desiredSets, "extender-EU-AAAA", 2, testExtenderDnsIps(6, 1, 2))
	assertTestExtenderDnsSet(t, desiredSets, "extender-default-AAAA", 2, testExtenderDnsIps(6, 1, 2))
}

// The default set answers every location no continent set covers, so it is
// drawn from the whole world rather than from any one continent.
func TestExtenderDnsSampleDefaultSetIsGlobal(t *testing.T) {
	addresses := slices.Concat(
		testExtenderDnsAddresses("DE", 4, 1, 2),
		testExtenderDnsAddresses("BR", 4, 3, 4),
	)
	// the sample is larger than the population, so the default set is the
	// whole of it and the assertion does not depend on the draw
	desiredSets := sampleTestExtenderDnsRecordSets(addresses, 8)

	global := assertTestExtenderDnsSet(
		t,
		desiredSets,
		"extender-default-A",
		4,
		testExtenderDnsIps(4, 1, 2, 3, 4),
	)
	for _, ip := range testExtenderDnsIps(4, 1, 2, 3, 4) {
		if !slices.Contains(global.ips, ip) {
			t.Fatalf("the default set %v is missing %s", global.ips, ip)
		}
	}
}

// An extender whose country has no continent -- unset, or a code no table
// carries -- is in the global pool only. Guessing it into a continent would
// advertise it as local to somewhere it may be nowhere near.
func TestExtenderDnsSampleKeepsAnUnknownCountryOutOfTheContinents(t *testing.T) {
	addresses := slices.Concat(
		testExtenderDnsAddresses("DE", 4, 1, 2, 3),
		testExtenderDnsAddresses("", 4, 4),
		testExtenderDnsAddresses("ZZ", 4, 5),
	)
	desiredSets := sampleTestExtenderDnsRecordSets(addresses, testExtenderDnsSampleCount)

	// europe is full of its own, so nothing without a continent reaches it
	connect.AssertEqual(t, len(desiredSets), 2)
	europe := assertTestExtenderDnsSet(
		t,
		desiredSets,
		"extender-EU-A",
		testExtenderDnsSampleCount,
		testExtenderDnsIps(4, 1, 2, 3),
	)
	for _, ip := range testExtenderDnsIps(4, 1, 2, 3) {
		if !slices.Contains(europe.ips, ip) {
			t.Fatalf("the continent set %v is missing its own %s", europe.ips, ip)
		}
	}

	// and they are in the global pool, which is what the default set answers
	// with
	global := assertTestExtenderDnsSet(
		t,
		sampleTestExtenderDnsRecordSets(addresses, 8),
		"extender-default-A",
		5,
		testExtenderDnsIps(4, 1, 2, 3, 4, 5),
	)
	for _, ip := range testExtenderDnsIps(4, 4, 5) {
		if !slices.Contains(global.ips, ip) {
			t.Fatalf("the default set %v is missing the countryless %s", global.ips, ip)
		}
	}
}

// Successive ticks redraw. Without the rotation a ttl of a minute would pin
// every client in a continent to the same few addresses until one of them
// failed, which is the load concentration the sample exists to avoid.
func TestExtenderDnsSampleRotatesBetweenTicks(t *testing.T) {
	addresses := testExtenderDnsAddresses("DE", 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	first := sampleExtenderDnsRecordSets(
		addresses,
		testExtenderDnsSampleCount,
		mathrand.New(mathrand.NewPCG(7, 11)),
	)
	second := sampleExtenderDnsRecordSets(
		addresses,
		testExtenderDnsSampleCount,
		mathrand.New(mathrand.NewPCG(13, 17)),
	)
	connect.AssertNotEqual(t, first[0].ips, second[0].ips)

	// and the same source draws the same sets, which is what makes every
	// assertion here reproducible
	repeated := sampleExtenderDnsRecordSets(
		addresses,
		testExtenderDnsSampleCount,
		mathrand.New(mathrand.NewPCG(7, 11)),
	)
	connect.AssertEqual(t, first[0].ips, repeated[0].ips)
}

// The Route 53 calls, delivered one record per page so the pager's early stop
// is observable.
//
// Used from the test goroutine only, as every tick here is synchronous.
type testRoute53Api struct {
	hostedZones        []*route53.HostedZone
	hostedZonesErr     error
	existingRecordSets []*route53.ResourceRecordSet
	deliveredPageCount int
	hostedZoneInputs   []*route53.ListHostedZonesByNameInput
	listInputs         []*route53.ListResourceRecordSetsInput
	changeInputs       []*route53.ChangeResourceRecordSetsInput
}

// Route 53 positions the listing at the name given and orders zones by name,
// so a zone before it is not in the answer at all.
func (self *testRoute53Api) ListHostedZonesByNameWithContext(
	_ aws.Context,
	input *route53.ListHostedZonesByNameInput,
	_ ...request.Option,
) (*route53.ListHostedZonesByNameOutput, error) {
	self.hostedZoneInputs = append(self.hostedZoneInputs, input)
	if self.hostedZonesErr != nil {
		return nil, self.hostedZonesErr
	}
	dnsName := extenderDnsFqdn(aws.StringValue(input.DNSName))
	hostedZones := []*route53.HostedZone{}
	for _, hostedZone := range self.hostedZones {
		if extenderDnsFqdn(aws.StringValue(hostedZone.Name)) < dnsName {
			continue
		}
		hostedZones = append(hostedZones, hostedZone)
	}
	return &route53.ListHostedZonesByNameOutput{
		HostedZones: hostedZones,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (self *testRoute53Api) ListResourceRecordSetsPagesWithContext(
	_ aws.Context,
	input *route53.ListResourceRecordSetsInput,
	fn func(*route53.ListResourceRecordSetsOutput, bool) bool,
	_ ...request.Option,
) error {
	self.listInputs = append(self.listInputs, input)
	for i, recordSet := range self.existingRecordSets {
		self.deliveredPageCount += 1
		lastPage := i == len(self.existingRecordSets)-1
		output := &route53.ListResourceRecordSetsOutput{
			ResourceRecordSets: []*route53.ResourceRecordSet{recordSet},
		}
		if !fn(output, lastPage) {
			return nil
		}
	}
	return nil
}

func (self *testRoute53Api) ChangeResourceRecordSetsWithContext(
	_ aws.Context,
	input *route53.ChangeResourceRecordSetsInput,
	_ ...request.Option,
) (*route53.ChangeResourceRecordSetsOutput, error) {
	self.changeInputs = append(self.changeInputs, input)
	return &route53.ChangeResourceRecordSetsOutput{}, nil
}

func testRoute53RecordSet(
	name string,
	setIdentifier string,
	recordType string,
	ip string,
) *route53.ResourceRecordSet {
	return &route53.ResourceRecordSet{
		Name:          aws.String(name),
		Type:          aws.String(recordType),
		TTL:           aws.Int64(testExtenderDnsTtl),
		SetIdentifier: aws.String(setIdentifier),
		ResourceRecords: []*route53.ResourceRecord{
			{Value: aws.String(ip)},
		},
	}
}

// One apply is one batch: every desired set upserted, every set of ours that
// is no longer desired deleted, and nothing else touched.
func TestRoute53ExtenderDnsPublishesOneBatch(t *testing.T) {
	name := testExtenderDnsRecordName + "."
	staleSet := testRoute53RecordSet(name, "extender-OC-A", route53.RRTypeA, "198.51.100.90")
	manualSet := testRoute53RecordSet(name, "operator-A", route53.RRTypeA, "198.51.100.91")
	api := &testRoute53Api{
		existingRecordSets: []*route53.ResourceRecordSet{
			testRoute53RecordSet(name, "extender-EU-A", route53.RRTypeA, "198.51.100.92"),
			staleSet,
			manualSet,
			// another name, which the zone orders after ours
			testRoute53RecordSet("other."+testExtenderWorkNetworkHost+".", "extender-EU-A", route53.RRTypeA, "198.51.100.93"),
			testRoute53RecordSet("other."+testExtenderWorkNetworkHost+".", "extender-NA-A", route53.RRTypeA, "198.51.100.94"),
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:          api,
		hostedZoneId: testExtenderDnsHostedZoneId,
	}

	desiredSets := []*extenderDnsRecordSet{
		{continentCode: "EU", ipVersion: 4, ips: testExtenderDnsIps(4, 1, 2)},
		{ipVersion: 6, ips: testExtenderDnsIps(6, 1)},
	}
	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		desiredSets,
	); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// the listing starts at the record name and stops at the first other name,
	// rather than paging through a zone that may hold thousands of records
	connect.AssertEqual(t, len(api.listInputs), 1)
	connect.AssertEqual(t, aws.StringValue(api.listInputs[0].HostedZoneId), testExtenderDnsHostedZoneId)
	connect.AssertEqual(t, aws.StringValue(api.listInputs[0].StartRecordName), name)
	connect.AssertEqual(t, api.deliveredPageCount, 4)

	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(t, aws.StringValue(api.changeInputs[0].HostedZoneId), testExtenderDnsHostedZoneId)
	changes := api.changeInputs[0].ChangeBatch.Changes
	connect.AssertEqual(t, len(changes), 3)

	// the desired sets, with the geolocation and identifier of C5
	connect.AssertEqual(t, aws.StringValue(changes[0].Action), route53.ChangeActionUpsert)
	europe := changes[0].ResourceRecordSet
	connect.AssertEqual(t, aws.StringValue(europe.Name), name)
	connect.AssertEqual(t, aws.StringValue(europe.Type), route53.RRTypeA)
	connect.AssertEqual(t, aws.Int64Value(europe.TTL), int64(testExtenderDnsTtl))
	connect.AssertEqual(t, aws.StringValue(europe.SetIdentifier), "extender-EU-A")
	connect.AssertEqual(t, aws.StringValue(europe.GeoLocation.ContinentCode), "EU")
	connect.AssertEqual(t, europe.GeoLocation.CountryCode, nil)
	// no health check: an address is in the set because the uptime probes say
	// it answers
	connect.AssertEqual(t, europe.HealthCheckId, nil)
	europeIps := []string{}
	for _, resourceRecord := range europe.ResourceRecords {
		europeIps = append(europeIps, aws.StringValue(resourceRecord.Value))
	}
	connect.AssertEqual(t, europeIps, testExtenderDnsIps(4, 1, 2))

	connect.AssertEqual(t, aws.StringValue(changes[1].Action), route53.ChangeActionUpsert)
	global := changes[1].ResourceRecordSet
	connect.AssertEqual(t, aws.StringValue(global.Type), route53.RRTypeAaaa)
	connect.AssertEqual(t, aws.StringValue(global.SetIdentifier), "extender-default-AAAA")
	// the default set is the `*` country rather than a continent
	connect.AssertEqual(t, aws.StringValue(global.GeoLocation.CountryCode), "*")
	connect.AssertEqual(t, global.GeoLocation.ContinentCode, nil)

	// the continent that went empty is deleted, carrying the set exactly as it
	// exists, which is what route 53 requires of a delete
	connect.AssertEqual(t, aws.StringValue(changes[2].Action), route53.ChangeActionDelete)
	if changes[2].ResourceRecordSet != staleSet {
		t.Fatalf("the delete does not carry the existing set")
	}
	// and a record an operator put at the same name by hand is not ours to
	// delete
	for _, change := range changes {
		if change.ResourceRecordSet == manualSet {
			t.Fatalf("a record that is not ours was changed")
		}
	}
}

// The first tick after a deployment: the zone holds no set at the record name
// at all, so every set the sample wants is created and the batch is an upsert
// per set with no delete in it. A neighbouring name carrying our own prefix is
// still not ours -- only the name being published is.
func TestRoute53ExtenderDnsCreatesEverySetInAnEmptyZone(t *testing.T) {
	name := testExtenderDnsRecordName + "."
	otherName := "other." + testExtenderWorkNetworkHost + "."
	api := &testRoute53Api{
		existingRecordSets: []*route53.ResourceRecordSet{
			testRoute53RecordSet(otherName, "extender-EU-A", route53.RRTypeA, "198.51.100.90"),
			testRoute53RecordSet(otherName, "extender-default-A", route53.RRTypeA, "198.51.100.91"),
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:          api,
		hostedZoneId: testExtenderDnsHostedZoneId,
	}

	// a sampled state rather than a hand written one: two continents in v4 and
	// one in v6, so the batch carries continent sets and the default set of
	// both families
	desiredSets := sampleTestExtenderDnsRecordSets(
		slices.Concat(
			testExtenderDnsAddresses("DE", 4, 1, 2),
			testExtenderDnsAddresses("US", 4, 3, 4),
			testExtenderDnsAddresses("DE", 6, 1, 2),
		),
		testExtenderDnsSampleCount,
	)
	setIdentifiers := []string{}
	for _, desiredSet := range desiredSets {
		setIdentifiers = append(setIdentifiers, desiredSet.setIdentifier())
	}
	connect.AssertEqual(t, setIdentifiers, []string{
		"extender-EU-A",
		"extender-NA-A",
		"extender-default-A",
		"extender-EU-AAAA",
		"extender-default-AAAA",
	})

	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		desiredSets,
	); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// the listing stops at the first record of another name, so nothing of the
	// neighbour is read as a set of ours
	connect.AssertEqual(t, len(api.listInputs), 1)
	connect.AssertEqual(t, aws.StringValue(api.listInputs[0].HostedZoneId), testExtenderDnsHostedZoneId)
	connect.AssertEqual(t, aws.StringValue(api.listInputs[0].StartRecordName), name)
	connect.AssertEqual(t, api.deliveredPageCount, 1)

	// one batch, one upsert per desired set in the order the sample drew them,
	// and nothing to delete because the zone held nothing of ours
	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(t, aws.StringValue(api.changeInputs[0].HostedZoneId), testExtenderDnsHostedZoneId)
	changes := api.changeInputs[0].ChangeBatch.Changes
	connect.AssertEqual(t, len(changes), len(desiredSets))
	for i, change := range changes {
		desiredSet := desiredSets[i]
		connect.AssertEqual(t, aws.StringValue(change.Action), route53.ChangeActionUpsert)
		recordSet := change.ResourceRecordSet
		connect.AssertEqual(t, aws.StringValue(recordSet.Name), name)
		connect.AssertEqual(t, aws.StringValue(recordSet.Type), desiredSet.recordType())
		connect.AssertEqual(t, aws.Int64Value(recordSet.TTL), int64(testExtenderDnsTtl))
		connect.AssertEqual(t, aws.StringValue(recordSet.SetIdentifier), desiredSet.setIdentifier())
		if desiredSet.continentCode == "" {
			// the default set is the `*` country rather than a continent
			connect.AssertEqual(t, aws.StringValue(recordSet.GeoLocation.CountryCode), "*")
			connect.AssertEqual(t, recordSet.GeoLocation.ContinentCode, nil)
		} else {
			connect.AssertEqual(
				t,
				aws.StringValue(recordSet.GeoLocation.ContinentCode),
				desiredSet.continentCode,
			)
			connect.AssertEqual(t, recordSet.GeoLocation.CountryCode, nil)
		}
		ips := []string{}
		for _, resourceRecord := range recordSet.ResourceRecords {
			ips = append(ips, aws.StringValue(resourceRecord.Value))
		}
		connect.AssertEqual(t, ips, desiredSet.ips)
	}
}

// An operator with no active extenders and nothing of ours in the zone writes
// nothing at all: an empty change batch is rejected by the api.
func TestRoute53ExtenderDnsWritesNothingWithoutChanges(t *testing.T) {
	api := &testRoute53Api{
		existingRecordSets: []*route53.ResourceRecordSet{
			testRoute53RecordSet(
				testExtenderDnsRecordName+".",
				"operator-A",
				route53.RRTypeA,
				"198.51.100.91",
			),
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:          api,
		hostedZoneId: testExtenderDnsHostedZoneId,
	}

	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		[]*extenderDnsRecordSet{},
	); err != nil {
		t.Fatalf("publish: %v", err)
	}
	connect.AssertEqual(t, len(api.changeInputs), 0)
}

// One zone as the api returns it: the id carries the `/hostedzone/` prefix
// every answer carries and no call takes.
func testRoute53HostedZone(name string, hostedZoneId string) *route53.HostedZone {
	return &route53.HostedZone{
		Name: aws.String(name),
		Id:   aws.String("/hostedzone/" + hostedZoneId),
	}
}

// Drops the zone ids resolved by an earlier test, so a test that counts
// resolutions counts its own.
func resetTestExtenderDnsHostedZoneIds(t testing.TB) {
	t.Helper()
	Testing_ResetExtenderDnsHostedZoneIds()
	t.Cleanup(Testing_ResetExtenderDnsHostedZoneIds)
}

// A zone configured by name is resolved to its id, and the id every later call
// carries is the bare one rather than the `/hostedzone/` path the answer holds.
// Only a zone whose name matches exactly is a match: a zone under it shares the
// suffix and is a different zone entirely.
func TestRoute53ExtenderDnsResolvesTheZoneByName(t *testing.T) {
	resetTestExtenderDnsHostedZoneIds(t)

	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderDnsHostedZoneName+".", testExtenderDnsNamedZoneId),
			testRoute53HostedZone("sub."+testExtenderDnsHostedZoneName+".", "Z0EXAMPLESUBZONE"),
			testRoute53HostedZone("zz.example.", "Z0EXAMPLEOTHERZONE"),
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:            api,
		hostedZoneName: testExtenderDnsHostedZoneName,
	}

	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		[]*extenderDnsRecordSet{{ipVersion: 4, ips: testExtenderDnsIps(4, 1)}},
	); err != nil {
		t.Fatalf("publish: %v", err)
	}

	connect.AssertEqual(t, len(api.hostedZoneInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.hostedZoneInputs[0].DNSName),
		testExtenderDnsHostedZoneName+".",
	)
	connect.AssertEqual(t, len(api.listInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.listInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)
	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)
}

// A configured id is the zone, whatever the name says. It is the escape hatch
// for an account with two zones of one name, so it must not cost a resolution
// and must not be second-guessed.
func TestRoute53ExtenderDnsZoneIdOverridesTheName(t *testing.T) {
	resetTestExtenderDnsHostedZoneIds(t)

	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderDnsHostedZoneName+".", testExtenderDnsNamedZoneId),
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:            api,
		hostedZoneId:   testExtenderDnsHostedZoneId,
		hostedZoneName: testExtenderDnsHostedZoneName,
	}

	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		[]*extenderDnsRecordSet{{ipVersion: 4, ips: testExtenderDnsIps(4, 1)}},
	); err != nil {
		t.Fatalf("publish: %v", err)
	}

	connect.AssertEqual(t, len(api.hostedZoneInputs), 0)
	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[0].HostedZoneId),
		testExtenderDnsHostedZoneId,
	)
}

// A name that does not resolve to exactly one zone writes nothing. Guessing
// would publish the record into a zone nobody resolves, which looks exactly
// like a working publisher until someone asks why the name is dead.
func TestRoute53ExtenderDnsRefusesAZoneItCannotName(t *testing.T) {
	resetTestExtenderDnsHostedZoneIds(t)

	cases := []struct {
		hostedZones  []*route53.HostedZone
		errSubstring string
	}{
		{
			hostedZones: []*route53.HostedZone{
				testRoute53HostedZone("zz.example.", "Z0EXAMPLEOTHERZONE"),
			},
			errSubstring: "there is no hosted zone named " + testExtenderDnsHostedZoneName + ".",
		},
		{
			// a public and a private zone of the same name
			hostedZones: []*route53.HostedZone{
				testRoute53HostedZone(testExtenderDnsHostedZoneName+".", testExtenderDnsNamedZoneId),
				testRoute53HostedZone(testExtenderDnsHostedZoneName+".", "Z0EXAMPLEPRIVATEZONE"),
			},
			errSubstring: "there are 2 hosted zones named " + testExtenderDnsHostedZoneName + ".",
		},
	}
	for _, c := range cases {
		api := &testRoute53Api{hostedZones: c.hostedZones}
		publisher := &route53ExtenderDnsPublisher{
			api:            api,
			hostedZoneName: testExtenderDnsHostedZoneName,
		}
		err := publisher.publish(
			context.Background(),
			testExtenderDnsRecordName,
			testExtenderDnsTtl,
			[]*extenderDnsRecordSet{{ipVersion: 4, ips: testExtenderDnsIps(4, 1)}},
		)
		if err == nil {
			t.Errorf("%v must not resolve to a zone", c.hostedZones)
		} else if !strings.Contains(err.Error(), c.errSubstring) {
			t.Errorf("publish error %q does not say %q", err, c.errSubstring)
		}
		// nothing was listed and nothing was written
		connect.AssertEqual(t, len(api.listInputs), 0)
		connect.AssertEqual(t, len(api.changeInputs), 0)
	}
}

// The zone of a name does not change while the process runs, so it is resolved
// once. A lookup on every tick would spend an api call every ten minutes to
// learn the same id.
func TestRoute53ExtenderDnsReusesTheResolvedZoneId(t *testing.T) {
	resetTestExtenderDnsHostedZoneIds(t)

	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderDnsHostedZoneName+".", testExtenderDnsNamedZoneId),
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:            api,
		hostedZoneName: testExtenderDnsHostedZoneName,
	}

	for range 3 {
		if err := publisher.publish(
			context.Background(),
			testExtenderDnsRecordName,
			testExtenderDnsTtl,
			[]*extenderDnsRecordSet{{ipVersion: 4, ips: testExtenderDnsIps(4, 1)}},
		); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	connect.AssertEqual(t, len(api.hostedZoneInputs), 1)
	connect.AssertEqual(t, len(api.changeInputs), 3)
	for _, changeInput := range api.changeInputs {
		connect.AssertEqual(
			t,
			aws.StringValue(changeInput.HostedZoneId),
			testExtenderDnsNamedZoneId,
		)
	}
}

// The zone is configured by name as well as by id, and a block that carries
// neither has nowhere to write.
func TestExtenderDnsConfigurationParsesTheZoneName(t *testing.T) {
	installTestExtenderWorkConfig(
		t,
		"dns:",
		"  enabled: true",
		"  hosted_zone_name: "+testExtenderDnsHostedZoneName,
		"  record_name: "+testExtenderDnsRecordName,
	)
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatalf("EnvExtenderConfig: %v", err)
	}
	connect.AssertEqual(t, config.Dns.Enabled, true)
	connect.AssertEqual(t, config.Dns.HostedZoneName, testExtenderDnsHostedZoneName)
	connect.AssertEqual(t, config.Dns.HostedZoneId, "")

	if _, err := newRoute53ExtenderDnsPublisher(&controller.ExtenderConfig{
		Dns: controller.ExtenderDnsConfig{Enabled: true},
	}); err == nil {
		t.Fatalf("a dns block with no zone must not build a publisher")
	}
}

// A deployment with no dns block, or with it disabled, leaves dns untouched.
// Publishing records without publishing dns is a supported configuration, so
// nothing here may reach the publisher at all.
func TestExtenderDnsPublishIsInertWithoutConfiguration(t *testing.T) {
	publisher := stubExtenderDnsPublisher(t)
	configs := []*controller.ExtenderConfig{
		nil,
		{},
		{Dns: controller.ExtenderDnsConfig{RecordName: testExtenderDnsRecordName}},
	}
	for _, config := range configs {
		if err := publishExtenderDns(context.Background(), config); err != nil {
			t.Fatalf("an unconfigured dns block must not fail the tick: %v", err)
		}
	}
	connect.AssertEqual(t, len(publisher.batches), 0)
}

// The record name is configured, not derived. Publishing to a guessed name
// would write geolocation sets somewhere nobody resolves.
func TestExtenderDnsPublishNeedsARecordName(t *testing.T) {
	publisher := stubExtenderDnsPublisher(t)
	err := publishExtenderDns(
		context.Background(),
		&controller.ExtenderConfig{Dns: controller.ExtenderDnsConfig{Enabled: true}},
	)
	if err == nil {
		t.Fatalf("a dns block with no record name must not publish")
	}
	connect.AssertEqual(t, len(publisher.batches), 0)
}

// Defaults of C5: eight addresses per set and a ttl of a minute unless the
// configuration says otherwise.
func TestExtenderDnsConfigurationDefaults(t *testing.T) {
	config := &controller.ExtenderConfig{}
	connect.AssertEqual(t, extenderDnsSampleCount(config), ExtenderDnsDefaultSampleCount)
	connect.AssertEqual(t, extenderDnsTtl(config), ExtenderDnsDefaultTtl)

	config = &controller.ExtenderConfig{
		Dns: controller.ExtenderDnsConfig{SampleCount: 4, Ttl: 120},
	}
	connect.AssertEqual(t, extenderDnsSampleCount(config), 4)
	connect.AssertEqual(t, extenderDnsTtl(config), 120)
}

// One address of a created extender.
type testExtenderDnsAddressSpec struct {
	ipVersion int
	active    bool
}

// Creates one extender with the country and activity given, and one address
// per spec addressed from the documentation ranges by index.
func createTestDnsExtender(
	ctx context.Context,
	index int,
	countryCode string,
	active bool,
	addressSpecs ...testExtenderDnsAddressSpec,
) server.Id {
	extenderId := server.NewId()
	createTime := server.NowUtc()
	addresses := []*model.NetworkExtenderAddress{}
	for _, addressSpec := range addressSpecs {
		addresses = append(addresses, &model.NetworkExtenderAddress{
			IpVersion:    addressSpec.ipVersion,
			Ip:           netip.MustParseAddr(testExtenderDnsIp(addressSpec.ipVersion, index)),
			Carriers:     []string{connect.ExtenderCarrierTcp},
			ActivateTime: createTime,
			Active:       addressSpec.active,
		})
	}
	model.Testing_CreateNetworkExtender(
		ctx,
		&model.NetworkExtender{
			ExtenderId:  extenderId,
			NetworkId:   server.NewId(),
			ClientId:    server.NewId(),
			PublicKey:   []byte(fmt.Sprintf("extender-public-key-dns-%07d", index)),
			CreateTime:  createTime,
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: countryCode,
			Active:      active,
		},
		addresses,
	)
	return extenderId
}

// The whole tick over a directory on several continents: the sets are one per
// continent that has an address of the family, plus the default, and only
// active addresses of active extenders are in any of them.
func TestExtenderDnsPublishSamplesEveryContinentAndFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		publisher := stubExtenderDnsPublisher(t)

		bothFamilies := []testExtenderDnsAddressSpec{
			{ipVersion: 4, active: true},
			{ipVersion: 6, active: true},
		}
		// north america, both families
		for _, index := range []int{1, 2, 3} {
			createTestDnsExtender(ctx, index, "us", true, bothFamilies...)
		}
		// europe, v4 only
		for _, index := range []int{4, 5} {
			createTestDnsExtender(ctx, index, "de", true, testExtenderDnsAddressSpec{ipVersion: 4, active: true})
		}
		// asia, with a v4 that failed its probes and a v6 that did not
		createTestDnsExtender(
			ctx,
			6,
			"jp",
			true,
			testExtenderDnsAddressSpec{ipVersion: 4, active: false},
			testExtenderDnsAddressSpec{ipVersion: 6, active: true},
		)
		// an extender that was revoked, whose addresses are in nothing
		createTestDnsExtender(ctx, 7, "fr", false, bothFamilies...)
		// and one the geolocation could not place
		createTestDnsExtender(ctx, 8, "", true, testExtenderDnsAddressSpec{ipVersion: 4, active: true})

		runTestExtenderPublish(t, ctx)

		batch := publisher.onlyBatch(t)
		connect.AssertEqual(t, batch.recordName, testExtenderDnsRecordName)
		connect.AssertEqual(t, batch.ttl, testExtenderDnsTtl)
		connect.AssertEqual(t, batch.deletedSetIdentifiers, []string{})
		// a continent with no address of a family has no set, so there is
		// nothing for AF, AN, OC or SA and no A set for asia
		connect.AssertEqual(t, batch.setIdentifiers(), []string{
			"extender-EU-A",
			"extender-NA-A",
			"extender-default-A",
			"extender-AS-AAAA",
			"extender-NA-AAAA",
			"extender-default-AAAA",
		})

		globalIpv4Ips := testExtenderDnsIps(4, 1, 2, 3, 4, 5, 8)
		globalIpv6Ips := testExtenderDnsIps(6, 1, 2, 3, 6)

		// north america has exactly its own three
		northAmerica := assertTestExtenderDnsSet(
			t,
			batch.upsertSets,
			"extender-NA-A",
			testExtenderDnsSampleCount,
			testExtenderDnsIps(4, 1, 2, 3),
		)
		connect.AssertEqual(t, len(northAmerica.ips), testExtenderDnsSampleCount)

		// europe has two of its own and one filled from the global pool
		europe := assertTestExtenderDnsSet(
			t,
			batch.upsertSets,
			"extender-EU-A",
			testExtenderDnsSampleCount,
			globalIpv4Ips,
		)
		for _, ip := range testExtenderDnsIps(4, 4, 5) {
			if !slices.Contains(europe.ips, ip) {
				t.Fatalf("the europe set %v is missing its own %s", europe.ips, ip)
			}
		}

		// asia has one of its own and two fills
		asia := assertTestExtenderDnsSet(
			t,
			batch.upsertSets,
			"extender-AS-AAAA",
			testExtenderDnsSampleCount,
			globalIpv6Ips,
		)
		if !slices.Contains(asia.ips, testExtenderDnsIp(6, 6)) {
			t.Fatalf("the asia set %v is missing its own address", asia.ips)
		}

		assertTestExtenderDnsSet(
			t,
			batch.upsertSets,
			"extender-default-A",
			testExtenderDnsSampleCount,
			globalIpv4Ips,
		)
		assertTestExtenderDnsSet(
			t,
			batch.upsertSets,
			"extender-default-AAAA",
			testExtenderDnsSampleCount,
			globalIpv6Ips,
		)

		// the revoked extender and the failed address are in no set at all
		for _, upsertSet := range batch.upsertSets {
			for _, ip := range upsertSet.ips {
				if ip == testExtenderDnsIp(4, 6) {
					t.Fatalf("%s holds a deactivated address", upsertSet.setIdentifier())
				}
				if ip == testExtenderDnsIp(4, 7) || ip == testExtenderDnsIp(6, 7) {
					t.Fatalf("%s holds an inactive extender's address", upsertSet.setIdentifier())
				}
			}
		}
	})
}

// A continent that loses its last address loses its set. Leaving it would
// answer a whole continent with addresses the uptime probes have already
// removed.
func TestExtenderDnsPublishDeletesAContinentThatWentEmpty(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		publisher := stubExtenderDnsPublisher(t)

		createTestDnsExtender(ctx, 1, "us", true, testExtenderDnsAddressSpec{ipVersion: 4, active: true})
		europeExtenderId := createTestDnsExtender(
			ctx,
			2,
			"de",
			true,
			testExtenderDnsAddressSpec{ipVersion: 4, active: true},
		)

		runTestExtenderPublish(t, ctx)
		first := publisher.onlyBatch(t)
		connect.AssertEqual(t, first.setIdentifiers(), []string{
			"extender-EU-A",
			"extender-NA-A",
			"extender-default-A",
		})
		connect.AssertEqual(t, first.deletedSetIdentifiers, []string{})

		model.Testing_DeactivateNetworkExtenderAddress(ctx, europeExtenderId, 4)
		runTestExtenderPublish(t, ctx)

		connect.AssertEqual(t, len(publisher.batches), 2)
		second := publisher.batches[1]
		connect.AssertEqual(t, second.setIdentifiers(), []string{
			"extender-NA-A",
			"extender-default-A",
		})
		connect.AssertEqual(t, second.deletedSetIdentifiers, []string{"extender-EU-A"})
		// and the zone the ticks left behind holds exactly what is desired
		connect.AssertEqual(t, publisher.zoneSetIdentifiers, []string{
			"extender-NA-A",
			"extender-default-A",
		})
	})
}

// The dns sets and the record drip are independent publishers of the same
// directory. A dns failure costs the drip nothing, and the chain still
// re-arms: a tick that returned the error would be retried on a backoff with
// its Post skipped, and the whole rotation would stall on an aws outage.
func TestExtenderDnsPublishFailureDoesNotStopTheDrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		publisher := stubExtenderDnsPublisher(t)
		publisher.err = fmt.Errorf("route 53 is unreachable")

		createTestDnsExtender(ctx, 1, "us", true, testExtenderDnsAddressSpec{ipVersion: 4, active: true})

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Published, 1)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 1)
		// the batch was attempted and left the zone as it was
		connect.AssertEqual(t, len(publisher.onlyBatch(t).upsertSets), 2)
		connect.AssertEqual(t, publisher.zoneSetIdentifiers, []string{})

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ExtenderPublishPost(
				&ExtenderPublishArgs{},
				result,
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("ExtenderPublishPost: %v", err)
			}
		})
		runAt := testExtenderTaskRunAt(t, ctx, "extender_publish")
		want := before.Add(ExtenderPublishTimeout)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("publish run_at = %s, want about %s", runAt, want)
		}
	})
}

// The name a comparison is made on is the form the zone returns: fully
// qualified and lower case. A client may configure any of these spellings and
// the listing must still line up with the record it is about.
func TestExtenderDnsFqdn(t *testing.T) {
	cases := []struct {
		recordName string
		fqdn       string
	}{
		{recordName: "extender.ur.example", fqdn: "extender.ur.example."},
		{recordName: "extender.ur.example.", fqdn: "extender.ur.example."},
		{recordName: "  EXTENDER.UR.Example. ", fqdn: "extender.ur.example."},
		{recordName: "", fqdn: "."},
		{recordName: "   ", fqdn: "."},
	}
	for _, c := range cases {
		if fqdn := extenderDnsFqdn(c.recordName); fqdn != c.fqdn {
			t.Errorf("extenderDnsFqdn(%q) = %q, want %q", c.recordName, fqdn, c.fqdn)
		}
	}
}

// What an apply removes is exactly the sets of ours the zone holds that this
// tick no longer wants. A desired set is never deleted and re-upserted, and a
// zone that lists one of ours twice produces one delete, since a second delete
// of the same set fails the whole batch.
func TestExtenderDnsDeletedSetIdentifiers(t *testing.T) {
	desiredSets := []*extenderDnsRecordSet{
		{continentCode: "EU", ipVersion: 4},
		{ipVersion: 6},
	}
	cases := []struct {
		name                   string
		existingSetIdentifiers []string
		deletedSetIdentifiers  []string
	}{
		{
			name:                   "an empty zone deletes nothing",
			existingSetIdentifiers: []string{},
			deletedSetIdentifiers:  []string{},
		},
		{
			name:                   "a desired set is upserted rather than deleted",
			existingSetIdentifiers: []string{"extender-EU-A", "extender-default-AAAA"},
			deletedSetIdentifiers:  []string{},
		},
		{
			name:                   "a set that is no longer desired is deleted",
			existingSetIdentifiers: []string{"extender-EU-A", "extender-AS-A", "extender-default-A"},
			deletedSetIdentifiers:  []string{"extender-AS-A", "extender-default-A"},
		},
		{
			name:                   "a repeated listing is one delete",
			existingSetIdentifiers: []string{"extender-AS-A", "extender-AS-A"},
			deletedSetIdentifiers:  []string{"extender-AS-A"},
		},
	}
	for _, c := range cases {
		deleted := extenderDnsDeletedSetIdentifiers(c.existingSetIdentifiers, desiredSets)
		if !slices.Equal(deleted, c.deletedSetIdentifiers) {
			t.Errorf("%s: deleted = %v, want %v", c.name, deleted, c.deletedSetIdentifiers)
		}
	}
}

// A zone holds more at one name than this publisher's address sets. A record of
// another type, a set another owner put there, and the same set of ours listed
// twice must all leave the batch with exactly one delete of our own stale set:
// anything else either destroys someone's record or fails the whole batch on a
// repeated change.
func TestRoute53ExtenderDnsIgnoresForeignAndNonAddressSets(t *testing.T) {
	name := testExtenderDnsRecordName + "."
	staleSet := testRoute53RecordSet(name, "extender-AS-A", route53.RRTypeA, "198.51.100.95")
	repeatedStaleSet := testRoute53RecordSet(name, "extender-AS-A", route53.RRTypeA, "198.51.100.96")
	txtSet := testRoute53RecordSet(name, "extender-AS-TXT", route53.RRTypeTxt, "\"not an address\"")
	foreignSet := testRoute53RecordSet(name, "operator-A", route53.RRTypeA, "198.51.100.97")
	api := &testRoute53Api{
		existingRecordSets: []*route53.ResourceRecordSet{
			staleSet,
			txtSet,
			foreignSet,
			repeatedStaleSet,
		},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:          api,
		hostedZoneId: testExtenderDnsHostedZoneId,
	}

	desiredSets := []*extenderDnsRecordSet{
		{continentCode: "EU", ipVersion: 4, ips: testExtenderDnsIps(4, 1)},
	}
	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		desiredSets,
	); err != nil {
		t.Fatalf("publish: %v", err)
	}

	connect.AssertEqual(t, len(api.changeInputs), 1)
	changes := api.changeInputs[0].ChangeBatch.Changes
	connect.AssertEqual(t, len(changes), 2)
	connect.AssertEqual(t, aws.StringValue(changes[0].Action), route53.ChangeActionUpsert)
	connect.AssertEqual(t, aws.StringValue(changes[0].ResourceRecordSet.SetIdentifier), "extender-EU-A")
	connect.AssertEqual(t, aws.StringValue(changes[1].Action), route53.ChangeActionDelete)
	// the first listing of the set is the one the delete carries
	if changes[1].ResourceRecordSet != staleSet {
		t.Fatal("the delete does not carry the set as the zone first listed it")
	}
	for _, change := range changes {
		if change.ResourceRecordSet == txtSet || change.ResourceRecordSet == foreignSet {
			t.Fatalf("a record that is not ours was changed: %v", change.ResourceRecordSet)
		}
	}
}

// Only a successful resolution is remembered. A zone that is missing now may be
// created later, and caching the failure would need a process restart to clear
// it -- which is a deploy to fix a dns record.
func TestRoute53ExtenderDnsRetriesAZoneThatDidNotResolve(t *testing.T) {
	resetTestExtenderDnsHostedZoneIds(t)

	api := &testRoute53Api{hostedZonesErr: errTestRoute53Unreachable}
	publisher := &route53ExtenderDnsPublisher{
		api:            api,
		hostedZoneName: testExtenderDnsHostedZoneName,
	}
	desiredSets := []*extenderDnsRecordSet{{ipVersion: 4, ips: testExtenderDnsIps(4, 1)}}

	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		desiredSets,
	); err == nil {
		t.Fatal("an unresolvable zone must fail the apply")
	}
	connect.AssertEqual(t, len(api.changeInputs), 0)

	// the zone exists on the next tick, with no restart in between
	api.hostedZonesErr = nil
	api.hostedZones = []*route53.HostedZone{
		testRoute53HostedZone(testExtenderDnsHostedZoneName+".", testExtenderDnsNamedZoneId),
	}
	if err := publisher.publish(
		context.Background(),
		testExtenderDnsRecordName,
		testExtenderDnsTtl,
		desiredSets,
	); err != nil {
		t.Fatalf("publish: %v", err)
	}
	connect.AssertEqual(t, len(api.hostedZoneInputs), 2)
	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)

	// a block that names no zone at all has nowhere to write and costs no call
	if _, err := resolveExtenderDnsHostedZoneId(context.Background(), api, "", "  "); err == nil {
		t.Fatal("a dns block with no zone resolved to one")
	}
	connect.AssertEqual(t, len(api.hostedZoneInputs), 2)
}

// What a route 53 call fails with when the api cannot be reached.
var errTestRoute53Unreachable = fmt.Errorf("route 53 is unreachable")

// Static credentials are used only when both halves are configured. A
// half-configured pair is a typo rather than an intent, and signing with it
// would fail every call rather than fall back to the instance role the host
// already has.
func TestExtenderRoute53ApiUsesStaticCredentialsOnlyWhenBothHalvesAreSet(t *testing.T) {
	// a resolvable default chain, so the fallback costs no metadata call
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLECHAINKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example-chain-secret")

	const configuredAccessKeyId = "AKIAEXAMPLESTATICKEY"
	const configuredSecretAccessKey = "example-static-secret"
	cases := []struct {
		name               string
		awsRegion          string
		awsAccessKeyId     string
		awsSecretAccessKey string
		region             string
		accessKeyId        string
	}{
		{
			name:               "both halves",
			awsRegion:          "eu-west-1",
			awsAccessKeyId:     configuredAccessKeyId,
			awsSecretAccessKey: configuredSecretAccessKey,
			region:             "eu-west-1",
			accessKeyId:        configuredAccessKeyId,
		},
		{
			name:           "only the key id",
			awsAccessKeyId: configuredAccessKeyId,
			region:         ExtenderDnsDefaultAwsRegion,
			accessKeyId:    "AKIAEXAMPLECHAINKEY",
		},
		{
			name:               "only the secret",
			awsSecretAccessKey: configuredSecretAccessKey,
			region:             ExtenderDnsDefaultAwsRegion,
			accessKeyId:        "AKIAEXAMPLECHAINKEY",
		},
		{
			name:        "neither half",
			region:      ExtenderDnsDefaultAwsRegion,
			accessKeyId: "AKIAEXAMPLECHAINKEY",
		},
		{
			name:               "blank halves are not configuration",
			awsRegion:          "  ",
			awsAccessKeyId:     "  ",
			awsSecretAccessKey: "  ",
			region:             ExtenderDnsDefaultAwsRegion,
			accessKeyId:        "AKIAEXAMPLECHAINKEY",
		},
	}
	for _, c := range cases {
		api, err := newExtenderRoute53Api(c.awsRegion, c.awsAccessKeyId, c.awsSecretAccessKey)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		route53Client, ok := api.(*route53.Route53)
		if !ok {
			t.Fatalf("%s: the api is %T", c.name, api)
		}
		if region := aws.StringValue(route53Client.Config.Region); region != c.region {
			t.Errorf("%s: region = %q, want %q", c.name, region, c.region)
		}
		value, err := route53Client.Config.Credentials.Get()
		if err != nil {
			t.Errorf("%s: credentials: %v", c.name, err)
			continue
		}
		if value.AccessKeyID != c.accessKeyId {
			t.Errorf("%s: access key = %q, want %q", c.name, value.AccessKeyID, c.accessKeyId)
		}
		if c.accessKeyId == configuredAccessKeyId && value.SecretAccessKey != configuredSecretAccessKey {
			t.Errorf("%s: the configured secret was not used", c.name)
		}
	}
}
