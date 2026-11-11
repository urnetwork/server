package monitor

// Pay with Gamma payments no credit reaches (SIGNALS.md §2.22a). The biller's
// credit_unfulfillable and batch_unfulfillable alarms, and a credit held for
// review, reached the gamma dashboard only: nothing told an operator that a
// customer's gamma was spent and never credited.

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
	gammaPaymentFailuresTestSchemaMarker = "monitor-signal-2.22a-gamma-payment-failures-schema"
	gammaPaymentFailuresTestMarker       = "monitor-signal-2.22a-gamma-payment-failures"
)

// gammaPaymentFailuresTestSource answers the schema preflight with the
// migration head and table presence, and the observation with rows. It
// records which queries were issued.
func gammaPaymentFailuresTestSource(head int, tables bool, rows []Row, issued *[]string) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, gammaPaymentFailuresTestSchemaMarker):
			*issued = append(*issued, "schema")
			present := "f"
			if tables {
				present = "t"
			}
			return []Row{{strconv.Itoa(head), present}}, nil
		case strings.Contains(query, gammaPaymentFailuresTestMarker):
			*issued = append(*issued, "observation")
			return rows, nil
		}
		return nil, nil
	}}
}

// The signal is registered, so the monitor runs it.
func TestGammaPaymentFailuresRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "gamma-payment-failures" {
			found++
			if signal.Number() != "2.22a" || signal.Cadence() != 5*time.Minute {
				t.Fatalf("gamma payment failures registry metadata: number %s cadence %s", signal.Number(), signal.Cadence())
			}
		}
	}
	if found != 1 {
		t.Fatalf("gamma payment failures registered %d times, want once", found)
	}
}

// Below the Gamma migration nothing can have been spent: the signal reads
// nothing more and reports nothing. At the migration, the tables must exist.
func TestGammaPaymentFailuresFollowsTheGammaMigration(t *testing.T) {
	version := server.GammaPaySchemaMigrationVersion()
	if version <= 0 {
		t.Fatal("the gamma migration is not in this build")
	}
	issued := []string{}
	alerts, err := NewGammaPaymentFailuresSignal().Run(context.Background(), syntheticSettings(gammaPaymentFailuresTestSource(version-1, false, nil, &issued)))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || !slices.Equal(issued, []string{"schema"}) {
		t.Fatalf("before the gamma migration: alerts %+v, queries %v", alerts, issued)
	}

	issued = nil
	if _, err := NewGammaPaymentFailuresSignal().Run(context.Background(), syntheticSettings(gammaPaymentFailuresTestSource(version, false, nil, &issued))); err == nil {
		t.Fatal("missing gamma tables at the gamma migration were accepted")
	}
	if slices.Contains(issued, "observation") {
		t.Fatal("the observation named tables the preflight did not find")
	}

	issued = nil
	alerts, err = NewGammaPaymentFailuresSignal().Run(context.Background(), syntheticSettings(gammaPaymentFailuresTestSource(version, true, nil, &issued)))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || !slices.Equal(issued, []string{"schema", "observation"}) {
		t.Fatalf("healthy: alerts %+v, queries %v", alerts, issued)
	}
}

// A spend for an account that is gone and a credit held for review page; a
// live batch carrying an unfulfillable charge warns. Each says what to do, and
// no identifier enters an alert.
func TestGammaPaymentFailuresClassifiesEachProblemWithoutIdentifiers(t *testing.T) {
	issued := []string{}
	source := gammaPaymentFailuresTestSource(server.GammaPaySchemaMigrationVersion(), true, []Row{
		{"gamma_batch_unfulfillable", "gamma", "1", "900", "900"},
		{"gamma_credit_failed", "gamma", "2", "7200", "600"},
		{"gamma_credit_unfulfillable", "gamma", "1", "3600", "3600"},
	}, &issued)
	alerts, err := NewGammaPaymentFailuresSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]struct {
		severity Severity
		phrases  []string
	}{
		"gamma-credit-unfulfillable": {SeverityPage, []string{
			"no longer exists", "gamma cannot be refunded on chain", "credit_unfulfillable", "authorized disposition",
			"Never recreate the deleted network", "voiding the intent", "57f98311f",
		}},
		"gamma-credit-failed": {SeverityPage, []string{
			"10 times or more", "review (credit_failed)", "[gamma]credit of intent", "Do not credit by hand", "one-shot",
		}},
		"gamma-batch-unfulfillable": {SeverityWarn, []string{
			"stopped resubmitting", "batch_unfulfillable", "about 13 minutes", "gamma_spend_batch", "aging out is no disposition",
		}},
	} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Severity != want.severity {
			t.Errorf("%s severity = %s, want %s", class, alert.Severity, want.severity)
		}
		markdown := alert.Markdown()
		for _, phrase := range want.phrases {
			if !strings.Contains(markdown, phrase) {
				t.Errorf("%s alert missing %q:\n%s", class, phrase, markdown)
			}
		}
		requireAlertOmits(t, alert,
			"synthetic-network-id", "synthetic-intent-id", "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty",
			"0x5d0c6b9e3e3c5c0a5b8f2a0f1d4a3e9b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f",
		)
	}
	if len(alerts) != 3 {
		t.Fatalf("alerts = %d, want 3", len(alerts))
	}
	// the review threshold is the biller's
	if !strings.Contains(gammaPaymentFailuresQuery, strconv.Itoa(controller.GammaMaxCreditFailures)+" <= spent_uncredited.credit_failure_count") {
		t.Fatal("the review threshold in the query is not the biller's")
	}
	if !strings.Contains(gammaPaymentFailuresQuery, "alarm.kind = '"+controller.GammaAlarmKindBatchUnfulfillable+"'") {
		t.Fatal("the batch alarm kind in the query is not the biller's")
	}
}

func TestGammaPaymentFailuresRejectsUnknownOrMalformedAggregate(t *testing.T) {
	head := server.GammaPaySchemaMigrationVersion()
	for name, rows := range map[string][]Row{
		"unknown kind":    {{"gamma_synthetic", "gamma", "1", "10", "5"}},
		"unknown target":  {{"gamma_credit_failed", "stripe", "1", "10", "5"}},
		"repeated kind":   {{"gamma_credit_failed", "gamma", "1", "10", "5"}, {"gamma_credit_failed", "gamma", "1", "10", "5"}},
		"nonpositive":     {{"gamma_credit_failed", "gamma", "0", "10", "5"}},
		"inverted age":    {{"gamma_credit_unfulfillable", "gamma", "1", "5", "10"}},
		"negative age":    {{"gamma_credit_unfulfillable", "gamma", "1", "-1", "-1"}},
		"wrong columns":   {{"gamma_credit_unfulfillable", "gamma"}},
		"malformed count": {{"gamma_credit_unfulfillable", "gamma", "one", "10", "5"}},
	} {
		t.Run(name, func(t *testing.T) {
			issued := []string{}
			if _, err := NewGammaPaymentFailuresSignal().Run(context.Background(), syntheticSettings(gammaPaymentFailuresTestSource(head, true, rows, &issued))); err == nil {
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
				if strings.Contains(query, gammaPaymentFailuresTestSchemaMarker) {
					return preflight, nil
				}
				t.Fatal("the observation ran after an invalid preflight")
				return nil, nil
			}}
			if _, err := NewGammaPaymentFailuresSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
				t.Fatal("invalid preflight error = nil")
			}
		})
	}
}

// The actual SELECT on connection-local tables shaped as the Gamma migration
// makes them: a spend for a network that is gone, and a spend whose credit
// failed ten times, are counted; a credited, a voided (disposition recorded)
// or a recently failing one is not; a batch alarm counts for 24 hours.
func TestGammaPaymentFailuresSqlCountsSpendsNoCreditReaches(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(t.Context(), `
CREATE TEMP TABLE network (network_id uuid);
CREATE TEMP TABLE gamma_payment_intent (
    intent_id uuid, network_id uuid, credited_at timestamp, voided_at timestamp,
    credit_failure_count int NOT NULL DEFAULT 0
);
CREATE TEMP TABLE gamma_charge (
    reference bytea, intent_id uuid, state varchar(16), resolved_at timestamp
);
CREATE TEMP TABLE gamma_alarm (
    alarm_key varchar(160), kind varchar(48), create_time timestamp
);
`))
			spend := func(networkExists bool, credited bool, voided bool, failures int, state string, age time.Duration) {
				t.Helper()
				networkId := server.NewId()
				intentId := server.NewId()
				if networkExists {
					server.RaisePgResult(conn.Exec(t.Context(), `INSERT INTO network (network_id) VALUES ($1)`, networkId))
				}
				var creditedAt, voidedAt *time.Time
				now := server.NowUtc()
				if credited {
					creditedAt = &now
				}
				if voided {
					voidedAt = &now
				}
				server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO gamma_payment_intent (intent_id, network_id, credited_at, voided_at, credit_failure_count)
VALUES ($1, $2, $3, $4, $5)`, intentId, networkId, creditedAt, voidedAt, failures))
				server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO gamma_charge (reference, intent_id, state, resolved_at)
VALUES ($1, $2, $3, localtimestamp - $4::bigint * interval '1 second')`,
					intentId.Bytes(), intentId, state, int64(age/time.Second)))
			}
			alarm := func(kind string, age time.Duration) {
				t.Helper()
				server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO gamma_alarm (alarm_key, kind, create_time)
VALUES ($1, $2, localtimestamp - $3::bigint * interval '1 second')`,
					kind+"/"+server.NewId().String(), kind, int64(age/time.Second)))
			}
			// counted
			spend(false, false, false, 3, "spent", time.Hour)
			spend(false, false, false, 12, "spent", 2*time.Hour)
			spend(true, false, false, 10, "spent", 3*time.Hour)
			alarm("batch_unfulfillable", time.Hour)
			// not counted: credited, voided after a disposition, failing but not
			// yet held for review, not spent, an alarm past 24 hours, another kind
			spend(false, true, false, 0, "spent", time.Hour)
			spend(false, false, true, 11, "spent", time.Hour)
			spend(true, false, false, 9, "spent", time.Hour)
			spend(false, false, false, 0, "pending", time.Hour)
			spend(true, false, false, 0, "skipped", time.Hour)
			alarm("batch_unfulfillable", 25*time.Hour)
			alarm("unknown_reference", time.Hour)

			groups := map[string]int64{}
			ages := map[string][2]int64{}
			result, err := conn.Query(t.Context(), gammaPaymentFailuresQuery)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var kind, target string
					var count, oldestAge, newestAge int64
					server.Raise(result.Scan(&kind, &target, &count, &oldestAge, &newestAge))
					if _, err := gammaPaymentFailureFinding(kind, count, oldestAge, newestAge); err != nil || target != "gamma" {
						t.Fatalf("%s/%s did not classify: %v", kind, target, err)
					}
					groups[kind] = count
					ages[kind] = [2]int64{oldestAge, newestAge}
				}
			})
			want := map[string]int64{
				"gamma_batch_unfulfillable":  1,
				"gamma_credit_failed":        1,
				"gamma_credit_unfulfillable": 2,
			}
			if !maps.Equal(groups, want) {
				t.Fatalf("gamma payment failure groups = %v, want %v", groups, want)
			}
			// ages from the spend's resolution
			if age := ages["gamma_credit_unfulfillable"]; age[0] < 7100 || 7300 < age[0] || age[1] < 3500 || 3700 < age[1] {
				t.Fatalf("credit unfulfillable ages = %v, want about 7200 and 3600", age)
			}
		})
	})
}
