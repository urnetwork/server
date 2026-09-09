package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const paymentReconciliationHealthQuery = `
/* monitor-signal-2.21-payment-reconciliation-health */
WITH expected_store(store) AS (
    VALUES ('apple'), ('google'), ('solana'), ('stripe')
), heartbeat AS (
    SELECT max(event_time) AS event_time
    FROM payment_reconciliation_event
    WHERE store = 'all' AND action = 'heartbeat' AND NOT dry_run
), task_state AS (
    SELECT count(*)::bigint AS task_count,
           count(*) FILTER (WHERE reschedule_error IS NOT NULL)::bigint AS task_error_count,
           count(*) FILTER (WHERE claim_time > now() - interval '2 minutes')::bigint AS task_claimed_count
    FROM pending_task
    WHERE function_name = 'github.com/urnetwork/server/taskworker/work.PaymentReconcile'
)
SELECT expected_store.store,
       COALESCE(extract(epoch FROM now() - heartbeat.event_time)::bigint, -1),
       COALESCE(extract(epoch FROM now() - watermark.update_time)::bigint, -1),
       (SELECT count(*) FROM payment_reconciliation_event event
        WHERE event.store = expected_store.store
          AND event.action = 'skipped_store'
          AND NOT event.dry_run
          AND event.event_time >= now() - interval '3 hours')::bigint,
       (SELECT count(*) FROM payment_reconciliation_event event
        WHERE event.store = expected_store.store
          AND event.action = 'error'
          AND NOT event.dry_run
          AND event.event_time >= now() - interval '3 hours')::bigint,
       COALESCE((SELECT extract(epoch FROM now() - max(event.event_time))::bigint
                 FROM payment_reconciliation_event event
                 WHERE event.store = expected_store.store AND NOT event.dry_run), -1),
       task_state.task_count,
       task_state.task_error_count,
       task_state.task_claimed_count
FROM expected_store
CROSS JOIN heartbeat
CROSS JOIN task_state
LEFT JOIN payment_reconciliation_watermark watermark
  ON watermark.store = expected_store.store
ORDER BY expected_store.store
`

const paymentReconciliationRepairQuery = `
/* monitor-signal-2.21-payment-reconciliation-repairs */
WITH reconciliation_run AS (
    SELECT DISTINCT run_id
    FROM payment_reconciliation_event
    WHERE store = 'all'
      AND action = 'heartbeat'
      AND NOT dry_run
      AND event_time >= now() - interval '24 hours'
)
SELECT event.store,
       event.action,
       count(*)::bigint,
       extract(epoch FROM now() - max(event.event_time))::bigint
FROM payment_reconciliation_event event
INNER JOIN reconciliation_run ON reconciliation_run.run_id = event.run_id
WHERE event.action IN ('credited', 'ended')
  AND NOT event.dry_run
  AND event.event_time >= now() - interval '24 hours'
GROUP BY event.store, event.action
ORDER BY event.store, event.action
`

const (
	paymentReconciliationHeartbeatMaximumAge = 150 * time.Minute
	paymentReconciliationWatermarkWarnAge    = 3 * time.Hour
	paymentReconciliationWatermarkPageAge    = 6 * time.Hour
)

// SIGNALS.md §2.21 maps to signal_payment_reconciliation.go and
// signal_payment_reconciliation_test.go.
func NewPaymentReconciliationSignal() Signal {
	return &signalAdapter{
		number: "2.21",
		key:    "payment-reconciliation",
		name:   "Payment reconciliation liveness and repair audit",
		probe:  paymentReconciliationProbe{},
	}
}

type paymentReconciliationProbe struct{}

func (paymentReconciliationProbe) id() string             { return "pg/payment-reconciliation" }
func (paymentReconciliationProbe) tier() string           { return tierPage }
func (paymentReconciliationProbe) cadence() time.Duration { return 5 * time.Minute }

type paymentReconciliationStoreState struct {
	store            string
	heartbeatAge     int64
	watermarkAge     int64
	skipped          int64
	errors           int64
	latestEventAge   int64
	taskCount        int64
	taskErrorCount   int64
	taskClaimedCount int64
}

func (paymentReconciliationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	healthRows, err := env.runner.pg(ctx, paymentReconciliationHealthQuery)
	if err != nil {
		return nil, err
	}
	states, err := parsePaymentReconciliationStates(healthRows)
	if err != nil {
		return nil, err
	}
	findings := paymentReconciliationHealthFindings(states)

	repairRows, err := env.runner.pg(ctx, paymentReconciliationRepairQuery)
	if err != nil {
		return nil, err
	}
	repairFindings, err := paymentReconciliationRepairFindings(repairRows)
	if err != nil {
		return nil, err
	}
	return append(findings, repairFindings...), nil
}

func parsePaymentReconciliationStates(rows []pgRow) ([]paymentReconciliationStoreState, error) {
	expected := map[string]bool{"apple": false, "google": false, "solana": false, "stripe": false}
	states := make([]paymentReconciliationStoreState, 0, len(expected))
	for _, row := range rows {
		if len(row) != 9 {
			return nil, fmt.Errorf("payment reconciliation health returned %d columns, want 9", len(row))
		}
		store := strings.TrimSpace(row.str(0))
		seen, ok := expected[store]
		if !ok {
			return nil, fmt.Errorf("payment reconciliation health returned an unknown store")
		}
		if seen {
			return nil, fmt.Errorf("payment reconciliation health repeated a store")
		}
		expected[store] = true
		values := make([]int64, 0, 8)
		for column := 1; column < 9; column++ {
			value, parseErr := parseStrictInt64(row.str(column))
			if parseErr != nil {
				return nil, fmt.Errorf("payment reconciliation health %s column %d: %w", store, column, parseErr)
			}
			values = append(values, value)
		}
		states = append(states, paymentReconciliationStoreState{
			store: store, heartbeatAge: values[0], watermarkAge: values[1], skipped: values[2],
			errors: values[3], latestEventAge: values[4], taskCount: values[5],
			taskErrorCount: values[6], taskClaimedCount: values[7],
		})
	}
	for store, seen := range expected {
		if !seen {
			return nil, fmt.Errorf("payment reconciliation health omitted store %s", store)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].store < states[j].store })
	return states, nil
}

func paymentReconciliationHealthFindings(states []paymentReconciliationStoreState) []finding {
	if len(states) == 0 {
		return nil
	}
	findings := []finding{}
	first := states[0]
	if first.heartbeatAge < 0 || time.Duration(first.heartbeatAge)*time.Second > paymentReconciliationHeartbeatMaximumAge {
		findings = append(findings, finding{
			probeId: "pg/payment-reconciliation", tier: tierPage, class: "payment-reconciliation-stale", target: "payment-reconciliation", sustain: 1,
			symptom:   "The hourly payment-reconciliation safety net has no current heartbeat",
			mechanism: "The recurring task is absent, parked, blocked, or failing before it appends its run heartbeat. Lost store notifications can therefore leave paid accounts uncredited or revoked accounts active with no bounded repair path.",
			baseline:  "A non-dry-run reconciliation heartbeat is at most 150 minutes old.",
			observed:  fmt.Sprintf("heartbeat_age_seconds=%d task_count=%d task_error_count=%d task_claimed_count=%d", first.heartbeatAge, first.taskCount, first.taskErrorCount, first.taskClaimedCount),
			evidence:  "Only aggregate ages and task counts are selected; run IDs, network IDs, transaction IDs, evidence, details, and credential values never leave PostgreSQL.",
			action:    "Inspect the singleton PaymentReconcile task state and the last taskworker execution before rescheduling anything. Restore the recurring chain or unblock its dependency; preserve normal idempotency and do not manually credit an account from this aggregate signal.",
			verify:    "A natural PaymentReconcile run completes, appends one heartbeat, retains exactly one recurring task, and the heartbeat remains inside 150 minutes through two cadences.",
			playbook:  "SIGNALS.md §2.21",
		})
	}
	if first.taskCount != 1 || first.taskErrorCount != 0 {
		findings = append(findings, finding{
			probeId: "pg/payment-reconciliation", tier: tierWarn, class: "payment-reconciliation-task", target: "PaymentReconcile", sustain: 2,
			symptom:   "The payment-reconciliation recurring task is missing, duplicated, or carrying an error",
			mechanism: "The safety net is a RunOnce singleton whose Post step schedules its successor. Zero rows loses the chain, multiple rows violate singleton ownership, and a retained reschedule error means the next run is delayed by backoff even when service liveness is green.",
			baseline:  "Exactly one PaymentReconcile row exists and it has no reschedule error; claim state may be either zero or one while a run is active.",
			observed:  fmt.Sprintf("task_count=%d task_error_count=%d task_claimed_count=%d", first.taskCount, first.taskErrorCount, first.taskClaimedCount),
			evidence:  "The query reads only function-scoped aggregate counts from pending_task; task IDs, arguments, client identity, and error text are excluded.",
			action:    "Correlate the last finished PaymentReconcile result and taskworker logs. Repair the code/config/dependency error first; seed the singleton only if the chain is genuinely absent, and never create a second run_once owner.",
			verify:    "Exactly one clean recurring row remains, a natural run reaches finished_task, its Post schedules one successor, and the heartbeat advances.",
			playbook:  "SIGNALS.md §2.21",
		})
	}
	for _, state := range states {
		if state.skipped > 0 {
			findings = append(findings, finding{
				probeId: "pg/payment-reconciliation", tier: tierPage, class: "payment-reconciliation-store-skipped", target: state.store, sustain: 1,
				symptom:   fmt.Sprintf("Payment reconciliation skipped %s because its credentials were unavailable", state.store),
				mechanism: "The global task still writes a healthy heartbeat after skipping one store, so task liveness alone masks the loss of that store's missed-notification safety net.",
				baseline:  "Zero skipped_store events for every expected store in the last three hours.",
				observed:  fmt.Sprintf("store=%s skipped_store_events_3h=%d watermark_age_seconds=%d", state.store, state.skipped, state.watermarkAge),
				evidence:  "The aggregate preserves only store, event count, and watermark age; no credential value, account, or provider object identifier is selected.",
				action:    "Run the credentials signal, provision or repair the named store credential through Vault, deploy that Vault generation, and allow the ordinary reconciliation overlap to recover missed work. Do not paste tokens into logs or manually fabricate renewal rows.",
				verify:    "The next two hourly runs contain no skipped_store event for this store, its watermark advances, and any resulting repair is audited exactly once.",
				playbook:  "SIGNALS.md §2.21 and §8.7",
			})
		}
		if state.errors > 0 {
			tier := tierWarn
			if state.errors >= 2 {
				tier = tierPage
			}
			findings = append(findings, finding{
				probeId: "pg/payment-reconciliation", tier: tier, class: "payment-reconciliation-store-error", target: state.store, sustain: 1,
				symptom:   fmt.Sprintf("Payment reconciliation recorded %d %s store error(s) in three hours", state.errors, state.store),
				mechanism: "The store adapter failed or panicked while listing or validating authoritative state. The run continues to other stores and still emits a heartbeat, but this store's watermark cannot safely advance.",
				baseline:  "Zero non-dry-run reconciliation error events for every store.",
				observed:  fmt.Sprintf("store=%s error_events_3h=%d watermark_age_seconds=%d", state.store, state.errors, state.watermarkAge),
				evidence:  "Only store-level counts and ages are selected. Provider responses, stored error details, credentials, accounts, and transaction identifiers are excluded.",
				action:    "Inspect the bounded taskworker error for this store, then distinguish authentication, provider availability, schema validation, and local persistence. Fix the cause without advancing the watermark or bypassing idempotency.",
				verify:    "A complete error-free run advances this store's watermark and no new error event appears through two hourly runs.",
				playbook:  "SIGNALS.md §2.21",
			})
		}
		if state.watermarkAge < 0 || time.Duration(state.watermarkAge)*time.Second > paymentReconciliationWatermarkWarnAge {
			tier := tierWarn
			if state.watermarkAge < 0 || time.Duration(state.watermarkAge)*time.Second > paymentReconciliationWatermarkPageAge {
				tier = tierPage
			}
			findings = append(findings, finding{
				probeId: "pg/payment-reconciliation", tier: tier, class: "payment-reconciliation-watermark-stale", target: state.store, sustain: 1,
				symptom:   fmt.Sprintf("The %s payment-reconciliation watermark is missing or stale", state.store),
				mechanism: "A watermark advances only after that store completes within its API budget without error. A stale value proves the authoritative listing is not completing even when the global heartbeat remains current.",
				baseline:  "Every expected store watermark is at most three hours old; six hours is page severity.",
				observed:  fmt.Sprintf("store=%s watermark_age_seconds=%d latest_store_event_age_seconds=%d", state.store, state.watermarkAge, state.latestEventAge),
				evidence:  "Only store-level timestamps reduced to ages are selected; no provider cursor, evidence, account, transaction, or credential value is returned.",
				action:    "Use same-window skipped/error events and taskworker diagnostics to find the failing store stage. Restore credentials or adapter correctness and retain the overlap lookback; never force the watermark forward.",
				verify:    "The store completes naturally, advances its watermark, and remains within three hours with zero skip/error events through two hourly runs.",
				playbook:  "SIGNALS.md §2.21",
			})
		}
	}
	return findings
}

func paymentReconciliationRepairFindings(rows []pgRow) ([]finding, error) {
	allowedStores := map[string]bool{"apple": true, "google": true, "solana": true, "stripe": true}
	allowedActions := map[string]bool{"credited": true, "ended": true}
	findings := make([]finding, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) != 4 {
			return nil, fmt.Errorf("payment reconciliation repairs returned %d columns, want 4", len(row))
		}
		store, action := strings.TrimSpace(row.str(0)), strings.TrimSpace(row.str(1))
		if !allowedStores[store] || !allowedActions[action] {
			return nil, fmt.Errorf("payment reconciliation repairs returned an unknown group")
		}
		frame := store + "/" + action
		if seen[frame] {
			return nil, fmt.Errorf("payment reconciliation repairs repeated a group")
		}
		seen[frame] = true
		count, err := parseStrictInt64(row.str(2))
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("payment reconciliation repairs returned an invalid count")
		}
		age, err := parseStrictInt64(row.str(3))
		if err != nil || age < 0 {
			return nil, fmt.Errorf("payment reconciliation repairs returned an invalid age")
		}
		findings = append(findings, finding{
			probeId: "pg/payment-reconciliation", tier: tierWarn, class: "payment-reconciliation-repair", target: store, frame: "action=" + action, sustain: 1,
			symptom:   fmt.Sprintf("The payment reconciler repaired %d missed %s %s event(s) in 24 hours", count, store, action),
			mechanism: "The hourly safety net found authoritative store state that the ordinary notification/task path had not applied. The repair protects the account, but its existence is evidence of a lost, rejected, or incorrectly handled payment lifecycle event.",
			baseline:  "Zero reconciler-originated credited or ended repairs; ordinary provider notifications apply each lifecycle event idempotently before reconciliation is needed.",
			observed:  fmt.Sprintf("store=%s action=%s repairs_24h=%d latest_age_seconds=%d", store, action, count, age),
			evidence:  "The query joins only run IDs that emitted a reconciliation heartbeat, then returns aggregate store/action counts and age. Run IDs, accounts, provider evidence, details, and credentials are excluded.",
			action:    "Validate the repaired account state, then trace the provider notification, verification, idempotency ledger, task, and database path for the same bounded time window. Keep the safety-net repair and fix the earlier missing stage rather than replaying provider events manually.",
			verify:    "The repaired entitlement matches authoritative store state exactly once, later notifications apply normally, and no new repair for this store/action appears through two complete reconciliation windows.",
			playbook:  "SIGNALS.md §2.21",
		})
	}
	return findings, nil
}

func parseStrictInt64(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty integer")
	}
	var parsed int64
	if _, err := fmt.Sscan(value, &parsed); err != nil {
		return 0, fmt.Errorf("invalid integer")
	}
	if fmt.Sprintf("%d", parsed) != value {
		return 0, fmt.Errorf("invalid integer")
	}
	return parsed, nil
}
