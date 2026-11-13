package work

// What the extender dns tick actually landed.
//
// The sampled sets are recomputed every tick and never stored, so the database
// can only say what was ELIGIBLE -- the active address pool. Only the publisher
// knows what Route 53 accepted. These gauges are therefore the sole source for
// "what is in dns right now", which is what makes them worth having rather than
// a derived view of the pool.
//
// They count the sets whose change Route 53 accepted in the last tick, never
// the sets the tick wanted. From 2026-10-28 18:12Z every tick's batch was
// refused while gauges drawn from the desired sets read 62 extenders in dns
// (factury/research/extender-health-2026-11-11.md §1.4). A set that failed
// this tick is not counted, even though the zone may still hold what an
// earlier tick wrote: that copy is stale, and a TXT set's records expire within
// a day.
//
// The tick runs on whichever taskworker claims it, so a process that last ran
// it an hour ago still holds that hour-old view, and a plain gauge would keep
// pushing it as current beside the fresh view of the process that ran the last
// tick. The gauges are therefore exported by a collector that reports an
// observation only while it is younger than extenderDnsObservationTtl, as the
// seed chain's are (extender_seed_metrics.go): a dashboard's `max` over
// instances reads only fresh views.
//
// Every change a tick wanted is counted by its outcome in an ordinary
// per-process counter, read through increase(): accepted, or failed -- refused
// by Route 53, a request that errored, or a change that was never sent because
// the apply failed first or an upsert of its type failed. A tick that fails
// before it draws any set (a dns block with no record name) counts no change;
// its gauges read zero.
//
// A country answered by sets of its own (extenderDnsCnSet) is counted in every
// gauge above like any location, and on its own in a gauge per country and
// record type, the values Route 53 accepted in its sets: addresses in its A
// and AAAA sets, signed records in its TXT set. China's TXT set holds one
// record by default, so a zero there is China answered by its continent's
// sets again.
//
// Every label is a bounded enum, so the series count is fixed no matter how
// large the extender population grows. Registered with the default prometheus
// registry, which `server.StartStatsPusher` pushes to mimir automatically.

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// How long one observation is exported: three ticks, so one late or missed
// tick does not read as a gap.
const extenderDnsObservationTtl = 3 * ExtenderPublishTimeout

// The outcomes of a change.
const (
	extenderDnsChangeAccepted = "accepted"
	extenderDnsChangeFailed   = "failed"
)

var (
	extenderDnsSetsDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_dns_sets",
		"Extender dns record sets Route 53 accepted in the last tick, per record type",
		[]string{"record_type"}, nil,
	)
	extenderDnsAddressesDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_dns_addresses",
		"Extender addresses in the sets Route 53 accepted in the last tick, per ip version",
		[]string{"ip_version"}, nil,
	)
	extenderDnsTxtRecordsDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_dns_txt_records",
		"Signed extender records in the TXT sets Route 53 accepted in the last tick",
		nil, nil,
	)
	extenderDnsExtendersDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_dns_extenders",
		"Distinct extenders reachable through the addresses Route 53 accepted in the last tick",
		nil, nil,
	)
	extenderDnsCountryRecordsDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_dns_country_records",
		"Values in the sets of a country answered by extender dns sets of its own that Route 53 accepted in the last tick, per country and record type: addresses in A and AAAA, signed records in TXT",
		[]string{"country", "record_type"}, nil,
	)
)

// The countries answered by sets of their own, each published with every
// record type.
var extenderDnsCountryCodes = []string{extenderDnsCnSet.countryCode}

// One country's values of one record type.
type extenderDnsCountryRecordsKey struct {
	countryCode string
	recordType  string
}

var extenderDnsChangesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_dns_changes_total",
	Help:      "Extender dns set changes the publish tick wanted, per record type and outcome (accepted by Route 53, or failed)",
}, []string{"record_type", "outcome"})

// What one tick landed.
type extenderDnsObservation struct {
	observedAt         time.Time
	setsByType         map[string]int
	addressesByVersion map[string]int
	txtRecords         int
	extenders          int
	countryRecords     map[extenderDnsCountryRecordsKey]int
}

type extenderDnsCollector struct {
	stateLock   sync.Mutex
	observation *extenderDnsObservation
	nowForTest  func() time.Time
}

var extenderDnsObservations = &extenderDnsCollector{}

func init() {
	prometheus.MustRegister(
		extenderDnsObservations,
		extenderDnsChangesCounter,
	)
	// every type and outcome exists from the start, so a first failure is an
	// increase from zero rather than a series that appears at one, which
	// increase() cannot see
	for _, recordType := range extenderDnsRecordTypes {
		for _, outcome := range []string{extenderDnsChangeAccepted, extenderDnsChangeFailed} {
			extenderDnsChangesCounter.WithLabelValues(recordType, outcome)
		}
	}
}

func (self *extenderDnsCollector) now() time.Time {
	if self.nowForTest != nil {
		return self.nowForTest()
	}
	return time.Now()
}

// observeExtenderDnsChanges counts every change of one tick by its outcome.
func observeExtenderDnsChanges(changes []*extenderDnsChange) {
	for _, change := range changes {
		outcome := extenderDnsChangeAccepted
		if change.err != nil {
			outcome = extenderDnsChangeFailed
		}
		extenderDnsChangesCounter.WithLabelValues(change.recordType, outcome).Inc()
	}
}

// observeExtenderDnsSets records what one tick landed: the sets Route 53
// accepted, and the addresses they were drawn from.
//
// Every label value is published, zeros included, so a record type or family
// that goes empty is a zero rather than a series that silently stops updating
// and keeps showing its last value.
func observeExtenderDnsSets(
	acceptedSets []*extenderDnsRecordSet,
	addresses []*model.NetworkExtenderDnsAddress,
) {
	ipExtenderIds := map[string]server.Id{}
	for _, address := range addresses {
		ipExtenderIds[address.Ip.String()] = address.ExtenderId
	}

	observation := &extenderDnsObservation{
		setsByType:         map[string]int{"A": 0, "AAAA": 0, "TXT": 0},
		addressesByVersion: map[string]int{"4": 0, "6": 0},
		countryRecords:     map[extenderDnsCountryRecordsKey]int{},
	}
	for _, countryCode := range extenderDnsCountryCodes {
		for _, recordType := range extenderDnsRecordTypes {
			observation.countryRecords[extenderDnsCountryRecordsKey{countryCode: countryCode, recordType: recordType}] = 0
		}
	}
	extenders := map[server.Id]bool{}
	for _, acceptedSet := range acceptedSets {
		if acceptedSet.countryCode != "" {
			key := extenderDnsCountryRecordsKey{countryCode: acceptedSet.countryCode, recordType: acceptedSet.recordType()}
			observation.countryRecords[key] += len(acceptedSet.ips) + len(acceptedSet.records)
		}
		switch {
		case 0 < len(acceptedSet.records):
			observation.setsByType["TXT"] += 1
			observation.txtRecords += len(acceptedSet.records)
		case acceptedSet.ipVersion == 4:
			observation.setsByType["A"] += 1
			observation.addressesByVersion["4"] += len(acceptedSet.ips)
		case acceptedSet.ipVersion == 6:
			observation.setsByType["AAAA"] += 1
			observation.addressesByVersion["6"] += len(acceptedSet.ips)
		}
		for _, ip := range acceptedSet.ips {
			if extenderId, ok := ipExtenderIds[ip]; ok {
				extenders[extenderId] = true
			}
		}
	}
	observation.extenders = len(extenders)

	extenderDnsObservations.stateLock.Lock()
	defer extenderDnsObservations.stateLock.Unlock()
	observation.observedAt = extenderDnsObservations.now()
	extenderDnsObservations.observation = observation
}

func (self *extenderDnsCollector) Describe(descs chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		extenderDnsSetsDesc,
		extenderDnsAddressesDesc,
		extenderDnsTxtRecordsDesc,
		extenderDnsExtendersDesc,
		extenderDnsCountryRecordsDesc,
	} {
		descs <- desc
	}
}

func (self *extenderDnsCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	observation := self.observation
	if observation == nil || extenderDnsObservationTtl <= self.now().Sub(observation.observedAt) {
		return
	}
	gauge := func(desc *prometheus.Desc, value int, labels ...string) {
		metrics <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(value), labels...)
	}
	for recordType, count := range observation.setsByType {
		gauge(extenderDnsSetsDesc, count, recordType)
	}
	for ipVersion, count := range observation.addressesByVersion {
		gauge(extenderDnsAddressesDesc, count, ipVersion)
	}
	gauge(extenderDnsTxtRecordsDesc, observation.txtRecords)
	gauge(extenderDnsExtendersDesc, observation.extenders)
	for key, count := range observation.countryRecords {
		gauge(extenderDnsCountryRecordsDesc, count, key.countryCode, key.recordType)
	}
}
