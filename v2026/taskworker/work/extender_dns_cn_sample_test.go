package work

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// China's own sets (extenderDnsCnSet): the seam the behavior tests of
// extender_dns_cn_set_test.go draw a tick's sets through, and what those tests
// cannot see from the answers China is given -- the setting, the geolocation
// and identifiers of the sets, the budget's arithmetic and the rotation's
// clock.

// A tick's sets over rows, under the sampler tests' secret, with China's sets
// drawn as the tick draws them: recordCount records, at the rotation given.
//
// The behavior tests reach the sampler through this function alone, so the
// same tests run against a sampler that predates China's sets by replacing it.
func sampleTestExtenderDnsChinaSets(
	addresses []*model.NetworkExtenderDnsAddress,
	sampleCount int,
	epoch uint64,
	signRecord extenderDnsRecordSigner,
	recordCount int,
	rotation uint64,
) []*extenderDnsRecordSet {
	return sampleExtenderDnsRecordSets(
		addresses,
		sampleCount,
		testExtenderDnsSecret,
		epoch,
		signRecord,
		extenderDnsCountrySample{
			extenderDnsCountrySet: extenderDnsCnSet,
			recordCount:           recordCount,
			rotation:              rotation,
			recordName:            testExtenderDnsRecordName,
		},
	)
}

// Unless the configuration says otherwise, China's set is capped at four
// records, as many of the shortest records Main signs as Alidns delivers
// whole, so Alidns's ceiling decides how many it holds.
func TestExtenderDnsCnRecordCountDefaultsToWhatAlidnsDeliversWhole(t *testing.T) {
	connect.AssertEqual(t, ExtenderDnsDefaultCnRecordCount, 4)
	connect.AssertEqual(t, extenderDnsCnRecordCount(&controller.ExtenderConfig{}), 4)
	connect.AssertEqual(t, extenderDnsCnRecordCount(&controller.ExtenderConfig{
		Dns: controller.ExtenderDnsConfig{CnRecordCount: -2},
	}), 4)
	shortest := extenderDnsTxtMessageOverheadBytes("extender.bringyour.com") + ExtenderDnsDefaultCnRecordCount*(14+264)
	if extenderDnsCnAlidnsMaxMessageBytes < shortest || extenderDnsCnAlidnsMaxMessageBytes >= shortest+14+264 {
		t.Fatalf("four of the shortest records are a %d-byte answer; the cap should be the most that fit Alidns's %d", shortest, extenderDnsCnAlidnsMaxMessageBytes)
	}

	installTestExtenderWorkConfig(
		t,
		"dns:",
		"  enabled: true",
		"  hosted_zone_id: "+testExtenderDnsHostedZoneId,
		"  record_name: "+testExtenderDnsRecordName,
		"  cn_record_count: 3",
	)
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatalf("EnvExtenderConfig: %v", err)
	}
	connect.AssertEqual(t, config.Dns.CnRecordCount, 3)
	connect.AssertEqual(t, extenderDnsCnRecordCount(config), 3)
}

// China's sets are Route 53's CN country, which answers China in place of the
// Asia set, under identifiers of their own that the listing reads as ours.
func TestExtenderDnsCnSetsAreGeolocatedByCountry(t *testing.T) {
	addresses := testExtenderDnsChinaShapedAddresses()
	desiredSets := sampleTestExtenderDnsChinaSets(
		addresses,
		8,
		testExtenderDnsEpoch,
		testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey),
		1,
		0,
	)
	chinaSetIdentifiers := []string{}
	for _, desiredSet := range desiredSets {
		if desiredSet.countryCode == "" {
			continue
		}
		chinaSetIdentifiers = append(chinaSetIdentifiers, desiredSet.setIdentifier())
		connect.AssertEqual(t, desiredSet.countryCode, "CN")
		connect.AssertEqual(t, desiredSet.continentCode, "AS")
		recordSet := extenderDnsResourceRecordSet(testExtenderDnsRecordName, testExtenderDnsTtl, desiredSet)
		connect.AssertEqual(t, aws.StringValue(recordSet.GeoLocation.CountryCode), "CN")
		connect.AssertEqual(t, recordSet.GeoLocation.ContinentCode, nil)
		if !strings.HasPrefix(aws.StringValue(recordSet.SetIdentifier), extenderDnsSetIdentifierPrefix) {
			t.Fatalf("%s is not marked as ours", aws.StringValue(recordSet.SetIdentifier))
		}
	}
	// every fixture extender has both families, so China has both address sets
	connect.AssertEqual(t, chinaSetIdentifiers, []string{
		"extender-country-CN-A",
		"extender-country-CN-AAAA",
		"extender-country-CN-TXT",
	})
	// and they come after every location's
	for i, desiredSet := range desiredSets {
		if desiredSet.countryCode == "" && slices.ContainsFunc(desiredSets[:i], func(earlier *extenderDnsRecordSet) bool {
			return earlier.countryCode != ""
		}) {
			t.Fatalf("%s comes after China's sets", desiredSet.setIdentifier())
		}
	}
}

// The budget counts a China answer the way a resolver packs it, to the byte,
// and holds a record DNSPod would not deliver alone out of the set, and the set
// to what Alidns delivers whole.
func TestExtenderDnsCountryTxtBudgetCountsTheWholeAnswer(t *testing.T) {
	overhead := extenderDnsTxtMessageOverheadBytes(testExtenderDnsRecordName)
	for _, lengths := range [][]int{{264}, {368}, {264, 300, 368}, {255}, {256}, {510}, {511}} {
		records := []string{}
		recordsBytes := 0
		for i, length := range lengths {
			record := testExtenderDnsBase64Record(i, length)
			records = append(records, record)
			recordsBytes += extenderDnsTxtAnswerBytes(record)
		}
		packed := testExtenderDnsChinaAnswer(t, testExtenderDnsRecordName, records, false)
		connect.AssertEqual(t, overhead+recordsBytes, len(packed))
	}
	// and the production name's, against what Alidns answered on 2026-11-12:
	// three records of 1,096 characters in 1,200 bytes with a client subnet
	connect.AssertEqual(t, extenderDnsTxtMessageOverheadBytes("extender.bringyour.com")+3*14+1_096, 1_200)

	sample := extenderDnsCountrySample{
		extenderDnsCountrySet: extenderDnsCnSet,
		recordCount:           3,
		recordName:            testExtenderDnsRecordName,
	}
	// the longest record DNSPod delivers alone
	longest := extenderDnsCnDnspodMaxMessageBytes - overhead - 14
	if !newExtenderDnsCountryTxtBudget(sample).take(testExtenderDnsBase64Record(0, longest)) {
		t.Fatalf("a record of %d characters, a %d-byte answer, was refused", longest, extenderDnsCnDnspodMaxMessageBytes)
	}
	if newExtenderDnsCountryTxtBudget(sample).take(testExtenderDnsBase64Record(0, longest+1)) {
		t.Fatalf("a record of %d characters, past what DNSPod delivers alone, was taken", longest+1)
	}
	// three of the longest records Main signs are past what Alidns delivers
	// whole, so the third is refused; four of the shortest fit
	budget := newExtenderDnsCountryTxtBudget(sample)
	for i := range 2 {
		if !budget.take(testExtenderDnsBase64Record(i, 368)) {
			t.Fatalf("record %d of 368 characters was refused", i)
		}
	}
	if budget.take(testExtenderDnsBase64Record(2, 368)) {
		t.Fatalf("a third record of 368 characters, a %d-byte answer, was taken", overhead+3*(14+368))
	}
	budget = newExtenderDnsCountryTxtBudget(sample)
	for i := range 4 {
		if !budget.take(testExtenderDnsBase64Record(i, 264)) {
			t.Fatalf("record %d of 264 characters was refused", i)
		}
	}
	if budget.take(testExtenderDnsBase64Record(4, 264)) {
		t.Fatalf("a fifth record of 264 characters, a %d-byte answer, was taken", overhead+5*(14+264))
	}
}

// The rotation is the tick's ten minutes: the same inside them, the next one
// after.
func TestExtenderDnsRotationIsTheTicksTenMinutes(t *testing.T) {
	start := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, extenderDnsRotation(start), extenderDnsRotation(start.Add(ExtenderPublishTimeout-time.Second)))
	connect.AssertEqual(t, extenderDnsRotation(start)+1, extenderDnsRotation(start.Add(ExtenderPublishTimeout)))
	connect.AssertEqual(t, extenderDnsRotation(start)+144, extenderDnsRotation(start.Add(24*time.Hour)))
}

// China's A and AAAA sets answer every family its vouched extender has, and a
// family it lacks is not answered by China's sets: Route 53 then answers
// China's question for that family from the Asia set. Tick by tick the zone
// follows, the set of a family China no longer has deleted like any set of
// ours.
func TestExtenderDnsCnSetsLeaveAFamilyTheirExtendersLack(t *testing.T) {
	// asia's extenders are single stack, half v4 and half v6, so China's
	// window holds one family or the other
	addresses := slices.Concat(
		testExtenderDnsAddresses("jp", 4, 1, 2, 3, 4),
		testExtenderDnsAddresses("sg", 6, 5, 6, 7, 8),
	)
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	api := newTestQuotaRoute53Api()
	publisher := &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}

	lacked := map[string]bool{}
	for rotation := range uint64(16) {
		desiredSets := sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, signer, 1, rotation)
		txtSet := testExtenderDnsChinaAnswerSet(desiredSets, route53.RRTypeTxt)
		if txtSet == nil || txtSet.countryCode != "CN" || len(txtSet.records) != 1 {
			t.Fatalf("rotation %d: China's TXT answer is %+v", rotation, txtSet)
		}
		recordFamilies := map[string]bool{}
		for _, address := range testDecodeTxtRecordBody(t, txtSet.records[0]).Addresses {
			if address.IpVersion == 6 {
				recordFamilies[route53.RRTypeAaaa] = true
			} else {
				recordFamilies[route53.RRTypeA] = true
			}
		}
		if _, err := publisher.publish(t.Context(), testExtenderDnsRecordName, testExtenderDnsTtl, desiredSets); err != nil {
			t.Fatalf("rotation %d: %v", rotation, err)
		}
		for _, recordType := range []string{route53.RRTypeA, route53.RRTypeAaaa} {
			answer := testExtenderDnsChinaAnswerSet(desiredSets, recordType)
			_, zoneHolds := api.zone["extender-country-CN-"+recordType]
			if recordFamilies[recordType] {
				if answer == nil || answer.countryCode != "CN" || !zoneHolds {
					t.Fatalf("rotation %d: China's %s question is answered by %v, want China's own set", rotation, recordType, answer)
				}
				continue
			}
			lacked[recordType] = true
			if answer == nil || answer.setIdentifier() != "extender-AS-"+recordType || zoneHolds {
				t.Fatalf("rotation %d: China's %s question is answered by %v (zone holds China's: %t), want the Asia set", rotation, recordType, answer, zoneHolds)
			}
		}
	}
	if !lacked[route53.RRTypeA] || !lacked[route53.RRTypeAaaa] {
		t.Fatalf("sixteen ticks lacked only %v; the fixture should lack each family in turn", lacked)
	}
}
