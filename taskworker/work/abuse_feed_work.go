package work

// §21.8's feeds (ABUSE-M5-13; §21.6, QD-499): every hour each feed abuse.yml lists is fetched whole, a plain list of
// urls or hosts, one a line, and each name it lists under a root is §21.8's feed signal, which goes to §21.8 by
// abuse_scan_work.go's path and takes a host kill. A name that is a root, or above one, opens a root incident and
// kills nothing, and a name under no root is dropped as it is read. The feed's category in abuse.yml is every hit's
// §21.4 row; a feed with no category the worker takes is skipped with a warning.
//
// A fetch is bounded in bytes (FeedMaxByteCount, QD-499's 64 MiB), in lines and in the length of one line, and one
// pass acts on at most FeedHitMaxCount names of a feed. A feed past a bound is cut there, its partial last line
// dropped, and counted: what was read is acted on, but a cut read is not a whole one, so it ends no cycle of the
// feed's listings. A feed that cannot be fetched, or answers anything but a 200, warns and acts on nothing, its
// listings left as they were. Each feed is its own source for abuseScanListings, so one failing says nothing of
// another's listings.
//
// Feeds are public lists fetched with no credential: a feed that needs one waits for a vault file of feed
// credentials, which nothing names yet. Which feeds, and their licences, stay with the owner (QD-499).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A feed gave no usable answer: unreachable, redirected away from https, or not a 200.
var errAbuseFeedSource = errors.New("abuse feed: the source gave no usable answer")

// Counts feeds cut at a bound: bytes, lines or hits. No label names a feed.
var abuseFeedCutCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "feed_cuts_total",
		Help:      "Abuse feeds cut at a bound, by the bound",
	},
	[]string{"bound"},
)

func init() {
	prometheus.MustRegister(abuseFeedCutCounter)
}

// Tunables of the feeds.
type AbuseFeedsSettings struct {
	// How often every feed is fetched.
	Interval time.Duration
	// The task runner's bound on one run of every feed.
	TaskMaxTime time.Duration
	// One feed's fetch deadline, its body included.
	RequestTimeout time.Duration
	// The most bytes one fetch reads: QD-499's 64 MiB.
	FeedMaxByteCount int64
	// The most lines one fetch reads.
	FeedMaxLineCount int
	// The longest line kept: a longer one names no host a join reads (AbuseSettings.ReportUrlMaxByteCount) and is
	// read past.
	LineMaxByteCount int
	// The most names under the roots one pass acts on for a feed. A feed listing more is an incident a person
	// reads, not one a machine acts on whole.
	FeedHitMaxCount int
	// The most redirects a fetch follows, each to https.
	RedirectMaxCount int
	// The most listings remembered for each feed (abuseScanListings).
	ListingMaxCount int
}

func DefaultAbuseFeedsSettings() *AbuseFeedsSettings {
	return &AbuseFeedsSettings{
		Interval:         time.Hour,
		TaskMaxTime:      30 * time.Minute,
		RequestTimeout:   5 * time.Minute,
		FeedMaxByteCount: 64 * 1024 * 1024,
		FeedMaxLineCount: 4 * 1024 * 1024,
		LineMaxByteCount: 8 * 1024,
		FeedHitMaxCount:  10000,
		RedirectMaxCount: 3,
		ListingMaxCount:  100000,
	}
}

type AbuseFeedsArgs struct {
}

// What one run did. Counts only: no name or network.
type AbuseFeedsResult struct {
	// no feed, no root, or another run holds the feeds
	Idle bool `json:"idle"`
	// a feed could not be read, or a hit of one could not reach §21.8
	Failed       bool `json:"failed"`
	FeedCount    int  `json:"feed_count"`
	SkippedCount int  `json:"skipped_count"`
	FailedCount  int  `json:"failed_count"`
	CutCount     int  `json:"cut_count"`
	// names read under or covering the roots, at most FeedHitMaxCount a feed
	HostCount int `json:"host_count"`
	// what became of them (abuseScanHitCounts)
	ReportedCount     int `json:"reported_count"`
	ActedCount        int `json:"acted_count"`
	UnjoinedCount     int `json:"unjoined_count"`
	SuppressedCount   int `json:"suppressed_count"`
	RootIncidentCount int `json:"root_incident_count"`
}

func (self *AbuseFeedsResult) addHits(counts *abuseScanHitCounts) {
	self.ReportedCount += counts.Reported
	self.ActedCount += counts.Acted
	self.UnjoinedCount += counts.Unjoined
	self.SuppressedCount += counts.Suppressed
	self.RootIncidentCount += counts.RootIncidents
}

func (self *AbuseFeedsResult) outcome() string {
	switch {
	case self.Idle:
		return "idle"
	case self.Failed:
		return "failed"
	}
	return "done"
}

// The feeds' seams, which a test replaces.
type abuseFeedDeps struct {
	now    func() time.Time
	config func() *server.AbuseConfig
	// how a fetch reaches a feed; nil dials directly
	transport http.RoundTripper
	intake    *abuseScanIntake
}

func defaultAbuseFeedDeps() *abuseFeedDeps {
	return &abuseFeedDeps{
		now:    server.NowUtc,
		config: server.GetAbuseConfig,
		intake: defaultAbuseScanIntake(),
	}
}

// The feeds' state in this process. Runs take turns through running, and only the run holding it touches
// feedListings, so no lock is held across a fetch or the store.
type abuseFeedScan struct {
	settings   *AbuseFeedsSettings
	deps       *abuseFeedDeps
	httpClient *http.Client

	running atomic.Bool
	// each feed's listings, by its url
	feedListings map[string]*abuseScanListings
}

func newAbuseFeedScan(settings *AbuseFeedsSettings, deps *abuseFeedDeps) *abuseFeedScan {
	transport := deps.transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}
	}
	return &abuseFeedScan{
		settings: settings,
		deps:     deps,
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if settings.RedirectMaxCount <= len(via) || request.URL.Scheme != "https" {
					return errAbuseFeedSource
				}
				return nil
			},
		},
		feedListings: map[string]*abuseScanListings{},
	}
}

// The process's feeds.
var abuseFeedProcess = struct {
	stateLock sync.Mutex
	scan      *abuseFeedScan
}{}

func processAbuseFeedScan() *abuseFeedScan {
	abuseFeedProcess.stateLock.Lock()
	defer abuseFeedProcess.stateLock.Unlock()
	if abuseFeedProcess.scan == nil {
		abuseFeedProcess.scan = newAbuseFeedScan(DefaultAbuseFeedsSettings(), defaultAbuseFeedDeps())
	}
	return abuseFeedProcess.scan
}

// One run: each feed abuse.yml lists, fetched, read and acted on, as the file header says.
func (self *abuseFeedScan) run(ctx context.Context) (result *AbuseFeedsResult) {
	result = &AbuseFeedsResult{}
	if !self.running.CompareAndSwap(false, true) {
		result.Idle = true
		return
	}
	defer self.running.Store(false)
	defer func() {
		abuseScanRunCounter.WithLabelValues(abuseScanFeed, result.outcome()).Inc()
	}()

	config := self.deps.config()
	feeds := config.Feeds()
	rootNames := config.RootNames()
	if len(feeds) == 0 || len(rootNames) == 0 {
		result.Idle = true
		return
	}
	hostSettings := model.DefaultAbuseSettings()
	listed := map[string]bool{}
	for _, feed := range feeds {
		listed[feed.Url] = true
		result.FeedCount += 1
		category, ok := model.AbuseScanFeedCategory(feed.Category)
		if !ok {
			glog.Warningf("[abuse]feed %q names no category of phishing, malware or command_and_control, so it is skipped\n", feed.Name)
			result.SkippedCount += 1
			continue
		}
		hosts, cut, err := self.fetch(ctx, feed, rootNames, hostSettings)
		if err != nil {
			glog.Warningf("[abuse]feed %q could not be read, so it acts on nothing this pass: %v\n", feed.Name, err)
			result.FailedCount += 1
			continue
		}
		if self.settings.FeedHitMaxCount < len(hosts) {
			glog.Warningf("[abuse]feed %q lists %d names under the roots, past the %d one pass acts on\n", feed.Name, len(hosts), self.settings.FeedHitMaxCount)
			abuseFeedCutCounter.WithLabelValues("hits").Inc()
			hosts = hosts[:self.settings.FeedHitMaxCount]
			cut = true
		}
		if cut {
			result.CutCount += 1
		}
		result.HostCount += len(hosts)

		listings, ok := self.feedListings[feed.Url]
		if !ok {
			listings = newAbuseScanListings(self.settings.ListingMaxCount)
			self.feedListings[feed.Url] = listings
		}
		now := self.deps.now()
		listings.startCycle(now)
		hits := make([]*abuseScanHit, 0, len(hosts))
		for _, host := range hosts {
			hits = append(hits, &abuseScanHit{host: host, category: category})
		}
		counts, err := reportAbuseScanHits(
			ctx,
			self.deps.intake,
			listings,
			abuseScanFeed,
			model.AbuseReporterClassFeed,
			model.AbuseSignalFeedHit,
			hits,
			rootNames,
			now,
		)
		result.addHits(counts)
		if err != nil {
			glog.Warningf("[abuse]a hit of feed %q could not reach intake, so the feed is read again next pass: %v\n", feed.Name, err)
			result.FailedCount += 1
			continue
		}
		if !cut {
			listings.endCycle()
		}
	}
	// a feed no longer listed takes its listings with it
	for feedUrl := range self.feedListings {
		if !listed[feedUrl] {
			delete(self.feedListings, feedUrl)
		}
	}
	result.Failed = 0 < result.FailedCount
	return
}

// One feed's names under or covering a root, each once in the order read, and whether a bound cut the read. An
// error is the fetch failing, and nothing of it is kept.
func (self *abuseFeedScan) fetch(
	ctx context.Context,
	feed server.AbuseFeed,
	rootNames []string,
	hostSettings *model.AbuseSettings,
) (hosts []string, cut bool, returnErr error) {
	requestCtx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, feed.Url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w: a url that cannot be fetched", errAbuseFeedSource)
	}
	response, err := self.httpClient.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("%w: unreachable", errAbuseFeedSource)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%w: status %d", errAbuseFeedSource, response.StatusCode)
	}

	limited := &io.LimitedReader{R: response.Body, N: self.settings.FeedMaxByteCount}
	reader := bufio.NewReaderSize(limited, self.settings.LineMaxByteCount)
	hosts = []string{}
	seenHosts := map[string]bool{}
	lineCount := 0
	for {
		line, readErr := reader.ReadSlice('\n')
		long := false
		for errors.Is(readErr, bufio.ErrBufferFull) {
			// a line past the bound names no host a join reads: read past it, keeping nothing
			long = true
			_, readErr = reader.ReadSlice('\n')
		}
		atEnd := errors.Is(readErr, io.EOF)
		if readErr != nil && !atEnd {
			return nil, false, fmt.Errorf("%w: a body cut short", errAbuseFeedSource)
		}
		if atEnd && limited.N <= 0 {
			// at the byte bound the last line may be the start of one: it counts only if the body ends here too
			if _, err := io.ReadFull(response.Body, make([]byte, 1)); err == nil {
				abuseFeedCutCounter.WithLabelValues("bytes").Inc()
				return hosts, true, nil
			}
		}
		if atEnd && len(line) == 0 {
			return hosts, false, nil
		}
		lineCount += 1
		if self.settings.FeedMaxLineCount < lineCount {
			abuseFeedCutCounter.WithLabelValues("lines").Inc()
			return hosts, true, nil
		}
		if !long {
			host := model.AbuseScanHost(string(line), hostSettings)
			_, covers := model.AbuseScanCoveredRoot(host, rootNames)
			if host != "" && !seenHosts[host] && (covers || model.AbuseScanUnderRoot(host, rootNames)) {
				seenHosts[host] = true
				hosts = append(hosts, host)
			}
		}
		if atEnd {
			return hosts, false, nil
		}
	}
}

// Schedules the feeds' next run one Interval from now, once. A binary that does not link MG5 schedules nothing.
func ScheduleAbuseFeeds(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := DefaultAbuseFeedsSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseFeeds,
		&AbuseFeedsArgs{},
		clientSession,
		task.RunOnce("abuse_feeds"),
		task.RunAt(server.NowUtc().Add(settings.Interval)),
		task.MaxTime(settings.TaskMaxTime),
	)
}

// One run of the process's feeds.
func AbuseFeeds(abuseFeeds *AbuseFeedsArgs, clientSession *session.ClientSession) (*AbuseFeedsResult, error) {
	if !server.SidecarMg5Linked() {
		return &AbuseFeedsResult{Idle: true}, nil
	}
	result := processAbuseFeedScan().run(clientSession.Ctx)
	if 0 < result.HostCount {
		glog.Infof("[abuse]feeds listed %d names under the roots: %d reported, %d acted on, %d suppressed, %d root incidents\n",
			result.HostCount, result.ReportedCount, result.ActedCount, result.SuppressedCount, result.RootIncidentCount)
	}
	return result, nil
}

// Schedules the next run.
func AbuseFeedsPost(
	abuseFeeds *AbuseFeedsArgs,
	abuseFeedsResult *AbuseFeedsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseFeeds(clientSession, tx)
	return nil
}
