package work

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The TXT half of the geo dns sets (connect/EXTENDER.md C5).
//
// Beside each location's A and AAAA sets goes a TXT set carrying the signed
// record of every extender behind the addresses those sets answer with. A
// client that resolves the name gets the addresses and, in the same answer,
// the operator's signature over them, so its bootstrap lands verified (E3)
// instead of waiting on gossip to vouch for what dns already said.

// testExtenderDnsAddress is one row of one extender, for a sampler test that
// needs several rows to share an extender.
func testExtenderDnsAddress(
	extenderId server.Id,
	countryCode string,
	ipVersion int,
	index int,
) *model.NetworkExtenderDnsAddress {
	return &model.NetworkExtenderDnsAddress{
		ExtenderId:  extenderId,
		IpVersion:   ipVersion,
		Ip:          netip.MustParseAddr(testExtenderDnsIp(ipVersion, index)),
		CountryCode: countryCode,
	}
}

// The txt set of a location, or nil.
func testExtenderDnsTxtSet(desiredSets []*extenderDnsRecordSet, continentCode string) *extenderDnsRecordSet {
	for _, desiredSet := range desiredSets {
		if desiredSet.ipVersion == 0 && desiredSet.continentCode == continentCode {
			return desiredSet
		}
	}
	return nil
}

// Every location's txt set holds one record per distinct extender behind the
// addresses of that location's sets, however many of its addresses and
// families were drawn; an extender the signer has nothing for is left out of
// the txt set without its addresses being touched; and each extender is
// signed once per tick.
func TestExtenderDnsSampleAddsATxtSetPerLocation(t *testing.T) {
	usId := server.NewId()
	deId := server.NewId()
	refusedId := server.NewId()
	addresses := []*model.NetworkExtenderDnsAddress{
		// a dual-stack extender, whose two addresses are one record
		testExtenderDnsAddress(usId, "us", 4, 1),
		testExtenderDnsAddress(usId, "us", 6, 1),
		testExtenderDnsAddress(deId, "de", 4, 2),
		testExtenderDnsAddress(refusedId, "us", 4, 3),
	}
	signCounts := map[server.Id]int{}
	signRecord := func(extenderId server.Id) (string, bool) {
		signCounts[extenderId] += 1
		if extenderId == refusedId {
			return "", false
		}
		return "record-" + extenderId.String(), true
	}

	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		8,
		mathrand.New(mathrand.NewPCG(7, 11)),
		signRecord,
	)

	// the address sets are exactly what they were without a signer
	connect.AssertEqual(t, len(desiredSets), 8)
	for _, setIdentifier := range []string{"extender-NA-A", "extender-EU-A", "extender-default-A"} {
		assertTestExtenderDnsSet(t, desiredSets, setIdentifier, 3, testExtenderDnsIps(4, 1, 2, 3))
	}
	for _, setIdentifier := range []string{"extender-NA-AAAA", "extender-default-AAAA"} {
		assertTestExtenderDnsSet(t, desiredSets, setIdentifier, 1, testExtenderDnsIps(6, 1))
	}

	want := []string{"record-" + deId.String(), "record-" + usId.String()}
	slices.Sort(want)
	for _, continentCode := range []string{"NA", "EU", ""} {
		txtSet := testExtenderDnsTxtSet(desiredSets, continentCode)
		if txtSet == nil {
			t.Fatalf("there is no txt set for %q", continentCode)
		}
		connect.AssertEqual(t, txtSet.recordType(), route53.RRTypeTxt)
		records := slices.Clone(txtSet.records)
		slices.Sort(records)
		// the dual-stack extender once, the refused one not at all
		connect.AssertEqual(t, records, want)
		connect.AssertEqual(t, len(txtSet.ips), 0)
	}
	connect.AssertEqual(t, testExtenderDnsTxtSet(desiredSets, "NA").setIdentifier(), "extender-NA-TXT")
	connect.AssertEqual(t, testExtenderDnsTxtSet(desiredSets, "").setIdentifier(), "extender-default-TXT")

	// three locations, and each extender signed once for all of them
	for _, extenderId := range []server.Id{usId, deId, refusedId} {
		connect.AssertEqual(t, signCounts[extenderId], 1)
	}

	// and no signer, no txt sets
	unsigned := sampleExtenderDnsRecordSets(
		addresses,
		8,
		mathrand.New(mathrand.NewPCG(7, 11)),
		nil,
	)
	connect.AssertEqual(t, len(unsigned), 5)
	for _, desiredSet := range unsigned {
		if desiredSet.ipVersion == 0 {
			t.Fatalf("%s is a txt set, expected none without a signer", desiredSet.setIdentifier())
		}
	}
}

// A location whose address sets are all filled from elsewhere still gets the
// records of what it was filled with: what matters is what the client is
// answered with, not where the address lives.
func TestExtenderDnsSampleTxtCoversTheFill(t *testing.T) {
	usId := server.NewId()
	deId := server.NewId()
	addresses := []*model.NetworkExtenderDnsAddress{
		testExtenderDnsAddress(usId, "us", 4, 1),
		testExtenderDnsAddress(deId, "de", 4, 2),
	}
	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		8,
		mathrand.New(mathrand.NewPCG(7, 11)),
		func(extenderId server.Id) (string, bool) {
			return extenderId.String(), true
		},
	)
	// europe answers with its own and the fill from the us
	europe := assertTestExtenderDnsSet(t, desiredSets, "extender-EU-A", 2, testExtenderDnsIps(4, 1, 2))
	connect.AssertEqual(t, europe.ips[0], testExtenderDnsIp(4, 2))
	records := slices.Clone(testExtenderDnsTxtSet(desiredSets, "EU").records)
	slices.Sort(records)
	want := []string{usId.String(), deId.String()}
	slices.Sort(want)
	connect.AssertEqual(t, records, want)
}

// A txt value goes to Route 53 quoted and split into strings of at most 255
// characters, which is the wire limit of one character string; a resolver
// joins them back (RFC 1035, RFC 7208).
func TestExtenderDnsTxtValueIsQuotedAndChunked(t *testing.T) {
	connect.AssertEqual(t, extenderDnsTxtValue("abc"), `"abc"`)
	exact := strings.Repeat("a", 255)
	connect.AssertEqual(t, extenderDnsTxtValue(exact), `"`+exact+`"`)
	long := strings.Repeat("b", 255) + strings.Repeat("c", 45)
	connect.AssertEqual(
		t,
		extenderDnsTxtValue(long),
		`"`+strings.Repeat("b", 255)+`" "`+strings.Repeat("c", 45)+`"`,
	)
	// three strings for a value over 510
	longer := strings.Repeat("d", 600)
	connect.AssertEqual(t, strings.Count(extenderDnsTxtValue(longer), `" "`), 2)
}

// The Route 53 form of a txt set: type TXT, the location's geolocation, the
// values quoted and chunked; and a txt set of ours that is no longer desired
// is deleted like any other.
func TestRoute53ExtenderDnsPublishesTxtSets(t *testing.T) {
	name := testExtenderDnsRecordName + "."
	staleTxtSet := testRoute53RecordSet(name, "extender-AS-TXT", route53.RRTypeTxt, `"stale"`)
	foreignTxtSet := testRoute53RecordSet(name, "operator-TXT", route53.RRTypeTxt, `"not ours"`)
	api := &testRoute53Api{
		existingRecordSets: []*route53.ResourceRecordSet{staleTxtSet, foreignTxtSet},
	}
	publisher := &route53ExtenderDnsPublisher{
		api:          api,
		hostedZoneId: testExtenderDnsHostedZoneId,
	}

	long := strings.Repeat("x", 255) + strings.Repeat("y", 45)
	desiredSets := []*extenderDnsRecordSet{
		{continentCode: "EU", ipVersion: 4, ips: testExtenderDnsIps(4, 1)},
		{continentCode: "EU", records: []string{"short", long}},
		{records: []string{"short"}},
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
	connect.AssertEqual(t, len(changes), 4)

	connect.AssertEqual(t, aws.StringValue(changes[0].ResourceRecordSet.SetIdentifier), "extender-EU-A")

	europeTxt := changes[1].ResourceRecordSet
	connect.AssertEqual(t, aws.StringValue(changes[1].Action), route53.ChangeActionUpsert)
	connect.AssertEqual(t, aws.StringValue(europeTxt.SetIdentifier), "extender-EU-TXT")
	connect.AssertEqual(t, aws.StringValue(europeTxt.Type), route53.RRTypeTxt)
	connect.AssertEqual(t, aws.StringValue(europeTxt.Name), name)
	connect.AssertEqual(t, aws.Int64Value(europeTxt.TTL), int64(testExtenderDnsTtl))
	connect.AssertEqual(t, aws.StringValue(europeTxt.GeoLocation.ContinentCode), "EU")
	connect.AssertEqual(t, len(europeTxt.ResourceRecords), 2)
	connect.AssertEqual(t, aws.StringValue(europeTxt.ResourceRecords[0].Value), `"short"`)
	connect.AssertEqual(
		t,
		aws.StringValue(europeTxt.ResourceRecords[1].Value),
		`"`+strings.Repeat("x", 255)+`" "`+strings.Repeat("y", 45)+`"`,
	)

	defaultTxt := changes[2].ResourceRecordSet
	connect.AssertEqual(t, aws.StringValue(defaultTxt.SetIdentifier), "extender-default-TXT")
	connect.AssertEqual(t, aws.StringValue(defaultTxt.GeoLocation.CountryCode), "*")

	connect.AssertEqual(t, aws.StringValue(changes[3].Action), route53.ChangeActionDelete)
	if changes[3].ResourceRecordSet != staleTxtSet {
		t.Fatal("the stale txt set of ours was not the one deleted")
	}
	for _, change := range changes {
		if change.ResourceRecordSet == foreignTxtSet {
			t.Fatal("a txt set that is not ours was changed")
		}
	}
}

// The whole tick signs a fresh record per extender behind the sampled
// addresses, with the operator's root key and the same shape as the drip: a
// record fetched from dns verifies exactly as one fetched from gossip, names
// every active address of its extender, and is the same bytes wherever the
// extender appears.
func TestExtenderDnsPublishSignsATxtRecordPerExtender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		publisher := stubExtenderDnsPublisher(t)

		bothFamilies := []testExtenderDnsAddressSpec{
			{ipVersion: 4, active: true},
			{ipVersion: 6, active: true},
		}
		createTestDnsExtender(ctx, 1, "us", true, bothFamilies...)
		createTestDnsExtender(ctx, 2, "de", true, testExtenderDnsAddressSpec{ipVersion: 4, active: true})

		runTestExtenderPublish(t, ctx)
		batch := publisher.onlyBatch(t)
		connect.AssertEqual(t, batch.setIdentifiers(), []string{
			"extender-EU-A",
			"extender-NA-A",
			"extender-default-A",
			"extender-NA-AAAA",
			"extender-default-AAAA",
			"extender-EU-TXT",
			"extender-NA-TXT",
			"extender-default-TXT",
		})

		keySet := connect.NewExtenderRootKeySet(rootPublicKey)
		// every location answers with both extenders, so every txt set holds
		// both records, the dual-stack one once
		recordsByKey := map[string]string{}
		for _, setIdentifier := range []string{"extender-EU-TXT", "extender-NA-TXT", "extender-default-TXT"} {
			txtSet := batch.upsertSet(setIdentifier)
			if txtSet == nil {
				t.Fatalf("there is no %s set", setIdentifier)
			}
			connect.AssertEqual(t, len(txtSet.records), 2)
			for _, record := range txtSet.records {
				message, err := connect.DecodeExtenderDnsRecord(record)
				if err != nil {
					t.Fatalf("%s: %v", setIdentifier, err)
				}
				body, err := keySet.VerifyRecord(message.GetRecord())
				if err != nil {
					t.Fatalf("%s: the record does not verify under the root key: %v", setIdentifier, err)
				}
				connect.AssertEqual(t, body.NetworkHost, testExtenderWorkNetworkHost)
				ips := []string{}
				for _, address := range body.Addresses {
					ips = append(ips, address.Ip)
				}
				slices.Sort(ips)
				switch string(body.PublicKey) {
				case fmt.Sprintf("extender-public-key-dns-%07d", 1):
					// the dual-stack extender's record names both families
					connect.AssertEqual(t, ips, []string{testExtenderDnsIp(4, 1), testExtenderDnsIp(6, 1)})
				case fmt.Sprintf("extender-public-key-dns-%07d", 2):
					connect.AssertEqual(t, ips, []string{testExtenderDnsIp(4, 2)})
				default:
					t.Fatalf("%s: a record for an unknown key %q", setIdentifier, body.PublicKey)
				}
				// signed once: the same bytes in every set
				if previous, ok := recordsByKey[string(body.PublicKey)]; ok {
					connect.AssertEqual(t, record, previous)
				}
				recordsByKey[string(body.PublicKey)] = record
			}
		}
		connect.AssertEqual(t, len(recordsByKey), 2)
	})
}

// Without a root key there is nothing to sign with, and the signer says so
// for every extender rather than failing the tick. The tick itself never
// reaches the dns sets without a root key, since the drip is paused first;
// this pins the signer's half on its own.
func TestExtenderDnsRecordSignerRefusesWithoutARootKey(t *testing.T) {
	signRecord := newExtenderDnsRecordSigner(context.Background(), &controller.ExtenderConfig{})
	record, ok := signRecord(server.NewId())
	connect.AssertEqual(t, ok, false)
	connect.AssertEqual(t, record, "")
}
