package monitor

import (
	"context"
	"fmt"
	"time"
)

// SIGNALS.md §2.25 maps to signal_probe_cleanup.go and
// signal_probe_cleanup_test.go. It verifies that short-lived clients derived
// from the durable egress-prober identity are retired after their bounded
// tunnel closes; client, connection, and network identifiers stay in
// PostgreSQL.
func NewProbeCleanupSignal() Signal {
	return &signalAdapter{
		number: "2.25", key: "probe-cleanup", name: "Provider probe child retirement",
		probe: probeCleanupProbe{},
	}
}

type probeCleanupProbe struct{}

func (probeCleanupProbe) id() string             { return "pg/probe-cleanup" }
func (probeCleanupProbe) tier() string           { return tierPage }
func (probeCleanupProbe) cadence() time.Duration { return 5 * time.Minute }

const probeCleanupPagePercent = int64(10)

func probeCleanupQuery() string {
	return `
/* monitor-signal-2.25-probe-cleanup */
WITH clock AS MATERIALIZED (
    SELECT statement_timestamp() AT TIME ZONE 'UTC' AS now_utc
), prober AS MATERIALIZED (
    SELECT network_id, client_id
    FROM prober_identity
    WHERE singleton
      AND network_id IS NOT NULL
      AND client_id IS NOT NULL
), recent AS MATERIALIZED (
    SELECT
        nc.client_id,
        nc.active,
        nc.create_time,
        nc.deactivate_time,
        EXISTS (
            SELECT 1
            FROM network_client_connection ncc
            WHERE ncc.client_id = nc.client_id
              AND ncc.connected
        ) AS connected,
        k.now_utc
    FROM prober p
    INNER JOIN network_client nc
      ON nc.network_id = p.network_id
     AND nc.source_client_id = p.client_id
    CROSS JOIN clock k
    WHERE nc.create_time >= k.now_utc - interval '6 hours'
), aggregate AS (
    SELECT
        count(*)::bigint AS created_6h,
        count(*) FILTER (
            WHERE create_time < now_utc - interval '10 minutes'
        )::bigint AS mature_created,
        count(*) FILTER (
            WHERE create_time < now_utc - interval '10 minutes'
              AND NOT active
        )::bigint AS mature_inactive,
        count(*) FILTER (
            WHERE create_time < now_utc - interval '10 minutes'
              AND active
              AND connected
        )::bigint AS mature_active_connected,
        count(*) FILTER (
            WHERE create_time < now_utc - interval '10 minutes'
              AND active
              AND NOT connected
        )::bigint AS mature_active_disconnected,
        count(*) FILTER (
            WHERE create_time >= now_utc - interval '10 minutes'
              AND active
        )::bigint AS fresh_active,
        count(*) FILTER (
            WHERE NOT active
              AND deactivate_time IS NULL
        )::bigint AS inactive_without_deactivate_time,
        COALESCE(floor(max(extract(epoch FROM (now_utc - create_time))) FILTER (
            WHERE create_time < now_utc - interval '10 minutes'
              AND active
              AND NOT connected
        )), 0)::bigint AS oldest_active_disconnected_age_seconds
    FROM recent
), residual_connection_history AS MATERIALIZED (
    SELECT
        EXISTS (
            SELECT 1
            FROM network_client_connection ncc
            WHERE ncc.client_id = r.client_id
        ) AS ever_connected
    FROM recent r
    WHERE r.create_time < r.now_utc - interval '10 minutes'
      AND r.active
      AND NOT r.connected
), residual_aggregate AS (
    SELECT
        count(*) FILTER (WHERE NOT ever_connected)::bigint AS mature_active_disconnected_never_connected,
        count(*) FILTER (WHERE ever_connected)::bigint AS mature_active_disconnected_ever_connected
    FROM residual_connection_history
)
SELECT
    (SELECT count(*) FROM prober)::text AS authority_rows,
    created_6h::text,
    mature_created::text,
    mature_inactive::text,
    mature_active_connected::text,
    mature_active_disconnected::text,
    fresh_active::text,
    inactive_without_deactivate_time::text,
    oldest_active_disconnected_age_seconds::text,
    mature_active_disconnected_never_connected::text,
    mature_active_disconnected_ever_connected::text
FROM aggregate
CROSS JOIN residual_aggregate;
`
}

type probeCleanupSnapshot struct {
	authorityRows                      int64
	created6h                          int64
	matureCreated                      int64
	matureInactive                     int64
	matureActiveConnected              int64
	matureActiveDisconnected           int64
	freshActive                        int64
	inactiveWithoutDeactivateTime      int64
	oldestActiveDisconnectedAgeSeconds int64
	matureDisconnectedNeverConnected   int64
	matureDisconnectedEverConnected    int64
}

func parseProbeCleanupSnapshot(rows []pgRow) (probeCleanupSnapshot, error) {
	if len(rows) != 1 || len(rows[0]) != 11 {
		return probeCleanupSnapshot{}, fmt.Errorf("provider probe cleanup returned an invalid aggregate shape")
	}
	values := make([]int64, 11)
	for index := range values {
		value, err := parseStrictInt64(rows[0].str(index))
		if err != nil || value < 0 {
			return probeCleanupSnapshot{}, fmt.Errorf("provider probe cleanup returned an invalid numeric field %d", index+1)
		}
		values[index] = value
	}
	snapshot := probeCleanupSnapshot{
		authorityRows: values[0], created6h: values[1], matureCreated: values[2],
		matureInactive: values[3], matureActiveConnected: values[4],
		matureActiveDisconnected: values[5], freshActive: values[6],
		inactiveWithoutDeactivateTime: values[7], oldestActiveDisconnectedAgeSeconds: values[8],
		matureDisconnectedNeverConnected: values[9], matureDisconnectedEverConnected: values[10],
	}
	if snapshot.authorityRows > 1 ||
		snapshot.matureCreated != snapshot.matureInactive+snapshot.matureActiveConnected+snapshot.matureActiveDisconnected ||
		snapshot.matureCreated > snapshot.created6h ||
		snapshot.freshActive > snapshot.created6h-snapshot.matureCreated ||
		snapshot.inactiveWithoutDeactivateTime > snapshot.created6h ||
		snapshot.matureActiveDisconnected != snapshot.matureDisconnectedNeverConnected+snapshot.matureDisconnectedEverConnected ||
		(snapshot.matureActiveDisconnected == 0 && snapshot.oldestActiveDisconnectedAgeSeconds != 0) ||
		(snapshot.authorityRows == 0 && probeCleanupValuesHaveNonzero(values[1:])) {
		return probeCleanupSnapshot{}, fmt.Errorf("provider probe cleanup returned contradictory aggregate values")
	}
	return snapshot, nil
}

func probeCleanupValuesHaveNonzero(values []int64) bool {
	for _, value := range values {
		if value != 0 {
			return true
		}
	}
	return false
}

func (probeCleanupProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, probeCleanupQuery())
	if err != nil {
		return nil, err
	}
	snapshot, err := parseProbeCleanupSnapshot(rows)
	if err != nil {
		return nil, err
	}
	target := "provider-egress-prober"
	findings := []finding{
		healthyFinding("pg/probe-cleanup", tierPage, "probe-child-retirement", target),
		healthyFinding("pg/probe-cleanup", tierPage, "probe-unused-args-retirement", target),
		healthyFinding("pg/probe-cleanup", tierWarn, "probe-child-retirement-identity", target),
		healthyFinding("pg/probe-cleanup", tierWarn, "probe-child-retirement-integrity", target),
	}
	if snapshot.authorityRows != 1 {
		findings[2] = probeCleanupIdentityFinding(snapshot)
		return findings, nil
	}
	if snapshot.inactiveWithoutDeactivateTime != 0 {
		findings[3] = probeCleanupIntegrityFinding(snapshot)
	}
	if snapshot.matureDisconnectedEverConnected != 0 {
		findings[0] = probeCleanupLeakFinding(
			snapshot,
			probeCleanupSeverity(snapshot, snapshot.matureDisconnectedEverConnected),
			snapshot.matureDisconnectedEverConnected,
		)
	}
	if snapshot.matureDisconnectedNeverConnected != 0 {
		findings[1] = probeUnusedArgsRetirementFinding(
			snapshot,
			probeCleanupSeverity(snapshot, snapshot.matureDisconnectedNeverConnected),
		)
	}
	return findings, nil
}

func probeCleanupSeverity(snapshot probeCleanupSnapshot, residual int64) string {
	if snapshot.matureCreated >= 20 && residual >= 20 &&
		residual*100 >= snapshot.matureCreated*probeCleanupPagePercent {
		return tierPage
	}
	return tierWarn
}

func probeCleanupObserved(snapshot probeCleanupSnapshot) string {
	unretiredPercent := float64(0)
	if snapshot.matureCreated > 0 {
		unretiredPercent = 100 * float64(snapshot.matureActiveDisconnected) / float64(snapshot.matureCreated)
	}
	return fmt.Sprintf(
		"authority_rows=%d created_6h=%d mature_grace_seconds=600 mature_created=%d mature_inactive=%d mature_active_connected=%d mature_active_disconnected=%d mature_active_disconnected_percent=%.1f mature_active_disconnected_never_connected=%d mature_active_disconnected_ever_connected=%d fresh_active=%d inactive_without_deactivate_time=%d oldest_active_disconnected_age_seconds=%d",
		snapshot.authorityRows, snapshot.created6h, snapshot.matureCreated, snapshot.matureInactive,
		snapshot.matureActiveConnected, snapshot.matureActiveDisconnected, unretiredPercent,
		snapshot.matureDisconnectedNeverConnected, snapshot.matureDisconnectedEverConnected,
		snapshot.freshActive, snapshot.inactiveWithoutDeactivateTime,
		snapshot.oldestActiveDisconnectedAgeSeconds,
	)
}

func probeCleanupLeakFinding(snapshot probeCleanupSnapshot, severity string, residual int64) finding {
	return finding{
		probeId: "pg/probe-cleanup", tier: severity,
		class: "probe-child-retirement", target: "provider-egress-prober", frame: "derived-client-lifecycle", sustain: 2,
		symptom: fmt.Sprintf(
			"%d of %d mature egress-prober children that previously opened a connection remain active after it closed",
			residual, snapshot.matureCreated,
		),
		mechanism: "A lifetime connection row proves this child reached the generated-client channel path. Once that connection is no longer live, channel retirement must keep the generator control plane available until its admitted remove-client request completes. Cancellation or unjoined teardown can otherwise leave the child active until the much later idle reaper.",
		baseline:  "After a ten-minute close grace, no prober-derived client remains active without a connected session; active connected children are the in-flight healthy control and stay bounded by current probe work.",
		observed:  probeCleanupObserved(snapshot),
		evidence:  "The query restricts six hours of children by both the singleton prober network and parent client, classifies only mature disconnected residuals by lifetime connection existence, and exports aggregate counts. Client, network, connection, and credential identifiers never leave PostgreSQL.",
		context:   "This is the reached-channel teardown branch. Never-connected client arguments are independently reported as probe-unused-args-retirement. Neither branch proves a Proxy active-client hardware ceiling or explains the independent legacy stored-contract HMAC rejection. Historical stock remains a separate database-retention and operational-cleanup concern after new leakage stops.",
		action:    "Trace generated-client channel close through the exact deployed Connect and Operator Proxy inputs. Preserve ordered tunnel close, generation-safe removal, and joined cleanup before control-plane cancellation. Add a deterministic blocked-remove regression for any newly found gap. Do not delete or deactivate production rows merely to clear this signal, and do not call a small or empty cohort recovery while §2.19 probe activity is insufficient.",
		verify:    "After every affected process converges on the correction, run an explicit cohort beginning after the rollout boundary and require zero mature ever-connected residuals while §2.19 proves continuing probe work. The rolling six-hour signal becomes independently clean only after rollout end plus six hours and the ten-minute grace.",
		playbook:  "SIGNALS.md §2.25, §2.19, §2.23, §2.24, and §8.12",
	}
}

func probeUnusedArgsRetirementFinding(snapshot probeCleanupSnapshot, severity string) finding {
	return finding{
		probeId: "pg/probe-cleanup", tier: severity,
		class: "probe-unused-args-retirement", target: "provider-egress-prober", frame: "derived-client-lifecycle", sustain: 2,
		symptom: fmt.Sprintf(
			"%d of %d mature egress-prober children never opened a connection and remain active",
			snapshot.matureDisconnectedNeverConnected, snapshot.matureCreated,
		),
		mechanism: "A child with no lifetime connection row was discarded before it entered the generated-client channel path, such as an unused, late, expired, or failed client argument. Those direct RemoveClientArgs calls must be admitted to the generator retirement lifecycle and joined by shutdown; otherwise API cancellation can strand their asynchronous remove request.",
		baseline:  "After a ten-minute close grace, every never-connected prober-derived client has been retired; fresh active children remain outside the mature cohort.",
		observed:  probeCleanupObserved(snapshot),
		evidence:  "The bounded query evaluates lifetime connection existence only for mature active-disconnected residuals and exports aggregate branch counts. Client, network, connection, and credential identifiers never leave PostgreSQL.",
		context:   "This is the pre-channel/direct-argument cleanup branch. Children that previously opened a connection are independently reported as probe-child-retirement. It is a software lifecycle fault, not proof of provider capacity pressure and not authority for historical row deletion.",
		action:    "Make live direct client-argument removal retirement-admitted and ensure generator CloseAndWait joins it before canceling the API. Retain generation-safe RemoveIfCurrent behavior, store preservation during shutdown, and bounded late-after-close best effort. Prove the ordering with a deterministic blocked remove-client response. Do not bulk-deactivate production rows.",
		verify:    "After every affected process converges, start a post-rollout cohort and require zero mature never-connected residuals while §2.19 proves continuing probe work. Then require the complete rolling six-hour window plus the ten-minute grace to clear.",
		playbook:  "SIGNALS.md §2.25, §2.19, §2.23, and §8.12",
	}
}

func probeCleanupIdentityFinding(snapshot probeCleanupSnapshot) finding {
	return finding{
		probeId: "pg/probe-cleanup", tier: tierWarn,
		class: "probe-child-retirement-identity", target: "provider-egress-prober", frame: "prober-identity", sustain: 2,
		symptom:   "The durable egress-prober identity is incomplete, so child retirement cannot be measured",
		mechanism: "The cleanup cohort is defined by both the singleton prober network and its durable parent client. Guessing from a description or counting every derived client would mix unrelated application windows into this signal.",
		baseline:  "Exactly one complete prober_identity singleton supplies network_id and client_id before egress probes create derived children.",
		observed:  probeCleanupObserved(snapshot),
		evidence:  "Only aggregate authority-row and zero cohort counts leave PostgreSQL; no partial identity value is selected.",
		context:   "This is a monitoring and prober-bootstrap boundary, not affirmative evidence that cleanup is healthy or broken.",
		action:    "Repair §2.23 prober bootstrap and credential readiness, then rerun this signal. Do not infer the parent from description text or expose the stored client credential.",
		verify:    "Exactly one complete singleton is present, egress probes advance, and this signal returns a measured six-hour cohort.",
		playbook:  "SIGNALS.md §2.25 and §2.23",
	}
}

func probeCleanupIntegrityFinding(snapshot probeCleanupSnapshot) finding {
	return finding{
		probeId: "pg/probe-cleanup", tier: tierWarn,
		class: "probe-child-retirement-integrity", target: "provider-egress-prober", frame: "derived-client-lifecycle", sustain: 2,
		symptom: fmt.Sprintf(
			"%d recently inactive egress-prober child clients have no deactivation timestamp",
			snapshot.inactiveWithoutDeactivateTime,
		),
		mechanism: "Client retirement is expected to change active and record deactivate_time together. An inactive child with no timestamp means a legacy or out-of-contract writer bypassed that lifecycle invariant, so age-based cleanup and incident ordering cannot be trusted for that row.",
		baseline:  "Every inactive egress-prober child in the bounded six-hour cohort has a non-null deactivation timestamp.",
		observed:  probeCleanupObserved(snapshot),
		evidence:  "The PostgreSQL query exports only the aggregate count of contradictory rows and the surrounding lifecycle totals; no client or network identifier leaves the database.",
		context:   "This is a software/data-integrity fault independent of the active-disconnected cleanup leak, legacy stored-contract HMAC compatibility, and Proxy hardware capacity.",
		action:    "Identify the exact writer and artifact that changed active without setting deactivate_time. Correct that transaction and add a deterministic lifecycle regression; do not backfill or delete production rows until the affected cohort and intended timestamp source are proved.",
		verify:    "After the corrected writer is deployed, require zero new contradictory rows for a complete six-hour cohort and separately account for any pre-fix rows before an authorized repair.",
		playbook:  "SIGNALS.md §2.25 and §8.12",
	}
}
