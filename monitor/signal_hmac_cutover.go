package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/model"
)

const (
	storedContractHMACCompatibleYear  = 2026
	storedContractHMACCompatibleMonth = 5
	storedContractHMACCompatibleDay   = 14
)

// SIGNALS.md §2.24 maps to signal_hmac_cutover.go and
// signal_hmac_cutover_test.go. It correlates bounded claimed-version cohorts
// with current blackhole verdicts across the stored-contract HMAC boundary;
// raw descriptions and provider or network identifiers stay in PostgreSQL.
func NewHMACCutoverSignal() Signal {
	return &signalAdapter{
		number: "2.24", key: "hmac-cutover", name: "Stored-contract HMAC cutover compatibility",
		probe: hmacCutoverProbe{},
	}
}

type hmacCutoverProbe struct{}

func (hmacCutoverProbe) id() string             { return "pg/hmac-cutover" }
func (hmacCutoverProbe) tier() string           { return tierPage }
func (hmacCutoverProbe) cadence() time.Duration { return 5 * time.Minute }

func storedContractHMACCutover() time.Time {
	return connect.DefaultContractManagerSettings().NetworkEventTimeChangeHmac.UTC()
}

func hmacCutoverQuery() string {
	cutover := storedContractHMACCutover().Format("2006-01-02 15:04:05")
	currentSeconds := int64(model.ProviderBlackholeCheckMaxAge / time.Second)
	return fmt.Sprintf(`
/* monitor-signal-2.24-hmac-cutover */
WITH clock AS MATERIALIZED (
    SELECT now() AT TIME ZONE 'UTC' AS utc_now
), eligible AS MATERIALIZED (
    SELECT nc.client_id,
           nc.network_id,
           lower(COALESCE(nc.description, '')) AS description
    FROM network_client_location_reliability nclr
    INNER JOIN network_client nc USING (client_id)
    WHERE nc.active
      AND nc.source_client_id IS NULL
      AND nclr.connected
      AND nclr.valid
      AND EXISTS (
          SELECT 1
          FROM provide_key pk
          WHERE pk.client_id = nc.client_id
            AND pk.provide_mode = 3
      )
), metadata AS MATERIALIZED (
    SELECT eligible.*,
           CASE
               WHEN description ~ 'provider.*linux.*20[0-9]{2}\.[0-9]{1,2}\.[0-9]{1,2}'
                AND regexp_count(description, '20[0-9]{2}\.[0-9]{1,2}\.[0-9]{1,2}') = 1
               THEN substring(description FROM '(20[0-9]{2}\.[0-9]{1,2}\.[0-9]{1,2})')
               ELSE NULL
           END AS claimed_version
    FROM eligible
), classified AS MATERIALIZED (
    SELECT metadata.*,
           CASE
               WHEN claimed_version IS NULL THEN 'unknown'
               WHEN ROW(
                   split_part(claimed_version, '.', 1)::integer,
                   split_part(claimed_version, '.', 2)::integer,
                   split_part(claimed_version, '.', 3)::integer
               ) < ROW(%d, %d, %d)
               THEN 'legacy'
               ELSE 'compatible'
           END AS capability
    FROM metadata
), checked AS MATERIALIZED (
    SELECT classified.*,
           pbc.ok,
           COALESCE(
               pbc.checked_at >= clock.utc_now - interval '%d seconds',
               false
           ) AS current_check
    FROM classified
    CROSS JOIN clock
    LEFT JOIN provider_blackhole_check pbc USING (client_id)
), aggregate AS (
    SELECT max(clock.utc_now) >= timestamp '%s' AS cutover_active,
           floor(extract(epoch FROM (max(clock.utc_now) - timestamp '%s')))::bigint AS cutoff_age_seconds,
           count(*)::bigint AS eligible,
           count(*) FILTER (WHERE capability = 'legacy')::bigint AS legacy,
           count(DISTINCT network_id) FILTER (WHERE capability = 'legacy')::bigint AS legacy_networks,
           count(*) FILTER (WHERE capability = 'compatible')::bigint AS compatible,
           count(*) FILTER (WHERE capability = 'unknown')::bigint AS unknown,
           count(*) FILTER (WHERE capability = 'legacy' AND current_check)::bigint AS legacy_checked,
           count(*) FILTER (WHERE capability = 'legacy' AND current_check AND NOT ok)::bigint AS legacy_dark,
           count(*) FILTER (WHERE capability = 'legacy' AND current_check AND ok)::bigint AS legacy_ok,
           count(*) FILTER (WHERE capability = 'compatible' AND current_check)::bigint AS compatible_checked,
           count(*) FILTER (WHERE capability = 'compatible' AND current_check AND NOT ok)::bigint AS compatible_dark,
           count(*) FILTER (WHERE capability = 'compatible' AND current_check AND ok)::bigint AS compatible_ok
    FROM checked
    CROSS JOIN clock
)
SELECT cutover_active::text,
       cutoff_age_seconds::text,
       eligible::text,
       legacy::text,
       legacy_networks::text,
       compatible::text,
       unknown::text,
       legacy_checked::text,
       legacy_dark::text,
       legacy_ok::text,
       compatible_checked::text,
       compatible_dark::text,
       compatible_ok::text
FROM aggregate;
`, storedContractHMACCompatibleYear, storedContractHMACCompatibleMonth, storedContractHMACCompatibleDay,
		currentSeconds, cutover, cutover)
}

type hmacCutoverSnapshot struct {
	cutoverActive     bool
	cutoffAgeSeconds  int64
	eligible          int64
	legacy            int64
	legacyNetworks    int64
	compatible        int64
	unknown           int64
	legacyChecked     int64
	legacyDark        int64
	legacyOK          int64
	compatibleChecked int64
	compatibleDark    int64
	compatibleOK      int64
}

func parseHMACCutoverSnapshot(rows []pgRow) (hmacCutoverSnapshot, error) {
	if len(rows) != 1 || len(rows[0]) != 13 {
		return hmacCutoverSnapshot{}, fmt.Errorf("stored-contract HMAC readiness returned an invalid aggregate shape")
	}
	active, err := strconv.ParseBool(strings.TrimSpace(rows[0].str(0)))
	if err != nil {
		return hmacCutoverSnapshot{}, fmt.Errorf("stored-contract HMAC readiness returned an invalid cutover state")
	}
	values := make([]int64, 12)
	for index := range values {
		value, parseErr := parseStrictInt64(rows[0].str(index + 1))
		if parseErr != nil || (index > 0 && value < 0) {
			return hmacCutoverSnapshot{}, fmt.Errorf("stored-contract HMAC readiness returned an invalid numeric field %d", index+1)
		}
		values[index] = value
	}
	snapshot := hmacCutoverSnapshot{
		cutoverActive: active, cutoffAgeSeconds: values[0],
		eligible: values[1], legacy: values[2], legacyNetworks: values[3],
		compatible: values[4], unknown: values[5],
		legacyChecked: values[6], legacyDark: values[7], legacyOK: values[8],
		compatibleChecked: values[9], compatibleDark: values[10], compatibleOK: values[11],
	}
	if snapshot.eligible != snapshot.legacy+snapshot.compatible+snapshot.unknown ||
		snapshot.legacyNetworks > snapshot.legacy ||
		snapshot.legacyChecked != snapshot.legacyDark+snapshot.legacyOK ||
		snapshot.legacyChecked > snapshot.legacy ||
		snapshot.compatibleChecked != snapshot.compatibleDark+snapshot.compatibleOK ||
		snapshot.compatibleChecked > snapshot.compatible ||
		(snapshot.cutoverActive && snapshot.cutoffAgeSeconds < 0) ||
		(!snapshot.cutoverActive && snapshot.cutoffAgeSeconds >= 0) {
		return hmacCutoverSnapshot{}, fmt.Errorf("stored-contract HMAC readiness returned contradictory aggregate values")
	}
	return snapshot, nil
}

func hmacShareAtLeast(numerator, denominator, percent int64) bool {
	return denominator > 0 && numerator*100 >= denominator*percent
}

func (hmacCutoverProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, hmacCutoverQuery())
	if err != nil {
		return nil, err
	}
	snapshot, err := parseHMACCutoverSnapshot(rows)
	if err != nil {
		return nil, err
	}
	target := "provider-fleet"
	findings := []finding{
		healthyFinding("pg/hmac-cutover", tierPage, "contract-hmac-incompatible", target),
		healthyFinding("pg/hmac-cutover", tierWarn, "contract-hmac-readiness", target),
	}
	causal := snapshot.cutoverActive && snapshot.legacy >= 20 && snapshot.legacyChecked >= 20 &&
		hmacShareAtLeast(snapshot.legacyDark, snapshot.legacyChecked, 90) &&
		snapshot.compatibleChecked >= 20 && hmacShareAtLeast(snapshot.compatibleOK, snapshot.compatibleChecked, 50)
	if causal {
		findings[0] = hmacCutoverIncompatibleFinding(snapshot)
	} else if snapshot.legacy > 0 && (snapshot.cutoverActive || snapshot.cutoffAgeSeconds >= -int64((30*24*time.Hour)/time.Second)) {
		findings[1] = hmacCutoverReadinessFinding(snapshot)
	}
	return findings, nil
}

func hmacCutoverObserved(snapshot hmacCutoverSnapshot) string {
	return fmt.Sprintf(
		"cutover_active=%t cutoff_age_seconds=%d minimum_dual_verifier_version=%d.%d.%d eligible=%d claimed_legacy=%d legacy_networks=%d claimed_compatible=%d unknown=%d legacy_checked=%d legacy_dark=%d legacy_ok=%d compatible_checked=%d compatible_dark=%d compatible_ok=%d",
		snapshot.cutoverActive, snapshot.cutoffAgeSeconds,
		storedContractHMACCompatibleYear, storedContractHMACCompatibleMonth, storedContractHMACCompatibleDay,
		snapshot.eligible, snapshot.legacy, snapshot.legacyNetworks, snapshot.compatible, snapshot.unknown,
		snapshot.legacyChecked, snapshot.legacyDark, snapshot.legacyOK,
		snapshot.compatibleChecked, snapshot.compatibleDark, snapshot.compatibleOK,
	)
}

func hmacCutoverIncompatibleFinding(snapshot hmacCutoverSnapshot) finding {
	legacyResult := fmt.Sprintf(
		"%d of %d current checks of providers claiming a pre-%d.%d.%d Linux version are dark after the stored-contract HMAC cutover.",
		snapshot.legacyDark, snapshot.legacyChecked,
		storedContractHMACCompatibleYear, storedContractHMACCompatibleMonth, storedContractHMACCompatibleDay,
	)
	if snapshot.legacyOK == 0 {
		legacyResult = fmt.Sprintf(
			"All %d current checks of providers claiming a pre-%d.%d.%d Linux version are dark after the stored-contract HMAC cutover; none pass.",
			snapshot.legacyChecked,
			storedContractHMACCompatibleYear, storedContractHMACCompatibleMonth, storedContractHMACCompatibleDay,
		)
	}
	return finding{
		probeId: "pg/hmac-cutover", tier: tierPage,
		class: "contract-hmac-incompatible", target: "provider-fleet", frame: "legacy-linux", sustain: 1,
		symptom:   legacyResult,
		mechanism: "At the configured wall-clock boundary, API and resident Connect signers switched from the legacy appended key-only MAC to the standard HMAC of the stored-contract bytes. Connect clients older than the dual-verifier release accept only the legacy shape, so they reject each newly created contract before forwarding traffic. The compatible-version cohort remains a healthy control, ruling out the shared prober tunnel and API path.",
		baseline:  "No eligible provider claims a legacy-only verifier when standard signing is active, or bounded current checks prove that the claimed metadata is stale and the cohort still accepts standard contracts.",
		observed:  hmacCutoverObserved(snapshot),
		evidence:  "The query recognizes only one date-shaped version in a provider/Linux description, reduces it to legacy, compatible, or unknown inside PostgreSQL, and joins only current aggregate blackhole verdicts. Raw descriptions plus provider and network identifiers never leave the database; the legacy network count is distinct and aggregate.",
		context:   "The version is provider-claimed metadata, so the behavioral dark/pass split is required before causal attribution. Unversioned and ambiguous descriptions remain unknown. This is a protocol compatibility and rollout decision, not a Proxy RAM or active-client hardware ceiling.",
		action:    "Obtain an explicit security/availability decision. The secure path quarantines this legacy cohort until its software is upgraded and verifies remaining provider capacity. A temporary compatibility rollback postpones standard signing and must converge both API and Connect signer paths, knowingly retaining the weak legacy key-only MAC. Durable per-client capability negotiation is an architecture change and requires separate approval; do not silently move the date, deploy API alone, lengthen probe timeouts, or treat retries as recovery.",
		verify:    "For the secure path, the legacy cohort is no longer eligible and §2.8/§2.9 prove adequate healthy capacity. For an approved compatibility path, every API and Connect signer has the same artifact/config boundary and this cohort produces successful current checks and settled destination bytes. In either case §2.19 completes a whole refresh inside the verdict lifetime and the compatible control stays healthy for two cadences.",
		playbook:  "SIGNALS.md §2.24, §2.19, §2.23, §2.8, and §2.9",
	}
}

func hmacCutoverReadinessFinding(snapshot hmacCutoverSnapshot) finding {
	state := "before"
	if snapshot.cutoverActive {
		state = "after"
	}
	return finding{
		probeId: "pg/hmac-cutover", tier: tierWarn,
		class: "contract-hmac-readiness", target: "provider-fleet", frame: "legacy-linux", sustain: 2,
		symptom: fmt.Sprintf(
			"%d eligible providers claim a pre-%d.%d.%d Linux version %s the stored-contract HMAC cutover, but current controls do not yet prove one disposition.",
			snapshot.legacy, storedContractHMACCompatibleYear, storedContractHMACCompatibleMonth, storedContractHMACCompatibleDay, state,
		),
		mechanism: "A wall-clock signing change is safe only after every active receiver can verify the new shape. Claimed legacy metadata establishes a readiness risk, while missing current checks or a missing healthy compatible control prevents this probe from calling the cohort failure causal.",
		baseline:  "The legacy-claim count reaches zero before the cutoff, or complete current behavior proves compatibility through the exact signer boundary.",
		observed:  hmacCutoverObserved(snapshot),
		evidence:  "Only aggregate claimed-version classes, distinct legacy-network count, and current verdict counts leave PostgreSQL. Descriptions and provider/network identities remain private.",
		context:   "Unknown metadata is not treated as compatible. Conversely, a claimed old version is not by itself proof that the live process still runs old code; the dark cohort and compatible control supply that discriminator.",
		action:    "Complete §2.19 coverage and compare current legacy and compatible cohorts. Before activating standard signing, upgrade or quarantine the legacy cohort and prove capacity. After activation, use the explicit secure-versus-compatibility decision in this section; do not infer readiness from the calendar or silently alter signing behavior.",
		verify:    "The claimed legacy cohort reaches zero or gains complete behaviorally verified compatibility, the compatible control remains healthy, and one full blackhole refresh finishes within its verdict lifetime.",
		playbook:  "SIGNALS.md §2.24, §2.19, and §2.23",
	}
}
