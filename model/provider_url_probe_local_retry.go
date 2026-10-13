// Local URL failures retry with a per-provider exponential backoff.
//
// A turn that measured nothing (no tunnel, nothing measurable, no
// acknowledgement) earns no quota credit, so the rolling quota never stops a
// provider that keeps failing that way. The flat failure interval re-ran such a
// provider about once a minute, each time minting a derived client, opening a
// tunnel, claiming and writing receipts. Now the first local failure keeps the
// configured failure interval and each consecutive earlier one doubles it, up
// to providerUrlProbeLocalRetryMaxInterval, spread by a factor in [0.9, 1.1]
// that is stable per issued claim. A measured completion, success or measured
// failure, ends the streak, and only receipts claimed inside the four-hour
// quota window count toward it, so old failures are not evidence of a current
// one. Measured-run pacing, the ten-run quota and security rechecks are
// untouched; the accepted-history writer owns them.
package model

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"time"

	"github.com/urnetwork/server"
)

// The longest local retry before jitter. A provider that keeps failing
// locally is still retried within about half an hour, so one that recovers
// soon starts refilling its four-hour quota. A configured failure interval
// above it is used unchanged.
const providerUrlProbeLocalRetryMaxInterval = 30 * time.Minute

// The configured failure interval, read the way the paced-attempt SQL reads it.
func providerUrlProbeLocalRetryBase() time.Duration {
	seconds := GetProviderEgressRules().UrlFailureIntervalSeconds
	if seconds == 0 {
		seconds = DefaultProviderEgressRules().UrlFailureIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// How many doublings take base to the cap. Zero when base is already there.
func providerUrlProbeLocalRetryDoublings(base time.Duration) int {
	doublings := 0
	for delay := base; 0 < delay && delay < providerUrlProbeLocalRetryMaxInterval; delay *= 2 {
		doublings++
	}
	return doublings
}

// Doubles base once for each earlier failure in streak, which counts this
// failure, up to the cap, then multiplies by a factor in [0.9, 1.1] hashed from
// the issued claim. Providers that fail together do not retry together, and
// one provider's successive retries are not in lockstep either.
func providerUrlProbeLocalRetryDelay(clientId server.Id, claimOrdinal int64, streak int, base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	ceiling := max(base, providerUrlProbeLocalRetryMaxInterval)
	delay := base
	for range min(max(streak-1, 0), providerUrlProbeLocalRetryDoublings(base)) {
		delay = min(2*delay, ceiling)
	}
	hash := fnv.New64a()
	hash.Write(clientId.Bytes())
	var ordinal [8]byte
	binary.BigEndian.PutUint64(ordinal[:], uint64(claimOrdinal))
	hash.Write(ordinal[:])
	factor := 0.9 + 0.2*float64(hash.Sum64()%2001)/2000
	// Timestamp columns hold microseconds.
	return time.Duration(float64(delay) * factor).Truncate(time.Microsecond)
}

// This failure plus each earlier completed local failure, newest first, back
// to the provider's latest completed measurement. Only claims issued within
// the four-hour quota window before claimedAt count. A lease that was never
// completed neither extends nor ends the streak. The read stops at the
// doublings that can still change the delay, so a provider that has failed for
// days costs the same few indexed rows as one that just started failing. The
// caller holds the provider's cycle lock, which serializes its completions.
func providerUrlProbeLocalFailureStreakInTx(ctx context.Context, tx server.PgTx, clientId server.Id, claimOrdinal int64, claimedAt time.Time, base time.Duration) int {
	doublings := providerUrlProbeLocalRetryDoublings(base)
	streak := 1
	if doublings == 0 {
		return streak
	}
	// A few abandoned leases may sit between failures; they are skipped.
	rows, err := tx.Query(ctx, `SELECT completed_at IS NOT NULL, probe_failure FROM provider_url_probe_run
		WHERE client_id=$1 AND claim_ordinal<$2 AND claimed_at>$3
		ORDER BY claim_ordinal DESC LIMIT $4`,
		clientId, claimOrdinal, claimedAt.Add(-ProviderEgressProbeRefreshAge).UTC(), doublings+4)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var completed bool
			var failure string
			server.Raise(rows.Scan(&completed, &failure))
			if !completed {
				continue
			}
			if failure == "" || doublings < streak {
				return
			}
			streak++
		}
	})
	return streak
}
