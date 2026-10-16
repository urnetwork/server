// The native URL-run minimums: Quality and Speed read a provider's URL success
// ratio only once enough accepted measured runs back it, as the Online
// URL-success rule reads no verdict from fewer than
// ProviderOnlineMinMeasuredUrlRuns. Until then a passing ratio is no verdict
// for that bucket yet: the provider stays in Online and the counts with its
// measured URL weight, leaves the bucket under the unprobed reason (the Online
// rule's "warming up or unprobed"), and probing goes on as for any provider.
// Like every bucket rule they are bucket membership only, no hard exclusion:
// probe admission, the rolling quota, explicit client ids and force_minimum
// answers do not read them.
//
// One run cannot show a share of four in five. On 2026-10-15 a provider
// network that adds US client ids in batches of a few hundred, one address
// each, had a batch of 509 admitted to URL probing at 21:27:45 UTC. At the
// 21:31 pass 437 of them had exactly one accepted run, a success. 1/1 passes
// the 4/5 ratio, so each entered Quality, at index 0, on that one sample. The
// same network's batches had entered the same way on 2026-10-14 at 07:30 and
// on 2026-10-15 at 03:27, and went on to fail every run while connected.
package model

import (
	"time"
)

// The fewest accepted measured URL runs in the evidence window that admit a
// provider to Quality. At three the 4/5 ratio needs every run to have
// succeeded (4·3 ≤ 5·ok holds only for ok = 3), so one success, or a success
// and a failure, never decides Quality.
const ProviderQualityMinMeasuredUrlRuns = 3

// The shortest span, from the earliest accepted run in the evidence window to
// the latest, of the runs that admit a provider to Quality: they come from
// separate paced probe turns, not one. After a success the probe paces a
// provider's next run by UrlSuccessIntervalSeconds (20 minutes, jittered by
// -10% to +10%, so 18 to 22 minutes), and after a failure by
// UrlFailureIntervalSeconds (about a minute). Thirty minutes is more than one
// success interval at its longest, so one paced turn with its quick retries
// never reaches it, and less than two at their shortest, so the three paced
// successes of a new provider (36 to 44 minutes) always do.
const ProviderQualityMinMeasuredUrlSpan = 30 * time.Minute

// The fewest accepted measured URL runs in the evidence window that admit a
// provider to Speed: as many as the Online URL-success rule reads a verdict
// from. Speed asks less of the URL checks than Quality, but one run is a
// sample, not a share.
const ProviderSpeedMinMeasuredUrlRuns = 2

// Whether the accepted URL runs in the evidence window are enough, and span
// long enough, for Quality to read their ratio, whatever it is. A provider
// without current evidence has no runs to read, so it has not met them.
func providerQualityUrlRunsMet(counts ProviderEgressHealthCounts, urlIndex EgressIndex) bool {
	return urlIndex.Evidence &&
		ProviderQualityMinMeasuredUrlRuns <= counts.Total &&
		ProviderQualityMinMeasuredUrlSpan <= counts.MeasuredAt.Sub(counts.FirstMeasuredAt)
}

// Whether the accepted URL runs in the evidence window are enough for Speed
// to read their ratio, whatever it is. A provider without current evidence
// has not met them.
func providerSpeedUrlRunsMet(counts ProviderEgressHealthCounts, urlIndex EgressIndex) bool {
	return urlIndex.Evidence && ProviderSpeedMinMeasuredUrlRuns <= counts.Total
}

// The minimums as the dashboard's policy key reads them
// (provider_egress_counts_policy.go): workers that hold other minimums, or
// none, count the buckets differently and must not share a snapshot.
type providerNativeMinRunsPolicy struct {
	QualityMinMeasuredUrlRuns int
	QualityMinMeasuredUrlSpan time.Duration
	SpeedMinMeasuredUrlRuns   int
}

var providerNativeMinRuns = providerNativeMinRunsPolicy{
	QualityMinMeasuredUrlRuns: ProviderQualityMinMeasuredUrlRuns,
	QualityMinMeasuredUrlSpan: ProviderQualityMinMeasuredUrlSpan,
	SpeedMinMeasuredUrlRuns:   ProviderSpeedMinMeasuredUrlRuns,
}
