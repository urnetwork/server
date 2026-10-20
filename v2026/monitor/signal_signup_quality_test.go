package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestSignupQualitySignalSyntheticLowShare(t *testing.T) {
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
		"cannot distinguish automation from legitimate device-less creation",
		"Do not infer automation, change a limiter",
		"next two complete matured UTC cohorts",
		"retained only as a legacy stable identity token",
		"it is not causal attribution",
		"SIGNALS.md §2.26",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("signup quality alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, overclaim := range []string{
		"an automated wave of account creation raises",
		"source of the wave",
	} {
		if strings.Contains(alert.Markdown(), overclaim) {
			t.Fatalf("signup quality alert over-attributes %q:\n%s", overclaim, alert.Markdown())
		}
	}
	for _, forbidden := range []string{"network_id", "client_id"} {
		if strings.Contains(alert.Markdown(), forbidden+"=") {
			t.Fatalf("signup quality alert leaks an identifier: %s", alert.Markdown())
		}
	}
}

func TestSignupQualitySignalRetainsLegacyAlertLifecycleIdentity(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"2026-09-03", "200", "20"}}, nil
	}}
	signal := NewSignupQualitySignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "signup-quality")
	legacy := Alert{
		SignalID: alert.SignalID,
		Class:    alert.Class,
		Target:   alert.Target,
		Frame:    "automated-signup-wave",
		Sustain:  2,
	}
	if alert.Identity() != legacy.Identity() {
		t.Fatalf("source-neutral correction changed active alert identity: got %q, want %q", alert.Identity(), legacy.Identity())
	}

	// Model two consecutive failing cadences that straddle the wording
	// correction. Stable identity must complete the existing sustain streak;
	// a renamed frame would start a second lifecycle at one.
	gate := newCadenceAlertGate()
	if got := gate.filter(signal, Alerts{legacy}); len(got) != 0 {
		t.Fatalf("first legacy cadence returned %d alert(s), want 0", len(got))
	}
	alert.Sustain = 2
	got := gate.filter(signal, Alerts{alert})
	if len(got) != 1 || got[0].Identity() != legacy.Identity() {
		t.Fatalf("corrected cadence did not continue the legacy lifecycle: %+v", got)
	}

	markdown := alert.Markdown()
	for _, want := range []string{
		"retained only as a legacy stable identity token",
		"it is not causal attribution",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("signup quality lifecycle caveat missing %q:\n%s", want, markdown)
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
