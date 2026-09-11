package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func populateE2ECoverageBaseline(t *testing.T, stateDir, mode string, value float64) {
	t.Helper()
	store, err := newBaselineStore(stateDir + "/baseline")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for index := 0; index < e2eCoverageBaselineSamples; index++ {
		age := time.Duration(e2eCoverageBaselineSamples-1-index) * 5 * time.Minute
		store.record(fmt.Sprintf("%s/%s", e2eCoverageMetricPrefix, mode), now.Add(-age), value)
	}
}

func keyPublicationRows(
	fresh15m int,
	fresh1h int,
	newestAge int,
	modeCounts map[int][2]int,
) []Row {
	rows := make([]Row, 0, len(e2eProvideModeNames))
	for mode := 1; mode <= 3; mode++ {
		counts := modeCounts[mode]
		rows = append(rows, Row{
			fmt.Sprintf("%d", mode),
			fmt.Sprintf("%d", counts[0]),
			fmt.Sprintf("%d", counts[1]),
			fmt.Sprintf("%d", fresh15m),
			fmt.Sprintf("%d", fresh1h),
			fmt.Sprintf("%d", newestAge),
		})
	}
	return rows
}

func runKeyPublicationSynthetic(t *testing.T, stateDir string, rows []Row) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, expected := range []string{
			"pk.provide_mode IN (1, 2, 3)",
			"nc.source_client_id IS NULL",
			"nc.auth_time >= now() - interval '2 hours'",
			"ncc.client_id = pk.client_id",
			"ncc.connect_time >= now() - interval '1 hour'",
			"FROM (VALUES (1), (2), (3))",
			"interval '15 minutes'",
			"FROM current_certificates",
		} {
			if !strings.Contains(query, expected) {
				t.Fatalf("key-publication query missing %q", expected)
			}
		}
		if strings.Contains(query, "VALUES (4)") || strings.Contains(query, "IN (1, 2, 3, 4)") {
			t.Fatalf("key-publication query admitted Stream-only clients")
		}
		if strings.Contains(query, "ctc.set_time >= ncc.connect_time") ||
			strings.Contains(query, "ctc.set_time > ncc.connect_time") {
			t.Fatalf("coverage query required a certificate republish after transport reconnect")
		}
		return rows, nil
	}}
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	return NewKeyPublicationSignal().Run(context.Background(), settings)
}

func containsKeyPublicationAlertClass(alerts Alerts, class string) bool {
	for _, alert := range alerts {
		if alert.Class == class {
			return true
		}
	}
	return false
}

func TestKeyPublicationQueryKeepsLosslessIndexedRecentConnectionPrefilter(t *testing.T) {
	for _, expected := range []string{
		"nc.active",
		"nc.source_client_id IS NULL",
		"nc.auth_time >= now() - interval '2 hours'",
		"EXISTS (",
		"ncc.client_id = pk.client_id",
		"ncc.connect_time >= now() - interval '1 hour'",
	} {
		if !strings.Contains(keyPublicationQuery, expected) {
			t.Fatalf("key-publication query missing indexed exact-activity contract %q", expected)
		}
	}
	if strings.Contains(keyPublicationQuery, "nc.auth_time >= now() - interval '1 hour'") {
		t.Fatal("key-publication query narrowed the lossless two-hour auth-time prefilter to one hour")
	}
}

func TestKeyPublicationSignalSyntheticPublicProviderCollapse(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		50, 200, 0,
		map[int][2]int{1: {200000, 4000}, 2: {200, 60}, 3: {1000, 100}},
	))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "e2e-key-coverage")
	if alert.Frame != "mode=public" {
		t.Fatalf("frame = %q; want mode=public", alert.Frame)
	}
	for _, expected := range []string{
		"shared publication/store path is live",
		"fresh_15m=50",
		"network_active=200000",
		"friends_covered=60",
		"No client, network, certificate, connection, or build identity",
	} {
		if !strings.Contains(alert.Markdown(), expected) {
			t.Fatalf("alert markdown missing %q:\n%s", expected, alert.Markdown())
		}
	}
}

func TestKeyPublicationSignalSyntheticIgnoresChildAndStreamMix(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		50, 200, 0,
		map[int][2]int{1: {200000, 4000}, 2: {200, 60}, 3: {1000, 600}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy Public-provider coverage inherited legacy all-client collapse: %+v", alerts)
	}
}

func TestKeyPublicationSignalSyntheticCoverageDoesNotCompareCertificateToReconnect(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	// The result is a certificate-existence aggregate. The query assertion in
	// runKeyPublicationSynthetic pins that coverage never compares certificate
	// set_time with the later network_client_connection.connect_time.
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		1, 50, 1,
		map[int][2]int{1: {500, 10}, 2: {0, 0}, 3: {500, 300}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("transport reconnect required an invalid certificate republish: %+v", alerts)
	}
}

func TestKeyPublicationSignalSyntheticPublicationStalled(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		0, 0, 7200,
		map[int][2]int{1: {5000, 2500}, 2: {200, 100}, 3: {1000, 600}},
	))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "e2e-key-publication-stalled")
	if alert.Frame != "provider-cohort" || !strings.Contains(alert.Markdown(), "fresh_15m=0") ||
		!strings.Contains(alert.Markdown(), "friends_active=200") {
		t.Fatalf("unexpected stalled alert:\n%s", alert.Markdown())
	}
}

func TestKeyPublicationSignalSyntheticPublicationAgeBoundary(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	counts := map[int][2]int{1: {5000, 2500}, 2: {200, 100}, 3: {1000, 600}}
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(0, 100, 900, counts))
	if err != nil {
		t.Fatal(err)
	}
	if containsKeyPublicationAlertClass(alerts, "e2e-key-publication-stalled") {
		t.Fatalf("exact freshness boundary alerted: %+v", alerts)
	}
	alerts, err = runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(0, 99, 901, counts))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "e2e-key-publication-stalled")
}

func TestKeyPublicationSignalSyntheticCohortBoundaryAndNoBaselinePoison(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	for index := 0; index < 20; index++ {
		alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
			1, 50, 1,
			map[int][2]int{1: {500, 250}, 2: {100, 50}, 3: {99, 0}},
		))
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 0 {
			t.Fatalf("undersized cohort alerted at sample %d: %+v", index, alerts)
		}
	}
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		1, 50, 1,
		map[int][2]int{1: {500, 250}, 2: {100, 50}, 3: {100, 10}},
	))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "e2e-key-coverage")
}

func TestKeyPublicationSignalSyntheticCoverageAndPublicationFailTogether(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.60)
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		0, 0, 901,
		map[int][2]int{1: {500, 250}, 2: {100, 50}, 3: {100, 10}},
	))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "e2e-key-coverage")
	requireAlertClass(t, alerts, "e2e-key-publication-stalled")
}

func TestKeyPublicationSignalSyntheticLowVolumeAndPreRolloutStayQuiet(t *testing.T) {
	stateDir := t.TempDir()
	populateE2ECoverageBaseline(t, stateDir, "public", 0.04)
	alerts, err := runKeyPublicationSynthetic(t, stateDir, keyPublicationRows(
		0, 50, 3600,
		map[int][2]int{1: {50, 0}, 2: {0, 0}, 3: {50, 0}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("low-volume/pre-rollout observation alerted: %+v", alerts)
	}
}

func TestKeyPublicationSignalSyntheticRejectsMalformedAggregate(t *testing.T) {
	rows := keyPublicationRows(
		10, 20, 1,
		map[int][2]int{1: {500, 250}, 2: {100, 50}, 3: {200, 100}},
	)
	rows[1][4] = "21"
	if _, err := runKeyPublicationSynthetic(t, t.TempDir(), rows); err == nil || !strings.Contains(err.Error(), "inconsistent common controls") {
		t.Fatalf("malformed aggregate error = %v", err)
	}
}

func TestKeyPublicationSignalSyntheticRejectsDuplicateMode(t *testing.T) {
	rows := keyPublicationRows(
		10, 20, 1,
		map[int][2]int{1: {500, 250}, 2: {100, 50}, 3: {200, 100}},
	)
	rows[2][0] = "2"
	if _, err := runKeyPublicationSynthetic(t, t.TempDir(), rows); err == nil || !strings.Contains(err.Error(), "duplicate provide mode") {
		t.Fatalf("duplicate mode error = %v", err)
	}
}
