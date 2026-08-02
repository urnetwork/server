package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
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
