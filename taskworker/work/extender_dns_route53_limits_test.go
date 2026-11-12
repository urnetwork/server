package work

// The extender dns sets against Route 53's quotas and the released reader
// (connect/EXTENDER.md C5, E3).
//
// From 2026-10-28 18:12Z every tick was refused whole: the publisher wrote
// every A, AAAA and TXT set in one change batch, and Route 53 refuses a request
// whose Value elements add up to more than 32,000 characters, counting each
// character of an UPSERT twice ("InvalidChangeBatch: [RDATA character limit of
// 32000 exceeded.]"). Its gauges were set from the desired sets before the
// batch was sent, so the dashboards showed 62 extenders in dns while none of it
// landed (factury/research/extender-health-2026-11-11.md §1.4).
//
// The Route 53 here is a fake that keeps the zone in memory and enforces the
// two request quotas exactly as the developer guide states them ("Quotas",
// "Maximums on API requests"). The released reader is the DoH path of connect
// v2026.10.27-1083160450, mirrored below: it reads at most 16 KiB of an answer
// and keeps the TXT records it parsed before the cut.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Route 53's quotas on one ChangeResourceRecordSets request: the characters
// of every Value element, and the ResourceRecord elements, each counted twice
// in an UPSERT.
const (
	testRoute53MaxRequestCharacters = 32_000
	testRoute53MaxRequestRecords    = 1_000
)

// A hosted zone as Route 53 keeps it, at the one name the publisher writes.
// A request is checked against the quotas and applied whole or not at all.
// Used from the test goroutine only.
type testQuotaRoute53Api struct {
	zoneSetIdentifiers []string
	zone               map[string]*route53.ResourceRecordSet
	// every change of this record type is refused, as Route 53 refuses a set
	// it will not take
	refusedRecordType string
	// the listing fails, as it does while Route 53 is unreachable
	listErr error

	changeInputs []*route53.ChangeResourceRecordSetsInput
	refusals     []error
}

func newTestQuotaRoute53Api() *testQuotaRoute53Api {
	return &testQuotaRoute53Api{
		zoneSetIdentifiers: []string{},
		zone:               map[string]*route53.ResourceRecordSet{},
	}
}

func (self *testQuotaRoute53Api) ListHostedZonesByNameWithContext(
	_ aws.Context,
	_ *route53.ListHostedZonesByNameInput,
	_ ...request.Option,
) (*route53.ListHostedZonesByNameOutput, error) {
	return nil, fmt.Errorf("the zone is configured by id")
}

func (self *testQuotaRoute53Api) ListResourceRecordSetsPagesWithContext(
	_ aws.Context,
	_ *route53.ListResourceRecordSetsInput,
	fn func(*route53.ListResourceRecordSetsOutput, bool) bool,
	_ ...request.Option,
) error {
	if self.listErr != nil {
		return self.listErr
	}
	recordSets := []*route53.ResourceRecordSet{}
	for _, setIdentifier := range self.zoneSetIdentifiers {
		recordSets = append(recordSets, self.zone[setIdentifier])
	}
	fn(&route53.ListResourceRecordSetsOutput{ResourceRecordSets: recordSets}, true)
	return nil
}

func (self *testQuotaRoute53Api) ChangeResourceRecordSetsWithContext(
	_ aws.Context,
	input *route53.ChangeResourceRecordSetsInput,
	_ ...request.Option,
) (*route53.ChangeResourceRecordSetsOutput, error) {
	self.changeInputs = append(self.changeInputs, input)
	refuse := func(code string, message string) (*route53.ChangeResourceRecordSetsOutput, error) {
		err := awserr.New(code, message, nil)
		self.refusals = append(self.refusals, err)
		return nil, err
	}

	characters := 0
	records := 0
	for _, change := range input.ChangeBatch.Changes {
		weight := 1
		if aws.StringValue(change.Action) == route53.ChangeActionUpsert {
			weight = 2
		}
		for _, resourceRecord := range change.ResourceRecordSet.ResourceRecords {
			characters += weight * len(aws.StringValue(resourceRecord.Value))
			records += weight
		}
		if aws.StringValue(change.ResourceRecordSet.Type) == self.refusedRecordType {
			return refuse("InvalidChangeBatch", "[the test zone refuses "+self.refusedRecordType+" sets]")
		}
		if aws.StringValue(change.Action) == route53.ChangeActionDelete {
			if _, ok := self.zone[aws.StringValue(change.ResourceRecordSet.SetIdentifier)]; !ok {
				return refuse("InvalidChangeBatch", "[Tried to delete resource record set that does not exist]")
			}
		}
	}
	if testRoute53MaxRequestCharacters < characters {
		return refuse("InvalidChangeBatch", fmt.Sprintf("[RDATA character limit of %d exceeded.]", testRoute53MaxRequestCharacters))
	}
	if testRoute53MaxRequestRecords < records {
		return refuse("InvalidChangeBatch", fmt.Sprintf("[Number of records limit of %d exceeded.]", testRoute53MaxRequestRecords))
	}

	for _, change := range input.ChangeBatch.Changes {
		setIdentifier := aws.StringValue(change.ResourceRecordSet.SetIdentifier)
		switch aws.StringValue(change.Action) {
		case route53.ChangeActionDelete:
			delete(self.zone, setIdentifier)
			self.zoneSetIdentifiers = slices.DeleteFunc(self.zoneSetIdentifiers, func(zoneSetIdentifier string) bool {
				return zoneSetIdentifier == setIdentifier
			})
		default:
			if _, ok := self.zone[setIdentifier]; !ok {
				self.zoneSetIdentifiers = append(self.zoneSetIdentifiers, setIdentifier)
			}
			self.zone[setIdentifier] = change.ResourceRecordSet
		}
	}
	return &route53.ChangeResourceRecordSetsOutput{}, nil
}

// The values of one set as the zone holds it, or nil when it holds no such
// set.
func (self *testQuotaRoute53Api) zoneValues(setIdentifier string) []string {
	recordSet, ok := self.zone[setIdentifier]
	if !ok {
		return nil
	}
	values := []string{}
	for _, resourceRecord := range recordSet.ResourceRecords {
		values = append(values, aws.StringValue(resourceRecord.Value))
	}
	return values
}

// The values a desired set is published with.
func testExtenderDnsSetValues(desiredSet *extenderDnsRecordSet) []string {
	values := slices.Clone(desiredSet.ips)
	for _, record := range desiredSet.records {
		values = append(values, extenderDnsTxtValue(record))
	}
	return values
}

// A record as large as the test needs, distinct per extender (the released
// reader drops a repeated value), in the base64 alphabet.
func testExtenderDnsSizedSigner(length int) extenderDnsRecordSigner {
	return func(extenderId server.Id) (string, bool) {
		prefix := hex.EncodeToString(extenderId.Bytes())
		return prefix + strings.Repeat("A", max(0, length-len(prefix))), true
	}
}

// The tick's sample over a directory shaped like Main's: three continents
// with deep single-stack pools in both families, so every location answers
// sampleCount addresses of each family and its TXT set carries a record for
// each of them.
func testExtenderDnsMainShapedAddresses() []*model.NetworkExtenderDnsAddress {
	addresses := []*model.NetworkExtenderDnsAddress{}
	index := 1
	for _, countryCode := range []string{"us", "de", "jp"} {
		for _, ipVersion := range extenderDnsIpVersions {
			for range 12 {
				addresses = append(addresses, testExtenderDnsAddresses(countryCode, ipVersion, index)...)
				index += 1
			}
		}
	}
	return addresses
}

// Main's sets on 11-11 were 5 A, 4 AAAA and 5 TXT, 40 and 32 addresses and 63
// signed records of 300 to 400 characters. More than 32,000 characters in one
// request whatever way they are batched together; well under it set by set.
// Every set lands, each in a request Route 53 takes.
func TestRoute53ExtenderDnsLandsEverySetPastTheRequestCharacterLimit(t *testing.T) {
	desiredSets := sampleExtenderDnsRecordSets(
		testExtenderDnsMainShapedAddresses(),
		8,
		testExtenderDnsSecret,
		testExtenderDnsEpoch,
		testExtenderDnsSizedSigner(400),
	)
	totalCharacters := 0
	for _, desiredSet := range desiredSets {
		for _, value := range testExtenderDnsSetValues(desiredSet) {
			totalCharacters += 2 * len(value)
		}
	}
	if totalCharacters <= testRoute53MaxRequestCharacters {
		t.Fatalf("the fixture's sets are %d characters as upserts, which one request could carry", totalCharacters)
	}

	api := newTestQuotaRoute53Api()
	publisher := &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}
	publisher.publish(context.Background(), testExtenderDnsRecordName, testExtenderDnsTtl, desiredSets)

	missing := []string{}
	for _, desiredSet := range desiredSets {
		if !slices.Equal(api.zoneValues(desiredSet.setIdentifier()), testExtenderDnsSetValues(desiredSet)) {
			missing = append(missing, desiredSet.setIdentifier())
		}
	}
	if 0 < len(missing) {
		t.Fatalf("Route 53 holds %d of the %d desired sets; missing %v; refusals %v",
			len(desiredSets)-len(missing), len(desiredSets), missing, api.refusals)
	}
	if 0 < len(api.refusals) {
		t.Fatalf("Route 53 refused %v", api.refusals)
	}
}

// A TXT set Route 53 will not take costs the TXT sets and nothing else: every
// A and AAAA set still lands, so a client is still answered with addresses.
func TestRoute53ExtenderDnsTxtRefusalDoesNotBlockTheAddressSets(t *testing.T) {
	desiredSets := sampleExtenderDnsRecordSets(
		testExtenderDnsMainShapedAddresses(),
		8,
		testExtenderDnsSecret,
		testExtenderDnsEpoch,
		testExtenderDnsSizedSigner(300),
	)
	api := newTestQuotaRoute53Api()
	api.refusedRecordType = route53.RRTypeTxt
	publisher := &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}
	publisher.publish(context.Background(), testExtenderDnsRecordName, testExtenderDnsTtl, desiredSets)

	addressSets := 0
	for _, desiredSet := range desiredSets {
		if desiredSet.recordType() == route53.RRTypeTxt {
			if api.zoneValues(desiredSet.setIdentifier()) != nil {
				t.Fatalf("%s landed in a zone that refuses TXT sets", desiredSet.setIdentifier())
			}
			continue
		}
		addressSets += 1
		if !slices.Equal(api.zoneValues(desiredSet.setIdentifier()), testExtenderDnsSetValues(desiredSet)) {
			t.Fatalf("the TXT refusal kept %s out of the zone too; the zone holds %v",
				desiredSet.setIdentifier(), api.zoneSetIdentifiers)
		}
	}
	if addressSets == 0 {
		t.Fatal("the fixture has no address sets")
	}
}

// The DoH answer cap of the released reader (connect v2026.10.27-1083160450,
// v2026/net_http_doh.go maxDohResponseBytes).
const testReleasedReaderMaxDohResponseBytes = 16 * 1024

// The TXT values a released client takes from one DoH answer, as
// v2026/net_http_doh.go reads it: the body cut at 16 KiB by io.LimitReader
// (queryWireRawDetailedWithRoute), then every TXT answer the parser gets
// through before the cut, the strings of each joined (parseDohWire). A partial
// answer is an answer, so the system resolver is never asked instead
// (net_extender_network.go resolveDnsTxt).
func testReleasedReaderTxts(response []byte) []string {
	data, _ := io.ReadAll(io.LimitReader(bytes.NewReader(response), testReleasedReaderMaxDohResponseBytes))
	var parser dnsmessage.Parser
	header, err := parser.Start(data)
	if err != nil || header.RCode != dnsmessage.RCodeSuccess {
		return nil
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return nil
	}
	txts := []string{}
	for {
		answerHeader, err := parser.AnswerHeader()
		if err != nil {
			// the section's end, or the cut
			return txts
		}
		if answerHeader.Type != dnsmessage.TypeTXT {
			if err := parser.SkipAnswer(); err != nil {
				return txts
			}
			continue
		}
		resource, err := parser.TXTResource()
		if err != nil {
			return txts
		}
		txt := strings.Join(resource.TXT, "")
		if txt != "" && !slices.Contains(txts, txt) {
			txts = append(txts, txt)
		}
	}
}

// What a resolver answers a DoH client for one TXT set: the question, every
// record of the set in strings of at most 255 characters, and an OPT record
// padded to a multiple of 468 bytes, the block RFC 8467 recommends for
// responses.
func testResolverTxtResponse(t testing.TB, recordName string, records []string) []byte {
	t.Helper()
	name := dnsmessage.MustNewName(extenderDnsFqdn(recordName))
	build := func(padding int) []byte {
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
			Response:           true,
			RecursionDesired:   true,
			RecursionAvailable: true,
		})
		builder.EnableCompression()
		if err := builder.StartQuestions(); err != nil {
			t.Fatal(err)
		}
		if err := builder.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}); err != nil {
			t.Fatal(err)
		}
		if err := builder.StartAnswers(); err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			strs := []string{}
			for value := record; ; value = value[extenderDnsTxtMaxStringLength:] {
				if len(value) <= extenderDnsTxtMaxStringLength {
					strs = append(strs, value)
					break
				}
				strs = append(strs, value[:extenderDnsTxtMaxStringLength])
			}
			if err := builder.TXTResource(
				dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60},
				dnsmessage.TXTResource{TXT: strs},
			); err != nil {
				t.Fatal(err)
			}
		}
		if err := builder.StartAdditionals(); err != nil {
			t.Fatal(err)
		}
		optHeader := dnsmessage.ResourceHeader{}
		if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		if err := builder.OPTResource(optHeader, dnsmessage.OPTResource{
			Options: []dnsmessage.Option{{Code: 12, Data: make([]byte, padding)}},
		}); err != nil {
			t.Fatal(err)
		}
		message, err := builder.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	unpadded := build(0)
	return build((468 - len(unpadded)%468) % 468)
}

// A TXT set fits both what Route 53 takes and what the released reader reads:
// one location's records, upserted alone, are under the request's character
// quota, and the answer a resolver hands a released client carries every one
// of them inside its 16 KiB. An address goes with its record, so every address
// a location is answered with is still one a record vouches for -- a released
// client dials only what was verified (net_extender_network.go runHellos).
func TestExtenderDnsTxtSetFitsRoute53AndTheReleasedReader(t *testing.T) {
	addresses := []*model.NetworkExtenderDnsAddress{}
	addresses = append(addresses, testExtenderDnsAddresses("us", 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)
	addresses = append(addresses, testExtenderDnsAddresses("us", 6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)
	ipExtenderIds := map[string]server.Id{}
	for _, address := range addresses {
		ipExtenderIds[address.Ip.String()] = address.ExtenderId
	}
	signRecord := testExtenderDnsSizedSigner(1_500)

	desiredSets := sampleExtenderDnsRecordSets(addresses, 8, testExtenderDnsSecret, testExtenderDnsEpoch, signRecord)

	txtSets := 0
	for _, desiredSet := range desiredSets {
		if desiredSet.recordType() != route53.RRTypeTxt {
			continue
		}
		txtSets += 1
		characters := 0
		for _, record := range desiredSet.records {
			characters += 2 * len(extenderDnsTxtValue(record))
		}
		if testRoute53MaxRequestCharacters < characters {
			t.Errorf("%s alone is %d characters as an upsert, over the %d Route 53 takes in one request",
				desiredSet.setIdentifier(), characters, testRoute53MaxRequestCharacters)
		}

		response := testResolverTxtResponse(t, testExtenderDnsRecordName, desiredSet.records)
		read := testReleasedReaderTxts(response)
		if len(read) != len(desiredSet.records) {
			t.Errorf("a released client reads %d of the %d records of %s from a %d-byte answer",
				len(read), len(desiredSet.records), desiredSet.setIdentifier(), len(response))
		}

		// the extenders the set's records belong to
		vouched := map[server.Id]bool{}
		for _, extenderId := range ipExtenderIds {
			if record, ok := signRecord(extenderId); ok && slices.Contains(desiredSet.records, record) {
				vouched[extenderId] = true
			}
		}
		for _, addressSet := range desiredSets {
			if addressSet.continentCode != desiredSet.continentCode || addressSet.recordType() == route53.RRTypeTxt {
				continue
			}
			for _, ip := range addressSet.ips {
				if !vouched[ipExtenderIds[ip]] {
					t.Fatalf("%s answers %s, which no record of %s vouches for",
						addressSet.setIdentifier(), ip, desiredSet.setIdentifier())
				}
			}
		}
	}
	if txtSets == 0 {
		t.Fatal("the sample has no TXT set")
	}
}

// When a location's records do not all fit, the ones kept are the head of
// what its address sets answered, rank by rank, A before AAAA: the location's
// canaries, then its own partition in the epoch's order, then the fill -- the
// order every set is drawn in (R2, R4). Every record of a tick is signed in
// that tick, so freshness does not tell them apart.
func TestExtenderDnsTxtBudgetKeepsTheHeadOfTheSample(t *testing.T) {
	addresses := []*model.NetworkExtenderDnsAddress{}
	addresses = append(addresses, testExtenderDnsAddresses("us", 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)
	addresses = append(addresses, testExtenderDnsAddresses("us", 6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)

	// small records fit whole, which is the sample before any budget
	unbudgeted := sampleExtenderDnsRecordSets(addresses, 8, testExtenderDnsSecret, testExtenderDnsEpoch, testExtenderDnsSizedSigner(64))
	ipv4 := assertTestExtenderDnsSet(t, unbudgeted, "extender-NA-A", 8, testExtenderDnsIps(4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)).ips
	ipv6 := assertTestExtenderDnsSet(t, unbudgeted, "extender-NA-AAAA", 8, testExtenderDnsIps(6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)).ips

	// records of 1,500 characters are 1,518 bytes of answer each, and nine
	// fit the budget: the first five of the A set and the first four of the
	// AAAA set, taken in turn
	budgeted := sampleExtenderDnsRecordSets(addresses, 8, testExtenderDnsSecret, testExtenderDnsEpoch, testExtenderDnsSizedSigner(1_500))
	for _, check := range []struct {
		setIdentifier string
		want          []string
	}{
		{setIdentifier: "extender-NA-A", want: ipv4[:5]},
		{setIdentifier: "extender-NA-AAAA", want: ipv6[:4]},
	} {
		var found *extenderDnsRecordSet
		for _, desiredSet := range budgeted {
			if desiredSet.setIdentifier() == check.setIdentifier {
				found = desiredSet
			}
		}
		if found == nil {
			t.Fatalf("the budgeted sample has no %s", check.setIdentifier)
		}
		if !slices.Equal(found.ips, check.want) {
			t.Fatalf("%s holds %v, want the head of the sample %v", check.setIdentifier, found.ips, check.want)
		}
	}
	txtSet := testExtenderDnsTxtSet(budgeted, "NA")
	if txtSet == nil || len(txtSet.records) != 9 {
		t.Fatalf("the NA TXT set is %+v, want 9 records", txtSet)
	}
}

// One gauge value as the process exports it, by name and labels.
func testGatheredExtenderDnsGauge(t testing.TB, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := 0
			for _, label := range metric.GetLabel() {
				if value, ok := labels[label.GetName()]; ok && value == label.GetValue() {
					matched += 1
				}
			}
			if matched == len(labels) && len(metric.GetLabel()) == len(labels) {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// What a tick's accepted requests wrote, counted the way the gauges count.
type testExtenderDnsLanded struct {
	setsByType         map[string]float64
	addressesByVersion map[string]float64
	txtRecords         float64
}

func testExtenderDnsLandedIn(api *testQuotaRoute53Api) testExtenderDnsLanded {
	landed := testExtenderDnsLanded{
		setsByType:         map[string]float64{"A": 0, "AAAA": 0, "TXT": 0},
		addressesByVersion: map[string]float64{"4": 0, "6": 0},
	}
	for _, setIdentifier := range api.zoneSetIdentifiers {
		recordSet := api.zone[setIdentifier]
		recordType := aws.StringValue(recordSet.Type)
		landed.setsByType[recordType] += 1
		switch recordType {
		case route53.RRTypeA:
			landed.addressesByVersion["4"] += float64(len(recordSet.ResourceRecords))
		case route53.RRTypeAaaa:
			landed.addressesByVersion["6"] += float64(len(recordSet.ResourceRecords))
		default:
			landed.txtRecords += float64(len(recordSet.ResourceRecords))
		}
	}
	return landed
}

// The gauges say what Route 53 accepted, not what the tick wanted.
func assertTestExtenderDnsGaugesMatch(t testing.TB, landed testExtenderDnsLanded) {
	t.Helper()
	for recordType, want := range landed.setsByType {
		got, ok := testGatheredExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_sets", map[string]string{"record_type": recordType})
		if !ok || got != want {
			t.Fatalf("the %s sets gauge reads %v (exported %t); Route 53 accepted %v", recordType, got, ok, want)
		}
	}
	for ipVersion, want := range landed.addressesByVersion {
		got, ok := testGatheredExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_addresses", map[string]string{"ip_version": ipVersion})
		if !ok || got != want {
			t.Fatalf("the v%s addresses gauge reads %v (exported %t); Route 53 accepted %v", ipVersion, got, ok, want)
		}
	}
	got, ok := testGatheredExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_txt_records", map[string]string{})
	if !ok || got != landed.txtRecords {
		t.Fatalf("the txt records gauge reads %v (exported %t); Route 53 accepted %v", got, ok, landed.txtRecords)
	}
}

// Runs one tick through the real Route 53 publisher over the fake zone.
func runTestExtenderPublishThroughRoute53(t testing.TB, ctx context.Context, api *testQuotaRoute53Api) {
	t.Helper()
	previousPublisher := newExtenderDnsPublisher
	newExtenderDnsPublisher = func(_ *controller.ExtenderConfig) (extenderDnsPublisher, error) {
		return &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}, nil
	}
	defer func() {
		newExtenderDnsPublisher = previousPublisher
	}()
	runTestExtenderPublish(t, ctx)
}

func createTestExtenderDnsGaugeDirectory(ctx context.Context) {
	bothFamilies := []testExtenderDnsAddressSpec{
		{ipVersion: 4, active: true},
		{ipVersion: 6, active: true},
	}
	createTestDnsExtender(ctx, 1, "us", true, bothFamilies...)
	createTestDnsExtender(ctx, 2, "us", true, bothFamilies...)
	createTestDnsExtender(ctx, 3, "de", true, testExtenderDnsAddressSpec{ipVersion: 4, active: true})
}

// A tick whose TXT sets Route 53 refused reports the address sets it landed
// and no TXT set at all.
func TestExtenderDnsGaugesReportWhatRoute53AcceptedWhenTxtIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		createTestExtenderDnsGaugeDirectory(ctx)

		api := newTestQuotaRoute53Api()
		api.refusedRecordType = route53.RRTypeTxt
		runTestExtenderPublishThroughRoute53(t, ctx, api)

		landed := testExtenderDnsLandedIn(api)
		if landed.setsByType["A"] == 0 {
			t.Fatalf("no A set landed; the zone holds %v; refusals %v", api.zoneSetIdentifiers, api.refusals)
		}
		assertTestExtenderDnsGaugesMatch(t, landed)
	})
}

// A tick that could not reach Route 53 at all reports nothing in dns.
func TestExtenderDnsGaugesReportNothingWhenRoute53IsUnreachable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		createTestExtenderDnsGaugeDirectory(ctx)

		api := newTestQuotaRoute53Api()
		api.listErr = errTestRoute53Unreachable
		runTestExtenderPublishThroughRoute53(t, ctx, api)

		assertTestExtenderDnsGaugesMatch(t, testExtenderDnsLandedIn(api))
	})
}
