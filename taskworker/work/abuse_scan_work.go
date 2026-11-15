package work

// What the abuse worker's scans share (ABUSE-M5-13; §21.6, §21.8, §21.10): the one way a listing reaches §21.8, and
// the memory of which listings recur.
//
// A Web Risk or feed hit names a host a source lists. A hit on a root itself, or on a name above one, opens a root
// incident and kills nothing (model.OpenAbuseRootIncident): no ladder acts on a root (D-ABUSE-1). A hit under a root
// goes to §21.8 as a report of the scanner's class (controller.AcceptAbuseReport), whose signal takes the host kill,
// so a machine acts only on §21.8's rows. A hit on a name under no root is no URnetwork name, and is dropped.
//
// Hits recur: each read of a source lists again every host it still lists, which §21.10's restore gap relies on, so
// a kill lost with the primary comes back on the next pass. A kill review lifted as wrong must not come back that
// way. So a host with a machine action lifted as wrong (model.GetAbuseScanLiftedWrongTime) is not reported while the
// source's listing of it has not changed since the lift, a listing changing when the source has been read whole once
// without the host and then lists it again. abuseScanListings remembers, for one source, each host it lists and since
// when, cycle by cycle, a cycle being one whole read of the source: the hourly check's pass over every name, or one
// fetch of a feed. It is this process's memory alone, so a host listed since before the process could see reads as
// listed since before any lift, and a restart suppresses rather than kills again.
//
// A source that cannot be read acts on nothing and ends no cycle, so silence is never taken for a listing gone, nor
// for one present. No log names a host or a network, and no metric carries either (I-13, §2.8 ops-1).

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The scans' names, as their counters label them.
const (
	abuseScanWebRisk = "web_risk"
	abuseScanFeed    = "feed"
	abuseScanCtWatch = "ct_watch"
)

// Counts hits by scan and what became of each: reported, acted, unjoined, suppressed, root incident, or dropped as
// under no root. No label names a host or a network.
var abuseScanHitCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "scan_hits_total",
		Help:      "Abuse scanner hits, by scan and what became of them",
	},
	[]string{"scan", "result"},
)

// Counts runs by scan and how they ended: idle, done, more, failed.
var abuseScanRunCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "scan_runs_total",
		Help:      "Abuse scanner runs, by scan and how they ended",
	},
	[]string{"scan", "result"},
)

func init() {
	prometheus.MustRegister(abuseScanHitCounter, abuseScanRunCounter)
}

// One listing a scan found: a host a source lists, and the §21.4 row the source lists it for.
type abuseScanHit struct {
	// lowercase, with no trailing dot
	host     string
	category model.AbuseCategory
}

// What acting on one source's hits did.
type abuseScanHitCounts struct {
	// went to §21.8 and joined a network
	Reported int
	// of those, took a machine action now
	Acted int
	// went to §21.8 and joined nothing
	Unjoined int
	// a listing unchanged since review lifted its host's kill as wrong
	Suppressed int
	// a hit on a root, or above one
	RootIncidents int
	// a name under no root
	Dropped int
}

// The scans' way to §21.8 and the store, which a test replaces.
type abuseScanIntake struct {
	accept           func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error)
	liftedWrongTime  func(ctx context.Context, host string) (time.Time, bool)
	openRootIncident func(ctx context.Context, args *model.AbuseRootIncidentArgs, now time.Time) error
}

// §21.8's intake, the store's lift lookup and root incidents, on §21's settings with abuse.yml laid over them.
func defaultAbuseScanIntake() *abuseScanIntake {
	return &abuseScanIntake{
		accept:          controller.AcceptAbuseReport,
		liftedWrongTime: model.GetAbuseScanLiftedWrongTime,
		openRootIncident: func(ctx context.Context, args *model.AbuseRootIncidentArgs, now time.Time) error {
			settings := model.DefaultAbuseSettings()
			model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
			_, _, err := model.OpenAbuseRootIncident(ctx, args, now, settings)
			return err
		},
	}
}

// One host's listing by one source.
type abuseScanListing struct {
	// the last cycle that listed the host
	listedCycle int64
	// the last cycle that acted on it: reported it, or found it suppressed
	handledCycle int64
	// when the host's unbroken run of listings began; zero when it began before this process could see it
	since time.Time
}

// The listings of one source this process has seen, cycle by cycle (the file header). Not safe for concurrent use:
// its scan's one run at a time owns it.
type abuseScanListings struct {
	// the most hosts remembered: past it a new host reads as listed since an unknown time, which suppresses
	maxCount int
	// the cycle in progress, from 1 once one has started, and when it started
	cycle      int64
	cycleStart time.Time
	// the last cycle that read the source whole
	completeCycle int64
	// the last cycle in which a host went unremembered for the bound
	overflowCycle int64
	hosts         map[string]*abuseScanListing
}

func newAbuseScanListings(maxCount int) *abuseScanListings {
	return &abuseScanListings{
		maxCount: maxCount,
		hosts:    map[string]*abuseScanListing{},
	}
}

// Starts a cycle at now: a whole read of the source begins.
func (self *abuseScanListings) startCycle(now time.Time) {
	self.cycle += 1
	self.cycleStart = now
}

// Records that the source lists host in the cycle in progress. since is when the host's unbroken run of listings
// began, zero when it began before this process could see it; handled is whether the cycle already acted on it.
func (self *abuseScanListings) list(host string) (since time.Time, handled bool) {
	listing, ok := self.hosts[host]
	if !ok {
		if self.maxCount <= len(self.hosts) {
			self.overflowCycle = self.cycle
			return time.Time{}, false
		}
		listing = &abuseScanListing{}
		// only a whole read of the source just before this one, remembering every host it listed, shows this host
		// was not listed then
		if 1 < self.cycle && self.completeCycle == self.cycle-1 && self.overflowCycle < self.cycle-1 {
			listing.since = self.cycleStart
		}
		self.hosts[host] = listing
	}
	listing.listedCycle = self.cycle
	return listing.since, listing.handledCycle == self.cycle
}

// Records that the cycle in progress acted on host.
func (self *abuseScanListings) handle(host string) {
	if listing, ok := self.hosts[host]; ok {
		listing.handledCycle = self.cycle
	}
}

// Ends the cycle in progress as a whole read of the source: the hosts it did not list are forgotten, so a host
// listed again later starts a new run.
func (self *abuseScanListings) endCycle() {
	for host, listing := range self.hosts {
		if listing.listedCycle < self.cycle {
			delete(self.hosts, host)
		}
	}
	self.completeCycle = self.cycle
}

// Acts on hits of one source in its cycle in progress at now, each host once a cycle, as the file header says: a root
// incident, a drop, a suppression, or a report of class on §21.8's signal, whose host kill the intake takes. An error
// is the store or the intake not answering: hits before it were acted on and stay so, and the one that failed is not
// marked, so the cycle's next try acts on it.
func reportAbuseScanHits(
	ctx context.Context,
	intake *abuseScanIntake,
	listings *abuseScanListings,
	scan string,
	class model.AbuseReporterClass,
	signal model.AbuseSignal,
	hits []*abuseScanHit,
	rootNames []string,
	now time.Time,
) (*abuseScanHitCounts, error) {
	counts := &abuseScanHitCounts{}
	count := func(result string) {
		abuseScanHitCounter.WithLabelValues(scan, result).Inc()
	}
	for _, hit := range hits {
		if root, ok := model.AbuseScanCoveredRoot(hit.host, rootNames); ok {
			// one incident a root and category a cycle, under a key no host name can take
			incidentKey := root + " " + string(hit.category)
			if _, handled := listings.list(incidentKey); handled {
				continue
			}
			err := intake.openRootIncident(ctx, &model.AbuseRootIncidentArgs{
				Root:          root,
				Category:      hit.category,
				ReporterClass: class,
				Signal:        signal,
			}, now)
			if err != nil {
				count("failed")
				return counts, err
			}
			listings.handle(incidentKey)
			counts.RootIncidents += 1
			count("root_incident")
			continue
		}
		if !model.AbuseScanUnderRoot(hit.host, rootNames) {
			counts.Dropped += 1
			count("dropped")
			continue
		}
		since, handled := listings.list(hit.host)
		if handled {
			continue
		}
		if liftTime, ok := intake.liftedWrongTime(ctx, hit.host); ok && (since.IsZero() || !since.After(liftTime)) {
			listings.handle(hit.host)
			counts.Suppressed += 1
			count("suppressed")
			continue
		}
		result, err := intake.accept(ctx, &model.AbuseReport{
			Category:      hit.category,
			ReporterClass: class,
			Url:           hit.host,
		})
		if err != nil {
			count("failed")
			return counts, err
		}
		listings.handle(hit.host)
		switch {
		case !result.Joined:
			counts.Unjoined += 1
			count("unjoined")
		case 0 < len(result.Actions):
			counts.Reported += 1
			counts.Acted += 1
			count("acted")
		default:
			counts.Reported += 1
			count("reported")
		}
	}
	return counts, nil
}
