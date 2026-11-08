package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

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
	// with the provider's place (see ScoreProviderEgressHealth), or a security
	// recheck of a URL with an open TLS finding, or a URL outcome on probation
	// (storeProviderEgressHealth).
	UnscoredFailedNames string
	// UrlSource is the catalog a URL outcome's destination came from, which
	// the operator ingest decides from the served catalog
	// (provider_egress_url_source.go). Zero, the general catalog, for a report
	// without URL evidence.
	UrlSource ProviderEgressUrlSource
	// UrlProbation is a URL outcome on a destination that has not graduated
	// into scoring: the ingest sets it for every country site, and the store
	// also for a general destination the pool holds on probation. Recorded and
	// in the rolling quota, never counted.
	UrlProbation bool
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

// The advisory lock a store holds on one run id until its transaction ends.
// Only the posts of one run wait on each other.
func providerEgressHealthRunLockKey(runId server.Id) string {
	return fmt.Sprintf("provider-egress-health-run-v1:%s", runId)
}

// An insert found no partition of the history for the result's day.
type providerEgressHealthPartitionMissingError struct {
	measuredAt time.Time
}

// Implements error.
func (self *providerEgressHealthPartitionMissingError) Error() string {
	return fmt.Sprintf("no %s partition holds %s", providerEgressHealthHistoryTable, self.measuredAt.Format(time.RFC3339))
}

// Raises a history insert's failure, a missing day partition as its own
// error so the store can create the day and store once more.
func raiseProviderEgressHealthInsertErr(err error, measuredAt time.Time) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.CheckViolation &&
		pgErr.TableName == providerEgressHealthHistoryTable &&
		pgErr.ConstraintName == "" {
		panic(&providerEgressHealthPartitionMissingError{measuredAt: measuredAt})
	}
	server.Raise(err)
}

// Records one accepted URL result idempotently, updates matching cycle progress
// atomically, and retains the newest diagnostic/security snapshot. An old cycle
// may contribute evidence but cannot advance a different cycle's quota.
//
// A run id is stored once across the retained history: a post whose run id is
// already stored stores and publishes nothing, whatever its measured_at. The
// history's key is (run_id, measured_at) once partitioned (C2), so the run id
// is looked up under a lock per run, at read committed: a second post of one
// run waits for the first to commit and then sees its row. The insert names no
// conflict target, so it is the same statement on the plain table (unique
// run_id) and on the partitioned one. An insert that finds no partition for
// its day creates the day (the registry's EnsureRangeFor) and stores once more.
func SetProviderEgressHealth(ctx context.Context, health *ProviderEgressHealth) {
	setProviderEgressHealthAt(ctx, health, server.NowUtc())
}

// SetProviderEgressHealth, reporting how the store counted the outcome, which
// the operator ingest's per-source outcome counter reads.
func StoreProviderEgressHealth(ctx context.Context, health *ProviderEgressHealth) ProviderEgressHealthStored {
	return setProviderEgressHealthAt(ctx, health, server.NowUtc())
}

// The store as the clock reads now, which only judges whether a missing day
// may be created; a test moves it across the retention.
func setProviderEgressHealthAt(ctx context.Context, health *ProviderEgressHealth, now time.Time) (stored ProviderEgressHealthStored) {
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

	runId := health.RunId
	if runId == (server.Id{}) {
		// Legacy local callers have no run token. Preserve idempotency of
		// their measured timestamp; production submitters carry RunId.
		identity := sha256.Sum256([]byte(health.ClientId.String() + ":" + health.MeasuredAt.UTC().Format(time.RFC3339Nano)))
		copy(runId[:], identity[:len(runId)])
	}

	store := func() {
		stored = storeProviderEgressHealth(ctx, health, runId, string(classResultsJson), evidenceJson, policyVersion)
	}
	var partitionMissingErr *providerEgressHealthPartitionMissingError
	func() {
		defer func() {
			if r := recover(); r != nil {
				// the one failure handled here: no partition holds the day
				if err, ok := r.(error); ok && errors.As(err, &partitionMissingErr) {
					return
				}
				panic(r)
			}
		}()
		store()
	}()
	if partitionMissingErr == nil {
		return
	}
	// a day older than the ingest takes is a day retention may have dropped
	// already, and recreating it would let a replay meet no row
	if maxIngestAge := DefaultProviderEgressHealthHistorySettings().MaxIngestAge; health.MeasuredAt.Before(now.Add(-maxIngestAge)) {
		panic(fmt.Errorf("%w: the measurement is older than the ingest's %s, so its day is not created", partitionMissingErr, maxIngestAge))
	}
	_, err = providerEgressHealthHistoryPartitionFamily.EnsureRangeFor(ctx, health.MeasuredAt)
	server.Raise(err)
	store()
	return
}

// The store's transaction (see SetProviderEgressHealth).
func storeProviderEgressHealth(
	ctx context.Context,
	health *ProviderEgressHealth,
	runId server.Id,
	classResultsJson string,
	evidenceJson any,
	policyVersion int,
) (result ProviderEgressHealthStored) {
	server.Tx(ctx, func(tx server.PgTx) {
		// a retried transaction discards what the rolled-back one decided
		result = ProviderEgressHealthStored{}
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			providerEgressHealthRunLockKey(runId),
		))
		var stored bool
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM provider_egress_health_history WHERE run_id = $1)`,
			runId,
		).Scan(&stored))
		if stored {
			return
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
		// A measured outcome on a URL the provider has an open finding for, one
		// pending or judged its own, is a security recheck: the claim offered
		// that stored destination (or a normal draw landed on it). It is
		// evidence about that URL alone, not a random sample of the provider,
		// so it is stored as a security-only receipt: out of the eight-hour
		// ratio, the rolling quota and the per-class totals, with its failed
		// name among the unscored ones. Its security events still resolve or
		// refresh the finding below, and its cycle is still paced.
		recheck := false
		if health.UrlProbeEvidence != nil && health.Total == 1 {
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM provider_egress_url_security AS security
				WHERE security.client_id = $1 AND security.url_key = $2 AND `+providerUrlSecurityRechecksSql("security")+`
			)`, health.ClientId, egresshealth.UrlProbeDestinationKey(health.UrlProbeEvidence.Destination)).Scan(&recheck))
		}
		// A measured outcome on a destination that has not graduated into
		// scoring is on probation: a country site, which the ingest marks, or
		// a general destination the pool holds on probation, read here as the
		// pool stands. It is a measured run like any other, so the history
		// keeps it with its outcome, its source and its probation state, and
		// the rolling quota, which paces turns by measured runs, counts it:
		// recording a site unscored costs no extra turns. What it may not do
		// is count (GEOMAP §11.4): the eight-hour window leaves it out
		// (providerEgressHealthWindowSql), and the latest-run row takes it out
		// of the counts and the per-class totals, its failed name among the
		// unscored ones, as a recheck's.
		probation := health.UrlProbation
		if health.UrlProbeEvidence != nil && health.Total == 1 && !probation {
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM provider_egress_destination WHERE name = $1 AND probation
			)`, health.UrlProbeEvidence.Destination.Name).Scan(&probation))
		}
		// the history row's counts are the rolling quota's: every measured
		// outcome but a recheck; the latest-run row's are the scored ones
		measuredOk, measuredTotal, measuredClassResultsJson := health.OKCount, health.Total, classResultsJson
		countedOk, countedTotal, countedClassResultsJson := health.OKCount, health.Total, classResultsJson
		countedFailedNames, countedUnscoredFailedNames := health.FailedNames, health.UnscoredFailedNames
		unscoredOutcome := recheck || (probation && health.UrlProbeEvidence != nil && health.Total == 1)
		if unscoredOutcome {
			countedOk, countedTotal, countedClassResultsJson = 0, 0, "{}"
			unscored := splitProviderEgressNames(health.UnscoredFailedNames)
			unscored = append(unscored, splitProviderEgressNames(health.FailedNames)...)
			countedFailedNames, countedUnscoredFailedNames = "", strings.Join(unscored, ",")
		}
		if recheck {
			measuredOk, measuredTotal, measuredClassResultsJson = 0, 0, "{}"
		}
		urlProbe := cycleAdmitted && measuredTotal == 1
		var securityMeasuredAt *time.Time
		if health.UrlProbeEvidence == nil && (health.TLSAuthenticationFailure || health.OKCount > 0) {
			t := health.MeasuredAt.UTC()
			securityMeasuredAt = &t
		}
		inserted, err := tx.Exec(ctx, `
			INSERT INTO provider_egress_health_history
				(run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure, cycle_started_at, url_probe, url_probe_evidence, url_probe_policy_version, url_source, url_probation)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10::jsonb,$11,$12,$13)
			ON CONFLICT DO NOTHING`,
			runId, health.ClientId, health.MeasuredAt.UTC(), measuredOk, measuredTotal,
			measuredClassResultsJson, health.TLSAuthenticationFailure, cycleStartedAt, urlProbe, evidenceJson, policyVersion,
			int16(health.UrlSource), probation)
		if err != nil {
			raiseProviderEgressHealthInsertErr(err, health.MeasuredAt)
		}
		if inserted.RowsAffected() == 0 {
			return
		}
		result = ProviderEgressHealthStored{Stored: true, Recheck: recheck, Probation: probation && !recheck}
		if health.UrlProbeEvidence != nil {
			for _, event := range health.UrlProbeEvidence.Security {
				destinationJson, err := json.Marshal(event.Destination)
				server.Raise(err)
				// A new or resolved finding is pending and decided nothing. A
				// finding that fails again keeps its verdict, and the failure
				// that verdict decided, until the judge reads the newer
				// failure: a judged one does not lapse into pending, and so
				// out of quarantine, on its own next failure.
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure,tls_verdict,tls_verdict_measured_at)
					VALUES($1,$2,$3::jsonb,$4,$5,$6,NULL)
					ON CONFLICT(client_id,url_key) DO UPDATE SET
						destination=EXCLUDED.destination,
						measured_at=EXCLUDED.measured_at,
						tls_failure=CASE WHEN provider_egress_url_security.measured_at=EXCLUDED.measured_at
							THEN provider_egress_url_security.tls_failure OR EXCLUDED.tls_failure ELSE EXCLUDED.tls_failure END,
						tls_verdict=CASE WHEN provider_egress_url_security.tls_failure
							AND (provider_egress_url_security.measured_at=EXCLUDED.measured_at OR EXCLUDED.tls_failure)
							THEN provider_egress_url_security.tls_verdict ELSE EXCLUDED.tls_verdict END,
						tls_verdict_measured_at=CASE WHEN provider_egress_url_security.tls_failure
							AND (provider_egress_url_security.measured_at=EXCLUDED.measured_at OR EXCLUDED.tls_failure)
							THEN provider_egress_url_security.tls_verdict_measured_at ELSE NULL END
					WHERE provider_egress_url_security.measured_at <= EXCLUDED.measured_at`,
					health.ClientId, egresshealth.UrlProbeDestinationKey(event.Destination), string(destinationJson), event.MeasuredAt.UTC(), event.TlsFailure,
					int16(ProviderEgressTlsVerdictPending)))
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
				-- a new unidentified failure starts the provider's clean rechecks over
				legacy_tls_recheck_from = CASE WHEN EXCLUDED.legacy_tls_authentication_failure THEN NULL ELSE provider_egress_health.legacy_tls_recheck_from END,
				legacy_tls_cleared_at = CASE WHEN EXCLUDED.legacy_tls_authentication_failure THEN NULL ELSE provider_egress_health.legacy_tls_cleared_at END,
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
			countedOk,
			countedTotal,
			countedClassResultsJson,
			health.ReputationOK,
			health.ReputationTotal,
			countedFailedNames,
			health.ReputationFailedNames,
			health.TLSAuthenticationFailure,
			health.NotMeasuredCount,
			health.NotMeasuredNames,
			health.CanaryPassedNames,
			health.CanaryFailedNames,
			health.ShortClasses,
			countedUnscoredFailedNames,
			securityMeasuredAt,
			health.UrlProbeEvidence == nil && health.TLSAuthenticationFailure,
		))
		if health.UrlProbeEvidence != nil {
			clearProviderEgressLegacyTlsInTx(ctx, tx, health)
		}
		// The projection is not a last-run boolean. One unrelated clean URL
		// cannot clear another URL, nor can it clear an unidentified legacy TLS
		// finding; only its provider's clean rechecks do. Exact-URL rows remain
		// authoritative under concurrent reports, and only one judged the
		// provider's own sets it (provider_egress_tls_verdict.go).
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_health AS health SET
			tls_authentication_failure = `+providerLegacyTlsUnclearedSql("health")+` OR EXISTS (
				SELECT 1 FROM provider_egress_url_security AS security
				WHERE security.client_id=health.client_id AND `+providerUrlSecurityExcludesSql("security")+`
			) WHERE health.client_id=$1`, health.ClientId))
		if cycleAdmitted {
			// A provider with an open finding is turned at the capped pace,
			// whatever its quota. Pacing reads the receipt's own outcome, so a
			// recheck that authenticated paces like a success; the counters
			// follow the rolling quota, so a recheck adds no error or outcome,
			// and an outcome on probation adds its own as the measured run it
			// is.
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
					outcome_count = outcome_count + $7,
					latest_result_at = GREATEST(latest_result_at, $5),
					next_attempt_at = CASE WHEN latest_result_at > $5 THEN next_attempt_at
						WHEN (`+providerHasUrlSecurityRecheckSql("cycle.client_id")+`) THEN `+
				providerUrlProbeSecurityPacedAttemptSql("cycle", "$5",
					providerUrlProbePacedAttemptSql("cycle", "$5", "$3", "recent.run_count < $6"))+`
						WHEN recent.run_count >= $6 THEN `+providerUrlProbeRenewalAtSql("recent.oldest_run_at", "$5::timestamp")+`
						ELSE `+providerUrlProbePacedAttemptSql("cycle", "$5", "$3", "recent.run_count < $6")+` END
				FROM recent WHERE cycle.client_id = recent.client_id`,
				health.ClientId, cycleStartedAt, health.OKCount, measuredTotal-measuredOk,
				health.MeasuredAt.UTC(), ProviderUrlProbeRunTarget, measuredTotal))
		}
	}, server.TxReadCommitted)
	return
}

// The distinct catalog destinations a legacy-flagged provider must verify TLS
// through, once back online, before its flag clears: the measured-run quota
// that makes any provider fully measured (ProviderUrlProbeRunTarget, ten URL
// turns per four-hour refresh age). At the capped recheck pace that is at
// least three and a half hours online.
const providerEgressLegacyTlsCleanSites = ProviderUrlProbeRunTarget

// The legacy snapshot's unidentified finding (legacy_tls_authentication_failure,
// the URL-less aggregate TLS failures of 2026-09-07 to 09-26) names no URL to
// recheck, so its provider is rechecked like one with a pending finding: still
// excluded, claimable past its quota at the capped pace, its turns ordinary
// catalog draws (providerHasUrlSecurityRecheckSql). This runs for each URL
// receipt, and one comes only from a provider online: the first after the
// provider is back starts the count (legacy_tls_recheck_from), so evidence
// from before, however clean, never counts, and nothing clears while the
// provider is offline. The flag clears once (legacy_tls_cleared_at; the flag
// itself is kept), on what a provider verdict rests on, mirrored:
//
//   - the provider's TLS verified through at least
//     providerEgressLegacyTlsCleanSites distinct catalog destinations, each
//     its latest outcome there, since the count started. A verified chain
//     through the provider is what an interceptor cannot present, and
//     distinct sites keep one site's repeated draws from standing for the
//     catalog; and
//   - no finding of the provider is still pending or judged its own. A TLS
//     failure during the rechecks is stored as an exact-URL finding and judged
//     like any other (provider_egress_tls_verdict.go): while it is pending or
//     the provider's own it also holds the flag, and once it is judged a site,
//     place or retired-URL fault, or a later turn authenticates that URL, it
//     no longer does.
func clearProviderEgressLegacyTlsInTx(ctx context.Context, tx server.PgTx, health *ProviderEgressHealth) {
	startedAt := health.MeasuredAt.UTC()
	for _, event := range health.UrlProbeEvidence.Security {
		if eventAt := event.MeasuredAt.UTC(); eventAt.Before(startedAt) {
			startedAt = eventAt
		}
	}
	server.RaisePgResult(tx.Exec(ctx, `
		UPDATE provider_egress_health AS health SET
			legacy_tls_recheck_from = COALESCE(health.legacy_tls_recheck_from, $2),
			legacy_tls_cleared_at = CASE WHEN $4 <= (
					SELECT COUNT(DISTINCT clean.destination->>'name') FROM provider_egress_url_security AS clean
					WHERE clean.client_id = health.client_id AND NOT clean.tls_failure
					AND COALESCE(clean.destination->>'name', '') <> ''
					AND COALESCE(health.legacy_tls_recheck_from, $2) <= clean.measured_at
				) AND NOT EXISTS (
					SELECT 1 FROM provider_egress_url_security AS unresolved
					WHERE unresolved.client_id = health.client_id AND `+providerUrlSecurityRechecksSql("unresolved")+`
				) THEN $3::timestamp END
		WHERE health.client_id = $1 AND `+providerLegacyTlsUnclearedSql("health"),
		health.ClientId, startedAt, health.MeasuredAt.UTC(), providerEgressLegacyTlsCleanSites))
}

// Accepted measured URL outcomes contribute for strictly less than eight hours.
// A TLS finding is no measured outcome: it is judged apart, and one judged the
// provider's own persists until later authenticated TLS from the same URL
// (provider_egress_tls_verdict.go).
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
// A security recheck is stored with no measured outcome (total_count 0), so it
// never enters this window: the finding it rechecks is judged on its own, and
// its failures would otherwise count against the provider twice. An outcome on
// probation (a country site, or a general site the refresh has not graduated)
// is a measured run of the rolling quota but never enters this window either:
// it has to earn the right to cost a provider a tier (provider_egress_url_source.go).
func providerEgressHealthWindowSql() string {
	return fmt.Sprintf(`
	SELECT client_id, measured_at, ok_count, total_count, class_results
	FROM provider_egress_health_history
	WHERE measured_at > $1 AND measured_at <= $2 AND total_count = 1
	AND url_probe_policy_version = %d AND NOT url_probation
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
// with an outstanding exact-URL TLS finding judged its own, or a retained
// legacy quarantine.
//
// There is intentionally no age cutoff on a judged finding. Its judgement
// already rests on evidence close in time to the failure (a direct,
// verified handshake to the same URL from the server, with no common-mode or
// place-level explanation), and it is positive evidence that the path is
// unsafe, not a quality sample that gradually becomes unknown. Only a newer
// authenticated response from each affected URL clears it. A finding still
// pending, or judged a site, place or retired-URL fault, excludes nothing
// (provider_egress_tls_verdict.go). An unidentified legacy finding is retained.
func GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx context.Context) map[server.Id]bool {
	return getProviderEgressTlsAuthenticationFailedClientIds(ctx, nil)
}

// The same findings for only clientIds, or for every provider when clientIds
// is nil. The rows are authoritative; the health row's projection is not read.
func getProviderEgressTlsAuthenticationFailedClientIds(ctx context.Context, clientIds []server.Id) map[server.Id]bool {
	failed := map[server.Id]bool{}
	query := `
			SELECT client_id
			FROM provider_egress_health
			WHERE ` + providerLegacyTlsUnclearedSql("provider_egress_health") + `
			UNION SELECT client_id FROM provider_egress_url_security AS security WHERE ` + providerUrlSecurityExcludesSql("security") + `
			`
	args := []any{}
	if clientIds != nil {
		query = `
			SELECT client_id
			FROM provider_egress_health
			WHERE client_id = ANY($1) AND ` + providerLegacyTlsUnclearedSql("provider_egress_health") + `
			UNION SELECT client_id FROM provider_egress_url_security AS security WHERE client_id = ANY($1) AND ` + providerUrlSecurityExcludesSql("security") + `
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
