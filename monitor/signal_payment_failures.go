package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const paymentFailuresQuery = `
/* monitor-signal-2.22-payment-failures */
WITH active_supporter AS (
    SELECT DISTINCT renewal.network_id, renewal.market, min(renewal.start_time) OVER (PARTITION BY renewal.network_id, renewal.market) AS first_start
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.start_time <= now()
      AND now() < renewal.end_time
), missing_entitlement AS (
    SELECT active_supporter.market AS target,
           count(DISTINCT active_supporter.network_id)::bigint AS issue_count,
           extract(epoch FROM now() - min(active_supporter.first_start))::bigint AS oldest_age,
           extract(epoch FROM now() - max(active_supporter.first_start))::bigint AS newest_age
    FROM active_supporter
    WHERE NOT EXISTS (
        SELECT 1
        FROM transfer_balance balance
        WHERE balance.network_id = active_supporter.network_id
          AND balance.pro
          AND balance.start_time <= now()
          AND now() < balance.end_time
    )
    GROUP BY active_supporter.market
), unfulfilled_solana AS (
    SELECT reason AS target,
           count(*)::bigint AS issue_count,
           extract(epoch FROM now() - min(record_time))::bigint AS oldest_age,
           extract(epoch FROM now() - max(record_time))::bigint AS newest_age
    FROM solana_unfulfilled_payment
    GROUP BY reason
), payment_audit AS (
    SELECT store AS target,
           action AS kind,
           count(*)::bigint AS issue_count,
           extract(epoch FROM now() - min(event_time))::bigint AS oldest_age,
           extract(epoch FROM now() - max(event_time))::bigint AS newest_age
    FROM payment_reconciliation_event
    WHERE action IN ('refund_unmatched', 'email_fallback')
      AND NOT dry_run
      AND event_time >= now() - interval '24 hours'
    GROUP BY store, action
)
SELECT 'entitlement_missing' AS kind, target, issue_count, oldest_age, newest_age
FROM missing_entitlement
UNION ALL
SELECT 'solana_unfulfilled' AS kind, target, issue_count, oldest_age, newest_age
FROM unfulfilled_solana
UNION ALL
SELECT kind, target, issue_count, oldest_age, newest_age
FROM payment_audit
ORDER BY kind, target
`

// SIGNALS.md §2.22 maps to signal_payment_failures.go and
// signal_payment_failures_test.go.
func NewPaymentFailuresSignal() Signal {
	return &signalAdapter{
		number: "2.22",
		key:    "payment-failures",
		name:   "Durable payment and entitlement failures",
		probe:  paymentFailuresProbe{},
	}
}

type paymentFailuresProbe struct{}

func (paymentFailuresProbe) id() string             { return "pg/payment-failures" }
func (paymentFailuresProbe) tier() string           { return tierPage }
func (paymentFailuresProbe) cadence() time.Duration { return 5 * time.Minute }

func (paymentFailuresProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, paymentFailuresQuery)
	if err != nil {
		return nil, err
	}
	findings := make([]finding, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) != 5 {
			return nil, fmt.Errorf("payment failures returned %d columns, want 5", len(row))
		}
		kind, target := strings.TrimSpace(row.str(0)), strings.TrimSpace(row.str(1))
		identity := kind + "/" + target
		if seen[identity] {
			return nil, fmt.Errorf("payment failures repeated a group")
		}
		seen[identity] = true
		count, err := parseStrictInt64(row.str(2))
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("payment failures returned an invalid count")
		}
		oldestAge, err := parseStrictInt64(row.str(3))
		if err != nil || oldestAge < 0 {
			return nil, fmt.Errorf("payment failures returned an invalid oldest age")
		}
		newestAge, err := parseStrictInt64(row.str(4))
		if err != nil || newestAge < 0 || newestAge > oldestAge {
			return nil, fmt.Errorf("payment failures returned an invalid newest age")
		}
		finding, err := paymentFailureFinding(kind, target, count, oldestAge, newestAge)
		if err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func paymentFailureFinding(kind, target string, count, oldestAge, newestAge int64) (finding, error) {
	common := finding{
		probeId: "pg/payment-failures", tier: tierPage, target: target, frame: "kind=" + kind, sustain: 1,
		baseline: "No durable paid-but-unfulfilled payment, paying-account entitlement mismatch, unmatched refund, or legacy identity fallback remains unresolved.",
		observed: fmt.Sprintf("kind=%s target=%s count=%d oldest_age_seconds=%d newest_age_seconds=%d", kind, target, count, oldestAge, newestAge),
		evidence: "Only aggregate kind/target counts and ages are selected. Network IDs, user identity, payment references, transaction signatures, provider evidence, stored details, and credentials never leave PostgreSQL.",
		playbook: "SIGNALS.md §2.22",
	}
	switch kind {
	case "entitlement_missing":
		if !map[string]bool{"apple": true, "google": true, "solana": true, "stripe": true}[target] {
			return finding{}, fmt.Errorf("payment failures returned an unknown entitlement market")
		}
		common.class = "payment-entitlement-missing"
		common.symptom = fmt.Sprintf("%d active %s paying account(s) have no current Pro entitlement", count, target)
		common.mechanism = "The authoritative local renewal window says the store is still billing, but no in-window transfer_balance with pro=true exists. Clients therefore receive free-tier behavior even though the account remains paid."
		common.action = "Inspect the bounded renewal and entitlement write path for one affected account using privileged tooling, repair the idempotent grant path, and refresh the Pro cache. Do not infer entitlement from revenue alone or expose account identifiers in the alert."
		common.verify = "Every active supporter renewal has an in-window pro=true entitlement, client JWT refresh reports Pro, and the aggregate remains zero through two payment-reconciliation runs."
	case "solana_unfulfilled":
		if target != "no_intent" && target != "underpaid" {
			return finding{}, fmt.Errorf("payment failures returned an unknown Solana reason")
		}
		common.class = "payment-solana-unfulfilled"
		common.symptom = fmt.Sprintf("%d received Solana payment(s) remain unfulfilled with reason %s", count, target)
		common.mechanism = "Helius delivered a confirmed transfer but the server could not match a live intent or the received amount was below the quoted amount. The webhook was acknowledged, so the durable row is the only bounded recovery source."
		common.action = "Validate the on-chain transfer and intent lookup through the supported reconciliation path. Credit only an exactly matched, sufficiently funded payment; otherwise route the durable row for an authorized refund/support decision. Never delete it to clear the alert."
		common.verify = "Every valid transfer is credited idempotently or receives a documented authorized resolution, the durable row clears through the supported path, and no duplicate grant is created."
	case "refund_unmatched":
		if target != "stripe" {
			return finding{}, fmt.Errorf("payment failures returned an unknown unmatched-refund store")
		}
		common.class = "payment-refund-unmatched"
		common.symptom = fmt.Sprintf("%d Stripe refund or dispute event(s) could not be mapped to a granted purchase in 24 hours", count)
		common.mechanism = "The processor withdrew or disputed funds, but the webhook could not find the idempotency ledger or purchase record that names what entitlement/data should be clawed back. Guessing would affect the wrong account."
		common.action = "Correlate the processor object with the immutable Stripe invoice/data-pack ledger using privileged tooling, repair the missing identity link, and apply the normal idempotent clawback. Do not guess from email or manually edit balances."
		common.verify = "Each unmatched event has an authorized resolution, future refunds resolve through immutable IDs, and the 24-hour aggregate returns to zero without duplicate clawback."
	case "email_fallback":
		if target != "stripe" {
			return finding{}, fmt.Errorf("payment failures returned an unknown email-fallback store")
		}
		common.tier = tierWarn
		common.class = "payment-identity-fallback"
		common.symptom = fmt.Sprintf("%d Stripe credit(s) used the legacy customer-email identity fallback in 24 hours", count)
		common.mechanism = "The payment was credited, but immutable network metadata was absent and the handler fell back to customer email. Email is mutable and non-unique across account lifecycle, so continued use is a correctness risk rather than a healthy payment path."
		common.action = "Find why checkout omitted immutable network metadata and fix that producer. Preserve the credited idempotency ledger; do not replay the invoice or log the customer email."
		common.verify = "New checkout objects carry immutable network metadata, invoice credits use it, and no email_fallback event appears for two full reconciliation windows."
	default:
		return finding{}, fmt.Errorf("payment failures returned an unknown kind")
	}
	return common, nil
}
