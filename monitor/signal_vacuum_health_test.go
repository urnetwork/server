package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestVacuumHealthSignalSyntheticDeadTuples(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_contract", "12000000", "08-29 11:00",
			"5000000", "vacuuming indexes", "3570", "23781003", "23781003", "0", "0", "4", "13",
			"783357", "10970904", "", "490000000", "5744", "active", "client backend", "pg_dump",
			"COPY public.transfer_contract TO stdout",
		}}, nil
	}}
	signal := NewVacuumHealthSignal()
	if signal.Cadence() != 5*time.Minute {
		t.Fatalf("vacuum cadence = %s, want 5m", signal.Cadence())
	}
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "dead-tuples")
	if len(alerts) != 1 || !strings.Contains(alerts[0].Evidence, "application=pg_dump") ||
		!strings.Contains(alerts[0].Evidence, "backend_xmin=490000000") ||
		!strings.Contains(alerts[0].Evidence, "vacuum phase=vacuuming indexes age_s=3570") ||
		!strings.Contains(alerts[0].Evidence, "heap_scanned=23781003/23781003") ||
		!strings.Contains(alerts[0].Evidence, "indexes_processed=4/13") ||
		!strings.Contains(alerts[0].Context, "payment retention") ||
		!strings.Contains(alerts[0].Verify, "consecutive five-minute samples") {
		t.Fatalf("dead-tuple alert did not attribute the vacuum and xmin holder: %+v", alerts)
	}
}

func TestVacuumHealthHorizonRankingPrefersOldTransactionOnEqualXmin(t *testing.T) {
	normalizedSQL := strings.Join(strings.Fields(pgVacuumHealthSQL), " ")
	for _, clause := range []string{
		"backend_xid IS NOT NULL OR backend_xmin IS NOT NULL",
		"greatest(coalesce(age(backend_xid),-1), coalesce(age(backend_xmin),-1)) AS horizon_age",
		"ORDER BY horizon_age DESC, coalesce(xact_start,query_start) ASC NULLS LAST, pid",
	} {
		if !strings.Contains(normalizedSQL, clause) {
			t.Fatalf("vacuum horizon query lost deterministic old-transaction ranking %q:\n%s", clause, normalizedSQL)
		}
	}
}

func TestVacuumHealthProgressQuerySupportsBothIndexProgressSchemas(t *testing.T) {
	normalizedSQL := strings.Join(strings.Fields(pgVacuumHealthSQL), " ")
	for _, field := range []string{"index_vacuum_count", "indexes_processed", "indexes_total"} {
		want := "to_jsonb(v)->>'" + field + "'"
		if !strings.Contains(normalizedSQL, want) {
			t.Fatalf("vacuum progress query does not read %s compatibly:\n%s", field, normalizedSQL)
		}
	}
}

func TestVacuumHealthSignalExplainsReliabilityHorizon(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_contract", "13099152", "08-30 06:34",
			"5000000", "vacuuming indexes", "2610", "23781003", "23781003", "0", "0", "9", "13",
			"774955", "6464477", "512924938", "512924938", "3200", "active", "client backend", "",
			"INSERT INTO client_reliability_running (client_id, lookback_index) SELECT client_id, $4 FROM client_reliability",
		}}, nil
	}}
	alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "dead-tuples")
	for _, detail := range []string{
		"full running-window re-anchor",
		"multi-billion-row transaction",
		"restricted each vacuum to rows removable before that old horizon",
		"four-hour reliability re-anchor cadence",
		"waiting maintenance proceeds",
	} {
		markdown := alert.Markdown()
		if !strings.Contains(markdown, detail) {
			t.Fatalf("reliability horizon alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestVacuumHealthSignalExplainsRetentionHorizon(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_contract", "21662641", "08-30 08:23",
			"5000000", "vacuuming indexes", "3134", "23793748", "23793748", "0", "0", "4", "13",
			"1614586", "90463", "531679478", "531679323", "98", "active", "client backend", "",
			"UPDATE transfer_contract SET reap_time = LEAST(COALESCE(transfer_contract.reap_time, 'infinity'), $2) WHERE transfer_contract.contract_id IN (SELECT contract_id FROM transfer_escrow_sweep)",
		}}, nil
	}}
	alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "dead-tuples")
	for _, detail := range []string{
		"legacy CompletePayment retention fan-out",
		"millions of transfer_contract",
		"contract_retention_pending queue",
		"bounded, committed contract_retention_cursor batches",
		"Do not cancel the progressing vacuum",
		"legacy retention query disappears",
	} {
		markdown := alert.Markdown()
		if !strings.Contains(markdown, detail) {
			t.Fatalf("retention horizon alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestVacuumHealthSignalExplainsBoundedCloseRecoveryWriter(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_contract", "12100000", "08-30 10:03",
			"5000000", "vacuuming heap", "120", "23793748", "4000000", "3900000", "0", "0", "13",
			"1630191", "900", "532300000", "532299900", "1", "idle in transaction", "client backend", "",
			"UPDATE transfer_contract SET outcome = $2, close_time = $3 WHERE contract_id = $1 AND outcome IS NULL",
		}}, nil
	}}
	alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "dead-tuples")
	for _, detail := range []string{
		"bounded per-contract CloseExpiredContracts",
		"not an old MVCC pin",
		"open-contract signal is authoritative",
		"25,000-contract task checkpoint",
		"Do not cancel the closer",
		"Older open-contract buckets fall",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, detail) {
			t.Fatalf("bounded-close vacuum alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestVacuumHealthSignalExplainsPaymentPlannerHorizon(t *testing.T) {
	queries := []string{
		"CREATE TEMPORARY TABLE temp_account_payment ON COMMIT DROP AS SELECT u.contract_id, u.balance_id, u.network_id FROM transfer_contract u",
		"SELECT MIN(transfer_contract.create_time) AS subsidy_start_time, MAX(transfer_contract.close_time) AS subsidy_end_time FROM transfer_escrow_sweep INNER JOIN transfer_contract",
		"SELECT start_time, end_time FROM subsidy_payment WHERE start_time < $2 AND $1 < end_time",
	}
	for _, query := range queries {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{{
				"transfer_contract", "14103687", "08-30 10:03",
				"5000000", "vacuuming indexes", "1673", "23980545", "23980545", "0", "0", "0", "13",
				"1742249", "81239", "536367736", "536367372", "83", "active", "client backend", "",
				query,
			}}, nil
		}}
		alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "dead-tuples")
		for _, detail := range []string{
			"Payout payment planner",
			"bounded temp_account_payment working set",
			"not the unbounded transfer_contract retention writer",
			"task-canary signal is authoritative",
			"SET LOCAL idle_in_transaction_session_timeout override",
			"Do not cancel this seconds-old reader",
			"unrelated PostgreSQL session retains the global five-minute",
		} {
			if markdown := alert.Markdown(); !strings.Contains(markdown, detail) {
				t.Fatalf("payment-planner vacuum alert missing %q for query %q:\n%s", detail, query, markdown)
			}
		}
	}
}

func TestVacuumHealthSignalRejectsFreshReadAsHorizonOwner(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_contract", "15056495", "08-30 10:03",
			"5000000", "vacuuming indexes", "3172", "23980545", "23980545", "0", "0", "4", "13",
			"1791054", "2114", "", "538585747", "2", "active", "client backend", "",
			"SELECT client_id, COALESCE(SUM(match_count), 0) FROM search_provider_stats WHERE client_id = ANY($1::uuid[])",
		}}, nil
	}}
	alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "dead-tuples")
	for _, detail := range []string{
		"fresh read-only snapshot",
		"not a persistent horizon holder",
		"large backend_xmin age is inherited",
		"negative attribution evidence",
		"Do not cancel or tune around the fresh SELECT",
		"index progress continues",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, detail) {
			t.Fatalf("fresh-read vacuum alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestVacuumHealthSignalHonorsExplicitCascadeThreshold(t *testing.T) {
	for _, test := range []struct {
		configured int64
		want       int64
	}{
		{configured: 0, want: 10_000_000},
		{configured: 5_000_000, want: 10_000_000},
		{configured: 25_000_000, want: 25_000_000},
	} {
		if got := vacuumDeadTupleAlertThreshold(test.configured); got != test.want {
			t.Fatalf("configured threshold %d => alert threshold %d, want %d", test.configured, got, test.want)
		}
	}

	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_escrow", "10593142", "08-29 11:25",
			"25000000", "", "0", "0", "0", "0", "0", "0", "0",
			"42", "100", "99", "98", "0", "idle in transaction", "client backend", "", "SELECT 1",
		}}, nil
	}}
	alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("cascade table below its explicit 25M threshold alerted: %+v", alerts)
	}
}
