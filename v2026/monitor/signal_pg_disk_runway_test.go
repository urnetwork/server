// Missing paired history stays visible without fabricating a forecast or
// keeping an independently recovered absolute floor incident open.
package monitor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The explicit coverage signal never contacts a source or emits healthy
// runway, including when no unique primary can be selected.
func TestPgDiskRunwayUnknownHasNoSourceFetch(t *testing.T) {
	for _, mode := range []string{"configured", "missing primary", "ambiguous primary", "excluded primary"} {
		settings := syntheticSettings(&syntheticSource{
			hostFn: func(HostSettings, string) (string, error) {
				t.Fatal("forecast contacted an unimplemented source")
				return "", nil
			},
			postgresFn: func(string) ([]Row, error) {
				t.Fatal("forecast contacted a database without history authority")
				return nil, nil
			},
		})
		switch mode {
		case "missing primary":
			settings.Hosts = settings.Hosts[1:]
		case "ambiguous primary":
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		case "excluded primary":
			settings.ExcludedHosts = []string{"pg-1"}
		}
		alerts, err := NewPgDiskRunwaySignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: missing forecast is not explicit", mode)
		}
		alert := requireAlertClass(t, alerts, "pg-disk-runway-unobservable")
		if !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.SignalKey != "pg-disk-runway" {
			t.Fatal("forecast coverage has the wrong independent policy")
		}
		if !strings.Contains(alert.Observed, "forecast=unknown") || !strings.Contains(alert.Observed, "paired-history-not-implemented") {
			t.Fatal("missing history was hidden as a measured forecast")
		}
	}
}

// An ended caller receives lifecycle rather than a newly published coverage
// observation, just as the measured floor predicate does.
func TestPgDiskRunwayCancellationDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewPgDiskRunwaySignal().Run(ctx, syntheticSettings(&syntheticSource{}))
	if len(alerts) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled forecast published coverage")
	}
}

// Exercise the notifier's actual reducer with both public signal keys. A
// floor source failure has no clear credit; two complete floor recoveries
// clear its incident even while every forecast remains unobservable.
func TestPgDiskFloorRecoveryIsIndependentOfForecastUnknown(t *testing.T) {
	consumer := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	floor, forecast := NewPgDiskSpaceSignal(), NewPgDiskRunwaySignal()
	for run := 0; run < 4; run++ {
		available := "100000000001"
		if run == 0 {
			available = "1"
		}
		var sourceErr error
		if run == 1 {
			sourceErr = errors.New("synthetic source unavailable")
		}
		settings := pgDiskSpaceTestSettings(t, pgDiskSpaceTestBody(t, available, nil), sourceErr)
		at := settings.Now().Add(time.Duration(run) * 15 * time.Minute)
		forecastAlerts, err := forecast.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("forecast coverage run failed")
		}
		consumer.observeWithLock(at, forecast.Key(), SignalExecution{Alerts: forecastAlerts})
		alerts, err := floor.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("floor run failed")
		}
		transitions := consumer.observeWithLock(at, floor.Key(), SignalExecution{Alerts: alerts})
		raised, cleared := 0, 0
		for _, transition := range transitions {
			if transition.alert.Class != "pg-disk-space-low" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
			}
		}
		if run == 0 && (raised != 1 || cleared != 0) {
			t.Fatal("measured floor did not raise once")
		}
		if (run == 1 || run == 2) && (raised != 0 || cleared != 0) {
			t.Fatal("incomplete floor or first recovery cleared the incident")
		}
		if run == 3 && (raised != 0 || cleared != 1) {
			t.Fatal("independent forecast uncertainty trapped a recovered floor incident")
		}
	}
}

// Registry coverage does not claim the missing forecast implementation. The
// explicit caveat and the two public keys must survive catalog maintenance.
func TestPgDiskCatalogRetainsPartialForecastCoverage(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal("catalog unavailable")
	}
	catalog := string(data)
	start, end := strings.Index(catalog, "### 2.4b "), strings.Index(catalog, "### 2.5 ")
	if start < 0 || end <= start {
		t.Fatal("owning disk catalog sections are absent")
	}
	section := catalog[start:end]
	for _, required := range []string{
		"Probe: `pg-disk-space`", "Probe: `pg-disk-runway`", "partial coverage",
		"100,000,000,000", "48 hours", "six complete hourly intervals",
		"forecast remains unimplemented", "independent signal keys", "actual scrape timestamps",
		"pg-disk-space-low", "pg-disk-space-unobservable", "pg-disk-runway-unobservable",
	} {
		if !strings.Contains(section, required) {
			t.Fatalf("disk contract lost %q", required)
		}
	}
}
