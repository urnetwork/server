package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Pay with Gamma's watcher alarms and billing readiness (mmm/BTCPAY.md §18.4,
// §18.8): the biller records an alarm for what the watcher must never see (a
// spend from our spender with a reference we never issued, the first sign of
// a leaked billing key; a reference spent twice; a change to the spender's
// proxies; ...) and pauses billing when it is not ready, but both reached the
// gamma dashboard only. SIGNALS.md §2.22a covers the spends no credit
// reaches; this signal, SIGNALS.md §2.22b, covers every other alarm kind and
// a biller that is paused or stopped while customers depend on it.
//
// The gamma tables exist from the Gamma migration on. Below that migration
// head nothing can have been charged, so the signal reads nothing; at or past
// it the tables must exist.

// What the schema preflight reads: the installed migration head, and whether
// the tables the observation reads exist.
const gammaWatcherSchemaQuery = `
/* monitor-signal-2.22b-gamma-watcher-schema */
SELECT coalesce(max(end_version_number), 0)::bigint,
       (to_regclass('public.gamma_alarm') IS NOT NULL
        AND to_regclass('public.gamma_chain_state') IS NOT NULL
        AND to_regclass('public.gamma_payment_intent') IS NOT NULL
        AND to_regclass('public.gamma_charge') IS NOT NULL
        AND to_regclass('public.gamma_account') IS NOT NULL
        AND to_regclass('public.network') IS NOT NULL)
FROM migration_audit
WHERE status = 'success'
`

// The watcher alarm kinds this signal reads, each raising its own class
// (gammaWatcherFinding). credit_unfulfillable and batch_unfulfillable are
// §2.22a's.
var gammaWatcherAlarmKindSet = []string{
	controller.GammaAlarmKindUnknownReference,
	controller.GammaAlarmKindDuplicateReference,
	controller.GammaAlarmKindSpendMismatch,
	controller.GammaAlarmKindSpenderProxyChanged,
	controller.GammaAlarmKindUnknownBillingTx,
	controller.GammaAlarmKindUnexpectedNonce,
	controller.GammaAlarmKindRuntimeUnsupported,
	controller.GammaAlarmKindUndecodableBlock,
	controller.GammaAlarmKindServiceChanged,
	controller.GammaAlarmKindStrayTao,
}

// An alarm counts for this long after it was recorded. Aging out is no
// disposition.
const gammaWatcherAlarmWindow = 24 * time.Hour

// The biller writes its readiness every run (every 12 seconds, at most 10
// minutes apart while gamma.yml is missing): older than this, it is not
// running.
const gammaWatcherStalledAfter = 20 * time.Minute

// The probe's cadence, and the ticks a pause must last before it pages: a
// chain unreachable for a minute is not an incident.
const (
	gammaWatcherCadence      = 5 * time.Minute
	gammaWatcherPauseSustain = 3
)

// gammaWatcherQuery is the aggregate of the watcher's alarms of the last
// day, by kind, and of the biller's readiness while customers depend on it.
// Only kinds, counts, ages and the readiness word leave PostgreSQL.
var gammaWatcherQuery = `
/* monitor-signal-2.22b-gamma-watcher */
WITH alarm_kind AS (
    SELECT 'alarm_' || alarm.kind AS kind,
           count(*)::bigint AS issue_count,
           greatest(0, extract(epoch FROM now() - min(alarm.create_time)))::bigint AS oldest_age,
           greatest(0, extract(epoch FROM now() - max(alarm.create_time)))::bigint AS newest_age,
           '' AS reason
    FROM gamma_alarm alarm
    WHERE alarm.kind IN (` + gammaWatcherSqlList(gammaWatcherAlarmKinds()) + `)
      AND alarm.create_time >= now() - interval '` + fmt.Sprintf("%d", int64(gammaWatcherAlarmWindow/time.Second)) + ` seconds'
    GROUP BY alarm.kind
), dependents AS (
    -- customers depend on billing: an open intent, a charge in flight, or an
    -- account whose automatic charges are on (the biller makes no renewal
    -- while it is paused)
    SELECT (
        EXISTS (
            SELECT 1 FROM gamma_payment_intent intent
            WHERE intent.credited_at IS NULL
              AND intent.voided_at IS NULL
              AND now() < intent.expires_at
        )
        OR EXISTS (
            SELECT 1 FROM gamma_charge charge
            INNER JOIN gamma_payment_intent intent ON intent.intent_id = charge.intent_id
            WHERE charge.state IN ('pending', 'spent')
              AND intent.credited_at IS NULL
              AND intent.voided_at IS NULL
        )
        OR EXISTS (
            SELECT 1 FROM gamma_account account
            WHERE account.hotkey IS NOT NULL
              AND (account.auto_renew OR account.auto_topup)
              AND EXISTS (SELECT 1 FROM network WHERE network.network_id = account.network_id)
        )
    ) AS depend
), readiness AS (
    SELECT CASE
               WHEN state.update_time < now() - interval '` + fmt.Sprintf("%d", int64(gammaWatcherStalledAfter/time.Second)) + ` seconds' THEN 'biller_stalled'
               ELSE 'billing_paused'
           END AS kind,
           1::bigint AS issue_count,
           greatest(0, extract(epoch FROM now() - state.update_time))::bigint AS oldest_age,
           greatest(0, extract(epoch FROM now() - state.update_time))::bigint AS newest_age,
           state.readiness AS reason
    FROM gamma_chain_state state
    CROSS JOIN dependents
    WHERE state.singleton
      AND dependents.depend
      AND (
          state.update_time < now() - interval '` + fmt.Sprintf("%d", int64(gammaWatcherStalledAfter/time.Second)) + ` seconds'
          OR state.readiness <> '` + gammaPaymentFailuresSqlName(controller.GammaReadinessReady) + `'
      )
)
SELECT kind, 'gamma' AS target, issue_count, oldest_age, newest_age, reason
FROM alarm_kind
UNION ALL
SELECT kind, 'gamma' AS target, issue_count, oldest_age, newest_age, reason
FROM readiness
ORDER BY kind
`

// gammaWatcherAlarmKinds are the alarm kinds this signal reads, sorted.
func gammaWatcherAlarmKinds() []string {
	kinds := slices.Clone(gammaWatcherAlarmKindSet)
	slices.Sort(kinds)
	return kinds
}

// gammaWatcherSqlList renders source-owned names as a SQL list.
func gammaWatcherSqlList(names []string) string {
	quoted := []string{}
	for _, name := range names {
		quoted = append(quoted, "'"+gammaPaymentFailuresSqlName(name)+"'")
	}
	return strings.Join(quoted, ", ")
}

// SIGNALS.md §2.22b maps to signal_gamma_watcher.go and
// signal_gamma_watcher_test.go.
func NewGammaWatcherSignal() Signal {
	return &signalAdapter{
		number: "2.22b",
		key:    "gamma-watcher",
		name:   "Pay with Gamma watcher alarms and billing readiness",
		probe:  gammaWatcherProbe{},
	}
}

type gammaWatcherProbe struct{}

func (gammaWatcherProbe) id() string             { return "pg/gamma-watcher" }
func (gammaWatcherProbe) tier() string           { return tierPage }
func (gammaWatcherProbe) cadence() time.Duration { return gammaWatcherCadence }

func (gammaWatcherProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	gammaVersion := server.GammaPaySchemaMigrationVersion()
	if gammaVersion <= 0 {
		return nil, fmt.Errorf("gamma watcher: this build carries no gamma migration")
	}
	rows, err := env.runner.pg(ctx, gammaWatcherSchemaQuery)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("gamma watcher schema observation is incomplete")
	}
	head, err := parseStrictInt64(rows[0].str(0))
	if err != nil || head < 0 || (rows[0].str(1) != "t" && rows[0].str(1) != "f") {
		return nil, fmt.Errorf("gamma watcher schema observation is invalid")
	}
	if head < int64(gammaVersion) {
		// before the Gamma migration: nothing can have been charged
		return nil, nil
	}
	if rows[0].str(1) != "t" {
		return nil, fmt.Errorf("gamma watcher tables unavailable at the installed migration head")
	}

	rows, err = env.runner.pg(ctx, gammaWatcherQuery)
	if err != nil {
		return nil, err
	}
	findings := make([]finding, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) != 6 {
			return nil, fmt.Errorf("gamma watcher returned %d columns, want 6", len(row))
		}
		kind, target, reason := strings.TrimSpace(row.str(0)), strings.TrimSpace(row.str(1)), strings.TrimSpace(row.str(5))
		if seen[kind] {
			return nil, fmt.Errorf("gamma watcher repeated a group")
		}
		seen[kind] = true
		if target != "gamma" {
			return nil, fmt.Errorf("gamma watcher returned an unknown target")
		}
		count, err := parseStrictInt64(row.str(2))
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("gamma watcher returned an invalid count")
		}
		oldestAge, err := parseStrictInt64(row.str(3))
		if err != nil || oldestAge < 0 {
			return nil, fmt.Errorf("gamma watcher returned an invalid oldest age")
		}
		newestAge, err := parseStrictInt64(row.str(4))
		if err != nil || newestAge < 0 || newestAge > oldestAge {
			return nil, fmt.Errorf("gamma watcher returned an invalid newest age")
		}
		finding, err := gammaWatcherFinding(kind, count, oldestAge, newestAge, reason)
		if err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

// gammaWatcherAlarmContext is the context every alarm class shares.
func gammaWatcherAlarmContext(alarmKind string, key string) string {
	return fmt.Sprintf("The count is %s alarms recorded in the last 24 hours (gamma_alarm kind %s, key %s; its detail names what the watcher saw). The biller records each alarm once per key and keeps it; the count ages out after 24 hours, and aging out is no disposition.", alarmKind, alarmKind, key)
}

func gammaWatcherFinding(kind string, count, oldestAge, newestAge int64, reason string) (finding, error) {
	common := finding{
		probeId: "pg/gamma-watcher", tier: tierPage, target: "gamma", frame: "kind=" + kind, sustain: 1,
		baseline: "The watcher sees only our own spends, for references we issued, from batches we recorded, under a runtime we reviewed; the biller is ready whenever customers depend on it.",
		observed: fmt.Sprintf("kind=%s count=%d oldest_age_seconds=%d newest_age_seconds=%d", kind, count, oldestAge, newestAge),
		evidence: "Only aggregate kind counts and ages, and the biller's readiness word, are selected. References, hotkeys, coldkeys, networks, intents, blocks and alarm details never leave PostgreSQL.",
		playbook: "SIGNALS.md §2.22b",
	}
	alarmKind, isAlarm := strings.CutPrefix(kind, "alarm_")
	if isAlarm {
		if !slices.Contains(gammaWatcherAlarmKindSet, alarmKind) || reason != "" {
			return finding{}, fmt.Errorf("gamma watcher returned an unknown kind")
		}
	} else if !slices.Contains(controller.GammaReadinessReasons(), reason) ||
		(kind == "billing_paused" && reason == controller.GammaReadinessReady) {
		return finding{}, fmt.Errorf("gamma watcher returned an unknown readiness")
	}
	switch kind {
	case "alarm_" + controller.GammaAlarmKindUnknownReference:
		common.class = "gamma-unknown-reference"
		common.symptom = fmt.Sprintf("%d spend(s) from URnetwork's Gamma spender carried a reference we never issued, in 24 hours", count)
		common.mechanism = "The watcher read a finalized GammaSpent or GammaSpendSkipped from our spender whose 32-byte reference matches no charge we recorded (gamma alarm unknown_reference). Only our billing key, the spender's GammaSpend proxy, can make the spender spend, and the biller records every charge before it signs, so this is the first sign of a leaked billing key, or of a second process signing with it. A leaked billing key can burn customers' SN25 gamma up to each deposit's balance and daily cap; it cannot move TAO or alpha."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindUnknownReference, "reference/<block>/<event>")
		common.action = "Treat the billing key as compromised, at once. On the owner's machine, the spender removes the billing key's GammaSpend proxy (Proxy.remove_proxy, the reverse of BTCPAY §18.7 step 3; the spender seed is in vault/main/gamma_keys.yml and never goes on a server): removal takes effect immediately, and billing pauses (billing_proxy_missing). If two taskworkers may run the gamma_biller chain, stop all but one. Then make a new billing key in vault/main/gamma.yml, add it as the spender's GammaSpend proxy (BTCPAY §18.7 step 3) and deliver gamma.yml to the taskworker hosts; rotate the billing key, never the spender. From the alarm details, list the deposits that were spent and bring each customer to the owner for a disposition: gamma cannot be refunded on chain."
		common.verify = "The spender's proxies list only the new billing key, gamma terms read available again, and no new unknown_reference alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindDuplicateReference:
		common.class = "gamma-duplicate-reference"
		common.symptom = fmt.Sprintf("%d Gamma spend event(s) carried the reference of a charge that was already resolved, in 24 hours", count)
		common.mechanism = "The watcher read our spender's GammaSpent or GammaSpendSkipped with the reference of a charge that is no longer pending (gamma alarm duplicate_reference). The chain does not deduplicate references: one live batch at a time, the mortal era and the nonce check are the only protection against charging twice, so a second spend of a reference is a customer charged twice for one purchase."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindDuplicateReference, "duplicate/<block>/<event>")
		common.action = "Look the reference up in gamma_charge and gamma_spend_batch: which batches carried it, and at which blocks it was spent. A customer charged twice goes to the owner for a disposition (gamma cannot be refunded on chain: a credit, or a refund in another asset). Then find how a second extrinsic carried it (a batch sent again after it expired, two billers, or a leaked billing key: see gamma-unknown-reference), and stop the taskworker's gamma_biller chain until the cause is known."
		common.verify = "Every duplicate spend has a documented disposition, its cause is fixed, and no new duplicate_reference alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindSpendMismatch:
		common.class = "gamma-spend-mismatch"
		common.symptom = fmt.Sprintf("%d Gamma spend(s) did not match the charge whose reference they carried, in 24 hours", count)
		common.mechanism = "The watcher read our spender's spend whose reference is a pending charge of ours, but whose amount, deposit hotkey or netuid differ from the charge (gamma alarm spend_mismatch). The biller signs only what it recorded, so that extrinsic was not built from our records: a defect, or the billing key used outside the biller. The spend does not resolve the charge: if the charge's own batch lands without spending it, the charge fails and can be re-armed once the deposit changes, so the customer could be charged again."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindSpendMismatch, "mismatch/<block>/<event>")
		common.action = "Stop the taskworker's gamma_biller chain. From the alarm detail, compare what the chain spent with the charge. Keep the charge from being charged again: once its intent's disposition is decided, void it (gamma_payment_intent.voided_at), so it is never re-armed. Bring the customer to the owner for a disposition, and find which extrinsic made the spend (an unknown_billing_transaction alarm at the same block points at the billing key: see gamma-unknown-reference)."
		common.verify = "The mismatched spend is explained, the customer's charge has a documented disposition, and no new spend_mismatch alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindSpenderProxyChanged:
		common.class = "gamma-spender-proxy-changed"
		common.symptom = fmt.Sprintf("%d proxy change(s) of URnetwork's Gamma spender, in 24 hours", count)
		common.mechanism = "A Proxy.ProxyAdded or Proxy.ProxyRemoved names our spender as the delegator (gamma alarm spender_proxy_changed). Only the spender's own key can change its proxies, and its seed is kept in vault/main/gamma_keys.yml, never on a host: the change is either the owner's planned billing-key rotation or a compromised spender key. A proxy of the spender that can spend can burn every deposit that names our spender."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindSpenderProxyChanged, "proxy/<block>/<event>")
		common.action = "Ask the owner whether they rotated the billing key at that block (BTCPAY §18.6, §18.7 step 3). If they did not, treat the spender seed as compromised: with it, on the owner's machine, remove every proxy the alarm details name that is not our billing key, and bring it to the owner at once. Replacing the spender takes the ur-owner multisig's set_service naming a new one, and every customer pointing their deposit at it."
		common.verify = "The spender's proxies list exactly the billing key's GammaSpend proxy the owner set, every change has a documented reason, and no new spender_proxy_changed alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindUnknownBillingTx:
		common.class = "gamma-unknown-billing-transaction"
		common.symptom = fmt.Sprintf("%d transaction(s) paid by the Gamma billing key were none of our spend batches, in 24 hours", count)
		common.mechanism = "A finalized extrinsic whose fee our billing key paid is none of the spend batches the biller recorded (gamma alarm unknown_billing_transaction). The biller records each batch's exact bytes before it submits, and one process owns the billing key's nonce, so another signer used the billing key: a second biller, or a leaked billing key."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindUnknownBillingTx, "billing_tx/<block>/<extrinsic index>")
		common.action = "Check that exactly one taskworker runs the gamma_biller chain (RunOnce), and read the extrinsic at the block and index the alarm names. Unless it is a second biller of ours, treat the billing key as compromised (see gamma-unknown-reference): the spender removes its GammaSpend proxy at once, and the billing key is rotated."
		common.verify = "The extrinsic is explained, at most one biller runs, and no new unknown_billing_transaction alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindUnexpectedNonce:
		common.class = "gamma-unexpected-nonce"
		common.symptom = fmt.Sprintf("%d Gamma spend batch(es) were held for review: the billing key's nonce moved without them, in 24 hours", count)
		common.mechanism = "A spend batch's era ended without the watcher seeing it land, yet the billing key's nonce moved past it (gamma alarm unexpected_nonce): something else used that nonce. The biller held the batch for review and signs nothing while any batch is in review (readiness batch_in_review): no Gamma purchase, renewal, top-up or seat upgrade is charged until it is resolved."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindUnexpectedNonce, "nonce/<batch id>")
		common.action = "Find the extrinsic that used the nonce (an unknown_billing_transaction alarm may name it: see that class). Then establish from the chain whether the held batch itself landed, by its extrinsic hash in gamma_spend_batch. If it never did, with the owner's go, expire it as the biller does (gamma_spend_batch.state 'expired', its pending charges' batch_id cleared), so its charges go out again with the same references; if it did, its spends must be read and credited before anything else is signed. There is no operator tool for it yet (bringyourctl gamma, BTCPAY §18.9)."
		common.verify = "No batch is in review, readiness reads ready, the nonce's use is explained, and no new unexpected_nonce alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindRuntimeUnsupported:
		common.class = "gamma-runtime-unsupported"
		common.tier = tierWarn
		common.symptom = fmt.Sprintf("Subtensor runs %d runtime(s) whose Gamma shapes are not reviewed, in 24 hours", count)
		common.mechanism = "A block the watcher reads, or the finalized head, runs a Subtensor runtime whose metadata fails the reviewed shape check for the calls, events, storage and signed extensions the Gamma code uses (gamma alarm runtime_unsupported, once per spec version). The biller pauses with its cursor held on that block rather than guess (readiness runtime_unsupported): nothing is charged, and a spend that landed is not credited, until the code is reviewed against the new runtime."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindRuntimeUnsupported, "runtime/<spec version>")
		common.action = "Review the new runtime's metadata against controller/gamma_chain.go's shape check, extend the reviewed shapes (with the runtime's recorded metadata as a test fixture) and deploy the taskworker. The cursor resumes from the held block, and spends that landed meanwhile are credited as it reads them. While customers depend on billing, gamma-billing-paused pages for the same pause."
		common.verify = "Readiness reads ready and the watcher trails the finalized head by a few blocks again."
	case "alarm_" + controller.GammaAlarmKindUndecodableBlock:
		common.class = "gamma-undecodable-block"
		common.tier = tierWarn
		common.symptom = fmt.Sprintf("The Gamma watcher could not decode %d block(s), in 24 hours", count)
		common.mechanism = "The watcher could not decode a block's events with the runtime that executed it (gamma alarm undecodable_block). Its cursor holds on that block and the biller pauses (readiness runtime_unsupported): nothing is charged, and a spend that landed is not credited, until the block is read."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindUndecodableBlock, "undecodable/<block>")
		common.action = "Read that block's events with its parent's metadata and find what the decoder rejects; fix the decoder or the reviewed shape and deploy the taskworker, and the cursor resumes from that block. If the RPC endpoint answered bad data, point gamma.yml's rpc_urls at another archive node. While customers depend on billing, gamma-billing-paused pages for the same pause."
		common.verify = "Readiness reads ready and the watcher trails the finalized head by a few blocks again."
	case "alarm_" + controller.GammaAlarmKindServiceChanged:
		common.class = "gamma-service-changed"
		common.tier = tierWarn
		common.symptom = fmt.Sprintf("SN25's published Gamma service names %d services url(s) other than ours, in 24 hours", count)
		common.mechanism = "SN25's published service names our spender, but a services url other than gamma.yml's services_url (gamma alarm service_changed, once per url). Only the SN25 owner coldkey, the ur-owner multisig, can call set_service, so either the owners published it again or gamma.yml is stale. Billing continues; customers who read the service on chain see that url."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindServiceChanged, "service_url/<url>")
		common.action = "Read the published service (btcli query services --netuid 25 --json) and ask the owners whether they changed it. If the chain is right, set services_url in vault/main/gamma.yml and deliver it to the hosts that run the taskworker, api and alt; if not, the multisig publishes BTCPAY §18.7 step 5's set_service again."
		common.verify = "The published service's url equals gamma.yml's services_url, and no new service_changed alarm is recorded."
	case "alarm_" + controller.GammaAlarmKindStrayTao:
		common.class = "gamma-stray-tao"
		common.tier = tierWarn
		common.symptom = fmt.Sprintf("Plain TAO was sent to a Gamma deposit hotkey %d time(s), in 24 hours", count)
		common.mechanism = "A Balances.Transfer sent plain TAO to one of our deposit hotkeys instead of a gamma mint into it (gamma alarm stray_tao): a customer's mistake. It buys nothing and stays at the hotkey (gamma_deposit_hotkey.stray_tao_rao, shown in the intent's status); the deposit-hotkey master seed in vault/main/gamma_keys.yml, never on a host, can move it."
		common.context = gammaWatcherAlarmContext(controller.GammaAlarmKindStrayTao, "stray/<block>/<hotkey>")
		common.action = "From the alarm detail, find the customer of the hotkey's network through support, and point them at their intent's mint command. Return the TAO only when the customer asks for it, to the address in their request (the owner, 2026-11-11), signed with the hotkey's key derived from the master seed on the owner's machine."
		common.verify = "Each stray transfer is answered: returned on the customer's request, or recorded as not returned."
	case "billing_paused":
		common.class = "gamma-billing-paused"
		common.sustain = gammaWatcherPauseSustain
		common.symptom = fmt.Sprintf("Gamma billing is paused (%s) while customers depend on it", reason)
		common.mechanism = "The biller charges nothing unless it is ready: a reviewed runtime, SN25's published service naming our spender, the billing key as the spender's GammaSpend proxy with its fee float, a watcher within 100 blocks of the finalized head, and no batch in review. While it is not, no purchase, renewal, top-up or seat upgrade is charged, no new purchase starts, and the automatic charges are not made: a term that ends meanwhile is renewed only if billing resumes within its grace."
		common.context = "The readiness is gamma_chain_state's, which the biller writes each run; customers depend on billing when an intent is open, a charge is pending or spent and not credited, or an account that pays with gamma has an automatic charge on. " + gammaWatcherPausedContext(reason)
		common.action = gammaWatcherPausedAction(reason)
		common.verify = "Readiness reads ready (GET /pay/gamma/terms answers available: true), and the open intents are charged."
	case "biller_stalled":
		common.class = "gamma-biller-stalled"
		common.symptom = fmt.Sprintf("The Gamma biller has not run for %d seconds while customers depend on it", oldestAge)
		common.mechanism = "The biller writes its readiness each run, every 12 seconds (at most 10 minutes apart while gamma.yml is missing), and none was written for longer than 20 minutes: the taskworker's gamma_biller chain is not running. Nothing is charged or credited, and no automatic charge is made."
		common.context = fmt.Sprintf("gamma_chain_state.update_time is %d seconds old; its last readiness was %s. Customers depend on billing when an intent is open, a charge is pending or spent and not credited, or an account that pays with gamma has an automatic charge on.", oldestAge, reason)
		common.action = "Check that the taskworker hosts are up and that the gamma_biller task (RunOnce, 10-minute run limit) is scheduled and not failing: read the taskworker log for [gamma] lines and for a gamma biller error. Restart the taskworker if the chain is gone."
		common.verify = "gamma_chain_state.update_time advances every run again, and readiness reads ready."
	default:
		return finding{}, fmt.Errorf("gamma watcher returned an unknown kind")
	}
	return common, nil
}

// gammaWatcherPausedContext says what the readiness means.
func gammaWatcherPausedContext(reason string) string {
	switch reason {
	case "not_configured":
		return "not_configured: the process running the biller has no gamma.yml with billing_key_seed, or no RPC endpoint."
	case "chain_unreachable":
		return "chain_unreachable: the RPC endpoint (gamma.yml rpc_urls, else st.yml's subtensor gateway) did not answer a read the biller needs."
	case "runtime_unsupported":
		return "runtime_unsupported: the finalized head, or the block the watcher holds on, runs a runtime the Gamma code was not reviewed against (see gamma-runtime-unsupported and gamma-undecodable-block)."
	case "service_not_published":
		return "service_not_published: SN25 publishes no service for its owner hotkey."
	case "service_spender_mismatch":
		return "service_spender_mismatch: SN25's published service names another spender than ours, so new deposits do not name our spender."
	case "billing_proxy_missing":
		return "billing_proxy_missing: the spender has no GammaSpend proxy for the billing key with no delay."
	case "fee_float_low":
		return "fee_float_low: the billing key holds less TAO than min_float_rao, the fees of a few batches."
	case "watcher_behind":
		return "watcher_behind: the watcher trails the finalized head by more than 100 blocks."
	case "batch_in_review":
		return "batch_in_review: a spend batch is held for review (see gamma-unexpected-nonce)."
	}
	return reason + "."
}

// gammaWatcherPausedAction says what to do for the readiness.
func gammaWatcherPausedAction(reason string) string {
	switch reason {
	case "not_configured":
		return "Deliver vault/main/gamma.yml, with billing_key_seed, to the hosts that run the taskworker (single-file delivery), and check its rpc_urls or st.yml's gateway. The api and alt read a copy without the seed and never bill."
	case "chain_unreachable", "watcher_behind":
		return "Check the RPC endpoint the biller reads (gamma.yml rpc_urls, else st.yml's gateway, snow's archive node): that it answers and follows the finalized head. If it cannot be fixed soon, point rpc_urls at the public archive (https://archive.chain.opentensor.ai), which trusts a third party's view of finality, and deliver gamma.yml."
	case "runtime_unsupported":
		return "Review the new runtime against the Gamma code's shape check and deploy the taskworker (see gamma-runtime-unsupported)."
	case "service_not_published", "service_spender_mismatch":
		return "Read the published service (btcli query services --netuid 25 --json) and bring it to the owners: only the ur-owner multisig's set_service (BTCPAY §18.7 step 5) publishes it, naming our spender."
	case "billing_proxy_missing":
		return "Ask the owner whether the proxy was removed on purpose (a leaked billing key: see gamma-unknown-reference). If not, the spender adds the billing key as its GammaSpend proxy again (BTCPAY §18.7 step 3), on the owner's machine."
	case "fee_float_low":
		return "The treasury funds the billing key (BTCPAY §18.7 step 2): a float of 0.02 TAO pays about 97 one-entry batches."
	case "batch_in_review":
		return "Resolve the batch held for review (see gamma-unexpected-nonce); the biller signs again once no batch is in review."
	}
	return "Read the taskworker log for the biller's [gamma]paused line and fix its cause."
}
