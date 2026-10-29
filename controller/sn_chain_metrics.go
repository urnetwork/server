package controller

// Metrics of the SN top board's chain-side readers (LEADERTOP.md §5.7, §6.2):
// the live fleet binding sync and the on-chain incentive reader. Counts and
// ages only; no client, hotkey or network identities. Panels are on the
// "urnetwork / sn board chain" dashboard.

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/stmetagraph"
)

var (
	snFleetBindingSyncLagBlocks = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_fleet_binding", Name: "sync_lag_blocks",
		Help: "Finalized blocks the live fleet binding census has not applied yet, after the last sync run.",
	})
	snFleetBindingSyncEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_fleet_binding", Name: "sync_events_total",
		Help: "Coordinator binding logs the live sync applied, by kind: FleetBound, FleetBindingRevoked, FleetBindingCleaned.",
	}, []string{"kind"})
	snFleetBindingSyncErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_fleet_binding", Name: "sync_errors_total",
		Help: "Failed live binding sync runs, by reason: config (no deployment), chain (an RPC read failed), canonical (a log or the checkpoint is off the canonical chain; the sync halts), census (logs contradict the applied census; the range is not applied).",
	}, []string{"reason"})
	snFleetBindingSyncLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_fleet_binding", Name: "sync_last_success_timestamp_seconds",
		Help: "Unix time of the last live binding sync run that reached its end without an error.",
	})
	snClaimedFleetsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_fleet_binding", Name: "claimed_fleets",
		Help: "Fleets with a binding active at the current settlement epoch, by whether the hotkey holds the bound UID at the newest metagraph census: held (live, the validator counts the members), lost, unknown (the census has not read the UID).",
	}, []string{"uid_held"})
	snClaimedFleetMembersGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_fleet_binding", Name: "live_members",
		Help: "Clients bound to a live claimed fleet at the current settlement epoch.",
	})

	snIncentiveReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "reads_total",
		Help: "Incentive reader runs, by result: stored (a new tempo's census), unchanged (SubnetEpochIndex has not advanced), skipped (no RPC: the tempo has not nearly passed or a failed read is waiting).",
	}, []string{"result"})
	snIncentiveReadErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "read_errors_total",
		Help: "Failed incentive reads, by reason: rpc, chain_identity (genesis differs), finalized_head, metadata, storage_shape (a runtime changed an item), storage_malformed, vectors_inconsistent, budget (the census exceeded its response bytes), checkpoint_not_canonical.",
	}, []string{"reason"})
	snIncentiveResponseBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "response_bytes_total",
		Help: "Archive response bytes the incentive reader read, by phase: index (finalized head and SubnetEpochIndex checks), census (one per tempo, bounded at 100 kB). Runtime metadata, fetched once per runtime per process, is not counted.",
	}, []string{"phase"})
	snIncentiveAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "age_seconds",
		Help: "Seconds since the newest stored incentive census was read. A tempo is about 72 minutes.",
	})
	snIncentiveLagTempos = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "lag_tempos",
		Help: "Tempos the chain's SubnetEpochIndex is ahead of the newest stored census, at the last index check.",
	})
	snIncentiveStale = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "stale",
		Help: "1 when the newest census is stale for the reason: fallback_row (the reference validator's row is the stopgap fallback), reference_inactive (its LastUpdate is outside the activity cutoff), low_validator_trust, no_reference_validator.",
	}, []string{"reason"})
	snIncentiveLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_incentive", Name: "last_success_timestamp_seconds",
		Help: "Unix time the newest incentive census was stored.",
	})

	snHeadSummaryUploads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_head_summary", Name: "uploads_total",
		Help: "Validator head summary uploads (POST /sn/head-summary), by result: stored, kept (the tempo's summary at an equal or later snapshot is already stored), invalid (400: malformed, not canonical, inconsistent or wrongly signed), not_accepted (403: the hotkey is not an accepted validator), rate_limited (429), too_large (413), unavailable (5xx).",
	}, []string{"result"})
	snHeadSummaryLastStored = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_head_summary", Name: "last_stored_timestamp_seconds",
		Help: "Unix time the operator last stored a validator head summary. The validator sends one per tempo, about every 72 minutes.",
	})
	snHeadSummaryEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_head_summary", Name: "entries",
		Help: "Eligible head UIDs in the last stored head summary. Zero while the head is empty.",
	})
)

// Head summary upload results, as metric labels.
const (
	snHeadSummaryResultInvalid     = "invalid"
	snHeadSummaryResultNotAccepted = "not_accepted"
	snHeadSummaryResultRateLimited = "rate_limited"
	snHeadSummaryResultTooLarge    = "too_large"
	snHeadSummaryResultUnavailable = "unavailable"
)

var snHeadSummaryResults = []string{SnHeadSummaryStored, SnHeadSummaryKept, snHeadSummaryResultInvalid, snHeadSummaryResultNotAccepted, snHeadSummaryResultRateLimited, snHeadSummaryResultTooLarge, snHeadSummaryResultUnavailable}

func init() {
	for _, kind := range []string{model.StFleetBoundEventKind, model.SnFleetBindingRevokedEventKind, model.SnFleetBindingCleanedEventKind} {
		snFleetBindingSyncEvents.WithLabelValues(kind)
	}
	for _, reason := range snFleetBindingSyncErrorReasons {
		snFleetBindingSyncErrors.WithLabelValues(reason)
	}
	for _, status := range []string{model.SnUidHeldYes, model.SnUidHeldNo, model.SnUidHeldUnknown} {
		snClaimedFleetsGauge.WithLabelValues(status)
	}
	for _, result := range snIncentiveReadResults {
		snIncentiveReads.WithLabelValues(result)
	}
	for _, reason := range stmetagraph.Reasons {
		snIncentiveReadErrors.WithLabelValues(reason)
	}
	for _, phase := range []string{"index", "census"} {
		snIncentiveResponseBytes.WithLabelValues(phase)
	}
	for _, reason := range model.SnIncentiveStaleReasons {
		snIncentiveStale.WithLabelValues(reason)
	}
	for _, result := range snHeadSummaryResults {
		snHeadSummaryUploads.WithLabelValues(result)
	}
	prometheus.MustRegister(
		snFleetBindingSyncLagBlocks,
		snFleetBindingSyncEvents,
		snFleetBindingSyncErrors,
		snFleetBindingSyncLastSuccess,
		snClaimedFleetsGauge,
		snClaimedFleetMembersGauge,
		snIncentiveReads,
		snIncentiveReadErrors,
		snIncentiveResponseBytes,
		snIncentiveAgeSeconds,
		snIncentiveLagTempos,
		snIncentiveStale,
		snIncentiveLastSuccess,
		snHeadSummaryUploads,
		snHeadSummaryLastStored,
		snHeadSummaryEntries,
	)
}

// Counts one upload by its result or refusal.
func observeSnHeadSummaryUpload(result string, err error) {
	switch {
	case err == nil:
		snHeadSummaryUploads.WithLabelValues(result).Inc()
	case errors.Is(err, ErrSnHeadSummaryInvalid):
		snHeadSummaryUploads.WithLabelValues(snHeadSummaryResultInvalid).Inc()
	case errors.Is(err, ErrSnHeadSummaryNotAccepted):
		snHeadSummaryUploads.WithLabelValues(snHeadSummaryResultNotAccepted).Inc()
	case errors.Is(err, ErrSnHeadSummaryRateLimited):
		snHeadSummaryUploads.WithLabelValues(snHeadSummaryResultRateLimited).Inc()
	case errors.Is(err, ErrSnHeadSummaryTooLarge):
		snHeadSummaryUploads.WithLabelValues(snHeadSummaryResultTooLarge).Inc()
	default:
		snHeadSummaryUploads.WithLabelValues(snHeadSummaryResultUnavailable).Inc()
	}
}

// ObserveSnHeadSummaryTooLarge counts a body refused before it was read.
func ObserveSnHeadSummaryTooLarge() {
	observeSnHeadSummaryUpload("", ErrSnHeadSummaryTooLarge)
}

func observeSnFleetBindingSync(result *SnFleetBindingSyncResult, err error) {
	if result != nil && 0 < result.NextBlock && result.NextBlock-1 <= result.FinalizedBlock {
		snFleetBindingSyncLagBlocks.Set(float64(result.FinalizedBlock - (result.NextBlock - 1)))
	}
	if err != nil {
		reason := snFleetBindingSyncErrorChain
		if syncErr, ok := err.(*snFleetBindingSyncError); ok {
			reason = syncErr.reason
		}
		snFleetBindingSyncErrors.WithLabelValues(reason).Inc()
		return
	}
	snFleetBindingSyncLastSuccess.Set(float64(time.Now().Unix()))
}

func observeSnIncentiveSnapshot(snapshot *model.SnIncentiveSnapshot, now time.Time) {
	for _, reason := range model.SnIncentiveStaleReasons {
		value := 0.0
		for _, stale := range snapshot.StaleReasons {
			if stale == reason {
				value = 1
			}
		}
		snIncentiveStale.WithLabelValues(reason).Set(value)
	}
	snIncentiveAgeSeconds.Set(now.Sub(snapshot.ReadAt).Seconds())
	snIncentiveLagTempos.Set(0)
	snIncentiveLastSuccess.Set(float64(snapshot.ReadAt.Unix()))
}
