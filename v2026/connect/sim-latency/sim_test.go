package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// the competition depends on providers.yml locking an identical run: the same
// seed must produce the same fleet, and the file must round-trip.
func TestGenerateFleetReproducible(t *testing.T) {
	a := defaultConfig(42, 500, 50, 200)
	b := defaultConfig(42, 500, 50, 200)
	if err := generateFleet(a); err != nil {
		t.Fatalf("generate a: %v", err)
	}
	if err := generateFleet(b); err != nil {
		t.Fatalf("generate b: %v", err)
	}
	if len(a.Fleet) != 500 || len(b.Fleet) != 500 {
		t.Fatalf("fleet size = %d, %d; want 500", len(a.Fleet), len(b.Fleet))
	}
	for i := range a.Fleet {
		if a.Fleet[i] != b.Fleet[i] {
			t.Fatalf("fleet entry %d differs between identical seeds:\n a=%+v\n b=%+v", i, a.Fleet[i], b.Fleet[i])
		}
	}
	// unique ips, and every entry carries an id and a component
	ips := map[string]bool{}
	for _, entry := range a.Fleet {
		if entry.Ip == "" || entry.ClientId == "" || entry.Component == "" {
			t.Fatalf("incomplete entry: %+v", entry)
		}
		if ips[entry.Ip] {
			t.Fatalf("duplicate ip %s", entry.Ip)
		}
		ips[entry.Ip] = true
	}
}

func TestConfigRoundTrip(t *testing.T) {
	config := defaultConfig(7, 100, 10, 60)
	config.Clients.QualityWindowSize = 5
	if err := generateFleet(config); err != nil {
		t.Fatalf("generate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "providers.yml")
	if err := SaveConfig(path, config); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := loaded.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(loaded.Fleet) != len(config.Fleet) {
		t.Fatalf("fleet len %d != %d", len(loaded.Fleet), len(config.Fleet))
	}
	if loaded.Fleet[0] != config.Fleet[0] {
		t.Fatalf("entry 0 changed across round-trip")
	}
	if loaded.Clients.QualityWindowSize != 5 {
		t.Fatalf("quality window = %d, want 5", loaded.Clients.QualityWindowSize)
	}
	// sharding partitions the fleet exactly
	seen := map[int]bool{}
	for shard := 0; shard < 4; shard += 1 {
		for _, entry := range loaded.shard(shard, 4) {
			if entry.Index%4 != shard {
				t.Fatalf("entry %d in wrong shard %d", entry.Index, shard)
			}
			if seen[entry.Index] {
				t.Fatalf("entry %d in two shards", entry.Index)
			}
			seen[entry.Index] = true
		}
	}
	if len(seen) != len(loaded.Fleet) {
		t.Fatalf("sharding lost entries: %d != %d", len(seen), len(loaded.Fleet))
	}
}

func TestConfigRejectsInvalidQualityWindow(t *testing.T) {
	config := defaultConfig(7, 100, 10, 60)
	config.Clients.QualityWindowSize = 33
	if err := config.validate(); err == nil {
		t.Fatal("quality window above the calibration bound was accepted")
	}
}

func TestGeneratedConfigBuildsFleetBeforeCompleteValidation(t *testing.T) {
	config, err := generatedConfig(48, 25, 5, 8, 2)
	if err != nil {
		t.Fatalf("generated config: %v", err)
	}
	if len(config.Fleet) != 25 {
		t.Fatalf("fleet size = %d, want 25", len(config.Fleet))
	}
	if config.Clients.QualityWindowSize != 2 {
		t.Fatalf("quality window = %d, want 2", config.Clients.QualityWindowSize)
	}
}

func TestMutatingCommandsRequireExpectedEnvironment(t *testing.T) {
	for _, command := range []string{"run", "fleet", "reset", "baseline"} {
		if err := validateEnvironment(command, "local"); err != nil {
			t.Fatalf("%s rejected local environment: %v", command, err)
		}
		for _, env := range []string{"main", "staging", ""} {
			if err := validateEnvironment(command, env); err == nil {
				t.Fatalf("%s accepted unsafe environment %q", command, env)
			}
		}
	}

	// init only creates a local configuration file and remains usable without
	// a service environment.
	if err := validateEnvironment("init", "main"); err != nil {
		t.Fatalf("init should not require a service environment: %v", err)
	}

	for _, command := range []string{"epoch-review", "promote", "launch-preflight", "credentials"} {
		if err := validateEnvironment(command, "main"); err != nil {
			t.Fatalf("%s rejected main environment: %v", command, err)
		}
		for _, env := range []string{"local", "staging", ""} {
			if err := validateEnvironment(command, env); err == nil {
				t.Fatalf("%s accepted unsafe environment %q", command, env)
			}
		}
	}
}

// the fake site's loading tree must terminate: following suburls from "/"
// yields a finite, bounded crawl.
func TestSiteTreeTerminates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	site, err := NewSite(ctx, "127.0.0.1:0", 99, SiteConfig{
		MeanDepth: 4, Branching: 3, MinBodyBytes: 16, MaxBodyBytes: 64,
	})
	if err != nil {
		t.Fatalf("site: %v", err)
	}

	// crawl breadth-first from "/", bounded so a non-terminating tree fails
	// loudly instead of hanging
	client := &http.Client{}
	queue := []string{"/"}
	visited := 0
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		visited += 1
		if visited > 100000 {
			t.Fatalf("crawl did not terminate (>100k pages)")
		}
		urls := fetchSitePage(t, client, site.Addr(), path)
		queue = append(queue, urls...)
	}
	// a mean-depth-4 tree with one continuing child per node is ~1+4*3 pages;
	// just assert it is finite and non-trivial
	if visited < 1 {
		t.Fatalf("no pages crawled")
	}
	t.Logf("crawled %d pages", visited)
}

// A workload contains many independent crawls, so its tree-depth distribution
// must be sampled across those crawls. Sampling only from (seed, "/") makes
// every arrival share one draw and lets a valid hidden seed collapse the whole
// measurement to root pages.
func TestSiteRootTopologyVariesAcrossCrawls(t *testing.T) {
	const crawlCount = 80
	for _, seed := range []int64{1, 99, 1001} {
		config := defaultConfig(seed, 900, crawlCount, 30)
		handler := &siteHandler{seed: seed, site: config.Site}
		pairedHandler := &siteHandler{seed: seed, site: config.Site}
		depths := map[int]bool{}
		totalDepth := 0
		for crawlIndex := 0; crawlIndex < crawlCount; crawlIndex += 1 {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodGet,
				fmt.Sprintf("http://site.test/?crawl=%d", crawlIndex),
				nil,
			)
			handler.ServeHTTP(recorder, request)
			result, err := readSiteResponse(recorder.Result())
			if err != nil || !result.complete {
				t.Fatalf("seed %d crawl %d response is incomplete: result=%+v err=%v", seed, crawlIndex, result, err)
			}
			pairedRecorder := httptest.NewRecorder()
			pairedHandler.ServeHTTP(pairedRecorder, request.Clone(request.Context()))
			pairedResult, pairedErr := readSiteResponse(pairedRecorder.Result())
			if pairedErr != nil || !pairedResult.complete {
				t.Fatalf(
					"seed %d paired crawl %d response is incomplete: result=%+v err=%v",
					seed,
					crawlIndex,
					pairedResult,
					pairedErr,
				)
			}
			if result.page.Size != pairedResult.page.Size ||
				strings.Join(result.page.Urls, "\n") != strings.Join(pairedResult.page.Urls, "\n") {
				t.Fatalf(
					"seed %d crawl %d is not reproducible: first=%+v paired=%+v",
					seed,
					crawlIndex,
					result.page,
					pairedResult.page,
				)
			}

			depth := 0
			if 0 < len(result.page.Urls) {
				if len(result.page.Urls) != int(config.Site.Branching) {
					t.Fatalf(
						"seed %d crawl %d root links = %d, want %d",
						seed,
						crawlIndex,
						len(result.page.Urls),
						int(config.Site.Branching),
					)
				}
				remaining, ok := handler.parsePath(result.page.Urls[0])
				if !ok {
					t.Fatalf("seed %d crawl %d has invalid child path %q", seed, crawlIndex, result.page.Urls[0])
				}
				depth = remaining + 1
			}
			depths[depth] = true
			totalDepth += depth
		}

		if len(depths) < 4 {
			t.Errorf("seed %d produced only %d distinct depths across %d crawls: %v", seed, len(depths), crawlCount, depths)
		}
		meanDepth := float64(totalDepth) / crawlCount
		if meanDepth < 2 || 6 < meanDepth {
			t.Errorf("seed %d mean depth = %.3f across %d crawls, want [2, 6]", seed, meanDepth, crawlCount)
		}
	}
}

// A hidden workload's scored root may be a slow download-tier page. Warmup
// must use a small unscored page so body-size sampling cannot decide whether a
// client is reported as having a provider path.
func TestSiteWarmupPageIsBoundedAcrossWorkloadSeeds(t *testing.T) {
	const failedCalibrationSeed = int64(6775002577590458567)
	config := defaultConfig(failedCalibrationSeed, 900, 80, 30)

	rootRecorder := httptest.NewRecorder()
	rootRequest := httptest.NewRequest(http.MethodGet, "http://site.test/", nil)
	(&siteHandler{seed: failedCalibrationSeed, site: config.Site}).ServeHTTP(rootRecorder, rootRequest)
	root, err := readSiteResponse(rootRecorder.Result())
	if err != nil || !root.complete {
		t.Fatalf("hidden-seed root response is incomplete: result=%+v err=%v", root, err)
	}
	if root.page.Size < config.Site.LargeMinBodyBytes {
		t.Fatalf(
			"hidden-seed root size = %d, want download tier >= %d",
			root.page.Size,
			config.Site.LargeMinBodyBytes,
		)
	}

	for _, seed := range []int64{48, failedCalibrationSeed} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://site.test"+siteWarmupPath, nil)
		(&siteHandler{seed: seed, site: config.Site}).ServeHTTP(recorder, request)
		result, err := readSiteResponse(recorder.Result())
		if err != nil || !result.complete {
			t.Fatalf("seed %d warmup response is incomplete: result=%+v err=%v", seed, result, err)
		}
		if result.page.Size != siteWarmupBodyBytes || len(result.page.Urls) != 0 {
			t.Fatalf(
				"seed %d warmup page = %+v, want no links and %d bytes",
				seed,
				result.page,
				siteWarmupBodyBytes,
			)
		}
	}
}

// The arrival index is the reproducible boundary between the load generator
// and fake site. Losing it from the root request silently restores one shared
// depth draw for every crawl in the evaluation.
func TestCrawlSendsStableIndexToSiteRoot(t *testing.T) {
	requestUris := make(chan string, 2)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestUris <- r.URL.RequestURI()
		(&siteHandler{}).writePage(w, sitePage{Size: 16})
	}))
	defer httpServer.Close()

	config := &Config{}
	config.Clients.ConnectionsPerCrawl = 1
	driver := &ClientDriver{
		ctx:      context.Background(),
		config:   config,
		siteAddr: strings.TrimPrefix(httpServer.URL, "http://"),
		out:      bufio.NewWriter(io.Discard),
	}
	for _, crawlIndex := range []int{37, 38} {
		driver.crawl(context.Background(), "test-client", httpServer.Client(), crawlIndex)
		requestUri := <-requestUris
		expectedUri := fmt.Sprintf("/?crawl=%d", crawlIndex)
		if requestUri != expectedUri {
			t.Fatalf("crawl %d root request = %q, want %q", crawlIndex, requestUri, expectedUri)
		}
	}
}

// a canceled crawl must fully unwind: queued jobs are balanced and the closer
// goroutine (pending.Wait) completes. crawl now joins the closer, so returning
// proves no goroutine is left waiting — before the fix each timed-out crawl
// leaked one.
func TestCrawlCancelDoesNotLeak(t *testing.T) {
	// A fake site whose root fans out 40 children and whose children never
	// respond. The explicit child-start barrier proves both workers are blocked
	// and queued work remains when cancellation lands.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	childStarted := make(chan struct{}, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			urls := []string{}
			for i := 0; i < 40; i += 1 {
				urls = append(urls, fmt.Sprintf("/child/%d", i))
			}
			headerBytes, _ := json.Marshal(sitePage{Urls: urls})
			w.Write(headerBytes)
			w.Write([]byte("\n"))
			return
		}
		select {
		case childStarted <- struct{}{}:
		default:
		}
		// Children stall until the client gives up.
		<-r.Context().Done()
	})
	httpServer := &http.Server{Handler: handler}
	go httpServer.Serve(listener)
	defer httpServer.Close()

	config := &Config{}
	config.Clients.ConnectionsPerCrawl = 2
	driver := &ClientDriver{
		ctx:      context.Background(),
		config:   config,
		siteAddr: listener.Addr().String(),
		out:      bufio.NewWriter(io.Discard),
	}

	crawlCtx, crawlCancel := context.WithCancel(context.Background())
	defer crawlCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		driver.crawl(crawlCtx, "test-client", &http.Client{}, 0)
	}()

	for workerIndex := 0; workerIndex < config.Clients.ConnectionsPerCrawl; workerIndex += 1 {
		select {
		case <-childStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("crawl did not reach the deterministic blocked-worker barrier")
		}
	}
	crawlCancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("canceled crawl did not unwind: queued jobs were not balanced (leaked pending.Wait)")
	}
}

// A lifecycle submission is one immutable FIFO admission presented to the
// deterministic RUN-MAIN contract simulator.
type lifecycleSubmission struct {
	jobId                     string
	submittedAt               time.Time
	statisticallySignificant  bool
	honest                    bool
	baselineSampleVariance    float64
	candidateSampleVariance   float64
	nextImprovementPercentage float64
}

// A lifecycle epoch fixes the exact admission window and submitted jobs used
// to exercise one agent-controlled review and promotion transition.
type lifecycleEpoch struct {
	epoch       int
	opensAt     time.Time
	closesAt    time.Time
	submissions []lifecycleSubmission
}

// A lifecycle transition is the durable state that RUN-MAIN carries into the
// next epoch after review and promotion.
type lifecycleTransition struct {
	epoch                 int
	winnerJobId           string
	rejectedJobIds        []string
	discardedJobIds       []string
	evaluatedJobIds       []string
	public                bool
	workerExited          bool
	drainedPastClose      bool
	improvementPercentage float64
	sourceCommit          string
}

// The lifecycle simulator has no wall-clock waits. Advancing its explicit
// clock is the barrier that proves early starts wait and post-close work drains.
type lifecycleHarness struct {
	now                   time.Time
	sourceCommit          string
	improvementPercentage float64
	waitedEpochs          []int
	transitions           []lifecycleTransition
}

// Completes all six admission, evaluation, review, and promotion transitions
// with the same fail-closed ordering required by RUN-MAIN.md.
func (self *lifecycleHarness) run(epochs []lifecycleEpoch) error {
	if len(epochs) != maximumCompetitionEpoch {
		return fmt.Errorf("season has %d epochs, want %d", len(epochs), maximumCompetitionEpoch)
	}
	for epochIndex, epoch := range epochs {
		expectedEpoch := epochIndex + 1
		if epoch.epoch != expectedEpoch || !epoch.opensAt.Before(epoch.closesAt) ||
			epoch.closesAt.Sub(epoch.opensAt) != 7*24*time.Hour {
			return fmt.Errorf("epoch %d has an invalid identity or admission window", expectedEpoch)
		}
		if self.now.Before(epoch.opensAt) {
			self.waitedEpochs = append(self.waitedEpochs, epoch.epoch)
			self.now = epoch.opensAt
		}

		transition := lifecycleTransition{
			epoch:                 epoch.epoch,
			improvementPercentage: self.improvementPercentage,
			sourceCommit:          self.sourceCommit,
		}
		eligibleSubmissions := []lifecycleSubmission{}
		for _, submission := range epoch.submissions {
			if submission.submittedAt.Before(epoch.opensAt) || !submission.submittedAt.Before(epoch.closesAt) {
				transition.discardedJobIds = append(transition.discardedJobIds, submission.jobId)
				continue
			}
			transition.evaluatedJobIds = append(transition.evaluatedJobIds, submission.jobId)
			if submission.baselineSampleVariance <= 0 || submission.candidateSampleVariance <= 0 {
				return fmt.Errorf("job %s omitted its immutable variance record", submission.jobId)
			}
			if submission.statisticallySignificant {
				eligibleSubmissions = append(eligibleSubmissions, submission)
			}
		}

		// The last accepted job deliberately completes after admission closes;
		// close rejects only new work and cannot truncate the FIFO.
		if len(transition.evaluatedJobIds) != 0 {
			self.now = epoch.closesAt.Add(time.Minute)
			transition.drainedPastClose = true
		} else if self.now.Before(epoch.closesAt) {
			self.now = epoch.closesAt
		}
		for _, candidate := range eligibleSubmissions {
			if !candidate.honest {
				transition.rejectedJobIds = append(transition.rejectedJobIds, candidate.jobId)
				continue
			}
			transition.winnerJobId = candidate.jobId
			transition.improvementPercentage = candidate.nextImprovementPercentage
			transition.sourceCommit = fmt.Sprintf("source-epoch-%d-%s", epoch.epoch, candidate.jobId)
			break
		}
		// Results become public only after every accepted job is terminal and
		// honesty review has either selected a winner or exhausted candidates.
		transition.workerExited = true
		transition.public = true
		self.improvementPercentage = transition.improvementPercentage
		self.sourceCommit = transition.sourceCommit
		self.transitions = append(self.transitions, transition)
	}
	return nil
}

// Exercises the complete six-week RUN-MAIN contract without sleeping: start
// barriers, admission boundaries, FIFO drain, embargo, review advancement,
// no-winner carry-forward, winner promotion, and final process exit.
func TestRunMainCompleteSixEpochLifecycle(t *testing.T) {
	firstOpen := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	harness := &lifecycleHarness{
		now:                   firstOpen.Add(-time.Hour),
		sourceCommit:          "source-epoch-0-baseline",
		improvementPercentage: 16.1,
	}
	epochs := make([]lifecycleEpoch, 0, maximumCompetitionEpoch)
	for epoch := 1; epoch <= maximumCompetitionEpoch; epoch += 1 {
		opensAt := firstOpen.Add(time.Duration(epoch-1) * (7*24*time.Hour + time.Hour))
		closesAt := opensAt.Add(7 * 24 * time.Hour)
		submissions := []lifecycleSubmission{
			{
				jobId: "epoch-before-window", submittedAt: opensAt.Add(-time.Nanosecond),
				baselineSampleVariance: 1, candidateSampleVariance: 1,
			},
			{
				jobId: "epoch-nonsignificant", submittedAt: opensAt,
				baselineSampleVariance: 4, candidateSampleVariance: 3,
			},
			{
				jobId: "epoch-at-close", submittedAt: closesAt,
				baselineSampleVariance: 1, candidateSampleVariance: 1,
			},
		}
		switch epoch {
		case 1:
			submissions = append(submissions,
				lifecycleSubmission{
					jobId: "epoch-1-dishonest", submittedAt: opensAt.Add(time.Second),
					statisticallySignificant: true, honest: false,
					baselineSampleVariance: 5, candidateSampleVariance: 2,
					nextImprovementPercentage: 13.5,
				},
				lifecycleSubmission{
					jobId: "epoch-1-winner", submittedAt: opensAt.Add(2 * time.Second),
					statisticallySignificant: true, honest: true,
					baselineSampleVariance: 5, candidateSampleVariance: 2,
					nextImprovementPercentage: 13.0,
				},
			)
		case 3, 5, 6:
			submissions = append(submissions, lifecycleSubmission{
				jobId: fmt.Sprintf("epoch-%d-winner", epoch), submittedAt: opensAt.Add(time.Second),
				statisticallySignificant: true, honest: true,
				baselineSampleVariance: 4, candidateSampleVariance: 2,
				nextImprovementPercentage: 13.0 - float64(epoch),
			})
		case 4:
			submissions = append(submissions, lifecycleSubmission{
				jobId: "epoch-4-dishonest", submittedAt: opensAt.Add(time.Second),
				statisticallySignificant: true, honest: false,
				baselineSampleVariance: 4, candidateSampleVariance: 2,
				nextImprovementPercentage: 8.0,
			})
		}
		epochs = append(epochs, lifecycleEpoch{
			epoch: epoch, opensAt: opensAt, closesAt: closesAt, submissions: submissions,
		})
	}

	if err := harness.run(epochs); err != nil {
		t.Fatal(err)
	}
	if len(harness.transitions) != maximumCompetitionEpoch ||
		len(harness.waitedEpochs) != maximumCompetitionEpoch {
		t.Fatalf("completed transitions=%d waits=%d, want six of each", len(harness.transitions), len(harness.waitedEpochs))
	}
	expectedWinners := []string{
		"epoch-1-winner", "", "epoch-3-winner", "", "epoch-5-winner", "epoch-6-winner",
	}
	for epochIndex, transition := range harness.transitions {
		if transition.winnerJobId != expectedWinners[epochIndex] {
			t.Errorf("epoch %d winner = %q, want %q", transition.epoch, transition.winnerJobId, expectedWinners[epochIndex])
		}
		if !transition.workerExited || !transition.public || !transition.drainedPastClose {
			t.Errorf("epoch %d did not drain, seal, publish, and exit: %+v", transition.epoch, transition)
		}
		if len(transition.discardedJobIds) != 2 ||
			transition.discardedJobIds[0] != "epoch-before-window" ||
			transition.discardedJobIds[1] != "epoch-at-close" {
			t.Errorf("epoch %d discarded jobs = %v", transition.epoch, transition.discardedJobIds)
		}
		if len(transition.evaluatedJobIds) == 0 || transition.evaluatedJobIds[0] != "epoch-nonsignificant" {
			t.Errorf("epoch %d did not preserve FIFO admission: %v", transition.epoch, transition.evaluatedJobIds)
		}
	}
	if len(harness.transitions[0].rejectedJobIds) != 1 ||
		harness.transitions[0].rejectedJobIds[0] != "epoch-1-dishonest" ||
		len(harness.transitions[3].rejectedJobIds) != 1 ||
		harness.transitions[3].rejectedJobIds[0] != "epoch-4-dishonest" {
		t.Fatalf("dishonest candidate advancement was not preserved: %+v", harness.transitions)
	}
	for _, epochIndex := range []int{1, 3} {
		previous := harness.transitions[epochIndex-1]
		current := harness.transitions[epochIndex]
		if current.sourceCommit != previous.sourceCommit ||
			current.improvementPercentage != previous.improvementPercentage {
			t.Errorf("epoch %d no-winner transition changed its incumbent", current.epoch)
		}
	}
	if harness.sourceCommit != "source-epoch-6-epoch-6-winner" ||
		harness.improvementPercentage != 7.0 {
		t.Fatalf("final source = %q threshold=%.1f", harness.sourceCommit, harness.improvementPercentage)
	}
}

func fetchSitePage(t *testing.T, client *http.Client, addr string, path string) []string {
	response, err := client.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status %d", path, response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// the leading line is the json page; the rest is padding
	newline := -1
	for i, b := range body {
		if b == '\n' {
			newline = i
			break
		}
	}
	if newline < 0 {
		return nil
	}
	var page sitePage
	if err := json.Unmarshal(body[:newline], &page); err != nil {
		return nil
	}
	return page.Urls
}
