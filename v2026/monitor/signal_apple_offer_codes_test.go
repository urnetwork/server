package monitor

// SIGNALS.md §2.22d: the App Store offer code supply.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The onboarding config as on Main: offers valid five days, the top-up
// configured with seven-day batches of 500 and a floor of 200.
func appleOfferCodesMainConfig() appleOfferCodesConfig {
	return appleOfferCodesConfig{
		offerEnabled:  true,
		offerValidity: 5 * 24 * time.Hour,
		topUp:         true,
		batchSize:     500,
		minAvailable:  200,
		batchExpiry:   7 * 24 * time.Hour,
		topUpInterval: 24 * time.Hour,
	}
}

// The registered signal's probe at the given config.
func newAppleOfferCodesTestSignal(config appleOfferCodesConfig) Signal {
	return &signalAdapter{
		number: "2.22d",
		key:    "apple-offer-codes",
		name:   "App Store offer code supply",
		probe: appleOfferCodesProbe{
			settings:   DefaultAppleOfferCodesSettings(),
			loadConfig: func() appleOfferCodesConfig { return config },
		},
	}
}

// Runs the signal over one aggregate row: assignable, next top-up
// assignable, horizon seconds, fresh batch, drawn in the window, drawn in the
// lookback. Returns the alerts and how many observations were issued.
func runAppleOfferCodes(t testing.TB, config appleOfferCodesConfig, row Row) ([]Alert, int) {
	observations := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "monitor-signal-2.22d-apple-offer-codes") {
			t.Fatalf("unexpected query: %s", query)
		}
		observations++
		return []Row{row}, nil
	}}
	alerts, err := newAppleOfferCodesTestSignal(config).Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts, observations
}

const appleOfferCodesDay = int64(24 * 3600)

// 2026-11-12 on Main: every offer drew a code, 509 a day, against about
// 1,511 left, with no batch arriving. The supply is under the floor whether
// the pool's expiry or its count runs out first, and so it is with a
// delivering top-up whose batch of 500 a day is under the draws.
func TestAppleOfferCodesWarnsAtTheNovember12Pool(t *testing.T) {
	alerts, _ := runAppleOfferCodes(t, appleOfferCodesMainConfig(), Row{"1511", "0", fmt.Sprint(2 * appleOfferCodesDay), "f", "1527", "9000"})
	alert := requireAlertClass(t, alerts, "apple-offer-code-supply-low")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatalf("supply alert = %s sustain %d, want warn sustain 2", alert.Severity, alert.Sustain)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"about 2.0 days of welcome offers",
		"1511 codes left that a new offer can take",
		"509 a day",
		"configured, but no batch arrived in two intervals",
		"top_up_live=false",
		"SIGNALS.md §2.22d",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("supply alert lacks %q:\n%s", want, markdown)
		}
	}

	alerts, _ = runAppleOfferCodes(t, appleOfferCodesMainConfig(), Row{"1511", "700", fmt.Sprint(2 * appleOfferCodesDay), "t", "1527", "9000"})
	alert = requireAlertClass(t, alerts, "apple-offer-code-supply-low")
	for _, want := range []string{"about 3.0 days of welcome offers", "delivering, up to 500 codes a day", "top_up_live=true"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("supply alert with a delivering top-up lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// After the fix only App Store readers draw, about 58 a day on 2026-11-12: a
// delivering top-up keeps up, however few codes are left this hour.
func TestAppleOfferCodesQuietWhileALiveTopUpKeepsUp(t *testing.T) {
	for _, row := range []Row{
		// a batch arrived today
		{"900", "500", fmt.Sprint(2 * appleOfferCodesDay), "t", "174", "5000"},
		// no batch was due: the pool is above the floor the top-up refills at
		{"250", "220", fmt.Sprint(appleOfferCodesDay), "f", "174", "5000"},
	} {
		alerts, observations := runAppleOfferCodes(t, appleOfferCodesMainConfig(), row)
		if observations != 1 || requireAlertClassCount(alerts, "apple-offer-code-supply-low") != 0 {
			t.Errorf("%v: alerts %+v after %d observations, want none", row, alerts, observations)
		}
	}
}

// A top-up that stopped delivering leaves a pool whose batches stop serving
// new offers within days: a count that would last weeks warns while its
// last code stays assignable less than the floor.
func TestAppleOfferCodesWarnsWhenTheTopUpStalls(t *testing.T) {
	alerts, _ := runAppleOfferCodes(t, appleOfferCodesMainConfig(), Row{"900", "150", fmt.Sprint(3 * appleOfferCodesDay / 2), "f", "174", "5000"})
	alert := requireAlertClass(t, alerts, "apple-offer-code-supply-low")
	for _, want := range []string{"about 1.5 days of welcome offers", "supply_days=1.50", "configured, but no batch arrived"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("stalled top-up alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Without a top-up the pool is all there is: it warns once its codes over
// the draw rate, or the time its last code stays assignable, fall under the
// floor.
func TestAppleOfferCodesWithoutATopUpCountsThePool(t *testing.T) {
	config := appleOfferCodesMainConfig()
	config.topUp = false
	alerts, _ := runAppleOfferCodes(t, config, Row{"2000", "2000", fmt.Sprint(60 * appleOfferCodesDay), "f", "174", "5000"})
	if requireAlertClassCount(alerts, "apple-offer-code-supply-low") != 0 {
		t.Fatalf("34 days of codes warned: %+v", alerts)
	}
	alerts, _ = runAppleOfferCodes(t, config, Row{"300", "300", fmt.Sprint(60 * appleOfferCodesDay), "f", "174", "5000"})
	alert := requireAlertClass(t, alerts, "apple-offer-code-supply-low")
	for _, want := range []string{"about 5.2 days", "not configured"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("pool-only alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// An empty pool warns while it is in use, though it stops the draws that
// measure the demand; one not drawn from for the lookback does not, and with
// the welcome offer off the pool is not read at all.
func TestAppleOfferCodesEmptyPoolWarnsOnlyWhileInUse(t *testing.T) {
	alerts, _ := runAppleOfferCodes(t, appleOfferCodesMainConfig(), Row{"0", "0", "-1", "f", "0", "40"})
	alert := requireAlertClass(t, alerts, "apple-offer-code-supply-low")
	if !strings.Contains(alert.Markdown(), "No App Store offer code is left that a new welcome offer can take, while 40 were drawn in the last 30 days") {
		t.Errorf("empty pool alert:\n%s", alert.Markdown())
	}

	alerts, _ = runAppleOfferCodes(t, appleOfferCodesMainConfig(), Row{"0", "0", "-1", "f", "0", "0"})
	if requireAlertClassCount(alerts, "apple-offer-code-supply-low") != 0 {
		t.Errorf("a pool not in use warned: %+v", alerts)
	}

	config := appleOfferCodesMainConfig()
	config.offerEnabled = false
	alerts, observations := runAppleOfferCodes(t, config, Row{"0", "0", "-1", "f", "0", "40"})
	if observations != 0 || requireAlertClassCount(alerts, "apple-offer-code-supply-low") != 0 {
		t.Errorf("with the offer off: %d observations, alerts %+v", observations, alerts)
	}
}

func TestAppleOfferCodesRejectsMalformedAggregates(t *testing.T) {
	for name, rows := range map[string][]Row{
		"no row":                   {},
		"two rows":                 {{"1", "1", "60", "t", "0", "0"}, {"1", "1", "60", "t", "0", "0"}},
		"wrong columns":            {{"1", "1", "60", "t", "0"}},
		"non-numeric count":        {{"many", "1", "60", "t", "0", "0"}},
		"invalid flag":             {{"1", "1", "60", "yes", "0", "0"}},
		"negative count":           {{"-1", "0", "-1", "f", "0", "0"}},
		"next above assignable":    {{"1", "2", "60", "t", "0", "0"}},
		"window above lookback":    {{"1", "1", "60", "t", "5", "4"}},
		"horizon without codes":    {{"0", "0", "60", "f", "0", "0"}},
		"codes without a horizon":  {{"3", "0", "-1", "f", "0", "0"}},
		"horizon below the absent": {{"0", "0", "-2", "f", "0", "0"}},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		alerts, err := newAppleOfferCodesTestSignal(appleOfferCodesMainConfig()).Run(context.Background(), syntheticSettings(source))
		if requireAlertClassCount(alerts, "apple-offer-code-supply-low") != 0 {
			t.Errorf("%s: a malformed aggregate alerted as low", name)
		}
		if err == nil {
			t.Errorf("%s: a malformed aggregate was not an observation failure", name)
		}
	}
}

// The observation reads the pool at the config's offer validity, top-up
// interval and batch expiry and at the probe's windows, in UTC as the pool's
// timestamps are stored.
func TestAppleOfferCodesQueryContract(t *testing.T) {
	query := appleOfferCodesQueryWith(DefaultAppleOfferCodesSettings(), appleOfferCodesMainConfig())
	for _, required := range []string{
		"/* monitor-signal-2.22d-apple-offer-codes */",
		"(statement_timestamp() AT TIME ZONE 'UTC') AS now_utc",
		"code.network_id IS NULL AND code.expires_at > clock.now_utc + interval '432000 seconds'",
		"code.network_id IS NULL AND code.expires_at > clock.now_utc + interval '518400 seconds'",
		"bool_or(code.expires_at > clock.now_utc + interval '432000 seconds')",
		"code.assigned_at > clock.now_utc + interval '-259200 seconds'",
		"code.assigned_at > clock.now_utc + interval '-2592000 seconds'",
		"LEFT JOIN network_onboarding_apple_offer_code code ON true",
	} {
		if !strings.Contains(query, required) {
			t.Errorf("apple offer codes query lacks %q:\n%s", required, query)
		}
	}
}

// The real query on the migrated schema, the pool's rows as the server
// writes them: without a top-up, three codes left whose last stops serving
// new offers in a day and a half, two drawn in the window and one before it.
func TestAppleOfferCodesOnTheMigratedSchema(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		exec := func(sql string, args ...any) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql, args...))
			})
		}
		for i := 0; i < 3; i++ {
			exec(`INSERT INTO network_onboarding_apple_offer_code (code, expires_at) VALUES ($1, $2)`,
				fmt.Sprintf("SYNTHETIC-SUPPLY-%d", i), now.Add(6*24*time.Hour+12*time.Hour))
		}
		for i, assignedAt := range []time.Time{now.Add(-2 * time.Hour), now.Add(-30 * time.Hour), now.Add(-10 * 24 * time.Hour)} {
			exec(`INSERT INTO network_onboarding_apple_offer_code (code, expires_at, network_id, assigned_at) VALUES ($1, $2, $3, $4)`,
				fmt.Sprintf("SYNTHETIC-DRAWN-%d", i), now.Add(2*24*time.Hour), server.NewId(), assignedAt)
		}
		// a code that expired unused
		exec(`INSERT INTO network_onboarding_apple_offer_code (code, expires_at) VALUES ('SYNTHETIC-EXPIRED', $1)`, now.Add(-24*time.Hour))

		config := appleOfferCodesMainConfig()
		config.topUp = false
		source := paymentNotificationTestDbSource(t, ctx)
		alerts, err := newAppleOfferCodesTestSignal(config).Run(ctx, syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "apple-offer-code-supply-low")
		for _, want := range []string{"assignable=3 next_top_up_assignable=3", "fresh_batch=true", "drawn_window=2 ", "drawn_lookback=3 ", "supply_days=1.50"} {
			if !strings.Contains(alert.Observed, want) {
				t.Errorf("observed values lack %q: %s", want, alert.Observed)
			}
		}
		requireAlertOmits(t, alert, "SYNTHETIC-SUPPLY-0", "SYNTHETIC-DRAWN-0")

		// a delivering top-up keeps up with two draws in three days
		alerts, err = newAppleOfferCodesTestSignal(appleOfferCodesMainConfig()).Run(ctx, syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if requireAlertClassCount(alerts, "apple-offer-code-supply-low") != 0 {
			t.Errorf("a delivering top-up warned: %+v", alerts)
		}
	})
}
