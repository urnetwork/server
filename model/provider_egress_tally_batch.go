// Coalesces the finite immutable pages of one owner drain before opening the
// shared-row critical section. The caller commits these increments with its
// durable stream cursor.
package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
)

// One normalized daily-place increment; all counters retain their original
// whole-run or per-site meanings, including the independent canary counts.
type ProviderEgressTallyPlaceDelta struct {
	Day          time.Time           `json:"day"`
	Place        ProviderEgressPlace `json:"place"`
	Runs         int                 `json:"runs"`
	HealthyRuns  int                 `json:"healthy_runs"`
	EchoFailures int                 `json:"echo_failures"`
}

type ProviderEgressTallySiteDelta struct {
	Day             time.Time           `json:"day"`
	Place           ProviderEgressPlace `json:"place"`
	Name            string              `json:"name"`
	Loads           int                 `json:"loads"`
	Failures        int                 `json:"failures"`
	HealthyLoads    int                 `json:"healthy_loads"`
	HealthyFailures int                 `json:"healthy_failures"`
	CanaryLoads     int                 `json:"canary_loads"`
	CanaryPasses    int                 `json:"canary_passes"`
}

type ProviderEgressTallyBatch struct {
	Places []ProviderEgressTallyPlaceDelta `json:"places"`
	Sites  []ProviderEgressTallySiteDelta  `json:"sites"`
}

// No I/O occurs while normalizing and coalescing. The drain bound limits both
// memory and SQL work, even if every record uses different sites and places.
func prepareProviderEgressTallyBatch(records []ProviderEgressTallyRecord) (*ProviderEgressTallyBatch, error) {
	if len(records) > providerEgressTallyDrainLength {
		return nil, fmt.Errorf("tally batch exceeds record bound")
	}
	coalescer := newProviderEgressTallyCoalescer()
	if err := coalescer.add(records); err != nil {
		return nil, err
	}
	return coalescer.batch(), nil
}

type providerEgressTallyPlaceKey struct {
	day             time.Time
	country, region string
}

type providerEgressTallySiteKey struct {
	place providerEgressTallyPlaceKey
	name  string
}

// Accumulates whole pages of one drain into one batch of shared-row deltas.
// The rows a candidate page would add are known before it is added, so a
// drain can stop on a page boundary instead of splitting a page.
type providerEgressTallyCoalescer struct {
	placeDeltas map[providerEgressTallyPlaceKey]*ProviderEgressTallyPlaceDelta
	siteDeltas  map[providerEgressTallySiteKey]*ProviderEgressTallySiteDelta
}

func newProviderEgressTallyCoalescer() *providerEgressTallyCoalescer {
	return &providerEgressTallyCoalescer{
		placeDeltas: map[providerEgressTallyPlaceKey]*ProviderEgressTallyPlaceDelta{},
		siteDeltas:  map[providerEgressTallySiteKey]*ProviderEgressTallySiteDelta{},
	}
}

// Normalizes a record to its daily place, as the direct SQL writer does.
func providerEgressTallyRecordPlace(record ProviderEgressTallyRecord) (providerEgressTallyPlaceKey, ProviderEgressPlace, error) {
	place, err := providerEgressTallyPlace(record.Run.Place)
	if err != nil || record.MeasuredAt.IsZero() || len(record.Loads) > 512 {
		return providerEgressTallyPlaceKey{}, ProviderEgressPlace{}, fmt.Errorf("invalid retained tally")
	}
	return providerEgressTallyPlaceKey{day: record.MeasuredAt.UTC().Truncate(24 * time.Hour), country: place.CountryCode, region: place.Region}, place, nil
}

// Empty and over-long names are ignored, as the direct SQL writer does.
func providerEgressTallyLoadName(load ProviderEgressSiteLoad) (string, bool) {
	name := strings.TrimSpace(load.Name)
	return name, name != "" && len(name) <= 128
}

// Place and site rows the batch would hold after adding records. Pure: the
// batch is unchanged, and an invalid record is reported without adding any.
func (self *providerEgressTallyCoalescer) rowsWith(records []ProviderEgressTallyRecord) (int, error) {
	newPlaceKeys := map[providerEgressTallyPlaceKey]bool{}
	newSiteKeys := map[providerEgressTallySiteKey]bool{}
	for _, record := range records {
		key, _, err := providerEgressTallyRecordPlace(record)
		if err != nil {
			return 0, err
		}
		if self.placeDeltas[key] == nil {
			newPlaceKeys[key] = true
		}
		for _, load := range record.Loads {
			if name, ok := providerEgressTallyLoadName(load); ok {
				siteKey := providerEgressTallySiteKey{place: key, name: name}
				if self.siteDeltas[siteKey] == nil {
					newSiteKeys[siteKey] = true
				}
			}
		}
	}
	return len(self.placeDeltas) + len(self.siteDeltas) + len(newPlaceKeys) + len(newSiteKeys), nil
}

// Adds whole records; every record is validated before any counter changes.
func (self *providerEgressTallyCoalescer) add(records []ProviderEgressTallyRecord) error {
	if _, err := self.rowsWith(records); err != nil {
		return err
	}
	for _, record := range records {
		key, place, _ := providerEgressTallyRecordPlace(record)
		p := self.placeDeltas[key]
		if p == nil {
			p = &ProviderEgressTallyPlaceDelta{Day: key.day, Place: place}
			self.placeDeltas[key] = p
		}
		p.Runs++
		if record.Run.Healthy {
			p.HealthyRuns++
		}
		if record.Run.EchoFailed {
			p.EchoFailures++
		}
		for _, load := range record.Loads {
			name, ok := providerEgressTallyLoadName(load)
			if !ok {
				continue
			}
			siteKey := providerEgressTallySiteKey{place: key, name: name}
			s := self.siteDeltas[siteKey]
			if s == nil {
				s = &ProviderEgressTallySiteDelta{Day: key.day, Place: place, Name: name}
				self.siteDeltas[siteKey] = s
			}
			if load.Canary {
				s.CanaryLoads++
				if load.Ok {
					s.CanaryPasses++
				}
				continue
			}
			s.Loads++
			if !load.Ok {
				s.Failures++
			}
			if load.Healthy {
				s.HealthyLoads++
				if !load.Ok {
					s.HealthyFailures++
				}
			}
		}
	}
	return nil
}

// A sorted copy in the shared rows' primary-key order.
func (self *providerEgressTallyCoalescer) batch() *ProviderEgressTallyBatch {
	batch := &ProviderEgressTallyBatch{}
	for _, p := range self.placeDeltas {
		batch.Places = append(batch.Places, *p)
	}
	for _, s := range self.siteDeltas {
		batch.Sites = append(batch.Sites, *s)
	}
	lessPlace := func(day1, day2 time.Time, p1, p2 ProviderEgressPlace) bool {
		if !day1.Equal(day2) {
			return day1.Before(day2)
		}
		if p1.CountryCode != p2.CountryCode {
			return p1.CountryCode < p2.CountryCode
		}
		return p1.Region < p2.Region
	}
	sort.Slice(batch.Places, func(i, j int) bool {
		a, b := batch.Places[i], batch.Places[j]
		return lessPlace(a.Day, b.Day, a.Place, b.Place)
	})
	// Match the site's primary-key order. All rollups use the same order;
	// stable place shards prevent rollups competing for one daily-place row.
	sort.Slice(batch.Sites, func(i, j int) bool {
		a, b := batch.Sites[i], batch.Sites[j]
		if !a.Day.Equal(b.Day) {
			return a.Day.Before(b.Day)
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return lessPlace(a.Day, b.Day, a.Place, b.Place)
	})
	return batch
}

// Pure validation can precede every mutation of the durable checkpoint. The
// generic Post retry converts a plain panic into an error without aborting SQL.
func ValidateProviderEgressTallyBatch(values *ProviderEgressTallyBatch) {
	if values == nil || len(values.Places) > providerEgressTallyDrainLength || len(values.Sites) > providerEgressTallyDrainLength*512 {
		panic("invalid tally batch bounds")
	}
	// Refuse malformed retained results before taking any shared tally row.
	// A drain represents at most one full shard of whole runs, regardless of
	// its site count.
	type placeKey struct {
		day   time.Time
		place ProviderEgressPlace
	}
	placeRuns := map[placeKey]int{}
	totalRuns := 0
	for _, p := range values.Places {
		place, err := providerEgressTallyPlace(p.Place)
		key := placeKey{day: p.Day, place: p.Place}
		if err != nil || place != p.Place || !p.Day.Equal(p.Day.UTC().Truncate(24*time.Hour)) || p.Day.IsZero() || p.Runs < 1 || p.Runs > providerEgressTallyDrainLength || p.HealthyRuns < 0 || p.HealthyRuns > p.Runs || p.EchoFailures < 0 || p.EchoFailures > p.Runs || placeRuns[key] != 0 {
			panic("invalid tally place delta")
		}
		placeRuns[key] = p.Runs
		totalRuns += p.Runs
	}
	if totalRuns > providerEgressTallyDrainLength {
		panic("invalid tally page run count")
	}
	type siteKey struct {
		place placeKey
		name  string
	}
	siteKeys := map[siteKey]bool{}
	placeLoads := map[placeKey]int{}
	for _, s := range values.Sites {
		pk := placeKey{day: s.Day, place: s.Place}
		sk := siteKey{place: pk, name: s.Name}
		if placeRuns[pk] == 0 || siteKeys[sk] || s.Name == "" || strings.TrimSpace(s.Name) != s.Name || len(s.Name) > 128 || !utf8.ValidString(s.Name) || s.Loads < 0 || s.Loads > providerEgressTallyDrainLength*512 || s.Failures < 0 || s.Failures > s.Loads || s.HealthyLoads < 0 || s.HealthyLoads > s.Loads || s.HealthyFailures < 0 || s.HealthyFailures > min(s.Failures, s.HealthyLoads) || s.CanaryLoads < 0 || s.CanaryLoads > providerEgressTallyDrainLength*512 || s.CanaryPasses < 0 || s.CanaryPasses > s.CanaryLoads {
			panic("invalid tally site delta")
		}
		siteKeys[sk] = true
		placeLoads[pk] += s.Loads + s.CanaryLoads
		if placeLoads[pk] > placeRuns[pk]*512 {
			panic("invalid tally page load count")
		}
	}
}

// Applies precomputed increments in the caller's finishing transaction. No
// Redis call or post-commit cleanup is allowed while any tally row is held.
func AddProviderEgressTallyBatchInTx(ctx context.Context, tx server.PgTx, values *ProviderEgressTallyBatch) {
	ValidateProviderEgressTallyBatch(values)
	now := server.NowUtc()
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, p := range values.Places {
			batch.Queue(`INSERT INTO provider_egress_place_tally
			(tally_day,country_code,region,run_count,healthy_run_count,echo_failure_count,update_time)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(tally_day,country_code,region) DO UPDATE SET
			run_count=provider_egress_place_tally.run_count+EXCLUDED.run_count,
			healthy_run_count=provider_egress_place_tally.healthy_run_count+EXCLUDED.healthy_run_count,
			echo_failure_count=provider_egress_place_tally.echo_failure_count+EXCLUDED.echo_failure_count,
			update_time=EXCLUDED.update_time`, p.Day, p.Place.CountryCode, p.Place.Region, p.Runs, p.HealthyRuns, p.EchoFailures, now)
		}
		for _, s := range values.Sites {
			batch.Queue(`INSERT INTO provider_egress_site_tally
			(tally_day,name,country_code,region,load_count,failure_count,healthy_load_count,healthy_failure_count,canary_load_count,canary_pass_count,update_time)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT(tally_day,name,country_code,region) DO UPDATE SET
			load_count=provider_egress_site_tally.load_count+EXCLUDED.load_count,
			failure_count=provider_egress_site_tally.failure_count+EXCLUDED.failure_count,
			healthy_load_count=provider_egress_site_tally.healthy_load_count+EXCLUDED.healthy_load_count,
			healthy_failure_count=provider_egress_site_tally.healthy_failure_count+EXCLUDED.healthy_failure_count,
			canary_load_count=provider_egress_site_tally.canary_load_count+EXCLUDED.canary_load_count,
			canary_pass_count=provider_egress_site_tally.canary_pass_count+EXCLUDED.canary_pass_count,
			update_time=EXCLUDED.update_time`, s.Day, s.Name, s.Place.CountryCode, s.Place.Region, s.Loads, s.Failures, s.HealthyLoads, s.HealthyFailures, s.CanaryLoads, s.CanaryPasses, now)
		}
	})
}
