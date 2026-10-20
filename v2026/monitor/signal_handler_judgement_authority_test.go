package monitor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// SQL staleness does not observe the additional judgement that current readers
// use. Exercise the public warning, including independent product evidence.
func TestHandlerLeaseJudgementPublicAuthority(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, independentPage := range []bool{false, true} {
			pgReads, nativeReads := 0, 0
			mature, eligible := int64(0), int64(0)
			if independentPage {
				mature, eligible = 1, 1001
			}
			cache := handlerLeaseTestCache(t, false)
			source := &syntheticSource{
				postgresFn: func(query string) ([]Row, error) {
					pgReads++
					rows := handlerLeaseTestRows(query, selection, 100, mature, eligible)
					if !selection && mature == 0 {
						rows[0][3] = "0"
					}
					return rows, nil
				},
				redisFn: func(host HostSettings, port int, args ...string) (string, error) {
					if len(args) < 2 || args[len(args)-2] != "GET" {
						t.Fatal("qualifier correction added a new Redis judgement read")
					}
					return cache(host, port, args...)
				},
				hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
					nativeReads++
					return "", nil
				},
			}
			var signal Signal = NewConnectionOrphansSignal()
			productClass, domain, wantPgReads := "connection-orphans", "open-connections", 1
			if selection {
				signal = NewSelectionPopulationSignal()
				productClass, domain, wantPgReads = "selection-empty", "potential-provider-connections", 2
			}
			alerts, err := signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("judgement qualifier control failed before observation")
			}
			warning := requireAlertClass(t, alerts, "cannot-observe")
			if warning.Frame != "handler-lease-coverage" || warning.Severity != SeverityWarn || warning.Sustain != 2 || !isVisibilityAlert(warning) {
				t.Fatal("unobserved judgement changed alert policy or recovery authority")
			}
			for _, marker := range []string{"coverage_domain=" + domain, "stale_owned_open_rows=100 ", "reader_judgement=unobserved", "read_side_exclusion=unproven", "process_liveness=unknown"} {
				if !strings.Contains(warning.Observed, marker) {
					t.Fatalf("%s lacks the bounded judgement qualifier %q", signal.Key(), marker)
				}
			}
			if !strings.Contains(warning.Mechanism, "also accept stale handlers judged live") ||
				!strings.Contains(warning.Baseline, "a fresh SQL heartbeat or an applicable judged-live stale handler") ||
				!strings.Contains(warning.Evidence, "does not override judged-live ownership") ||
				!strings.Contains(warning.Verify, "may establish read-side recovery") ||
				!strings.Contains(warning.Verify, "leaves that authority unobserved") {
				t.Fatal("SQL-only warning imposed fresh heartbeat as the only read-side authority")
			}
			for _, consumer := range []string{"Location", "subscriber", "named-subscriber", "audit", "ARIN"} {
				if !strings.Contains(warning.Context, consumer) {
					t.Fatal("warning omitted an applicable judged-live consumer")
				}
			}
			requireAlertOmits(t, warning, "12345678-1234-4234-8234-123456789abc", "heartbeat-stale rows independently of live Connect transports")
			wantPages := 0
			if independentPage {
				wantPages = 1
				if requireAlertClass(t, alerts, productClass).Severity != SeverityPage {
					t.Fatal("qualifier erased an independent complete product finding")
				}
			}
			if requireAlertClassCount(alerts, productClass) != wantPages || pgReads != wantPgReads || nativeReads != 0 {
				t.Fatal("qualifier changed query count, source scope or independent PAGE policy")
			}
		}
	}
}

// Fresh owned rows and unused stale handlers still satisfy the existing narrow
// SQL branch; neither control supplies a native or consumer-cache verdict.
func TestHandlerLeaseJudgementFreshAndUnusedControls(t *testing.T) {
	for _, selection := range []bool{false, true} {
		source := &syntheticSource{
			postgresFn: func(query string) ([]Row, error) {
				rows := handlerLeaseTestRows(query, selection, 0, 0, 0)
				if !selection {
					rows[0][3] = "0"
					rows[0][4] = "23"
					rows[0][5] = "22"
				}
				return rows, nil
			},
			redisFn: handlerLeaseTestCache(t, false),
		}
		var signal Signal = NewConnectionOrphansSignal()
		if selection {
			signal = NewSelectionPopulationSignal()
		}
		alerts, err := signal.Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 0 {
			t.Fatal("qualifier converted a fresh or unused-handler control to an incident")
		}
	}
}

// The owning prose preserves the new model authority without asserting that a
// source snapshot proves adoption or that SQL alone observed the judgement.
func TestHandlerLeaseJudgementCatalogAuthority(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	for _, heading := range []string{"### 2.9 Provider-selection population", "### 2.16 Open connection handler ownership", "### 5.13 Planned primary restart"} {
		start := strings.Index(catalog, heading)
		if start < 0 {
			t.Fatal("missing owning catalog section")
		}
		section := catalog[start:]
		if next := strings.Index(section[1:], "\n### "); next >= 0 {
			section = section[:next+1]
		}
		if !strings.Contains(section, "judgement") || !strings.Contains(section, "adoption") {
			t.Fatal("owning catalog lost the reader judgement or actual adoption boundary")
		}
		if strings.Contains(section, "readers still require the existing") || strings.Contains(section, "Read-side heartbeat exclusion remains a separate concern.") {
			t.Fatal("catalog reimposed the superseded blanket heartbeat requirement")
		}
	}
	for _, authority := range []string{"reader_judgement=unobserved", "read_side_exclusion=unproven", "not a necessary\ncondition for all reader health", "Source ancestry and documentary reports alone do not\nattest current process adoption or health"} {
		if !strings.Contains(catalog, authority) {
			t.Fatal("catalog omitted the scoped alternative-recovery authority")
		}
	}
}
