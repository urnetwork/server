package work

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

// The gossip record setup task (connect/EXTENDER.md C6).
//
// The task is driven through the same `route53Api` fake the geo dns publisher
// uses, so what is asserted here is the change batch the task computes rather
// than a mock's idea of what aws would have done. That is also what makes the
// idempotence assertion mean something: a second run over an unchanged source
// must compute the identical batch, since nothing reads the gossip name back.

const (
	testExtenderGossipDnsName       = "gossip." + testExtenderWorkNetworkHost
	testExtenderGossipDnsSourceName = "connect." + testExtenderWorkNetworkHost
	testExtenderGossipDnsTtl        = 300

	// the second space an operator answers on, which is a second zone
	testExtenderGossipDnsOtherHost       = "ur2.example"
	testExtenderGossipDnsOtherName       = "gossip." + testExtenderGossipDnsOtherHost
	testExtenderGossipDnsOtherSourceName = "connect." + testExtenderGossipDnsOtherHost
	testExtenderGossipDnsOtherZoneId     = "Z0EXAMPLEGOSSIPOTHER"
)

// The gossip_dns block the task tests run with, for the record names given.
func testExtenderGossipDnsConfigYaml(records ...[3]string) []string {
	yamlLines := []string{
		"gossip_dns:",
		"  enabled: true",
		"  aws_region: us-east-1",
		"  records:",
	}
	for _, record := range records {
		yamlLines = append(
			yamlLines,
			"    - hosted_zone_name: "+record[0],
			"      name: "+record[1],
			"      source_name: "+record[2],
		)
	}
	return yamlLines
}

// The one record of the single-zone tests.
func testExtenderGossipDnsRecord() [3]string {
	return [3]string{
		testExtenderWorkNetworkHost,
		testExtenderGossipDnsName,
		testExtenderGossipDnsSourceName,
	}
}

// One simple set at the source name.
func testExtenderGossipDnsSourceRecordSet(
	recordType string,
	ips ...string,
) *route53.ResourceRecordSet {
	resourceRecords := []*route53.ResourceRecord{}
	for _, ip := range ips {
		resourceRecords = append(resourceRecords, &route53.ResourceRecord{
			Value: aws.String(ip),
		})
	}
	return &route53.ResourceRecordSet{
		Name:            aws.String(testExtenderGossipDnsSourceName + "."),
		Type:            aws.String(recordType),
		TTL:             aws.Int64(testExtenderGossipDnsTtl),
		ResourceRecords: resourceRecords,
	}
}

// Replaces the api the task writes through, and drops any zone id an earlier
// test resolved so this one's resolutions are its own.
func stubExtenderGossipDnsApi(t testing.TB, api route53Api) {
	t.Helper()
	resetTestExtenderDnsHostedZoneIds(t)
	previousApi := newExtenderGossipDnsApi
	newExtenderGossipDnsApi = func(_ *controller.ExtenderConfig) (route53Api, error) {
		return api, nil
	}
	t.Cleanup(func() {
		newExtenderGossipDnsApi = previousApi
	})
}

func runTestExtenderGossipDns(t testing.TB, ctx context.Context) *ExtenderGossipDnsResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderGossipDns(&ExtenderGossipDnsArgs{}, clientSession)
	if err != nil {
		t.Fatalf("ExtenderGossipDns: %v", err)
	}
	return result
}

// The upserts of one change batch, named by the record set they carry.
func testExtenderGossipDnsUpserts(
	t testing.TB,
	input *route53.ChangeResourceRecordSetsInput,
) []*route53.ResourceRecordSet {
	t.Helper()
	recordSets := []*route53.ResourceRecordSet{}
	for _, change := range input.ChangeBatch.Changes {
		if action := aws.StringValue(change.Action); action != route53.ChangeActionUpsert {
			t.Fatalf("the batch carries a %s, and the setup task only upserts", action)
		}
		recordSets = append(recordSets, change.ResourceRecordSet)
	}
	return recordSets
}

// The values of one set, in the order the batch carries them.
func testExtenderGossipDnsValues(recordSet *route53.ResourceRecordSet) []string {
	values := []string{}
	for _, resourceRecord := range recordSet.ResourceRecords {
		values = append(values, aws.StringValue(resourceRecord.Value))
	}
	return values
}

// The block parses as written, including the record list, which is the shape
// an operator's `extender.yml` carries.
func TestExtenderGossipDnsConfiguration(t *testing.T) {
	installTestExtenderWorkConfig(
		t,
		testExtenderGossipDnsConfigYaml(
			testExtenderGossipDnsRecord(),
			[3]string{
				testExtenderGossipDnsOtherHost,
				testExtenderGossipDnsOtherName,
				testExtenderGossipDnsOtherSourceName,
			},
		)...,
	)
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatalf("EnvExtenderConfig: %v", err)
	}
	connect.AssertEqual(t, config.GossipDns.Enabled, true)
	connect.AssertEqual(t, config.GossipDns.AwsRegion, "us-east-1")
	connect.AssertEqual(t, len(config.GossipDns.Records), 2)
	connect.AssertEqual(t, config.GossipDns.Records[0].HostedZoneName, testExtenderWorkNetworkHost)
	connect.AssertEqual(t, config.GossipDns.Records[0].Name, testExtenderGossipDnsName)
	connect.AssertEqual(t, config.GossipDns.Records[0].SourceName, testExtenderGossipDnsSourceName)
	connect.AssertEqual(t, config.GossipDns.Records[1].HostedZoneName, testExtenderGossipDnsOtherHost)
	connect.AssertEqual(t, config.GossipDns.Records[1].Name, testExtenderGossipDnsOtherName)
	connect.AssertEqual(t, config.GossipDns.Records[1].SourceName, testExtenderGossipDnsOtherSourceName)

	// an operator with no gossip_dns block at all parses to an inert one
	installTestExtenderWorkConfig(t)
	config, err = controller.EnvExtenderConfig()
	if err != nil {
		t.Fatalf("EnvExtenderConfig: %v", err)
	}
	connect.AssertEqual(t, config.GossipDns.Enabled, false)
	connect.AssertEqual(t, len(config.GossipDns.Records), 0)
}

// A source with both families puts both under the gossip name, with the type,
// the ttl and the values of the source. The gossip service is reached at the
// same addresses connect is, so anything else would be this task inventing an
// answer.
func TestExtenderGossipDnsMirrorsBothFamilies(t *testing.T) {
	installTestExtenderWorkConfig(t, testExtenderGossipDnsConfigYaml(testExtenderGossipDnsRecord())...)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1", "198.51.100.2"),
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeAaaa, "2001:db8:1::1"),
			// another name, which the zone orders after ours
			testRoute53RecordSet(
				"zz."+testExtenderWorkNetworkHost+".",
				"",
				route53.RRTypeA,
				"198.51.100.9",
			),
		},
	}
	stubExtenderGossipDnsApi(t, api)

	result := runTestExtenderGossipDns(t, context.Background())
	connect.AssertEqual(t, result.Records, 1)
	connect.AssertEqual(t, result.Mirrored, 1)
	connect.AssertEqual(t, result.Skipped, 0)
	connect.AssertEqual(t, result.Failed, 0)

	// the source is read from the zone the name resolved to, starting at the
	// source name rather than at the top of the zone
	connect.AssertEqual(t, len(api.listInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.listInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)
	connect.AssertEqual(
		t,
		aws.StringValue(api.listInputs[0].StartRecordName),
		testExtenderGossipDnsSourceName+".",
	)

	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)
	upserts := testExtenderGossipDnsUpserts(t, api.changeInputs[0])
	connect.AssertEqual(t, len(upserts), 2)

	connect.AssertEqual(t, aws.StringValue(upserts[0].Name), testExtenderGossipDnsName+".")
	connect.AssertEqual(t, aws.StringValue(upserts[0].Type), route53.RRTypeA)
	connect.AssertEqual(t, aws.Int64Value(upserts[0].TTL), int64(testExtenderGossipDnsTtl))
	connect.AssertEqual(t, testExtenderGossipDnsValues(upserts[0]), []string{"198.51.100.1", "198.51.100.2"})
	connect.AssertEqual(t, upserts[0].AliasTarget, nil)

	connect.AssertEqual(t, aws.StringValue(upserts[1].Name), testExtenderGossipDnsName+".")
	connect.AssertEqual(t, aws.StringValue(upserts[1].Type), route53.RRTypeAaaa)
	connect.AssertEqual(t, aws.Int64Value(upserts[1].TTL), int64(testExtenderGossipDnsTtl))
	connect.AssertEqual(t, testExtenderGossipDnsValues(upserts[1]), []string{"2001:db8:1::1"})
}

// A second run over an unchanged source computes the identical batch. Route 53
// takes the repeated upsert as a no-op, which is what lets the daily cadence
// write without reading the gossip name back and diffing it.
func TestExtenderGossipDnsIsIdempotent(t *testing.T) {
	installTestExtenderWorkConfig(t, testExtenderGossipDnsConfigYaml(testExtenderGossipDnsRecord())...)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1"),
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeAaaa, "2001:db8:1::1"),
		},
	}
	stubExtenderGossipDnsApi(t, api)

	runTestExtenderGossipDns(t, context.Background())
	runTestExtenderGossipDns(t, context.Background())

	connect.AssertEqual(t, len(api.changeInputs), 2)
	// and the zone was resolved once for the process, not once per run
	connect.AssertEqual(t, len(api.hostedZoneInputs), 1)

	first := testExtenderGossipDnsUpserts(t, api.changeInputs[0])
	second := testExtenderGossipDnsUpserts(t, api.changeInputs[1])
	connect.AssertEqual(t, len(second), len(first))
	for i, firstUpsert := range first {
		secondUpsert := second[i]
		connect.AssertEqual(t, aws.StringValue(secondUpsert.Name), aws.StringValue(firstUpsert.Name))
		connect.AssertEqual(t, aws.StringValue(secondUpsert.Type), aws.StringValue(firstUpsert.Type))
		connect.AssertEqual(t, aws.Int64Value(secondUpsert.TTL), aws.Int64Value(firstUpsert.TTL))
		connect.AssertEqual(
			t,
			testExtenderGossipDnsValues(secondUpsert),
			testExtenderGossipDnsValues(firstUpsert),
		)
	}
}

// An alias source is copied as an alias. A load balancer's addresses change
// without anyone editing dns, so a gossip name pinned to the addresses the
// alias resolves to today would point at nothing tomorrow.
func TestExtenderGossipDnsCopiesAnAliasTarget(t *testing.T) {
	installTestExtenderWorkConfig(t, testExtenderGossipDnsConfigYaml(testExtenderGossipDnsRecord())...)
	aliasSet := &route53.ResourceRecordSet{
		Name: aws.String(testExtenderGossipDnsSourceName + "."),
		Type: aws.String(route53.RRTypeA),
		AliasTarget: &route53.AliasTarget{
			DNSName:              aws.String("balancer." + testExtenderWorkNetworkHost + "."),
			HostedZoneId:         aws.String("Z0EXAMPLEBALANCER"),
			EvaluateTargetHealth: aws.Bool(true),
		},
	}
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{aliasSet},
	}
	stubExtenderGossipDnsApi(t, api)

	result := runTestExtenderGossipDns(t, context.Background())
	connect.AssertEqual(t, result.Mirrored, 1)

	upserts := testExtenderGossipDnsUpserts(t, api.changeInputs[0])
	connect.AssertEqual(t, len(upserts), 1)
	connect.AssertEqual(t, aws.StringValue(upserts[0].Name), testExtenderGossipDnsName+".")
	connect.AssertEqual(t, aws.StringValue(upserts[0].Type), route53.RRTypeA)
	connect.AssertEqual(
		t,
		aws.StringValue(upserts[0].AliasTarget.DNSName),
		"balancer."+testExtenderWorkNetworkHost+".",
	)
	connect.AssertEqual(t, aws.StringValue(upserts[0].AliasTarget.HostedZoneId), "Z0EXAMPLEBALANCER")
	connect.AssertEqual(t, aws.BoolValue(upserts[0].AliasTarget.EvaluateTargetHealth), true)
	// an alias carries no ttl of its own, and route 53 rejects a set that has
	// both
	connect.AssertEqual(t, upserts[0].TTL, nil)
	// the copy is the task's own set rather than the source's, so a later
	// change to one cannot reach the other
	if upserts[0].AliasTarget == aliasSet.AliasTarget {
		t.Fatalf("the upsert shares the source's alias target")
	}
}

// A source with neither an A nor an AAAA set has nothing to mirror. That is
// what an operator sees while the connect record is still being created, so it
// is logged and skipped rather than written as an empty record.
func TestExtenderGossipDnsSkipsASourceWithNoRecords(t *testing.T) {
	installTestExtenderWorkConfig(t, testExtenderGossipDnsConfigYaml(testExtenderGossipDnsRecord())...)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{
			// a text record at the source name is not an address
			{
				Name: aws.String(testExtenderGossipDnsSourceName + "."),
				Type: aws.String(route53.RRTypeTxt),
				TTL:  aws.Int64(testExtenderGossipDnsTtl),
				ResourceRecords: []*route53.ResourceRecord{
					{Value: aws.String(`"v=spf1 -all"`)},
				},
			},
		},
	}
	stubExtenderGossipDnsApi(t, api)

	result := runTestExtenderGossipDns(t, context.Background())
	connect.AssertEqual(t, result.Records, 1)
	connect.AssertEqual(t, result.Mirrored, 0)
	connect.AssertEqual(t, result.Skipped, 1)
	connect.AssertEqual(t, result.Failed, 0)
	connect.AssertEqual(t, len(api.changeInputs), 0)
}

// A record whose zone cannot be named costs only itself. The records are
// independent names in independent zones, and one operator typo must not stop
// the rest being maintained.
func TestExtenderGossipDnsMissingZoneFailsOnlyItsRecord(t *testing.T) {
	installTestExtenderWorkConfig(
		t,
		testExtenderGossipDnsConfigYaml(
			// the first record names a zone that is not in the account
			[3]string{
				"missing.example",
				"gossip.missing.example",
				"connect.missing.example",
			},
			testExtenderGossipDnsRecord(),
		)...,
	)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1"),
		},
	}
	stubExtenderGossipDnsApi(t, api)

	result := runTestExtenderGossipDns(t, context.Background())
	connect.AssertEqual(t, result.Records, 2)
	connect.AssertEqual(t, result.Mirrored, 1)
	connect.AssertEqual(t, result.Failed, 1)

	// the surviving record was still written, and the missing zone was never
	// listed
	connect.AssertEqual(t, len(api.changeInputs), 1)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)
	upserts := testExtenderGossipDnsUpserts(t, api.changeInputs[0])
	connect.AssertEqual(t, len(upserts), 1)
	connect.AssertEqual(t, aws.StringValue(upserts[0].Name), testExtenderGossipDnsName+".")
	connect.AssertEqual(t, len(api.listInputs), 1)
}

// Two names in two zones are two batches, each atomic. One zone's records land
// together or not at all, which is what keeps a name and its family from being
// half updated.
func TestExtenderGossipDnsWritesOneBatchPerZone(t *testing.T) {
	installTestExtenderWorkConfig(
		t,
		testExtenderGossipDnsConfigYaml(
			testExtenderGossipDnsRecord(),
			[3]string{
				testExtenderGossipDnsOtherHost,
				testExtenderGossipDnsOtherName,
				testExtenderGossipDnsOtherSourceName,
			},
		)...,
	)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
			testRoute53HostedZone(
				testExtenderGossipDnsOtherHost+".",
				testExtenderGossipDnsOtherZoneId,
			),
		},
		zoneRecordSets: map[string][]*route53.ResourceRecordSet{
			testExtenderDnsNamedZoneId: {
				testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1"),
			},
			testExtenderGossipDnsOtherZoneId: {
				{
					Name: aws.String(testExtenderGossipDnsOtherSourceName + "."),
					Type: aws.String(route53.RRTypeAaaa),
					TTL:  aws.Int64(testExtenderGossipDnsTtl),
					ResourceRecords: []*route53.ResourceRecord{
						{Value: aws.String("2001:db8:2::1")},
					},
				},
			},
		},
	}
	stubExtenderGossipDnsApi(t, api)

	result := runTestExtenderGossipDns(t, context.Background())
	connect.AssertEqual(t, result.Mirrored, 2)
	connect.AssertEqual(t, result.Failed, 0)

	connect.AssertEqual(t, len(api.changeInputs), 2)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[0].HostedZoneId),
		testExtenderDnsNamedZoneId,
	)
	connect.AssertEqual(
		t,
		aws.StringValue(api.changeInputs[1].HostedZoneId),
		testExtenderGossipDnsOtherZoneId,
	)
	firstUpserts := testExtenderGossipDnsUpserts(t, api.changeInputs[0])
	connect.AssertEqual(t, len(firstUpserts), 1)
	connect.AssertEqual(t, aws.StringValue(firstUpserts[0].Name), testExtenderGossipDnsName+".")
	secondUpserts := testExtenderGossipDnsUpserts(t, api.changeInputs[1])
	connect.AssertEqual(t, len(secondUpserts), 1)
	connect.AssertEqual(t, aws.StringValue(secondUpserts[0].Name), testExtenderGossipDnsOtherName+".")
	connect.AssertEqual(t, testExtenderGossipDnsValues(secondUpserts[0]), []string{"2001:db8:2::1"})
}

// A deployment with no gossip_dns block, or with it disabled, or with an empty
// record list, touches nothing at all. An operator that maintains these records
// by hand is a supported configuration rather than a misconfiguration.
func TestExtenderGossipDnsIsInertWithoutConfiguration(t *testing.T) {
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1"),
		},
	}
	stubExtenderGossipDnsApi(t, api)

	yamlLineCases := [][]string{
		// no block at all
		nil,
		// the records, disabled
		append(
			[]string{"gossip_dns:", "  enabled: false", "  records:"},
			testExtenderGossipDnsConfigYaml(testExtenderGossipDnsRecord())[4:]...,
		),
		// enabled with nothing to write
		{"gossip_dns:", "  enabled: true"},
	}
	for _, yamlLines := range yamlLineCases {
		installTestExtenderWorkConfig(t, yamlLines...)
		result := runTestExtenderGossipDns(t, context.Background())
		if result.Mirrored != 0 || result.Failed != 0 || result.Skipped != 0 {
			t.Errorf("%v is not an inert configuration: %+v", yamlLines, result)
		}
	}
	connect.AssertEqual(t, len(api.hostedZoneInputs), 0)
	connect.AssertEqual(t, len(api.listInputs), 0)
	connect.AssertEqual(t, len(api.changeInputs), 0)
}

// The chain re-arms a day out. A setup task that stopped re-arming would leave
// a connect record change unmirrored until the next deploy, and nothing would
// say so.
func TestExtenderGossipDnsChainReArms(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(
			t,
			testExtenderGossipDnsConfigYaml(testExtenderGossipDnsRecord())...,
		)
		api := &testRoute53Api{
			hostedZones: []*route53.HostedZone{
				testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
			},
			existingRecordSets: []*route53.ResourceRecordSet{
				testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1"),
			},
		}
		stubExtenderGossipDnsApi(t, api)

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		result := runTestExtenderGossipDns(t, ctx)
		connect.AssertEqual(t, result.Mirrored, 1)

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ExtenderGossipDnsPost(
				&ExtenderGossipDnsArgs{},
				result,
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("ExtenderGossipDnsPost: %v", err)
			}
		})
		runAt := testExtenderTaskRunAt(t, ctx, "extender_gossip_dns")
		want := before.Add(ExtenderGossipDnsTimeout)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("gossip dns setup run_at = %s, want about %s", runAt, want)
		}

		// and a restart pulls the pending setup forward to now rather than
		// stacking a second one, so a deploy that adds a record mirrors it at
		// once instead of a day later
		before = server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleExtenderGossipDns(clientSession, tx)
		})
		runAt = testExtenderTaskRunAt(t, ctx, "extender_gossip_dns")
		if runAt.After(before.Add(5 * time.Second)) {
			t.Fatalf("the restarted gossip dns setup run_at = %s, want about %s", runAt, before)
		}
	})
}

// A record with no name, or no source name, is a typo that would otherwise
// resolve to the zone apex and mirror the whole zone onto itself.
func TestExtenderGossipDnsRefusesAnUnnamedRecord(t *testing.T) {
	installTestExtenderWorkConfig(
		t,
		testExtenderGossipDnsConfigYaml(
			[3]string{testExtenderWorkNetworkHost, "", testExtenderGossipDnsSourceName},
			[3]string{testExtenderWorkNetworkHost, testExtenderGossipDnsName, ""},
		)...,
	)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
	}
	stubExtenderGossipDnsApi(t, api)

	result := runTestExtenderGossipDns(t, context.Background())
	connect.AssertEqual(t, result.Records, 2)
	connect.AssertEqual(t, result.Failed, 2)
	connect.AssertEqual(t, result.Mirrored, 0)
	connect.AssertEqual(t, len(api.changeInputs), 0)
}

// The zone read and the batch, without the task around them: a source with one
// family in one zone is one upsert, and the summary counts it once.
func TestExtenderGossipDnsZoneBatches(t *testing.T) {
	resetTestExtenderDnsHostedZoneIds(t)
	api := &testRoute53Api{
		hostedZones: []*route53.HostedZone{
			testRoute53HostedZone(testExtenderWorkNetworkHost+".", testExtenderDnsNamedZoneId),
		},
		existingRecordSets: []*route53.ResourceRecordSet{
			testExtenderGossipDnsSourceRecordSet(route53.RRTypeA, "198.51.100.1"),
		},
	}

	zoneBatches, skippedNames, errs := extenderGossipDnsZoneBatches(
		context.Background(),
		api,
		[]*controller.ExtenderGossipDnsRecordConfig{
			{
				HostedZoneName: testExtenderWorkNetworkHost,
				Name:           testExtenderGossipDnsName,
				SourceName:     testExtenderGossipDnsSourceName,
			},
		},
	)
	connect.AssertEqual(t, len(errs), 0)
	connect.AssertEqual(t, len(skippedNames), 0)
	connect.AssertEqual(t, len(zoneBatches), 1)
	connect.AssertEqual(t, zoneBatches[0].hostedZoneId, testExtenderDnsNamedZoneId)
	connect.AssertEqual(t, len(zoneBatches[0].records), 1)
	connect.AssertEqual(t, len(zoneBatches[0].changes), 1)
	connect.AssertEqual(t, zoneBatches[0].records[0].name, testExtenderGossipDnsName+".")
	connect.AssertEqual(t, zoneBatches[0].records[0].sourceName, testExtenderGossipDnsSourceName+".")

	// and the change is an upsert of the gossip name, never of the source
	change := zoneBatches[0].changes[0]
	connect.AssertEqual(t, aws.StringValue(change.Action), route53.ChangeActionUpsert)
	connect.AssertEqual(
		t,
		aws.StringValue(change.ResourceRecordSet.Name),
		testExtenderGossipDnsName+".",
	)
}
