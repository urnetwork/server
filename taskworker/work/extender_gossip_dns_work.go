package work

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The gossip record setup task (connect/EXTENDER.md C6).
//
// The gossip service answers at `gossip.<host>` behind the same nginx that
// serves connect, on the same addresses. So the record is not something this
// operator computes: it is whatever `connect.<host>` already resolves to, under
// a second name. The task reads the source name's A and AAAA sets and upserts
// the same values, the same types and the same ttl under the gossip name.
//
// An alias source is copied as an alias rather than as the addresses it
// currently resolves to. A load balancer's addresses change without anyone
// editing dns, and a gossip name pinned to yesterday's addresses would point at
// nothing; copying the alias target makes the gossip name follow the same
// balancer the source follows.
//
// This is a setup task, not a publisher: it runs at taskworker start and then
// daily, because what it mirrors changes when someone changes the connect
// record, which is rare and is not an event this process hears about. Being
// idempotent is what makes the cadence safe -- a run over an unchanged source
// computes the same batch and Route 53 treats the repeated upsert as a no-op,
// so there is no reason to read back and diff before writing.
//
// A record that fails costs only itself. The zone of one record being missing,
// or one source having no records at all, does not stop the others: every
// failure is logged and the task returns nil so the daily chain re-arms, the
// same contract the probe and publish ticks keep.

const (
	// How often every configured record is re-mirrored. The first run is
	// immediate (ScheduleExtenderGossipDns), so a deploy that adds a record
	// does not wait a day for it.
	ExtenderGossipDnsTimeout = 24 * time.Hour
)

// The types a gossip name is mirrored for. Anything else at the source is not
// an address the gossip service is reached at.
var extenderGossipDnsRecordTypes = []string{route53.RRTypeA, route53.RRTypeAaaa}

// newExtenderGossipDnsApi builds the api the setup task writes through.
//
// It is a variable so a test can substitute a fake rather than reaching aws.
// Production never replaces it.
var newExtenderGossipDnsApi = func(config *controller.ExtenderConfig) (route53Api, error) {
	// no credentials in the gossip_dns block: the host's own credentials are
	// what the operator runs this with
	return newExtenderRoute53Api(config.GossipDns.AwsRegion, "", "")
}

// One configured record resolved into what it wants written: the zone, and the
// sets the gossip name is upserted with, in the order of
// extenderGossipDnsRecordTypes.
type extenderGossipDnsRecord struct {
	name         string
	sourceName   string
	hostedZoneId string
	recordSets   []*route53.ResourceRecordSet
}

// One zone's changes, which is one Route 53 change batch.
type extenderGossipDnsZoneBatch struct {
	hostedZoneId string
	// the records that contributed, so a failed batch is counted against each
	// of them rather than against the zone
	records []*extenderGossipDnsRecord
	changes []*route53.Change
}

// extenderGossipDnsRecordSet copies one source set onto the gossip name.
//
// Only what identifies the answer is carried across. A routing policy is not:
// the source's set identifier, weight, region, geolocation and health check all
// belong to the source's own set and would need the whole policy copied to mean
// anything, so the gossip name gets one simple set per type.
func extenderGossipDnsRecordSet(
	name string,
	sourceRecordSet *route53.ResourceRecordSet,
) *route53.ResourceRecordSet {
	recordSet := &route53.ResourceRecordSet{
		Name: aws.String(name),
		Type: sourceRecordSet.Type,
	}
	if sourceRecordSet.AliasTarget != nil {
		// an alias carries neither a ttl nor values of its own, and route 53
		// rejects a set that has an alias and a ttl together
		recordSet.AliasTarget = &route53.AliasTarget{
			DNSName:              sourceRecordSet.AliasTarget.DNSName,
			HostedZoneId:         sourceRecordSet.AliasTarget.HostedZoneId,
			EvaluateTargetHealth: sourceRecordSet.AliasTarget.EvaluateTargetHealth,
		}
		return recordSet
	}
	recordSet.TTL = sourceRecordSet.TTL
	resourceRecords := []*route53.ResourceRecord{}
	for _, sourceResourceRecord := range sourceRecordSet.ResourceRecords {
		resourceRecords = append(resourceRecords, &route53.ResourceRecord{
			Value: sourceResourceRecord.Value,
		})
	}
	recordSet.ResourceRecords = resourceRecords
	return recordSet
}

// listExtenderGossipDnsSourceRecordSets reads the A and AAAA sets at one name,
// keyed by type.
//
// The listing starts at the source name and the zone is ordered by name, so the
// first set of another name is the end of the source's -- there is no reason to
// page through the rest of a zone that may hold thousands of unrelated records.
//
// The first set of each type wins. A source with several sets of one type is a
// routing policy, and since a policy is not copied there is no batch that could
// carry more than one of them: two upserts of the same name and type with no
// set identifier are a duplicate that Route 53 rejects outright.
func listExtenderGossipDnsSourceRecordSets(
	ctx context.Context,
	api route53Api,
	hostedZoneId string,
	sourceName string,
) (map[string]*route53.ResourceRecordSet, error) {
	sourceRecordSets := map[string]*route53.ResourceRecordSet{}

	err := api.ListResourceRecordSetsPagesWithContext(
		ctx,
		&route53.ListResourceRecordSetsInput{
			HostedZoneId:    aws.String(hostedZoneId),
			StartRecordName: aws.String(sourceName),
		},
		func(output *route53.ListResourceRecordSetsOutput, _ bool) bool {
			for _, recordSet := range output.ResourceRecordSets {
				if extenderDnsFqdn(aws.StringValue(recordSet.Name)) != sourceName {
					return false
				}
				recordType := aws.StringValue(recordSet.Type)
				if recordType != route53.RRTypeA && recordType != route53.RRTypeAaaa {
					continue
				}
				if _, ok := sourceRecordSets[recordType]; ok {
					continue
				}
				sourceRecordSets[recordType] = recordSet
			}
			return true
		},
	)
	if err != nil {
		return nil, err
	}
	return sourceRecordSets, nil
}

// extenderGossipDnsZoneBatches resolves every configured record and groups the
// upserts by zone, one batch per zone.
//
// Grouping is what makes a zone's records land together: Route 53 applies a
// batch atomically, so two names mirrored into one zone are either both updated
// or both left alone, and the whole zone costs one write.
//
// The returned errors are per record. A record whose zone cannot be resolved or
// whose source cannot be read contributes an error and nothing else; the
// remaining records are still grouped and still written. A source with neither
// an A nor an AAAA set is not an error -- there is nothing to mirror yet, which
// is what an operator sees while the connect record is still being created --
// so it is counted as skipped and named in the log.
func extenderGossipDnsZoneBatches(
	ctx context.Context,
	api route53Api,
	recordConfigs []*controller.ExtenderGossipDnsRecordConfig,
) (zoneBatches []*extenderGossipDnsZoneBatch, skippedNames []string, errs []error) {
	zoneBatchIndexes := map[string]int{}

	for _, recordConfig := range recordConfigs {
		name := extenderDnsFqdn(recordConfig.Name)
		sourceName := extenderDnsFqdn(recordConfig.SourceName)
		if name == "." || sourceName == "." {
			errs = append(errs, fmt.Errorf(
				"a gossip_dns record needs both a name and a source_name, got %q and %q",
				recordConfig.Name,
				recordConfig.SourceName,
			))
			continue
		}

		hostedZoneId, err := resolveExtenderDnsHostedZoneId(
			ctx,
			api,
			"",
			recordConfig.HostedZoneName,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}

		sourceRecordSets, err := listExtenderGossipDnsSourceRecordSets(
			ctx,
			api,
			hostedZoneId,
			sourceName,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: read %s: %w", name, sourceName, err))
			continue
		}

		record := &extenderGossipDnsRecord{
			name:         name,
			sourceName:   sourceName,
			hostedZoneId: hostedZoneId,
			recordSets:   []*route53.ResourceRecordSet{},
		}
		for _, recordType := range extenderGossipDnsRecordTypes {
			if sourceRecordSet, ok := sourceRecordSets[recordType]; ok {
				record.recordSets = append(
					record.recordSets,
					extenderGossipDnsRecordSet(name, sourceRecordSet),
				)
			}
		}
		if len(record.recordSets) == 0 {
			skippedNames = append(skippedNames, name)
			continue
		}

		zoneBatchIndex, ok := zoneBatchIndexes[hostedZoneId]
		if !ok {
			zoneBatchIndex = len(zoneBatches)
			zoneBatchIndexes[hostedZoneId] = zoneBatchIndex
			zoneBatches = append(zoneBatches, &extenderGossipDnsZoneBatch{
				hostedZoneId: hostedZoneId,
				records:      []*extenderGossipDnsRecord{},
				changes:      []*route53.Change{},
			})
		}
		zoneBatch := zoneBatches[zoneBatchIndex]
		zoneBatch.records = append(zoneBatch.records, record)
		for _, recordSet := range record.recordSets {
			zoneBatch.changes = append(zoneBatch.changes, &route53.Change{
				Action:            aws.String(route53.ChangeActionUpsert),
				ResourceRecordSet: recordSet,
			})
		}
	}
	return zoneBatches, skippedNames, errs
}

type ExtenderGossipDnsArgs struct {
}

type ExtenderGossipDnsResult struct {
	// counts only; which record failed is in the log
	Records  int `json:"records"`
	Mirrored int `json:"mirrored"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// ScheduleExtenderGossipDns schedules the first setup to run IMMEDIATELY.
//
// The gossip name is what every member resolves to find the operator's node, so
// a deployment that has just added a record should not wait a day for it. The
// daily cadence is set in the Post below.
//
// RunOnce merges on conflict with `run_at = LEAST(existing, new)`, so a
// taskworker restart pulls a pending setup forward to now rather than stacking
// a second one. That costs one listing and one idempotent batch per record per
// restart, which is nothing.
func ScheduleExtenderGossipDns(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleExtenderGossipDnsAt(clientSession, tx, server.NowUtc())
}

func scheduleExtenderGossipDnsAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderGossipDns,
		&ExtenderGossipDnsArgs{},
		clientSession,
		task.RunOnce("extender_gossip_dns"),
		task.RunAt(runAt),
	)
}

// ExtenderGossipDns mirrors every configured source name onto its gossip name
// (C6).
//
// It returns nil even when records failed, for the same reason the probe and
// publish ticks do: a task that returns an error is rescheduled on a backoff
// with its Post skipped, so one unreachable zone would stop the whole chain and
// the remaining records would silently stop being maintained. Failures are loud
// in the log and counted in the result instead.
func ExtenderGossipDns(
	_ *ExtenderGossipDnsArgs,
	clientSession *session.ClientSession,
) (*ExtenderGossipDnsResult, error) {
	result := &ExtenderGossipDnsResult{}

	config, err := controller.EnvExtenderConfig()
	if err != nil {
		// no extender network in this deployment; the chain still re-arms
		return result, nil
	}
	if !config.GossipDns.Enabled || len(config.GossipDns.Records) == 0 {
		// an operator that runs no gossip service, or that maintains the
		// records by hand, is a supported configuration rather than a
		// misconfiguration
		return result, nil
	}
	result.Records = len(config.GossipDns.Records)

	api, err := newExtenderGossipDnsApi(config)
	if err != nil {
		glog.Errorf("[extendergossipdns]no aws session, the records are unchanged: %s\n", err)
		result.Failed = result.Records
		return result, nil
	}

	zoneBatches, skippedNames, errs := extenderGossipDnsZoneBatches(
		clientSession.Ctx,
		api,
		config.GossipDns.Records,
	)
	result.Skipped = len(skippedNames)
	result.Failed = len(errs)

	for _, skippedName := range skippedNames {
		glog.Errorf(
			"[extendergossipdns]%s was not written, its source has no a or aaaa record\n",
			skippedName,
		)
	}
	for _, err := range errs {
		glog.Errorf("[extendergossipdns]record FAILED, it is unchanged: %s\n", err)
	}

	for _, zoneBatch := range zoneBatches {
		_, err := api.ChangeResourceRecordSetsWithContext(
			clientSession.Ctx,
			&route53.ChangeResourceRecordSetsInput{
				HostedZoneId: aws.String(zoneBatch.hostedZoneId),
				ChangeBatch: &route53.ChangeBatch{
					Changes: zoneBatch.changes,
				},
			},
		)
		if err != nil {
			// one batch is atomic, so every record in it is unchanged
			result.Failed += len(zoneBatch.records)
			glog.Errorf(
				"[extendergossipdns]zone %s FAILED, its %d records are unchanged: %s\n",
				zoneBatch.hostedZoneId,
				len(zoneBatch.records),
				err,
			)
			continue
		}
		for _, record := range zoneBatch.records {
			result.Mirrored += 1
			recordTypes := []string{}
			for _, recordSet := range record.recordSets {
				recordTypes = append(recordTypes, aws.StringValue(recordSet.Type))
			}
			glog.Infof(
				"[extendergossipdns]%s mirrors %s (%s)\n",
				record.name,
				record.sourceName,
				strings.Join(recordTypes, ", "),
			)
		}
	}

	glog.Infof(
		"[extendergossipdns]mirrored %d of %d records in %d zones: %d skipped, %d failed\n",
		result.Mirrored,
		result.Records,
		len(zoneBatches),
		result.Skipped,
		result.Failed,
	)
	return result, nil
}

func ExtenderGossipDnsPost(
	_ *ExtenderGossipDnsArgs,
	_ *ExtenderGossipDnsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	// the recurring cadence; only the very first run is immediate
	scheduleExtenderGossipDnsAt(
		clientSession,
		tx,
		server.NowUtc().Add(ExtenderGossipDnsTimeout),
	)
	return nil
}
