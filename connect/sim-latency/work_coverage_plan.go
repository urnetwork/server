package main

// The prospective workload is inventoried before any response is observed.
// Bounds reject an unsupported workload; they never truncate offered work.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"time"

	"github.com/urnetwork/server/model"
)

// These are allocation/iteration ceilings of the versioned instrument, not
// candidate knobs. Changing them changes the instrument release identity.
type workCoverageLimits struct {
	Roots, Pages, PagesPerRoot, Ticks, RandomDraws int
	BodyBytes, InventoryBytes, ExpectedBytes       int64
}

// The frozen workload must pass these owner ceilings before qualification.
// Unsupported shape or overflow is rejected, never silently truncated.
func defaultWorkCoverageLimits() workCoverageLimits {
	return workCoverageLimits{
		Roots: 16384, Pages: 1024 * 1024, PagesPerRoot: 4096,
		Ticks: 65536, RandomDraws: 16 * 1024 * 1024,
		BodyBytes: 64 * 1024 * 1024, InventoryBytes: model.CompetitionWorkCoverageMaximumManifestBytes / 2, ExpectedBytes: 16 * 1024 * 1024 * 1024 * 1024,
	}
}

// Expected response representations include the canonical JSON line and its
// newline as well as padding. URI identity is scoped to one admitted root.
type workCoveragePage struct {
	Uri   string   `json:"uri"`
	Depth int      `json:"depth"`
	Page  sitePage `json:"page"`
	Bytes int64    `json:"bytes"`
}

// Admission offsets are deterministic and independent of scheduler latency.
type workCoverageRoot struct {
	OffsetNs int64              `json:"offset_ns"`
	Pages    []workCoveragePage `json:"pages"`
}

// This digestible inventory is independent of wall time and response success.
type workCoveragePlan struct {
	Kind       string             `json:"kind"`
	DurationNs int64              `json:"duration_ns"`
	TimeoutNs  int64              `json:"timeout_ns"`
	Roots      []workCoverageRoot `json:"roots"`
	TotalPages int64              `json:"total_pages"`
	TotalBytes int64              `json:"total_bytes"`
	Sha256     string             `json:"-"`
}

// Validates sizes before conversions, multiplication, allocation or RNG work.
func validateWorkCoverageConfig(config *Config, duration, timeout time.Duration, limits workCoverageLimits) error {
	if config == nil || duration <= 0 || timeout <= 0 || limits.Roots <= 0 || limits.Pages <= 0 ||
		limits.PagesPerRoot <= 0 || limits.Ticks <= 0 || limits.RandomDraws <= 0 ||
		limits.BodyBytes <= 0 || limits.InventoryBytes <= 0 || limits.ExpectedBytes <= 0 {
		return fmt.Errorf("invalid bounded work inventory settings")
	}
	site := config.Site
	if !finiteNonnegative(site.MeanDepth) || !finiteNonnegative(site.Branching) ||
		float64((limits.PagesPerRoot-1)/siteMaxDepth) < math.Max(1, math.Trunc(site.Branching)) ||
		!finiteNonnegative(config.Clients.MeanPerMinute) ||
		math.Exp(-config.Clients.MeanPerMinute/60) == 0 ||
		float64(limits.Roots) < config.Clients.MeanPerMinute/60 ||
		!finiteNonnegative(site.LargeFraction) || 1 < site.LargeFraction ||
		config.Clients.PoolSize <= 0 || config.Clients.ConnectionsPerCrawl <= 0 ||
		int64(limits.Ticks) < int64(duration/time.Second) {
		return fmt.Errorf("workload exceeds the finite work inventory shape")
	}
	for _, size := range []int{site.MinBodyBytes, site.MaxBodyBytes, site.LargeMinBodyBytes, site.LargeMaxBodyBytes} {
		if size < 0 || limits.BodyBytes < int64(size) {
			return fmt.Errorf("workload body size exceeds inventory bounds")
		}
	}
	if site.MaxBodyBytes < site.MinBodyBytes || site.LargeMaxBodyBytes < site.LargeMinBodyBytes ||
		(0 < site.LargeFraction && site.LargeMinBodyBytes <= 0) {
		return fmt.Errorf("workload has an invalid body tier")
	}
	return nil
}

// Preserves the historical draw sequence but bounds pathological parameters
// and unlucky tails before any candidate is executed.
func buildWorkCoveragePlan(config *Config, duration, timeout time.Duration, limits workCoverageLimits) (*workCoveragePlan, error) {
	if err := validateWorkCoverageConfig(config, duration, timeout, limits); err != nil {
		return nil, err
	}
	draws := 0
	draw := func(source *rng) (float64, error) {
		if draws == limits.RandomDraws {
			return 0, fmt.Errorf("work inventory random-draw bound exceeded")
		}
		draws++
		return source.float64(), nil
	}
	depth := func(source *rng, mean float64) (int, error) {
		if mean <= 0 {
			return 0, nil
		}
		p := mean / (mean + 1)
		for n := 0; ; n++ {
			value, err := draw(source)
			if err != nil || p <= value {
				return n, err
			}
		}
	}
	plan := &workCoveragePlan{Kind: model.CompetitionWorkCoverageKind, DurationNs: int64(duration), TimeoutNs: int64(timeout), Roots: []workCoverageRoot{}}
	handler := &siteHandler{seed: config.Seed, site: config.Site}
	arrivals := newRng(config.Seed ^ 0x5eed)
	metadataBytes := int64(0)
	// Tick zero is not offered by the historical arrival process. Every
	// scheduled tick is strictly inside [measurement start, measurement end).
	for offset := time.Second; offset < duration; {
		threshold, product := math.Exp(-config.Clients.MeanPerMinute/60), 1.0
		count := 0
		if 0 < config.Clients.MeanPerMinute {
			for {
				value, err := draw(arrivals)
				if err != nil {
					return nil, err
				}
				product *= value
				if product <= threshold {
					break
				}
				count++
				if limits.Roots-len(plan.Roots) < count {
					return nil, fmt.Errorf("work inventory offered-root bound exceeded")
				}
			}
		}
		for i := 0; i < count; i++ {
			if len(plan.Roots) == limits.Roots {
				return nil, fmt.Errorf("work inventory offered-root bound exceeded")
			}
			root := workCoverageRoot{OffsetNs: int64(offset), Pages: []workCoveragePage{}}
			queue := []workCoveragePage{{Uri: fmt.Sprintf("/?crawl=%d", len(plan.Roots))}}
			seen := map[string]bool{}
			for len(queue) != 0 {
				item := queue[0]
				queue = queue[1:]
				if seen[item.Uri] {
					continue
				}
				seen[item.Uri] = true
				if len(root.Pages) == limits.PagesPerRoot || plan.TotalPages == int64(limits.Pages) {
					return nil, fmt.Errorf("work inventory page bound exceeded")
				}
				parsed, err := url.ParseRequestURI(item.Uri)
				if err != nil {
					return nil, fmt.Errorf("invalid generated work URI")
				}
				page, ok, err := handler.describePage(parsed.Path, item.Uri, depth)
				if err != nil || !ok {
					return nil, fmt.Errorf("cannot inventory generated work: %v", err)
				}
				header, err := json.Marshal(page)
				if err != nil || int64(len(header))+1 > limits.InventoryBytes || int64(page.Size) > math.MaxInt64-int64(len(header))-1 {
					return nil, fmt.Errorf("work inventory representation overflow")
				}
				item.Page, item.Bytes = page, int64(len(header))+1+int64(page.Size)
				// Include URI, children and fixed-width ledger overhead in the
				// aggregate metadata ceiling, not only the body byte total.
				cost := int64(len(header)) + int64(len(item.Uri)) + 256
				if limits.InventoryBytes-metadataBytes < cost || limits.ExpectedBytes-plan.TotalBytes < item.Bytes {
					return nil, fmt.Errorf("work inventory byte bound exceeded")
				}
				metadataBytes += cost
				plan.TotalPages++
				plan.TotalBytes += item.Bytes
				root.Pages = append(root.Pages, item)
				for _, child := range page.Urls {
					queue = append(queue, workCoveragePage{Uri: child, Depth: item.Depth + 1})
				}
			}
			plan.Roots = append(plan.Roots, root)
		}
		if duration-offset <= time.Second {
			break
		}
		offset += time.Second
	}
	if len(plan.Roots) == 0 || plan.TotalPages <= 0 || plan.TotalBytes <= 0 {
		return nil, fmt.Errorf("work inventory has no positive offered cohort")
	}
	content, err := json.Marshal(plan)
	if err != nil || int64(len(content)) > limits.InventoryBytes {
		return nil, fmt.Errorf("work inventory serialized byte bound exceeded")
	}
	plan.Sha256 = fmt.Sprintf("%x", sha256.Sum256(content))
	return plan, nil
}
