package monitor

import (
	"context"
	"os"
	"strings"
	"testing"
)

func poolRetentionRows(values ...string) []Row {
	return []Row{values}
}

func TestPoolRetentionSignalSyntheticRetainedFleet(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"client_addr <<= inet '127.0.0.0/8'",
			"client_addr = inet '::1'",
			"interval '600 seconds'",
			"backend_type = 'client backend'",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("pool-retention query missing %q:\n%s", want, query)
			}
		}
		if strings.Contains(query, "client_addr::text") {
			t.Fatalf("pool-retention query exports an exact client address:\n%s", query)
		}
		return poolRetentionRows("1021", "714", "608", "600", "4", "4", "384", "1700"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pgbouncer-idle-retention")
	if alert.SignalNumber != "1.3b" || alert.SignalKey != "pool-retention" || alert.Sustain != 10 {
		t.Fatalf("pool-retention identity = %+v", alert)
	}
	for _, want := range []string{
		"600 idle loopback backends consume 58.8%",
		"loopback_idle_at_least_600s=384",
		"384 loopback idle backend(s) continuously idle beyond the 600-second drain interval",
		"32 live PgBouncer shards",
		"server_idle_timeout=0",
		"608 total",
		"Zero disables idle draining",
		"intentional local Xops checkout containing the 31ae1e7 idle-drain change",
		"run-dbs.sh --pgbouncer-only",
		"there is no separate run-pgbouncer.sh",
		"requires their PIDs to remain unchanged",
		"add database hardware",
		"Do not restart PostgreSQL/PgBouncer",
		"server_idle_timeout=600",
		"256-connection warm floor",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("pool-retention alert missing %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{"127.0.0.0/8", "::1"} {
		if markdown := alert.Markdown(); strings.Contains(markdown, forbidden) {
			t.Fatalf("pool-retention alert leaked address predicate %q: %s", forbidden, markdown)
		}
	}
}

func TestPoolRetentionSignalSyntheticYoungCohortPreservesDiscriminator(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "710", "610", "600", "5", "5", "0", "175"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pgbouncer-idle-retention")
	for _, want := range []string{
		"No loopback idle backend in this snapshot is yet continuously idle for 600 seconds",
		"young post-peak or recurring-demand cohort",
		"does not prove that idle draining is disabled",
		"loopback clients fell from 589 to 366",
		"NRestarts=0",
		"observe one complete timeout interval",
		"not an assumption",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("young-cohort alert missing %q: %s", want, alert.Markdown())
		}
	}
	for _, stale := range []string{
		"With idle draining disabled",
		"a peak can leave every shard near default_pool_size",
	} {
		if strings.Contains(alert.Markdown(), stale) {
			t.Fatalf("young-cohort alert retained stale diagnosis %q: %s", stale, alert.Markdown())
		}
	}
}

func TestPoolRetentionSignalSyntheticHealthyReserve(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "360", "270", "250", "12", "8", "0", "590"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy pool reserve produced alerts: %+v", alerts)
	}
}

func TestPoolRetentionSignalRejectsInconsistentSummary(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "300", "200", "190", "20", "1", "0", "10"), nil
	}}
	if _, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("inconsistent pool-retention error = %v", err)
	}
}

// Emitted guidance retains local-checkout policy and both independent reserve guards.
func TestPoolRetentionSignalGuidancePreservesLocalCheckoutAndCapacity(t *testing.T) {
	cases := []struct {
		name string
		rows []Row
	}{
		{
			name: "young-cohort-small-ceiling",
			rows: poolRetentionRows("200", "140", "120", "110", "8", "2", "0", "120"),
		},
		{
			name: "aged-cohort-large-ceiling",
			rows: poolRetentionRows("1000", "700", "600", "560", "35", "5", "400", "1700"),
		},
	}
	for _, c := range cases {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return c.rows, nil
		}}
		alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: pool-retention run failed: %v", c.name, err)
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: alert count = %d, want 1", c.name, len(alerts))
		}
		alert := alerts[0]
		if alert.Class != "pgbouncer-idle-retention" || alert.Severity != SeverityWarn || alert.Sustain != 10 {
			t.Errorf("%s: guidance change altered the retained threshold identity", c.name)
		}
		for _, want := range []string{
			"intentional local Xops checkout containing the 31ae1e7 idle-drain change",
			"deliberate local changes are allowed",
			"explicit operational authorization",
			"run-dbs.sh --pgbouncer-only",
			"requires their PIDs to remain unchanged",
		} {
			if !strings.Contains(alert.Action, want) {
				t.Errorf("%s: emitted action omitted %q", c.name, want)
			}
		}
		for _, forbidden := range []string{"clean Xops", "clean checkout", "clean-tree"} {
			if strings.Contains(alert.Action, forbidden) {
				t.Errorf("%s: emitted action retained unintended policy %q", c.name, forbidden)
			}
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{name: "baseline", value: alert.Baseline},
			{name: "verify", value: alert.Verify},
		} {
			if !strings.Contains(field.value, "both more than 25% normal-role headroom and more than 64 normal slots") {
				t.Errorf("%s: emitted %s does not require both independent reserve guards", c.name, field.name)
			}
		}
	}
}

// The owning catalog section uses the same checkout and reserve contract as Alerts.
func TestPoolRetentionCatalogGuidanceMatchesEmittedContract(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "### 1.3b PgBouncer idle-backend retention")
	if start < 0 {
		t.Fatal("pool-retention catalog section is missing")
	}
	section := catalog[start:]
	end := strings.Index(section, "\n### ")
	if end < 0 {
		t.Fatal("pool-retention catalog section end is missing")
	}
	section = strings.Join(strings.Fields(section[:end]), " ")
	for _, want := range []string{
		"intentional local Xops checkout containing the 31ae1e7 idle-drain change",
		"deliberate local changes are allowed",
		"explicit operational authorization",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("pool-retention catalog omitted %q", want)
		}
	}
	for _, band := range []struct {
		name  string
		start string
		end   string
	}{
		{name: "healthy", start: "- HEALTHY:", end: "- WARN:"},
		{name: "verify", start: "- VERIFY:", end: ""},
	} {
		bandStart := strings.Index(section, band.start)
		if bandStart < 0 {
			t.Fatalf("pool-retention catalog %s band is missing", band.name)
		}
		text := section[bandStart:]
		if band.end != "" {
			bandEnd := strings.Index(text, band.end)
			if bandEnd < 0 {
				t.Fatalf("pool-retention catalog %s band end is missing", band.name)
			}
			text = text[:bandEnd]
		}
		if !strings.Contains(text, "both more than 25% normal-role headroom and more than 64 normal slots") {
			t.Errorf("pool-retention catalog %s band omitted an independent reserve guard", band.name)
		}
	}
}
