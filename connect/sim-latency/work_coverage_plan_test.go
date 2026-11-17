package main

// Frozen-reference controls protect the historical site's bytes, while the
// prospective inventory is exercised without a socket or candidate process.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
)

// This source preflight uses the intended unchanged 80/min, 180s, 120s tuple
// and the canonical site generator with many synthetic seeds. Provider/client
// record counts do not affect this inventory, so no real fleet is constructed.
// It is not an accepted seed, new public freeze, R9 or native qualification.
func TestWorkCoveragePlanPreflightsIntendedShapeAndRejectsOversizeDefault(t *testing.T) {
	for seed := int64(1); seed <= 32; seed++ {
		content, err := controller.GenerateSimLatencyWorkload(seed, 1, 1, 80, 0)
		if err != nil {
			t.Fatal(err)
		}
		config, err := decodeConfig(content)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := buildWorkCoveragePlan(config, 180*time.Second, 120*time.Second, defaultWorkCoverageLimits())
		if err != nil || plan.TotalPages <= 0 || plan.TotalBytes <= 0 || len(plan.Roots) == 0 {
			t.Fatal("intended workload shape did not fit explicit inventory ceilings", seed, err)
		}
	}
	content, err := controller.GenerateSimLatencyWorkload(71, 1, 1, 80, 0)
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeConfig(content)
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := buildWorkCoveragePlan(config, 30*time.Minute, 120*time.Second, defaultWorkCoverageLimits()); err == nil || plan != nil {
		t.Fatal("oversized thirty-minute default was silently shortened or clipped")
	}
}

// This deliberately retains the pre-extraction implementation rather than
// calling the new descriptor helper, including its complete RNG draw order.
func frozenWorkCoveragePage(handler *siteHandler, path, identity string) sitePage {
	remaining, _ := handler.parsePath(path)
	random := handler.pathRng(identity)
	if path == "/" {
		remaining = random.geometricDepth(handler.site.MeanDepth)
		if siteMaxDepth < remaining {
			remaining = siteMaxDepth
		}
	}
	size := handler.site.MinBodyBytes
	if 0 < handler.site.LargeFraction && 0 < handler.site.LargeMinBodyBytes && random.float64() < handler.site.LargeFraction {
		size = handler.site.LargeMinBodyBytes
		if handler.site.LargeMaxBodyBytes > handler.site.LargeMinBodyBytes {
			size += random.intn(handler.site.LargeMaxBodyBytes - handler.site.LargeMinBodyBytes)
		}
	} else if handler.site.MaxBodyBytes > handler.site.MinBodyBytes {
		size += random.intn(handler.site.MaxBodyBytes - handler.site.MinBodyBytes)
	}
	urls := []string{}
	if 0 < remaining {
		branching := max(1, int(handler.site.Branching))
		for index := 0; index < branching; index++ {
			next := 0
			if index == 0 {
				next = remaining - 1
			}
			urls = append(urls, fmt.Sprintf("/p/%d/%s", next, handler.childToken(identity, index)))
		}
	}
	return sitePage{Urls: urls, Size: size}
}

// The fixed literal also checks null-versus-empty URLs and newline semantics.
func TestWorkCoverageDescriptorPreservesFrozenSiteBytes(t *testing.T) {
	for _, seed := range []int64{-73, 0, 1, 29, math.MaxInt64} {
		for _, depth := range []float64{0, 2.5, 100} {
			for _, tier := range []float64{0, .25, 1} {
				handler := &siteHandler{seed: seed, site: SiteConfig{MeanDepth: depth, Branching: 2.5, MinBodyBytes: 13, MaxBodyBytes: 71, LargeFraction: tier, LargeMinBodyBytes: 32769, LargeMaxBodyBytes: 32799}}
				for _, uri := range []string{"/?crawl=0", "/?crawl=19", "/p/0/synthetic", "/p/1/synthetic", "/p/24/synthetic"} {
					request := httptest.NewRequest("GET", uri, nil)
					want := frozenWorkCoveragePage(handler, request.URL.Path, request.URL.RequestURI())
					actual, ok, err := handler.describePage(request.URL.Path, request.URL.RequestURI(), func(random *rng, mean float64) (int, error) { return random.geometricDepth(mean), nil })
					if err != nil || !ok || !equalWorkCoveragePage(want, actual) {
						t.Fatal("descriptor changed frozen seeded response", seed, depth, tier, uri, err)
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					header, err := json.Marshal(want)
					if err != nil {
						t.Fatal(err)
					}
					body := append(header, '\n')
					for offset := 0; offset < want.Size; offset++ {
						body = append(body, byte('a'+(offset%(32<<10))%26))
					}
					if !bytes.Equal(response.Body.Bytes(), body) || response.Header().Get("Content-Length") != fmt.Sprint(len(body)) {
						t.Fatal("site header/body bytes changed from frozen reference", seed, depth, tier, uri)
					}
				}
			}
		}
	}
	handler := &siteHandler{site: SiteConfig{MinBodyBytes: 3, MaxBodyBytes: 3}}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/?crawl=0", nil))
	if response.Body.String() != "{\"urls\":[],\"size\":3}\nabc" {
		t.Fatal("literal historical site representation changed", response.Body.String())
	}
}

// Small positive fixtures use only synthetic seeded workload settings.
func workCoverageConfigForTest() *Config {
	return &Config{Seed: 71, Site: SiteConfig{MeanDepth: 30, Branching: 2, MinBodyBytes: 13, MaxBodyBytes: 31}, Clients: ClientsConfig{PoolSize: 3, MeanPerMinute: 180, ConnectionsPerCrawl: 2}}
}

// The RNG stream is identical to historical arrivals, but their times cannot
// move when the event loop or a first matchmaking probe runs late.
func TestWorkCoveragePlanMatchesSeededRootsAndFiniteInventory(t *testing.T) {
	config := workCoverageConfigForTest()
	plan, err := buildWorkCoveragePlan(config, 6*time.Second, 120*time.Second, defaultWorkCoverageLimits())
	if err != nil || len(plan.Roots) == 0 || plan.TotalPages <= int64(len(plan.Roots)) {
		t.Fatal("finite positive offered plan missing", err)
	}
	random, next := newRng(config.Seed^0x5eed), 0
	for offset := time.Second; offset < 6*time.Second; offset += time.Second {
		for count := random.poisson(config.Clients.MeanPerMinute / 60); 0 < count; count-- {
			if len(plan.Roots) <= next || plan.Roots[next].OffsetNs != int64(offset) || plan.Roots[next].Pages[0].Uri != fmt.Sprintf("/?crawl=%d", next) {
				t.Fatal("seeded offered root or admission boundary changed", offset, next)
			}
			next++
		}
	}
	if next != len(plan.Roots) {
		t.Fatal("unplanned roots appeared")
	}
	handler := &siteHandler{seed: config.Seed, site: config.Site}
	var totalPages, totalBytes int64
	for _, root := range plan.Roots {
		seen := map[string]bool{}
		for _, item := range root.Pages {
			path := item.Uri
			if item.Depth == 0 {
				path = "/"
			}
			want := frozenWorkCoveragePage(handler, path, item.Uri)
			header, _ := json.Marshal(want)
			if seen[item.Uri] || !equalWorkCoveragePage(item.Page, want) || item.Bytes != int64(len(header)+1+want.Size) || item.Depth > siteMaxDepth {
				t.Fatal("inventory lost exact deduplicated representation", item.Uri)
			}
			seen[item.Uri] = true
			totalPages++
			totalBytes += item.Bytes
		}
		if len(root.Pages) > 1+siteMaxDepth*int(config.Site.Branching) {
			t.Fatal("linear continuing-child graph became exponential")
		}
	}
	other, err := buildWorkCoveragePlan(config, 6*time.Second, 120*time.Second, defaultWorkCoverageLimits())
	if err != nil || !reflect.DeepEqual(plan, other) || totalPages != plan.TotalPages || totalBytes != plan.TotalBytes {
		t.Fatal("offered inventory is not independently reproducible", err)
	}
}

// Bounds fail before allocation and never silently clip a denominator.
func TestWorkCoveragePlanRejectsUnsupportedShapeAndBudgets(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(config *Config) { config.Site.Branching = math.Inf(1) },
		func(config *Config) { config.Site.Branching = 1 << 30 },
		func(config *Config) { config.Site.MeanDepth = math.NaN() },
		func(config *Config) { config.Site.MinBodyBytes = -1 },
		func(config *Config) { config.Site.LargeFraction = 1.1 },
		func(config *Config) { config.Site.MaxBodyBytes = math.MaxInt },
		func(config *Config) { config.Clients.MeanPerMinute = math.Inf(1) },
		func(config *Config) { config.Clients.ConnectionsPerCrawl = 0 },
		func(config *Config) { config.Clients.MeanPerMinute = 0 },
	} {
		config := workCoverageConfigForTest()
		mutate(config)
		if _, err := buildWorkCoveragePlan(config, 6*time.Second, time.Second, defaultWorkCoverageLimits()); err == nil {
			t.Fatal("unsupported workload became a truncated or empty offered plan")
		}
	}
	for _, mutate := range []func(*workCoverageLimits){
		func(limits *workCoverageLimits) { limits.Roots = 1 },
		func(limits *workCoverageLimits) { limits.Pages = 1 },
		func(limits *workCoverageLimits) { limits.PagesPerRoot = 2 },
		func(limits *workCoverageLimits) { limits.Ticks = 1 },
		func(limits *workCoverageLimits) { limits.RandomDraws = 1 },
		func(limits *workCoverageLimits) { limits.BodyBytes = 1 },
		func(limits *workCoverageLimits) { limits.InventoryBytes = 1 },
		func(limits *workCoverageLimits) { limits.ExpectedBytes = 1 },
	} {
		limits := defaultWorkCoverageLimits()
		mutate(&limits)
		if _, err := buildWorkCoveragePlan(workCoverageConfigForTest(), 6*time.Second, time.Second, limits); err == nil {
			t.Fatal("inventory exceeded an owner bound")
		}
	}
}
