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

func TestMutatingCommandsRequireLocalEnvironment(t *testing.T) {
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
	// a fake site whose root fans out 40 children and whose children never
	// respond: the job queue is full and both workers are blocked when the
	// cancel lands, the exact shape that leaked
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
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
		// children stall until the client gives up
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

	// let the root fetch fan out and the workers block on stalled children,
	// then cancel mid-crawl with jobs still queued
	time.Sleep(300 * time.Millisecond)
	crawlCancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("canceled crawl did not unwind: queued jobs were not balanced (leaked pending.Wait)")
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
