// The stored judgement of each exact-URL TLS finding (D1,
// model/provider_egress_tls_verdict.go).
//
// A finding used to exclude its provider the moment it was stored, and until
// a later authenticated response from the same URL. It now excludes only once
// the taskworker's judge stores the provider verdict for it. tls_verdict holds
// that verdict: 0 pending, 1 provider, 2 site or prober fault, 3 place fault,
// 4 retired URL. tls_verdict_measured_at holds the failure (the finding's
// measured_at) the verdict decided, NULL when none was, so each failure is
// judged once, on evidence close in time to it.
//
// The health row's legacy flag (legacy_tls_authentication_failure, the
// URL-less aggregate TLS failures of 2026-09-07 to 09-26) names no URL to
// recheck. It now clears once its provider, back online, verifies TLS cleanly
// through enough catalog URLs (model provider_egress_health_model.go,
// clearProviderEgressLegacyTlsInTx). legacy_tls_recheck_from holds the first
// URL receipt after the provider came back, so only rechecks from then on
// count; legacy_tls_cleared_at holds when they cleared it. The flag itself is
// kept.
//
// The migration is additive and keeps every current exclusion:
//
//   - The columns are a constant default and NULLs, so adding them is a
//     catalog change with no rewrite of either table's rows (33 million and
//     1.1 million). Each ALTER still takes ACCESS EXCLUSIVE for that instant,
//     and both tables take an upsert for every URL turn, so the wait is
//     bounded at five seconds: a migration queued behind a long reader fails
//     for a retry rather than stall URL ingest behind it. Every legacy flag
//     starts uncleared, with no recheck begun.
//   - Every unresolved finding (a few hundred, read through the partial index
//     on tls_failure) starts as provider, so nothing that excludes today stops
//     excluding before the judge has read it, with no failure decided, so the
//     judge judges each one: a recent failure on its evidence, an old one when
//     a recheck refreshes it. A finding on a URL the pool has not served for a
//     day is retired.
//   - The health row's projection is recomputed from those verdicts and the
//     legacy flag, as the new code maintains it, through the partial index on
//     the projection.
//
// Old binaries ignore the columns: their upserts leave them as they were,
// their readers still treat every unresolved finding and every legacy flag as
// an exclusion until they are replaced, and they clear nothing. Apply before
// deploying the api, alt, taskworker and proxy that use it.
package server

// The migration's statement. It does nothing when the column exists, so a
// failed lock wait is safe to retry.
const ProviderEgressTlsVerdictSql = `
	DO $verdict$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = 'public.provider_egress_url_security'::regclass
			  AND attname = 'tls_verdict' AND NOT attisdropped
		) THEN
			PERFORM set_config('lock_timeout', '5s', true);
			ALTER TABLE provider_egress_url_security
				ADD COLUMN tls_verdict smallint NOT NULL DEFAULT 0,
				ADD COLUMN tls_verdict_measured_at timestamp NULL;
			ALTER TABLE provider_egress_health
				ADD COLUMN legacy_tls_recheck_from timestamp NULL,
				ADD COLUMN legacy_tls_cleared_at timestamp NULL;
			UPDATE provider_egress_url_security SET tls_verdict = 1 WHERE tls_failure;
			UPDATE provider_egress_health AS health SET
				tls_authentication_failure = (health.legacy_tls_authentication_failure AND health.legacy_tls_cleared_at IS NULL) OR EXISTS (
					SELECT 1 FROM provider_egress_url_security AS security
					WHERE security.client_id = health.client_id AND security.tls_failure AND security.tls_verdict = 1
				)
			WHERE health.tls_authentication_failure = true;
		END IF;
	END
	$verdict$
`

var providerEgressTlsVerdictMigration = newSqlMigration(ProviderEgressTlsVerdictSql)

// The migration's version: its position in the list, from 1.
func ProviderEgressTlsVerdictVersion() int {
	for i, migration := range migrations {
		if migration == providerEgressTlsVerdictMigration {
			return i + 1
		}
	}
	return 0
}
