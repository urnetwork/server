package work

// §21.6's proactive check (ABUSE-M5-13; §21.8, §2.8 ext-6, QD-554, QD-770): every hour each published host and each
// user record's owner under a root is looked up against Google Web Risk's locally matched Update API. Safe Browsing
// is non-commercial only, so it is Web Risk.
//
// A cycle starts by bringing the local lists current (server.WebRiskClient.Update), then pages the names
// (model.ListAbuseScanNames) by a cursor, a run stopping at a page's end once RunMaxTime has passed and the next run
// continuing the cycle after ContinueDelay. Each name is looked up as each of its url expressions
// (server.WebRiskHostExpressions): only an expression whose hash a local list holds a prefix of sends that prefix to
// hashes.search, and only a full hash among the answer's is a hit. A hit is §21.8's Web Risk signal: it goes to §21.8
// by abuse_scan_work.go's path and takes a host kill, or, on a root's own expression, opens a root incident and kills
// nothing.
//
// The client, its lists, the cursor and the listings are this process's memory (QD-554), so no row holds them and a
// restart fetches the lists in full and starts a new cycle. A list update that fails, a page of names that cannot be
// read (a process with no published-names source answers unavailable, which counts as a failed pass, QD-770), a
// search that fails and a hit that cannot reach §21.8 each end the run with a warning, nothing acted on from the
// page that failed and the cursor before it, so the page is read again when the run is retried after
// RetryInterval. The check idles, scheduling itself on, while abuse.yml leaves web_risk off or names no root, or
// vault holds no key.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Tunables of the check.
type AbuseWebRiskSettings struct {
	// How often a cycle starts: §21.6's hour.
	Interval time.Duration
	// How soon a run that stopped at RunMaxTime continues its cycle.
	ContinueDelay time.Duration
	// How soon a failed run is tried again.
	RetryInterval time.Duration
	// One run's own bound: past it the run stops at a page's end and the cycle continues in the next run.
	RunMaxTime time.Duration
	// The task runner's bound on one run, past RunMaxTime and a page's work, a page of published names loading each
	// quiet network it reaches (QD-770).
	TaskMaxTime time.Duration
	// The most listings the check remembers (abuseScanListings). Past it a new hit reads as listed since an unknown
	// time, which suppresses a host with a kill lifted as wrong rather than kill it again.
	ListingMaxCount int
	// The names' pages.
	Names *model.AbuseScanSettings
	// The client's.
	Client *server.WebRiskClientSettings
}

// An hourly cycle in runs of at most 15 minutes, on pages of a hundred published names, since each quiet network a
// page reaches is loaded before its names are known (QD-770).
func DefaultAbuseWebRiskSettings() *AbuseWebRiskSettings {
	names := model.DefaultAbuseScanSettings()
	names.PublishedPageCount = 100
	return &AbuseWebRiskSettings{
		Interval:        time.Hour,
		ContinueDelay:   time.Minute,
		RetryInterval:   10 * time.Minute,
		RunMaxTime:      15 * time.Minute,
		TaskMaxTime:     time.Hour,
		ListingMaxCount: 100000,
		Names:           names,
		Client:          server.DefaultWebRiskClientSettings(),
	}
}

type AbuseWebRiskArgs struct {
}

// What one run did. Counts only: no name, network or prefix.
type AbuseWebRiskResult struct {
	// web_risk off, no root, no key, or another run holds the check
	Idle bool `json:"idle"`
	// the run stopped at a failure, acting on nothing from the page that failed
	Failed bool `json:"failed"`
	// the run stopped at its bound with the cycle unfinished
	More bool `json:"more"`
	// names read, expressions whose prefix a local list holds, and full-hash matches
	NameCount        int `json:"name_count"`
	PrefixMatchCount int `json:"prefix_match_count"`
	ConfirmedCount   int `json:"confirmed_count"`
	// what became of the hits (abuseScanHitCounts)
	ReportedCount     int `json:"reported_count"`
	ActedCount        int `json:"acted_count"`
	UnjoinedCount     int `json:"unjoined_count"`
	SuppressedCount   int `json:"suppressed_count"`
	RootIncidentCount int `json:"root_incident_count"`
}

func (self *AbuseWebRiskResult) addHits(counts *abuseScanHitCounts) {
	self.ReportedCount += counts.Reported
	self.ActedCount += counts.Acted
	self.UnjoinedCount += counts.Unjoined
	self.SuppressedCount += counts.Suppressed
	self.RootIncidentCount += counts.RootIncidents
}

// How the run ended, as the run counter labels it.
func (self *AbuseWebRiskResult) outcome() string {
	switch {
	case self.Idle:
		return "idle"
	case self.Failed:
		return "failed"
	case self.More:
		return "more"
	}
	return "done"
}

// The check's seams, which a test replaces.
type abuseWebRiskDeps struct {
	now    func() time.Time
	config func() *server.AbuseConfig
	apiKey func() (string, error)
	// how the client reaches the API; nil dials directly
	transport http.RoundTripper
	names     func(ctx context.Context, cursor *model.AbuseScanNamesCursor, rootNames []string, now time.Time, settings *model.AbuseScanSettings) ([]*model.AbuseScanName, *model.AbuseScanNamesCursor, error)
	intake    *abuseScanIntake
}

func defaultAbuseWebRiskDeps() *abuseWebRiskDeps {
	return &abuseWebRiskDeps{
		now:    server.NowUtc,
		config: server.GetAbuseConfig,
		apiKey: server.LoadWebRiskApiKey,
		names:  model.ListAbuseScanNames,
		intake: defaultAbuseScanIntake(),
	}
}

// The check's state in this process. Runs take turns through running, and only the run holding it touches the
// fields below it, so no lock is held across the API's or the store's I/O.
type abuseWebRiskScan struct {
	settings *AbuseWebRiskSettings
	deps     *abuseWebRiskDeps

	running   atomic.Bool
	client    *server.WebRiskClient
	clientKey string
	// a cycle is in progress, and its next page resumes at cursor
	inCycle  bool
	cursor   *model.AbuseScanNamesCursor
	listings *abuseScanListings
}

func newAbuseWebRiskScan(settings *AbuseWebRiskSettings, deps *abuseWebRiskDeps) *abuseWebRiskScan {
	return &abuseWebRiskScan{
		settings: settings,
		deps:     deps,
		listings: newAbuseScanListings(settings.ListingMaxCount),
	}
}

// The process's check, one a process (QD-554).
var abuseWebRiskProcess = struct {
	stateLock sync.Mutex
	scan      *abuseWebRiskScan
}{}

func processAbuseWebRiskScan() *abuseWebRiskScan {
	abuseWebRiskProcess.stateLock.Lock()
	defer abuseWebRiskProcess.stateLock.Unlock()
	if abuseWebRiskProcess.scan == nil {
		abuseWebRiskProcess.scan = newAbuseWebRiskScan(DefaultAbuseWebRiskSettings(), defaultAbuseWebRiskDeps())
	}
	return abuseWebRiskProcess.scan
}

// One run: the cycle's start if none is in progress, then its pages until the cycle ends, the run's bound passes or
// something fails. See the file header.
func (self *abuseWebRiskScan) run(ctx context.Context) (result *AbuseWebRiskResult) {
	result = &AbuseWebRiskResult{}
	if !self.running.CompareAndSwap(false, true) {
		result.Idle = true
		return
	}
	defer self.running.Store(false)
	defer func() {
		abuseScanRunCounter.WithLabelValues(abuseScanWebRisk, result.outcome()).Inc()
	}()

	config := self.deps.config()
	rootNames := config.RootNames()
	if !config.WebRiskEnabled() || len(rootNames) == 0 {
		result.Idle = true
		return
	}
	key, err := self.deps.apiKey()
	if err != nil {
		glog.Warningf("[abuse]web risk is on with no key, so the check idles: %v\n", err)
		result.Idle = true
		return
	}
	if self.client == nil || self.clientKey != key {
		client, err := server.NewWebRiskClient(&server.WebRiskClientArgs{
			ApiKey:    key,
			Transport: self.deps.transport,
			Settings:  self.settings.Client,
		})
		if err != nil {
			glog.Warningf("[abuse]web risk's client could not start, so this pass acts on nothing: %v\n", err)
			result.Failed = true
			return
		}
		if self.client != nil {
			self.client.Close()
		}
		self.client, self.clientKey = client, key
		// a new client holds no lists: the cycle starts over with them
		self.inCycle = false
	}

	start := self.deps.now()
	if !self.inCycle {
		if err := self.client.Update(ctx, start); err != nil {
			glog.Warningf("[abuse]web risk's lists could not be brought current, so this pass acts on nothing: %v\n", err)
			result.Failed = true
			return
		}
		self.inCycle = true
		self.cursor = nil
		self.listings.startCycle(start)
	}
	for {
		now := self.deps.now()
		names, next, err := self.deps.names(ctx, self.cursor, rootNames, now, self.settings.Names)
		if err != nil {
			glog.Warningf("[abuse]the names web risk checks could not be read, so the pass stops and reads the page again: %v\n", err)
			result.Failed = true
			return
		}
		result.NameCount += len(names)
		hits, err := self.lookup(ctx, names, now, result)
		if err != nil {
			glog.Warningf("[abuse]web risk could not be searched, so a page acts on nothing and is read again: %v\n", err)
			result.Failed = true
			return
		}
		counts, err := reportAbuseScanHits(
			ctx,
			self.deps.intake,
			self.listings,
			abuseScanWebRisk,
			model.AbuseReporterClassWebRisk,
			model.AbuseSignalWebRiskHit,
			hits,
			rootNames,
			now,
		)
		result.addHits(counts)
		if err != nil {
			glog.Warningf("[abuse]a web risk hit could not reach intake, so its page is read again: %v\n", err)
			result.Failed = true
			return
		}
		self.cursor = next
		if next == nil {
			self.listings.endCycle()
			self.inCycle = false
			return
		}
		if self.settings.RunMaxTime <= self.deps.now().Sub(start) || ctx.Err() != nil {
			result.More = true
			return
		}
	}
}

// The hits among names at now: each url expression looked up once a page. An error is a search failing.
func (self *abuseWebRiskScan) lookup(ctx context.Context, names []*model.AbuseScanName, now time.Time, result *AbuseWebRiskResult) ([]*abuseScanHit, error) {
	hits := []*abuseScanHit{}
	seenExpressions := map[string]bool{}
	for _, name := range names {
		for _, expression := range server.WebRiskHostExpressions(name.Name, self.settings.Client) {
			if seenExpressions[expression] {
				continue
			}
			seenExpressions[expression] = true
			lookup, err := self.client.Lookup(ctx, sha256.Sum256([]byte(expression)), now)
			if err != nil {
				return nil, err
			}
			if lookup.PrefixMatch {
				result.PrefixMatchCount += 1
			}
			if 0 < len(lookup.ThreatTypes) {
				result.ConfirmedCount += 1
				hits = append(hits, &abuseScanHit{
					host:     strings.TrimSuffix(expression, "/"),
					category: abuseWebRiskCategory(lookup.ThreatTypes),
				})
			}
		}
	}
	return hits, nil
}

// §21.4's row for the lists a full hash is on: social engineering is phishing, and malware and unwanted software
// are malware.
func abuseWebRiskCategory(threatTypes []server.WebRiskThreatType) model.AbuseCategory {
	for _, threatType := range threatTypes {
		switch threatType {
		case server.WebRiskThreatTypeSocialEngineering, server.WebRiskThreatTypeSocialEngineeringExtendedCoverage:
			return model.AbuseCategoryPhishing
		}
	}
	return model.AbuseCategoryMalware
}

// Schedules the check's next run one Interval from now, once. A binary that does not link MG5 schedules nothing.
func ScheduleAbuseWebRisk(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	scheduleAbuseWebRisk(clientSession, tx, server.NowUtc().Add(DefaultAbuseWebRiskSettings().Interval))
}

func scheduleAbuseWebRisk(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		AbuseWebRisk,
		&AbuseWebRiskArgs{},
		clientSession,
		task.RunOnce("abuse_web_risk"),
		task.RunAt(runAt),
		task.MaxTime(DefaultAbuseWebRiskSettings().TaskMaxTime),
	)
}

// One run of the process's check.
func AbuseWebRisk(abuseWebRisk *AbuseWebRiskArgs, clientSession *session.ClientSession) (*AbuseWebRiskResult, error) {
	if !server.SidecarMg5Linked() {
		return &AbuseWebRiskResult{Idle: true}, nil
	}
	result := processAbuseWebRiskScan().run(clientSession.Ctx)
	if 0 < result.ConfirmedCount || 0 < result.SuppressedCount {
		glog.Infof("[abuse]web risk confirmed %d listings: %d reported, %d acted on, %d suppressed, %d root incidents\n",
			result.ConfirmedCount, result.ReportedCount, result.ActedCount, result.SuppressedCount, result.RootIncidentCount)
	}
	return result, nil
}

// Schedules the next run: soon for an unfinished cycle, after RetryInterval for a failed run, else after Interval.
func AbuseWebRiskPost(
	abuseWebRisk *AbuseWebRiskArgs,
	abuseWebRiskResult *AbuseWebRiskResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if !server.SidecarMg5Linked() {
		return nil
	}
	settings := DefaultAbuseWebRiskSettings()
	now := server.NowUtc()
	runAt := now.Add(settings.Interval)
	switch {
	case abuseWebRiskResult.More:
		runAt = now.Add(settings.ContinueDelay)
	case abuseWebRiskResult.Failed:
		runAt = now.Add(settings.RetryInterval)
	}
	scheduleAbuseWebRisk(clientSession, tx, runAt)
	return nil
}
