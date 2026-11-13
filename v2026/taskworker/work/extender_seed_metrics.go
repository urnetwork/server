package work

// What the seed publisher stored and what every mirror serves
// (connect/EXTENDER-UPDATE2.md §2.3: refresh the bundle before its validity
// ends and monitor every mirror's sequence and expiry).
//
// The task runs on whichever taskworker claims it, so a process that last ran it
// an hour ago still holds that hour-old view. A plain gauge would keep pushing
// it as current. These gauges are therefore exported by a collector that
// reports an observation only while it is younger than
// extenderSeedObservationTtl: a process that stopped observing stops
// exporting, and a dashboard's `max` over instances reads only fresh views.
// The two counters are ordinary per-process counters, read through rate().
//
// Every label is bounded: a mirror is labeled by its configured name (a short
// word, never an address or a host; ExtenderSeedConfig), a route by "dns",
// "ipv4" or "ipv6", a family by "4" or "6", and an outcome by a fixed code.

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
)

// How long one observation is exported: three task periods, so one late or
// missed pass does not read as a gap.
const extenderSeedObservationTtl = 3 * ExtenderSeedTimeout

var (
	extenderSeedEnabledDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_enabled",
		"1 while extender.yml enables the public seed bundle in the process that ran the last pass, else 0",
		nil, nil,
	)
	extenderSeedPublishedSequenceDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_published_sequence",
		"Sequence of the seed bundle the origin store holds",
		nil, nil,
	)
	extenderSeedPublishedIssuedDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_published_issued_timestamp_seconds",
		"Issue time of the seed bundle the origin store holds",
		nil, nil,
	)
	extenderSeedPublishedExpiresDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_published_expires_timestamp_seconds",
		"Expiry of the seed bundle the origin store holds",
		nil, nil,
	)
	extenderSeedPublishedRecordsDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_published_records",
		"Records of the stored seed bundle with an address of the family",
		[]string{"ip_version"}, nil,
	)
	extenderSeedPublishedRevocationsDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_published_revocations",
		"Revocations the stored seed bundle carries",
		nil, nil,
	)
	extenderSeedMirrorUpDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_mirror_up",
		"1 when the last check of the mirror route fetched a bundle that verified and was fresh, else 0",
		[]string{"mirror", "route"}, nil,
	)
	extenderSeedMirrorSequenceDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_mirror_sequence",
		"Sequence of the bundle the mirror route served at its last check, when it verified",
		[]string{"mirror", "route"}, nil,
	)
	extenderSeedMirrorIssuedDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_mirror_issued_timestamp_seconds",
		"Issue time of the bundle the mirror route served at its last check, when it verified",
		[]string{"mirror", "route"}, nil,
	)
	extenderSeedMirrorExpiresDesc = prometheus.NewDesc(
		"urnetwork_taskworker_extender_seed_mirror_expires_timestamp_seconds",
		"Expiry of the bundle the mirror route served at its last check, when it verified",
		[]string{"mirror", "route"}, nil,
	)
)

var extenderSeedPublishesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_seed_publishes_total",
	Help:      "Seed publisher passes by outcome",
}, []string{"outcome"})

var extenderSeedMirrorChecksCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_seed_mirror_checks_total",
	Help:      "Seed mirror route checks by outcome",
}, []string{"mirror", "route", "outcome"})

// The stored bundle as one pass read or wrote it.
type extenderSeedPublishedObservation struct {
	observedAt  time.Time
	sequence    uint64
	issueTime   time.Time
	expireTime  time.Time
	ipv4Records int
	ipv6Records int
	revocations int
}

type extenderSeedMirrorKey struct {
	mirror string
	route  string
}

// One mirror route's last check. The bundle's fields are set only when it
// verified, fresh or not.
type extenderSeedMirrorObservation struct {
	observedAt time.Time
	up         bool
	verified   bool
	sequence   uint64
	issueTime  time.Time
	expireTime time.Time
}

type extenderSeedCollector struct {
	stateLock  sync.Mutex
	enabledAt  time.Time
	enabled    bool
	published  *extenderSeedPublishedObservation
	mirrors    map[extenderSeedMirrorKey]*extenderSeedMirrorObservation
	nowForTest func() time.Time
}

var extenderSeedObservations = &extenderSeedCollector{
	mirrors: map[extenderSeedMirrorKey]*extenderSeedMirrorObservation{},
}

func init() {
	prometheus.MustRegister(
		extenderSeedObservations,
		extenderSeedPublishesCounter,
		extenderSeedMirrorChecksCounter,
	)
}

func (self *extenderSeedCollector) now() time.Time {
	if self.nowForTest != nil {
		return self.nowForTest()
	}
	return time.Now()
}

// Records whether the pass found the seed enabled. A disabled pass drops every
// other observation, so an off seed exports only its off state.
func (self *extenderSeedCollector) observeEnabled(enabled bool, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.enabled, self.enabledAt = enabled, at
	if !enabled {
		self.published = nil
		self.mirrors = map[extenderSeedMirrorKey]*extenderSeedMirrorObservation{}
	}
}

// Records the stored bundle after a pass, or forgets it when none verified.
func (self *extenderSeedCollector) observePublished(bundle *connect.ExtenderSeedBundle, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if bundle == nil {
		self.published = nil
		return
	}
	observation := &extenderSeedPublishedObservation{
		observedAt:  at,
		sequence:    bundle.Sequence,
		issueTime:   bundle.IssueTime,
		expireTime:  bundle.ExpireTime,
		revocations: len(bundle.Revocations),
	}
	for _, record := range bundle.Records {
		ipv4, ipv6 := false, false
		for _, address := range record.Body.Addresses {
			switch address.IpVersion {
			case 4:
				ipv4 = true
			case 6:
				ipv6 = true
			}
		}
		if ipv4 {
			observation.ipv4Records += 1
		}
		if ipv6 {
			observation.ipv6Records += 1
		}
	}
	self.published = observation
}

// Records one mirror route's check.
func (self *extenderSeedCollector) observeMirror(mirror string, route string, observation *extenderSeedMirrorObservation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mirrors[extenderSeedMirrorKey{mirror: mirror, route: route}] = observation
}

func (self *extenderSeedCollector) Describe(descs chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		extenderSeedEnabledDesc,
		extenderSeedPublishedSequenceDesc,
		extenderSeedPublishedIssuedDesc,
		extenderSeedPublishedExpiresDesc,
		extenderSeedPublishedRecordsDesc,
		extenderSeedPublishedRevocationsDesc,
		extenderSeedMirrorUpDesc,
		extenderSeedMirrorSequenceDesc,
		extenderSeedMirrorIssuedDesc,
		extenderSeedMirrorExpiresDesc,
	} {
		descs <- desc
	}
}

func (self *extenderSeedCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.now()
	fresh := func(at time.Time) bool {
		return !at.IsZero() && now.Sub(at) < extenderSeedObservationTtl
	}
	gauge := func(desc *prometheus.Desc, value float64, labels ...string) {
		metrics <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	}
	seconds := func(t time.Time) float64 {
		return float64(t.UnixMilli()) / 1000
	}
	boolValue := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	if fresh(self.enabledAt) {
		gauge(extenderSeedEnabledDesc, boolValue(self.enabled))
	}
	if published := self.published; published != nil && fresh(published.observedAt) {
		gauge(extenderSeedPublishedSequenceDesc, float64(published.sequence))
		gauge(extenderSeedPublishedIssuedDesc, seconds(published.issueTime))
		gauge(extenderSeedPublishedExpiresDesc, seconds(published.expireTime))
		gauge(extenderSeedPublishedRecordsDesc, float64(published.ipv4Records), "4")
		gauge(extenderSeedPublishedRecordsDesc, float64(published.ipv6Records), "6")
		gauge(extenderSeedPublishedRevocationsDesc, float64(published.revocations))
	}
	for key, observation := range self.mirrors {
		if !fresh(observation.observedAt) {
			continue
		}
		gauge(extenderSeedMirrorUpDesc, boolValue(observation.up), key.mirror, key.route)
		if !observation.verified {
			continue
		}
		gauge(extenderSeedMirrorSequenceDesc, float64(observation.sequence), key.mirror, key.route)
		gauge(extenderSeedMirrorIssuedDesc, seconds(observation.issueTime), key.mirror, key.route)
		gauge(extenderSeedMirrorExpiresDesc, seconds(observation.expireTime), key.mirror, key.route)
	}
}
