package model

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Derived locations (connect/GEOMAP.md §5.4, §5.7, §6): where the derive phase
// placed a provider or an extender from the pings it co-signed, published only
// when the placement improves on the node's genesis. One row per node, written
// whole by each derivation -- a node the derivation does not publish has no
// row, and keeps its genesis -- and swept a day after it was written, so a node
// that stops pinging falls back to its genesis on its own.
//
// A row is read at connection time and at record signing (§6): its presence is
// its freshness, because nothing older than the sweep's cut can be present.

// The kind of node a row places. Stored values, the same numbering as the
// pinger kinds (GEOMAP §5.4).
const (
	DerivedLocationNodeKindProvider = 1
	DerivedLocationNodeKindExtender = 2
)

// One published derivation.
type DerivedLocation struct {
	NodeKind int
	// the provider's client id, or the extender's extender id
	NodeId server.Id
	// the genesis the node was solved from, and the accuracy radius it was
	// anchored with, in km
	GenesisLatitude   float64
	GenesisLongitude  float64
	GenesisAccuracyKm float32
	// the correction as a latitude and longitude delta, in degrees:
	// Latitude = GenesisLatitude + DeltaLatitude, and Longitude likewise modulo
	// 360
	DeltaLatitude  float64
	DeltaLongitude float64
	Latitude       float64
	Longitude      float64
	// the co-signed pings and distinct peers behind the correction, the RMS
	// ping residual with it, in km, and the node's reputation as a source
	// (§5.5), from QMin to 1
	PingCount  int
	PeerCount  int
	ResidualKm float32
	Reputation float32
	// whether the place the derived point maps to (§6) lies outside the
	// genesis region or country
	CrossedRegion  bool
	CrossedCountry bool
	// the place the derived point maps to: its city row, and that row's city,
	// region and country
	LocationId        server.Id
	CityLocationId    server.Id
	RegionLocationId  server.Id
	CountryLocationId server.Id
	UpdateTime        time.Time

	// Read only: the country code of CountryLocationId, lower case, which the
	// readers join for the paths that publish a country (§6).
	CountryCode string
}

// The rows of one statement of ReplaceDerivedLocations. Each is twenty array
// elements, so a batch of this many is a few MB of parameters.
const derivedLocationWriteBatchSize = 10000

// Makes the table exactly the given rows, in one transaction: every row is
// written, whatever the table held for its node, and every row for a node not
// given is deleted. A derivation is a whole answer, never a patch on the last
// one: a node it does not publish keeps its genesis (§5.4), and readers see the
// previous derivation or this one, never half of each. Returns the rows written
// and the rows removed.
func ReplaceDerivedLocations(ctx context.Context, derivedLocations []*DerivedLocation) (written int, removed int) {
	server.Tx(ctx, func(tx server.PgTx) {
		written = 0
		removed = 0
		nodeKinds := make([]int16, 0, len(derivedLocations))
		nodeIds := make([]server.Id, 0, len(derivedLocations))
		for start := 0; start < len(derivedLocations); start += derivedLocationWriteBatchSize {
			batch := derivedLocations[start:min(start+derivedLocationWriteBatchSize, len(derivedLocations))]
			batchNodeKinds := make([]int16, 0, len(batch))
			batchNodeIds := make([]server.Id, 0, len(batch))
			genesisLatitudes := make([]float64, 0, len(batch))
			genesisLongitudes := make([]float64, 0, len(batch))
			genesisAccuracyKms := make([]float32, 0, len(batch))
			deltaLatitudes := make([]float64, 0, len(batch))
			deltaLongitudes := make([]float64, 0, len(batch))
			latitudes := make([]float64, 0, len(batch))
			longitudes := make([]float64, 0, len(batch))
			pingCounts := make([]int32, 0, len(batch))
			peerCounts := make([]int32, 0, len(batch))
			residualKms := make([]float32, 0, len(batch))
			reputations := make([]float32, 0, len(batch))
			crossedRegions := make([]bool, 0, len(batch))
			crossedCountries := make([]bool, 0, len(batch))
			locationIds := make([]server.Id, 0, len(batch))
			cityLocationIds := make([]server.Id, 0, len(batch))
			regionLocationIds := make([]server.Id, 0, len(batch))
			countryLocationIds := make([]server.Id, 0, len(batch))
			updateTimes := make([]time.Time, 0, len(batch))
			for _, derivedLocation := range batch {
				batchNodeKinds = append(batchNodeKinds, int16(derivedLocation.NodeKind))
				batchNodeIds = append(batchNodeIds, derivedLocation.NodeId)
				genesisLatitudes = append(genesisLatitudes, derivedLocation.GenesisLatitude)
				genesisLongitudes = append(genesisLongitudes, derivedLocation.GenesisLongitude)
				genesisAccuracyKms = append(genesisAccuracyKms, derivedLocation.GenesisAccuracyKm)
				deltaLatitudes = append(deltaLatitudes, derivedLocation.DeltaLatitude)
				deltaLongitudes = append(deltaLongitudes, derivedLocation.DeltaLongitude)
				latitudes = append(latitudes, derivedLocation.Latitude)
				longitudes = append(longitudes, derivedLocation.Longitude)
				pingCounts = append(pingCounts, int32(derivedLocation.PingCount))
				peerCounts = append(peerCounts, int32(derivedLocation.PeerCount))
				residualKms = append(residualKms, derivedLocation.ResidualKm)
				reputations = append(reputations, derivedLocation.Reputation)
				crossedRegions = append(crossedRegions, derivedLocation.CrossedRegion)
				crossedCountries = append(crossedCountries, derivedLocation.CrossedCountry)
				locationIds = append(locationIds, derivedLocation.LocationId)
				cityLocationIds = append(cityLocationIds, derivedLocation.CityLocationId)
				regionLocationIds = append(regionLocationIds, derivedLocation.RegionLocationId)
				countryLocationIds = append(countryLocationIds, derivedLocation.CountryLocationId)
				updateTimes = append(updateTimes, derivedLocation.UpdateTime.UTC())
			}
			tag, err := tx.Exec(
				ctx,
				`
				INSERT INTO derived_location (
					node_kind,
					node_id,
					genesis_latitude,
					genesis_longitude,
					genesis_accuracy_km,
					delta_latitude,
					delta_longitude,
					latitude,
					longitude,
					ping_count,
					peer_count,
					residual_km,
					reputation,
					crossed_region,
					crossed_country,
					location_id,
					city_location_id,
					region_location_id,
					country_location_id,
					update_time
				)
				SELECT * FROM unnest(
					$1::smallint[],
					$2::uuid[],
					$3::double precision[],
					$4::double precision[],
					$5::real[],
					$6::double precision[],
					$7::double precision[],
					$8::double precision[],
					$9::double precision[],
					$10::integer[],
					$11::integer[],
					$12::real[],
					$13::real[],
					$14::boolean[],
					$15::boolean[],
					$16::uuid[],
					$17::uuid[],
					$18::uuid[],
					$19::uuid[],
					$20::timestamp[]
				)
				ON CONFLICT (node_kind, node_id) DO UPDATE
				SET
					genesis_latitude = EXCLUDED.genesis_latitude,
					genesis_longitude = EXCLUDED.genesis_longitude,
					genesis_accuracy_km = EXCLUDED.genesis_accuracy_km,
					delta_latitude = EXCLUDED.delta_latitude,
					delta_longitude = EXCLUDED.delta_longitude,
					latitude = EXCLUDED.latitude,
					longitude = EXCLUDED.longitude,
					ping_count = EXCLUDED.ping_count,
					peer_count = EXCLUDED.peer_count,
					residual_km = EXCLUDED.residual_km,
					reputation = EXCLUDED.reputation,
					crossed_region = EXCLUDED.crossed_region,
					crossed_country = EXCLUDED.crossed_country,
					location_id = EXCLUDED.location_id,
					city_location_id = EXCLUDED.city_location_id,
					region_location_id = EXCLUDED.region_location_id,
					country_location_id = EXCLUDED.country_location_id,
					update_time = EXCLUDED.update_time
				`,
				batchNodeKinds,
				batchNodeIds,
				genesisLatitudes,
				genesisLongitudes,
				genesisAccuracyKms,
				deltaLatitudes,
				deltaLongitudes,
				latitudes,
				longitudes,
				pingCounts,
				peerCounts,
				residualKms,
				reputations,
				crossedRegions,
				crossedCountries,
				locationIds,
				cityLocationIds,
				regionLocationIds,
				countryLocationIds,
				updateTimes,
			)
			server.Raise(err)
			written += int(tag.RowsAffected())
			nodeKinds = append(nodeKinds, batchNodeKinds...)
			nodeIds = append(nodeIds, batchNodeIds...)
		}

		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM derived_location
			WHERE NOT EXISTS (
				SELECT 1
				FROM unnest($1::smallint[], $2::uuid[]) AS kept(node_kind, node_id)
				WHERE
					kept.node_kind = derived_location.node_kind AND
					kept.node_id = derived_location.node_id
			)
			`,
			nodeKinds,
			nodeIds,
		)
		server.Raise(err)
		removed = int(tag.RowsAffected())
	})
	return written, removed
}

// The columns every reader selects, from `derived_location` and the `country`
// row it names, in scanDerivedLocation's order.
const derivedLocationSelect = `
	derived_location.node_kind,
	derived_location.node_id,
	derived_location.genesis_latitude,
	derived_location.genesis_longitude,
	derived_location.genesis_accuracy_km,
	derived_location.delta_latitude,
	derived_location.delta_longitude,
	derived_location.latitude,
	derived_location.longitude,
	derived_location.ping_count,
	derived_location.peer_count,
	derived_location.residual_km,
	derived_location.reputation,
	derived_location.crossed_region,
	derived_location.crossed_country,
	derived_location.location_id,
	derived_location.city_location_id,
	derived_location.region_location_id,
	derived_location.country_location_id,
	derived_location.update_time,
	country.country_code
`

// One row of derivedLocationSelect, from the result's current row.
func scanDerivedLocation(result server.PgResult) *DerivedLocation {
	derivedLocation := &DerivedLocation{}
	// the country row is joined, and a row a merge deleted reads as none
	var countryCode *string
	server.Raise(result.Scan(
		&derivedLocation.NodeKind,
		&derivedLocation.NodeId,
		&derivedLocation.GenesisLatitude,
		&derivedLocation.GenesisLongitude,
		&derivedLocation.GenesisAccuracyKm,
		&derivedLocation.DeltaLatitude,
		&derivedLocation.DeltaLongitude,
		&derivedLocation.Latitude,
		&derivedLocation.Longitude,
		&derivedLocation.PingCount,
		&derivedLocation.PeerCount,
		&derivedLocation.ResidualKm,
		&derivedLocation.Reputation,
		&derivedLocation.CrossedRegion,
		&derivedLocation.CrossedCountry,
		&derivedLocation.LocationId,
		&derivedLocation.CityLocationId,
		&derivedLocation.RegionLocationId,
		&derivedLocation.CountryLocationId,
		&derivedLocation.UpdateTime,
		&countryCode,
	))
	if countryCode != nil {
		derivedLocation.CountryCode = strings.ToLower(strings.TrimSpace(*countryCode))
	}
	return derivedLocation
}

// Every row, which is at most one per node that pinged in the last day. The
// derive phase warm starts each node from the position its row holds.
func GetDerivedLocations(ctx context.Context) []*DerivedLocation {
	derivedLocations := []*DerivedLocation{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+derivedLocationSelect+`
			FROM derived_location
			LEFT JOIN location AS country ON country.location_id = derived_location.country_location_id
			ORDER BY derived_location.node_kind, derived_location.node_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				derivedLocations = append(derivedLocations, scanDerivedLocation(result))
			}
		})
	})
	return derivedLocations
}

// One node's row, or nil.
func GetDerivedLocation(ctx context.Context, nodeKind int, nodeId server.Id) *DerivedLocation {
	var derivedLocation *DerivedLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+derivedLocationSelect+`
			FROM derived_location
			LEFT JOIN location AS country ON country.location_id = derived_location.country_location_id
			WHERE derived_location.node_kind = $1 AND derived_location.node_id = $2
			`,
			nodeKind,
			nodeId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				derivedLocation = scanDerivedLocation(result)
			}
		})
	})
	return derivedLocation
}

// The row of the provider a connection belongs to, or nil, in one query from
// the connection, for the connect-announce path (SetConnectionLocation), which
// already spends one round trip on the egress probe.
func GetDerivedLocationForConnection(ctx context.Context, connectionId server.Id) *DerivedLocation {
	var derivedLocation *DerivedLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+derivedLocationSelect+`
			FROM network_client_connection
			INNER JOIN derived_location ON
				derived_location.node_kind = $2 AND
				derived_location.node_id = network_client_connection.client_id
			LEFT JOIN location AS country ON country.location_id = derived_location.country_location_id
			WHERE network_client_connection.connection_id = $1
			`,
			connectionId,
			DerivedLocationNodeKindProvider,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				derivedLocation = scanDerivedLocation(result)
			}
		})
	})
	return derivedLocation
}

// The country of an extender's derived location, lower case, or empty when it
// has none.
func derivedCountryCodeInTx(ctx context.Context, tx server.PgTx, extenderId server.Id) string {
	var countryCode string
	result, err := tx.Query(
		ctx,
		`
		SELECT country.country_code
		FROM derived_location
		INNER JOIN location AS country ON country.location_id = derived_location.country_location_id
		WHERE derived_location.node_kind = $1 AND derived_location.node_id = $2
		`,
		DerivedLocationNodeKindExtender,
		extenderId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&countryCode))
		}
	})
	return strings.ToLower(strings.TrimSpace(countryCode))
}

// The retention sweep (§5.7): rows written before `minUpdateTime` go.
func RemoveExpiredDerivedLocations(ctx context.Context, minUpdateTime time.Time) (removed int) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM derived_location
			WHERE update_time < $1
			`,
			minUpdateTime.UTC(),
		)
		server.Raise(err)
		removed = int(tag.RowsAffected())
	})
	return removed
}

// The dashboard counts of the published rows (grafana/dashboards/extenders.json).
type DerivedLocationCount struct {
	// every node kind, zero when none
	NodeKinds []DerivedLocationNodeKindCount
	// the rows whose mapped place lies outside the genesis region, and outside
	// the genesis country; a country crossing is also a region crossing
	CrossedRegion  int64
	CrossedCountry int64
}

// The published rows of one node kind.
type DerivedLocationNodeKindCount struct {
	NodeKind int
	Count    int64
}

// The node kinds every count carries, in publication order, so a kind with no
// rows is a zero rather than an absent series.
var DerivedLocationNodeKinds = []int{
	DerivedLocationNodeKindProvider,
	DerivedLocationNodeKindExtender,
}

// Counts the published rows by node kind and crossing, in one scan of a table
// of at most one row per node.
func CountDerivedLocations(ctx context.Context) DerivedLocationCount {
	count := DerivedLocationCount{}
	nodeKindCounts := map[int]int64{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				node_kind,
				COUNT(*),
				COUNT(*) FILTER (WHERE crossed_region),
				COUNT(*) FILTER (WHERE crossed_country)
			FROM derived_location
			GROUP BY node_kind
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var nodeKind int
				var rows int64
				var crossedRegion int64
				var crossedCountry int64
				server.Raise(result.Scan(
					&nodeKind,
					&rows,
					&crossedRegion,
					&crossedCountry,
				))
				nodeKindCounts[nodeKind] = rows
				count.CrossedRegion += crossedRegion
				count.CrossedCountry += crossedCountry
			}
		})
	})
	for _, nodeKind := range DerivedLocationNodeKinds {
		count.NodeKinds = append(count.NodeKinds, DerivedLocationNodeKindCount{
			NodeKind: nodeKind,
			Count:    nodeKindCounts[nodeKind],
		})
	}
	return count
}

// What one derivation did, as the dashboard and the monitor read it. Only the
// derivation knows these numbers -- the table holds
// what it published, not what it left out or how well the solve fit -- and the
// derivation runs every eight hours on whichever taskworker claims it, so each
// run records them where every reader sees the same answer, rather than in its
// own process, where a restart would lose them and a host that ran an earlier
// derivation would keep publishing that one's.
type DeriveLocationsRun struct {
	RunTime time.Time `json:"run_time"`
	// the nodes solved, those of them with terms of their own (the sources
	// reputation scores), and the terms they were solved on
	Nodes   int `json:"nodes"`
	Sources int `json:"sources"`
	Terms   int `json:"terms"`
	// the co-signed direct pings the derivation read: its supply, which a fall
	// in the published count is judged against
	CosignedPings int `json:"cosigned_pings"`
	// the rows published, and the sources the final solve left out on
	// reputation (§5.5)
	Published       int `json:"published"`
	ExcludedSources int `json:"excluded_sources"`
	// the RMS ping residual over the final solve's terms, in km, at the
	// derived positions and at genesis
	ResidualKm        float64 `json:"residual_km"`
	GenesisResidualKm float64 `json:"genesis_residual_km"`
	// whether the last reputation round stopped before the sweep cap, whether
	// that stop was the objective's stagnation rather than the step
	// tolerance, the sweeps it took, and the cap (§5.3)
	Converged       bool `json:"converged"`
	Stagnated       bool `json:"stagnated"`
	LastRoundSweeps int  `json:"last_round_sweeps"`
	SweepCap        int  `json:"sweep_cap"`
	// The solved nodes not published, by the first gate each failed (§5.4):
	// too few pings, too few peers, still moving when the solve stopped, no
	// better than genesis; then the publishable ones a fresh probe's country
	// contradicted, and those no place or location row was found for.
	RefusedFewPings      int `json:"refused_few_pings"`
	RefusedFewPeers      int `json:"refused_few_peers"`
	RefusedStillMoving   int `json:"refused_still_moving"`
	RefusedNoImprovement int `json:"refused_no_improvement"`
	RefusedProbeCountry  int `json:"refused_probe_country"`
	Unmapped             int `json:"unmapped"`

	// What the run cost (GEOMAP §5.3, "Scale"), measured at the cores it ran
	// on: the sweeps of every round, the cursors the day was read over and
	// the wall time of reading it, the solve's wall time, the peak heap over
	// the ingest and the solve above the heap before it, and the per-unit
	// costs those split into. The planner projects the next run from them.
	Sweeps              int     `json:"sweeps"`
	Cores               int     `json:"cores"`
	ReadCursors         int     `json:"read_cursors"`
	IngestSeconds       float64 `json:"ingest_seconds"`
	SolveSeconds        float64 `json:"solve_seconds"`
	PeakBytes           int64   `json:"peak_bytes"`
	SecondsPerTermSweep float64 `json:"seconds_per_term_sweep"`
	SecondsPerNodeSweep float64 `json:"seconds_per_node_sweep"`
	BytesPerTerm        float64 `json:"bytes_per_term"`
	BytesPerNode        float64 `json:"bytes_per_node"`

	// The planner's projection of the next run from this one, and the
	// budgets it is judged against (SIGNALS.md §2.19c, "capacity"): the solve
	// must stay under MaxSolveSeconds and MaxSolveBytes on this host.
	ProjectedSeconds float64 `json:"projected_seconds"`
	ProjectedBytes   int64   `json:"projected_bytes"`
	MaxSolveSeconds  float64 `json:"max_solve_seconds"`
	MaxSolveBytes    int64   `json:"max_solve_bytes"`

	// the peers a node was expected to measure (D26): an extender's, and a
	// provider's
	ExpectedExtenderPeers int `json:"expected_extender_peers"`
	ExpectedProviderPeers int `json:"expected_provider_peers"`
}

// The runs are kept in Redis as a list, newest first. The monitor compares a
// run with the ones before it (SIGNALS.md §2.19c: a published count against
// the previous run's, a residual over two runs, the sweep cap over three), so
// the history is the record of the derivations, not a cache of the latest.
const DeriveLocationsRunsRedisKey = "derive_locations.runs"

// The runs kept: every comparison the monitor makes, with the rest of the
// day's runs and a run to spare. At under a kilobyte a run, the list costs
// nothing to keep whole.
const DeriveLocationsRunHistory = 8

// The history outlives many runs, so a derivation that stops running shows as
// an old run rather than as no data, and still expires after a stopped
// deployment has been noticed.
const deriveLocationsRunsTtl = 7 * 24 * time.Hour

// Records a derivation at the head of the history and trims the history to
// DeriveLocationsRunHistory runs, in one transaction on the one key.
func AddDeriveLocationsRun(ctx context.Context, run *DeriveLocationsRun) {
	runJson, err := json.Marshal(run)
	if err != nil {
		panic(err)
	}
	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.LPush(ctx, DeriveLocationsRunsRedisKey, runJson)
			pipe.LTrim(ctx, DeriveLocationsRunsRedisKey, 0, DeriveLocationsRunHistory-1)
			pipe.Expire(ctx, DeriveLocationsRunsRedisKey, deriveLocationsRunsTtl)
			return nil
		})
		server.Raise(err)
	})
}

// Up to `count` of the latest derivations, newest first. A record that cannot
// be read is skipped; none recorded, or a history that cannot be read, is an
// empty list.
func GetDeriveLocationsRuns(ctx context.Context, count int) []*DeriveLocationsRun {
	runs := []*DeriveLocationsRun{}
	if count <= 0 {
		return runs
	}
	server.Redis(ctx, func(client server.RedisClient) {
		values, err := client.LRange(ctx, DeriveLocationsRunsRedisKey, 0, int64(count-1)).Result()
		if err != nil {
			return
		}
		for _, value := range values {
			run := &DeriveLocationsRun{}
			if json.Unmarshal([]byte(value), run) == nil {
				runs = append(runs, run)
			}
		}
	})
	return runs
}

// The last derivation; ok is false when none is recorded, or the record cannot
// be read.
func GetDeriveLocationsRun(ctx context.Context) (run *DeriveLocationsRun, ok bool) {
	runs := GetDeriveLocationsRuns(ctx, 1)
	if len(runs) == 0 {
		return nil, false
	}
	return runs[0], true
}

// The genesis inputs of the derive phase (§5.1), read for the whole node set
// at once.

// Each extender's latest activation, an extender's genesis, keyed by extender
// id. An extender with none is absent.
func GetLatestNetworkExtenderActivations(ctx context.Context, extenderIds []server.Id) map[server.Id]*NetworkExtenderActivationRecord {
	activations := map[server.Id]*NetworkExtenderActivationRecord{}
	if len(extenderIds) == 0 {
		return activations
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT ON (extender_id)
				extender_id,
				activation_id,
				activate_time,
				ip_version,
				client_address_hash,
				country_code,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id,
				accuracy_km
			FROM network_extender_activation
			WHERE extender_id = ANY($1)
			ORDER BY extender_id, activate_time DESC, activation_id
			`,
			extenderIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				activation := &NetworkExtenderActivationRecord{}
				server.Raise(result.Scan(
					&activation.ExtenderId,
					&activation.ActivationId,
					&activation.ActivateTime,
					&activation.IpVersion,
					&activation.ClientAddressHash,
					&activation.CountryCode,
					&activation.LocationId,
					&activation.CityLocationId,
					&activation.RegionLocationId,
					&activation.CountryLocationId,
					&activation.AccuracyKm,
				))
				activations[activation.ExtenderId] = activation
			}
		})
	})
	return activations
}

// The egress probe of each provider observed at or after `minObservedAt`,
// keyed by client id: the provider genesis while it is fresh. A provider with
// no fresh probe is absent.
func GetFreshProviderEgressLocations(
	ctx context.Context,
	clientIds []server.Id,
	minObservedAt time.Time,
) map[server.Id]*ProviderEgressLocation {
	egressLocations := map[server.Id]*ProviderEgressLocation{}
	if len(clientIds) == 0 {
		return egressLocations
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				location_id,
				country_code,
				city_confident,
				observed_at
			FROM provider_egress_location
			WHERE client_id = ANY($1) AND $2 <= observed_at
			`,
			clientIds,
			minObservedAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				egressLocation := &ProviderEgressLocation{}
				server.Raise(result.Scan(
					&egressLocation.ClientId,
					&egressLocation.LocationId,
					&egressLocation.CountryCode,
					&egressLocation.CityConfident,
					&egressLocation.ObservedAt,
				))
				egressLocations[egressLocation.ClientId] = egressLocation
			}
		})
	})
	return egressLocations
}

// The location one of a provider's connections was stored with, as the derive
// phase reads its genesis.
type ConnectionGenesisLocation struct {
	ClientId server.Id
	// the published location; a coarser location stores its coarsest id in the
	// finer columns (SetConnectionLocation)
	CityLocationId    server.Id
	RegionLocationId  server.Id
	CountryLocationId server.Id
	// the lookup's own location, nil for a location the probe placed and on
	// rows written before the column
	GenesisLocationId *server.Id
	AccuracyKm        *float32
}

// For each provider, the location of its newest connection that is connected,
// else of its newest connection, keyed by client id. A provider with no located
// connection is absent.
func GetConnectionGenesisLocations(ctx context.Context, clientIds []server.Id) map[server.Id]*ConnectionGenesisLocation {
	genesisLocations := map[server.Id]*ConnectionGenesisLocation{}
	if len(clientIds) == 0 {
		return genesisLocations
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT ON (network_client_location.client_id)
				network_client_location.client_id,
				network_client_location.city_location_id,
				network_client_location.region_location_id,
				network_client_location.country_location_id,
				network_client_location.genesis_location_id,
				network_client_location.accuracy_km
			FROM network_client_location
			INNER JOIN network_client_connection ON
				network_client_connection.connection_id = network_client_location.connection_id
			WHERE network_client_location.client_id = ANY($1)
			ORDER BY
				network_client_location.client_id,
				network_client_connection.connected DESC,
				network_client_connection.connect_time DESC,
				network_client_location.connection_id
			`,
			clientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				genesisLocation := &ConnectionGenesisLocation{}
				server.Raise(result.Scan(
					&genesisLocation.ClientId,
					&genesisLocation.CityLocationId,
					&genesisLocation.RegionLocationId,
					&genesisLocation.CountryLocationId,
					&genesisLocation.GenesisLocationId,
					&genesisLocation.AccuracyKm,
				))
				genesisLocations[genesisLocation.ClientId] = genesisLocation
			}
		})
	})
	return genesisLocations
}
