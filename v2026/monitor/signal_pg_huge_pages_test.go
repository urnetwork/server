// Root-cause controls for the absent §8.14b reader use the public signal and
// its actual PostgreSQL source seam. No fixture needs a database or host.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Every cell originates in one SQL row in the same bounded psql/SSH invocation.
func pgHugePagesTestRows(status string) []Row {
	return []Row{{"pg-1", fmt.Sprint(syntheticSettings(nil).Now().Unix()), "primary", status}}
}

// A concrete supported off value was previously invisible to every registry
// entry, while try/configured pool size could appear healthy elsewhere.
func TestPgHugePagesPublicSignalOnOff(t *testing.T) {
	for _, status := range []string{"on", "off"} {
		calls := 0
		settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
			calls++
			if query != pgHugePagesQuery || !strings.Contains(query, "\\set monitor_primary_hostname `hostname -s`") || !strings.Contains(query, "SELECT :'monitor_primary_hostname',") || !strings.Contains(query, "pg_is_in_recovery()") || !strings.Contains(query, "current_setting('huge_pages_status', true)") {
				t.Fatal("primary predicate lost its same-source identity or live setting")
			}
			return pgHugePagesTestRows(status), nil
		}})
		alerts, err := NewPgHugePagesSignal().Run(context.Background(), settings)
		if err != nil || calls != 1 {
			t.Fatal("primary observation did not complete once")
		}
		if status == "on" {
			if len(alerts) != 0 {
				t.Fatal("supported on control alerted")
			}
		} else {
			alert := requireAlertClass(t, alerts, "pg-huge-pages-disabled")
			if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Target != "pg-1" {
				t.Fatal("off predicate changed its proposed policy")
			}
			for _, want := range []string{"huge_pages_status=off", "False-positive qualifier", "False-negative qualifier", "separate operator authorization"} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatal("huge-page finding lost qualification")
				}
			}
		}
	}
}

// Wrong host/role, unsupported setting, partial rows and stale clocks must
// remain unknown, including after a previously observed off value.
func TestPgHugePagesUnknownControls(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func([]Row) []Row
	}{
		{name: "missing", mutate: func([]Row) []Row { return nil }},
		{name: "host missing", mutate: func(rows []Row) []Row { return rows[1:] }},
		{name: "host mismatch", mutate: func(rows []Row) []Row { rows[0][0] = "private-wrong-host"; return rows }},
		{name: "replica", mutate: func(rows []Row) []Row { rows[0][2] = "replica"; return rows }},
		{name: "unknown setting", mutate: func(rows []Row) []Row { rows[0][3] = "unavailable"; return rows }},
		{name: "malformed setting", mutate: func(rows []Row) []Row { rows[0][3] = "private-token-kid-key"; return rows }},
		{name: "missing setting", mutate: func(rows []Row) []Row { rows[0] = rows[0][:3]; return rows }},
		{name: "nonfinite clock", mutate: func(rows []Row) []Row { rows[0][1] = "NaN"; return rows }},
		{name: "stale", mutate: func(rows []Row) []Row {
			rows[0][1] = fmt.Sprint(syntheticSettings(nil).Now().Add(-31 * time.Second).Unix())
			return rows
		}},
		{name: "future", mutate: func(rows []Row) []Row {
			rows[0][1] = fmt.Sprint(syntheticSettings(nil).Now().Add(31 * time.Second).Unix())
			return rows
		}},
		{name: "duplicate", mutate: func(rows []Row) []Row { return append(rows, rows[0]) }},
	} {
		t.Logf("case: %s", change.name)
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return change.mutate(pgHugePagesTestRows("off")), nil }})
		alerts, err := NewPgHugePagesSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: unknown setting lost its single visibility finding", change.name)
		}
		alert := requireAlertClass(t, alerts, "pg-huge-pages-unobservable")
		if !alert.unobservable {
			t.Fatalf("%s: unknown primary source could clear a prior incident", change.name)
		}
		requireAlertOmits(t, alert, "private-wrong-host", "private-token-kid-key")
	}
}

// Inventory failure prevents source contact and cannot select an arbitrary
// primary; an excluded owner is never reached through the PostgreSQL seam.
func TestPgHugePagesInventoryAndExclusion(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "excluded"} {
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { t.Fatal("invalid primary inventory contacted a source"); return nil, nil }})
		switch mode {
		case "missing":
			settings.Hosts = nil
		case "ambiguous":
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		case "excluded":
			settings.ExcludedHosts = []string{"pg-1"}
		}
		alerts, err := NewPgHugePagesSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("inventory control returned execution error")
		}
		found := false
		for _, alert := range alerts {
			found = found || alert.unobservable
		}
		if !found {
			t.Fatal("invalid primary inventory certified health")
		}
	}
}

// Cancellation is lifecycle, never setting-off or a successful unknown sample.
func TestPgHugePagesCancellationAndPrivateError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { t.Fatal("canceled primary read contacted source"); return nil, nil }})
	if alerts, err := NewPgHugePagesSignal().Run(ctx, settings); !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("cancellation fabricated a primary finding")
	}
	settings.Source = &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, errors.New("private-password-token-query") }}
	alerts, err := NewPgHugePagesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("private source failure escaped the fixed visibility finding")
	}
	requireAlertOmits(t, requireAlertClass(t, alerts, "pg-huge-pages-unobservable"), "private-password-token-query")
}
