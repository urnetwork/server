// The connected provider fleet by what the capability probe last read of it
// (capprobe/README.md), for the capability probes dashboard.
package model

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// End-to-end readings of the census. The first three are fresh evidence; the
// last two are a provider the gate would take out for want of it.
const (
	ProviderCapabilityE2ePostQuantum  = "post_quantum"
	ProviderCapabilityE2eClassical    = "classical"
	ProviderCapabilityE2eNoResponder  = "no_responder"
	ProviderCapabilityE2eNoEvidence   = "no_evidence"
	ProviderCapabilityE2eStale        = "stale_evidence"
	ProviderCapabilityWindowResizable = "resizable"
	ProviderCapabilityWindowFixed     = "fixed"
	// no fresh evidence, so no window reading
	ProviderCapabilityWindowUnknown = "unknown"
)

// The evidence ages of the census: fresh within a day, three days (the
// default refresh) and the seven-day staleness window, then stale or none.
const (
	ProviderCapabilityAge1d    = "1d"
	ProviderCapabilityAge3d    = "3d"
	ProviderCapabilityAge7d    = "7d"
	ProviderCapabilityAgeStale = "stale"
	ProviderCapabilityAgeNone  = "none"
)

// Every e2e and window pair the census publishes, zero included.
var ProviderCapabilityPairs = [][2]string{
	{ProviderCapabilityE2ePostQuantum, ProviderCapabilityWindowResizable},
	{ProviderCapabilityE2ePostQuantum, ProviderCapabilityWindowFixed},
	{ProviderCapabilityE2eClassical, ProviderCapabilityWindowResizable},
	{ProviderCapabilityE2eClassical, ProviderCapabilityWindowFixed},
	{ProviderCapabilityE2eNoResponder, ProviderCapabilityWindowResizable},
	{ProviderCapabilityE2eNoResponder, ProviderCapabilityWindowFixed},
	{ProviderCapabilityE2eNoEvidence, ProviderCapabilityWindowUnknown},
	{ProviderCapabilityE2eStale, ProviderCapabilityWindowUnknown},
}

var ProviderCapabilityAges = []string{
	ProviderCapabilityAge1d,
	ProviderCapabilityAge3d,
	ProviderCapabilityAge7d,
	ProviderCapabilityAgeStale,
	ProviderCapabilityAgeNone,
}

// Key exchange groups kept by name; the rest fold into other.
const providerCapabilityKeyExchangeLimit = 8

const ProviderCapabilityKeyExchangeOther = "other"

type ProviderCapabilityStats struct {
	// providers per e2e and window pair of ProviderCapabilityPairs
	Pairs map[[2]string]int64
	// providers per age of ProviderCapabilityAges
	Ages map[string]int64
	// fresh negotiated sessions per key exchange group, at most
	// providerCapabilityKeyExchangeLimit plus other
	KeyExchanges map[string]int64
	ObservedAt   time.Time
}

// The connected, valid public providers, the capability probe's population,
// by their evidence at the database's statement time. A failed or canceled
// read returns no partial census.
func ReadProviderCapabilityStats(ctx context.Context) (stats *ProviderCapabilityStats, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	server.HandleError(func() {
		server.ReplicaDb(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			server.Raise(err)
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer cleanupCancel()
				_ = tx.Rollback(cleanupCtx)
			}()
			_, err = tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`)
			server.Raise(err)
			stats, returnErr = readProviderCapabilityStats(ctx, tx)
		})
	}, func(err error) { returnErr = err })
	if ctx.Err() != nil {
		returnErr = ctx.Err()
	}
	if returnErr != nil {
		stats = nil
	}
	return
}

func readProviderCapabilityStats(ctx context.Context, query server.PgCanQuery) (*ProviderCapabilityStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := query.Query(
		ctx,
		providerCapabilityStatsSql,
		ProvideModePublic,
		ProviderModernEvidenceMaxAge.Microseconds(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := &ProviderCapabilityStats{
		Pairs:        map[[2]string]int64{},
		Ages:         map[string]int64{},
		KeyExchanges: map[string]int64{},
	}
	for _, pair := range ProviderCapabilityPairs {
		stats.Pairs[pair] = 0
	}
	for _, age := range ProviderCapabilityAges {
		stats.Ages[age] = 0
	}
	keyExchanges := map[string]int64{}
	for rows.Next() {
		var kind, label, window string
		var count int64
		var rowTime time.Time
		if err := rows.Scan(&kind, &label, &window, &count, &rowTime); err != nil {
			return nil, err
		}
		if count < 0 || rowTime.IsZero() || (!stats.ObservedAt.IsZero() && !stats.ObservedAt.Equal(rowTime)) {
			return nil, errors.New("invalid provider capability snapshot")
		}
		stats.ObservedAt = rowTime
		switch kind {
		case "pair":
			pair := [2]string{label, window}
			if _, ok := stats.Pairs[pair]; !ok {
				return nil, errors.New("invalid provider capability snapshot")
			}
			stats.Pairs[pair] += count
		case "age":
			if _, ok := stats.Ages[label]; !ok {
				return nil, errors.New("invalid provider capability snapshot")
			}
			stats.Ages[label] += count
		case "key_exchange":
			if label == "" {
				return nil, errors.New("invalid provider capability snapshot")
			}
			keyExchanges[label] += count
		default:
			return nil, errors.New("invalid provider capability snapshot")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stats.ObservedAt.IsZero() {
		return nil, errors.New("missing provider capability snapshot")
	}
	stats.KeyExchanges = foldProviderCapabilityKeyExchanges(keyExchanges)
	return stats, nil
}

// The largest groups by name, ties by name, and the rest as other, which is
// always published.
func foldProviderCapabilityKeyExchanges(keyExchanges map[string]int64) map[string]int64 {
	names := make([]string, 0, len(keyExchanges))
	for name := range keyExchanges {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a string, b string) int {
		if c := cmp.Compare(keyExchanges[b], keyExchanges[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	folded := map[string]int64{ProviderCapabilityKeyExchangeOther: 0}
	for i, name := range names {
		if i < providerCapabilityKeyExchangeLimit && name != ProviderCapabilityKeyExchangeOther {
			folded[name] += keyExchanges[name]
		} else {
			folded[ProviderCapabilityKeyExchangeOther] += keyExchanges[name]
		}
	}
	return folded
}

// The probe's sweep population (providerCapabilityProbePageSql), each client
// once however many reliability rows or keys it has. Evidence measured at or
// before the staleness cutoff is stale, as the gate classifies it
// (classifyProviderModernEvidence); measured_at holds naive utc.
const providerCapabilityStatsSql = `
	WITH bounds AS (
		SELECT
			statement_timestamp() AT TIME ZONE 'UTC' AS observed_at,
			(statement_timestamp() AT TIME ZONE 'UTC') - make_interval(secs => $2::bigint / 1e6) AS stale_before,
			(statement_timestamp() AT TIME ZONE 'UTC') - interval '1 day' AS day_before,
			(statement_timestamp() AT TIME ZONE 'UTC') - interval '3 days' AS days3_before
	), providers AS MATERIALIZED (
		SELECT network_client.client_id
		FROM network_client
		WHERE network_client.active AND network_client.source_client_id IS NULL
			AND EXISTS (
				SELECT 1 FROM network_client_location_reliability
				WHERE network_client_location_reliability.client_id = network_client.client_id
					AND connected AND valid
			)
			AND EXISTS (
				SELECT 1 FROM provide_key
				WHERE provide_key.client_id = network_client.client_id AND provide_mode = $1
			)
	), classified AS (
		SELECT
			CASE
				WHEN evidence.client_id IS NULL THEN 'no_evidence'
				WHEN evidence.measured_at <= bounds.stale_before THEN 'stale_evidence'
				WHEN evidence.post_quantum THEN 'post_quantum'
				WHEN evidence.key_exchange <> '' THEN 'classical'
				ELSE 'no_responder'
			END AS e2e,
			CASE
				WHEN evidence.client_id IS NULL OR evidence.measured_at <= bounds.stale_before THEN 'unknown'
				WHEN evidence.window_resizable THEN 'resizable'
				ELSE 'fixed'
			END AS window_reading,
			CASE
				WHEN evidence.client_id IS NULL THEN 'none'
				WHEN evidence.measured_at <= bounds.stale_before THEN 'stale'
				WHEN evidence.measured_at > bounds.day_before THEN '1d'
				WHEN evidence.measured_at > bounds.days3_before THEN '3d'
				ELSE '7d'
			END AS age,
			CASE
				WHEN evidence.client_id IS NOT NULL AND bounds.stale_before < evidence.measured_at AND evidence.key_exchange <> '' THEN evidence.key_exchange
			END AS key_exchange
		FROM providers
		CROSS JOIN bounds
		LEFT JOIN provider_modern_evidence evidence ON evidence.client_id = providers.client_id
	), counts AS (
		SELECT 'pair' AS kind, e2e AS label, window_reading, count(*) AS count FROM classified GROUP BY e2e, window_reading
		UNION ALL
		SELECT 'age', age, '', count(*) FROM classified GROUP BY age
		UNION ALL
		SELECT 'key_exchange', key_exchange, '', count(*) FROM classified WHERE key_exchange IS NOT NULL GROUP BY key_exchange
		UNION ALL
		-- one row whatever the population, so an empty fleet still has a snapshot time
		SELECT 'age', 'none', '', 0
	)
	SELECT counts.kind, counts.label, counts.window_reading, counts.count::bigint, bounds.observed_at
	FROM counts CROSS JOIN bounds
	ORDER BY counts.kind, counts.label, counts.window_reading
`
