package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// MG5's §14 part (SMTP-M5-06) on a private database: the report tables follow
// the tag, an aggregate counts once per (reporter, report_id) however many
// writers race, complaints read back as written, a row a column cannot hold is
// refused whole, and every column maps to smtp-5.

// §14.14's report tables, which plan-SMTP-report-intake-home put in MG5.
var smtpReportTables = []string{"smtp_complaint", "smtp_dmarc_daily"}

// Which of the report tables the database holds.
func smtpPresentTables(ctx context.Context, tables []string) []string {
	present := []string{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, table := range tables {
			var found *string
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&found))
			if found != nil {
				present = append(present, table)
			}
		}
	})
	return present
}

// A report id no other run shares.
func smtpTestReportId() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return fmt.Sprintf("report-%x", value)
}

// A DMARC report from reporter of one count for networkId.
func smtpTestAggregate(reporter string, reportId string, networkId sidecar.Id, messageCount uint64) *smtpedgeservice.SmtpAggregateReport {
	return &smtpedgeservice.SmtpAggregateReport{
		Reporter:    reporter,
		ReportId:    reportId,
		Kind:        smtpedgeservice.SmtpAggregateKindDmarc,
		Day:         time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC),
		ArrivalTime: time.Date(2026, time.November, 3, 4, 5, 6, 0, time.UTC),
		Counts: []*smtpedgeservice.SmtpAggregateCount{{
			NetworkId:     networkId,
			Domain:        "quietnet.root.example",
			MessageCount:  messageCount,
			DkimPassCount: messageCount,
		}},
	}
}

// A complaint against networkId with every field set, its sealed copy random.
func smtpTestComplaint(networkId sidecar.Id, arrival time.Time) *smtpedgeservice.SmtpComplaint {
	sealed := make([]byte, 96)
	if _, err := rand.Read(sealed); err != nil {
		panic(err)
	}
	return &smtpedgeservice.SmtpComplaint{
		ComplaintId:   sidecar.NewId(),
		NetworkId:     networkId,
		Reporter:      "loop.example",
		Type:          smtpedgeservice.SmtpComplaintTypeAbuse,
		ArrivalTime:   arrival,
		Domain:        "quiet-otter.quietnet.root.example",
		DkimDomain:    "quietnet.root.example",
		DkimSelector:  "abcdefghijklmnopqrstuvwxyz",
		DkimResult:    "pass",
		MessageIdHash: smtpedgeservice.SmtpMessageIdHash("<m1@quietnet.root.example>"),
		XUrRef:        "1.abcdefghijklmnopqrstuvwxyz234567ab",
		FeedbackId:    "n:c:abcdefghijklmnopqrstuvwxyz:urnetwork",
		JoinResult:    "high",
		SealedReport:  sealed,
	}
}

// The group is absent without the tag and present with it, both tables at
// once; without it every write refuses and every read finds nothing.
func TestSmtpReportTablesFollowTheTag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		present := smtpPresentTables(ctx, smtpReportTables)
		networkId := sidecar.NewId()
		store := NewSmtpEdgeStore(nil)
		if !server.SidecarMg5Linked() {
			if len(present) != 0 {
				t.Fatalf("a binary that does not link MG5 has %v", present)
			}
			if err := InsertSmtpComplaint(ctx, smtpTestComplaint(networkId, server.NowUtc())); !errors.Is(err, ErrSmtpReportUnlinked) {
				t.Fatalf("a complaint without MG5 answered %v", err)
			}
			if _, err := store.InsertAggregate(ctx, smtpTestAggregate("reporter.example", smtpTestReportId(), networkId, 1)); !errors.Is(err, ErrSmtpReportUnlinked) {
				t.Fatalf("an aggregate without MG5 answered %v", err)
			}
			if err := store.InsertComplaint(ctx, smtpTestComplaint(networkId, server.NowUtc())); !errors.Is(err, ErrSmtpReportUnlinked) {
				t.Fatalf("the store's complaint without MG5 answered %v", err)
			}
			if GetSmtpComplaints(ctx, server.Id(networkId), time.Time{}) != nil || GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{}) != nil {
				t.Fatal("a read without MG5 answered rows")
			}
			return
		}
		if !slices.Equal(present, smtpReportTables) {
			t.Fatalf("a binary that links MG5 has %v of %v", present, smtpReportTables)
		}
	})
}

// A second insert of a (reporter, report_id) answers false and changes
// nothing, whatever counts or networks it carries, and the same report id from
// another reporter is another report (§14.8, D-SMTP-23).
func TestSmtpAggregateInsertsOncePerReporterAndReportId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		networkId := sidecar.NewId()
		otherNetworkId := sidecar.NewId()
		reportId := smtpTestReportId()
		report := smtpTestAggregate("reporter.example", reportId, networkId, 5)
		// a burner's row of the same report
		report.Counts = append(report.Counts, &smtpedgeservice.SmtpAggregateCount{NetworkId: networkId, Domain: "quiet-otter.quietnet.root.example", MessageCount: 2, RejectCount: 2})
		inserted, err := store.InsertAggregate(ctx, report)
		if err != nil || !inserted {
			t.Fatalf("a new report: %t, %v", inserted, err)
		}
		inserted, err = store.InsertAggregate(ctx, report)
		if err != nil || inserted {
			t.Fatalf("the same report again: %t, %v", inserted, err)
		}
		// a replay under the stored pair with other counts and another network
		replay := smtpTestAggregate("reporter.example", reportId, otherNetworkId, 500)
		inserted, err = store.InsertAggregate(ctx, replay)
		if err != nil || inserted {
			t.Fatalf("a replay with other counts: %t, %v", inserted, err)
		}
		if reports := GetSmtpAggregateReports(ctx, server.Id(otherNetworkId), time.Time{}); len(reports) != 0 {
			t.Fatalf("the replay's network has %d reports", len(reports))
		}
		reports := GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{})
		if len(reports) != 1 {
			t.Fatalf("%d reports", len(reports))
		}
		stored := reports[0]
		if stored.Reporter != "reporter.example" || stored.ReportId != reportId || stored.Kind != smtpedgeservice.SmtpAggregateKindDmarc ||
			!stored.Day.Equal(report.Day) || !stored.ArrivalTime.Equal(report.ArrivalTime) || len(stored.Counts) != 2 {
			t.Fatalf("the report reads back as %+v", stored)
		}
		byDomain := map[string]*smtpedgeservice.SmtpAggregateCount{}
		for _, count := range stored.Counts {
			byDomain[count.Domain] = count
		}
		network := byDomain["quietnet.root.example"]
		burner := byDomain["quiet-otter.quietnet.root.example"]
		if network == nil || network.NetworkId != networkId || network.MessageCount != 5 || network.DkimPassCount != 5 {
			t.Fatalf("the network's count reads back as %+v", network)
		}
		if burner == nil || burner.MessageCount != 2 || burner.RejectCount != 2 || burner.DkimPassCount != 0 {
			t.Fatalf("the burner's count reads back as %+v", burner)
		}
		// another reporter's report of the same id
		inserted, err = store.InsertAggregate(ctx, smtpTestAggregate("other-reporter.example", reportId, networkId, 1))
		if err != nil || !inserted {
			t.Fatalf("another reporter's report: %t, %v", inserted, err)
		}
		if reports := GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{}); len(reports) != 2 {
			t.Fatalf("%d reports after another reporter's", len(reports))
		}
	})
}

// Writers racing on one report store it once: exactly one answers true, and
// the rows are the first one's alone.
func TestSmtpAggregateWritersAtOnceStoreItOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		reportId := smtpTestReportId()
		writerCount := 8
		networkIds := make([]sidecar.Id, writerCount)
		for i := range networkIds {
			networkIds[i] = sidecar.NewId()
		}
		start := make(chan struct{})
		insertedResults := make([]bool, writerCount)
		errs := make([]error, writerCount)
		var wait sync.WaitGroup
		for i := range writerCount {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				insertedResults[i], errs[i] = store.InsertAggregate(ctx, smtpTestAggregate("reporter.example", reportId, networkIds[i], uint64(i+1)))
			}()
		}
		close(start)
		wait.Wait()
		winners := []int{}
		for i := range writerCount {
			if errs[i] != nil {
				t.Fatalf("writer %d: %v", i, errs[i])
			}
			if insertedResults[i] {
				winners = append(winners, i)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("%d writers stored the report", len(winners))
		}
		for i, networkId := range networkIds {
			reports := GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{})
			if i == winners[0] {
				if len(reports) != 1 || reports[0].Counts[0].MessageCount != uint64(i+1) {
					t.Fatalf("the stored writer's rows: %v", reports)
				}
			} else if len(reports) != 0 {
				t.Fatalf("writer %d, which lost, left %d reports", i, len(reports))
			}
		}
	})
}

// A complaint reads back as written, with and without its sealed copy, and a
// complaint id written twice is one row.
func TestSmtpComplaintReadsBackAsWritten(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		networkId := sidecar.NewId()
		arrival := time.Date(2026, time.November, 3, 10, 0, 0, 0, time.UTC)
		sealedComplaint := smtpTestComplaint(networkId, arrival)
		// an aggregate's sealed copy rides a row of type rua, with only its reporter and domain
		ruaRow := &smtpedgeservice.SmtpComplaint{
			ComplaintId:  sidecar.NewId(),
			NetworkId:    networkId,
			Reporter:     "reporter.example",
			Type:         smtpedgeservice.SmtpComplaintTypeRua,
			ArrivalTime:  arrival.Add(time.Minute),
			Domain:       "quietnet.root.example",
			SealedReport: []byte{1, 2, 3},
		}
		// role mail keeps no reporter, and with no mail key no sealed copy
		roleRow := &smtpedgeservice.SmtpComplaint{
			ComplaintId: sidecar.NewId(),
			NetworkId:   networkId,
			Type:        smtpedgeservice.SmtpComplaintTypeRole,
			ArrivalTime: arrival.Add(2 * time.Minute),
			Domain:      "quietnet.root.example",
			JoinResult:  "medium",
		}
		for _, complaint := range []*smtpedgeservice.SmtpComplaint{sealedComplaint, ruaRow, roleRow, sealedComplaint} {
			if err := store.InsertComplaint(ctx, complaint); err != nil {
				t.Fatal(err)
			}
		}
		complaints := GetSmtpComplaints(ctx, server.Id(networkId), time.Time{})
		if len(complaints) != 3 {
			t.Fatalf("%d complaints", len(complaints))
		}
		for i, want := range []*smtpedgeservice.SmtpComplaint{sealedComplaint, ruaRow, roleRow} {
			got := complaints[i]
			if got.ComplaintId != want.ComplaintId || got.NetworkId != want.NetworkId || got.Reporter != want.Reporter ||
				got.Type != want.Type || !got.ArrivalTime.Equal(want.ArrivalTime) || got.Domain != want.Domain ||
				got.DkimDomain != want.DkimDomain || got.DkimSelector != want.DkimSelector || got.DkimResult != want.DkimResult ||
				!bytes.Equal(got.MessageIdHash, want.MessageIdHash) || got.XUrRef != want.XUrRef || got.FeedbackId != want.FeedbackId ||
				got.JoinResult != want.JoinResult || !bytes.Equal(got.SealedReport, want.SealedReport) {
				t.Fatalf("complaint %d reads back as %+v", i, got)
			}
		}
		if complaints[2].SealedReport != nil {
			t.Fatal("a complaint with no sealed copy reads back with one")
		}
		// since is inclusive, and later rows only
		if later := GetSmtpComplaints(ctx, server.Id(networkId), arrival.Add(time.Minute)); len(later) != 2 {
			t.Fatalf("%d complaints from the second's arrival", len(later))
		}
	})
}

// A row a column cannot hold is refused whole, and nothing of it is stored: a
// NUL in a report id (JSON may carry one), a name past its column, invalid
// UTF-8, a missing network, a type outside a column, no counts.
func TestSmtpReportRowsRefuseWhatAColumnCannotHold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		networkId := sidecar.NewId()
		aggregates := map[string]func(report *smtpedgeservice.SmtpAggregateReport){
			"a NUL in the report id":    func(report *smtpedgeservice.SmtpAggregateReport) { report.ReportId = "a\x00b" },
			"a report id past 1024":     func(report *smtpedgeservice.SmtpAggregateReport) { report.ReportId = strings.Repeat("r", 1025) },
			"no reporter":               func(report *smtpedgeservice.SmtpAggregateReport) { report.Reporter = "" },
			"an unknown kind":           func(report *smtpedgeservice.SmtpAggregateReport) { report.Kind = "ruf" },
			"no counts":                 func(report *smtpedgeservice.SmtpAggregateReport) { report.Counts = nil },
			"a count with no network":   func(report *smtpedgeservice.SmtpAggregateReport) { report.Counts[0].NetworkId = sidecar.Id{} },
			"a header_from past 253":    func(report *smtpedgeservice.SmtpAggregateReport) { report.Counts[0].Domain = strings.Repeat("d", 254) },
			"invalid UTF-8 in a domain": func(report *smtpedgeservice.SmtpAggregateReport) { report.Counts[0].Domain = "\xff.example" },
		}
		for name, change := range aggregates {
			report := smtpTestAggregate("reporter.example", smtpTestReportId(), networkId, 1)
			change(report)
			inserted, err := store.InsertAggregate(ctx, report)
			if inserted || !errors.Is(err, ErrSmtpRowRefused) {
				t.Errorf("%s: %t, %v", name, inserted, err)
			}
		}
		complaints := map[string]func(complaint *smtpedgeservice.SmtpComplaint){
			"no complaint id":               func(complaint *smtpedgeservice.SmtpComplaint) { complaint.ComplaintId = sidecar.Id{} },
			"no network":                    func(complaint *smtpedgeservice.SmtpComplaint) { complaint.NetworkId = sidecar.Id{} },
			"no type":                       func(complaint *smtpedgeservice.SmtpComplaint) { complaint.Type = "" },
			"a Feedback-ID past 128":        func(complaint *smtpedgeservice.SmtpComplaint) { complaint.FeedbackId = strings.Repeat("f", 129) },
			"a NUL in the X-UR-Ref":         func(complaint *smtpedgeservice.SmtpComplaint) { complaint.XUrRef = "1.a\x00" },
			"a Message-ID hash of 31":       func(complaint *smtpedgeservice.SmtpComplaint) { complaint.MessageIdHash = make([]byte, 31) },
			"a selector past one label":     func(complaint *smtpedgeservice.SmtpComplaint) { complaint.DkimSelector = strings.Repeat("s", 64) },
			"invalid UTF-8 in the reporter": func(complaint *smtpedgeservice.SmtpComplaint) { complaint.Reporter = "\xc3" },
		}
		for name, change := range complaints {
			complaint := smtpTestComplaint(networkId, server.NowUtc())
			change(complaint)
			if err := store.InsertComplaint(ctx, complaint); !errors.Is(err, ErrSmtpRowRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if reports := GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{}); len(reports) != 0 {
			t.Fatalf("refused aggregates left %d reports", len(reports))
		}
		if stored := GetSmtpComplaints(ctx, server.Id(networkId), time.Time{}); len(stored) != 0 {
			t.Fatalf("refused complaints left %d rows", len(stored))
		}
	})
}

// A count past a bigint, which saturating sums can reach, holds at the
// largest bigint rather than failing the report or wrapping negative.
func TestSmtpAggregateCountsPastABigintHoldAtTheLargest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		networkId := sidecar.NewId()
		report := smtpTestAggregate("reporter.example", smtpTestReportId(), networkId, math.MaxUint64)
		// a second count for the same row, summed into it
		report.Counts = append(report.Counts, &smtpedgeservice.SmtpAggregateCount{NetworkId: networkId, Domain: "quietnet.root.example", MessageCount: 7})
		inserted, err := InsertSmtpAggregate(ctx, report)
		if err != nil || !inserted {
			t.Fatalf("%t, %v", inserted, err)
		}
		reports := GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{})
		if len(reports) != 1 || len(reports[0].Counts) != 1 || reports[0].Counts[0].MessageCount != math.MaxInt64 || reports[0].Counts[0].DkimPassCount != math.MaxInt64 {
			t.Fatalf("the counts read back as %+v", reports)
		}
	})
}

// Each report column, with the row of §2.8's list that shows it and words of
// that row naming it (I-18). §22.7's guard (sidecar_schema_guard_test.go) maps
// columns to rows through local/sidecar-columns.yml; this holds the words too.
var smtpReportColumnRows = map[string][2]string{
	"smtp_complaint.complaint_id":    {"smtp-5", "reports parsed header-only"},
	"smtp_complaint.network_id":      {"smtp-5", "per network"},
	"smtp_complaint.reporter":        {"smtp-5", "reporter"},
	"smtp_complaint.complaint_type":  {"smtp-5", "type"},
	"smtp_complaint.arrival_time":    {"smtp-5", "arrival"},
	"smtp_complaint.domain":          {"smtp-5", "domain"},
	"smtp_complaint.dkim_domain":     {"smtp-5", "DKIM d= and s= with result"},
	"smtp_complaint.dkim_selector":   {"smtp-5", "DKIM d= and s= with result"},
	"smtp_complaint.dkim_result":     {"smtp-5", "DKIM d= and s= with result"},
	"smtp_complaint.message_id_hash": {"smtp-5", "Message-ID hash"},
	"smtp_complaint.x_ur_ref":        {"smtp-5", "X-UR-Ref"},
	"smtp_complaint.feedback_id":     {"smtp-5", "Feedback-ID"},
	"smtp_complaint.join_result":     {"smtp-5", "join result"},
	"smtp_complaint.sealed_report":   {"smtp-5", "sealed report copies"},

	"smtp_dmarc_daily.network_id":       {"smtp-5", "per network, day and reporter"},
	"smtp_dmarc_daily.day":              {"smtp-5", "per network, day and reporter"},
	"smtp_dmarc_daily.reporter":         {"smtp-5", "per network, day and reporter"},
	"smtp_dmarc_daily.header_from":      {"smtp-5", "header_from domains"},
	"smtp_dmarc_daily.report_id":        {"smtp-5", "each report_id counted once"},
	"smtp_dmarc_daily.policy_type":      {"smtp-5", "TLS-RPT aggregates"},
	"smtp_dmarc_daily.kind":             {"smtp-5", "RUA and TLS-RPT aggregates"},
	"smtp_dmarc_daily.arrival_time":     {"smtp-5", "arrival"},
	"smtp_dmarc_daily.message_count":    {"smtp-5", "counts and results"},
	"smtp_dmarc_daily.dkim_pass_count":  {"smtp-5", "counts and results"},
	"smtp_dmarc_daily.spf_pass_count":   {"smtp-5", "counts and results"},
	"smtp_dmarc_daily.quarantine_count": {"smtp-5", "counts and results"},
	"smtp_dmarc_daily.reject_count":     {"smtp-5", "counts and results"},
	"smtp_dmarc_daily.success_count":    {"smtp-5", "counts and results"},
	"smtp_dmarc_daily.failure_count":    {"smtp-5", "counts and results"},
}

// Every column of the given tables, table.column.
func smtpSchemaColumns(ctx context.Context, tables []string) map[string]bool {
	columns := map[string]bool{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT table_name || '.' || column_name
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ANY($1)
		`, tables)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var column string
				server.Raise(result.Scan(&column))
				columns[column] = true
			}
		})
	})
	return columns
}

// Checks every column of tables against columnRows: a row whose store names
// the table and whose words name the column, and no mapped column missing.
func smtpCheckColumnRows(t testing.TB, listBytes []byte, tables []string, columnRows map[string][2]string) {
	columns := smtpSchemaColumns(context.Background(), tables)
	for column := range columns {
		mapped, ok := columnRows[column]
		if !ok {
			t.Errorf("%s maps to no row of §2.8's list", column)
			continue
		}
		row := tlsMetadataRowText(listBytes, mapped[0])
		table, _, _ := strings.Cut(column, ".")
		if !strings.Contains(row, table) {
			t.Errorf("%s: row %s does not store %s", column, mapped[0], table)
		}
		if !strings.Contains(row, mapped[1]) {
			t.Errorf("%s: row %s does not say %q", column, mapped[0], mapped[1])
		}
	}
	for column := range columnRows {
		if !columns[column] {
			t.Errorf("%s is mapped but does not exist", column)
		}
	}
}

// Every column of the report tables maps to smtp-5, whose store names the
// table and whose words name the column.
func TestSmtpReportColumnsMapToMetadataRows(t *testing.T) {
	listPath := "../../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		smtpCheckColumnRows(t, listBytes, smtpReportTables, smtpReportColumnRows)
	})
}
