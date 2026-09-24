package work

import (
	"context"
	"iter"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/geo/solve"
	"github.com/urnetwork/server/model"
)

// The whole derive job over a generated day at a fraction of the scale target
// (connect/GEOMAP.md §5.8 item 2, D27), against the local database: the
// target is a million extenders pinging 64 peers eight times a day and a
// million providers, half of them reconnecting and probing 16 extenders, so a
// tenth is 100 000 extenders, 50 000 probing providers, 51.2 million pings
// and 7.2 million terms. GEOMAP_SCALE=1 runs it at a tenth;
// GEOMAP_SCALE_FRACTION runs another fraction, for a host whose disk cannot
// hold a tenth of a day (about 20 GB of network_ping with its indexes).

// The target the fraction is of (GEOMAP §5.3, "Scale"; D26).
const (
	deriveTargetExtenders       = 1000000
	deriveTargetProviders       = 1000000
	deriveTargetExtenderPeers   = 64
	deriveTargetExtenderSamples = 8
	deriveTargetProviderPeers   = 16
)

// The job over a generated day, within the task's cap. The nodes sit at
// jittered cities of the deployment's place list, activated or located at
// those cities, and every round trip is the distance between their true
// positions at the solver's km per millisecond, so the solve does the work a
// real day asks of it.
func TestDeriveLocationsAtAFractionOfTheTarget(t *testing.T) {
	if os.Getenv("GEOMAP_SCALE") != "1" {
		t.Skip("GEOMAP_SCALE=1 runs the derive job over a generated day at a tenth of the target")
	}
	fraction := 0.1
	if value := os.Getenv("GEOMAP_SCALE_FRACTION"); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || !(0 < parsed && parsed <= 1) {
			t.Fatalf("GEOMAP_SCALE_FRACTION=%q is no fraction", value)
		}
		fraction = parsed
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		places := model.CurrentPlaces()
		if places == nil {
			t.Fatal("the deployment's place list is not loaded")
		}
		settings := solve.DefaultSettings()
		jobSettings := DefaultDeriveLocationsSettings()
		now := server.NowUtc()
		extenderCount := int(fraction * deriveTargetExtenders)
		// half the providers reconnect, and a provider probes when it connects
		providerCount := int(fraction * deriveTargetProviders / 2)

		// splitmix64, for a generated day that is the same every run
		hash := func(values ...uint64) uint64 {
			h := uint64(5)
			for _, value := range values {
				h += value + 0x9e3779b97f4a7c15
				h = (h ^ (h >> 30)) * 0xbf58476d1ce4e5b9
				h = (h ^ (h >> 27)) * 0x94d049bb133111eb
				h ^= h >> 31
			}
			return h
		}

		// the sites: cities of the list, spread over it, each with its row
		seedStart := time.Now()
		sites := []*model.Location{}
		siteStride := 0
		for range places.Cities() {
			siteStride += 1
		}
		siteStride = max(1, siteStride/1000)
		i := 0
		for place := range places.Cities() {
			i += 1
			if i%siteStride != 0 {
				continue
			}
			country := places.Country(place.CountryCode)
			if country == nil {
				continue
			}
			location := &model.Location{
				LocationType:     model.LocationTypeCity,
				City:             place.City,
				Region:           place.Region,
				Country:          country.Name,
				CountryCode:      place.CountryCode,
				Latitude:         place.Latitude,
				Longitude:        place.Longitude,
				Timezone:         place.TimeZone,
				CityGeonameId:    place.GeonameId,
				RegionGeonameId:  place.RegionGeonameId,
				CountryGeonameId: country.GeonameId,
			}
			model.CreateLocation(ctx, location)
			if location.LocationType == model.LocationTypeCity && location.CityLocationId != (server.Id{}) {
				sites = append(sites, location)
			}
		}
		if len(sites) < 100 {
			t.Fatalf("only %d sites", len(sites))
		}

		// every node at a site, a quarter degree of jitter from it
		type node struct {
			id       server.Id
			site     *model.Location
			position solve.LatLon
		}
		placeNode := func(kind uint64, index int) node {
			site := sites[hash(kind, uint64(index))%uint64(len(sites))]
			return node{
				id:   server.NewId(),
				site: site,
				position: solve.LatLon{
					Latitude:  max(-89.9, min(89.9, site.Latitude+(float64(hash(kind, uint64(index), 1)%500)/1000-0.25))),
					Longitude: site.Longitude + (float64(hash(kind, uint64(index), 2)%500)/1000 - 0.25),
				},
			}
		}
		extenders := make([]node, extenderCount)
		for i := range extenders {
			extenders[i] = placeNode(1, i)
		}
		providers := make([]node, providerCount)
		for i := range providers {
			providers[i] = placeNode(2, i)
		}

		// the extenders' activations and the providers' located connections,
		// copied in bulk
		copyRows := func(table string, columns []string, rowCount int, row func(i int) []any) {
			index := 0
			server.Db(ctx, func(conn server.PgConn) {
				copied, err := conn.CopyFrom(ctx, pgx.Identifier{table}, columns, pgx.CopyFromFunc(func() ([]any, error) {
					if rowCount <= index {
						return nil, nil
					}
					index += 1
					return row(index - 1), nil
				}))
				server.Raise(err)
				connect.AssertEqual(t, copied, int64(rowCount))
			})
		}
		accuracyKm := float32(25)
		copyRows(
			"network_extender_activation",
			[]string{"activation_id", "extender_id", "activate_time", "ip_version", "country_code", "location_id", "city_location_id", "region_location_id", "country_location_id", "accuracy_km"},
			extenderCount,
			func(i int) []any {
				site := extenders[i].site
				return []any{server.NewId(), extenders[i].id, now.Add(-time.Hour), 4, site.CountryCode, site.LocationId, site.CityLocationId, site.RegionLocationId, site.CountryLocationId, accuracyKm}
			},
		)
		connectionIds := make([]server.Id, providerCount)
		for i := range connectionIds {
			connectionIds[i] = server.NewId()
		}
		copyRows(
			"network_client_connection",
			[]string{"client_id", "connection_id", "connected", "connect_time", "connection_host", "connection_service", "connection_block"},
			providerCount,
			func(i int) []any {
				return []any{providers[i].id, connectionIds[i], true, now.Add(-time.Hour), "scale-test", "scale-test", "scale-test"}
			},
		)
		providerAccuracyKm := float32(100)
		copyRows(
			"network_client_location",
			[]string{"connection_id", "client_id", "city_location_id", "region_location_id", "country_location_id", "genesis_location_id", "accuracy_km"},
			providerCount,
			func(i int) []any {
				site := providers[i].site
				return []any{connectionIds[i], providers[i].id, site.CityLocationId, site.RegionLocationId, site.CountryLocationId, site.LocationId, providerAccuracyKm}
			},
		)

		// the day: every extender pings 64 peers spread over the fleet eight
		// times, in rounds, and every probing provider 16 extenders once
		rttMs := func(from node, to node, round int, pinger int, target int) int {
			noise := float64(hash(3, uint64(pinger), uint64(target), uint64(round)) % 4)
			return int(solve.DistanceKm(from.position, to.position)/settings.KmPerMs + settings.OverheadMs + noise)
		}
		extenderPeerStride := max(1, extenderCount/deriveTargetExtenderPeers)
		providerPeerStride := max(1, extenderCount/deriveTargetProviderPeers)
		rows := func(yield func(*model.NetworkPingTerm) bool) {
			for round := range deriveTargetExtenderSamples {
				for i := range extenders {
					for j := range deriveTargetExtenderPeers {
						target := (i + 1 + j*extenderPeerStride) % extenderCount
						if target == i {
							continue
						}
						if !yield(&model.NetworkPingTerm{
							PingerKind:       model.NetworkPingPingerKindExtender,
							PingerId:         extenders[i].id,
							TargetExtenderId: extenders[target].id,
							RttMs:            rttMs(extenders[i], extenders[target], round, i, target),
						}) {
							return
						}
					}
				}
				if round != 0 {
					continue
				}
				for i := range providers {
					for j := range deriveTargetProviderPeers {
						target := (int(hash(4, uint64(i))%uint64(extenderCount)) + j*providerPeerStride) % extenderCount
						if !yield(&model.NetworkPingTerm{
							PingerKind:       model.NetworkPingPingerKindProvider,
							PingerId:         providers[i].id,
							TargetExtenderId: extenders[target].id,
							RttMs:            rttMs(providers[i], extenders[target], 0, extenderCount+i, target),
						}) {
							return
						}
					}
				}
			}
		}
		rowCount := 0
		for range iter.Seq[*model.NetworkPingTerm](rows) {
			rowCount += 1
		}
		copied := deriveTestCopyPings(t, ctx, rows, rowCount, now, 23*time.Hour)
		connect.AssertEqual(t, copied, int64(rowCount))
		t.Logf(
			"fraction %g of the target: %d extenders, %d probing providers, %d sites; %d pings seeded in %.1fs",
			fraction, extenderCount, providerCount, len(sites), rowCount, time.Since(seedStart).Seconds(),
		)

		start := time.Now()
		result, err := deriveLocations(ctx, places, settings, jobSettings, now)
		if err != nil {
			t.Fatal(err)
		}
		jobSeconds := time.Since(start).Seconds()
		run, ok := model.GetDeriveLocationsRun(ctx)
		if !ok {
			t.Fatal("the run was not recorded")
		}
		t.Logf(
			"the job: %.1fs of MaxTime=%s at %d cores; read %d pings into %d terms over %d cursors in %.1fs; solved %d nodes in %.1fs with sweeps %v (converged %t); peak heap %.2f GiB; published %d, excluded %d; residual %.2f km against %.2f km at genesis",
			jobSeconds, jobSettings.MaxTime, runtime.GOMAXPROCS(0),
			result.CosignedPings, result.Terms, run.ReadCursors, result.IngestSeconds,
			result.Nodes, result.SolveSeconds, result.Sweeps, result.Converged,
			float64(run.PeakBytes)/(1024*1024*1024),
			result.Published, result.ExcludedSources,
			result.ResidualKm, result.GenesisResidualKm,
		)
		t.Logf(
			"measured: %.3g s a term sweep, %.3g s a node sweep, %.0f bytes a term, %.0f bytes a node; the next run projected at %.1fs of MaxSolveSeconds=%.0fs and %.2f GiB of MaxSolveBytes=%.0f GiB",
			run.SecondsPerTermSweep, run.SecondsPerNodeSweep, run.BytesPerTerm, run.BytesPerNode,
			run.ProjectedSeconds, run.MaxSolveSeconds,
			float64(run.ProjectedBytes)/(1024*1024*1024), float64(run.MaxSolveBytes)/(1024*1024*1024),
		)
		// the target itself, from this run's unit costs at this host's cores
		target := projectDeriveRun(jobSettings, &deriveRunCosts{
			terms:               int(float64(run.Terms) / fraction),
			nodes:               int(float64(run.Nodes) / fraction),
			sweeps:              run.Sweeps,
			cores:               run.Cores,
			secondsPerTermSweep: run.SecondsPerTermSweep,
			secondsPerNodeSweep: run.SecondsPerNodeSweep,
			bytesPerTerm:        run.BytesPerTerm,
			bytesPerNode:        run.BytesPerNode,
		}, run.Cores)
		t.Logf(
			"extrapolated to the target on this host: %d terms, %d nodes -> %.0fs, %.2f GiB",
			target.terms, target.nodes, target.seconds, float64(target.bytes)/(1024*1024*1024),
		)

		connect.AssertEqual(t, result.CosignedPings, rowCount)
		if !(jobSeconds < jobSettings.MaxTime.Seconds()) {
			t.Fatalf("the job took %.1fs, past the task's cap of %s", jobSeconds, jobSettings.MaxTime)
		}
		if result.Nodes == 0 || result.Published == 0 {
			t.Fatalf("the job solved %d nodes and published %d", result.Nodes, result.Published)
		}
		if !(result.ResidualKm < result.GenesisResidualKm) {
			t.Fatalf("the derivation did not improve on genesis: %.2f km against %.2f km", result.ResidualKm, result.GenesisResidualKm)
		}
		// the sweep's window is the pings' own life
		connect.AssertEqual(t, controller.DefaultExtenderPingReportSettings().Retention, 24*time.Hour)
	})
}
