//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The owner's "blogger" case (2026-11-21): "a client write updated to a file, and the tls edge to serve that file from
// a cache, and clear the cache whenever it gets a file changed notification ... multiple writers writing multiple
// articles, and the reader using the tls edge to load each article. Each writer should run a webserver behind the
// same tls edge using the socket load balancing."
//
// tlsedge routes by SNI without terminating TLS (X1, R-TRUST-4, R-CTX-21), so it never sees an article and cannot
// cache one: the cache belongs to each writer's webserver, behind the edge, which ends TLS in its writer's process and
// serves articles from its own cache, cleared at each change of the blog repo its sidecar's EvsClient raises from evs's
// hints (blogger_web_test.go names the API). The writers of one network all listen on the blog's one name, so each
// visit reaches one of them through sockets' fair scheduler (§9.7, R-SOCK-2).
//
//   - setup: two networks, made Pro by a paid balance so verified and able to hold a standing publish (§8.5); in each
//     a blog repo its owner created on evs, every writer's certificate row (the world's certificate path), socket
//     component, §16.6 approval gate and evs component, each article's first version, and each writer's subscription
//     of the article directory; then each network's document publishing tls/443/<host> raw to the public, its label
//     held at its zone's serial in the label map tlsedge routes by, every writer's webserver listening under the
//     document's standing publish, and a visit through tlsedge reaching each blog;
//   - the steady phase: every writer updates its articles while every reader loads each article through tlsedge,
//     one goroutine a burst, random pauses between, connect's and proxy's style;
//   - the writer chaos: the same, while one writer of the first network is stopped and started again on a cold cache;
//   - the last writes: every run's cache warmed with every article, then every writer writes each of its articles
//     once more, the notifications given SettleTimeout to settle, every run's cache read for what it still holds,
//     and a final sweep of every article by every reader through tlsedge;
//   - isolation: each blog's readers asking their blog for the other network's article are answered 404 by their own
//     writers, every answer of a blog is its own network's article from its own writer's key, and the second network's
//     writers read none of the first's repo, at their clients and at evs;
//   - the tlsedge chaos: the same reads and writes while tlsedge is restarted, so readers reconnect; it runs after the
//     rest, so a restart that keeps readers out is reported and the privacy and ledger checks still run;
//   - after each phase its changes are given SettleTimeout to settle, then it is checked: every body verifies
//     (network, id, writer, version, digest); per article and reader versions never go backwards; every answer from
//     evs is at least the newest version committed before the visit began, and every answer and every cache entry it
//     met at least the newest committed SettleTimeout before; every commit reached every other writer's running
//     webserver as a change within SettleTimeout; every visit reached some writer and, in the steady phase, the spread
//     across writers is within the fairness bound; visits fail only inside a chaos window; the restarted writer
//     rejoins on a cold cache, and every reader reads again within TlsedgeReconnectTimeout of tlsedge's restart;
//   - privacy: what tlsedge took and gave in every visit, and its connection records, go to the world's privacy
//     capture, whose scan must find no article id or text there or anywhere else it holds, and past each hello every
//     record either way is encrypted, the visitor's hello naming the server: the edge sees only SNI and ciphertext;
//   - the ledgers: the webservers stopped and tlsedge, sockets and evs restarted so each drains, every byte drawn from
//     each network's balances is a debit on a meter, exactly, no balance overdrawn, and tlsedge's meter holds at least
//     every byte the network's visitors were sent through it.
//
// The case runs on the World's stand-ins alone (the World's header). The two it carried past at first are gone with
// their fixes: §16.6's stock gate takes each writer's listen under the standing publish (listen-approval-effect), and a
// label's draw takes its zone's serial (dns-label-serial-order), which the publish checks.
//
// Its sub-case, TestE2eBloggerChangeAfterTheReadersOwnRead (blogger_changes_test.go), runs the cache clearing's
// change path alone: a commit must reach the other writer's change listener whether or not that writer's client read
// the path first.
//
// Each phase has its bound and dumps the world's and the blogs' progress when it stalls; the case has the conventions'
// 10 minutes. Run it through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
)

// What the blogger case runs with (QD-702): every count, bound and name it adds to the world's.
type bloggerSettings struct {
	// W, the first network's writers, each a sidecar running a webserver behind tlsedge on the blog's one name, and the
	// second network's, whose blog of its own the isolation checks read.
	WriterCount       int
	SecondWriterCount int
	// A, the articles each writer writes and keeps updating.
	ArticlesPerWriter int
	// R, the readers of the first network's blog and of the second's.
	ReaderCount       int
	SecondReaderCount int
	// Each phase's work: every writer's updates and every reader's reads.
	SteadyUpdatesPerWriter       int
	SteadyReadsPerReader         int
	WriterChaosUpdatesPerWriter  int
	WriterChaosReadsPerReader    int
	TlsedgeChaosUpdatesPerWriter int
	TlsedgeChaosReadsPerReader   int
	// The final sweep's reads of every article by every reader, once the last writes settled.
	FinalReadsPerArticle int
	// The readers' bursts: their pauses and seed, and each phase's bound. A writer's step adds a pause of its own of
	// up to WriterMaxPause, so the updates spread over the readers' reads.
	Bursts         *WorldBurstSettings
	WriterMaxPause time.Duration
	// When the writer chaos stops a writer and starts it again, and when the tlsedge chaos restarts tlsedge: each at
	// that percent of its phase's reads done.
	WriterStopPercent     int
	WriterStartPercent    int
	TlsedgeRestartPercent int
	// Which of the first network's writers chaos stops: not its owner's device, which filed its statements.
	ChaosWriterIndex int
	// How long past a writer's stop or start a visit may still fail: a ticket sockets signed before the writer's
	// unlisten is answered retry.
	ChaosGrace time.Duration
	// How soon after tlsedge's restart reads ready every reader must read through it again: visits may fail until
	// then, and past it each failure is the restart's finding.
	TlsedgeReconnectTimeout time.Duration
	// The notification's bound: within it of a write's commit being answered, every other writer's running webserver
	// must have heard a change at or past it, and no read anywhere may meet the version it replaced (§19.8's
	// head-to-hint target is 2 s).
	SettleTimeout time.Duration
	// The steady phase's fairness bound (R-SOCK-2): each writer answers at least FairShareMinPercent and at most
	// FairShareMaxPercent of an even share of its blog's visits.
	FairShareMinPercent int
	FairShareMaxPercent int
	// The blog's host under each network's zone, and the publish grant's life.
	Host      string
	GrantLife time.Duration
	// The repo directory the articles live in.
	ArticleDirectory string
	// One visit's bound (its dial, handshake, request and answer), the bound on a read's visits while they fail below
	// HTTP, and how soon a failed visit is tried again.
	VisitTimeout      time.Duration
	ReadRetryTimeout  time.Duration
	ReadRetryInterval time.Duration
	// The ALPN ids the readers offer and the writers' listeners serve and register.
	Alpns []string
	// The most bytes of an answer's body a reader reads, and of an article id a webserver takes.
	BodyMaxByteCount int
	IdMaxByteCount   int
	// Each blog's readers' first documentation address, the i-th reader at its last byte plus i, a /24 of its own per
	// blog under tlsedge's visitor caps (§13.9), and the source ports their visits' PROXY headers name.
	VisitorAddresses []netip.Addr
	VisitorPortStart int
	VisitorPortCount int
	// Each phase's bound beside the bursts'.
	PhaseTimeout time.Duration
	// What the recorder of sockets' registration calls keeps, and how far before a listen that never takes its calls
	// are shown: the unlisten a restart's stop sent is in them.
	SocketCalls        *bloggerSocketCallSettings
	SocketCallLookBack time.Duration
	// The TLS helpers' bounds: listens, the paid balance, certificate rows.
	Tls *WorldTlsSettings
	// The blocks restarted at the end so each drains its usage window before the ledgers.
	FinalDrainBlocks []string
}

func defaultBloggerSettings() *bloggerSettings {
	bursts := DefaultWorldBurstSettings()
	bursts.MaxPause = 100 * time.Millisecond
	return &bloggerSettings{
		WriterCount:                  4,
		SecondWriterCount:            2,
		ArticlesPerWriter:            3,
		ReaderCount:                  4,
		SecondReaderCount:            2,
		SteadyUpdatesPerWriter:       6,
		SteadyReadsPerReader:         48,
		WriterChaosUpdatesPerWriter:  8,
		WriterChaosReadsPerReader:    60,
		TlsedgeChaosUpdatesPerWriter: 4,
		TlsedgeChaosReadsPerReader:   30,
		FinalReadsPerArticle:         2,
		Bursts:                       bursts,
		WriterMaxPause:               time.Second,
		WriterStopPercent:            25,
		WriterStartPercent:           50,
		TlsedgeRestartPercent:        30,
		ChaosWriterIndex:             1,
		ChaosGrace:                   10 * time.Second,
		TlsedgeReconnectTimeout:      30 * time.Second,
		SettleTimeout:                10 * time.Second,
		FairShareMinPercent:          50,
		FairShareMaxPercent:          150,
		Host:                         "blog",
		GrantLife:                    12 * time.Hour,
		ArticleDirectory:             "articles",
		VisitTimeout:                 15 * time.Second,
		ReadRetryTimeout:             2 * time.Minute,
		ReadRetryInterval:            250 * time.Millisecond,
		Alpns:                        []string{"http/1.1"},
		BodyMaxByteCount:             64 * 1024,
		IdMaxByteCount:               128,
		VisitorAddresses:             []netip.Addr{netip.MustParseAddr("198.51.100.40"), netip.MustParseAddr("203.0.113.40")},
		VisitorPortStart:             41000,
		VisitorPortCount:             20000,
		PhaseTimeout:                 4 * time.Minute,
		SocketCalls:                  defaultBloggerSocketCallSettings(),
		SocketCallLookBack:           2 * time.Minute,
		Tls:                          DefaultWorldTlsSettings(),
		FinalDrainBlocks:             []string{WorldBlockTlsedge, WorldBlockSockets, WorldBlockEvs},
	}
}

// A network's document for its blog: its members publish and listen on tls/443/<host> under the public audience in
// raw mode until expires, a standing publish, which §8.5 holds to a Pro or Team document.
func (self *bloggerSettings) aclPolicy(expires time.Time) string {
	return worldAclPolicy(
		fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:%s", "sock:%s"], "allow": ["publish", "listen"], "audience": "public", "app_mode": "raw", "expires": %q}`, self.Host, self.socketName(), expires.UTC().Format(time.RFC3339)),
	)
}

// The name every writer of a blog listens on.
func (self *bloggerSettings) socketName() string {
	return fmt.Sprintf("tls/%d/%s", worldTlsPort, self.Host)
}

// The phases, as visits name them.
const (
	bloggerPhaseSetup        = "setup"
	bloggerPhaseSteady       = "steady"
	bloggerPhaseWriterChaos  = "writer chaos"
	bloggerPhaseTlsedgeChaos = "tlsedge chaos"
	bloggerPhaseLast         = "last writes"
	bloggerPhaseSweep        = "final sweep"
	bloggerPhaseProbe        = "isolation"
)

// The kinds of the world capture's canaries the case plants.
const (
	bloggerCanaryArticleId   = "blogger article id"
	bloggerCanaryArticleText = "blogger article text"
)

// The owner's blogger case: see the file header.
func TestE2eBlogger(t *testing.T) {
	settings := defaultBloggerSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := NewWorld(t, DefaultWorldSettings())
		defer world.Close()
		runBlogger(t, world, settings)
	})
}

// One network's blog: its repo, zone and server name, its writers and their articles, its readers and its ledger.
type bloggerBlog struct {
	world    *World
	settings *bloggerSettings
	network  *WorldNetwork
	// the world's sockets origin's registration calls, the evidence a listen that never takes is reported with
	socketCalls *bloggerSocketCalls
	repoId      sidecar.Id
	// main's ref id, from the first article's read back
	refId      sidecar.Id
	zone       *sidecar.DnsZone
	serverName string
	writers    []*bloggerWriter
	articles   []*bloggerArticle
	// by id, every article of the blog
	articleIdArticles map[string]*bloggerArticle
	readers           []*bloggerReader
	ledger            *bloggerLedger
}

// The repo path of an article id.
func (self *bloggerBlog) articlePath(articleId string) string {
	return self.settings.ArticleDirectory + "/" + articleId
}

// The writer whose TLS key spkiSha256 names, nil for none of the blog's.
func (self *bloggerBlog) writerOfSpki(spkiSha256 []byte) *bloggerWriter {
	for _, writer := range self.writers {
		if bytes.Equal(writer.spkiSha256, spkiSha256) {
			return writer
		}
	}
	return nil
}

// writer's listen on the blog's name, raw, serving the readers' ALPN ids, which §16.6's stock gate takes under the
// document's standing publish.
func (self *bloggerBlog) listen(ctx context.Context, writer *bloggerWriter) (*sidecar.SocketListener, error) {
	settings := self.settings
	name := settings.socketName()
	listenSettings := &sidecar.SocketListenSettings{AppMode: "raw", Alpn: slices.Clone(settings.Alpns)}
	startedAt := time.Now()
	listener, err := self.world.Listen(ctx, writer.device, name, listenSettings, settings.Tls)
	if err != nil {
		since := startedAt.Add(-self.settings.SocketCallLookBack)
		stacks := bloggerGoroutineStacks(settings.SocketCalls.StackMaxByteCount, "socketRegistrar", "socketsservice.(*SocketsService).registrations", "SocketComponent")
		return nil, fmt.Errorf("%s's listen on %s, begun at %s: %w; its registration calls since %s: %s; every client's calls since: %s; the registrar's and sockets' goroutines:\n%s", writer.device.Name, name, startedAt.Format("15:04:05.000"), err, since.Format("15:04:05.000"), self.socketCalls.describe(writer.device.Sidecar().Identity().ClientId.String(), since), self.socketCalls.summary(since), stacks)
	}
	return listener, nil
}

// One commit of an article: its version, the path's and ref's versions evs answered, and when the write began and
// was answered.
type bloggerCommit struct {
	article     *bloggerArticle
	version     int
	pathVersion uint64
	refVersion  uint64
	startedAt   time.Time
	answeredAt  time.Time
}

// A blog's ledger: every commit and every visit. Safe for concurrent use.
type bloggerLedger struct {
	stateLock sync.Mutex
	// each article's commits in version order, by id
	articleIdCommits map[string][]*bloggerCommit
	commits          []*bloggerCommit
	visits           []*bloggerVisit
}

func newBloggerLedger() *bloggerLedger {
	return &bloggerLedger{articleIdCommits: map[string][]*bloggerCommit{}}
}

func (self *bloggerLedger) addCommit(commit *bloggerCommit) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.articleIdCommits[commit.article.id] = append(self.articleIdCommits[commit.article.id], commit)
	self.commits = append(self.commits, commit)
}

func (self *bloggerLedger) addVisit(visit *bloggerVisit) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.visits = append(self.visits, visit)
}

// Every commit, in the order they were answered.
func (self *bloggerLedger) commitList() []*bloggerCommit {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.commits)
}

// Every visit, in the order they ended.
func (self *bloggerLedger) visitList() []*bloggerVisit {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.visits)
}

// The newest version of articleId whose commit was answered at or before at, 0 for none.
func (self *bloggerLedger) newestAnsweredBy(articleId string, at time.Time) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	newest := 0
	for _, commit := range self.articleIdCommits[articleId] {
		if !commit.answeredAt.After(at) {
			newest = max(newest, commit.version)
		}
	}
	return newest
}

// The newest version of articleId committed.
func (self *bloggerLedger) newestVersion(articleId string) int {
	return self.newestAnsweredBy(articleId, time.Now())
}

// A chaos event: what was done, when it began and ended, and how long past its end a visit may still fail.
type bloggerChaosEvent struct {
	name      string
	grace     time.Duration
	startedAt time.Time
	endedAt   time.Time
}

// One step of a chaos phase: what it does, once that percent of the phase's reads is done.
type bloggerChaosStep struct {
	name    string
	percent int
	// how long past the step's end a visit may still fail, and what it does
	grace time.Duration
	run   func(ctx context.Context) error
}

// A chaos phase's steps, each at a share of the phase's reads, so each lands mid-phase whatever the phase's pace, and
// the events they made. Safe for concurrent use.
type bloggerChaos struct {
	steps []*bloggerChaosStep
	// reads the phase's readers completed, and how many it holds in all
	completedReads *sidecar.MonitorValue[int]
	readCount      int

	stateLock sync.Mutex
	events    []*bloggerChaosEvent
}

// The chaos events' names.
const (
	bloggerChaosWriterStop     = "writer stop"
	bloggerChaosWriterStart    = "writer start"
	bloggerChaosTlsedgeRestart = "tlsedge restart"
)

// A chaos phase of steps over blogs' readers' readsPerReader reads each.
func newBloggerChaos(blogs []*bloggerBlog, readsPerReader int, steps ...*bloggerChaosStep) *bloggerChaos {
	readCount := 0
	for _, blog := range blogs {
		readCount += len(blog.readers) * readsPerReader
	}
	return &bloggerChaos{steps: steps, completedReads: sidecar.NewMonitorValue(0), readCount: readCount}
}

// Every event so far.
func (self *bloggerChaos) eventList() []*bloggerChaosEvent {
	if self == nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// The named event, nil before it ran.
func (self *bloggerChaos) event(name string) *bloggerChaosEvent {
	for _, event := range self.eventList() {
		if event.name == name {
			return event
		}
	}
	return nil
}

// Waits until the phase's readers completed count reads.
func (self *bloggerChaos) awaitReads(ctx context.Context, count int) error {
	for {
		completed, changed := self.completedReads.Get()
		if count <= completed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d of the %d reads chaos waits for: %w", completed, count, ctx.Err())
		case <-changed:
		}
	}
}

// The chaos burst: each step in turn, once its share of the phase's reads is done.
func (self *bloggerChaos) burst() *WorldBurst {
	return &WorldBurst{
		Name:      "the chaos",
		StepCount: len(self.steps),
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			step := self.steps[index]
			if err := self.awaitReads(ctx, self.readCount*step.percent/100); err != nil {
				return err
			}
			event := &bloggerChaosEvent{name: step.name, grace: step.grace, startedAt: time.Now()}
			err := step.run(ctx)
			event.endedAt = time.Now()
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.events = append(self.events, event)
			}()
			if err != nil {
				return fmt.Errorf("the %s: %w", event.name, err)
			}
			return nil
		},
	}
}

// Whether a visit's span meets a chaos event's, the event widened past its end by its grace.
func (self *bloggerChaos) covers(visit *bloggerVisit) bool {
	for _, event := range self.eventList() {
		if !visit.endedAt.Before(event.startedAt) && !visit.startedAt.After(event.endedAt.Add(event.grace)) {
			return true
		}
	}
	return false
}

// The case on world: see the file header.
func runBlogger(t testing.TB, world *World, settings *bloggerSettings) {
	startedAt := time.Now()
	socketCalls, err := installBloggerSocketCalls(world, settings.SocketCalls)
	if err != nil {
		t.Fatalf("the recorder of sockets' registration calls: %v", err)
	}

	// setup: the networks, their blogs, the grants and every writer's first run
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	networks := []*WorldNetwork{}
	for index, count := range []int{settings.WriterCount, settings.SecondWriterCount} {
		network, err := world.AddNetwork(ctx, worldNetworkName(index), count, nil)
		if err != nil {
			cancel()
			world.FailProgress(t, "the enrollment", err, nil)
		}
		networks = append(networks, network)
	}
	blogs := []*bloggerBlog{}
	for index, readerCount := range []int{settings.ReaderCount, settings.SecondReaderCount} {
		blog, err := newBloggerBlog(ctx, world, settings, networks[index], index, readerCount, socketCalls)
		if err != nil {
			cancel()
			world.FailProgress(t, networks[index].Name+"'s blog", err, nil)
		}
		blogs = append(blogs, blog)
	}
	for _, blog := range blogs {
		if err := blog.publish(ctx); err != nil {
			cancel()
			world.FailProgress(t, blog.network.Name+"'s publish", err, nil)
		}
	}
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			if _, err := writer.start(ctx); err != nil {
				cancel()
				world.FailProgress(t, writer.device.Name+"'s webserver", err, nil)
			}
		}
	}
	defer func() {
		for _, blog := range blogs {
			for _, writer := range blog.writers {
				writer.stop()
				if writer.subscription != nil {
					writer.subscription.Close()
				}
			}
		}
	}()
	for _, blog := range blogs {
		if err := blog.awaitRoute(ctx); err != nil {
			cancel()
			bloggerFailProgress(t, world, blogs, "the route through tlsedge", err, nil)
		}
	}
	cancel()
	t.Logf("the blogs stood %s after the case began: %s at %s, %s at %s", time.Since(startedAt).Round(time.Millisecond), blogs[0].network.Name, blogs[0].serverName, blogs[1].network.Name, blogs[1].serverName)

	for _, blog := range blogs {
		checkBloggerVisits(t, blog, nil, bloggerPhaseSetup)
	}

	// the steady phase, its notifications given their bound to settle, then its checks
	steadyStartedAt := time.Now()
	runBloggerPhase(t, world, blogs, bloggerPhaseSteady, settings.SteadyUpdatesPerWriter, settings.SteadyReadsPerReader, nil, true)
	bloggerSettle(t, world, blogs, steadyStartedAt)
	for _, blog := range blogs {
		checkBloggerVisits(t, blog, nil, bloggerPhaseSteady)
		checkBloggerFairness(t, blog)
		checkBloggerNotifications(t, blog, steadyStartedAt)
	}

	// the writer chaos: one writer of the first network stopped and started again on a cold cache, the same checks,
	// and its rejoining
	writer := blogs[0].writers[settings.ChaosWriterIndex]
	writerChaos := newBloggerChaos(blogs, settings.WriterChaosReadsPerReader,
		&bloggerChaosStep{name: bloggerChaosWriterStop, percent: settings.WriterStopPercent, grace: settings.ChaosGrace, run: func(ctx context.Context) error {
			writer.stop()
			return nil
		}},
		&bloggerChaosStep{name: bloggerChaosWriterStart, percent: settings.WriterStartPercent, grace: settings.ChaosGrace, run: func(ctx context.Context) error {
			if _, err := writer.start(ctx); err != nil {
				// its writes end now, rather than wait out the phase for a run that never comes
				writer.abandon()
				return err
			}
			return nil
		}},
	)
	writerChaosStartedAt := time.Now()
	runBloggerPhase(t, world, blogs, bloggerPhaseWriterChaos, settings.WriterChaosUpdatesPerWriter, settings.WriterChaosReadsPerReader, writerChaos, true)
	bloggerLogChaos(t, writerChaos)
	bloggerSettle(t, world, blogs, writerChaosStartedAt)
	for _, blog := range blogs {
		checkBloggerVisits(t, blog, writerChaos, bloggerPhaseWriterChaos)
		checkBloggerNotifications(t, blog, writerChaosStartedAt)
	}
	checkBloggerWriterRestart(t, blogs[0], writer, writerChaos)

	// the last writes, their settling, every cache and the final sweep, then their checks
	lastStartedAt := time.Now()
	runBloggerLastWrites(t, world, blogs)
	for _, blog := range blogs {
		checkBloggerVisits(t, blog, nil, bloggerPhaseSweep)
		checkBloggerNotifications(t, blog, lastStartedAt)
	}

	// isolation
	ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
	checkBloggerIsolation(ctx, t, world, blogs)
	cancel()

	// the tlsedge chaos: tlsedge restarted mid-run, so readers reconnect. A read whose visits fail past
	// ReadRetryTimeout is the restart's finding, reported, and the case goes on to its privacy and ledgers
	tlsedgeChaos := newBloggerChaos(blogs, settings.TlsedgeChaosReadsPerReader,
		&bloggerChaosStep{name: bloggerChaosTlsedgeRestart, percent: settings.TlsedgeRestartPercent, grace: settings.TlsedgeReconnectTimeout, run: func(ctx context.Context) error {
			return world.Restart(WorldBlockTlsedge)
		}},
	)
	tlsedgeChaosStartedAt := time.Now()
	runBloggerPhase(t, world, blogs, bloggerPhaseTlsedgeChaos, settings.TlsedgeChaosUpdatesPerWriter, settings.TlsedgeChaosReadsPerReader, tlsedgeChaos, false)
	bloggerLogChaos(t, tlsedgeChaos)
	bloggerSettle(t, world, blogs, tlsedgeChaosStartedAt)
	for _, blog := range blogs {
		checkBloggerVisits(t, blog, tlsedgeChaos, bloggerPhaseTlsedgeChaos)
		checkBloggerNotifications(t, blog, tlsedgeChaosStartedAt)
		checkBloggerReconnect(t, blog, tlsedgeChaos)
	}

	// the webservers stopped, then each block that metered the case drained by a restart, so tlsedge's connection
	// records and every usage window are written; then privacy and the ledgers
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			writer.stop()
		}
	}
	for _, name := range settings.FinalDrainBlocks {
		if err := world.Restart(name); err != nil {
			world.FailProgress(t, name+"'s restart for its drain", err, nil)
		}
	}
	checkBloggerPrivacy(t, world, blogs, startedAt)
	ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
	ledgers, err := world.ReadLedgers(ctx, time.Time{})
	cancel()
	if err != nil {
		t.Fatalf("the ledgers at the end: %v", err)
	}
	for _, blog := range blogs {
		checkBloggerLedger(t, blog, ledgers)
	}
}

// network's blog up to its grants: verified and Pro, its repo, zone and server name, every writer's certificate row,
// leaf, socket component, approval gate, evs component and articles, each article's first version, main's ref id,
// every writer's subscription of the article directory, and its readers.
func newBloggerBlog(ctx context.Context, world *World, settings *bloggerSettings, network *WorldNetwork, index int, readerCount int, socketCalls *bloggerSocketCalls) (*bloggerBlog, error) {
	blog := &bloggerBlog{
		world:             world,
		settings:          settings,
		network:           network,
		socketCalls:       socketCalls,
		articleIdArticles: map[string]*bloggerArticle{},
		ledger:            newBloggerLedger(),
	}
	if err := world.VerifyNetwork(ctx, network, true, settings.Tls); err != nil {
		return nil, fmt.Errorf("%s's Pro balance: %w", network.Name, err)
	}
	repoId, err := network.CreateSharedRepo(ctx, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER)
	if err != nil {
		return nil, fmt.Errorf("%s's blog repo: %w", network.Name, err)
	}
	blog.repoId = repoId
	dnsClient, err := network.Owner().Sidecar().Dns()
	if err != nil {
		return nil, err
	}
	zone, err := dnsClient.Zone(ctx)
	if err != nil || zone.Apex() == "" {
		return nil, fmt.Errorf("%s's zone: %+v, %w", network.Name, zone, err)
	}
	blog.zone = zone
	blog.serverName = worldTlsServerName(settings.Host, zone.Apex())
	capture := world.T3().Net().Capture()
	for _, device := range network.Clients {
		writer := &bloggerWriter{
			blog:              blog,
			device:            device,
			state:             sidecar.NewMonitorValue(bloggerWriterStopped),
			articleIdVersions: map[string]int{},
		}
		if err := world.IssueTlsCertificateRow(ctx, device, zone, settings.Tls); err != nil {
			return nil, fmt.Errorf("%s's certificate row: %w", device.Name, err)
		}
		if writer.certificate, err = device.Sidecar().TlsDeviceCertificate([]string{blog.serverName}); err != nil {
			return nil, fmt.Errorf("%s's leaf: %w", device.Name, err)
		}
		if writer.spkiSha256, err = worldTlsKeySpkiSha256(device); err != nil {
			return nil, err
		}
		// the component's registrar runs until the context it is made with ends (Sidecar.NewSocketComponent), so it
		// is made on the world's, never on a step's, or every heartbeat, unlisten and listen after the step is lost
		if err := world.StartSockets(world.T3().Context(), device); err != nil {
			return nil, err
		}
		if _, err := world.InstallApprovalGate(ctx, device); err != nil {
			return nil, err
		}
		if writer.client, err = device.EvsClient(); err != nil {
			return nil, fmt.Errorf("%s's evs client: %w", device.Name, err)
		}
		// the evs component, through which evs's hints reach the client (cli's daemon_clients.go composes it so)
		if device.Sidecar().EvsClient() == nil {
			if _, err := device.Sidecar().AddComponent(sidecar.NewEvsComponent(writer.client)); err != nil {
				return nil, fmt.Errorf("%s's evs component: %w", device.Name, err)
			}
		}
		for articleIndex := range settings.ArticlesPerWriter {
			id := fmt.Sprintf("%s-%d-%s", device.Name, articleIndex, capture.CanaryText(bloggerCanaryArticleId))
			if !bloggerArticleIdValid(id, settings.IdMaxByteCount) {
				return nil, fmt.Errorf("an article id %q a webserver refuses", id)
			}
			article := &bloggerArticle{blog: blog, writer: writer, id: id, path: blog.articlePath(id)}
			writer.articles = append(writer.articles, article)
			blog.articles = append(blog.articles, article)
			blog.articleIdArticles[id] = article
		}
		blog.writers = append(blog.writers, writer)
	}
	for _, writer := range blog.writers {
		for _, article := range writer.articles {
			if _, err := writer.write(ctx, article, capture.CanaryText(bloggerCanaryArticleText)); err != nil {
				return nil, err
			}
		}
	}
	var content bytes.Buffer
	info, err := blog.writers[0].client.Repo(repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: blog.articles[0].path}, &content)
	if err != nil {
		return nil, fmt.Errorf("%s's read of the first article: %w", blog.writers[0].device.Name, err)
	}
	blog.refId = info.RefId
	for _, writer := range blog.writers {
		subscription, err := writer.client.Repo(repoId).Subscribe(ctx, &sidecar.EvsSubscribeArgs{Paths: []string{settings.ArticleDirectory}})
		if err != nil {
			return nil, fmt.Errorf("%s's subscription of %s: %w", writer.device.Name, settings.ArticleDirectory, err)
		}
		writer.subscription = subscription
	}
	for readerIndex := range readerCount {
		address := settings.VisitorAddresses[index].As4()
		address[3] += byte(readerIndex)
		blog.readers = append(blog.readers, &bloggerReader{
			blog:                  blog,
			index:                 readerIndex,
			name:                  fmt.Sprintf("%s-reader-%d", network.Name, readerIndex),
			address:               netip.AddrFrom4(address),
			articleIdSeenVersions: map[string]int{},
		})
	}
	return blog, nil
}

// Saves the network's document publishing the blog's name, and checks that its label holds its zone's serial in the
// label map, which tlsedge follows since its last serial (DnsLabelMapSince): a label at 0 is never routed.
func (self *bloggerBlog) publish(ctx context.Context) error {
	network := self.network
	if _, err := self.world.SaveAclPolicy(ctx, network, network.Owner(), self.settings.aclPolicy(time.Now().Add(self.settings.GrantLife))); err != nil {
		return err
	}
	serial, found, err := worldDnsLabelChangeSerial(ctx, network.NetworkId, self.zone.Root)
	if err != nil || !found {
		return fmt.Errorf("%s's label in the map: found %t, %v", network.Name, found, err)
	}
	if serial == 0 {
		return fmt.Errorf("%s's label %s holds change serial 0 in %s's label map, so tlsedge never routes %s", network.Name, self.zone.Label, self.zone.Root, self.serverName)
	}
	return nil
}

// Visits the blog's first article until a writer answers it through tlsedge: the registrations reach sockets at their
// verdicts and tlsedge reaches them at its Find, with no hook a test reads, so a visit is tried every
// ReadRetryInterval within the TLS helpers' Timeout.
func (self *bloggerBlog) awaitRoute(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.Tls.Timeout)
	defer cancel()
	reader := self.readers[0]
	for tries := 1; ; tries += 1 {
		visit := reader.visit(ctx, bloggerPhaseSetup, self.articles[0].id)
		self.ledger.addVisit(visit)
		if visit.problem != nil {
			return errors.New(visit.describe())
		}
		if visit.err == nil && visit.status == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s through tlsedge, %d visits, the last %s: %w; %s", self.serverName, tries, visit.describe(), ctx.Err(), worldTlsServerState(context.Background(), self.network.NetworkId, self.zone.Root))
		case <-time.After(self.settings.ReadRetryInterval):
		}
	}
}

// A writer's burst of updateCount writes in phase, each of its articles in turn, each after the writer's own pause
// and once a run of its webserver serves.
func (self *bloggerWriter) burst(phase string, updateCount int) *WorldBurst {
	settings := self.blog.settings
	capture := self.blog.world.T3().Net().Capture()
	return &WorldBurst{
		Name:      fmt.Sprintf("%s's %s writes", self.device.Name, phase),
		StepCount: updateCount,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(random.Int64N(int64(settings.WriterMaxPause) + 1))):
			}
			if err := self.awaitUp(ctx); err != nil {
				return err
			}
			_, err := self.write(ctx, self.articles[index%len(self.articles)], capture.CanaryText(bloggerCanaryArticleText))
			return err
		},
	}
}

// A reader's burst of readCount reads in phase of articles, each of its blog's in turn from an offset of the reader's
// own, each read counted in completed once answered.
func (self *bloggerReader) burst(phase string, articles []*bloggerArticle, readCount int, completed *sidecar.MonitorValue[int]) *WorldBurst {
	return &WorldBurst{
		Name:      fmt.Sprintf("%s's %s reads", self.name, phase),
		StepCount: readCount,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			if err := self.read(ctx, phase, articles[(self.index+index)%len(articles)]); err != nil {
				return err
			}
			if completed != nil {
				completed.Update(func(count int) int {
					return count + 1
				})
			}
			return nil
		},
	}
}

// Reads article until a visit is answered, trying again past failures below HTTP every ReadRetryInterval within
// ReadRetryTimeout. A problem, a 404 for an article the blog holds, or the bound is the read's error.
func (self *bloggerReader) read(ctx context.Context, phase string, article *bloggerArticle) error {
	settings := self.blog.settings
	deadline := time.Now().Add(settings.ReadRetryTimeout)
	for {
		visit := self.visit(ctx, phase, article.id)
		self.blog.ledger.addVisit(visit)
		switch {
		case visit.problem != nil:
			return errors.New(visit.describe())
		case visit.err == nil && visit.status == http.StatusOK:
			return nil
		case visit.err == nil:
			return fmt.Errorf("%s: an article the blog holds", visit.describe())
		case !time.Now().Before(deadline):
			return fmt.Errorf("%s, every visit failing for %s", visit.describe(), settings.ReadRetryTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", visit.describe(), ctx.Err())
		case <-time.After(settings.ReadRetryInterval):
		}
	}
}

// One phase: every writer's updateCount writes and every reader's readCount reads at once, with chaos's burst beside
// them when chaos is given; a stall dumps the world's and the blogs' progress. A burst's errors end the case when
// stopOnFailure, and otherwise are failed and the case goes on.
func runBloggerPhase(t testing.TB, world *World, blogs []*bloggerBlog, phase string, updateCount int, readCount int, chaos *bloggerChaos, stopOnFailure bool) {
	settings := blogs[0].settings
	bursts := []*WorldBurst{}
	var completed *sidecar.MonitorValue[int]
	if chaos != nil {
		completed = chaos.completedReads
	}
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			bursts = append(bursts, writer.burst(phase, updateCount))
		}
		for _, reader := range blog.readers {
			bursts = append(bursts, reader.burst(phase, blog.articles, readCount, completed))
		}
	}
	if chaos != nil {
		bursts = append(bursts, chaos.burst())
	}
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	startedAt := time.Now()
	if err := RunBursts(ctx, t, "the "+phase+" phase", bursts, settings.Bursts); err != nil {
		bloggerFailProgress(t, world, blogs, "the "+phase+" phase", err, bursts)
	}
	if stopOnFailure {
		bloggerFailBursts(t, blogs, bursts)
	} else {
		FailBurstErrors(t, bursts)
	}
	t.Logf("the %s phase took %s", phase, time.Since(startedAt).Round(time.Millisecond))
}

// Logs how long each of chaos's events took.
func bloggerLogChaos(t testing.TB, chaos *bloggerChaos) {
	t.Helper()
	for _, event := range chaos.eventList() {
		t.Logf("chaos: the %s took %s", event.name, event.endedAt.Sub(event.startedAt).Round(time.Millisecond))
	}
}

// Fails t on every burst's errors and ends the case past the blogs' progress when any burst failed: a phase whose
// reads or writes failed leaves the next nothing to stand on. Findings reported earlier leave it running.
func bloggerFailBursts(t testing.TB, blogs []*bloggerBlog, bursts []*WorldBurst) {
	t.Helper()
	FailBurstErrors(t, bursts)
	for _, burst := range bursts {
		if 0 < len(burst.Errors()) {
			bloggerLogProgress(t, blogs)
			t.FailNow()
		}
	}
}

// The last writes and what follows them: every running webserver's cache warmed with every article of its blog,
// every writer writing each of its articles once more at once, then each running webserver given until SettleTimeout
// past the last commit's answer to hear a change at or past every other writer's last commit; then every cache must
// hold each article at its newest version or not at all, and a final sweep of every article by every reader through
// tlsedge must be answered the newest version of each.
func runBloggerLastWrites(t testing.TB, world *World, blogs []*bloggerBlog) {
	settings := blogs[0].settings
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			server := writer.currentServer()
			if server == nil {
				t.Fatalf("%s runs no webserver before the last writes", writer.device.Name)
			}
			for _, article := range blog.articles {
				if err := server.fill(ctx, article.id); err != nil {
					t.Fatalf("%s's warming of %s: %v", writer.device.Name, article.id, err)
				}
			}
		}
	}
	lastStartedAt := time.Now()
	bursts := []*WorldBurst{}
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			bursts = append(bursts, writer.burst(bloggerPhaseLast, len(writer.articles)))
		}
	}
	if err := RunBursts(ctx, t, "the last writes", bursts, settings.Bursts); err != nil {
		bloggerFailProgress(t, world, blogs, "the last writes", err, bursts)
	}
	bloggerFailBursts(t, blogs, bursts)
	bloggerSettle(t, world, blogs, lastStartedAt)
	// every cache holds each article at its newest version or not at all
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			server := writer.currentServer()
			for articleId, entry := range server.entrySnapshot() {
				if newest := blog.ledger.newestVersion(articleId); entry.version < newest {
					t.Errorf("%s's cache still holds %s at version %d past the last writes' settling, its newest version %d: %s", writer.device.Name, articleId, entry.version, newest, blog.notificationEvidence(writer, server, blog.articleCommit(articleId, newest)))
				}
			}
		}
	}
	// the final sweep: every article by every reader, answered its newest version
	sweepStartedAt := time.Now()
	bursts = []*WorldBurst{}
	for _, blog := range blogs {
		for _, reader := range blog.readers {
			bursts = append(bursts, reader.burst(bloggerPhaseSweep, blog.articles, len(blog.articles)*settings.FinalReadsPerArticle, nil))
		}
	}
	if err := RunBursts(ctx, t, "the final sweep", bursts, settings.Bursts); err != nil {
		bloggerFailProgress(t, world, blogs, "the final sweep", err, bursts)
	}
	bloggerFailBursts(t, blogs, bursts)
	for _, blog := range blogs {
		stale := []string{}
		for _, visit := range blog.ledger.visitList() {
			if visit.phase != bloggerPhaseSweep || visit.fields == nil || visit.startedAt.Before(sweepStartedAt) {
				continue
			}
			newest := blog.ledger.newestVersion(visit.articleId)
			if visit.fields.version != newest || (0 <= visit.cachedVersion && visit.cachedVersion != newest) {
				stale = append(stale, fmt.Sprintf("%s (newest %d)", visit.describe(), newest))
			}
		}
		if 0 < len(stale) {
			t.Errorf("%s's final sweep met %d answers or cache entries below their article's newest version: %s", blog.network.Name, len(stale), strings.Join(stale[:min(len(stale), 5)], "; "))
		}
	}
}

// Waits until every running webserver heard a change at or past the newest ref version every other writer's commit
// answered since since reached, each up to SettleTimeout past the last such answer: a wait on the changes themselves,
// bounded, which checkBloggerNotifications then holds each commit to. Logs what never came.
func bloggerSettle(t testing.TB, world *World, blogs []*bloggerBlog, since time.Time) {
	t.Helper()
	settings := blogs[0].settings
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	lastAnsweredAt := time.Time{}
	for _, blog := range blogs {
		for _, commit := range blog.ledger.commitList() {
			if !commit.answeredAt.Before(since) && lastAnsweredAt.Before(commit.answeredAt) {
				lastAnsweredAt = commit.answeredAt
			}
		}
	}
	if lastAnsweredAt.IsZero() {
		return
	}
	startedAt := time.Now()
	deadline := time.After(time.Until(lastAnsweredAt.Add(settings.SettleTimeout)))
	unheard := []string{}
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			server := writer.currentServer()
			if server == nil {
				continue
			}
			needed := uint64(0)
			for _, commit := range blog.ledger.commitList() {
				if !commit.answeredAt.Before(since) && commit.article.writer != writer {
					needed = max(needed, commit.refVersion)
				}
			}
			if heard := bloggerAwaitChange(ctx, server, needed, deadline); heard < needed {
				unheard = append(unheard, fmt.Sprintf("%s run %d heard up to version %d of %d", writer.device.Name, server.run, heard, needed))
			}
		}
	}
	if 0 < len(unheard) {
		t.Logf("the changes since %s did not all come within %s of the last commit's answer: %s", since.Format("15:04:05.000"), settings.SettleTimeout, strings.Join(unheard, ", "))
		return
	}
	t.Logf("the changes since %s settled in %s", since.Format("15:04:05.000"), time.Since(startedAt).Round(time.Millisecond))
}

// Waits until server heard a change at or past version, or deadline fires: the newest version it heard either way.
func bloggerAwaitChange(ctx context.Context, server *bloggerServer, version uint64, deadline <-chan time.Time) uint64 {
	heard := func() uint64 {
		newest := uint64(0)
		for _, clear := range server.clearList() {
			if clear.cause == bloggerClearChange {
				newest = max(newest, clear.version)
			}
		}
		return newest
	}
	for {
		_, changed := server.changeCount.Get()
		if newest := heard(); version <= newest {
			return newest
		}
		select {
		case <-ctx.Done():
			return heard()
		case <-deadline:
			return heard()
		case <-changed:
		}
	}
}

// The commit of articleId at version, nil for none.
func (self *bloggerBlog) articleCommit(articleId string, version int) *bloggerCommit {
	for _, commit := range self.ledger.commitList() {
		if commit.article.id == articleId && commit.version == version {
			return commit
		}
	}
	return nil
}

// What a run of writer heard about commit, as evidence for a missed notification: the newest change it heard by the
// commit's settling, its first change at or past the commit's version, and its writer's own pin rises across the
// commit's ref version.
func (self *bloggerBlog) notificationEvidence(writer *bloggerWriter, server *bloggerServer, commit *bloggerCommit) string {
	if commit == nil {
		return "no commit of it is kept"
	}
	settle := commit.answeredAt.Add(self.settings.SettleTimeout)
	lines := []string{fmt.Sprintf("%s's write of version %d committed at ref version %d, answered at %s", commit.article.writer.device.Name, commit.version, commit.refVersion, commit.answeredAt.Format("15:04:05.000"))}
	heardBy, first := uint64(0), (*bloggerClear)(nil)
	changeCount := 0
	for _, clear := range server.clearList() {
		if clear.cause != bloggerClearChange {
			continue
		}
		changeCount += 1
		if !clear.at.After(settle) {
			heardBy = max(heardBy, clear.version)
		}
		if first == nil && commit.refVersion <= clear.version {
			first = clear
		}
	}
	lines = append(lines, fmt.Sprintf("run %d heard %d changes, the newest by the commit's settling at version %d", server.run, changeCount, heardBy))
	if first != nil {
		lines = append(lines, fmt.Sprintf("its first at or past %d came at %s (version %d, gap %t)", commit.refVersion, first.at.Format("15:04:05.000"), first.version, first.gap))
	} else {
		lines = append(lines, fmt.Sprintf("none at or past %d ever came", commit.refVersion))
	}
	for _, rise := range writer.pinRiseList() {
		if rise.from < commit.refVersion && commit.refVersion <= rise.to {
			lines = append(lines, fmt.Sprintf("%s's own %s of %s raised its pin from %d to %d at %s, across the commit, and the subscriber's read after that brings no statement below the pin", writer.device.Name, rise.cause, rise.articleId, rise.from, rise.to, rise.at.Format("15:04:05.000")))
		}
	}
	return strings.Join(lines, "; ")
}

// Holds every visit of blog's to the commits its ledger knows of (the file header's checks): visits fail only in a
// chaos window, every answer from evs is at least the newest version committed before its visit began, every answer
// and every cache entry it met at least the newest committed SettleTimeout before, and the edge saw only the hello
// in the clear, the visitor's naming the server.
func checkBloggerVisits(t testing.TB, blog *bloggerBlog, chaos *bloggerChaos, phase string) {
	t.Helper()
	settings := blog.settings
	failed, outside, stale, unfresh, plaintext := 0, []string{}, []string{}, []string{}, []string{}
	visitCount := 0
	sourceCounts := map[string]int{}
	for _, visit := range blog.ledger.visitList() {
		if visit.phase != phase {
			continue
		}
		visitCount += 1
		if visit.plaintext != "" || (visit.spliced() && !visit.helloNamesServer) {
			plaintext = append(plaintext, fmt.Sprintf("%s: %q, hello naming the server %t", visit.describe(), visit.plaintext, visit.helloNamesServer))
		}
		if visit.err != nil {
			failed += 1
			if visit.phase != bloggerPhaseSetup && !chaos.covers(visit) {
				outside = append(outside, visit.describe())
			}
			continue
		}
		if visit.fields == nil {
			continue
		}
		sourceCounts[visit.source] += 1
		settled := blog.ledger.newestAnsweredBy(visit.articleId, visit.startedAt.Add(-settings.SettleTimeout))
		if visit.fields.version < settled || (0 <= visit.cachedVersion && visit.cachedVersion < settled) {
			stale = append(stale, fmt.Sprintf("%s, the newest committed %s before it %d", visit.describe(), settings.SettleTimeout, settled))
		}
		if visit.source == bloggerSourceEvs {
			if committed := blog.ledger.newestAnsweredBy(visit.articleId, visit.startedAt); visit.fields.version < committed {
				unfresh = append(unfresh, fmt.Sprintf("%s, the newest committed before it %d", visit.describe(), committed))
			}
		}
	}
	t.Logf("%s's %s visits: %d, %d failed below HTTP, answers by source %v", blog.network.Name, phase, visitCount, failed, sourceCounts)
	if 0 < len(outside) {
		t.Errorf("%s's %s visits failed %d times outside every chaos window: %s", blog.network.Name, phase, len(outside), strings.Join(outside[:min(len(outside), 5)], "; "))
	}
	if 0 < len(stale) {
		t.Errorf("%s's %s visits met %d answers or cache entries older than a version committed %s before the visit began, so a writer's cache was not cleared within the bound: %s", blog.network.Name, phase, len(stale), settings.SettleTimeout, strings.Join(stale[:min(len(stale), 5)], "; "))
	}
	if 0 < len(unfresh) {
		t.Errorf("%s's %s visits met %d answers a fresh evs read gave below a version committed before the visit began: %s", blog.network.Name, phase, len(unfresh), strings.Join(unfresh[:min(len(unfresh), 5)], "; "))
	}
	if 0 < len(plaintext) {
		t.Errorf("%s's %s visits showed the edge %d connections with more than the hello in the clear, or a hello not naming the server: %s", blog.network.Name, phase, len(plaintext), strings.Join(plaintext[:min(len(plaintext), 5)], "; "))
	}
}

// Holds the steady phase's spread of blog's visits across its writers to the fairness bound (R-SOCK-2), every visit
// having reached one of them.
func checkBloggerFairness(t testing.TB, blog *bloggerBlog) {
	t.Helper()
	settings := blog.settings
	writerCounts := map[*bloggerWriter]int{}
	total := 0
	for _, visit := range blog.ledger.visitList() {
		if visit.phase != bloggerPhaseSteady || visit.err != nil || visit.fields == nil {
			continue
		}
		writerCounts[visit.writer] += 1
		total += 1
	}
	spread := []string{}
	for _, writer := range blog.writers {
		spread = append(spread, fmt.Sprintf("%s %d", writer.device.Name, writerCounts[writer]))
	}
	t.Logf("%s's steady visits by writer: %s of %d", blog.network.Name, strings.Join(spread, ", "), total)
	if total == 0 {
		t.Errorf("%s's steady phase was answered no visit", blog.network.Name)
		return
	}
	for _, writer := range blog.writers {
		count := writerCounts[writer]
		// each count against an even share, by cross multiplying: count / (total / W) as a percent
		percent := count * 100 * len(blog.writers) / total
		if percent < settings.FairShareMinPercent || settings.FairShareMaxPercent < percent {
			t.Errorf("%s answered %d of %s's %d steady visits, %d%% of an even share, outside %d%%-%d%%: %s", writer.device.Name, count, blog.network.Name, total, percent, settings.FairShareMinPercent, settings.FairShareMaxPercent, strings.Join(spread, ", "))
		}
	}
}

// Holds every commit of blog's to the notification's bound: every other writer's run that ran from before the commit's
// answer to past its settling heard a change at or past the commit's ref version within SettleTimeout of the answer.
// Logs how long the changes took.
func checkBloggerNotifications(t testing.TB, blog *bloggerBlog, since time.Time) {
	t.Helper()
	settings := blog.settings
	missed := []string{}
	latencies := []time.Duration{}
	ownChanges := 0
	for _, writer := range blog.writers {
		for _, server := range writer.serverList() {
			clears := server.clearList()
			for _, clear := range clears {
				if clear.cause == bloggerClearChange && clear.signerSidecarId == writer.device.Sidecar().Identity().SidecarId {
					ownChanges += 1
				}
			}
			for _, commit := range blog.ledger.commitList() {
				settle := commit.answeredAt.Add(settings.SettleTimeout)
				stoppedAt := server.stopTime()
				if commit.answeredAt.Before(since) || commit.article.writer == writer || !server.startedAt.Before(commit.answeredAt) || (!stoppedAt.IsZero() && stoppedAt.Before(settle)) || time.Now().Before(settle) {
					continue
				}
				heard := false
				for _, clear := range clears {
					if clear.cause == bloggerClearChange && commit.refVersion <= clear.version {
						if !clear.at.After(settle) {
							heard = true
							latencies = append(latencies, clear.at.Sub(commit.answeredAt))
						}
						break
					}
				}
				if !heard {
					missed = append(missed, fmt.Sprintf("%s run %d: %s", writer.device.Name, server.run, blog.notificationEvidence(writer, server, commit)))
				}
			}
		}
	}
	slices.Sort(latencies)
	if 0 < len(latencies) {
		t.Logf("%s's changes reached the other writers' webservers %d times, from %s to %s past a commit's answer, %s at the median; %d changes named a writer's own push", blog.network.Name, len(latencies), latencies[0].Round(time.Millisecond), latencies[len(latencies)-1].Round(time.Millisecond), latencies[len(latencies)/2].Round(time.Millisecond), ownChanges)
	}
	if 0 < len(missed) {
		t.Errorf("%s's commits reached %d running webservers of other writers as no change within %s, so those caches were never told to clear: EvsClient raises a change only for a statement its subscriber's own read brings past the pin, and a read or write of the client's own that verified the statement first leaves the subscriber nothing to raise. %s", blog.network.Name, len(missed), settings.SettleTimeout, strings.Join(missed[:min(len(missed), 4)], " | "))
	}
}

// Holds tlsedge's restart in chaos to its readers reconnecting: every reader of blog's must read through tlsedge again
// within TlsedgeReconnectTimeout of the restart reading ready.
func checkBloggerReconnect(t testing.TB, blog *bloggerBlog, chaos *bloggerChaos) {
	t.Helper()
	restart := chaos.event(bloggerChaosTlsedgeRestart)
	if restart == nil {
		t.Errorf("the tlsedge chaos never restarted tlsedge")
		return
	}
	// each reader's first answer through tlsedge once its restart read ready, and its visits that failed before it
	outages := []string{}
	late := false
	for _, reader := range blog.readers {
		var first *bloggerVisit
		failedCount := 0
		var lastFailure *bloggerVisit
		for _, visit := range blog.ledger.visitList() {
			if visit.reader != reader || visit.phase != bloggerPhaseTlsedgeChaos || visit.startedAt.Before(restart.startedAt) {
				continue
			}
			if visit.err != nil {
				failedCount += 1
				lastFailure = visit
				continue
			}
			if visit.fields != nil && restart.endedAt.Before(visit.startedAt) && (first == nil || visit.endedAt.Before(first.endedAt)) {
				first = visit
			}
		}
		if first == nil {
			late = true
			outages = append(outages, fmt.Sprintf("%s read nothing after it, %d visits failing", reader.name, failedCount))
			continue
		}
		outage := first.endedAt.Sub(restart.endedAt)
		late = late || blog.settings.TlsedgeReconnectTimeout < outage
		line := fmt.Sprintf("%s read again %s after it, %d visits failing before", reader.name, outage.Round(time.Millisecond), failedCount)
		if lastFailure != nil {
			line += fmt.Sprintf(", the last %v", lastFailure.err)
		}
		outages = append(outages, line)
	}
	t.Logf("%s's readers after tlsedge's restart read ready at %s: %s", blog.network.Name, restart.endedAt.Format("15:04:05.000"), strings.Join(outages, "; "))
	if late {
		t.Errorf("%s's readers did not all read through tlsedge again within %s of its restart reading ready: %s", blog.network.Name, blog.settings.TlsedgeReconnectTimeout, strings.Join(outages, "; "))
	}
}

// Holds a writer's restart in chaos to its rejoining on a cold cache: every article its new run answered first from evs
// with nothing cached, at least one answer, and no answer from its stopped run after the stop.
func checkBloggerWriterRestart(t testing.TB, blog *bloggerBlog, writer *bloggerWriter, chaos *bloggerChaos) {
	t.Helper()
	stop := chaos.event(bloggerChaosWriterStop)
	if stop == nil || chaos.event(bloggerChaosWriterStart) == nil {
		t.Errorf("the writer chaos ran %d of its 2 events", len(chaos.eventList()))
		return
	}
	servers := writer.serverList()
	if len(servers) != 2 {
		t.Errorf("%s ran %d webservers through one restart", writer.device.Name, len(servers))
		return
	}
	firstAnswers := map[string]*bloggerVisit{}
	rejoinCount := 0
	for _, visit := range blog.ledger.visitList() {
		if visit.err != nil || visit.writer != writer {
			continue
		}
		switch visit.run {
		case 1:
			if stop.endedAt.Before(visit.startedAt) {
				t.Errorf("%s's stopped run answered a visit begun after its stop: %s", writer.device.Name, visit.describe())
			}
		case 2:
			// the writer chaos's alone: the last writes warm every cache
			if visit.phase != bloggerPhaseWriterChaos {
				continue
			}
			rejoinCount += 1
			if first := firstAnswers[visit.articleId]; first == nil || visit.startedAt.Before(first.startedAt) {
				firstAnswers[visit.articleId] = visit
			}
		}
	}
	if rejoinCount == 0 {
		t.Errorf("%s's restarted webserver answered no visit: sockets' scheduler never picked it again", writer.device.Name)
	}
	for _, visit := range firstAnswers {
		if visit.source != bloggerSourceEvs || 0 <= visit.cachedVersion {
			t.Errorf("%s's restarted webserver answered its first visit of %s from a warm cache: %s", writer.device.Name, visit.articleId, visit.describe())
		}
	}
	t.Logf("%s rejoined: its new run answered %d visits, %d articles first from evs", writer.device.Name, rejoinCount, len(firstAnswers))
}

// The isolation checks: each blog's first reader asks its own blog for the other's first article and is answered
// 404 by its own writer; the second network's writers read none of the first's repo, refused at their clients and
// by evs's 403 to their tokens.
func checkBloggerIsolation(ctx context.Context, t testing.TB, world *World, blogs []*bloggerBlog) {
	t.Helper()
	first, second := blogs[0], blogs[1]
	for _, pair := range [][2]*bloggerBlog{{first, second}, {second, first}} {
		blog, other := pair[0], pair[1]
		reader := blog.readers[0]
		article := other.articles[0]
		deadline := time.Now().Add(blog.settings.ReadRetryTimeout)
		for {
			visit := reader.visit(ctx, bloggerPhaseProbe, article.id)
			blog.ledger.addVisit(visit)
			if visit.err == nil {
				if visit.status != http.StatusNotFound || visit.problem != nil || visit.fields != nil {
					t.Errorf("%s asking %s for %s's article was answered other than 404: %s", reader.name, blog.serverName, other.network.Name, visit.describe())
				}
				break
			}
			if !time.Now().Before(deadline) {
				t.Errorf("%s's visits for %s's article kept failing: %s", reader.name, other.network.Name, visit.describe())
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s: %v", visit.describe(), ctx.Err())
			case <-time.After(blog.settings.ReadRetryInterval):
			}
		}
	}
	evsOrigin := world.T3().Api().ServiceOrigin(WorldBlockEvs)
	for _, writer := range second.writers {
		var content bytes.Buffer
		if _, err := writer.client.Repo(first.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: first.articles[0].path}, &content); err == nil || 0 < content.Len() {
			t.Errorf("%s of %s read %s's blog repo: %v, %d bytes", writer.device.Name, second.network.Name, first.network.Name, err, content.Len())
		}
		token, err := writer.device.Sidecar().ServiceToken(ctx, evsOrigin, []string{"repo:" + second.repoId.String() + ":r"})
		if err != nil {
			t.Errorf("%s's read token for its own blog repo: %v", writer.device.Name, err)
			continue
		}
		if status, _ := worldCall(ctx, world, http.MethodGet, evsOrigin+"/evs/v1/repos/"+sidecar.EvsKeyId(first.repoId), token, ""); status != http.StatusForbidden {
			t.Errorf("evs answered %s of %s %d at %s's blog repo, not 403", writer.device.Name, second.network.Name, status, first.network.Name)
		}
	}
}

// tlsedge's connection records of one network since a time, as the world's capture reads them at each scan.
type bloggerTlsRecords struct {
	networkId server.Id
	since     time.Time
}

func (self *bloggerTlsRecords) Records() []*tlsedgeservice.TlsConnectionRecord {
	return worldTlsConnectionRecords(context.Background(), self.networkId, self.since)
}

// The privacy checks: tlsedge's connection records of each network join the visits' bytes in the world's capture,
// whose scan must find none of the case's canaries, nor any secret the world handed it, in what tlsedge took and gave
// or anywhere else it holds.
func checkBloggerPrivacy(t testing.TB, world *World, blogs []*bloggerBlog, since time.Time) {
	t.Helper()
	capture := world.T3().Net().Capture()
	for _, blog := range blogs {
		capture.AddTlsEdgeRecords(bloggerTlsedgeSource+" records of "+blog.network.Name, &bloggerTlsRecords{networkId: blog.network.NetworkId, since: since})
	}
	if count := capture.ItemCount(sidecartest.PrivacyClassHandler); count == 0 {
		t.Errorf("the world's capture holds no handler item, so the scan read nothing tlsedge took or gave")
	}
	if count := capture.ItemCount(sidecartest.PrivacyClassEdgeRecord); count == 0 {
		t.Errorf("the world's capture holds no tlsedge connection record of the blogs' networks")
	}
	scanStartedAt := time.Now()
	findings := capture.Scan()
	edge, elsewhere := []string{}, []string{}
	for _, finding := range findings {
		if strings.HasPrefix(finding.Source, bloggerTlsedgeSource) {
			edge = append(edge, finding.String())
		} else {
			elsewhere = append(elsewhere, finding.String())
		}
	}
	t.Logf("the world's capture scanned in %s: %d findings at the edge, %d elsewhere", time.Since(scanStartedAt).Round(time.Millisecond), len(edge), len(elsewhere))
	if 0 < len(edge) {
		t.Errorf("tlsedge saw what it must not, only SNI and ciphertext being its to see: %s", strings.Join(edge[:min(len(edge), 8)], "; "))
	}
	if 0 < len(elsewhere) {
		t.Errorf("the world's capture holds %d findings past the edge: %s", len(elsewhere), strings.Join(elsewhere[:min(len(elsewhere), 8)], "; "))
	}
}

// Holds blog's network's ledger at the end: no balance overdrawn, every byte drawn from its balances a debit on a
// meter, exactly, the evs meter holding its writes and reads, and tlsedge's meter at least every byte the blog's
// visitors were sent through the splice, which spends each chunk before it moves.
func checkBloggerLedger(t testing.TB, blog *bloggerBlog, ledgers *WorldLedgers) {
	t.Helper()
	network := blog.network
	ledger := ledgers.Networks[network.NetworkId]
	switch {
	case ledger == nil:
		t.Errorf("%s has no quota ledger at the end", network.Name)
		return
	case ledger.Spent || ledger.BalanceByteCount < 0:
		t.Errorf("%s's balance at the end: %+v", network.Name, ledger)
	case ledgers.Meters.TotalByteCount(network.NetworkId) != ledger.DebitedByteCount():
		t.Errorf("%s's balances were debited %d by the end, its meters: %s", network.Name, ledger.DebitedByteCount(), ledgers.Meters.Describe(network.NetworkId))
	}
	if ledgers.Meters.ByteCount(network.NetworkId, sidecar.QuotaMeterEvs) <= 0 {
		t.Errorf("%s's evs meter holds nothing after its writes and reads: %s", network.Name, ledgers.Meters.Describe(network.NetworkId))
	}
	received, sent, spliced := 0, 0, 0
	for _, visit := range blog.ledger.visitList() {
		if visit.spliced() {
			received += visit.receivedByteCount
			sent += visit.sentByteCount
			spliced += 1
		}
	}
	metered := ledgers.Meters.ByteCount(network.NetworkId, sidecar.QuotaMeterTls)
	t.Logf("%s's meters at the end: %s; its %d spliced visits sent %d B past the lb's header and were sent %d B, tlsedge metered %d B", network.Name, ledgers.Meters.Describe(network.NetworkId), spliced, sent, received, metered)
	if metered < int64(received) {
		t.Errorf("tlsedge metered %d B of %s's visits, below the %d B its visitors were sent through the splice", metered, network.Name, received)
	}
}

// Logs every blog's progress: each writer's runs and pin, each reader's visits.
func bloggerLogProgress(t testing.TB, blogs []*bloggerBlog) {
	t.Helper()
	lines := []string{}
	for _, blog := range blogs {
		for _, writer := range blog.writers {
			for _, server := range writer.serverList() {
				lines = append(lines, "  "+server.progress())
			}
			lines = append(lines, fmt.Sprintf("  %s: pin %d, %d pin rises of its own", writer.device.Name, writer.pinVersion(), len(writer.pinRiseList())))
		}
		readerVisitCounts := map[string]int{}
		readerFailCounts := map[string]int{}
		for _, visit := range blog.ledger.visitList() {
			readerVisitCounts[visit.reader.name] += 1
			if visit.err != nil {
				readerFailCounts[visit.reader.name] += 1
			}
		}
		names := []string{}
		for name := range readerVisitCounts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			lines = append(lines, fmt.Sprintf("  %s: %d visits, %d failed", name, readerVisitCounts[name], readerFailCounts[name]))
		}
		lines = append(lines, fmt.Sprintf("  %s: %d commits", blog.network.Name, len(blog.ledger.commitList())))
	}
	t.Logf("the blogs:\n%s", strings.Join(lines, "\n"))
}

// The world's progress dump (World.FailProgress) after the blogs'.
func bloggerFailProgress(t testing.TB, world *World, blogs []*bloggerBlog, phase string, err error, bursts []*WorldBurst) {
	t.Helper()
	bloggerLogProgress(t, blogs)
	world.FailProgress(t, phase, err, bursts)
}
