package work

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
)

// The measurement names in Route 53 (lag.dev/PLAN.md C1, C5, C8).
//
// The measurement zone is shared. The geo dns publisher keeps its sets at the
// zone's apex (connect/EXTENDER.md C5), and this file has two writers of its
// own: the reconcile, which keeps each serving extender's A and AAAA records
// at its measurement name (C8), and the DNS-01 step of an order, which adds
// and removes its `_acme-challenge` TXT record (C5). Each changes only what
// it owns:
//
//   - the reconcile changes and deletes only A and AAAA records at names that
//     parse as measurement names (ownsRecordSet). It never touches the apex,
//     another type or another name, so the zone's SOA, NS and CAA records,
//     the geo sets and the Public Suffix List's proof are never its to judge;
//   - an order writes only the challenge records of its own identifiers.
//
// Their changes never share a batch. The scoped IAM policy
// (lag.dev/ops/route53.md) authorizes a batch as a whole, address records
// under one statement and challenge records under another, and denies one
// that mixes them or names the apex.
//
// A name is published once the change that wrote its records is in sync on
// every authoritative server, and only then does the reconcile record it so
// (C8). The lag.dev sample offers only published names (A4): a resolver asked
// for a name before it exists caches that it does not, for the lesser of the
// SOA's ttl and minimum (C1).
//
// The zone is named like the dns block's, by zone_name with hosted_zone_id as
// the override, through the same resolution (resolveExtenderDnsHostedZoneId)
// and the same aws session rules (newExtenderRoute53Api). The scoped identity
// may not list zones by name, so production names the id. Before anything is
// written the hosted zone is checked to hold the measurement zone, which
// turns a mistyped id into an error that says so.

const (
	// The ttl of a challenge record. The CA reads it once, right after the
	// change is in sync.
	ExtenderMeasurementChallengeTtl = 60

	// The most changes in one batch. Route 53 takes 1,000 records a batch
	// and counts an UPSERT twice; a change here carries one or two records.
	ExtenderMeasurementDnsBatchSize = 200

	// The longest wait for a change to be in sync. Route 53 is normally in
	// sync within a minute.
	ExtenderMeasurementInsyncTimeout = 3 * time.Minute
)

// How often a change is polled until it is in sync. A test shortens it.
var extenderMeasurementInsyncPollInterval = 5 * time.Second

// The Route 53 calls of the measurement zone: the geo dns publisher's, plus
// the zone read that checks its name and the change read that waits for sync.
type extenderMeasurementRoute53Api interface {
	route53Api
	GetHostedZoneWithContext(
		ctx aws.Context,
		input *route53.GetHostedZoneInput,
		opts ...request.Option,
	) (*route53.GetHostedZoneOutput, error)
	GetChangeWithContext(
		ctx aws.Context,
		input *route53.GetChangeInput,
		opts ...request.Option,
	) (*route53.GetChangeOutput, error)
}

// newExtenderMeasurementRoute53Api builds the aws client of the measurement
// block. A variable so a test can substitute a fake zone; production never
// replaces it.
var newExtenderMeasurementRoute53Api = func(
	config *controller.ExtenderConfig,
) (extenderMeasurementRoute53Api, error) {
	api, err := newExtenderRoute53Api(
		config.Measurement.AwsRegion,
		config.Measurement.AwsAccessKeyId,
		config.Measurement.AwsSecretAccessKey,
	)
	if err != nil {
		return nil, err
	}
	measurementApi, ok := api.(extenderMeasurementRoute53Api)
	if !ok {
		return nil, fmt.Errorf("the route 53 client cannot read zones and changes")
	}
	return measurementApi, nil
}

// The names of the hosted zones already read, by id, remembered for the
// process like the resolved ids, and guarded by
// extenderMeasurementZoneStateLock.
var extenderMeasurementZoneStateLock sync.Mutex
var extenderMeasurementHostedZoneNames = map[string]string{}

// Testing_ResetExtenderMeasurementZones drops the hosted zone names read.
// Call it from the test goroutine only.
func Testing_ResetExtenderMeasurementZones() {
	extenderMeasurementZoneStateLock.Lock()
	defer extenderMeasurementZoneStateLock.Unlock()
	clear(extenderMeasurementHostedZoneNames)
}

// The measurement zone in one hosted zone.
type extenderMeasurementZone struct {
	api extenderMeasurementRoute53Api
	// the measurement zone as names are parsed against it (N1): lower case,
	// no trailing dot
	measurementZone string
	// the same, fully qualified, as Route 53 spells names
	zoneName     string
	hostedZoneId string
	recordTtl    int
}

// openExtenderMeasurementZone resolves the hosted zone of the measurement
// zone and checks that the measurement zone is that zone or under it.
func openExtenderMeasurementZone(
	ctx context.Context,
	config *controller.ExtenderConfig,
) (*extenderMeasurementZone, error) {
	api, err := newExtenderMeasurementRoute53Api(config)
	if err != nil {
		return nil, err
	}
	measurementZone := config.Measurement.MeasurementZone()
	zoneName := extenderDnsFqdn(measurementZone)
	hostedZoneId, err := resolveExtenderDnsHostedZoneId(
		ctx,
		api,
		config.Measurement.HostedZoneId,
		zoneName,
	)
	if err != nil {
		return nil, err
	}

	hostedZoneName := func() string {
		extenderMeasurementZoneStateLock.Lock()
		defer extenderMeasurementZoneStateLock.Unlock()
		return extenderMeasurementHostedZoneNames[hostedZoneId]
	}()
	if hostedZoneName == "" {
		output, err := api.GetHostedZoneWithContext(ctx, &route53.GetHostedZoneInput{
			Id: aws.String(hostedZoneId),
		})
		if err != nil {
			return nil, err
		}
		if output.HostedZone == nil {
			return nil, fmt.Errorf("the hosted zone %s was not read", hostedZoneId)
		}
		hostedZoneName = extenderDnsFqdn(aws.StringValue(output.HostedZone.Name))
		func() {
			extenderMeasurementZoneStateLock.Lock()
			defer extenderMeasurementZoneStateLock.Unlock()
			extenderMeasurementHostedZoneNames[hostedZoneId] = hostedZoneName
		}()
	}
	if zoneName != hostedZoneName && !strings.HasSuffix(zoneName, "."+hostedZoneName) {
		return nil, fmt.Errorf(
			"the hosted zone %s is %s, which does not hold the measurement zone %s",
			hostedZoneId,
			hostedZoneName,
			zoneName,
		)
	}
	return &extenderMeasurementZone{
		api:             api,
		measurementZone: measurementZone,
		zoneName:        zoneName,
		hostedZoneId:    hostedZoneId,
		recordTtl:       config.Measurement.RecordTtl(),
	}, nil
}

// Whether a fully qualified name is the measurement zone or under it.
func (self *extenderMeasurementZone) contains(name string) bool {
	return name == self.zoneName || strings.HasSuffix(name, "."+self.zoneName)
}

// The name of a record set as measurement names are written: lower case, no
// trailing dot.
func extenderMeasurementRecordName(recordSet *route53.ResourceRecordSet) string {
	return strings.TrimSuffix(extenderDnsFqdn(aws.StringValue(recordSet.Name)), ".")
}

// ownsRecordSet is whether a record set is the reconcile's to change or
// delete (C8): an A or AAAA record at a name that parses as a measurement
// name. Everything else in the zone is someone else's -- the apex, where the
// zone's own records and the geo sets are, any other type, and any name that
// is not a measurement name, challenge records included.
func (self *extenderMeasurementZone) ownsRecordSet(recordSet *route53.ResourceRecordSet) bool {
	switch aws.StringValue(recordSet.Type) {
	case route53.RRTypeA, route53.RRTypeAaaa:
	default:
		return false
	}
	_, ok := controller.ParseExtenderMeasurementName(extenderMeasurementRecordName(recordSet), self.measurementZone)
	return ok
}

// The record sets at and under the measurement zone, in the zone's order.
//
// The listing starts at the zone's name and ends at the first name outside
// it. Route 53 orders names by their labels reversed, so a name and
// everything under it are together, and in a hosted zone that is exactly the
// measurement zone that is the whole listing.
func (self *extenderMeasurementZone) listRecordSets(ctx context.Context) ([]*route53.ResourceRecordSet, error) {
	recordSets := []*route53.ResourceRecordSet{}
	err := self.api.ListResourceRecordSetsPagesWithContext(
		ctx,
		&route53.ListResourceRecordSetsInput{
			HostedZoneId:    aws.String(self.hostedZoneId),
			StartRecordName: aws.String(self.zoneName),
		},
		func(output *route53.ListResourceRecordSetsOutput, _ bool) bool {
			for _, recordSet := range output.ResourceRecordSets {
				if !self.contains(extenderDnsFqdn(aws.StringValue(recordSet.Name))) {
					return false
				}
				recordSets = append(recordSets, recordSet)
			}
			return true
		},
	)
	if err != nil {
		return nil, err
	}
	return recordSets, nil
}

// One address record set: a measurement name and a family.
type extenderMeasurementRecordKey struct {
	name       string
	recordType string
}

// What one reconcile is to change (C8), as plan reads it from the zone.
type extenderMeasurementDnsPlan struct {
	// the names with changes, in order, and each name's changes: deletions,
	// then upserts
	names       []string
	nameChanges map[string][]*route53.Change
	// published names whose records are in place, or are only updated: they
	// stay published whatever becomes of this plan's changes
	published []string
	// names to publish once the change that writes their records is in sync:
	// a name not yet published, or one that holds none of its records now.
	// Records already in place are written again, since Route 53 tells
	// whether a change is in sync, not whether a record is
	publishing map[string]bool
	// published names that hold none of their records now: they must not be
	// offered until their records are written again and in sync
	absent []string
}

// What one reconcile changed.
type extenderMeasurementDnsChanges struct {
	upserted int
	deleted  int
	// the names published now, each in sync on every authoritative server
	published []string
	// how many of those this reconcile published
	newlyPublished int
}

// plan reads the zone and works out the changes that make its address records
// exactly `hosts` (C8): for each serving extender, an A record of its IPv4
// addresses and an AAAA record of its IPv6 addresses at its measurement name,
// at the configured ttl and with no routing policy. An address record at a
// measurement name that no host wants is deleted, and nothing else in the
// zone is read as anything but an obstacle: a name that holds a CNAME, which
// cannot share a name, is left out with an error rather than failing every
// other name's batch.
//
// Every change is an A or AAAA change at a measurement name. A name already
// published whose records match gets none, so a pass over a zone that matches
// and is published sends nothing.
func (self *extenderMeasurementZone) plan(
	ctx context.Context,
	hosts []*controller.ExtenderMeasurementHost,
) (*extenderMeasurementDnsPlan, error) {
	desiredValues := map[extenderMeasurementRecordKey][]string{}
	hostNames := map[string]*controller.ExtenderMeasurementHost{}
	for _, host := range hosts {
		if _, ok := controller.ParseExtenderMeasurementName(host.Name, self.measurementZone); !ok {
			// only measurement names are written, as only they are deleted
			return nil, fmt.Errorf("%s is not a measurement name in %s", host.Name, self.measurementZone)
		}
		hostNames[host.Name] = host
		for _, ip := range host.Ips {
			key := extenderMeasurementRecordKey{name: host.Name, recordType: extenderMeasurementRecordType(ip)}
			if value := ip.String(); !slices.Contains(desiredValues[key], value) {
				desiredValues[key] = append(desiredValues[key], value)
			}
		}
	}
	for key := range desiredValues {
		slices.Sort(desiredValues[key])
	}

	recordSets, err := self.listRecordSets(ctx)
	if err != nil {
		return nil, err
	}

	plan := &extenderMeasurementDnsPlan{
		names:       []string{},
		nameChanges: map[string][]*route53.Change{},
		published:   []string{},
		publishing:  map[string]bool{},
		absent:      []string{},
	}
	addChange := func(name string, change *route53.Change) {
		if _, ok := plan.nameChanges[name]; !ok {
			plan.names = append(plan.names, name)
		}
		plan.nameChanges[name] = append(plan.nameChanges[name], change)
	}

	unchangedKeys := map[extenderMeasurementRecordKey]bool{}
	// names that hold an address record of the reconcile's now
	presentNames := map[string]bool{}
	blockedNames := map[string]bool{}
	for _, recordSet := range recordSets {
		name := extenderMeasurementRecordName(recordSet)
		if !self.ownsRecordSet(recordSet) {
			if aws.StringValue(recordSet.Type) == route53.RRTypeCname {
				blockedNames[name] = true
			}
			continue
		}
		presentNames[name] = true
		key := extenderMeasurementRecordKey{name: name, recordType: aws.StringValue(recordSet.Type)}
		values, wanted := desiredValues[key]
		if wanted && recordSet.SetIdentifier == nil && recordSet.AliasTarget == nil {
			// the set an upsert replaces; it is left alone when it matches
			existingValues := []string{}
			for _, resourceRecord := range recordSet.ResourceRecords {
				existingValues = append(existingValues, aws.StringValue(resourceRecord.Value))
			}
			slices.Sort(existingValues)
			if aws.Int64Value(recordSet.TTL) == int64(self.recordTtl) && slices.Equal(existingValues, values) {
				unchangedKeys[key] = true
			}
			continue
		}
		addChange(name, &route53.Change{
			Action:            aws.String(route53.ChangeActionDelete),
			ResourceRecordSet: recordSet,
		})
	}

	// a published name keeps being published while it holds a record
	stays := func(name string) bool {
		return hostNames[name].PublishTime != nil && presentNames[name]
	}

	keys := []extenderMeasurementRecordKey{}
	for key := range desiredValues {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a extenderMeasurementRecordKey, b extenderMeasurementRecordKey) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		return strings.Compare(a.recordType, b.recordType)
	})
	for _, key := range keys {
		if unchangedKeys[key] && stays(key.name) {
			continue
		}
		if blockedNames[key.name] {
			glog.Errorf("[extendermeasurement]%s holds a cname, its address records are not written\n", key.name)
			continue
		}
		resourceRecords := []*route53.ResourceRecord{}
		for _, value := range desiredValues[key] {
			resourceRecords = append(resourceRecords, &route53.ResourceRecord{Value: aws.String(value)})
		}
		addChange(key.name, &route53.Change{
			Action: aws.String(route53.ChangeActionUpsert),
			ResourceRecordSet: &route53.ResourceRecordSet{
				Name:            aws.String(extenderDnsFqdn(key.name)),
				Type:            aws.String(key.recordType),
				TTL:             aws.Int64(int64(self.recordTtl)),
				ResourceRecords: resourceRecords,
			},
		})
	}

	for _, host := range hosts {
		if host.PublishTime != nil && !presentNames[host.Name] {
			plan.absent = append(plan.absent, host.Name)
		}
		switch {
		case blockedNames[host.Name]:
		case stays(host.Name):
			plan.published = append(plan.published, host.Name)
		case 0 < len(plan.nameChanges[host.Name]):
			plan.publishing[host.Name] = true
		}
	}
	slices.Sort(plan.names)
	return plan, nil
}

// apply sends a plan's changes, and waits until each batch that publishes a
// name is in sync, which is when every authoritative server answers for it
// (C8). A name's changes always share a batch, so dns never answers for it
// with only part of them: a deleted record without the one that replaces it.
// A batch that fails, or is not in sync in time, does not keep the others;
// the first error is returned with what was changed.
func (self *extenderMeasurementZone) apply(ctx context.Context, plan *extenderMeasurementDnsPlan) (*extenderMeasurementDnsChanges, error) {
	changes := &extenderMeasurementDnsChanges{
		published: slices.Clone(plan.published),
	}

	// whole names to a batch, up to the batch size
	batches := [][]string{}
	batchSize := 0
	for _, name := range plan.names {
		size := len(plan.nameChanges[name])
		if 0 < len(batches) && batchSize+size <= ExtenderMeasurementDnsBatchSize {
			batches[len(batches)-1] = append(batches[len(batches)-1], name)
			batchSize += size
		} else {
			batches = append(batches, []string{name})
			batchSize = size
		}
	}

	type publishingBatch struct {
		changeId string
		status   string
		names    []string
	}
	publishingBatches := []*publishingBatch{}
	var firstErr error
	for _, names := range batches {
		batchChanges := []*route53.Change{}
		upserted := 0
		deleted := 0
		publishingNames := []string{}
		for _, name := range names {
			for _, change := range plan.nameChanges[name] {
				batchChanges = append(batchChanges, change)
				if aws.StringValue(change.Action) == route53.ChangeActionDelete {
					deleted += 1
				} else {
					upserted += 1
				}
			}
			if plan.publishing[name] {
				publishingNames = append(publishingNames, name)
			}
		}
		output, err := self.api.ChangeResourceRecordSetsWithContext(ctx, &route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(self.hostedZoneId),
			ChangeBatch: &route53.ChangeBatch{
				Comment: aws.String("lag.dev measurement names"),
				Changes: batchChanges,
			},
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		changes.upserted += upserted
		changes.deleted += deleted
		if len(publishingNames) == 0 {
			continue
		}
		if output.ChangeInfo == nil || output.ChangeInfo.Id == nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("the change of %v has no id", publishingNames)
			}
			continue
		}
		publishingBatches = append(publishingBatches, &publishingBatch{
			changeId: aws.StringValue(output.ChangeInfo.Id),
			status:   aws.StringValue(output.ChangeInfo.Status),
			names:    publishingNames,
		})
	}

	// Route 53 brings the batches in sync together, so the waits share one
	// limit, which keeps the pass within its task's
	insyncCtx, insyncCancel := context.WithTimeout(ctx, ExtenderMeasurementInsyncTimeout)
	defer insyncCancel()
	for _, batch := range publishingBatches {
		if err := self.waitInsync(insyncCtx, batch.changeId, batch.status); err != nil {
			// written but not known to be answered everywhere: the next pass
			// writes the names again and waits again
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		changes.published = append(changes.published, batch.names...)
		changes.newlyPublished += len(batch.names)
	}
	return changes, firstErr
}

func extenderMeasurementRecordType(ip netip.Addr) string {
	if ip.Unmap().Is4() {
		return route53.RRTypeA
	}
	return route53.RRTypeAaaa
}

// The TXT record set of one challenge record.
func (self *extenderMeasurementZone) challengeRecordSet(record extenderMeasurementChallengeRecord) *route53.ResourceRecordSet {
	return &route53.ResourceRecordSet{
		Name: aws.String(extenderDnsFqdn(record.name)),
		Type: aws.String(route53.RRTypeTxt),
		TTL:  aws.Int64(ExtenderMeasurementChallengeTtl),
		ResourceRecords: []*route53.ResourceRecord{
			{Value: aws.String(extenderDnsTxtValue(record.value))},
		},
	}
}

// present writes the challenge records in one change, which holds nothing but
// TXT records, and waits until Route 53 reports it in sync, which is when
// every authoritative server answers with them (C5). Implements
// extenderMeasurementChallenges.
func (self *extenderMeasurementZone) present(ctx context.Context, records []extenderMeasurementChallengeRecord) error {
	changes := []*route53.Change{}
	for _, record := range records {
		if !self.contains(extenderDnsFqdn(record.name)) {
			return fmt.Errorf("the challenge record %s is outside the measurement zone", record.name)
		}
		changes = append(changes, &route53.Change{
			Action:            aws.String(route53.ChangeActionUpsert),
			ResourceRecordSet: self.challengeRecordSet(record),
		})
	}
	output, err := self.api.ChangeResourceRecordSetsWithContext(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(self.hostedZoneId),
		ChangeBatch: &route53.ChangeBatch{
			Comment: aws.String("lag.dev measurement certificate challenge"),
			Changes: changes,
		},
	})
	if err != nil {
		return err
	}
	if output.ChangeInfo == nil || output.ChangeInfo.Id == nil {
		return fmt.Errorf("the challenge change has no id")
	}
	return self.waitInsync(ctx, aws.StringValue(output.ChangeInfo.Id), aws.StringValue(output.ChangeInfo.Status))
}

// Polls one change until it is in sync.
func (self *extenderMeasurementZone) waitInsync(ctx context.Context, changeId string, status string) error {
	insyncCtx, insyncCancel := context.WithTimeout(ctx, ExtenderMeasurementInsyncTimeout)
	defer insyncCancel()
	for status != route53.ChangeStatusInsync {
		select {
		case <-insyncCtx.Done():
			return fmt.Errorf("the change %s was not in sync: %w", changeId, insyncCtx.Err())
		case <-time.After(extenderMeasurementInsyncPollInterval):
		}
		output, err := self.api.GetChangeWithContext(insyncCtx, &route53.GetChangeInput{
			Id: aws.String(changeId),
		})
		if err != nil {
			return err
		}
		if output.ChangeInfo == nil {
			return fmt.Errorf("the change %s was not read", changeId)
		}
		status = aws.StringValue(output.ChangeInfo.Status)
	}
	return nil
}

// remove deletes the challenge records an order wrote, in a change that holds
// nothing but TXT records. A record that is already gone does not keep the
// others: the batch is retried one record at a time. Implements
// extenderMeasurementChallenges.
func (self *extenderMeasurementZone) remove(ctx context.Context, records []extenderMeasurementChallengeRecord) error {
	deleteRecords := func(records []extenderMeasurementChallengeRecord) error {
		changes := []*route53.Change{}
		for _, record := range records {
			changes = append(changes, &route53.Change{
				Action:            aws.String(route53.ChangeActionDelete),
				ResourceRecordSet: self.challengeRecordSet(record),
			})
		}
		_, err := self.api.ChangeResourceRecordSetsWithContext(ctx, &route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(self.hostedZoneId),
			ChangeBatch: &route53.ChangeBatch{
				Comment: aws.String("lag.dev measurement certificate challenge"),
				Changes: changes,
			},
		})
		return err
	}
	if err := deleteRecords(records); err == nil || len(records) == 1 {
		return err
	}
	var firstErr error
	for _, record := range records {
		if err := deleteRecords([]extenderMeasurementChallengeRecord{record}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
