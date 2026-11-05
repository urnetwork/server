// The read the active bandwidth probe (taskworker/work/provider_bandwidth_probe_work.go)
// selects its providers with: a random draw from the connected providers,
// each with whether its active figure is due.
//
// This is the only read of provider_bandwidth, and it schedules the probe; it
// ranks and selects no provider for a client. ProviderBandwidth is advisory,
// and the URL probe, provider discovery and every bucket the score export
// publishes never read it (provider_bandwidth_readers_test.go pins that).
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The most providers one draw reads.
const ProviderBandwidthProbeMaxSampleSize = 4096

// One provider of a draw.
type ProviderBandwidthProbeCandidate struct {
	ClientId server.Id
	// An active top-level client holding a public provide key, without an
	// active CDN figure or with one measured before its refresh time.
	Due bool
}

// The draw is uniform over the connected, valid providers, so the figures a
// day of passes records are a sample of the fleet as it is, not of the
// providers one id range or one creation period holds. It reads the
// population through network_client_location_reliability's (valid,
// connected, client_id) index, keeps sampleSize of it by a random key, and
// probes three primary keys for each one kept. A figure is keyed on
// (client_id, source); the CDN figure decides due because every sample takes
// it, while the operator target is dropped where the public api url is unset.
const providerBandwidthProbeSampleSql = `
	WITH sample AS MATERIALIZED (
		SELECT client_id
		FROM network_client_location_reliability
		WHERE valid AND connected
		ORDER BY random()
		LIMIT $1
	)
	SELECT
		sample.client_id,
		COALESCE(
			network_client.active AND
			network_client.source_client_id IS NULL AND
			EXISTS (
				SELECT 1 FROM provide_key
				WHERE provide_key.client_id = sample.client_id AND provide_key.provide_mode = $2
			) AND (
				provider_bandwidth.client_id IS NULL OR
				provider_bandwidth.window_end < $3
			),
			false
		) AS due
	FROM sample
	LEFT JOIN network_client ON network_client.client_id = sample.client_id
	LEFT JOIN provider_bandwidth ON provider_bandwidth.client_id = sample.client_id AND provider_bandwidth.source = $4
`

// Up to sampleSize connected, valid providers drawn at random, in no
// particular order, each with whether its active figure is due: none, or one
// measured before refreshBefore. Fewer than sampleSize rows means the draw
// took the whole population.
func GetProviderBandwidthProbeSample(
	ctx context.Context,
	sampleSize int,
	refreshBefore time.Time,
) []ProviderBandwidthProbeCandidate {
	sampleSize = max(1, min(sampleSize, ProviderBandwidthProbeMaxSampleSize))
	candidates := make([]ProviderBandwidthProbeCandidate, 0, sampleSize)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			providerBandwidthProbeSampleSql,
			sampleSize,
			ProvideModePublic,
			refreshBefore.UTC(),
			ProviderBandwidthSourceActiveCDN,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var candidate ProviderBandwidthProbeCandidate
				server.Raise(result.Scan(&candidate.ClientId, &candidate.Due))
				candidates = append(candidates, candidate)
			}
		})
	})
	return candidates
}
