package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestSignupQualitySignalSyntheticWave(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"statement_timestamp() AT TIME ZONE 'UTC'",
			"(utc_today - 3)::timestamp without time zone AS start_utc",
			"(utc_today - 2)::timestamp without time zone AS end_utc",
			"to_char(cohort_day, 'YYYY-MM-DD')",
			"WHERE EXISTS (",
			"nc.create_time < cohort.create_time + interval '48 hours'",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("signup quality query missing %q", want)
			}
		}
		for _, forbidden := range []string{"min(", "MIN(", "JOIN network_client"} {
			if strings.Contains(query, forbidden) {
				t.Fatalf("signup quality query must probe per network, found %q", forbidden)
			}
		}
		// the Main shape of 2026-09-03: 14,521 networks, 1,961 with a device
		return []Row{{"2026-09-03", "14521", "1961"}}, nil
	}}
	alerts, err := NewSignupQualitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "signup-quality")
	if alert.Frame != "automated-signup-wave" || alert.Sustain != 1 {
		t.Fatalf("wrong signup quality framing: %+v", alert)
	}
	for _, want := range []string{
		"13.5% of the 14521 networks created on 2026-09-03",
		"device_networks_48h=1961",
		"device_share=0.135",
		"e4c34a21",
		"do not size or judge an onboarding experiment on the raw sign-up count",
		"SIGNALS.md §2.26",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("signup quality alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, forbidden := range []string{"network_id", "client_id"} {
		if strings.Contains(alert.Markdown(), forbidden+"=") {
			t.Fatalf("signup quality alert leaks an identifier: %s", alert.Markdown())
		}
	}
}

func TestSignupQualityQueryUsesUTCBoundsIndependentOfSessionTimezone(t *testing.T) {
	for _, want := range []string{
		"WITH clock AS MATERIALIZED",
		"statement_timestamp() AT TIME ZONE 'UTC'",
		"n.create_time >= b.start_utc",
		"n.create_time < b.end_utc",
		"to_char(cohort_day, 'YYYY-MM-DD')",
	} {
		if !strings.Contains(signupQualityQuery, want) {
			t.Fatalf("signup quality query missing session-timezone-independent boundary %q:\n%s", want, signupQualityQuery)
		}
	}
	for _, forbidden := range []string{
		"date_trunc('day', now())",
		"CURRENT_DATE",
		"LOCALTIMESTAMP",
	} {
		if strings.Contains(signupQualityQuery, forbidden) {
			t.Fatalf("signup quality query retains session-timezone-dependent expression %q:\n%s", forbidden, signupQualityQuery)
		}
	}
}

func TestSignupQualitySignalSyntheticHealthyDay(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		// the quiet week of August 2026: 265 networks, 234 with a device
		return []Row{{"2026-08-24", "265", "234"}}, nil
	}}
	alerts, err := NewSignupQualitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy day alerted: %+v", alerts)
	}
}

func TestSignupQualitySignalSyntheticLowVolumeCannotDecide(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		// 40 networks, 2 with a device: too few to call a wave
		return []Row{{"2026-08-24", "40", "2"}}, nil
	}}
	alerts, err := NewSignupQualitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("low-volume day alerted: %+v", alerts)
	}
}

func TestSignupQualitySignalSyntheticExactFloorIsHealthy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"2026-08-24", "200", "100"}}, nil
	}}
	alerts, err := NewSignupQualitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("a share at the floor alerted: %+v", alerts)
	}
}

func TestSignupQualitySignalSyntheticMalformedRows(t *testing.T) {
	for name, rows := range map[string][]Row{
		"no rows":          {},
		"short row":        {{"2026-08-24", "200"}},
		"extra column":     {{"2026-08-24", "200", "100", "unexpected"}},
		"two rows":         {{"2026-08-24", "200", "100"}, {"2026-08-25", "200", "100"}},
		"invalid day":      {{"2026-02-30", "200", "100"}},
		"timestamp as day": {{"2026-08-24 00:00:00", "200", "100"}},
		"nonnumeric count": {{"2026-08-24", "not-a-number", "0"}},
		"decimal count":    {{"2026-08-24", "200.0", "100"}},
		"overflow count":   {{"2026-08-24", "9223372036854775808", "0"}},
		"negative count":   {{"2026-08-24", "-1", "0"}},
		"contradictory":    {{"2026-08-24", "200", "300"}},
	} {
		rows := rows
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		// a malformed or contradictory result is an observation failure the
		// runner reports, never a partial reading
		if _, err := NewSignupQualitySignal().Run(context.Background(), syntheticSettings(source)); err == nil {
			t.Fatalf("%s: malformed rows must fail the probe", name)
		}
	}
}
