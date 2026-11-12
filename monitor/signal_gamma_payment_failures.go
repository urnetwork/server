package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Pay with Gamma payments that cannot be credited (mmm/BTCPAY.md §18.4): the
// biller's alarms for them (credit_unfulfillable, batch_unfulfillable) were on
// the gamma dashboard only. A gamma spend cannot be refunded on chain, so a
// customer whose gamma was spent and never credited needs an operator.
//
// The gamma tables exist from the Gamma migration on. Below that migration
// head nothing can have been spent, so the signal reads nothing; at or past it
// the tables must exist.

// What the schema preflight reads: the installed migration head, and whether
// the gamma tables the observation reads exist. PostgreSQL resolves every
// relation a statement names before it runs, so the observation is issued
// only once they do.
const gammaPaymentFailuresSchemaQuery = `
/* monitor-signal-2.22a-gamma-payment-failures-schema */
SELECT coalesce(max(end_version_number), 0)::bigint,
       (to_regclass('public.gamma_payment_intent') IS NOT NULL
        AND to_regclass('public.gamma_charge') IS NOT NULL
        AND to_regclass('public.gamma_alarm') IS NOT NULL
        AND to_regclass('public.network') IS NOT NULL)
FROM migration_audit
WHERE status = 'success'
`

// The aggregate of the gamma spends no credit reaches and of the live batches
// that carried one. Only kinds, counts and ages leave PostgreSQL.
var gammaPaymentFailuresQuery = `
/* monitor-signal-2.22a-gamma-payment-failures */
WITH spent_uncredited AS (
    -- our finalized GammaSpent took the price, and the intent is neither
    -- credited nor voided (voiding records an operator's disposition)
    SELECT intent.intent_id,
           intent.credit_failure_count,
           charge.resolved_at,
           EXISTS (
               SELECT 1 FROM network
               WHERE network.network_id = intent.network_id
           ) AS network_exists
    FROM gamma_charge charge
    INNER JOIN gamma_payment_intent intent ON intent.intent_id = charge.intent_id
    WHERE charge.state = 'spent'
      AND intent.credited_at IS NULL
      AND intent.voided_at IS NULL
), credit_problem AS (
    SELECT CASE WHEN spent_uncredited.network_exists THEN 'gamma_credit_failed' ELSE 'gamma_credit_unfulfillable' END AS kind,
           count(DISTINCT spent_uncredited.intent_id)::bigint AS issue_count,
           greatest(0, extract(epoch FROM now() - min(spent_uncredited.resolved_at)))::bigint AS oldest_age,
           greatest(0, extract(epoch FROM now() - max(spent_uncredited.resolved_at)))::bigint AS newest_age
    FROM spent_uncredited
    WHERE NOT spent_uncredited.network_exists
       OR ` + strconv.Itoa(controller.GammaMaxCreditFailures) + ` <= spent_uncredited.credit_failure_count
    GROUP BY 1
), batch_alarm AS (
    -- the biller stopped resubmitting a live batch that carries a charge no
    -- credit can reach; the alarm is recorded once per batch
    SELECT 'gamma_batch_unfulfillable' AS kind,
           count(*)::bigint AS issue_count,
           greatest(0, extract(epoch FROM now() - min(alarm.create_time)))::bigint AS oldest_age,
           greatest(0, extract(epoch FROM now() - max(alarm.create_time)))::bigint AS newest_age
    FROM gamma_alarm alarm
    WHERE alarm.kind = '` + gammaPaymentFailuresSqlName(controller.GammaAlarmKindBatchUnfulfillable) + `'
      AND alarm.create_time >= now() - interval '24 hours'
    HAVING count(*) > 0
)
SELECT kind, 'gamma' AS target, issue_count, oldest_age, newest_age
FROM credit_problem
UNION ALL
SELECT kind, 'gamma' AS target, issue_count, oldest_age, newest_age
FROM batch_alarm
ORDER BY kind
`

// gammaPaymentFailuresSqlName renders a source-owned name, never input, into
// SQL; anything but lowercase letters and underscores panics at package init.
func gammaPaymentFailuresSqlName(name string) string {
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz_") != "" {
		panic(fmt.Sprintf("gamma payment failures: %q is not a plain name", name))
	}
	return name
}

// SIGNALS.md §2.22a maps to signal_gamma_payment_failures.go and
// signal_gamma_payment_failures_test.go.
func NewGammaPaymentFailuresSignal() Signal {
	return &signalAdapter{
		number: "2.22a",
		key:    "gamma-payment-failures",
		name:   "Pay with Gamma payments that cannot be credited",
		probe:  gammaPaymentFailuresProbe{},
	}
}

type gammaPaymentFailuresProbe struct{}

func (gammaPaymentFailuresProbe) id() string             { return "pg/gamma-payment-failures" }
func (gammaPaymentFailuresProbe) tier() string           { return tierPage }
func (gammaPaymentFailuresProbe) cadence() time.Duration { return 5 * time.Minute }

func (gammaPaymentFailuresProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	gammaVersion := server.GammaPaySchemaMigrationVersion()
	if gammaVersion <= 0 {
		return nil, fmt.Errorf("gamma payment failures: this build carries no gamma migration")
	}
	rows, err := env.runner.pg(ctx, gammaPaymentFailuresSchemaQuery)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("gamma payment failures schema observation is incomplete")
	}
	head, err := parseStrictInt64(rows[0].str(0))
	if err != nil || head < 0 || (rows[0].str(1) != "t" && rows[0].str(1) != "f") {
		return nil, fmt.Errorf("gamma payment failures schema observation is invalid")
	}
	if head < int64(gammaVersion) {
		// before the Gamma migration: nothing can have been spent
		return nil, nil
	}
	if rows[0].str(1) != "t" {
		return nil, fmt.Errorf("gamma payment tables unavailable at the installed migration head")
	}

	rows, err = env.runner.pg(ctx, gammaPaymentFailuresQuery)
	if err != nil {
		return nil, err
	}
	findings := make([]finding, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) != 5 {
			return nil, fmt.Errorf("gamma payment failures returned %d columns, want 5", len(row))
		}
		kind, target := strings.TrimSpace(row.str(0)), strings.TrimSpace(row.str(1))
		if seen[kind] {
			return nil, fmt.Errorf("gamma payment failures repeated a group")
		}
		seen[kind] = true
		if target != "gamma" {
			return nil, fmt.Errorf("gamma payment failures returned an unknown target")
		}
		count, err := parseStrictInt64(row.str(2))
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("gamma payment failures returned an invalid count")
		}
		oldestAge, err := parseStrictInt64(row.str(3))
		if err != nil || oldestAge < 0 {
			return nil, fmt.Errorf("gamma payment failures returned an invalid oldest age")
		}
		newestAge, err := parseStrictInt64(row.str(4))
		if err != nil || newestAge < 0 || newestAge > oldestAge {
			return nil, fmt.Errorf("gamma payment failures returned an invalid newest age")
		}
		finding, err := gammaPaymentFailureFinding(kind, count, oldestAge, newestAge)
		if err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func gammaPaymentFailureFinding(kind string, count, oldestAge, newestAge int64) (finding, error) {
	common := finding{
		probeId: "pg/gamma-payment-failures", tier: tierPage, target: "gamma", frame: "kind=" + kind, sustain: 1,
		baseline: "Every gamma spend from a customer's deposit is credited to the account it paid for, and no live spend batch carries a charge that can no longer be credited.",
		observed: fmt.Sprintf("kind=%s count=%d oldest_age_seconds=%d newest_age_seconds=%d", kind, count, oldestAge, newestAge),
		evidence: "Only aggregate kind counts and ages are selected. Intent and network ids, deposit hotkeys and coldkeys, spend references, blocks and alarm details never leave PostgreSQL.",
		playbook: "SIGNALS.md §2.22a",
	}
	switch kind {
	case "gamma_credit_unfulfillable":
		common.class = "gamma-credit-unfulfillable"
		common.symptom = fmt.Sprintf("%d gamma payment(s) were spent from the customer's deposit for an account that no longer exists, and were never credited", count)
		common.mechanism = "Our own finalized GammaSpent took the price from the customer's SN25 gamma deposit, but the intent's network no longer exists, so its credit stops on the missing network (gamma alarm credit_unfulfillable). Account deletion waits for a gamma charge in flight (server 57f98311f), so only a network removed another way, or a build older than that, gets here. Nothing credits the payment by itself, and gamma cannot be refunded on chain."
		common.context = "The count is distinct intents: a spent gamma_charge whose gamma_payment_intent is neither credited nor voided and names a network that is gone; ages are from the spend's resolution. The gamma_alarm row of kind credit_unfulfillable (key credit_unfulfillable/<intent id>) names the intent, network, deposit hotkey, block and reference; the biller retries the credit and counts the failures."
		common.action = "From the alarm, find the intent and establish who paid from support evidence and the deposit's coldkey on chain. Bring each to the owner for an authorized disposition: a credit to the customer's verified live account, or a refund in another asset (gamma itself cannot be refunded). Never recreate the deleted network, and never credit an account on an email or name match. Once the disposition is done, record it by voiding the intent (gamma_payment_intent.voided_at): the biller stops retrying it and it leaves this count. Then find how the network was removed outside account deletion, and compare the running api and taskworker with server commit 57f98311f."
		common.verify = "Each counted intent has the owner's documented disposition and is voided after its spend, the aggregate returns to zero, and no new spend lands for a network that is gone."
	case "gamma_credit_failed":
		common.class = "gamma-credit-failed"
		common.symptom = fmt.Sprintf("%d gamma payment(s) were spent from the customer's deposit and their credit has failed %d times or more", count, controller.GammaMaxCreditFailures)
		common.mechanism = fmt.Sprintf("Our own finalized GammaSpent took the price, and crediting the intent (the plan, data pack or balance code it buys) failed at least %d times for an account that still exists, so its status says review (credit_failed). The biller retries a few such credits each run, the least retried first, and credits the intent once the cause is fixed; nothing else credits it.", controller.GammaMaxCreditFailures)
		common.context = "The count is distinct intents: a spent gamma_charge whose gamma_payment_intent is neither credited nor voided, names a live network and has failed its credit that many times; ages are from the spend's resolution."
		common.action = "Read the taskworker log for \"[gamma]credit of intent <id> failed (<n>): <error>\" and fix the cause: the Pro configuration (the data code duration, the data pack catalog) or a database failure. Do not credit by hand or edit balances: the biller's retry credits the intent once, through its one-shot. If the cause cannot be fixed, bring it to the owner for an authorized disposition, and void the intent once that is done."
		common.verify = "Each counted intent is credited (status paid) or voided after a documented disposition, and the aggregate returns to zero."
	case "gamma_batch_unfulfillable":
		common.tier = tierWarn
		common.class = "gamma-batch-unfulfillable"
		common.symptom = fmt.Sprintf("%d live gamma spend batch(es) carried a charge that can no longer be credited, in 24 hours", count)
		common.mechanism = "The biller found its live spend batch carrying a pending charge whose intent was closed (voided or credited) or whose network no longer exists, and stopped resubmitting it (gamma alarm batch_unfulfillable). Account deletion waits for a batch in flight, so only a network removed another way, or an intent closed by hand, gets here. No other batch is signed until this one's era ends (64 blocks, about 13 minutes), so gamma billing waits meanwhile; the pool may still include the batch, and a spend that lands for such a charge can never be credited."
		common.context = "The count is distinct batch_unfulfillable alarms recorded in the last 24 hours (key batch_unfulfillable/<batch id>; the detail names the batch and its nonce). It ages out after 24 hours, and aging out is no disposition."
		common.action = "Find the batch from the alarm in gamma_spend_batch. Expired: its other charges went out again in the next batch with their references unchanged. Included: list its charges that were spent for an intent that is voided or credited or whose network is gone; each is a customer payment no credit reaches (gamma-credit-unfulfillable counts those whose network is gone) and needs the owner's authorized disposition. Then find how the intent was closed or the network removed outside account deletion, and stop that path."
		common.verify = "The batch has expired or landed, every spend it landed for an unfulfillable charge has a documented disposition, and no new batch_unfulfillable alarm is recorded."
	default:
		return finding{}, fmt.Errorf("gamma payment failures returned an unknown kind")
	}
	return common, nil
}
