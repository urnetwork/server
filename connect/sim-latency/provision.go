package main

// database provisioning for a run.
//
// Providers and the client pool need network/user/device/client rows so the
// connect service can record connections and reliability. Providers are bulk-
// inserted in batches (idempotent, ON CONFLICT DO NOTHING) so a 100k fleet
// provisions in seconds; the smaller client pool additionally gets a transfer
// balance so its contracts can be paid. The sim region is created up front so
// the country-code cache and the client provider spec resolve to it, and the
// ip_overrides settings are written into the site so the server geolocates the
// fake subnets to the region.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	provisionBatchSize       = 2000
	clientIdentitySeedDomain = int64(0x434c49454e5453) // "CLIENTS"
)

// A fixture provider's mature-market reliability. The synthetic prewarm uses
// this instead of claiming that every seeded churn profile is perfectly up.
type matureProviderReliability struct {
	clientId          server.Id
	reliabilityWeight float64
}

// A fixture provider's deterministic initial performance evidence. Connection
// tests belong to short-lived platform transports, so an active-connection
// snapshot can legitimately contain no completed test even though the fixture
// already defines the provider's network performance.
type matureProviderPerformance struct {
	clientId                 server.Id
	minRelativeLatencyMillis int64
	maxBytesPerSecond        int64
}

// provisionRegion creates the sim country/region/city and returns the country
// location id (used as the client provider spec).
func provisionRegion(ctx context.Context, region RegionConfig) (server.Id, error) {
	location := &model.Location{
		LocationType: model.LocationTypeCity,
		City:         region.City,
		Region:       region.Region,
		Country:      region.Country,
		CountryCode:  region.CountryCode,
	}
	model.CreateLocation(ctx, location)
	if location.CountryLocationId == (server.Id{}) {
		return server.Id{}, fmt.Errorf("region country location not created")
	}
	return location.CountryLocationId, nil
}

// provisionProviders bulk-inserts the fleet's identities. No balances (providers
// earn), no auth rows (jwts are pre-signed).
func provisionProviders(
	ctx context.Context,
	entries []ProviderEntry,
	locationId server.Id,
	countryCode string,
) error {
	total := len(entries)
	for start := 0; start < total; start += provisionBatchSize {
		end := start + provisionBatchSize
		if total < end {
			end = total
		}
		batch := entries[start:end]
		if err := provisionIdentityBatch(ctx, batch); err != nil {
			return err
		}
		if err := provisionEgressEvidenceBatch(ctx, batch, locationId, countryCode); err != nil {
			return err
		}
		logf("provisioned providers %d/%d", end, total)
	}
	return nil
}

// provisionEgressEvidenceBatch establishes the simulated providers as usable
// supply for the real egress-health gate. Production fills these tables from an
// external prober, but the self-contained simulation has no external internet
// or operator-proxy process. Its ground truth already defines each fleet entry
// as a functioning egress with modeled latency, bandwidth, loss, and churn, so
// one passing synthetic probe and an observation in the configured fake country
// are part of the simulation's initial condition, like the reliability prewarm.
func provisionEgressEvidenceBatch(
	ctx context.Context,
	entries []ProviderEntry,
	locationId server.Id,
	countryCode string,
) error {
	measuredAt := server.NowUtc()
	countryCode = strings.ToLower(countryCode)
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, entry := range entries {
				clientId := server.RequireParseId(entry.ClientId)
				batch.Queue(
					`
					INSERT INTO provider_egress_health (
						client_id, measured_at, ok_count, total_count,
						class_results, reputation_ok, reputation_total,
						failed_names, reputation_failed_names
					)
					VALUES ($1, $2, 1, 1, '{"sim":{"ok":1,"total":1}}'::jsonb, 0, 0, '', '')
					ON CONFLICT (client_id) DO UPDATE
					SET
						measured_at = EXCLUDED.measured_at,
						ok_count = EXCLUDED.ok_count,
						total_count = EXCLUDED.total_count,
						class_results = EXCLUDED.class_results,
						reputation_ok = EXCLUDED.reputation_ok,
						reputation_total = EXCLUDED.reputation_total,
						failed_names = EXCLUDED.failed_names,
						reputation_failed_names = EXCLUDED.reputation_failed_names
					`,
					clientId, measuredAt,
				)
				batch.Queue(
					`
					INSERT INTO provider_egress_location (
						client_id, location_id, country_code, asn, org,
						hosting, proxy, mobile, city_confident,
						observed_at, verdict, verdict_reason, assurance, update_time
					)
					VALUES ($1, $2, $3, 0, 'sim', $4, false, $5, false, $6, 'verified', '', 'direct', $6)
					ON CONFLICT (client_id) DO UPDATE
					SET
						location_id = EXCLUDED.location_id,
						country_code = EXCLUDED.country_code,
						asn = EXCLUDED.asn,
						org = EXCLUDED.org,
						hosting = EXCLUDED.hosting,
						proxy = EXCLUDED.proxy,
						mobile = EXCLUDED.mobile,
						city_confident = EXCLUDED.city_confident,
						observed_at = EXCLUDED.observed_at,
						verdict = EXCLUDED.verdict,
						verdict_reason = EXCLUDED.verdict_reason,
						assurance = EXCLUDED.assurance,
						update_time = EXCLUDED.update_time
					WHERE provider_egress_location.observed_at < EXCLUDED.observed_at
					`,
					clientId,
					locationId,
					countryCode,
					entry.UserType == "hosting",
					entry.Component == "mobile-variable",
					measuredAt,
				)
			}
		})
	})
	return nil
}

// provisionIdentityBatch inserts network/user/device/client rows for a batch.
func provisionIdentityBatch(ctx context.Context, entries []ProviderEntry) error {
	createTime := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, entry := range entries {
				networkId := server.RequireParseId(entry.NetworkId)
				userId := server.RequireParseId(entry.UserId)
				deviceId := server.RequireParseId(entry.DeviceId)
				clientId := server.RequireParseId(entry.ClientId)

				// Client JWT validation joins the network's admin to network_user.
				// Keep every generated user available because providers in a shared
				// fleet network retain their explicit identities in the fixture;
				// authentication uses the first entry's user as that network's admin.
				batch.Queue(
					`
					INSERT INTO network_user (user_id, user_name, auth_type, verified)
					VALUES ($1, $2, 'guest', false)
					ON CONFLICT (user_id) DO NOTHING
					`,
					userId, "sim-provider",
				)
				// A shared fleet network is inserted once; fixture order therefore
				// deterministically selects its admin user.
				batch.Queue(
					`
					INSERT INTO network (network_id, network_name, admin_user_id, contains_profanity)
					VALUES ($1, $2, $3, false)
					ON CONFLICT (network_id) DO NOTHING
					`,
					networkId, entry.NetworkId, userId,
				)
				batch.Queue(
					`
					INSERT INTO device (device_id, network_id, device_name, device_spec, create_time)
					VALUES ($1, $2, $3, $4, $5)
					ON CONFLICT (device_id) DO NOTHING
					`,
					deviceId, networkId, "sim-provider", "sim-provider", createTime,
				)
				batch.Queue(
					`
					INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time)
					VALUES ($1, $2, $3, $4, $5, $5)
					ON CONFLICT (client_id) DO UPDATE
					SET
						network_id = EXCLUDED.network_id,
						device_id = EXCLUDED.device_id,
						active = true,
						auth_time = EXCLUDED.auth_time
					`,
					clientId, networkId, deviceId, "sim-provider", createTime,
				)
			}
		})
	})
	return nil
}

// provisionClientPool creates the pool of client identities (network + device +
// client + transfer balance) and returns their identities with signed jwts.
func provisionClientPool(ctx context.Context, config *Config) ([]ClientIdentity, error) {
	pool := make([]ClientIdentity, 0, config.Clients.PoolSize)

	entries := generatedClientPoolEntries(config)

	// reuse the bulk identity insert
	if err := provisionIdentityBatch(ctx, entries); err != nil {
		return nil, err
	}

	// grant each client network a transfer balance so contracts are payable
	now := server.NowUtc()
	for _, entry := range entries {
		networkId := server.RequireParseId(entry.NetworkId)
		if err := model.AddBasicTransferBalance(
			ctx, networkId, model.ByteCount(config.Clients.BalanceBytes), now, now.Add(365*24*time.Hour),
		); err != nil {
			return nil, err
		}
	}

	for _, entry := range entries {
		byJwt := signClientJwt(entry)
		pool = append(pool, ClientIdentity{
			ClientId: server.RequireParseId(entry.ClientId),
			ByJwt:    byJwt,
		})
	}

	logf("provisioned client pool: %d", len(pool))
	return pool, nil
}

// generatedClientPoolEntries derives the client identities from the fixture
// seed. The domain separator keeps this stream independent of the provider
// identity stream while making a clean reset replay the same client networks,
// devices, clients, and simulated source addresses.
func generatedClientPoolEntries(config *Config) []ProviderEntry {
	r := newRng(config.Seed ^ clientIdentitySeedDomain)
	entries := make([]ProviderEntry, 0, config.Clients.PoolSize)
	for i := 0; i < config.Clients.PoolSize; i += 1 {
		entries = append(entries, ProviderEntry{
			Index:     i,
			NetworkId: r.id().String(),
			UserId:    r.id().String(),
			DeviceId:  r.id().String(),
			ClientId:  r.id().String(),
		})
	}
	return entries
}

// provisionPrewarm establishes the market so providers are immediately
// selectable, instead of waiting the ~8.4 hours the 12h-lookback reliability
// gate (0.7) requires from a cold start. This sets the initial condition of a
// mature market — the competition measures selection and egress among
// established providers, not the onboarding period.
//
// Rather than backfill raw reliability blocks (which would have to satisfy the
// intricate running-window/shift/degraded maintenance), it seeds deterministic
// performance tests on each active connection, materializes the derived
// location-reliability rows, and writes the final reliability scores directly
// for every score lookback. Each provider's weight is its seeded long-run uptime
// fraction, so the mature initial market preserves the fixture's reliability
// ranking instead of treating a mobile churn profile and business fiber as
// equally perfect. Providers must be connected first; the caller runs this
// after the ramp.
//
// The pipeline must run in prewarmed mode afterwards (Services.SetPrewarmed),
// so the periodic reliability-score recompute does not overwrite these rows;
// it keeps refreshing the location reliabilities (so churn still gates
// selection) and re-exporting the redis samples.
func provisionPrewarm(ctx context.Context, lookback time.Duration, entries []ProviderEntry) error {
	reliabilities, err := matureProviderReliabilities(entries)
	if err != nil {
		return err
	}
	performances, err := matureProviderPerformances(entries)
	if err != nil {
		return err
	}

	// The prewarm is DB-heavy (a fleet-wide reliability rebuild plus a score
	// upsert). With a very large fleet connected, postgres can be transiently
	// saturated, and `dbWithPool`/`MaintenanceTx` panics once its own connect
	// retries are exhausted (server/db.go). Recover that panic per attempt and
	// retry with backoff so a brief blip does not abort the run; if postgres
	// stays unreachable, fail with a clear cause instead of an opaque panic
	// stack. At that point the fleet almost certainly exceeds the environment's
	// DB capacity — observed at ~40k providers against a single co-located
	// postgres, which is starved into dial timeouts under the connect load.
	const attempts = 3
	backoff := 3 * time.Second
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt += 1 {
		if err := prewarmOnce(ctx, lookback, reliabilities, performances); err != nil {
			lastErr = err
			logf("prewarm attempt %d/%d failed: %s", attempt, attempts, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		logf("prewarmed reliability scores for the connected fleet")
		return nil
	}
	return fmt.Errorf(
		"prewarm failed after %d attempts: postgres unreachable under the connected fleet — "+
			"reduce --count, add --fleet-shards, or point at a dedicated postgres: %w",
		attempts, lastErr,
	)
}

// Converts fixture churn into the reliability weights a mature history would
// converge toward. Invalid ground truth fails before any database mutation.
func matureProviderReliabilities(entries []ProviderEntry) ([]matureProviderReliability, error) {
	reliabilities := make([]matureProviderReliability, 0, len(entries))
	for _, entry := range entries {
		clientId, err := server.ParseId(entry.ClientId)
		if err != nil {
			return nil, fmt.Errorf("provider %d client id: %w", entry.Index, err)
		}
		if math.IsNaN(entry.UptimeSeconds) || math.IsInf(entry.UptimeSeconds, 0) || entry.UptimeSeconds <= 0 {
			return nil, fmt.Errorf("provider %d uptime must be finite and positive", entry.Index)
		}
		if math.IsNaN(entry.DowntimeSeconds) || math.IsInf(entry.DowntimeSeconds, 0) || entry.DowntimeSeconds < 0 {
			return nil, fmt.Errorf("provider %d downtime must be finite and non-negative", entry.Index)
		}
		totalSeconds := entry.UptimeSeconds + entry.DowntimeSeconds
		if math.IsInf(totalSeconds, 0) {
			return nil, fmt.Errorf("provider %d churn cycle must be finite", entry.Index)
		}
		reliabilities = append(reliabilities, matureProviderReliability{
			clientId:          clientId,
			reliabilityWeight: entry.UptimeSeconds / totalSeconds,
		})
	}
	return reliabilities, nil
}

// Converts the fixture's one-way link latency and throughput into the fields
// produced by connection tests. The latency test is a round trip through the
// impaired transport, while its minimum sample removes seeded positive jitter.
func matureProviderPerformances(entries []ProviderEntry) ([]matureProviderPerformance, error) {
	performances := make([]matureProviderPerformance, 0, len(entries))
	for _, entry := range entries {
		clientId, err := server.ParseId(entry.ClientId)
		if err != nil {
			return nil, fmt.Errorf("provider %d client id: %w", entry.Index, err)
		}
		if math.IsNaN(entry.LatencyMillis) || math.IsInf(entry.LatencyMillis, 0) || entry.LatencyMillis < 0 {
			return nil, fmt.Errorf("provider %d latency must be finite and non-negative", entry.Index)
		}
		roundTripLatencyMillis := math.Round(2 * entry.LatencyMillis)
		if math.MaxInt32 < roundTripLatencyMillis {
			return nil, fmt.Errorf("provider %d round-trip latency exceeds the database range", entry.Index)
		}
		if entry.BandwidthBps <= 0 {
			return nil, fmt.Errorf("provider %d bandwidth must be positive", entry.Index)
		}
		performances = append(performances, matureProviderPerformance{
			clientId:                 clientId,
			minRelativeLatencyMillis: int64(roundTripLatencyMillis),
			maxBytesPerSecond:        entry.BandwidthBps,
		})
	}
	return performances, nil
}

// prewarmOnce runs the DB-heavy prewarm a single time, converting the deep
// `dbWithPool` panic (raised when postgres is unreachable after its own connect
// retries) into a returned error so the caller can retry or fail cleanly. Every
// step is idempotent, so retrying after a partial failure is safe: performance
// writes and score writes upsert, and UpdateClientLocationReliabilities rebuilds
// nclr.
func prewarmOnce(
	ctx context.Context,
	lookback time.Duration,
	reliabilities []matureProviderReliability,
	performances []matureProviderPerformance,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("prewarm db op failed (postgres likely unreachable): %v", r)
		}
	}()

	now := server.NowUtc()

	// build network_client_location_reliability from the currently connected,
	// latency/speed-tested providers
	writeMatureProviderPerformances(ctx, performances)
	model.UpdateClientLocationReliabilities(ctx, now.Add(-lookback), now)
	writeMatureReliabilityScores(ctx, now, lookback, reliabilities)
	return nil
}

// Seeds deterministic test evidence on the currently active connections before
// building the location-reliability snapshot. Completed tests belong to a
// specific platform transport, so relying on an earlier transport's tests made
// a newly active connection look untested and excluded it from matchmaking.
// Writing the connection tables, instead of only the derived snapshot, also
// makes later pipeline refreshes preserve the prewarmed evidence.
func writeMatureProviderPerformances(
	ctx context.Context,
	performances []matureProviderPerformance,
) {
	clientIds := make([]server.Id, 0, len(performances))
	minRelativeLatencyMillisValues := make([]int64, 0, len(performances))
	maxBytesPerSecondValues := make([]int64, 0, len(performances))
	for _, performance := range performances {
		clientIds = append(clientIds, performance.clientId)
		minRelativeLatencyMillisValues = append(minRelativeLatencyMillisValues, performance.minRelativeLatencyMillis)
		maxBytesPerSecondValues = append(maxBytesPerSecondValues, performance.maxBytesPerSecond)
	}

	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			WITH mature_performance AS (
				SELECT client_id, min_relative_latency_ms
				FROM unnest($1::uuid[], $2::bigint[])
					AS mature(client_id, min_relative_latency_ms)
			)
			INSERT INTO network_client_latency (connection_id, latency_ms, sample_count)
			SELECT
				network_client_connection.connection_id,
				(mature.min_relative_latency_ms + network_client_connection.expected_latency_ms)::integer,
				1
			FROM mature_performance mature
			INNER JOIN network_client_connection ON
				network_client_connection.client_id = mature.client_id AND
				network_client_connection.connected = true
			ON CONFLICT (connection_id) DO UPDATE
			SET
				latency_ms = EXCLUDED.latency_ms,
				sample_count = EXCLUDED.sample_count
			`,
			clientIds,
			minRelativeLatencyMillisValues,
		))
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			WITH mature_performance AS (
				SELECT client_id, max_bytes_per_second
				FROM unnest($1::uuid[], $2::bigint[])
					AS mature(client_id, max_bytes_per_second)
			)
			INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count)
			SELECT
				network_client_connection.connection_id,
				mature.max_bytes_per_second,
				1
			FROM mature_performance mature
			INNER JOIN network_client_connection ON
				network_client_connection.client_id = mature.client_id AND
				network_client_connection.connected = true
			ON CONFLICT (connection_id) DO UPDATE
			SET
				bytes_per_second = EXCLUDED.bytes_per_second,
				sample_count = EXCLUDED.sample_count
			`,
			clientIds,
			maxBytesPerSecondValues,
		))
	})
}

// Writes the seeded mature reliability beside the real connected/location and
// latency/speed evidence. An inner join keeps disconnected providers out.
func writeMatureReliabilityScores(
	ctx context.Context,
	now time.Time,
	lookback time.Duration,
	reliabilities []matureProviderReliability,
) {
	clientIds := make([]server.Id, 0, len(reliabilities))
	reliabilityWeights := make([]float64, 0, len(reliabilities))
	for _, reliability := range reliabilities {
		clientIds = append(clientIds, reliability.clientId)
		reliabilityWeights = append(reliabilityWeights, reliability.reliabilityWeight)
	}

	block := now.UTC().UnixMilli() / int64(model.ReliabilityBlockDuration/time.Millisecond)

	// Write every score lookback (see ClientLookbacks: indices 0,1,2) for each
	// valid connected provider, joined to its fixture reliability and location.
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			WITH mature_reliability AS (
				SELECT client_id, reliability_weight
				FROM unnest($3::uuid[], $4::double precision[])
					AS mature(client_id, reliability_weight)
			)
			INSERT INTO client_connection_reliability_score (
				client_id, lookback_index,
				independent_reliability_score, independent_reliability_weight,
				reliability_score, reliability_weight,
				min_block_number, max_block_number,
				city_location_id, region_location_id, country_location_id
			)
			SELECT
				nclr.client_id, lb.lookback_index,
				mature.reliability_weight, mature.reliability_weight,
				mature.reliability_weight, mature.reliability_weight,
				$1::bigint - $2::bigint, $1::bigint,
				nclr.city_location_id, nclr.region_location_id, nclr.country_location_id
			FROM network_client_location_reliability nclr
			INNER JOIN mature_reliability mature ON mature.client_id = nclr.client_id
			CROSS JOIN (VALUES (0), (1), (2)) AS lb(lookback_index)
			WHERE nclr.connected AND nclr.valid
			ON CONFLICT (client_id, lookback_index) DO UPDATE
			SET
				independent_reliability_score = EXCLUDED.independent_reliability_score,
				independent_reliability_weight = EXCLUDED.independent_reliability_weight,
				reliability_score = EXCLUDED.reliability_score,
				reliability_weight = EXCLUDED.reliability_weight,
				max_block_number = EXCLUDED.max_block_number
			`,
			block, int64(lookback/model.ReliabilityBlockDuration),
			clientIds, reliabilityWeights,
		))
	})
}

// writeSiteSettings writes the site settings.yml the in-process server reads:
// the ip_overrides mapping the testing subnets to the sim region, and the
// FindProviders2 stats sampling knobs.
func writeSiteSettings(siteHome string, config *Config) error {
	if err := os.MkdirAll(siteHome, 0o755); err != nil {
		return err
	}
	settings := map[string]any{
		"ip_overrides":                         config.ipOverridesSettings(),
		"stats_findproviders2_sample_fraction": 1.0,
		"stats_findproviders2_max_candidates":  2000,
	}
	settingsBytes, err := yaml.Marshal(settings)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(siteHome, "settings.yml"), settingsBytes, 0o644)
}

func signClientJwt(entry ProviderEntry) string {
	networkId := server.RequireParseId(entry.NetworkId)
	userId := server.RequireParseId(entry.UserId)
	deviceId := server.RequireParseId(entry.DeviceId)
	clientId := server.RequireParseId(entry.ClientId)
	return jwtSign(networkId, userId, entry.NetworkId, deviceId, clientId)
}
