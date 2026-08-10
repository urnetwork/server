package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The bandwidth sources. A stored figure is tagged with the source that
// produced it, and the row is keyed on (client_id, source), so every source
// keeps its own figure and none overwrites another.
//
// Adding a further active target later needs a constant here and nothing else
// -- no migration, no new column. That generality is why the source is a key
// column rather than a set of per-target columns.
const (
	// ProviderBandwidthSourcePassive is derived from already-settled contract
	// bytes: zero additional cost, and it cannot be gamed selectively. It is
	// computed server-side by ComputePassiveProviderBandwidth and is the one
	// source no prober may submit -- see IsSubmittableProviderBandwidthSource.
	ProviderBandwidthSourcePassive = "passive"
	// ProviderBandwidthSourceActiveOperator is a sampled download from the
	// operator's own endpoint, over the provider's tunnel.
	ProviderBandwidthSourceActiveOperator = "active-operator"
	// ProviderBandwidthSourceActiveCDN is the same sample taken against a
	// public CDN over the same tunnel.
	//
	// The two active figures are stored separately and must never be averaged
	// into one. A provider that prioritises the operator's own path while
	// starving the internet at large is invisible in a combined number and
	// obvious in a pair -- which is the entire reason a second target exists.
	ProviderBandwidthSourceActiveCDN = "active-cdn"
)

// IsProviderBandwidthSource reports whether source is one this deployment
// knows. Storage is keyed on the source, so an unrecognised value is not a
// harmless label: it silently creates a row nothing will ever read or replace.
func IsProviderBandwidthSource(source string) bool {
	switch source {
	case ProviderBandwidthSourcePassive,
		ProviderBandwidthSourceActiveOperator,
		ProviderBandwidthSourceActiveCDN:
		return true
	}
	return false
}

// IsSubmittableProviderBandwidthSource reports whether source may arrive from
// outside, over the result endpoint.
//
// It is the active subset, deliberately: "passive" is derived server-side from
// bytes a provider has already been paid to carry, which is exactly what makes
// it ungameable. Accepting a submitted "passive" row would let the submitter
// overwrite that derived figure with an asserted one -- through an endpoint
// whose secret now travels over a provider-controlled path -- and destroy the
// one bandwidth signal in the system that cannot be gamed selectively.
func IsSubmittableProviderBandwidthSource(source string) bool {
	switch source {
	case ProviderBandwidthSourceActiveOperator,
		ProviderBandwidthSourceActiveCDN:
		return true
	}
	return false
}

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
				-- client acting as a fast provider. Confirmed empirically on a
				-- live deployment: every non-Public-key "earner" turned out to
				-- be exactly this.
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

// StoreProviderBandwidth records a provider's current throughput figure for
// one source. The row is keyed on (client_id, source), so a new measurement
// replaces the previous one FROM THE SAME SOURCE and leaves the other sources
// alone: this is the current figure per source a consumer reads, not a history
// (see the provider_bandwidth migrations).
//
// The source is part of the key rather than a plain column, and that is what
// makes two active targets possible at all: keyed on client_id alone, the
// operator and cdn measurements for one provider would overwrite each other on
// every pass and the divergence between them -- the entire reason there is a
// second target -- would never be visible. Averaging them into one row would
// destroy the same signal more quietly.
//
// Every source writes through here, tagged by bw.Source, so nothing downstream
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
			ON CONFLICT (client_id, source) DO UPDATE
			SET
				bytes_per_second = $2,
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
