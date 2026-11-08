package model

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// §21's seven tables (ABUSE-M5-03), in either build: absent from a binary that
// does not link MG5, present in one built with sidecar_pending, and in that one
// holding no address or body and mapping to §2.8's api-14.

// §21.16's tables, exactly seven.
var abuseSchemaTables = []string{
	"abuse_case",
	"abuse_action",
	"abuse_appeal",
	"network_abuse_state",
	"abuse_legal_hold",
	"abuse_access_log",
	"legal_request",
}

// Each table's columns, as the migrated database has them.
func abuseSchemaColumns(ctx context.Context) map[string]bool {
	columns := map[string]bool{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT table_name || '.' || column_name
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ANY($1)
		`, abuseSchemaTables)
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

// Which of the seven tables the database holds.
func abuseSchemaPresentTables(ctx context.Context) []string {
	present := []string{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, table := range abuseSchemaTables {
			var found *string
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&found))
			if found != nil {
				present = append(present, table)
			}
		}
	})
	return present
}

// The group is absent without the tag and present with it, every table at once,
// and applying the migrations again changes nothing.
func TestAbuseSchemaFollowsTheTag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		present := abuseSchemaPresentTables(ctx)
		if !server.SidecarMg5Linked() {
			if len(present) != 0 {
				t.Fatalf("a binary that does not link MG5 has %v", present)
			}
			return
		}
		if !slices.Equal(present, abuseSchemaTables) {
			t.Fatalf("a binary that links MG5 has %v of %v", present, abuseSchemaTables)
		}

		version := server.DbVersion(ctx)
		columns := abuseSchemaColumns(ctx)
		server.ApplyDbMigrations(ctx)
		if got := server.DbVersion(ctx); got != version {
			t.Fatalf("a re-run moved the version from %d to %d", version, got)
		}
		if again := abuseSchemaColumns(ctx); len(again) != len(columns) {
			t.Fatalf("a re-run changed the columns: %d, then %d", len(columns), len(again))
		}
	})
}

// No column name reads as a client address, a body or a reporter's contact
// (I-13, §21.8: third-party data dies at the join). QD-141 will add a reported
// URL's path, kept only until a case's first review, with the intake task
// (ABUSE-M5-09); it names its own column and ttl in api-14 then.
func TestAbuseSchemaHoldsNoAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		pattern := regexp.MustCompile(`(^|_)(ip|ips|addr|address|addresses|client_address|host_address|email|phone|user_auth|mail_from|sender|recipient|body|bodies|text|content|message|header|headers|subject|url|path|query)($|_)`)
		columns := abuseSchemaColumns(context.Background())
		if len(columns) == 0 {
			t.Fatal("no columns read")
		}
		for column := range columns {
			_, name, _ := strings.Cut(column, ".")
			if pattern.MatchString(name) {
				t.Errorf("%s reads as an address or a body", column)
			}
		}
	})
}

// A column api-14 has no words for yet, and the decided question that gives it
// them at the next records pass. The test reports these, and fails once the row
// says the words, so the mark comes out.
type abuseSchemaPendingWords struct {
	words      string
	resolution string
}

// The words of api-14 each column shows under (§2.8, I-18), which the guard
// finds in the row's text. TEST-M1-03's full guard is not on server yet; this
// keeps the seven tables from growing a column without a row in the meantime.
var abuseSchemaColumnWords = map[string]string{
	"abuse_case.case_id":         "abuse cases",
	"abuse_case.network_id":      "abuse cases",
	"abuse_case.category":        "category",
	"abuse_case.target_kind":     "target host, label or network",
	"abuse_case.target":          "target host, label or network",
	"abuse_case.reporter_class":  "reporter class",
	"abuse_case.join_kind":       "join keys",
	"abuse_case.join_key":        "join keys",
	"abuse_case.state":           "abuse cases",
	"abuse_case.barred":          "abuse cases",
	"abuse_case.review_due_time": "abuse cases",
	"abuse_case.create_time":     "abuse cases",
	"abuse_case.close_time":      "abuse cases",

	"abuse_action.action_id":     "actions",
	"abuse_action.case_id":       "actions",
	"abuse_action.network_id":    "actions",
	"abuse_action.category":      "category",
	"abuse_action.kind":          "kills, halts, suspensions, cap tightening",
	"abuse_action.target":        "target host, label or network",
	"abuse_action.until_time":    "actions",
	"abuse_action.by_machine":    "actions",
	"abuse_action.review_state":  "reviewers' reasons",
	"abuse_action.review_reason": "reviewers' reasons",
	"abuse_action.review_time":   "reviewers' reasons",
	"abuse_action.applied_time":  "actions",
	"abuse_action.mailed_time":   "actions",
	"abuse_action.create_time":   "actions",

	"abuse_appeal.appeal_id":       "appeals",
	"abuse_appeal.case_id":         "appeals",
	"abuse_appeal.network_id":      "appeals",
	"abuse_appeal.user_id":         "appeals",
	"abuse_appeal.reason":          "appeals",
	"abuse_appeal.state":           "appeals",
	"abuse_appeal.decision_reason": "reviewers' reasons",
	"abuse_appeal.create_time":     "appeals",
	"abuse_appeal.decide_time":     "appeals",

	"network_abuse_state.network_id":  "each network's standing",
	"network_abuse_state.state":       "each network's standing",
	"network_abuse_state.case_id":     "each network's standing",
	"network_abuse_state.until_time":  "each network's standing",
	"network_abuse_state.update_time": "each network's standing",

	"abuse_legal_hold.network_id":  "holds (network",
	"abuse_legal_hold.case_id":     "case",
	"abuse_legal_hold.kind":        "kind",
	"abuse_legal_hold.scope":       "scope",
	"abuse_legal_hold.expire_time": "expiry",
	"abuse_legal_hold.copied_rows": "copied_rows",

	"abuse_access_log.log_id":      "every abuse-role read",
	"abuse_access_log.lookup_kind": "every abuse-role read",
	"abuse_access_log.case_id":     "every abuse-role read",
	"abuse_access_log.access_time": "every abuse-role read",

	"legal_request.request_id":       "legal requests",
	"legal_request.case_id":          "legal requests",
	"legal_request.instrument":       "instrument",
	"legal_request.country":          "country",
	"legal_request.network_ids":      "accounts",
	"legal_request.sealed":           "sealing orders",
	"legal_request.seal_until":       "sealing orders",
	"legal_request.due_time":         "legal requests",
	"legal_request.production_state": "productions",
	"legal_request.create_time":      "legal requests",
	"legal_request.close_time":       "legal requests",
}

// The columns whose words wait on a records pass, by the decided entry that
// gives them.
var abuseSchemaPendingColumns = map[string]abuseSchemaPendingWords{
	"abuse_legal_hold.part":        {words: "part", resolution: "plan-ABUSE-metadata-rows #2 and #3 (QD-058, QD-457)"},
	"abuse_legal_hold.place_time":  {words: "place_time", resolution: "plan-ABUSE-metadata-rows #2 (QD-058)"},
	"abuse_legal_hold.report_time": {words: "report_time", resolution: "plan-ABUSE-metadata-rows #2 (QD-058)"},
	"abuse_legal_hold.renew_time":  {words: "renew_time", resolution: "plan-ABUSE-metadata-rows #2 (QD-058)"},
	"abuse_legal_hold.copy_state":  {words: "copy_state", resolution: "plan-ABUSE-metadata-rows #2 (QD-058)"},
	"abuse_action.reviewer_id":     {words: "a reviewer id", resolution: "plan-ABUSE-metadata-rows #6 (QD-054)"},
	"abuse_appeal.reviewer_id":     {words: "a reviewer id", resolution: "plan-ABUSE-metadata-rows #6 (QD-054)"},
	"abuse_access_log.reviewer_id": {words: "a reviewer id", resolution: "plan-ABUSE-metadata-rows #6 (QD-054)"},
	"legal_request.legal_signoff_id": {
		words:      "sign-off",
		resolution: "abuse-m5-03-signoff-ids, beside plan-ABUSE-metadata-rows #6",
	},
	"legal_request.engineering_signoff_id": {
		words:      "sign-off",
		resolution: "abuse-m5-03-signoff-ids, beside plan-ABUSE-metadata-rows #6",
	},
}

// A row of §2.8's list: its id line and the indented lines under it, or "".
func abuseSchemaRowText(listBytes []byte, rowId string) string {
	lines := strings.Split(string(listBytes), "\n")
	for i, line := range lines {
		if line != rowId+":" {
			continue
		}
		row := []string{}
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "  ") {
				break
			}
			row = append(row, next)
		}
		return strings.Join(row, "\n")
	}
	return ""
}

// §22.7's guard for the seven tables: api-14 stores each, every column maps to
// words of its row or waits on a decided records pass, and every mapped column
// exists.
func TestAbuseSchemaColumnsMapToApi14(t *testing.T) {
	listPath := "../../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	row := abuseSchemaRowText(listBytes, "api-14")
	if row == "" {
		t.Fatal("§2.8 has no api-14")
	}
	for _, table := range abuseSchemaTables {
		if !strings.Contains(row, table) {
			t.Errorf("api-14 does not store %s", table)
		}
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		columns := abuseSchemaColumns(context.Background())
		for column := range columns {
			if words, ok := abuseSchemaColumnWords[column]; ok {
				if !strings.Contains(row, words) {
					t.Errorf("%s: api-14 does not say %q", column, words)
				}
				continue
			}
			pending, ok := abuseSchemaPendingColumns[column]
			if !ok {
				t.Errorf("%s maps to no words of api-14", column)
				continue
			}
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(pending.words) + `\b`).MatchString(row) {
				t.Errorf("%s: api-14 now says %q, so its pending mark (%s) comes out", column, pending.words, pending.resolution)
				continue
			}
			t.Logf("%s waits for api-14's %q on %s", column, pending.words, pending.resolution)
		}
		for column := range abuseSchemaColumnWords {
			if !columns[column] {
				t.Errorf("%s is mapped but does not exist", column)
			}
		}
		for column := range abuseSchemaPendingColumns {
			if !columns[column] {
				t.Errorf("%s is pending but does not exist", column)
			}
		}
	})
}
