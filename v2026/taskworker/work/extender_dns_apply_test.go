package work

// How one tick's changes are sent and reported (connect/EXTENDER.md C5): one
// request per set, upserts before deletes, a delete held while an upsert of
// its type failed, and every change the tick wanted reported with its
// outcome. And the arithmetic of the TXT budget each location is held to.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The outcome of every reported change, as "<set identifier> <outcome>".
func testExtenderDnsChangeOutcomes(changes []*extenderDnsChange) []string {
	outcomes := []string{}
	for _, change := range changes {
		outcome := extenderDnsChangeAccepted
		if change.err != nil {
			outcome = extenderDnsChangeFailed
		}
		outcomes = append(outcomes, change.setIdentifier+" "+outcome)
	}
	return outcomes
}

// A continent that went empty loses its set so that the default answers it
// instead. While the default of that type failed to land, the continent keeps
// its set: deleting it would leave the continent answered by nothing. Other
// types are not held back by it.
func TestRoute53ExtenderDnsHoldsADeleteWhileItsTypeFailed(t *testing.T) {
	name := testExtenderDnsRecordName + "."
	api := newTestQuotaRoute53Api()
	for _, recordSet := range []*route53.ResourceRecordSet{
		testRoute53RecordSet(name, "extender-AS-A", route53.RRTypeA, "198.51.100.90"),
		testRoute53RecordSet(name, "extender-AS-TXT", route53.RRTypeTxt, `"stale"`),
	} {
		setIdentifier := aws.StringValue(recordSet.SetIdentifier)
		api.zoneSetIdentifiers = append(api.zoneSetIdentifiers, setIdentifier)
		api.zone[setIdentifier] = recordSet
	}
	api.refusedRecordType = route53.RRTypeTxt
	publisher := &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}

	desiredSets := []*extenderDnsRecordSet{
		{ipVersion: 4, ips: testExtenderDnsIps(4, 1)},
		{records: []string{"record"}},
	}
	changes, err := publisher.publish(context.Background(), testExtenderDnsRecordName, testExtenderDnsTtl, desiredSets)
	if err == nil || !strings.Contains(err.Error(), "upsert extender-default-TXT") {
		t.Fatalf("publish err = %v, want the TXT upsert named", err)
	}

	connect.AssertEqual(t, testExtenderDnsChangeOutcomes(changes), []string{
		"extender-default-A accepted",
		"extender-default-TXT failed",
		"extender-AS-A accepted",
		"extender-AS-TXT failed",
	})
	// the held delete was never sent: the zone keeps the TXT set it had, and
	// lost the A set whose replacement landed
	connect.AssertEqual(t, api.zoneSetIdentifiers, []string{"extender-AS-TXT", "extender-default-A"})
	for _, input := range api.changeInputs {
		recordSet := input.ChangeBatch.Changes[0].ResourceRecordSet
		if aws.StringValue(input.ChangeBatch.Changes[0].Action) == route53.ChangeActionDelete &&
			aws.StringValue(recordSet.Type) == route53.RRTypeTxt {
			t.Fatal("a TXT delete was sent while the TXT upsert failed")
		}
	}
}

// A tick that cannot read the zone sends nothing and reports every set it
// wanted as failed, so a Route 53 that is unreachable reads as failures and
// not as a quiet tick.
func TestRoute53ExtenderDnsReportsUnsentChangesAsFailed(t *testing.T) {
	api := newTestQuotaRoute53Api()
	api.listErr = errTestRoute53Unreachable
	publisher := &route53ExtenderDnsPublisher{api: api, hostedZoneId: testExtenderDnsHostedZoneId}

	desiredSets := []*extenderDnsRecordSet{
		{continentCode: "EU", ipVersion: 4, ips: testExtenderDnsIps(4, 1)},
		{continentCode: "EU", records: []string{"record"}},
	}
	changes, err := publisher.publish(context.Background(), testExtenderDnsRecordName, testExtenderDnsTtl, desiredSets)
	if !errors.Is(err, errTestRoute53Unreachable) {
		t.Fatalf("publish err = %v, want the listing's", err)
	}
	connect.AssertEqual(t, testExtenderDnsChangeOutcomes(changes), []string{
		"extender-EU-A failed",
		"extender-EU-TXT failed",
	})
	connect.AssertEqual(t, len(api.changeInputs), 0)
}

// The tick logs an apply's failures as one line, though Route 53 puts its
// status on a second, and each failure still unwraps to its cause.
func TestExtenderDnsChangeErrorsAreOneLine(t *testing.T) {
	refused := errors.New("InvalidChangeBatch: [refused]\n\tstatus code: 400, request id: example")
	_, err := applyExtenderDnsChanges(
		nil,
		nil,
		[]*extenderDnsRecordSet{
			{continentCode: "EU", records: []string{"record"}},
			{records: []string{"record"}},
		},
		func(*extenderDnsRecordSet) error { return refused },
		func(string) error { return nil },
	)
	connect.AssertEqual(
		t,
		err.Error(),
		"upsert extender-EU-TXT: InvalidChangeBatch: [refused] status code: 400, request id: example; "+
			"upsert extender-default-TXT: InvalidChangeBatch: [refused] status code: 400, request id: example",
	)
	if !errors.Is(err, refused) {
		t.Fatal("the apply's error does not unwrap to the refusal")
	}

	// and an apply that landed everything has no error at all
	_, err = applyExtenderDnsChanges(nil, nil, []*extenderDnsRecordSet{{records: []string{"record"}}},
		func(*extenderDnsRecordSet) error { return nil },
		func(string) error { return nil },
	)
	if err != nil {
		t.Fatalf("an apply that landed everything returned %v", err)
	}
}

// The answer bytes of a TXT record: the compressed owner name and the fixed
// header (12), a length byte per string of at most 255 characters, and the
// characters.
func TestExtenderDnsTxtAnswerBytes(t *testing.T) {
	for _, c := range []struct {
		length int
		want   int
	}{
		{length: 0, want: 13},
		{length: 1, want: 14},
		{length: 255, want: 268},
		{length: 256, want: 270},
		{length: 1_500, want: 1_518},
	} {
		if got := extenderDnsTxtAnswerBytes(strings.Repeat("A", c.length)); got != c.want {
			t.Errorf("a %d-character record takes %d answer bytes, want %d", c.length, got, c.want)
		}
	}
}

// The budget takes a record only while both limits hold, and a record it
// refuses costs nothing, so a smaller record after it can still fit.
func TestExtenderDnsTxtBudgetTakesWhatFits(t *testing.T) {
	budget := &extenderDnsTxtBudget{}
	large := strings.Repeat("A", 7_000)
	if !budget.take(large) || !budget.take(large) {
		t.Fatal("two 7,000-character records must fit")
	}
	if budget.take(large) {
		t.Fatalf("a third 7,000-character record fit: %+v", budget)
	}
	// 14,080 answer bytes taken, 920 left, and the record takes 916
	if !budget.take(strings.Repeat("A", 900)) {
		t.Fatalf("a 900-character record must fit after a refusal: %+v", budget)
	}
	if budget.take("A") {
		t.Fatalf("a record past the answer budget fit: %+v", budget)
	}
	connect.AssertEqual(t, budget.answerBytes <= extenderDnsTxtMaxAnswerBytes, true)
	connect.AssertEqual(t, budget.valueCharacters <= extenderDnsTxtMaxValueCharacters, true)
}

// A location whose only record does not fit its budget has nothing it can
// vouch for, so it is answered with nothing of its own: its address sets and
// its TXT set are not desired, and a tick deletes what an earlier one wrote.
func TestExtenderDnsTxtBudgetDropsASetItEmptied(t *testing.T) {
	addresses := testExtenderDnsAddresses("us", 4, 1)
	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		8,
		testExtenderDnsSecret,
		testExtenderDnsEpoch,
		testExtenderDnsSizedSigner(extenderDnsTxtMaxAnswerBytes),
	)
	setIdentifiers := []string{}
	for _, desiredSet := range desiredSets {
		setIdentifiers = append(setIdentifiers, desiredSet.setIdentifier())
	}
	connect.AssertEqual(t, setIdentifiers, []string{})

	// without a signer the same address is answered, unvouched, as before
	unsigned := sampleExtenderDnsRecordSets(addresses, 8, testExtenderDnsSecret, testExtenderDnsEpoch, nil)
	connect.AssertEqual(t, len(unsigned), 2)
}

// An extender the signer has nothing for is left out of the TXT set without
// costing the budget anything, and keeps its addresses, as it did before the
// budget existed.
func TestExtenderDnsTxtBudgetSkipsAnExtenderWithoutARecord(t *testing.T) {
	addresses := testExtenderDnsAddresses("us", 4, 1, 2, 3)
	refused := addresses[0].ExtenderId
	signRecord := func(extenderId server.Id) (string, bool) {
		if extenderId == refused {
			return "", false
		}
		return testExtenderDnsSizedSigner(64)(extenderId)
	}
	desiredSets := sampleExtenderDnsRecordSets(addresses, 8, testExtenderDnsSecret, testExtenderDnsEpoch, signRecord)

	northAmerica := assertTestExtenderDnsSet(t, desiredSets, "extender-NA-A", 3, testExtenderDnsIps(4, 1, 2, 3))
	if !slices.Contains(northAmerica.ips, addresses[0].Ip.String()) {
		t.Fatalf("the address of an extender without a record was dropped: %v", northAmerica.ips)
	}
	connect.AssertEqual(t, len(testExtenderDnsTxtSet(desiredSets, "NA").records), 2)
}
