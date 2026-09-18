package work

import (
	"context"
	"encoding/base64"
	"fmt"
	mathrand "math/rand/v2"
	"slices"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The geo dns half of the extender publish tick (connect/EXTENDER.md C5).
//
// Route 53 answers `extender.<host>` by geolocation: one A and one AAAA set
// per continent, plus a default set per family for every location no continent
// set covers. Each set holds up to sample_count addresses drawn at random from
// the active addresses of that family in that continent, filled from the
// global pool of the family when the continent is short. A continent with no
// addresses of a family at all is not published; its set is deleted instead,
// so the record never answers with a continent's worth of dead addresses.
//
// The sample is redrawn every tick from a fresh random source. That rotation
// is the point: with a ttl of a minute, successive samples spread clients over
// the whole active set instead of pinning every client in a continent to the
// same eight addresses until one of them fails.
//
// One apply is one change batch. Route 53 applies a batch atomically, so the
// record never exists in a half-updated state where a continent has been
// deleted and not yet re-upserted, and the whole tick costs one write.
//
// Deletion is narrowed to sets this publisher owns -- the record name, an A or
// AAAA type and a set identifier under the extender prefix. A record an
// operator put at the same name by hand is not a set this tick "no longer
// wants"; deleting it would be a destructive surprise that no later tick could
// undo.
//
// Nothing here fails the tick. The caller logs and continues, because the
// record drip and the dns sets are independent publishers of the same
// directory and one being unreachable must not cost the other its turn.
//
// The zone the batch goes to is configured by name as well as by id, and a
// name is resolved to an id once per process (resolveExtenderDnsHostedZoneId).

const (
	// Addresses per set when the configuration does not say.
	ExtenderDnsDefaultSampleCount = 8

	// Seconds a resolver may cache a set when the configuration does not say.
	// Short on purpose: the set is a rotating sample, and a long ttl would
	// hold a client on addresses the next tick has already replaced.
	ExtenderDnsDefaultTtl = 60

	// Route 53 is a global service reached through one endpoint; the region
	// only has to be a real one when the configuration leaves it out.
	ExtenderDnsDefaultAwsRegion = "us-east-1"

	// Set identifiers are `extender-<continent>-<type>` and
	// `extender-default-<type>`. The prefix is also what marks a set as ours
	// when the zone is read back.
	extenderDnsSetIdentifierPrefix = "extender-"
	extenderDnsDefaultSetLabel     = "default"
)

// The families a set can be published for, in the order the batch carries
// them.
var extenderDnsIpVersions = []int{4, 6}

// One geolocation set: the addresses one continent, or the default, is
// answered with for one family.
type extenderDnsRecordSet struct {
	// the empty code is the default set, which answers every location no
	// continent set covers
	continentCode string
	// 4 or 6 for an address set; 0 for the TXT set of the same location
	ipVersion int
	ips       []string
	// records is the TXT set's values: one base64 signed gossip message per
	// extender whose address the location's A or AAAA set answers with (C5).
	// A client that resolved the name gets, beside the addresses, the
	// operator's signature over each of them, so its bootstrap lands verified
	// instead of waiting on gossip to vouch for what dns already said.
	records []string
}

// What Route 53 tells two sets of the same name and type apart by.
func (self *extenderDnsRecordSet) setIdentifier() string {
	label := self.continentCode
	if label == "" {
		label = extenderDnsDefaultSetLabel
	}
	return fmt.Sprintf("%s%s-%s", extenderDnsSetIdentifierPrefix, label, self.recordType())
}

// The record type the set is published as.
func (self *extenderDnsRecordSet) recordType() string {
	switch self.ipVersion {
	case 6:
		return route53.RRTypeAaaa
	case 4:
		return route53.RRTypeA
	default:
		return route53.RRTypeTxt
	}
}

// extenderDnsRecordSigner produces the TXT value of one extender: the base64
// of its freshly signed gossip message. false when the extender has nothing to
// sign for, which drops it from the TXT set without dropping its addresses.
type extenderDnsRecordSigner func(extenderId server.Id) (string, bool)

// The record types this publisher owns under the record name.
var extenderDnsRecordTypes = []string{route53.RRTypeA, route53.RRTypeAaaa, route53.RRTypeTxt}

// Applies one tick's desired state to the record.
//
// An implementation reads what the zone already holds under the name, upserts
// every desired set and deletes every set of its own that is no longer
// desired, in one batch.
type extenderDnsPublisher interface {
	publish(
		ctx context.Context,
		recordName string,
		ttl int,
		desiredSets []*extenderDnsRecordSet,
	) error
}

// newExtenderDnsPublisher builds the publisher one tick writes through.
//
// It is a variable so a test can substitute a fake that records the batches
// rather than reaching aws. Production never replaces it.
var newExtenderDnsPublisher = func(
	config *controller.ExtenderConfig,
) (extenderDnsPublisher, error) {
	publisher, err := newRoute53ExtenderDnsPublisher(config)
	if err != nil {
		return nil, err
	}
	return publisher, nil
}

// extenderDnsRandom is the source one tick's sample is drawn from.
//
// It is a variable for the same reason: seeded in a test the sets are exactly
// reproducible, while production draws a fresh seed per tick, which is what
// makes successive ticks rotate the addresses a set answers with.
var extenderDnsRandom = func() *mathrand.Rand {
	return mathrand.New(mathrand.NewPCG(mathrand.Uint64(), mathrand.Uint64()))
}

// publishExtenderDns refreshes the geolocation sets for this tick (C5).
//
// A deployment with no dns block, or with it disabled, leaves dns untouched
// and says so by doing nothing: the drip is the half of the tick that always
// runs, and an operator that publishes records without publishing dns is a
// supported configuration rather than a misconfiguration.
func publishExtenderDns(ctx context.Context, config *controller.ExtenderConfig) error {
	if config == nil || !config.Dns.Enabled {
		return nil
	}
	recordName := strings.TrimSpace(config.Dns.RecordName)
	if recordName == "" {
		// the name is configured, never derived: the sdk's default is
		// `extender.<host>` under the env prefix rule, and which name this
		// operator serves is an operations decision
		return fmt.Errorf("the extender dns has no record name")
	}

	publisher, err := newExtenderDnsPublisher(config)
	if err != nil {
		return err
	}

	addresses := model.GetActiveNetworkExtenderDnsAddresses(ctx)
	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		extenderDnsSampleCount(config),
		extenderDnsRandom(),
		newExtenderDnsRecordSigner(ctx, config),
	)
	// what this tick converges the zone to, which is the only record of what
	// is in dns: the sets are recomputed every tick and never stored
	observeExtenderDnsSets(desiredSets, addresses)
	return publisher.publish(ctx, recordName, extenderDnsTtl(config), desiredSets)
}

// newExtenderDnsRecordSigner signs a fresh record per extender for the TXT
// sets, with the same root key and the same record shape as the drip, so a
// record fetched from dns is indistinguishable from one fetched from gossip.
//
// Without a root key there are no TXT sets and the tick says so once; the
// address sets are still published, since an operator that cannot sign is
// exactly as it was before TXT existed rather than worse.
func newExtenderDnsRecordSigner(
	ctx context.Context,
	config *controller.ExtenderConfig,
) extenderDnsRecordSigner {
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		glog.Errorf("[extenderpublish]no root key, dns txt records are not published: %s\n", err)
		return func(server.Id) (string, bool) { return "", false }
	}
	return func(extenderId server.Id) (string, bool) {
		extender, addresses := model.GetActiveNetworkExtenderForRecord(ctx, extenderId)
		if extender == nil {
			return "", false
		}
		_, message, err := controller.SignExtenderRecord(
			config,
			rootPrivateKey,
			extender,
			addresses,
			server.NowUtc(),
		)
		if err != nil {
			glog.Errorf("[extenderpublish]dns txt record for %s not signed: %s\n", extenderId, err)
			return "", false
		}
		return base64.StdEncoding.EncodeToString(message), true
	}
}

// Addresses per set, with the default for an unset or nonsense value.
func extenderDnsSampleCount(config *controller.ExtenderConfig) int {
	if config.Dns.SampleCount <= 0 {
		return ExtenderDnsDefaultSampleCount
	}
	return config.Dns.SampleCount
}

// Seconds a set may be cached, with the default for an unset or nonsense
// value.
func extenderDnsTtl(config *controller.ExtenderConfig) int {
	if config.Dns.Ttl <= 0 {
		return ExtenderDnsDefaultTtl
	}
	return config.Dns.Ttl
}

// One pool of candidate addresses: a family, narrowed to a continent when the
// code is set. The empty code is the global pool of the family.
type extenderDnsPool struct {
	continentCode string
	ipVersion     int
}

// sampleExtenderDnsRecordSets draws the sets one tick wants (C5).
//
// A continent set takes its own addresses first and fills from the global pool
// of the family only when it is short, so a client is answered with local
// addresses whenever local addresses exist and with something reachable
// otherwise. No address appears twice in a set: the global pool contains the
// continent's own addresses, so the fill has to skip what was already drawn.
//
// A family with no active address anywhere produces no sets at all, not even a
// default one -- an empty set is a record that resolves to nothing, which is
// worse than no record, since a client that gets NXDOMAIN for AAAA falls
// straight through to A.
func sampleExtenderDnsRecordSets(
	addresses []*model.NetworkExtenderDnsAddress,
	sampleCount int,
	random *mathrand.Rand,
	signRecord extenderDnsRecordSigner,
) []*extenderDnsRecordSet {
	poolIps := map[extenderDnsPool][]string{}
	// which extender an address belongs to, for the TXT set of its location
	ipExtenderIds := map[string]server.Id{}
	for _, address := range addresses {
		ip := address.Ip.String()
		ipExtenderIds[ip] = address.ExtenderId
		globalPool := extenderDnsPool{ipVersion: address.IpVersion}
		poolIps[globalPool] = append(poolIps[globalPool], ip)
		// an extender whose country has no continent -- unset, or a code the
		// table does not carry -- is in the global pool only, rather than
		// being guessed into a continent it may be nowhere near
		if continentCode := model.ContinentCodeForCountry(address.CountryCode); continentCode != "" {
			continentPool := extenderDnsPool{continentCode: continentCode, ipVersion: address.IpVersion}
			poolIps[continentPool] = append(poolIps[continentPool], ip)
		}
	}

	sample := func(candidateIps []string, fillIps []string) []string {
		ips := []string{}
		draw := func(pool []string) {
			shuffled := slices.Clone(pool)
			random.Shuffle(len(shuffled), func(i int, j int) {
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			})
			for _, ip := range shuffled {
				if sampleCount <= len(ips) {
					return
				}
				if !slices.Contains(ips, ip) {
					ips = append(ips, ip)
				}
			}
		}
		draw(candidateIps)
		draw(fillIps)
		return ips
	}

	desiredSets := []*extenderDnsRecordSet{}
	for _, ipVersion := range extenderDnsIpVersions {
		globalIps := poolIps[extenderDnsPool{ipVersion: ipVersion}]
		if len(globalIps) == 0 {
			continue
		}
		for _, continentCode := range model.ContinentCodes {
			continentIps := poolIps[extenderDnsPool{continentCode: continentCode, ipVersion: ipVersion}]
			if len(continentIps) == 0 {
				// no address on this continent at all; the set is not desired
				// and an existing one is deleted
				continue
			}
			desiredSets = append(desiredSets, &extenderDnsRecordSet{
				continentCode: continentCode,
				ipVersion:     ipVersion,
				ips:           sample(continentIps, globalIps),
			})
		}
		desiredSets = append(desiredSets, &extenderDnsRecordSet{
			ipVersion: ipVersion,
			ips:       sample(globalIps, nil),
		})
	}

	// One TXT set per location the address sets answer for, carrying the
	// signed record of every extender behind those addresses. Built from the
	// sampled sets rather than the pools so a client is vouched for exactly
	// the addresses it was answered with, and each extender once however many
	// of its addresses were drawn.
	if signRecord != nil {
		locationExtenderIds := map[string][]server.Id{}
		locationOrder := []string{}
		for _, desiredSet := range desiredSets {
			location := desiredSet.continentCode
			if _, ok := locationExtenderIds[location]; !ok {
				locationOrder = append(locationOrder, location)
			}
			for _, ip := range desiredSet.ips {
				extenderId, ok := ipExtenderIds[ip]
				if !ok || slices.Contains(locationExtenderIds[location], extenderId) {
					continue
				}
				locationExtenderIds[location] = append(locationExtenderIds[location], extenderId)
			}
		}
		// sign each extender once, however many locations answer with it;
		// a refusal is remembered too, so an extender the signer has nothing
		// for costs one lookup per tick rather than one per location
		type signedRecord struct {
			record string
			ok     bool
		}
		signed := map[server.Id]signedRecord{}
		for _, location := range locationOrder {
			records := []string{}
			for _, extenderId := range locationExtenderIds[location] {
				entry, seen := signed[extenderId]
				if !seen {
					entry.record, entry.ok = signRecord(extenderId)
					signed[extenderId] = entry
				}
				if !entry.ok {
					continue
				}
				records = append(records, entry.record)
			}
			if len(records) == 0 {
				continue
			}
			desiredSets = append(desiredSets, &extenderDnsRecordSet{
				continentCode: location,
				records:       records,
			})
		}
	}
	return desiredSets
}

// extenderDnsDeletedSetIdentifiers is the set identifiers an apply must
// remove: everything the zone already holds under the record name that this
// tick no longer wants.
//
// Shared by every publisher so the fake and Route 53 cannot disagree about
// what a tick leaves behind.
func extenderDnsDeletedSetIdentifiers(
	existingSetIdentifiers []string,
	desiredSets []*extenderDnsRecordSet,
) []string {
	desiredSetIdentifiers := []string{}
	for _, desiredSet := range desiredSets {
		desiredSetIdentifiers = append(desiredSetIdentifiers, desiredSet.setIdentifier())
	}
	deletedSetIdentifiers := []string{}
	for _, setIdentifier := range existingSetIdentifiers {
		if slices.Contains(desiredSetIdentifiers, setIdentifier) {
			continue
		}
		if slices.Contains(deletedSetIdentifiers, setIdentifier) {
			continue
		}
		deletedSetIdentifiers = append(deletedSetIdentifiers, setIdentifier)
	}
	return deletedSetIdentifiers
}

// The name as Route 53 holds it: fully qualified and lower case, which is the
// form a listing comes back in and the form a comparison has to be made on.
func extenderDnsFqdn(recordName string) string {
	recordName = strings.ToLower(strings.TrimSpace(recordName))
	if !strings.HasSuffix(recordName, ".") {
		recordName += "."
	}
	return recordName
}

// The Route 53 calls the publisher makes, which is the seam a test drives the
// real batch builder through.
type route53Api interface {
	ListHostedZonesByNameWithContext(
		ctx aws.Context,
		input *route53.ListHostedZonesByNameInput,
		opts ...request.Option,
	) (*route53.ListHostedZonesByNameOutput, error)
	ListResourceRecordSetsPagesWithContext(
		ctx aws.Context,
		input *route53.ListResourceRecordSetsInput,
		fn func(*route53.ListResourceRecordSetsOutput, bool) bool,
		opts ...request.Option,
	) error
	ChangeResourceRecordSetsWithContext(
		ctx aws.Context,
		input *route53.ChangeResourceRecordSetsInput,
		opts ...request.Option,
	) (*route53.ChangeResourceRecordSetsOutput, error)
}

// newExtenderRoute53Api builds the aws session of the dns block.
//
// Static credentials are used only when both halves are configured; anything
// else falls back to the default chain, which is what an instance role or the
// environment provides. A half-configured pair is a typo rather than an
// intent, and using it would fail every call with a signature error instead of
// the working default.
func newExtenderRoute53Api(
	awsRegion string,
	awsAccessKeyId string,
	awsSecretAccessKey string,
) (route53Api, error) {
	awsRegion = strings.TrimSpace(awsRegion)
	if awsRegion == "" {
		awsRegion = ExtenderDnsDefaultAwsRegion
	}
	awsConfig := &aws.Config{
		Region: aws.String(awsRegion),
	}
	awsAccessKeyId = strings.TrimSpace(awsAccessKeyId)
	awsSecretAccessKey = strings.TrimSpace(awsSecretAccessKey)
	if awsAccessKeyId != "" && awsSecretAccessKey != "" {
		awsConfig.Credentials = credentials.NewStaticCredentials(
			awsAccessKeyId,
			awsSecretAccessKey,
			"",
		)
	}

	awsSession, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, err
	}
	return route53.New(awsSession), nil
}

// The zone ids already resolved from a name, remembered for the process and
// guarded by extenderDnsHostedZoneIdsStateLock.
//
// One resolution per name per process is the point: the zone of a name does not
// change while the process runs, and a lookup on every tick would spend an api
// call to learn the same id every ten minutes. Only a success is remembered --
// a zone that is missing now may be created later, and caching that failure
// would need a restart to clear.
var extenderDnsHostedZoneIdsStateLock sync.Mutex
var extenderDnsHostedZoneIds = map[string]string{}

// Testing_ResetExtenderDnsHostedZoneIds drops the resolved zone ids so a test
// starts from an unresolved process. Call it from the test goroutine only.
func Testing_ResetExtenderDnsHostedZoneIds() {
	extenderDnsHostedZoneIdsStateLock.Lock()
	defer extenderDnsHostedZoneIdsStateLock.Unlock()
	clear(extenderDnsHostedZoneIds)
}

// resolveExtenderDnsHostedZoneId is the zone a tick writes to.
//
// A configured id wins outright and costs no call at all, which is both the
// escape hatch for an operator with two zones of one name and what keeps a
// deployment that already names its zone by id working unchanged. Otherwise
// the name is resolved through the api and remembered for the process.
//
// The listing is positioned at the name, and Route 53 orders zones by name, so
// every zone of that name is at the head of the answer. Exactly one match is
// required: no match is a zone that does not exist in this account, and more
// than one is a public and a private zone of the same name, where guessing
// which one the operator meant would publish the record where nobody resolves
// it.
func resolveExtenderDnsHostedZoneId(
	ctx context.Context,
	api route53Api,
	hostedZoneId string,
	hostedZoneName string,
) (string, error) {
	if hostedZoneId = strings.TrimSpace(hostedZoneId); hostedZoneId != "" {
		return hostedZoneId, nil
	}
	name := extenderDnsFqdn(hostedZoneName)
	if name == "." {
		return "", fmt.Errorf("the extender dns has no hosted zone")
	}

	cachedHostedZoneId := func() string {
		extenderDnsHostedZoneIdsStateLock.Lock()
		defer extenderDnsHostedZoneIdsStateLock.Unlock()
		return extenderDnsHostedZoneIds[name]
	}()
	if cachedHostedZoneId != "" {
		return cachedHostedZoneId, nil
	}

	output, err := api.ListHostedZonesByNameWithContext(
		ctx,
		&route53.ListHostedZonesByNameInput{
			DNSName: aws.String(name),
		},
	)
	if err != nil {
		return "", err
	}
	matchedHostedZoneIds := []string{}
	for _, hostedZone := range output.HostedZones {
		if extenderDnsFqdn(aws.StringValue(hostedZone.Name)) != name {
			continue
		}
		// the id comes back as `/hostedzone/<id>`; every call takes the bare id
		matchedHostedZoneIds = append(
			matchedHostedZoneIds,
			strings.TrimPrefix(aws.StringValue(hostedZone.Id), "/hostedzone/"),
		)
	}
	switch len(matchedHostedZoneIds) {
	case 0:
		return "", fmt.Errorf("there is no hosted zone named %s", name)
	case 1:
	default:
		return "", fmt.Errorf(
			"there are %d hosted zones named %s; configure hosted_zone_id",
			len(matchedHostedZoneIds),
			name,
		)
	}

	func() {
		extenderDnsHostedZoneIdsStateLock.Lock()
		defer extenderDnsHostedZoneIdsStateLock.Unlock()
		extenderDnsHostedZoneIds[name] = matchedHostedZoneIds[0]
	}()
	return matchedHostedZoneIds[0], nil
}

// The publisher an operator runs (C5).
//
// The zone is held as it was configured rather than as an id: an id is
// resolved from a name on the first apply, so a publisher built before aws is
// reachable is still a usable publisher.
type route53ExtenderDnsPublisher struct {
	api            route53Api
	hostedZoneId   string
	hostedZoneName string
}

// newRoute53ExtenderDnsPublisher builds the aws session from the dns block.
func newRoute53ExtenderDnsPublisher(
	config *controller.ExtenderConfig,
) (*route53ExtenderDnsPublisher, error) {
	hostedZoneId := strings.TrimSpace(config.Dns.HostedZoneId)
	hostedZoneName := strings.TrimSpace(config.Dns.HostedZoneName)
	if hostedZoneId == "" && hostedZoneName == "" {
		return nil, fmt.Errorf("the extender dns has no hosted zone")
	}

	api, err := newExtenderRoute53Api(
		config.Dns.AwsRegion,
		config.Dns.AwsAccessKeyId,
		config.Dns.AwsSecretAccessKey,
	)
	if err != nil {
		return nil, err
	}
	return &route53ExtenderDnsPublisher{
		api:            api,
		hostedZoneId:   hostedZoneId,
		hostedZoneName: hostedZoneName,
	}, nil
}

// publish writes the desired state as one change batch.
func (self *route53ExtenderDnsPublisher) publish(
	ctx context.Context,
	recordName string,
	ttl int,
	desiredSets []*extenderDnsRecordSet,
) error {
	name := extenderDnsFqdn(recordName)

	hostedZoneId, err := resolveExtenderDnsHostedZoneId(
		ctx,
		self.api,
		self.hostedZoneId,
		self.hostedZoneName,
	)
	if err != nil {
		return err
	}

	existingSetIdentifiers, existingRecordSets, err := self.listRecordSets(ctx, hostedZoneId, name)
	if err != nil {
		return err
	}

	changes := []*route53.Change{}
	for _, desiredSet := range desiredSets {
		changes = append(changes, &route53.Change{
			Action:            aws.String(route53.ChangeActionUpsert),
			ResourceRecordSet: extenderDnsResourceRecordSet(name, ttl, desiredSet),
		})
	}
	for _, setIdentifier := range extenderDnsDeletedSetIdentifiers(
		existingSetIdentifiers,
		desiredSets,
	) {
		// a delete must carry the set exactly as it exists, which is why the
		// listing is kept rather than rebuilt from the identifier
		changes = append(changes, &route53.Change{
			Action:            aws.String(route53.ChangeActionDelete),
			ResourceRecordSet: existingRecordSets[setIdentifier],
		})
	}
	if len(changes) == 0 {
		// no active address anywhere and nothing of ours in the zone; an empty
		// batch is rejected by the api
		return nil
	}

	_, err = self.api.ChangeResourceRecordSetsWithContext(
		ctx,
		&route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(hostedZoneId),
			ChangeBatch: &route53.ChangeBatch{
				Changes: changes,
			},
		},
	)
	return err
}

// listRecordSets reads the sets of this publisher already in the zone, in the
// order the zone returns them.
//
// The listing starts at the record name, and the zone is ordered by name, so
// the first set of another name is the end of ours -- there is no reason to
// page through the rest of a zone that may hold thousands of unrelated
// records.
func (self *route53ExtenderDnsPublisher) listRecordSets(
	ctx context.Context,
	hostedZoneId string,
	name string,
) ([]string, map[string]*route53.ResourceRecordSet, error) {
	setIdentifiers := []string{}
	recordSets := map[string]*route53.ResourceRecordSet{}

	err := self.api.ListResourceRecordSetsPagesWithContext(
		ctx,
		&route53.ListResourceRecordSetsInput{
			HostedZoneId:    aws.String(hostedZoneId),
			StartRecordName: aws.String(name),
		},
		func(output *route53.ListResourceRecordSetsOutput, _ bool) bool {
			for _, recordSet := range output.ResourceRecordSets {
				if extenderDnsFqdn(aws.StringValue(recordSet.Name)) != name {
					return false
				}
				recordType := aws.StringValue(recordSet.Type)
				if !slices.Contains(extenderDnsRecordTypes, recordType) {
					continue
				}
				setIdentifier := aws.StringValue(recordSet.SetIdentifier)
				if !strings.HasPrefix(setIdentifier, extenderDnsSetIdentifierPrefix) {
					// someone else's record at our name; upserting our sets
					// beside it is the api's business, deleting it is not ours
					continue
				}
				if _, ok := recordSets[setIdentifier]; ok {
					continue
				}
				setIdentifiers = append(setIdentifiers, setIdentifier)
				recordSets[setIdentifier] = recordSet
			}
			return true
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return setIdentifiers, recordSets, nil
}

// The wire form of one desired set: geolocation routing by continent, or the
// `*` country that Route 53 reads as the default location, and no health
// check -- an address is in the set because the uptime probes say it answers,
// and a second opinion from Route 53 would only remove addresses this
// operator still believes in.
func extenderDnsResourceRecordSet(
	name string,
	ttl int,
	desiredSet *extenderDnsRecordSet,
) *route53.ResourceRecordSet {
	geoLocation := &route53.GeoLocation{
		CountryCode: aws.String("*"),
	}
	if desiredSet.continentCode != "" {
		geoLocation = &route53.GeoLocation{
			ContinentCode: aws.String(desiredSet.continentCode),
		}
	}
	resourceRecords := []*route53.ResourceRecord{}
	for _, ip := range desiredSet.ips {
		resourceRecords = append(resourceRecords, &route53.ResourceRecord{
			Value: aws.String(ip),
		})
	}
	for _, record := range desiredSet.records {
		resourceRecords = append(resourceRecords, &route53.ResourceRecord{
			Value: aws.String(extenderDnsTxtValue(record)),
		})
	}
	return &route53.ResourceRecordSet{
		Name:            aws.String(name),
		Type:            aws.String(desiredSet.recordType()),
		TTL:             aws.Int64(int64(ttl)),
		SetIdentifier:   aws.String(desiredSet.setIdentifier()),
		GeoLocation:     geoLocation,
		ResourceRecords: resourceRecords,
	}
}

// extenderDnsTxtMaxStringLength is the longest character string one TXT
// record may carry (RFC 1035); a longer value is several strings, which a
// resolver joins.
const extenderDnsTxtMaxStringLength = 255

// extenderDnsTxtValue is the Route 53 form of one TXT value: quoted, split into
// strings no longer than the wire allows. A signed record is base64, which
// contains neither a quote nor a backslash, so no escaping is needed and none
// is done -- a value that needed it would be a bug upstream, not a case to
// handle here.
func extenderDnsTxtValue(value string) string {
	strs := []string{}
	for len(value) > extenderDnsTxtMaxStringLength {
		strs = append(strs, fmt.Sprintf("%q", value[:extenderDnsTxtMaxStringLength]))
		value = value[extenderDnsTxtMaxStringLength:]
	}
	strs = append(strs, fmt.Sprintf("%q", value))
	return strings.Join(strs, " ")
}
