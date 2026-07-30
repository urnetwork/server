package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// bandwidth sources. A stored figure is tagged with the source that produced
// it so consumers never have to know which one did.
const (
	// ProviderBandwidthSourcePassive is derived from already-settled contract
	// bytes: zero additional cost, and it cannot be gamed selectively.
	ProviderBandwidthSourcePassive = "passive"
	// ProviderBandwidthSourceActive is a sampled download over the provider's
	// tunnel, used only where passive history does not exist yet.
	ProviderBandwidthSourceActive = "active"
)

// ProviderBandwidth is one throughput figure for a provider. It is advisory:
// nothing may gate provider selection on it.
type ProviderBandwidth struct {
	ClientId        server.Id
	BytesPerSecond  float64
	Source          string
	SampleByteCount ByteCount
	WindowStart     time.Time
	WindowEnd       time.Time
}

// ComputePassiveProviderBandwidth derives a provider's throughput from bytes it
// has already been paid to carry. `transfer_escrow`/`contract_close` record
// settled bytes per contract as a byproduct of billing, so reading them costs
// no additional bandwidth, and a provider cannot inflate the figure selectively
// -- it cannot move more real user traffic without actually being fast for real
// users.
//
// The rate is the total settled bytes in the window over the wall-clock span
// those contracts covered, so it is an average over the sampled traffic rather
// than a peak. Returns nil, nil when the provider settled no bytes in the
// window: no history, which is not the same as measured-zero throughput.
func ComputePassiveProviderBandwidth(
	ctx context.Context,
	clientId server.Id,
	window time.Duration,
) (*ProviderBandwidth, error) {
	windowStart := server.NowUtc().Add(-window)

	var contractCount int
	var sampleByteCount ByteCount
	// null whenever no contract matched
	var minCreateTime *time.Time
	var maxCloseTime *time.Time

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				COUNT(*),
				COALESCE(SUM(contract_close.used_transfer_byte_count), 0)::bigint,
				MIN(transfer_contract.create_time),
				MAX(contract_close.close_time)

			FROM contract_close

			INNER JOIN transfer_contract ON
				transfer_contract.contract_id = contract_close.contract_id

			WHERE
				transfer_contract.destination_id = $1 AND
				contract_close.party = 'destination' AND
				-- companion_contract_id IS NULL excludes return-traffic legs: a
				-- client's return traffic settles as a contract where the CLIENT
				-- is the destination, which would otherwise be misread as that
				-- client acting as a fast provider. See
				-- docs/superpowers/specs/2026-07-25-enforced-provider-geo-probing-design.md
				-- "Threat model" -- confirmed empirically: on beta, every
				-- non-Public-key "earner" turned out to be exactly this.
				transfer_contract.companion_contract_id IS NULL AND
				$2 <= contract_close.close_time
			`,
			clientId,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&contractCount,
					&sampleByteCount,
					&minCreateTime,
					&maxCloseTime,
				))
			}
		})
	})

	if contractCount == 0 || sampleByteCount <= 0 || minCreateTime == nil || maxCloseTime == nil {
		return nil, nil
	}

	elapsed := maxCloseTime.Sub(*minCreateTime)
	if elapsed <= 0 {
		// no usable denominator (a single instantaneous close, or skew between
		// the create and close writers). A rate is undefined here, and dividing
		// by zero or a negative span would report an absurd one.
		return nil, nil
	}

	return &ProviderBandwidth{
		ClientId:        clientId,
		BytesPerSecond:  float64(sampleByteCount) / elapsed.Seconds(),
		Source:          ProviderBandwidthSourcePassive,
		SampleByteCount: sampleByteCount,
		// the span actually measured, which is at most `window` wide
		WindowStart: *minCreateTime,
		WindowEnd:   *maxCloseTime,
	}, nil
}

// StoreProviderBandwidth records a provider's current throughput figure,
// whatever produced it. The row is keyed on client_id, so a new measurement
// replaces the previous one: this is the current figure a consumer reads, not a
// history (see the provider_bandwidth migration).
//
// Both sources write through here, tagged by bw.Source, so nothing downstream
// has to know whether a figure came from settled traffic or an active sample.
// The figure is advisory -- storing one must never gate provider selection.
func StoreProviderBandwidth(ctx context.Context, bw *ProviderBandwidth) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_bandwidth (
				client_id,
				bytes_per_second,
				source,
				sample_byte_count,
				window_start,
				window_end,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (client_id) DO UPDATE
			SET
				bytes_per_second = $2,
				source = $3,
				sample_byte_count = $4,
				window_start = $5,
				window_end = $6,
				update_time = $7
			`,
			bw.ClientId,
			bw.BytesPerSecond,
			bw.Source,
			bw.SampleByteCount,
			// window_start/window_end are naive timestamp columns holding utc,
			// as everywhere else in this schema
			bw.WindowStart.UTC(),
			bw.WindowEnd.UTC(),
			server.NowUtc(),
		))
	})
}
