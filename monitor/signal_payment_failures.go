package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// The markets whose in-window supporter renewal must carry a Pro window: every
// subscription market (model.SubscriptionMarkets), so each market the
// subscription dashboard counts is watched here too.
var paymentFailuresEntitlementMarkets = slices.Sorted(slices.Values(model.SubscriptionMarkets))

// The markets whose missing Pro window the hourly reconciler repairs
// (controller/payment_reconcile_controller.go, repairEntitlement). The other
// markets' credit writes the renewal and its Pro window in one transaction,
// and nothing restores a lost window.
var paymentFailuresRepairedEntitlementMarkets = []string{"apple", "google", "solana", "stripe"}

// The recurring markets, whose renewal can still bill once its account is
// deleted: the subscription markets but the one-time crypto ones, which are
// prepaid fixed windows. Each has its own disposition (paymentFailureFinding);
// a test holds this list to the model's markets.
var paymentFailuresOrphanMarkets = []string{"apple", "gamma", "google", "stripe"}

// paymentFailuresSqlList renders source-owned market names, never input, as
// the SQL string literals of an IN list. A name that is not plain lowercase
// letters, digits and underscores panics at package init instead of reaching
// SQL.
func paymentFailuresSqlList(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
			panic(fmt.Sprintf("payment failures: market %q is not a plain name", name))
		}
		quoted = append(quoted, "'"+name+"'")
	}
	return strings.Join(quoted, ", ")
}

var paymentFailuresQuery = `
/* monitor-signal-2.22-payment-failures */
WITH active_supporter AS (
    SELECT DISTINCT renewal.network_id, renewal.market, min(renewal.start_time) OVER (PARTITION BY renewal.network_id, renewal.market) AS first_start
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.market IN (` + paymentFailuresSqlList(paymentFailuresEntitlementMarkets) + `)
      AND renewal.start_time <= now()
      AND now() < renewal.end_time
      AND EXISTS (
          SELECT 1 FROM network
          WHERE network.network_id = renewal.network_id
      )
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
), orphan_renewal AS (
    SELECT renewal.market AS target,
           count(DISTINCT renewal.network_id)::bigint AS issue_count,
           extract(epoch FROM now() - min(renewal.start_time))::bigint AS oldest_age,
           extract(epoch FROM now() - max(renewal.start_time))::bigint AS newest_age
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.market IN (` + paymentFailuresSqlList(paymentFailuresOrphanMarkets) + `)
      AND renewal.start_time <= now()
      AND now() < renewal.end_time
      AND NOT EXISTS (
          SELECT 1 FROM network
          WHERE network.network_id = renewal.network_id
      )
    GROUP BY renewal.market
), unfulfilled_solana AS (
    SELECT reason AS target,
           count(*)::bigint AS issue_count,
           extract(epoch FROM now() - min(record_time))::bigint AS oldest_age,
           extract(epoch FROM now() - max(record_time))::bigint AS newest_age
    FROM solana_unfulfilled_payment
    GROUP BY reason
), payment_audit AS (
    SELECT event.store AS target,
           event.action AS kind,
           count(DISTINCT event.evidence)::bigint AS issue_count,
           extract(epoch FROM now() - min(event.event_time))::bigint AS oldest_age,
           extract(epoch FROM now() - max(event.event_time))::bigint AS newest_age
    FROM payment_reconciliation_event event
    WHERE (event.action = 'refund_unmatched'
           OR (event.action = 'credit_unfulfillable' AND event.store IN ('apple', 'google', 'stripe')))
      AND NOT event.dry_run
      AND event.event_time >= now() - interval '24 hours'
      -- a store purchase recorded before its owner was known and credited
      -- later (an offer or promo code the app bound) is resolved
      AND NOT (
          event.store = 'apple'
          AND EXISTS (
              SELECT 1 FROM apple_subscription_transaction ledger
              WHERE ledger.transaction_id = event.evidence
          )
      )
      AND NOT (
          event.store = 'google'
          AND COALESCE(event.details::jsonb ->> 'reason', '') = 'destination_unresolved'
          AND EXISTS (
              SELECT 1 FROM play_purchase_binding binding
              WHERE binding.purchase_token = event.evidence
          )
      )
    GROUP BY event.store, event.action
), crypto_held AS (
    -- crypto invoices whose payment an operator holds: a sanctions match, a
    -- sender the screen could not read, a TAO transfer shape the rail does
    -- not credit, an amount no invoice of a shared address was quoted
    SELECT receipt.asset AS target,
           CASE receipt.review_reason
               WHEN 'sanctions_hold' THEN 'crypto_sanctions_hold'
               WHEN 'screening_unavailable' THEN 'crypto_screening_unavailable'
               ELSE 'crypto_payment_held'
           END AS kind,
           count(DISTINCT receipt.intent_id)::bigint AS issue_count,
           extract(epoch FROM now() - min(receipt.first_seen_at))::bigint AS oldest_age,
           extract(epoch FROM now() - max(receipt.first_seen_at))::bigint AS newest_age
    FROM crypto_payment_receipt receipt
    WHERE receipt.state = 'review'
      AND receipt.review_reason IN ('sanctions_hold', 'screening_unavailable', 'transfer_shape', 'amount_unmatched')
      AND receipt.intent_id IS NOT NULL
      AND receipt.first_seen_at <= now()
    GROUP BY 1, 2
), crypto_scanner AS (
    -- a coin's scanner that succeeded in the last week and has not for 15
    -- minutes: payments to its open invoices are not being read (a cursor
    -- left by a coin taken out of service stops paging after the week)
    SELECT scan.asset AS target,
           1::bigint AS issue_count,
           extract(epoch FROM now() - scan.last_success_at)::bigint AS oldest_age,
           extract(epoch FROM now() - scan.last_success_at)::bigint AS newest_age
    FROM crypto_scan_cursor scan
    WHERE scan.last_success_at IS NOT NULL
      AND scan.last_success_at < now() - interval '15 minutes'
      AND scan.last_success_at > now() - interval '7 days'
)
SELECT 'entitlement_missing' AS kind, target, issue_count, oldest_age, newest_age
FROM missing_entitlement
UNION ALL
SELECT 'orphan_renewal' AS kind, target, issue_count, oldest_age, newest_age
FROM orphan_renewal
UNION ALL
SELECT 'solana_unfulfilled' AS kind, target, issue_count, oldest_age, newest_age
FROM unfulfilled_solana
UNION ALL
SELECT kind, target, issue_count, oldest_age, newest_age
FROM payment_audit
UNION ALL
SELECT kind, target, issue_count, oldest_age, newest_age
FROM crypto_held
UNION ALL
SELECT 'crypto_scanner_stalled' AS kind, target, issue_count, oldest_age, newest_age
FROM crypto_scanner
ORDER BY kind, target
`

// The crypto rail's coins (model.CryptoAssets).
var paymentFailuresCryptoAssets = paymentFailuresNameSet(model.CryptoAssets)

func paymentFailuresNameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

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
		baseline: "No durable paid-but-unfulfilled payment, paying-account entitlement mismatch, orphaned active renewal, unmatched refund, or paid Stripe invoice that names no account remains unresolved.",
		observed: fmt.Sprintf("kind=%s target=%s count=%d oldest_age_seconds=%d newest_age_seconds=%d", kind, target, count, oldestAge, newestAge),
		evidence: "Only aggregate kind/target counts and ages are selected. Network IDs, user identity, payment references, transaction signatures, provider evidence, stored details, and credentials never leave PostgreSQL.",
		playbook: "SIGNALS.md §2.22",
	}
	switch kind {
	case "entitlement_missing":
		if !slices.Contains(paymentFailuresEntitlementMarkets, target) {
			return finding{}, fmt.Errorf("payment failures returned an unknown entitlement market")
		}
		common.class = "payment-entitlement-missing"
		common.symptom = fmt.Sprintf("%d active %s paying account(s) have no current Pro entitlement", count, target)
		common.mechanism = "The authoritative local renewal window says the store is still billing, but no in-window transfer_balance with pro=true exists. Clients therefore receive free-tier behavior even though the account remains paid."
		common.action = "Confirm this store's reconciliation credentials and authoritative lookup are healthy, then allow the ordinary reconciler to validate the exact renewal and restore only its missing Pro metadata. Trace and fix the earlier write-path loss. Never grant from the local renewal alone or expose account identifiers in the alert."
		common.verify = "The provider-confirmed reconciliation records one entitlement_repaired event, the Pro cache and a refreshed client JWT report Pro, and the aggregate remains zero through two later reconciliation runs without changing purchased bytes or revenue."
		if !slices.Contains(paymentFailuresRepairedEntitlementMarkets, target) {
			common.mechanism = fmt.Sprintf("The %s credit writes the renewal and its Pro window (transfer_balance with pro=true) in one transaction, and the hourly reconciler repairs a Pro window only for Apple, Google Play, Stripe and Solana. A renewal still in its paid window with no Pro window means a later write ended or removed the window, and nothing restores it: the account is paid but gets free-tier behavior.", target)
			common.action = "Find and fix the write that ended or removed the Pro window. Restore the window only through an authorized repair with the owner's go; waiting for reconciliation does not restore it. Never grant from the local renewal alone or expose account identifiers in the alert."
			common.verify = "The account's Pro window covers its paid renewal again, a refreshed client JWT reports Pro, and the aggregate remains zero through two later runs without changing purchased bytes or revenue."
		}
	case "orphan_renewal":
		if !slices.Contains(paymentFailuresOrphanMarkets, target) {
			return finding{}, fmt.Errorf("payment failures returned an unknown orphan-renewal market")
		}
		common.tier = tierWarn
		common.class = "payment-renewal-orphan"
		common.symptom = fmt.Sprintf("%d deleted account(s) retain in-window local %s renewal history", count, target)
		common.mechanism = "Account deletion removed the owning network but retained one or more in-window local rows for a recurring subscription market. This is historical residue, not a paying account eligible for Pro repair. The aggregate counts distinct deleted owners rather than renewal rows, and local history alone cannot prove whether provider-side cancellation completed or which pre-fence write/delete ordering created it."
		common.observed = fmt.Sprintf("kind=%s target=%s deleted_owner_count=%d oldest_renewal_start_age_seconds=%d newest_renewal_start_age_seconds=%d", kind, target, count, oldestAge, newestAge)
		common.action = "Using authorized provider tooling, determine whether each deleted owner's provider subscription remains active and complete the supported cancellation or support workflow. Compare exact running artifacts with current-main Server commit 2b28387a, which adds the network-row deletion/credit fence plus provider-confirmed cancellation-before-delete for Stripe and failure propagation, and commit df2ea15b, which adds complete bounded Stripe subscription discovery. Deploy API and Taskworker only where those runtime capabilities are absent. Stripe subscriptions missing active local renewal metadata remain a separate discovery gap until df2ea15b is present. Apple and Google disposition is separate policy, not evidence that Stripe cancellation covered them. Retain financial history; do not recreate the network, grant Pro, or infer current billing from the local row alone."
		common.verify = "Every affected deleted owner has a provider-side disposition; a failed Stripe cancellation retains the network; delayed paid writers stop before consuming their ledger or intent; and no new deleted owner enters this aggregate through two reconciliation windows. Alert disappearance from natural expiry alone is not causal closure."
		if target == "google" {
			common.mechanism = "Account deletion removed the owner while its local Google renewal remains in the paid window. The aggregate counts distinct deleted owners rather than renewal rows. Google Play cancellation stops future payments without ending the paid local window, so successful cancellation before deletion can produce this same aggregate. Local history alone establishes neither current provider billing nor the deletion's artifact or ordering."
			common.action = "Establish each deleted owner's Google Play provider-side disposition with authorized tooling, including whether future payments are stopped. Compare the exact running API artifact with Server commit 1a67d9d3 (Google Play cancellation before deletion) and commit cd881b22 (one store snapshot plus renewal recheck under the deletion lock); repair or deploy only where those capabilities are absent. Verify the shared payment network-row fence from commit 2b28387a separately in API and Taskworker. A source checkout or renewal start age does not establish the historical deletion artifact or deletion time. Retain financial history; do not recreate the network, grant Pro, or infer current billing from the local row alone."
			common.verify = "Every affected deleted owner has a Google Play provider-side disposition; failed Play cancellation or a renewal outside the checked snapshot retains the network; and no new unresolved provider disposition appears through two reconciliation/deletion windows. Confirmed canceled paid-window history may remain in the aggregate until expiry. Alert disappearance from natural expiry alone is not causal closure."
		}
		if target == "gamma" {
			common.mechanism = "Account deletion removed the owner while its local gamma renewal is still in its paid window. The aggregate counts distinct deleted owners rather than renewal rows. A plan paid with gamma renews through our own biller, not a store: deletion neither refuses nor cancels it, and the account's gamma_account row (automatic renewal, plan and deposit hotkey) survives it. The biller renews from that row near the end of the term without reading the network, so it can charge the deposit for an account the credit can no longer reach: the credit stops on the missing network."
			common.action = "Before the term ends, stop the deleted owner's automatic gamma renewal with the owner's go, and check whether the biller already made and charged a renew intent for it that was never credited: that payment needs an authorized refund or support disposition. Fix account deletion so it ends gamma's automatic renewal. Retain financial history; do not recreate the network, grant Pro, or credit another account from the local row."
			common.verify = "No renew intent is made for a deleted owner, every affected deleted owner has a documented disposition, and no new deleted owner enters this aggregate through two term ends. Alert disappearance from natural expiry alone is not causal closure."
		}
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
	case "credit_unfulfillable":
		if !map[string]bool{"apple": true, "google": true, "stripe": true}[target] {
			return finding{}, fmt.Errorf("payment failures returned an unknown unfulfillable-credit store")
		}
		common.tier = tierWarn
		common.class = "payment-credit-unfulfillable"
		if target != "stripe" {
			storeName := map[string]string{"apple": "App Store", "google": "Google Play"}[target]
			common.symptom = fmt.Sprintf("%d paid %s purchase(s) were not credited because no live account can be credited for them, in 24 hours", count, storeName)
			common.mechanism = "The store confirmed a paid purchase that names no live network: an App Store transaction with no appAccountToken and no offer-code binding, or a token naming a deleted network; a Play purchase with no account identifiers and no binding, an account link naming a deleted network, or a purchase bound to a different network than the one it renewed for. The server credits nobody on a guess; the notification webhook or the payment reconciler recorded the purchase once. A purchase the app later binds and credits (an offer or promo code) drops out of this count. Family-shared App Store transactions are not the customer's own purchase and are not recorded."
			common.context = "The durable event (payment_reconciliation_event, store=apple or google, action=credit_unfulfillable, evidence=transaction id or purchase token) carries the reason (destination_unresolved, destination_deleted or bound_to_another_network), product and dates; Play events carry the order id on the customer's receipt. The count is distinct purchases."
			common.action = "Match the purchase to its payer with authorized store tooling (App Store Connect order lookup, Play Console order id) and the support thread. For a verified owner, have the customer restore or re-verify the purchase from the app while signed in to that network, or refund it in the store. Never credit a network on an email or name match, and never edit balances by hand."
			common.verify = "Each recorded purchase is credited once through its store ledger after the owner restores it, or has a documented refund or support disposition. The aggregate can remain until its 24-hour window expires; aging out does not prove disposition."
			break
		}
		common.symptom = fmt.Sprintf("%d paid Stripe invoice(s) were not credited because they name no live account, in 24 hours", count)
		common.mechanism = "invoice.paid resolves the account only from the subscription's network_id metadata or the checkout session's client_reference_id. The customer-email fallback is removed (UPGRADE.md S11). After complete authority reads find neither destination, the webhook durably records destination_unresolved with leg=webhook before acknowledging delivery. Reconciliation also records unresolved destinations, or destination_deleted when the named network is absent under the credit lifecycle lock. This aggregate does not distinguish those reasons, establish account deletion, or prove a provider failure."
		common.context = "The durable event (payment_reconciliation_event, store=stripe, action=credit_unfulfillable, evidence=invoice id) retains the payment for support. Unresolved-webhook details carry the subscription, customer, customer email, amount and paid period. The count is distinct invoices; the webhook and the reconciler may both record one. The complete unresolved-webhook tuple belongs to this WARN; malformed records remain a separate §2.21 PAGE."
		common.action = "Using authorized read-only provider tooling and payment/support evidence, establish which account paid. For a verified mapping, set network_id metadata on the Stripe subscription, then resend the invoice.paid event from the Stripe dashboard so the ordinary ledger-gated credit runs; future renewals then resolve by metadata. Otherwise record an authorized refund or support disposition. Email alone is not safe write authority; never edit balances by hand."
		common.verify = "Each invoice is either credited once through the stripe_invoice ledger after its metadata repair or has a documented refund/support disposition, without a duplicate credit. The audit aggregate can remain until its 24-hour window expires; neither aging out nor a fresh reconciliation watermark proves disposition."
	case "crypto_sanctions_hold":
		if !paymentFailuresCryptoAssets[target] {
			return finding{}, fmt.Errorf("payment failures returned an unknown crypto asset")
		}
		common.class = "crypto-sanctions-hold"
		common.symptom = fmt.Sprintf("%d %s crypto invoice payment(s) are held because a sender is on OFAC's sanctioned addresses", count, target)
		common.mechanism = "Before a TAO, Bitcoin, ETH or SOL credit the scanner screens every sender the chain names against the newest config/all/crypto table (cryptosanctions.Match, BTCPAY.md §17.3, §17.5). A match moved the payment's receipts to review (sanctions_hold); nothing was credited, and the buyer sees only \"screening\"."
		common.action = "Do not credit and do not refund. Bring the invoice (bringyourctl crypto show) to the owner and counsel, who decide. Only a hold they cleared is credited, with bringyourctl crypto credit --sanctions-cleared; never move funds out of Coinbase for it without their go."
		common.verify = "Each held invoice has the owner's and counsel's documented disposition; a cleared one is credited once; the receipts leave review only through the operator tools."
	case "crypto_screening_unavailable":
		if !paymentFailuresCryptoAssets[target] {
			return finding{}, fmt.Errorf("payment failures returned an unknown crypto asset")
		}
		common.class = "crypto-screening-unavailable"
		common.symptom = fmt.Sprintf("%d %s crypto invoice payment(s) are held because their senders could not be screened", count, target)
		common.mechanism = "The sanctions table could not be read (no config/all/crypto/<release>/ table, or one that fails verification), or a sender's address cannot be read on its chain (a Bitcoin input with no address). The scanner never credits unscreened: the receipts are in review (screening_unavailable) and screened again each scan."
		common.action = "Check the deployed config/all/crypto/<release>/ table and the release's cryptoctl run (build/all/run.sh); restore the table rather than crediting by hand. A payment the table cannot screen at all (an unnamed input) goes to the owner, who may credit it with bringyourctl crypto credit after review."
		common.verify = "Once the table reads, the held payments screen clean and are credited by the scanner, or become sanctions_hold; the aggregate returns to zero."
	case "crypto_payment_held":
		if !paymentFailuresCryptoAssets[target] {
			return finding{}, fmt.Errorf("payment failures returned an unknown crypto asset")
		}
		common.tier = tierWarn
		common.class = "crypto-payment-held"
		common.symptom = fmt.Sprintf("%d %s crypto invoice payment(s) wait for an operator", count, target)
		common.mechanism = "A TAO transfer of a shape the rail does not credit by itself (a batch, a proxy or multisig call, an EVM precompile send, a stake transfer: transfer_shape), or a payment to a coin's one shared deposit address whose amount no open invoice was quoted (amount_unmatched). Coinbase publishes nothing on which TAO transfer shapes it credits."
		common.action = "Confirm in our Coinbase account that the funds arrived as the coin, then credit the invoice with bringyourctl crypto credit, or return the coins on the owner's go. Never credit on the chain's word alone for these shapes."
		common.verify = "Each held invoice is credited once or has a documented refund; the aggregate returns to zero."
	case "crypto_scanner_stalled":
		if !paymentFailuresCryptoAssets[target] {
			return finding{}, fmt.Errorf("payment failures returned an unknown crypto asset")
		}
		common.class = "crypto-scanner-stalled"
		common.symptom = fmt.Sprintf("the %s payment scanner has not completed a scan for %d minutes", target, oldestAge/60)
		common.mechanism = "The coin's scan chain fails every run (its chain source is down, refuses the key, lags or pauses on a runtime it cannot read, or its vault file is gone): the coin is listed as unavailable, and payments to its open invoices are not read until it recovers. crypto_scan_cursor.last_error holds the reason."
		common.action = "Read crypto_scan_cursor.last_error for the coin and the taskworker log ([cryptopay]). Restore the chain source (Alchemy, Helius, snow's Finney node, the wallet daemon) or its vault file. For TAO on a new runtime, check that the reader decodes it before forcing anything; never point the scanner at an older block range."
		common.verify = "The scanner succeeds again, the coin is listed as available, and payments made while it was down are credited from the chain."
	default:
		return finding{}, fmt.Errorf("payment failures returned an unknown kind")
	}
	return common, nil
}
