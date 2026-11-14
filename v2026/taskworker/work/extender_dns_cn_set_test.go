package work

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// China's extender dns answers as China's clients get them (owner,
// 2026-11-12): the sets Route 53 answers China with, through China's resolvers
// as they answered on 2026-11-12, read by a released client.
//
// On 11-12 China was answered with the Asia set, 13 records in a 4.6 KB
// answer, and its resolvers did not deliver it: Alidns served 3 of the
// records, trimmed without being marked truncated, and DNSPod answered it
// truncated and empty. A client in China learned three extenders from dns, or
// none.
//
// The sampler tests draw the tick's sets through sampleTestExtenderDnsChinaSets,
// the last one runs the whole tick, and every one finds what China is answered
// with from the sets' Route 53 wire form, so the same tests run against the
// publisher from before China had sets of its own.

// The root key the real signer signs under.
var testExtenderDnsRootPrivateKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x11}, ed25519.SeedSize))

// The record of one sampler row's extender as the tick signs it
// (controller.SignExtenderRecord, extenderDnsRecordSigner), under rootPrivateKey
// at a fixed time, so the same rows sign the same bytes on every run.
func testExtenderDnsRealSigner(
	t testing.TB,
	addresses []*model.NetworkExtenderDnsAddress,
	rootPrivateKey ed25519.PrivateKey,
) extenderDnsRecordSigner {
	t.Helper()
	config := &controller.ExtenderConfig{NetworkHost: testExtenderWorkNetworkHost}
	issueTime := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	extenderRows := map[server.Id][]*model.NetworkExtenderDnsAddress{}
	for _, address := range addresses {
		extenderRows[address.ExtenderId] = append(extenderRows[address.ExtenderId], address)
	}
	return func(extenderId server.Id) (string, bool) {
		rows := extenderRows[extenderId]
		if len(rows) == 0 {
			return "", false
		}
		extender := &model.NetworkExtender{
			ExtenderId:    extenderId,
			PublicKey:     rows[0].PublicKey,
			TcpPort:       443,
			UdpPort:       443,
			DnsPort:       53,
			DnsTld:        connect.DefaultExtenderDnsTld,
			CountryCode:   rows[0].CountryCode,
			DirectoryTier: rows[0].DirectoryTier,
			CanaryChannel: rows[0].CanaryChannel,
		}
		recordAddresses := []*model.NetworkExtenderAddress{}
		for _, row := range rows {
			recordAddresses = append(recordAddresses, &model.NetworkExtenderAddress{
				IpVersion: row.IpVersion,
				Ip:        row.Ip,
				Carriers:  []string{connect.ExtenderCarrierTcp},
				Active:    true,
			})
		}
		_, message, err := controller.SignExtenderRecord(config, rootPrivateKey, extender, recordAddresses, issueTime)
		if err != nil {
			t.Fatalf("sign %s: %v", extenderId, err)
		}
		return base64.StdEncoding.EncodeToString(message), true
	}
}

// One dual-stack extender's rows, its key derived from its name so a run
// places it where the last run did.
func testExtenderDnsDualStackAddresses(countryCode string, index int) []*model.NetworkExtenderDnsAddress {
	extenderId := server.NewId()
	publicKey := testExtenderDnsKey(fmt.Sprintf("china-shaped/%s/%d", countryCode, index))
	addresses := []*model.NetworkExtenderDnsAddress{}
	for _, ipVersion := range extenderDnsIpVersions {
		addresses = append(addresses, &model.NetworkExtenderDnsAddress{
			ExtenderId:  extenderId,
			PublicKey:   publicKey,
			IpVersion:   ipVersion,
			Ip:          netip.MustParseAddr(testExtenderDnsIp(ipVersion, index)),
			CountryCode: countryCode,
		})
	}
	return addresses
}

// A directory shaped like Main's with Asia in it: twelve dual-stack extenders
// on each of three continents, so every location's sets are full and its TXT
// set is about the size of Main's.
func testExtenderDnsChinaShapedAddresses() []*model.NetworkExtenderDnsAddress {
	addresses := []*model.NetworkExtenderDnsAddress{}
	index := 1
	for _, countryCode := range []string{"jp", "us", "de"} {
		for range 12 {
			addresses = append(addresses, testExtenderDnsDualStackAddresses(countryCode, index)...)
			index += 1
		}
	}
	return addresses
}

// A directory shaped like Main's on 2026-11-12, when Route 53 answered China
// with 13 records in 4.6 KB: Main's extenders are mostly single stack, so on
// each of three continents two are dual stack, seven v4 only and seven v6
// only, and a location's A and AAAA samples draw different extenders, its TXT
// set carrying 14 records or more.
func testExtenderDnsMainSizedAddresses() []*model.NetworkExtenderDnsAddress {
	addresses := []*model.NetworkExtenderDnsAddress{}
	index := 1
	for _, countryCode := range []string{"jp", "us", "de"} {
		for range 2 {
			addresses = append(addresses, testExtenderDnsDualStackAddresses(countryCode, index)...)
			index += 1
		}
		for _, ipVersion := range []int{4, 4, 4, 4, 4, 4, 4, 6, 6, 6, 6, 6, 6, 6} {
			address := testExtenderDnsDualStackAddresses(countryCode, index)[0]
			if ipVersion == 6 {
				address = testExtenderDnsDualStackAddresses(countryCode, index)[1]
			}
			addresses = append(addresses, address)
			index += 1
		}
	}
	return addresses
}

// A set and its size, for a failure message, or "none".
func testExtenderDnsSetSummary(desiredSet *extenderDnsRecordSet) string {
	if desiredSet == nil {
		return "none"
	}
	return fmt.Sprintf("%s (%d records, %d addresses)", desiredSet.setIdentifier(), len(desiredSet.records), len(desiredSet.ips))
}

// A value of the length given in the base64 alphabet, distinct per index.
func testExtenderDnsBase64Record(index int, length int) string {
	prefix := fmt.Sprintf("%04d", index)
	return prefix + strings.Repeat("A", max(0, length-len(prefix)))
}

// The set Route 53 answers China's question of one type with, read from the
// sets' wire form: the CN country's set when there is one, else Asia's, else
// the default -- the smallest location that has a set of the type. Nil when no
// set of the type answers China.
func testExtenderDnsChinaAnswerSet(desiredSets []*extenderDnsRecordSet, recordType string) *extenderDnsRecordSet {
	var country, continent, fallback *extenderDnsRecordSet
	for _, desiredSet := range desiredSets {
		recordSet := extenderDnsResourceRecordSet(testExtenderDnsRecordName, testExtenderDnsTtl, desiredSet)
		if aws.StringValue(recordSet.Type) != recordType {
			continue
		}
		switch {
		case aws.StringValue(recordSet.GeoLocation.CountryCode) == "CN":
			country = desiredSet
		case aws.StringValue(recordSet.GeoLocation.ContinentCode) == "AS":
			continent = desiredSet
		case aws.StringValue(recordSet.GeoLocation.CountryCode) == "*":
			fallback = desiredSet
		}
	}
	for _, answer := range []*extenderDnsRecordSet{country, continent, fallback} {
		if answer != nil {
			return answer
		}
	}
	return nil
}

// The answer a forwarding resolver packs for one TXT set, as dnsmessage packs
// it with compression: the question, each record in strings of at most 255
// characters, and an OPT record with the client subnet it echoes, an IPv4 /24.
func testExtenderDnsChinaAnswer(t testing.TB, recordName string, records []string, truncated bool) []byte {
	t.Helper()
	name := dnsmessage.MustNewName(extenderDnsFqdn(recordName))
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		Response:           true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		Truncated:          truncated,
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
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}
	// family 1, source /24, scope /24, three bytes of the prefix
	clientSubnet := []byte{0, 1, 24, 24, 223, 5, 5}
	if err := builder.OPTResource(optHeader, dnsmessage.OPTResource{
		Options: []dnsmessage.Option{{Code: 8, Data: clientSubnet}},
	}); err != nil {
		t.Fatal(err)
	}
	message, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// A resolver the sdk gives a client in China, as it answered on 2026-11-12
// (extender_dns_publish.go): an answer within wholeBytes is delivered whole; a
// larger one is trimmed, unmarked, to the records that fit trimBytes; one of
// truncatedBytes or more is answered truncated and empty. Bytes are the whole
// DNS message.
type testChinaResolver struct {
	name           string
	wholeBytes     int
	trimBytes      int
	truncatedBytes int
}

var testChinaResolvers = []testChinaResolver{
	// 481 bytes whole; 948 to 2,297 trimmed at record boundaries to whole
	// records in at most 462; 4,511 truncated, empty
	{name: "DNSPod", wholeBytes: 481, trimBytes: 462, truncatedBytes: 4_511},
	// 1,144 bytes whole; every larger answer trimmed to 1,151 to 1,204
	{name: "Alidns", wholeBytes: 1_204, trimBytes: 1_204},
}

// The answer the resolver hands a client for Route 53's records, packed, and
// the records in it.
func (self testChinaResolver) deliver(t testing.TB, records []string) ([]byte, []string) {
	t.Helper()
	whole := testExtenderDnsChinaAnswer(t, testExtenderDnsRecordName, records, false)
	switch {
	case len(whole) <= self.wholeBytes:
		return whole, slices.Clone(records)
	case 0 < self.truncatedBytes && self.truncatedBytes <= len(whole):
		return testExtenderDnsChinaAnswer(t, testExtenderDnsRecordName, nil, true), nil
	}
	kept := []string{}
	for _, record := range records {
		if self.trimBytes < len(testExtenderDnsChinaAnswer(t, testExtenderDnsRecordName, append(slices.Clone(kept), record), false)) {
			break
		}
		kept = append(kept, record)
	}
	return testExtenderDnsChinaAnswer(t, testExtenderDnsRecordName, kept, false), kept
}

// What China's resolvers hand a client of China's TXT answer: Alidns every
// record of it, and DNSPod, which trims at record boundaries, a run of its
// whole records, at least one. A failure says why when either does not. A
// client takes the answer of whichever resolver answers first.
func testExtenderDnsChinaDelivered(t testing.TB, desiredSets []*extenderDnsRecordSet) (*extenderDnsRecordSet, []string, string) {
	t.Helper()
	txtSet := testExtenderDnsChinaAnswerSet(desiredSets, route53.RRTypeTxt)
	if txtSet == nil {
		return nil, nil, "no TXT set answers China"
	}
	whole := testExtenderDnsChinaAnswer(t, testExtenderDnsRecordName, txtSet.records, false)
	delivered := map[string][]string{}
	for _, resolver := range testChinaResolvers {
		_, delivered[resolver.name] = resolver.deliver(t, txtSet.records)
	}
	dnspod := delivered["DNSPod"]
	wholeRecords := 0 < len(dnspod) && !slices.ContainsFunc(dnspod, func(record string) bool {
		return !slices.Contains(txtSet.records, record)
	})
	if !slices.Equal(delivered["Alidns"], txtSet.records) || !wholeRecords {
		return txtSet, nil, fmt.Sprintf("China is answered by %s, %d records in a %d-byte answer; Alidns delivers %d of them whole, DNSPod %d",
			txtSet.setIdentifier(), len(txtSet.records), len(whole), len(delivered["Alidns"]), len(dnspod))
	}
	return txtSet, dnspod, ""
}

// The extender key a TXT value carries, hex.
func testExtenderDnsRecordKeyHex(t testing.TB, record string) string {
	t.Helper()
	return hex.EncodeToString(testDecodeTxtRecordBody(t, record).PublicKey)
}

// Whether a set is one of China's own: Route 53's CN country.
func testExtenderDnsIsChinaSet(desiredSet *extenderDnsRecordSet) bool {
	geoLocation := extenderDnsResourceRecordSet(testExtenderDnsRecordName, testExtenderDnsTtl, desiredSet).GeoLocation
	return aws.StringValue(geoLocation.CountryCode) == "CN"
}

// The identifiers of a tick's sets other than China's own, in the tick's order.
func testExtenderDnsNonChinaSetIdentifiers(desiredSets []*extenderDnsRecordSet) []string {
	setIdentifiers := []string{}
	for _, desiredSet := range desiredSets {
		if !testExtenderDnsIsChinaSet(desiredSet) {
			setIdentifiers = append(setIdentifiers, desiredSet.setIdentifier())
		}
	}
	return setIdentifiers
}

// China's own sets of a tick: a TXT set of minRecords to maxRecords records,
// whole in Alidns's answer and each whole in DNSPod's trimmed one, each
// verifying under rootPublicKey and the same bytes as the extender's record in
// every other set of the tick, and an A and an AAAA set answering exactly the
// addresses of the family those records name. A family none of them names has
// no set of China's.
func assertTestExtenderDnsChinaSets(
	t testing.TB,
	desiredSets []*extenderDnsRecordSet,
	minRecords int,
	maxRecords int,
	rootPublicKey ed25519.PublicKey,
) {
	t.Helper()
	chinaSets := map[string]*extenderDnsRecordSet{}
	otherRecords := map[string][]string{}
	for _, desiredSet := range desiredSets {
		if testExtenderDnsIsChinaSet(desiredSet) {
			chinaSets[desiredSet.recordType()] = desiredSet
			continue
		}
		for _, record := range desiredSet.records {
			keyHex := testExtenderDnsRecordKeyHex(t, record)
			otherRecords[keyHex] = append(otherRecords[keyHex], record)
		}
	}
	txtSet := chinaSets[route53.RRTypeTxt]
	if txtSet == nil || len(txtSet.records) < minRecords || maxRecords < len(txtSet.records) {
		t.Fatalf("China's own TXT set is %s, want %d to %d records; China is answered by %s",
			testExtenderDnsSetSummary(txtSet), minRecords, maxRecords,
			testExtenderDnsSetSummary(testExtenderDnsChinaAnswerSet(desiredSets, route53.RRTypeTxt)))
	}
	if _, _, failure := testExtenderDnsChinaDelivered(t, desiredSets); failure != "" {
		t.Fatal(failure)
	}
	keySet := connect.NewExtenderRootKeySet(rootPublicKey)
	named := map[string][]string{}
	for _, record := range txtSet.records {
		message, err := connect.DecodeExtenderDnsRecord(record)
		if err != nil {
			t.Fatalf("China's record: %v", err)
		}
		body, err := keySet.VerifyRecord(message.GetRecord())
		if err != nil {
			t.Fatalf("China's record does not verify under the root key: %v", err)
		}
		for _, other := range otherRecords[hex.EncodeToString(body.PublicKey)] {
			if other != record {
				t.Fatal("China's record of an extender is not the bytes its other sets carry")
			}
		}
		for _, address := range body.Addresses {
			recordType := route53.RRTypeA
			if address.IpVersion == 6 {
				recordType = route53.RRTypeAaaa
			}
			named[recordType] = append(named[recordType], address.Ip)
		}
	}
	for _, recordType := range []string{route53.RRTypeA, route53.RRTypeAaaa} {
		addressSet := chinaSets[recordType]
		if len(named[recordType]) == 0 {
			if addressSet != nil {
				t.Fatalf("China has a %s set, %v, which no record of its own names", recordType, addressSet.ips)
			}
			continue
		}
		if addressSet == nil {
			t.Fatalf("China has no %s set; its records name %v", recordType, named[recordType])
		}
		ips := slices.Clone(addressSet.ips)
		slices.Sort(ips)
		slices.Sort(named[recordType])
		if !slices.Equal(ips, named[recordType]) {
			t.Fatalf("China's %s set answers %v; its records name %v", recordType, ips, named[recordType])
		}
	}
}

// By default China is answered as many whole records as Alidns delivers --
// four of the shortest records Main signs, three of the usual ones, two of the
// longest -- and DNSPod, trimming that answer at record boundaries, still hands
// its clients one whole record of it: the best for users (owner, 2026-11-12).
// One record a lookup, which both would deliver whole, gave a client on Alidns
// a third of what it can carry.
func TestExtenderDnsChinaIsAnsweredAsManyWholeRecordsAsItsResolversDeliver(t *testing.T) {
	mainSized := testExtenderDnsMainSizedAddresses()
	addresses := testExtenderDnsChinaShapedAddresses()
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	for _, test := range []struct {
		name      string
		addresses []*model.NetworkExtenderDnsAddress
		signer    extenderDnsRecordSigner
		records   int
	}{
		// the shortest, average and longest records in Main's sets on 11-12, in
		// a directory of Main's shape
		{"264-character records", mainSized, testExtenderDnsSizedSigner(264), 4},
		{"Main-sized", mainSized, testExtenderDnsSizedSigner(327), 3},
		{"368-character records", mainSized, testExtenderDnsSizedSigner(368), 2},
		{"signed records", addresses, signer, 3},
	} {
		for rotation := range uint64(24) {
			desiredSets := sampleTestExtenderDnsChinaSets(test.addresses, 8, testExtenderDnsEpoch, test.signer, ExtenderDnsDefaultCnRecordCount, rotation)
			txtSet, dnspod, failure := testExtenderDnsChinaDelivered(t, desiredSets)
			if failure != "" {
				t.Fatalf("%s, tick %d: %s", test.name, rotation, failure)
			}
			if len(txtSet.records) != test.records {
				t.Fatalf("%s, tick %d: China is answered %d records, want the %d Alidns delivers whole", test.name, rotation, len(txtSet.records), test.records)
			}
			if len(dnspod) != 1 {
				t.Fatalf("%s, tick %d: DNSPod hands on %d of China's records, want one whole record", test.name, rotation, len(dnspod))
			}
		}
	}
}

// China's answer rotates tick by tick through the extenders China's vantage
// is dealt, so successive lookups cover them all: within an epoch each tick
// answers the next extender of the ranking until every one has been answered,
// and then the same cycle again. The same tick always answers the same
// extender. A day of ticks covers at least the ranking, and an epoch never
// more than the two family samples the vantage draws.
func TestExtenderDnsChinaSetRotatesThroughItsRanking(t *testing.T) {
	addresses := testExtenderDnsChinaShapedAddresses()
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	answered := func(epoch uint64, rotation uint64) string {
		t.Helper()
		txtSet, _, failure := testExtenderDnsChinaDelivered(t, sampleTestExtenderDnsChinaSets(addresses, 8, epoch, signer, 1, rotation))
		if failure != "" {
			t.Fatalf("epoch %d, tick %d: %s", epoch, rotation, failure)
		}
		if len(txtSet.records) != 1 {
			t.Fatalf("epoch %d, tick %d: China is answered %d records, want 1", epoch, rotation, len(txtSet.records))
		}
		return testExtenderDnsRecordKeyHex(t, txtSet.records[0])
	}

	sequence := []string{}
	for rotation := range uint64(64) {
		sequence = append(sequence, answered(testExtenderDnsEpoch, rotation))
	}
	connect.AssertEqual(t, answered(testExtenderDnsEpoch, 5), sequence[5])
	ranking := []string{}
	for _, keyHex := range sequence {
		if !slices.Contains(ranking, keyHex) {
			ranking = append(ranking, keyHex)
		}
	}
	if len(ranking) < 2 {
		t.Fatalf("sixty-four ticks answered China with %d extender", len(ranking))
	}
	if 2*8 < len(ranking) {
		t.Fatalf("one epoch answered China with %d extenders, more than its two samples of 8", len(ranking))
	}
	for i, keyHex := range sequence {
		if i < len(ranking) {
			if keyHex != ranking[i] {
				t.Fatalf("tick %d answered an extender an earlier tick of the cycle answered", i)
			}
			continue
		}
		if keyHex != sequence[i-len(ranking)] {
			t.Fatalf("tick %d left the cycle of %d", i, len(ranking))
		}
	}

	// a day: 144 ticks, the epoch turning every 6
	day := map[string]bool{}
	for tick := range uint64(144) {
		day[answered(testExtenderDnsEpoch+tick/6, tick)] = true
	}
	if len(day) < len(ranking) {
		t.Fatalf("a day of ticks answered China with %d extenders, fewer than one epoch's %d", len(day), len(ranking))
	}
	t.Logf("one epoch cycles through %d extenders; a day answers %d", len(ranking), len(day))

	// With the default count and records so long that two fit, each tick's set
	// still leads with the extender the one-record set of that tick holds:
	// every extender heads a set once a cycle, which is the record a resolver
	// that keeps one hands on, and every extender is in a set within the
	// cycle, however many of a window fit.
	longSigner := testExtenderDnsSizedSigner(368)
	shown := map[string]bool{}
	for rotation := range uint64(2 * len(ranking)) {
		one := testExtenderDnsChinaAnswerSet(sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, longSigner, 1, rotation), route53.RRTypeTxt)
		txtSet, _, failure := testExtenderDnsChinaDelivered(t, sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, longSigner, ExtenderDnsDefaultCnRecordCount, rotation))
		if failure != "" {
			t.Fatalf("368-character records, tick %d: %s", rotation, failure)
		}
		if one == nil || len(txtSet.records) != 2 || txtSet.records[0] != one.records[0] {
			t.Fatalf("368-character records, tick %d: China's set of %d records does not lead with the extender of the tick's one-record set", rotation, len(txtSet.records))
		}
		if rotation < uint64(len(ranking)) {
			for _, record := range txtSet.records {
				shown[record] = true
			}
		}
	}
	if len(shown) != len(ranking) {
		t.Fatalf("one cycle of two-record sets answered China with %d of its %d extenders", len(shown), len(ranking))
	}
}

// A dns canary of Asia stays at the head of China's set while a slot is left
// to rotate: with two records, every tick answers the canary first and the
// second record rotates. With one, the canary takes its turn in the rotation
// rather than answering China alone for ever.
func TestExtenderDnsChinaSetKeepsItsCanariesFirst(t *testing.T) {
	canary := testExtenderDnsDualStackAddresses("jp", 99)
	for _, address := range canary {
		address.CanaryChannel = connect.ExtenderChannelDns
	}
	canaryKeyHex := hex.EncodeToString(canary[0].PublicKey)
	addresses := slices.Concat(testExtenderDnsChinaShapedAddresses(), canary)
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	canaryIps := []string{canary[0].Ip.String(), canary[1].Ip.String()}

	seconds := map[string]bool{}
	for rotation := range uint64(16) {
		desiredSets := sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, signer, 2, rotation)
		txtSet := testExtenderDnsChinaAnswerSet(desiredSets, route53.RRTypeTxt)
		if txtSet == nil || len(txtSet.records) != 2 {
			t.Fatalf("tick %d: with two records China's TXT answer is %s", rotation, testExtenderDnsSetSummary(txtSet))
		}
		if testExtenderDnsRecordKeyHex(t, txtSet.records[0]) != canaryKeyHex {
			t.Fatalf("tick %d: China's first record is not Asia's canary", rotation)
		}
		seconds[testExtenderDnsRecordKeyHex(t, txtSet.records[1])] = true
		for i, recordType := range []string{route53.RRTypeA, route53.RRTypeAaaa} {
			answer := testExtenderDnsChinaAnswerSet(desiredSets, recordType)
			if answer == nil || len(answer.ips) == 0 || answer.ips[0] != canaryIps[i] {
				t.Fatalf("tick %d: China's %s answer %s does not lead with the canary", rotation, recordType, testExtenderDnsSetSummary(answer))
			}
		}
	}
	if len(seconds) < 2 {
		t.Fatalf("the second record of China's set never rotated: %d extenders", len(seconds))
	}

	canaryTicks := 0
	for rotation := range uint64(32) {
		desiredSets := sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, signer, 1, rotation)
		txtSet, _, failure := testExtenderDnsChinaDelivered(t, desiredSets)
		if failure != "" {
			t.Fatalf("tick %d: %s", rotation, failure)
		}
		if len(txtSet.records) == 1 && testExtenderDnsRecordKeyHex(t, txtSet.records[0]) == canaryKeyHex {
			canaryTicks += 1
		}
	}
	if canaryTicks == 0 || 32 <= canaryTicks {
		t.Fatalf("with one record the canary answered China in %d of 32 ticks, want its turn and no more", canaryTicks)
	}
}

// Every address Route 53 answers China with is one a record China's
// resolvers deliver whole vouches for, and every address those records name
// is answered: a released client dials only what a record vouched for, so an
// address without its record is a slot no client in China can use.
func TestExtenderDnsChinaIsAnsweredOnlyAddressesItsRecordsVouchFor(t *testing.T) {
	addresses := testExtenderDnsChinaShapedAddresses()
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	for _, recordCount := range []int{1, 2, ExtenderDnsDefaultCnRecordCount} {
		for rotation := range uint64(24) {
			desiredSets := sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, signer, recordCount, rotation)
			// a record Alidns delivers whole vouches for its addresses to the
			// clients it answers; DNSPod's clients get one of them
			records := []string{}
			if txtSet := testExtenderDnsChinaAnswerSet(desiredSets, route53.RRTypeTxt); txtSet != nil {
				_, records = testChinaResolvers[1].deliver(t, txtSet.records)
				if !slices.Equal(records, txtSet.records) {
					records = nil
				}
			}
			vouched := map[string][]string{}
			for _, record := range records {
				for _, address := range testDecodeTxtRecordBody(t, record).Addresses {
					recordType := route53.RRTypeA
					if address.IpVersion == 6 {
						recordType = route53.RRTypeAaaa
					}
					vouched[recordType] = append(vouched[recordType], address.Ip)
				}
			}
			for _, recordType := range []string{route53.RRTypeA, route53.RRTypeAaaa} {
				answer := testExtenderDnsChinaAnswerSet(desiredSets, recordType)
				if answer == nil {
					t.Fatalf("%d records, tick %d: no %s set answers China", recordCount, rotation, recordType)
				}
				for _, ip := range answer.ips {
					if !slices.Contains(vouched[recordType], ip) {
						t.Fatalf("%d records, tick %d: %s answers China %s, which no record China's resolvers deliver whole vouches for (%d records delivered)",
							recordCount, rotation, answer.setIdentifier(), ip, len(records))
					}
				}
				for _, ip := range vouched[recordType] {
					if !slices.Contains(answer.ips, ip) {
						t.Fatalf("%d records, tick %d: China's record names %s, which %s does not answer", recordCount, rotation, ip, answer.setIdentifier())
					}
				}
			}
		}
	}
}

// A released client reads every record a resolver of China hands it and
// accepts each: through Alidns the whole of China's answer, through DNSPod
// the whole records it trimmed the answer to, at least one. Read by the reader
// of connect v2026.10.27 (mirrored, testReleasedReaderTxts) and by connect's
// own DoH TXT query against a server that answers as the resolver does, each
// value decoded as a client decodes it and verified under the root key.
func TestExtenderDnsChinaAnswerReadsAsAReleasedClientReadsIt(t *testing.T) {
	addresses := testExtenderDnsChinaShapedAddresses()
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	rootKeySet := connect.NewExtenderRootKeySet(testExtenderDnsRootPrivateKey.Public().(ed25519.PublicKey))

	var servedLock sync.Mutex
	var served []byte
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		servedLock.Lock()
		body := served
		servedLock.Unlock()
		writer.Header().Set("Content-Type", "application/dns-message")
		writer.Write(body)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	settings := connect.DefaultDohSettings()
	settings.Log = connect.NewNoopLogger()
	settings.DnsResolverSettings = &connect.DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{server.URL + "/dns-query"},
		TlsConfig:         &tls.Config{RootCAs: pool},
	}

	for rotation := range uint64(4) {
		desiredSets := sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, signer, ExtenderDnsDefaultCnRecordCount, rotation)
		txtSet, _, failure := testExtenderDnsChinaDelivered(t, desiredSets)
		if failure != "" {
			t.Fatalf("tick %d: %s", rotation, failure)
		}
		for _, resolver := range testChinaResolvers {
			answer, delivered := resolver.deliver(t, txtSet.records)
			servedLock.Lock()
			served = answer
			servedLock.Unlock()
			released := testReleasedReaderTxts(answer)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			current := connect.DohQueryTxt(ctx, settings, testExtenderDnsRecordName)
			cancel()
			if len(delivered) == 0 || !slices.Equal(released, delivered) || !slices.Equal(current, delivered) {
				t.Fatalf("tick %d: through %s, which hands on %d of %s's %d records (a %d-byte answer), a released client reads %d and connect %d",
					rotation, resolver.name, len(delivered), txtSet.setIdentifier(), len(txtSet.records), len(answer), len(released), len(current))
			}
			for _, txt := range current {
				message, err := connect.DecodeExtenderDnsRecord(txt)
				if err != nil {
					t.Fatalf("tick %d: %v", rotation, err)
				}
				body, err := rootKeySet.VerifyRecord(message.GetRecord())
				if err != nil {
					t.Fatalf("tick %d: China's record does not verify: %v", rotation, err)
				}
				for _, recordType := range []string{route53.RRTypeA, route53.RRTypeAaaa} {
					answer := testExtenderDnsChinaAnswerSet(desiredSets, recordType)
					for _, address := range body.Addresses {
						if (address.IpVersion == 6) == (recordType == route53.RRTypeAaaa) && !slices.Contains(answer.ips, address.Ip) {
							t.Fatalf("tick %d: China's record names %s, which its %s answer leaves out", rotation, address.Ip, recordType)
						}
					}
				}
			}
		}
	}
}

// China's sets change no other location's: with them or without them, every
// continent's and the default's sets are the same sets, in the same order,
// holding the same addresses and records.
func TestExtenderDnsChinaSetLeavesEveryOtherLocationAlone(t *testing.T) {
	addresses := slices.Concat(testExtenderDnsChinaShapedAddresses(), testExtenderDnsAddresses("br", 4, 200, 201))
	for name, signer := range map[string]extenderDnsRecordSigner{
		"signed records": testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey),
		// large enough that the TXT budget holds records back
		"1,500-character records": testExtenderDnsSizedSigner(1_500),
	} {
		for _, epoch := range []uint64{testExtenderDnsEpoch, testExtenderDnsEpoch + 1} {
			without := sampleExtenderDnsRecordSets(addresses, 8, testExtenderDnsSecret, epoch, signer)
			with := []*extenderDnsRecordSet{}
			for _, desiredSet := range sampleTestExtenderDnsChinaSets(addresses, 8, epoch, signer, ExtenderDnsDefaultCnRecordCount, epoch*7) {
				geoLocation := extenderDnsResourceRecordSet(testExtenderDnsRecordName, testExtenderDnsTtl, desiredSet).GeoLocation
				if aws.StringValue(geoLocation.CountryCode) == "CN" {
					continue
				}
				with = append(with, desiredSet)
			}
			if len(with) != len(without) {
				t.Fatalf("%s, epoch %d: %d other sets with China's, %d without", name, epoch, len(with), len(without))
			}
			for i := range without {
				if with[i].setIdentifier() != without[i].setIdentifier() ||
					!slices.Equal(with[i].ips, without[i].ips) ||
					!slices.Equal(with[i].records, without[i].records) {
					t.Fatalf("%s, epoch %d: %s changed with China's sets", name, epoch, without[i].setIdentifier())
				}
			}
		}
	}
}

// The country gauge says what Route 53 accepted of China's sets: its records
// and the addresses they vouch for, and a zero TXT value on a tick Route 53
// refused TXT sets, while the address sets that landed still count.
func TestExtenderDnsChinaGaugeReportsWhatRoute53Accepted(t *testing.T) {
	addresses := testExtenderDnsChinaShapedAddresses()
	signer := testExtenderDnsRealSigner(t, addresses, testExtenderDnsRootPrivateKey)
	for _, refusedRecordType := range []string{"", route53.RRTypeTxt} {
		desiredSets := sampleTestExtenderDnsChinaSets(addresses, 8, testExtenderDnsEpoch, signer, ExtenderDnsDefaultCnRecordCount, 3)
		api := newTestQuotaRoute53Api()
		api.refusedRecordType = refusedRecordType
		publisher := &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}
		changes, _ := publisher.publish(context.Background(), testExtenderDnsRecordName, testExtenderDnsTtl, desiredSets)
		acceptedSets := []*extenderDnsRecordSet{}
		for _, change := range changes {
			if change.err == nil && change.desiredSet != nil {
				acceptedSets = append(acceptedSets, change.desiredSet)
			}
		}
		observeExtenderDnsSets(acceptedSets, addresses)

		landed := map[string]float64{}
		for _, recordSet := range api.zone {
			if aws.StringValue(recordSet.GeoLocation.CountryCode) == "CN" {
				landed[aws.StringValue(recordSet.Type)] += float64(len(recordSet.ResourceRecords))
			}
		}
		if refusedRecordType == "" && (landed[route53.RRTypeTxt] < 2 || landed[route53.RRTypeA] != landed[route53.RRTypeTxt]) {
			t.Fatalf("Route 53 holds %v of China's records by type, want the records Alidns delivers whole and an address of each", landed)
		}
		for _, recordType := range extenderDnsRecordTypes {
			want := landed[recordType]
			got, ok := testGatheredExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_country_records",
				map[string]string{"country": "CN", "record_type": recordType})
			if !ok || got != want {
				t.Fatalf("refused %q: the China %s gauge reads %v (exported %t); Route 53 accepted %v", refusedRecordType, recordType, got, ok, want)
			}
		}
	}
}

// The tick answers China with as many records as the dns block's
// cn_record_count says, each signed under the operator's root key, and with
// exactly the addresses those records name.
func TestExtenderDnsPublishAnswersChinaWithItsConfiguredRecordCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installTestExtenderWorkConfig(t, append(testExtenderDnsConfigYaml(), "  cn_record_count: 2")...)
		publisher := stubExtenderDnsPublisher(t)

		bothFamilies := []testExtenderDnsAddressSpec{
			{ipVersion: 4, active: true},
			{ipVersion: 6, active: true},
		}
		for _, index := range []int{1, 2, 3} {
			createTestDnsExtender(ctx, index, "jp", true, bothFamilies...)
		}
		for _, index := range []int{4, 5} {
			createTestDnsExtender(ctx, index, "us", true, bothFamilies...)
		}

		runTestExtenderPublish(t, ctx)
		assertTestExtenderDnsChinaSets(t, publisher.onlyApply(t).upsertSets, 2, 2, rootPublicKey)
	})
}
