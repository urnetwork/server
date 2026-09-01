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
			},
			{
				"pending_task",
				"2",
				"2",
				"1048576",
				"public.pending_task_pkey_ccnew, pg_toast.pg_toast_99_index_ccnew",
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
		"2 table(s) own 39 inactive incomplete",
		"write_ready_indexes=31",
		"bytes=123457837588",
		"transfer_escrow: indexes=37 write_ready=29 bytes=123456789012",
		"pending_task: indexes=2 write_ready=2 bytes=1048576",
		"TOAST relation",
		"not the temporary invalid index of a live concurrent build",
		"without emitting one duplicate alert per table",
		"skips full-table transfer_escrow rebuilds",
		"cleanup before and after each selected object",
		"bringyourctl db maintenance all --cleanup",
		"Do not wildcard-drop indexes or cancel a live rebuild",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("reindex-debris alert missing %q:\n%s", want, markdown)
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
		return []Row{{"transfer_escrow", "2", "3", "1024", "sample"}}, nil
	}}
	if _, err := NewReindexDebrisSignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "count=2 ready=3") {
		t.Fatalf("malformed debris counts error = %v", err)
	}
}
