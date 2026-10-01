package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestReindexDebrisSignalSyntheticFailedTransferEscrowRetries(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"reltoastrelid",
			"indisready",
			"pg_stat_progress_create_index",
			"query_age_s",
			"blocks_done",
			"tuples_done",
			"lockers_done",
			"partitions_done",
			"relation_in_progress",
			"exact_index_in_progress",
			"active_progress",
			"_(ccnew|ccold)[0-9]*$",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("reindex-debris query is missing %q:\n%s", want, query)
			}
		}
		return []Row{
			{
				"transfer_escrow",
				"37",
				"29",
				"123456789012",
				"public.transfer_escrow_pkey_ccnew, pg_toast.pg_toast_42_index_ccnew7",
				"0",
				"0",
				"0",
				"",
				"",
			},
			{
				"pending_task",
				"2",
				"2",
				"1048576",
				"public.pending_task_pkey_ccnew, pg_toast.pg_toast_99_index_ccnew",
				"0",
				"0",
				"0",
				"",
				"",
			},
		}, nil
	}}

	alerts, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reindex-debris")
	markdown := alert.Markdown()
	for _, want := range []string{
		"2 table(s) own at least 39 inactive incomplete",
		"write_ready_indexes=31",
		"bytes=123457837588",
		"transfer_escrow: indexes=37 write_ready=29 bytes=123456789012",
		"pending_task: indexes=2 write_ready=2 bytes=1048576",
		"TOAST relation",
		"excludes an exact index represented in pg_stat_progress_create_index",
		"No invalid candidate is currently hidden behind active index work",
		"without emitting one duplicate alert per table",
		"908a8b2c",
		"d8392c83",
		"patch-identical equivalents",
		"compare every active Taskworker artifact",
		"Deploy only blocks that predate either fix",
		"When all blocks already contain both fixes, do not redeploy Taskworker",
		"exclude the large/high-churn contract and escrow tables",
		"cleanup before and after each permitted object",
		"keep DbMaintenance owned when only its pooled timestamp refresh stalls",
		"§8.12 proves every active Taskworker's source/digest identity and the runtime behavior of both fixes",
		"bringyourctl db maintenance all --cleanup",
		"Do not wildcard-drop indexes or cancel a live rebuild",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("reindex-debris alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestReindexDebrisSignalKeepsActiveTableBytesVisibleAsUnclassified(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{
			{
				"contract_close",
				"13", "0", "7423918080", "public.contract_close_idx_ccnew",
				"0", "0", "0", "", "",
			},
			{
				"transfer_escrow",
				"0", "0", "0", "",
				"15", "15", "174100488192", "public.transfer_escrow_pkey_ccnew7",
				"relation=transfer_escrow index=transfer_escrow_pkey_ccnew8 command='REINDEX CONCURRENTLY' phase='building index: scanning table' query_age_s=1880 wait=IO:DataFileRead blockers=0 blocks=123/456 tuples=0/0 lockers=0/0 partitions=0/0",
			},
		}, nil
	}}

	alerts, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%d, want one aggregate debris alert: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "reindex-debris")
	markdown := alert.Markdown()
	for _, want := range []string{
		"1 table(s) own at least 13 inactive incomplete",
		"15 additional invalid candidate(s), totaling 162.14 GiB, share 1 active table(s) and are not counted as reclaimed",
		"active_table_candidates=15",
		"active_table_write_ready=15",
		"active_table_bytes=174100488192",
		"transfer_escrow: invalid_candidates=15 write_ready=15 bytes=174100488192",
		"phase='building index: scanning table'",
		"query_age_s=1880",
		"wait=IO:DataFileRead",
		"blocks=123/456",
		"a fall in confirmed bytes while this value rises is not cleanup",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("active-table accounting missing %q:\n%s", want, markdown)
		}
	}
}

func TestReindexDebrisSignalReportsOnlyActiveTableCandidatesAsObscured(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"transfer_escrow",
			"0", "0", "0", "",
			"15", "12", "174100488192", "public.transfer_escrow_pkey_ccnew7",
			"relation=transfer_escrow index=transfer_escrow_pkey_ccnew8 command='REINDEX CONCURRENTLY' phase='waiting for writers before build' query_age_s=63 wait=Lock:virtualxid blockers=1 blocks=0/0 tuples=0/0 lockers=0/1 partitions=0/0",
		}}, nil
	}}

	alerts, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "reindex-debris-obscured" {
		t.Fatalf("active-table-only result = %+v", alerts)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{
		"cannot yet be classified as live or debris",
		"active_table_candidates=15",
		"confirmed_inactive_indexes=0",
		"phase='waiting for writers before build'",
		"blockers=1",
		"not reclaimed storage",
		"Do not drop a candidate or cancel the operation",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("obscured alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestReindexDebrisSignalHealthyWithoutInactiveArtifacts(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, nil
	}}
	alerts, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("empty debris catalog produced alerts: %+v", alerts)
	}
}

func TestReindexDebrisSignalRejectsMalformedCounts(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"transfer_escrow", "2", "3", "1024", "sample", "0", "0", "0", "", ""}}, nil
	}}
	if _, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "count=2 ready=3") {
		t.Fatalf("malformed debris counts error = %v", err)
	}
}

func TestReindexDebrisSignalRejectsMissingOrImpossibleProgressDetail(t *testing.T) {
	tests := []struct {
		name string
		row  Row
		want string
	}{
		{
			name: "active candidates without progress",
			row:  Row{"transfer_escrow", "0", "0", "0", "", "2", "1", "1024", "sample", ""},
			want: "have no progress detail",
		},
		{
			name: "inactive owner with progress",
			row:  Row{"contract_close", "2", "0", "1024", "sample", "0", "0", "0", "", "phase='building index'"},
			want: "unexpectedly has progress detail",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{test.row}, nil
			}}
			if _, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("progress-detail error = %v, want %q", err, test.want)
			}
		})
	}
}
