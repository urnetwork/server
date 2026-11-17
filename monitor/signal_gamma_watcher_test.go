package monitor

// Pay with Gamma's watcher alarms and billing readiness (SIGNALS.md §2.22b).
// The biller's unknown_reference alarm, the first sign of a leaked billing
// key, and its other watcher alarms reached the gamma dashboard only, and so
// did a biller paused or stopped while customers depend on it.

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

const (
	gammaWatcherTestSchemaMarker = "monitor-signal-2.22b-gamma-watcher-schema"
	gammaWatcherTestMarker       = "monitor-signal-2.22b-gamma-watcher"
)

// gammaWatcherTestSource answers the schema preflight with the migration head
// and table presence, and the observation with rows. It records which
// queries were issued.
func gammaWatcherTestSource(head int, tables bool, rows []Row, issued *[]string) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, gammaWatcherTestSchemaMarker):
			*issued = append(*issued, "schema")
			present := "f"
			if tables {
				present = "t"
			}
			return []Row{{strconv.Itoa(head), present}}, nil
		case strings.Contains(query, gammaWatcherTestMarker):
			*issued = append(*issued, "observation")
			return rows, nil
		}
		return nil, nil
	}}
}

// The signal is registered, so the monitor runs it.
func TestGammaWatcherRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "gamma-watcher" {
			found++
			if signal.Number() != "2.22b" || signal.Cadence() != 5*time.Minute || signal.ID() != "pg/gamma-watcher" {
				t.Fatalf("gamma watcher registry metadata: number %s cadence %s id %s", signal.Number(), signal.Cadence(), signal.ID())
			}
		}
	}
	if found != 1 {
		t.Fatalf("gamma watcher registered %d times, want once", found)
	}
}

// Every alarm kind the biller records is read by §2.22a or §2.22b, and by
// only one of them: a new alarm kind needs an alert.
func TestGammaWatcherCoversEveryBillerAlarmKind(t *testing.T) {
	// §2.22a reads credit_unfulfillable through the spends themselves, and
	// batch_unfulfillable through its alarms
	paymentFailures := []string{controller.GammaAlarmKindCreditUnfulfillable, controller.GammaAlarmKindBatchUnfulfillable}
	for _, kind := range controller.GammaAlarmKinds() {
		watcher := slices.Contains(gammaWatcherAlarmKindSet, kind)
		failures := slices.Contains(paymentFailures, kind)
		if watcher == failures {
			t.Errorf("biller alarm kind %q is read by %d of §2.22a and §2.22b, want exactly one", kind, map[bool]int{true: 2, false: 0}[watcher])
		}
		if watcher && !strings.Contains(gammaWatcherQuery, "'"+kind+"'") {
			t.Errorf("the watcher query does not read alarm kind %q", kind)
		}
	}
	for _, kind := range gammaWatcherAlarmKindSet {
		if !slices.Contains(controller.GammaAlarmKinds(), kind) {
			t.Errorf("the watcher reads alarm kind %q the biller never records", kind)
		}
	}
	if !strings.Contains(gammaPaymentFailuresQuery, controller.GammaAlarmKindBatchUnfulfillable) {
		t.Fatal("§2.22a no longer reads batch_unfulfillable")
	}
}

// Below the Gamma migration nothing can have been charged: the signal reads
// nothing more and reports nothing. At the migration, the tables must exist.
func TestGammaWatcherFollowsTheGammaMigration(t *testing.T) {
	version := server.GammaPaySchemaMigrationVersion()
	if version <= 0 {
		t.Fatal("the gamma migration is not in this build")
	}
	issued := []string{}
	alerts, err := NewGammaWatcherSignal().Run(context.Background(), syntheticSettings(gammaWatcherTestSource(version-1, false, nil, &issued)))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || !slices.Equal(issued, []string{"schema"}) {
		t.Fatalf("before the gamma migration: alerts %+v, queries %v", alerts, issued)
	}

	issued = nil
	if _, err := NewGammaWatcherSignal().Run(context.Background(), syntheticSettings(gammaWatcherTestSource(version, false, nil, &issued))); err == nil {
		t.Fatal("missing gamma tables at the gamma migration were accepted")
	}
	if slices.Contains(issued, "observation") {
		t.Fatal("the observation named tables the preflight did not find")
	}

	issued = nil
	alerts, err = NewGammaWatcherSignal().Run(context.Background(), syntheticSettings(gammaWatcherTestSource(version, true, nil, &issued)))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || !slices.Equal(issued, []string{"schema", "observation"}) {
		t.Fatalf("healthy: alerts %+v, queries %v", alerts, issued)
	}
}

// Each alarm kind and the biller's pause and stall raise their class, with
// its severity, sustain and guidance, and no identifier enters an alert.
func TestGammaWatcherClassifiesEachAlarmAndPauseWithoutIdentifiers(t *testing.T) {
	rows := []Row{}
	for _, kind := range gammaWatcherAlarmKinds() {
		rows = append(rows, Row{"alarm_" + kind, "gamma", "2", "7200", "600", ""})
	}
	rows = append(rows,
		Row{"billing_paused", "gamma", "1", "12", "12", "fee_float_low"},
		Row{"biller_stalled", "gamma", "1", "1800", "1800", "ready"},
	)
	issued := []string{}
	alerts, err := NewGammaWatcherSignal().Run(context.Background(), syntheticSettings(gammaWatcherTestSource(server.GammaPaySchemaMigrationVersion(), true, rows, &issued)))
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]struct {
		severity Severity
		sustain  int
		phrases  []string
	}{
		"gamma-unknown-reference": {SeverityPage, 1, []string{
			"reference we never issued", "leaked billing key", "removes the billing key's GammaSpend proxy", "never goes on a server",
			"rotate the billing key, never the spender", "gamma cannot be refunded on chain", "aging out is no disposition",
		}},
		"gamma-duplicate-reference": {SeverityPage, 1, []string{
			"does not deduplicate references", "charged twice", "gamma_spend_batch", "disposition", "stop the taskworker's gamma_biller chain",
		}},
		"gamma-spend-mismatch": {SeverityPage, 1, []string{
			"amount, deposit hotkey or netuid", "re-armed", "voided_at", "Stop the taskworker's gamma_biller chain",
		}},
		"gamma-spender-proxy-changed": {SeverityPage, 1, []string{
			"Proxy.ProxyAdded or Proxy.ProxyRemoved", "planned billing-key rotation", "compromised spender key", "set_service",
		}},
		"gamma-unknown-billing-transaction": {SeverityPage, 1, []string{
			"none of the spend batches", "second biller", "leaked billing key", "exactly one taskworker",
		}},
		"gamma-unexpected-nonce": {SeverityPage, 1, []string{
			"nonce moved past it", "batch_in_review", "owner's go", "'expired'", "bringyourctl gamma",
		}},
		"gamma-runtime-unsupported": {SeverityWarn, 1, []string{
			"reviewed shape check", "cursor held", "controller/gamma_chain.go", "deploy the taskworker",
		}},
		"gamma-undecodable-block": {SeverityWarn, 1, []string{
			"could not decode", "cursor holds", "rpc_urls",
		}},
		"gamma-service-changed": {SeverityWarn, 1, []string{
			"services url", "ur-owner multisig", "btcli query services --netuid 25", "services_url",
		}},
		"gamma-stray-tao": {SeverityWarn, 1, []string{
			"plain TAO", "customer's mistake", "only when the customer asks", "address in their request",
		}},
		"gamma-billing-paused": {SeverityPage, gammaWatcherPauseSustain, []string{
			"paused (fee_float_low)", "customers depend on it", "fee_float_low", "treasury funds the billing key", "within its grace",
		}},
		"gamma-biller-stalled": {SeverityPage, 1, []string{
			"has not run for 1800 seconds", "gamma_biller", "20 minutes", "last readiness was ready",
		}},
	} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Severity != want.severity {
			t.Errorf("%s severity = %s, want %s", class, alert.Severity, want.severity)
		}
		if alert.Sustain != want.sustain {
			t.Errorf("%s sustain = %d, want %d", class, alert.Sustain, want.sustain)
		}
		markdown := alert.Markdown()
		for _, phrase := range want.phrases {
			if !strings.Contains(markdown, phrase) {
				t.Errorf("%s alert missing %q:\n%s", class, phrase, markdown)
			}
		}
		requireAlertOmits(t, alert,
			"synthetic-network-id", "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty",
			"0x5d0c6b9e3e3c5c0a5b8f2a0f1d4a3e9b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f",
		)
	}
	if len(alerts) != len(gammaWatcherAlarmKindSet)+2 {
		t.Fatalf("alerts = %d, want %d", len(alerts), len(gammaWatcherAlarmKindSet)+2)
	}
}

// A pause names the cause and the remedy for every readiness the biller can
// record.
func TestGammaWatcherPauseNamesTheRemedyForEveryReadiness(t *testing.T) {
	fallbackAction := gammaWatcherPausedAction("synthetic_reason")
	for _, reason := range controller.GammaReadinessReasons() {
		if reason == controller.GammaReadinessReady {
			if _, err := gammaWatcherFinding("billing_paused", 1, 12, 12, reason); err == nil {
				t.Fatal("a pause with readiness ready was accepted")
			}
			continue
		}
		finding, err := gammaWatcherFinding("billing_paused", 1, 12, 12, reason)
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if !strings.Contains(finding.context, reason+": ") {
			t.Errorf("%s: the context does not say what it means: %q", reason, finding.context)
		}
		if finding.action == fallbackAction {
			t.Errorf("%s: the action is the generic fallback", reason)
		}
	}
	if _, err := gammaWatcherFinding("billing_paused", 1, 12, 12, "synthetic_reason"); err == nil {
		t.Fatal("an unknown readiness was accepted")
	}
}

func TestGammaWatcherRejectsUnknownOrMalformedAggregate(t *testing.T) {
	head := server.GammaPaySchemaMigrationVersion()
	for name, rows := range map[string][]Row{
		"unknown kind":          {{"gamma_synthetic", "gamma", "1", "10", "5", ""}},
		"unknown alarm":         {{"alarm_synthetic", "gamma", "1", "10", "5", ""}},
		"payment failure alarm": {{"alarm_" + controller.GammaAlarmKindBatchUnfulfillable, "gamma", "1", "10", "5", ""}},
		"alarm with a reason":   {{"alarm_" + controller.GammaAlarmKindUnknownReference, "gamma", "1", "10", "5", "ready"}},
		"unknown readiness":     {{"billing_paused", "gamma", "1", "10", "10", "synthetic"}},
		"paused while ready":    {{"billing_paused", "gamma", "1", "10", "10", "ready"}},
		"unknown target":        {{"alarm_" + controller.GammaAlarmKindUnknownReference, "stripe", "1", "10", "5", ""}},
		"repeated kind":         {{"billing_paused", "gamma", "1", "10", "10", "fee_float_low"}, {"billing_paused", "gamma", "1", "10", "10", "fee_float_low"}},
		"nonpositive":           {{"alarm_" + controller.GammaAlarmKindStrayTao, "gamma", "0", "10", "5", ""}},
		"inverted age":          {{"alarm_" + controller.GammaAlarmKindStrayTao, "gamma", "1", "5", "10", ""}},
		"negative age":          {{"biller_stalled", "gamma", "1", "-1", "-1", "ready"}},
		"wrong columns":         {{"alarm_" + controller.GammaAlarmKindStrayTao, "gamma", "1", "10", "5"}},
		"malformed count":       {{"alarm_" + controller.GammaAlarmKindStrayTao, "gamma", "one", "10", "5", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			issued := []string{}
			if _, err := NewGammaWatcherSignal().Run(context.Background(), syntheticSettings(gammaWatcherTestSource(head, true, rows, &issued))); err == nil {
				t.Fatal("invalid aggregate error = nil")
			}
		})
	}
	for name, preflight := range map[string][]Row{
		"no row":       {},
		"wrong shape":  {{"1"}},
		"bad head":     {{"x", "t"}},
		"bad presence": {{strconv.Itoa(head), "yes"}},
	} {
		t.Run("preflight "+name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, gammaWatcherTestSchemaMarker) {
					return preflight, nil
				}
				t.Fatal("the observation ran after an invalid preflight")
				return nil, nil
			}}
			if _, err := NewGammaWatcherSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
				t.Fatal("invalid preflight error = nil")
			}
		})
	}
}

// The actual SELECT on connection-local tables shaped as the Gamma migration
// makes them: every watcher alarm of the last day is counted by kind, older
// ones and §2.22a's are not; the readiness raises nothing before anyone
// depends on billing, a pause once a customer does, and a stall once the
// biller stops writing it.
func TestGammaWatcherSqlCountsAlarmsAndPausesWhileCustomersDepend(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(t.Context(), `
CREATE TEMP TABLE network (network_id uuid);
CREATE TEMP TABLE gamma_payment_intent (
    intent_id uuid, network_id uuid, expires_at timestamp,
    credited_at timestamp, voided_at timestamp
);
CREATE TEMP TABLE gamma_charge (
    reference bytea, intent_id uuid, state varchar(16)
);
CREATE TEMP TABLE gamma_account (
    network_id uuid, hotkey varchar(64),
    auto_renew bool NOT NULL DEFAULT true, auto_topup bool NOT NULL DEFAULT true
);
CREATE TEMP TABLE gamma_alarm (
    alarm_key varchar(160), kind varchar(48), create_time timestamp
);
CREATE TEMP TABLE gamma_chain_state (
    singleton bool NOT NULL DEFAULT true, readiness varchar(48) NOT NULL,
    update_time timestamp NOT NULL
);
`))
			exec := func(sql string, args ...any) {
				t.Helper()
				server.RaisePgResult(conn.Exec(t.Context(), sql, args...))
			}
			alarm := func(kind string, age time.Duration) {
				t.Helper()
				exec(`INSERT INTO gamma_alarm (alarm_key, kind, create_time) VALUES ($1, $2, localtimestamp - $3::bigint * interval '1 second')`,
					kind+"/"+server.NewId().String(), kind, int64(age/time.Second))
			}
			state := func(readiness string, age time.Duration) {
				t.Helper()
				exec(`DELETE FROM gamma_chain_state`)
				exec(`INSERT INTO gamma_chain_state (readiness, update_time) VALUES ($1, localtimestamp - $2::bigint * interval '1 second')`,
					readiness, int64(age/time.Second))
			}
			observe := func() (map[string]int64, map[string]string) {
				t.Helper()
				groups := map[string]int64{}
				reasons := map[string]string{}
				result, err := conn.Query(t.Context(), gammaWatcherQuery)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var kind, target, reason string
						var count, oldestAge, newestAge int64
						server.Raise(result.Scan(&kind, &target, &count, &oldestAge, &newestAge, &reason))
						if _, err := gammaWatcherFinding(kind, count, oldestAge, newestAge, reason); err != nil || target != "gamma" {
							t.Fatalf("%s/%s/%s did not classify: %v", kind, target, reason, err)
						}
						groups[kind] = count
						reasons[kind] = reason
					}
				})
				return groups, reasons
			}

			// counted: every watcher kind in the last day, two unknown references
			for _, kind := range gammaWatcherAlarmKinds() {
				alarm(kind, time.Hour)
			}
			alarm(controller.GammaAlarmKindUnknownReference, 3*time.Hour)
			// not counted: past the day, and §2.22a's
			alarm(controller.GammaAlarmKindUnknownReference, 25*time.Hour)
			alarm(controller.GammaAlarmKindBatchUnfulfillable, time.Hour)
			alarm(controller.GammaAlarmKindCreditUnfulfillable, time.Hour)
			// paused, but nobody depends on billing yet
			state("service_not_published", 10*time.Second)

			want := map[string]int64{}
			for _, kind := range gammaWatcherAlarmKinds() {
				want["alarm_"+kind] = 1
			}
			want["alarm_"+controller.GammaAlarmKindUnknownReference] = 2
			groups, _ := observe()
			if !maps.Equal(groups, want) {
				t.Fatalf("gamma watcher groups before go-live = %v, want %v", groups, want)
			}

			exec(`DELETE FROM gamma_alarm`)
			// an expired purchase, a voided one, and an account with both
			// switches off depend on nothing
			networkId := server.NewId()
			exec(`INSERT INTO network (network_id) VALUES ($1)`, networkId)
			exec(`INSERT INTO gamma_payment_intent (intent_id, network_id, expires_at) VALUES ($1, $2, localtimestamp - interval '1 hour')`, server.NewId(), networkId)
			exec(`INSERT INTO gamma_payment_intent (intent_id, network_id, expires_at, voided_at) VALUES ($1, $2, localtimestamp + interval '1 hour', localtimestamp)`, server.NewId(), networkId)
			exec(`INSERT INTO gamma_account (network_id, hotkey, auto_renew, auto_topup) VALUES ($1, 'synthetic-hotkey', false, false)`, networkId)
			if groups, _ := observe(); len(groups) != 0 {
				t.Fatalf("gamma watcher groups with nobody depending on billing = %v, want none", groups)
			}

			// an account that pays with gamma, renewal on: a pause counts
			otherNetworkId := server.NewId()
			exec(`INSERT INTO network (network_id) VALUES ($1)`, otherNetworkId)
			exec(`INSERT INTO gamma_account (network_id, hotkey, auto_renew, auto_topup) VALUES ($1, 'synthetic-hotkey-2', true, false)`, otherNetworkId)
			groups, reasons := observe()
			if !maps.Equal(groups, map[string]int64{"billing_paused": 1}) || reasons["billing_paused"] != "service_not_published" {
				t.Fatalf("gamma watcher while a customer depends on billing = %v %v, want billing_paused service_not_published", groups, reasons)
			}
			// ready: nothing
			state("ready", 10*time.Second)
			if groups, _ := observe(); len(groups) != 0 {
				t.Fatalf("gamma watcher while ready = %v, want none", groups)
			}
			// the biller stopped writing its state: stalled, whatever it said
			state("ready", 30*time.Minute)
			groups, reasons = observe()
			if !maps.Equal(groups, map[string]int64{"biller_stalled": 1}) || reasons["biller_stalled"] != "ready" {
				t.Fatalf("gamma watcher with a stale state = %v %v, want biller_stalled", groups, reasons)
			}

			// a deleted account depends on nothing; an open intent or a charge
			// in flight does
			exec(`DELETE FROM network WHERE network_id = $1`, otherNetworkId)
			state("chain_unreachable", 10*time.Second)
			if groups, _ := observe(); len(groups) != 0 {
				t.Fatalf("gamma watcher with only a deleted account = %v, want none", groups)
			}
			openId := server.NewId()
			exec(`INSERT INTO gamma_payment_intent (intent_id, network_id, expires_at) VALUES ($1, $2, localtimestamp + interval '1 hour')`, openId, networkId)
			if groups, _ := observe(); !maps.Equal(groups, map[string]int64{"billing_paused": 1}) {
				t.Fatalf("gamma watcher with an open intent = %v, want billing_paused", groups)
			}
			exec(`UPDATE gamma_payment_intent SET expires_at = localtimestamp - interval '1 minute' WHERE intent_id = $1`, openId)
			exec(`INSERT INTO gamma_charge (reference, intent_id, state) VALUES ($1, $2, 'spent')`, openId.Bytes(), openId)
			if groups, _ := observe(); !maps.Equal(groups, map[string]int64{"billing_paused": 1}) {
				t.Fatalf("gamma watcher with a spend not yet credited = %v, want billing_paused", groups)
			}
		})
	})
}
