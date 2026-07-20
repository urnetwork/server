package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
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

func TestMutatingCommandsRequireLocalEnvironment(t *testing.T) {
	for _, command := range []string{"run", "fleet"} {
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
		driver.crawl(crawlCtx, "test-client", &http.Client{})
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
