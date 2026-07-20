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
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const provisionBatchSize = 2000

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
func provisionProviders(ctx context.Context, entries []ProviderEntry) error {
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
		logf("provisioned providers %d/%d", end, total)
	}
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

				// network.admin_user_id has no FK, so no network_user row is
				// needed; the connection/reliability/selection path never joins
				// it. a shared fleet network is inserted once (ON CONFLICT skips).
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
					ON CONFLICT (client_id) DO NOTHING
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

	entries := make([]ProviderEntry, 0, config.Clients.PoolSize)
	for i := 0; i < config.Clients.PoolSize; i += 1 {
		entries = append(entries, ProviderEntry{
			Index:     i,
			NetworkId: server.NewId().String(),
			UserId:    server.NewId().String(),
			DeviceId:  server.NewId().String(),
			ClientId:  server.NewId().String(),
		})
	}

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

// provisionPrewarm establishes the market so providers are immediately
// selectable, instead of waiting the ~8.4 hours the 12h-lookback reliability
// gate (0.7) requires from a cold start. This sets the initial condition of a
// mature market — the competition measures selection and egress among
// established providers, not the onboarding period.
//
// Rather than backfill raw reliability blocks (which would have to satisfy the
// intricate running-window/shift/degraded maintenance), it materializes the
// location-reliability rows from the connected+tested fleet and then writes the
// final reliability SCORES directly with a passing weight (1.0) for every score
// lookback. The provider's quality/speed score still comes from its real
// latency/speed tests (via network_client_location_reliability), so ranking
// among providers is unaffected — only the reliability-history requirement is
// short-circuited. Providers must be connected and latency/speed-tested first;
// the caller runs this after the ramp.
//
// The pipeline must run in prewarmed mode afterwards (Services.SetPrewarmed),
// so the periodic reliability-score recompute does not overwrite these rows;
// it keeps refreshing the location reliabilities (so churn still gates
// selection) and re-exporting the redis samples.
func provisionPrewarm(ctx context.Context, lookback time.Duration) error {
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
		if err := prewarmOnce(ctx, lookback); err != nil {
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

// prewarmOnce runs the DB-heavy prewarm a single time, converting the deep
// `dbWithPool` panic (raised when postgres is unreachable after its own connect
// retries) into a returned error so the caller can retry or fail cleanly. Both
// steps are individually idempotent, so retrying after a partial failure is
// safe: UpdateClientLocationReliabilities rebuilds nclr, and the score write
// upserts (ON CONFLICT DO UPDATE).
func prewarmOnce(ctx context.Context, lookback time.Duration) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("prewarm db op failed (postgres likely unreachable): %v", r)
		}
	}()

	now := server.NowUtc()

	// build network_client_location_reliability from the currently connected,
	// latency/speed-tested providers
	model.UpdateClientLocationReliabilities(ctx, now.Add(-lookback), now)

	block := now.UTC().UnixMilli() / int64(model.ReliabilityBlockDuration/time.Millisecond)

	// write a passing score for every score lookback (see ClientLookbacks:
	// indices 0,1,2) for every valid connected provider, joined to its location.
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			INSERT INTO client_connection_reliability_score (
				client_id, lookback_index,
				independent_reliability_score, independent_reliability_weight,
				reliability_score, reliability_weight,
				min_block_number, max_block_number,
				city_location_id, region_location_id, country_location_id
			)
			SELECT
				nclr.client_id, lb.lookback_index,
				1.0, 1.0, 1.0, 1.0,
				$1::bigint - $2::bigint, $1::bigint,
				nclr.city_location_id, nclr.region_location_id, nclr.country_location_id
			FROM network_client_location_reliability nclr
			CROSS JOIN (VALUES (0), (1), (2)) AS lb(lookback_index)
			WHERE nclr.connected AND nclr.valid
			ON CONFLICT (client_id, lookback_index) DO UPDATE
			SET
				independent_reliability_weight = 1.0,
				reliability_weight = 1.0,
				max_block_number = EXCLUDED.max_block_number
			`,
			block, int64(lookback/model.ReliabilityBlockDuration),
		))
	})
	return nil
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
