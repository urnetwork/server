package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// ProviderEgressHealthClassResult is one class's ok/total tally over the
// destinations a single run SAMPLED, not over the whole destination table. The
// prober draws a bounded random subset of each class per run (see the
// qualityprobe/egresshealth package), so `{"cdn":{"ok":4,"total":5}}` means
// four of the five drawn this pass, out of a much larger table.
type ProviderEgressHealthClassResult struct {
	OK    int `json:"ok"`
	Total int `json:"total"`
}

// ProviderEgressHealth is one egress-health run for one provider: does this
// provider actually carry traffic to the real internet, across several
// independent classes of destination.
//
// Every count is over the loads the run sampled and measured, after every
// load's retries (connect/GEOMAP.md §11.3): a load that passed on some attempt
// is ok, one that failed every attempt is a failure, and that is what the
// egress indexer's success ratio counts. A load whose tunnel was gone and could
// not be re-created is neither -- it is out of OKCount and Total and named in
// NotMeasuredNames -- and so is a canary, an unscored load from a place its
// site is marked incompatible with.
//
// # Reputation is dissolved
//
// The sites that used to form an unscored reputation class are ordinary site
// destinations now, sampled and scored like the rest (GEOMAP §11.3). The
// ReputationOK/ReputationTotal/ReputationFailedNames columns are kept for the
// release that still submits them, always zero and empty.
type ProviderEgressHealth struct {
	// RunId survives submission retries. CycleStartedAt is the stable durable
	// admission token; rolling quota comes from accepted history, not its age.
	RunId            server.Id
	CycleStartedAt   time.Time
	ClientId         server.Id
	MeasuredAt       time.Time
	UrlProbeEvidence *egresshealth.UrlProbeEvidence
	// OKCount and Total cover the scored loads the run measured: every class,
	// after retries, less what ingest left out of the counts (see
	// UnscoredFailedNames).
	OKCount int
	Total   int
	// ClassResults is the per-class tally of the same loads. A partial failure
	// is only diagnosable per class: "ok=44/50" alone says nothing, while
	// "dns=6/6 cdn=0/10 site=26/26" says the tunnel carries bytes and resolves
	// names but is being refused by content providers -- a completely
	// different outcome from a run with no successes.
	ClassResults map[string]ProviderEgressHealthClassResult
	// ReputationOK/ReputationTotal: always zero since the class dissolved.
	ReputationOK    int
	ReputationTotal int
	// FailedNames is the comma-joined names of the scored loads that failed
	// every attempt. It is the only record of WHICH destinations a given
	// provider failed on a given pass, since the sample is drawn fresh each
	// run.
	FailedNames string
	// ReputationFailedNames: always empty since the class dissolved.
	ReputationFailedNames string
	// TLSAuthenticationFailure records that at least one sampled HTTPS peer did
	// not authenticate the requested hostname. It is not a score component: one
	// forged identity is sufficient to make the provider unsafe.
	TLSAuthenticationFailure bool
	// NotMeasuredCount/NotMeasuredNames are the loads whose tunnel was gone for
	// their last attempt and could not be re-created: neither passed nor
	// failed, and out of every count, so a short run is told from a thin one.
	NotMeasuredCount int
	NotMeasuredNames string
	// CanaryPassedNames/CanaryFailedNames are the canaries of this run: sites
	// loaded, unscored, from a place they are marked incompatible with, which
	// is how the pool learns a site works there again (GEOMAP §11.4).
	CanaryPassedNames string
	CanaryFailedNames string
	// ShortClasses is the comma-joined classes too thin, for the provider's
	// place, to fill their sample: the pool's signal, not the provider's.
	ShortClasses string
	// UnscoredFailedNames is the comma-joined failed loads the server left out
	// of the counts at ingest: a site on probation, or one marked incompatible
	// with the provider's place (see ScoreProviderEgressHealth).
	UnscoredFailedNames string
}

// Takes out of a submitted run's counts every failed load the pool says must
// not count against this provider: a site on probation, which has to earn the
// right to cost a provider a tier, and a site marked incompatible with the
// provider's place, which says nothing about the exits there (GEOMAP §11.3,
// §11.4). Each such failure leaves FailedNames, its class's total and Total,
// and is named in UnscoredFailedNames.
//
// Only failures can be taken out here: a run reports which loads failed, not
// which passed. The probe task, which holds every load of the run, submits a
// run already scored this way -- passes included -- so for it this finds
// nothing; for any other submitter it is the part of the rule the wire allows.
// A failed name whose class the run's tally cannot give back is left counted:
// taking it out would make the tally inconsistent with itself.
func ScoreProviderEgressHealth(
	health *ProviderEgressHealth,
	place ProviderEgressPlace,
	scoring *ProviderEgressHealthScoring,
) *ProviderEgressHealth {
	scored := *health
	scored.ClassResults = map[string]ProviderEgressHealthClassResult{}
	for class, tally := range health.ClassResults {
		scored.ClassResults[class] = tally
	}
	kept := []string{}
	unscored := []string{}
	if health.UnscoredFailedNames != "" {
		unscored = append(unscored, splitProviderEgressNames(health.UnscoredFailedNames)...)
	}
	for _, name := range splitProviderEgressNames(health.FailedNames) {
		if scoring.Scores(name, place) {
			kept = append(kept, name)
			continue
		}
		class := scoring.Class(name)
		tally, ok := scored.ClassResults[class]
		if !ok || tally.Total <= tally.OK || scored.Total <= scored.OKCount {
			kept = append(kept, name)
			continue
		}
		tally.Total -= 1
		scored.ClassResults[class] = tally
		scored.Total -= 1
		unscored = append(unscored, name)
	}
	scored.FailedNames = strings.Join(kept, ",")
	scored.UnscoredFailedNames = strings.Join(unscored, ",")
	return &scored
}

// Splits a comma-joined name list, dropping empty elements and the "…+N more"
// marker a truncated list ends with.
func splitProviderEgressNames(names string) []string {
	out := []string{}
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if index := strings.Index(name, "…"); 0 <= index {
			name = strings.TrimSpace(name[:index])
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Records one accepted URL result idempotently, updates matching cycle progress
// atomically, and retains the newest diagnostic/security snapshot. An old cycle
// may contribute evidence but cannot advance a different cycle's quota.
func SetProviderEgressHealth(ctx context.Context, health *ProviderEgressHealth) {
	if health.UrlProbeEvidence != nil {
		// Arrival time must not rejuvenate a delayed measurement. Do not mutate
		// the caller's receipt; publication retries may share the same value.
		measured := *health
		measured.MeasuredAt = health.UrlProbeEvidence.MeasuredAt
		health = &measured
	}
	classResults := health.ClassResults
	if classResults == nil {
		classResults = map[string]ProviderEgressHealthClassResult{}
	}
	// marshalled here rather than handed to pgx as a map, so the column always
	// receives a jsonb document of a known shape (an absent map becomes `{}`,
	// not sql NULL, and the column is NOT NULL)
	classResultsJson, err := json.Marshal(classResults)
	server.Raise(err)
	server.Raise(health.UrlProbeEvidence.ValidateOutcome(health.OKCount, health.Total, health.TLSAuthenticationFailure))
	var evidenceJson any
	policyVersion := 0
	if health.UrlProbeEvidence != nil {
		policyVersion = health.UrlProbeEvidence.PolicyVersion
		encoded, err := json.Marshal(health.UrlProbeEvidence)
		server.Raise(err)
		evidenceJson = string(encoded)
	}

	server.Tx(ctx, func(tx server.PgTx) {
		runId := health.RunId
		if runId == (server.Id{}) {
			// Legacy local callers have no run token. Preserve idempotency of
			// their measured timestamp; production submitters carry RunId.
			identity := sha256.Sum256([]byte(health.ClientId.String() + ":" + health.MeasuredAt.UTC().Format(time.RFC3339Nano)))
			copy(runId[:], identity[:len(runId)])
		}
		var cycleStartedAt *time.Time
		if !health.CycleStartedAt.IsZero() {
			t := health.CycleStartedAt.UTC()
			cycleStartedAt = &t
		}
		cycleAdmitted := false
		securityOnly := health.Total == 0 && health.UrlProbeEvidence != nil && len(health.UrlProbeEvidence.Security) > 0
		if cycleStartedAt != nil && (health.Total == 1 || securityOnly) && policyVersion == SelectedProviderUrlProbePolicyVersion() {
			// Serialize URL receipt acceptance before reading the rolling window.
			// The later UPDATE uses a fresh statement snapshot after this lock,
			// so another acknowledged receipt cannot disappear behind MVCC.
			rows, err := tx.Query(ctx, `SELECT true FROM provider_egress_probe_cycle
				WHERE client_id=$1 AND cycle_started_at=$2 FOR UPDATE`, health.ClientId, cycleStartedAt)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&cycleAdmitted))
				}
			})
		}
		urlProbe := cycleAdmitted && health.Total == 1
		var securityMeasuredAt *time.Time
		if health.UrlProbeEvidence == nil && (health.TLSAuthenticationFailure || health.OKCount > 0) {
			t := health.MeasuredAt.UTC()
			securityMeasuredAt = &t
		}
		inserted, err := tx.Exec(ctx, `
			INSERT INTO provider_egress_health_history
				(run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure, cycle_started_at, url_probe, url_probe_evidence, url_probe_policy_version)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10::jsonb,$11)
			ON CONFLICT (run_id) DO NOTHING`,
			runId, health.ClientId, health.MeasuredAt.UTC(), health.OKCount, health.Total,
			string(classResultsJson), health.TLSAuthenticationFailure, cycleStartedAt, urlProbe, evidenceJson, policyVersion)
		server.Raise(err)
		if inserted.RowsAffected() == 0 {
			return
		}
		if health.UrlProbeEvidence != nil {
			for _, event := range health.UrlProbeEvidence.Security {
				destinationJson, err := json.Marshal(event.Destination)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure)
					VALUES($1,$2,$3::jsonb,$4,$5)
					ON CONFLICT(client_id,url_key) DO UPDATE SET
						destination=EXCLUDED.destination,
						measured_at=EXCLUDED.measured_at,
						tls_failure=CASE WHEN provider_egress_url_security.measured_at=EXCLUDED.measured_at
							THEN provider_egress_url_security.tls_failure OR EXCLUDED.tls_failure ELSE EXCLUDED.tls_failure END
					WHERE provider_egress_url_security.measured_at <= EXCLUDED.measured_at`,
					health.ClientId, egresshealth.UrlProbeDestinationKey(event.Destination), string(destinationJson), event.MeasuredAt.UTC(), event.TlsFailure))
			}
		}
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
				reputation_failed_names,
				tls_authentication_failure,
				not_measured_count,
				not_measured_names,
				canary_passed_names,
				canary_failed_names,
				short_classes,
				unscored_failed_names,
				security_measured_at,
				legacy_tls_authentication_failure
			)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
			ON CONFLICT (client_id) DO UPDATE
			SET
				measured_at = GREATEST(provider_egress_health.measured_at, EXCLUDED.measured_at),
				ok_count = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.ok_count ELSE provider_egress_health.ok_count END,
				total_count = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.total_count ELSE provider_egress_health.total_count END,
				class_results = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.class_results ELSE provider_egress_health.class_results END,
				reputation_ok = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.reputation_ok ELSE provider_egress_health.reputation_ok END,
				reputation_total = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.reputation_total ELSE provider_egress_health.reputation_total END,
				failed_names = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.failed_names ELSE provider_egress_health.failed_names END,
				reputation_failed_names = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.reputation_failed_names ELSE provider_egress_health.reputation_failed_names END,
				tls_authentication_failure = CASE
					WHEN EXCLUDED.security_measured_at IS NULL THEN provider_egress_health.tls_authentication_failure
					WHEN provider_egress_health.security_measured_at IS NULL OR provider_egress_health.security_measured_at < EXCLUDED.security_measured_at THEN EXCLUDED.tls_authentication_failure
					WHEN provider_egress_health.security_measured_at = EXCLUDED.security_measured_at THEN provider_egress_health.tls_authentication_failure OR EXCLUDED.tls_authentication_failure
					ELSE provider_egress_health.tls_authentication_failure END,
				security_measured_at = GREATEST(provider_egress_health.security_measured_at, EXCLUDED.security_measured_at),
				legacy_tls_authentication_failure = provider_egress_health.legacy_tls_authentication_failure OR EXCLUDED.legacy_tls_authentication_failure,
				not_measured_count = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.not_measured_count ELSE provider_egress_health.not_measured_count END,
				not_measured_names = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.not_measured_names ELSE provider_egress_health.not_measured_names END,
				canary_passed_names = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.canary_passed_names ELSE provider_egress_health.canary_passed_names END,
				canary_failed_names = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.canary_failed_names ELSE provider_egress_health.canary_failed_names END,
				short_classes = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.short_classes ELSE provider_egress_health.short_classes END,
				unscored_failed_names = CASE WHEN provider_egress_health.measured_at < EXCLUDED.measured_at THEN EXCLUDED.unscored_failed_names ELSE provider_egress_health.unscored_failed_names END
			WHERE provider_egress_health.measured_at < EXCLUDED.measured_at
				OR EXCLUDED.legacy_tls_authentication_failure
				OR (EXCLUDED.security_measured_at IS NOT NULL AND (provider_egress_health.security_measured_at IS NULL OR provider_egress_health.security_measured_at <= EXCLUDED.security_measured_at))
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
			health.TLSAuthenticationFailure,
			health.NotMeasuredCount,
			health.NotMeasuredNames,
			health.CanaryPassedNames,
			health.CanaryFailedNames,
			health.ShortClasses,
			health.UnscoredFailedNames,
			securityMeasuredAt,
			health.UrlProbeEvidence == nil && health.TLSAuthenticationFailure,
		))
		// The projection is not a last-run boolean. One unrelated clean URL
		// cannot clear another URL, nor can it clear an unidentified legacy TLS
		// finding. Exact-URL rows remain authoritative under concurrent reports.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_health AS health SET
			tls_authentication_failure = health.legacy_tls_authentication_failure OR EXISTS (
				SELECT 1 FROM provider_egress_url_security AS security
				WHERE security.client_id=health.client_id AND security.tls_failure
			) WHERE health.client_id=$1`, health.ClientId))
		if cycleAdmitted {
			server.RaisePgResult(tx.Exec(ctx, `
				WITH current AS MATERIALIZED (
					SELECT client_id, GREATEST(latest_result_at,$5) AS rolling_at
					FROM provider_egress_probe_cycle WHERE client_id=$1 AND cycle_started_at=$2
				), recent AS MATERIALIZED (
					SELECT current.client_id, successes.*, runs.* FROM current
					CROSS JOIN LATERAL (`+providerUrlProbeSuccessWindowSql("current.client_id", "current.rolling_at")+`) AS successes
					CROSS JOIN LATERAL (`+providerUrlProbeRunWindowSql("current.client_id", "current.rolling_at")+`) AS runs
				)
				UPDATE provider_egress_probe_cycle AS cycle SET
					success_count = recent.success_count,
					error_count = error_count + $4,
					outcome_count = outcome_count + $8,
					latest_result_at = GREATEST(latest_result_at, $5),
					next_attempt_at = CASE WHEN latest_result_at > $5 THEN next_attempt_at
						WHEN recent.run_count >= $6 AND NOT (`+providerHasUrlSecurityExceptionSql("cycle.client_id")+`) THEN recent.oldest_run_at + ($7 * interval '1 second')
						ELSE `+providerUrlProbePacedAttemptSql("cycle", "$5", "$3", "recent.run_count < $6")+` END
				FROM recent WHERE cycle.client_id = recent.client_id`,
				health.ClientId, cycleStartedAt, health.OKCount, health.Total-health.OKCount,
				health.MeasuredAt.UTC(), ProviderUrlProbeRunTarget, (ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom).Seconds(), health.Total))
		}
	})
}

// Accepted measured URL outcomes contribute for strictly less than eight hours.
// Security exceptions persist until later authenticated TLS from the same URL.
const ProviderEgressHealthMaxAge = 8 * time.Hour

// ProviderEgressHealthCounts is the ok/total tally alone, for consumers that
// only need to decide "did this provider carry traffic" in bulk. The heavy
// fields (per-class results, failure name lists) are diagnostics and are left
// unread, so a whole-table load stays cheap.
type ProviderEgressHealthCounts struct {
	MeasuredAt      time.Time
	FirstMeasuredAt time.Time
	OKCount         int
	Total           int
}

// Only the explicitly selected URL policy contributes. Legacy/unknown versions
// remain stored; their weaker or unknown success contracts never become quality.
func providerEgressHealthWindowSql() string {
	return fmt.Sprintf(`
	SELECT client_id, measured_at, ok_count, total_count, class_results
	FROM provider_egress_health_history
	WHERE measured_at > $1 AND measured_at <= $2 AND total_count = 1
	AND url_probe_policy_version = %d
`, SelectedProviderUrlProbePolicyVersion())
}

// Aggregates accepted URL successes and measured errors in the eight-hour
// window. Unmeasured attempts and submission replays contribute nothing.
func GetAllProviderEgressHealthCounts(ctx context.Context) map[server.Id]ProviderEgressHealthCounts {
	healthCounts, _ := getAllProviderEgressHealthCountsSnapshot(ctx)
	return healthCounts
}

// Keep the exact query endpoint with its already loaded map. Publication
// diagnostics must not relabel the later export time as the evidence window.
func getAllProviderEgressHealthCountsSnapshot(ctx context.Context) (map[server.Id]ProviderEgressHealthCounts, time.Time) {
	return getProviderEgressHealthCountsSnapshot(ctx, nil)
}

// The same window and aggregate for only clientIds, or for every provider
// when clientIds is nil.
func getProviderEgressHealthCountsSnapshot(ctx context.Context, clientIds []server.Id) (map[server.Id]ProviderEgressHealthCounts, time.Time) {
	healthCounts := map[server.Id]ProviderEgressHealthCounts{}

	now := server.NowUtc()
	minMeasuredAt := now.Add(-ProviderEgressHealthMaxAge)

	query := `SELECT client_id, MAX(measured_at), MIN(measured_at), SUM(ok_count), SUM(total_count)
			FROM (` + providerEgressHealthWindowSql() + `) AS evidence GROUP BY client_id`
	args := []any{minMeasuredAt.UTC(), now.UTC()}
	if clientIds != nil {
		query = `SELECT client_id, MAX(measured_at), MIN(measured_at), SUM(ok_count), SUM(total_count)
			FROM (` + providerEgressHealthWindowSql() + `) AS evidence WHERE client_id = ANY($3) GROUP BY client_id`
		args = append(args, clientIds)
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			query,
			args...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var counts ProviderEgressHealthCounts
				server.Raise(result.Scan(
					&clientId,
					&counts.MeasuredAt,
					&counts.FirstMeasuredAt,
					&counts.OKCount,
					&counts.Total,
				))
				healthCounts[clientId] = counts
			}
		})
	})

	return healthCounts, now
}

// GetAllProviderEgressTLSAuthenticationFailedClientIds returns every provider
// with any outstanding exact-URL TLS finding or retained legacy quarantine.
//
// There is intentionally no age cutoff. A certificate-authentication failure
// is positive evidence that the path is unsafe, not a quality sample that
// gradually becomes unknown. Only a newer authenticated response from each
// affected URL clears its finding. An unidentified legacy finding is retained.
func GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx context.Context) map[server.Id]bool {
	return getProviderEgressTlsAuthenticationFailedClientIds(ctx, nil)
}

// The same findings for only clientIds, or for every provider when clientIds
// is nil.
func getProviderEgressTlsAuthenticationFailedClientIds(ctx context.Context, clientIds []server.Id) map[server.Id]bool {
	failed := map[server.Id]bool{}
	query := `
			SELECT client_id
			FROM provider_egress_health
			WHERE tls_authentication_failure OR legacy_tls_authentication_failure
			UNION SELECT client_id FROM provider_egress_url_security WHERE tls_failure
			`
	args := []any{}
	if clientIds != nil {
		query = `
			SELECT client_id
			FROM provider_egress_health
			WHERE client_id = ANY($1) AND (tls_authentication_failure OR legacy_tls_authentication_failure)
			UNION SELECT client_id FROM provider_egress_url_security WHERE client_id = ANY($1) AND tls_failure
			`
		args = append(args, clientIds)
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			query,
			args...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				failed[clientId] = true
			}
		})
	})
	return failed
}

// GetProviderEgressHealth reads a provider's latest egress-health run, or nil
// when the provider has never been measured. Never measured is not the same as
// measured-unhealthy, so it is a nil result rather than a zero-valued one:
// a caller that cannot tell those apart would read every unprobed provider as
// a run with no successes.
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
				reputation_failed_names,
				tls_authentication_failure,
				not_measured_count,
				not_measured_names,
				canary_passed_names,
				canary_failed_names,
				short_classes,
				unscored_failed_names
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
					&h.TLSAuthenticationFailure,
					&h.NotMeasuredCount,
					&h.NotMeasuredNames,
					&h.CanaryPassedNames,
					&h.CanaryFailedNames,
					&h.ShortClasses,
					&h.UnscoredFailedNames,
				))
				h.ClassResults = map[string]ProviderEgressHealthClassResult{}
				server.Raise(json.Unmarshal(classResultsJson, &h.ClassResults))
				health = h
			}
		})
	})

	return health
}
