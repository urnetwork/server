package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server/v2026"
)

// ProviderEgressHealthClassResult is one class's ok/total tally over the
// destinations a single run SAMPLED, not over the whole destination table. The
// prober draws a bounded random subset of each class per run (see the
// operator-proxy's egresshealth package), so `{"cdn":{"ok":4,"total":5}}` means
// four of the five drawn this pass, out of a much larger table.
type ProviderEgressHealthClassResult struct {
	OK    int `json:"ok"`
	Total int `json:"total"`
}

// ProviderEgressHealth is one egress-health run for one provider: does this
// provider actually carry traffic to the real internet, across several
// independent classes of destination.
//
// # Reputation is not health
//
// ReputationOK/ReputationTotal are stored because they are measured in the
// same pass, and they are deliberately NOT part of OKCount/Total. This mirrors
// the operator-proxy's egresshealth package comment, which calls its own
// version of this "the one thing in this package most likely to be 'fixed'
// into a bug", and the reasoning holds identically server-side.
//
// The reputation class measures whether large vendors treat the exit ip as a
// datacenter address. Nearly every honest hosted provider fails most of it,
// because it IS hosted -- that is a fact about the vendor's ip intelligence
// feed, not about whether the provider carries traffic. Folding it into
// OKCount/Total would take a provider that carried every byte it was asked for
// and score it as partly broken, and the providers it would punish hardest are
// the well-run datacenter ones. Nothing downstream may add these figures into
// OKCount, Total, or any health score derived from them.
//
// The two failure name lists are kept apart for the same reason: FailedNames
// is destinations that mean the provider is not carrying traffic, while
// ReputationFailedNames is vendors that refused a datacenter ip. Merged, they
// would read as one longer failure list.
type ProviderEgressHealth struct {
	ClientId   server.Id
	MeasuredAt time.Time
	// OKCount and Total cover the SCORED classes only (dns, connectivity, cdn,
	// site), over this run's sample.
	OKCount int
	Total   int
	// ClassResults is the per-class tally for the scored classes only. A
	// partial failure is only diagnosable per class: "ok=14/26" alone says
	// nothing, while "dns=4/4 cdn=0/5 site=12/12" says the tunnel carries
	// bytes and resolves names but is being refused by content providers --
	// a completely different fault from a total blackhole.
	ClassResults map[string]ProviderEgressHealthClassResult
	// ReputationOK/ReputationTotal: stored, never scored. See the type comment.
	ReputationOK    int
	ReputationTotal int
	// FailedNames is the comma-joined names of the scored destinations that
	// failed. It is the only record of WHICH destinations a given provider was
	// asked for on a given pass, since the sample is drawn fresh each run.
	FailedNames string
	// ReputationFailedNames is the comma-joined names of the reputation
	// destinations that refused. Separate from FailedNames, deliberately.
	ReputationFailedNames string
}

// SetProviderEgressHealth records a provider's latest egress-health run.
//
// The row is keyed on client_id alone, so a new run REPLACES the previous one:
// this is the current picture per provider that a consumer reads, not a
// history. That mirrors provider_egress_location's lifecycle exactly. If
// trending is wanted later it belongs in a separate partitioned append table,
// not in a second key column here -- the read path for "is this provider
// carrying traffic right now" wants one row per provider and nothing else.
//
// Nothing here folds reputation into the health figures; see the
// ProviderEgressHealth comment for why that must stay true.
func SetProviderEgressHealth(ctx context.Context, health *ProviderEgressHealth) {
	classResults := health.ClassResults
	if classResults == nil {
		classResults = map[string]ProviderEgressHealthClassResult{}
	}
	// marshalled here rather than handed to pgx as a map, so the column always
	// receives a jsonb document of a known shape (an absent map becomes `{}`,
	// not sql NULL, and the column is NOT NULL)
	classResultsJson, err := json.Marshal(classResults)
	server.Raise(err)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_egress_health (
				client_id,
				measured_at,
				ok_count,
				total_count,
				class_results,
				reputation_ok,
				reputation_total,
				failed_names,
				reputation_failed_names
			)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)
			ON CONFLICT (client_id) DO UPDATE
			SET
				measured_at = $2,
				ok_count = $3,
				total_count = $4,
				class_results = $5::jsonb,
				reputation_ok = $6,
				reputation_total = $7,
				failed_names = $8,
				reputation_failed_names = $9
			`,
			health.ClientId,
			// measured_at is a naive timestamp column holding utc, as
			// everywhere else in this schema
			health.MeasuredAt.UTC(),
			health.OKCount,
			health.Total,
			string(classResultsJson),
			health.ReputationOK,
			health.ReputationTotal,
			health.FailedNames,
			health.ReputationFailedNames,
		))
	})
}

// ProviderEgressHealthMaxAge is how long an egress-health measurement is
// treated as current. Past it the provider is indistinguishable from one never
// measured, and the gate fails it closed.
//
// 24h, against ProviderEgressLocationMaxAge's 7 days, because the two decay at
// different rates. Where a provider egresses from is a property of its network
// and rarely changes; whether it still carries traffic is a property of the
// moment and can change without warning or notice -- a provider that stops
// forwarding stays connected and keeps accepting clients, so nothing else in
// the system reveals it.
//
// The value is a floor on how bad the list can get, not a tuning knob: it is
// the longest a provider can blackhole while still being advertised. Shortening
// it shrinks that window at the cost of demanding more probe throughput, since
// every gated provider must be re-measured inside it or it drops out of the
// list.
const ProviderEgressHealthMaxAge = 24 * time.Hour

// ProviderEgressHealthCounts is the ok/total tally alone, for consumers that
// only need to decide "did this provider carry traffic" in bulk. The heavy
// fields (per-class results, failure name lists) are diagnostics and are left
// unread, so a whole-table load stays cheap.
type ProviderEgressHealthCounts struct {
	MeasuredAt time.Time
	OKCount    int
	Total      int
}

// GetAllProviderEgressHealthCounts reads every provider's latest egress-health
// tally in one query, keyed by client id.
//
// This exists for UpdateClientScores, which walks the whole provider population
// on every pass and needs each one's health. A per-client GetProviderEgressHealth
// there would be one round trip per provider per pass; the table holds exactly
// one row per ever-probed provider (SetProviderEgressHealth upserts on
// client_id), which is hundreds of rows, so the whole thing is cheaper to hold
// in a map than to query piecemeal.
//
// A provider that has never been measured has no entry, exactly as
// GetProviderEgressHealth returns nil for it. Never measured is not the same as
// measured-unhealthy and the two must stay distinguishable to the caller.
//
// # Stale evidence is not evidence
//
// Only measurements newer than ProviderEgressHealthMaxAge are returned. A
// provider whose measurement has aged out is absent from the map and therefore
// fails passesHealth closed, exactly as a never-measured one does -- which is
// the correct reading, because both mean "no current evidence this provider
// carries traffic."
//
// Omitting this bound published blackholes. Health drives the gate but nothing
// re-measured it on its own schedule: the due queue keyed re-probes off
// provider_egress_location's age, so a provider with a fresh location was never
// re-probed and its health tally sat unchanged for days. On beta that left
// 98.6% of gated providers advertised on evidence older than six hours, and 12
// of 12 sampled from the stalest cohort answered ok=0/131 when probed -- total
// blackholes, still in the public list, because a measurement taken days ago
// said they were fine. GetAllProviderEgressCountryCodes has always bounded its
// half this way; this is the missing symmetry.
func GetAllProviderEgressHealthCounts(ctx context.Context) map[server.Id]ProviderEgressHealthCounts {
	healthCounts := map[server.Id]ProviderEgressHealthCounts{}

	minMeasuredAt := server.NowUtc().Add(-ProviderEgressHealthMaxAge)

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				measured_at,
				ok_count,
				total_count
			FROM provider_egress_health
			WHERE measured_at >= $1
			`,
			minMeasuredAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var counts ProviderEgressHealthCounts
				server.Raise(result.Scan(
					&clientId,
					&counts.MeasuredAt,
					&counts.OKCount,
					&counts.Total,
				))
				healthCounts[clientId] = counts
			}
		})
	})

	return healthCounts
}

// GetProviderEgressHealth reads a provider's latest egress-health run, or nil
// when the provider has never been measured. Never measured is not the same as
// measured-unhealthy, so it is a nil result rather than a zero-valued one:
// a caller that cannot tell those apart would read every unprobed provider as
// a total blackhole.
func GetProviderEgressHealth(ctx context.Context, clientId server.Id) *ProviderEgressHealth {
	var health *ProviderEgressHealth

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				measured_at,
				ok_count,
				total_count,
				class_results,
				reputation_ok,
				reputation_total,
				failed_names,
				reputation_failed_names
			FROM provider_egress_health
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				h := &ProviderEgressHealth{ClientId: clientId}
				var classResultsJson []byte
				server.Raise(result.Scan(
					&h.MeasuredAt,
					&h.OKCount,
					&h.Total,
					&classResultsJson,
					&h.ReputationOK,
					&h.ReputationTotal,
					&h.FailedNames,
					&h.ReputationFailedNames,
				))
				h.ClassResults = map[string]ProviderEgressHealthClassResult{}
				server.Raise(json.Unmarshal(classResultsJson, &h.ClassResults))
				health = h
			}
		})
	})

	return health
}
