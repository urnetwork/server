package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func syntheticPgCapacityRows(total string, active string, idle string, idleInTx string) []Row {
	return []Row{
		{"summary", "1024", "3", "0", "1021", total, active, idle, idleInTx, "239674kB", "256GB"},
		{"owner", "connect", "bringyour", "192.0.2.31", "idle", "180", "Client:ClientRead", "540", "3500", "", ""},
		{"owner", "connect", "bringyour", "2001:db8:31::/64", "idle", "120", "IO:DataFileRead", "600", "3550", "", ""},
		{"owner", "taskworker", "bringyour", "198.51.100.32", "active", "80", "IO:DataFileRead", "3", "1200", "", ""},
	}
}

func TestPgCapacitySignalHealthyWithHeadroom(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"pg_settings",
			"superuser_reserved_connections",
			"reserved_connections",
			"backend_type = 'client backend'",
			"client_addr::text",
			"GROUP BY application_name, role_name, client_address, connection_state",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("pg-capacity query missing %q:\n%s", want, query)
			}
		}
		if strings.Contains(query, "LIMIT 10") {
			t.Fatalf("pg-capacity query ranks raw addresses before privacy-safe aggregation:\n%s", query)
		}
		return syntheticPgCapacityRows("400", "31", "340", "7"), nil
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy PostgreSQL capacity produced alerts: %+v", alerts)
	}
}

func TestPgCapacitySignalWarnsWithBoundedOwnerEvidence(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return syntheticPgCapacityRows("800", "230", "540", "7"), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:           "edge-synthetic",
		LANAddress:     "192.0.2.31",
		OverlayAddress: "2001:db8:31::",
		Roles:          []string{"services"},
	})
	alerts, err := NewPgCapacitySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "1.3a" || alert.SignalKey != "pg-capacity" {
		t.Fatalf("capacity alert identity = %+v", alert)
	}
	for _, want := range []string{
		"uses 78.4%",
		"normal_role_ceiling=1021",
		"normal_role_slots_remaining=221",
		"work_mem=239674kB",
		"shared_buffers=256GB",
		"application=connect role=bringyour client_owner=edge-synthetic state=idle clients=300 waits=Client:ClientRead,IO:DataFileRead oldest_state_s=600 oldest_backend_s=3550",
		"bounded to ten after privacy-safe host aggregation; no client addresses or query text",
		"Independent PgBouncer processes",
		"SHOW POOLS where administrative access exists",
		"60-66-second COMMIT latency",
		"replacement overlap",
		"idle recovery cohort did not establish retention",
		"Do not raise max_connections first",
		"large work_mem",
		"diagnostic amplification",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("capacity warning missing %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{"192.0.2.31", "2001:db8:31::/64", "198.51.100.32"} {
		if markdown := alert.Markdown(); strings.Contains(markdown, forbidden) {
			t.Fatalf("capacity warning leaked client address %q:\n%s", forbidden, markdown)
		}
	}
}

func TestPgCapacityOwnerEvidenceReducesUnexpectedRawAddresses(t *testing.T) {
	rows := []pgRow{
		{"summary", "1024", "3", "0", "1021", "800", "230", "540", "7", "239674kB", "256GB"},
		{"owner", "connect", "bringyour", "203.0.113.41/32", "idle", "4", "Client:ClientRead", "10", "20", "", ""},
	}
	_, owners, err := parsePgCapacityRows(rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence := pgCapacityEvidence(owners)
	if !strings.Contains(evidence, "client_owner=unmapped-service-client") {
		t.Fatalf("capacity owner evidence omitted privacy-safe fallback: %s", evidence)
	}
	for _, forbidden := range []string{"203.0.113.41", "203.0.113.41/32"} {
		if strings.Contains(evidence, forbidden) {
			t.Fatalf("capacity owner diagnostic leaked client address %q: %s", forbidden, evidence)
		}
	}
}

func TestPgCapacityMalformedRowsDoNotEchoAddresses(t *testing.T) {
	tests := []struct {
		name string
		row  pgRow
	}{
		{
			name: "address in numeric column",
			row:  pgRow{"owner", "connect", "bringyour", "192.0.2.51", "idle", "198.51.100.51", "-:-", "10", "20", "", ""},
		},
		{
			name: "address as row kind",
			row:  pgRow{"203.0.113.51", "", "", "", "", "", "", "", "", "", ""},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parsePgCapacityRows([]pgRow{test.row}, nil)
			if err == nil {
				t.Fatal("malformed capacity row was accepted")
			}
			for _, forbidden := range []string{"192.0.2.51", "198.51.100.51", "203.0.113.51"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("capacity parser error leaked address %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestPgCapacitySignalPagesNearExhaustion(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return syntheticPgCapacityRows("940", "746", "170", "12"), nil
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatalf("near-exhaustion alert = %+v", alert)
	}
	if !strings.Contains(alert.Observed, "normal_role_slots_remaining=81") {
		t.Fatalf("near-exhaustion observed = %q", alert.Observed)
	}
}

func TestPgCapacitySignalClassifiesDirectSlotRejection(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("psql: FATAL: sorry, too many clients already")
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityPage {
		t.Fatalf("direct rejection severity = %q", alert.Severity)
	}
	for _, want := range []string{
		"rejected the direct capacity observation",
		"direct_connection_result=too_many_clients_already",
		"canonical `too many clients already`",
		"capacity_values=unavailable",
		"not a count of unique rejected PostgreSQL sessions",
		"60-66-second COMMIT latency",
		"replacement",
		"headroom stays above 25%",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("direct rejection alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestPgCapacitySignalPreservesUnrelatedQueryFailure(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("synthetic route unavailable")
	}}
	if _, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "route unavailable") {
		t.Fatalf("unrelated capacity query error = %v", err)
	}
}

func TestPgCapacitySignalRejectsMalformedSummary(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"owner", "connect", "bringyour", "198.51.100.31", "idle", "4", "Client:ClientRead", "10", "20", "", ""}}, nil
	}}
	if _, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "no summary row") {
		t.Fatalf("missing capacity summary error = %v", err)
	}
}

// Preserve both independent capacity bands.
func TestPgCapacityIndependentRemainingSlotGuard(t *testing.T) {
	for _, test := range []struct {
		name      string
		maximum   string
		ceiling   string
		clients   string
		remaining string
		severity  Severity
		sustain   int
	}{
		{name: "small ceiling sixty remaining", maximum: "203", ceiling: "200", clients: "140", remaining: "60", severity: SeverityPage, sustain: 1},
		{name: "small ceiling exactly sixty-four", maximum: "203", ceiling: "200", clients: "136", remaining: "64", severity: SeverityPage, sustain: 1},
		{name: "small ceiling sixty-five healthy", maximum: "203", ceiling: "200", clients: "135", remaining: "65"},
		{name: "large ceiling below warning", maximum: "1003", ceiling: "1000", clients: "749", remaining: "251"},
		{name: "large ceiling warning boundary", maximum: "1003", ceiling: "1000", clients: "750", remaining: "250", severity: SeverityWarn, sustain: 2},
		{name: "large ceiling percentage page", maximum: "1003", ceiling: "1000", clients: "900", remaining: "100", severity: SeverityPage, sustain: 1},
		{name: "sixty-five preserves utilization warning", maximum: "303", ceiling: "300", clients: "235", remaining: "65", severity: SeverityWarn, sustain: 2},
		{name: "sixty-four promotes utilization warning", maximum: "303", ceiling: "300", clients: "236", remaining: "64", severity: SeverityPage, sustain: 1},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{{
				"summary", test.maximum, "3", "0", test.ceiling,
				test.clients, test.clients, "0", "0", "4MB", "128MB",
			}}, nil
		}}
		alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.severity == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: healthy independent bands produced %d alerts", test.name, len(alerts))
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: capacity alerts = %d, want one", test.name, len(alerts))
		}
		alert := alerts[0]
		if alert.Class != "pg-client-capacity" || alert.Severity != test.severity || alert.Sustain != test.sustain {
			t.Fatalf("%s: class/severity/sustain = %s/%s/%d, want pg-client-capacity/%s/%d",
				test.name, alert.Class, alert.Severity, alert.Sustain, test.severity, test.sustain)
		}
		if !strings.Contains(alert.Observed, "normal_role_slots_remaining="+test.remaining+" ") {
			t.Fatalf("%s: independent slot observation missing: %s", test.name, alert.Observed)
		}
		if !strings.Contains(alert.Baseline, "more than 64") ||
			!strings.Contains(alert.Verify, "more than 64") {
			t.Fatalf("%s: capacity recovery guidance omits the independent slot band", test.name)
		}
	}
}

// Preserve source uncertainty when numeric capacity evidence is unavailable.
func TestPgCapacitySlotGuardDoesNotReplaceUnknownObservation(t *testing.T) {
	for _, test := range []struct {
		name string
		rows []Row
		err  error
	}{
		{name: "missing summary"},
		{name: "unrelated source error", err: errors.New("synthetic observation unavailable")},
		{name: "canceled source", err: context.Canceled},
		{name: "invalid ceiling", rows: []Row{
			{"summary", "3", "3", "0", "0", "0", "0", "0", "0", "4MB", "128MB"},
		}},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return test.rows, test.err
		}}
		alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
		if err == nil || len(alerts) != 0 {
			t.Fatalf("%s: unknown observation became numeric capacity evidence: err=%v alerts=%d",
				test.name, err, len(alerts))
		}
	}
}
