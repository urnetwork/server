package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

type legacyTargetSqlEntry struct {
	UserID     string             `json:"user_id"`
	QueryID    string             `json:"query_id"`
	TopLevel   bool               `json:"top_level"`
	Query      string             `json:"query"`
	QueryBytes int                `json:"query_bytes"`
	StatsSince *string            `json:"stats_since"`
	Metrics    map[string]float64 `json:"metrics"`
}

type legacyTargetSqlState struct {
	Version     string                 `json:"version"`
	DatabaseOID string                 `json:"database_oid"`
	Reset       float64                `json:"reset_epoch"`
	Dealloc     float64                `json:"dealloc"`
	Postmaster  float64                `json:"postmaster_epoch"`
	Track       string                 `json:"track"`
	Utility     string                 `json:"utility"`
	Entries     []legacyTargetSqlEntry `json:"entries"`
}

type legacyTargetSqlWork struct {
	legacyTargetSqlEntry
	Category string `json:"category"`
	Family   string `json:"family"`
}

type legacyTargetSqlWorkDelta struct {
	Version        string                        `json:"extension_version"`
	Track          string                        `json:"track"`
	TopLevelTotals map[string]float64            `json:"top_level_totals"`
	Totals         map[string]float64            `json:"totals"`
	Categories     map[string]map[string]float64 `json:"categories"`
	Families       map[string]map[string]float64 `json:"top_level_families"`
	Statements     []legacyTargetSqlWork         `json:"statements"`
}

// Snapshots are taken only while fixture workers are joined. A single statement
// captures every counter and its lifetime. That statement excludes itself.
// A normalized capability lookup can remain and is explicitly classified as
// a measurement probe; no statement statistics are reset.
func legacyTargetSqlSnapshot(t testing.TB, ctx context.Context) *legacyTargetSqlState {
	t.Helper()
	var value *legacyTargetSqlState
	server.Db(ctx, func(conn server.PgConn) {
		var available bool
		server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('pg_stat_statements') IS NOT NULL`).Scan(&available))
		if !available {
			if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
				t.Fatal("native performance gate requires isolated pg_stat_statements")
			}
			return
		}
		var raw []byte
		server.Raise(conn.QueryRow(ctx, `WITH entries AS MATERIALIZED (
			SELECT userid::text AS user_id, queryid::text AS query_id, toplevel AS top_level,
			left(query,16384) AS query,octet_length(query) AS query_bytes,
			to_jsonb(statements)->>'stats_since' AS stats_since,
			jsonb_build_object('calls',calls,'rows',rows,'execution_ms',total_exec_time,
			'shared_hit_blocks',shared_blks_hit,'shared_read_blocks',shared_blks_read,
			'shared_dirtied_blocks',shared_blks_dirtied,'shared_written_blocks',shared_blks_written,
			'local_hit_blocks',local_blks_hit,'temp_read_blocks',temp_blks_read,'temp_written_blocks',temp_blks_written,
			'normalized_query_text_bytes_times_calls',octet_length(query)::numeric*calls) AS metrics
			FROM pg_stat_statements AS statements
			WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND query NOT LIKE '%pg_stat_statements%'
			ORDER BY userid,queryid,toplevel LIMIT 8193
		) SELECT jsonb_build_object(
			'version',(SELECT extversion FROM pg_extension WHERE extname='pg_stat_statements'),
			'database_oid',(SELECT oid::text FROM pg_database WHERE datname=current_database()),
			'reset_epoch',extract(epoch FROM info.stats_reset),'dealloc',info.dealloc,
			'postmaster_epoch',extract(epoch FROM pg_postmaster_start_time()),
			'track',current_setting('pg_stat_statements.track'),'utility',current_setting('pg_stat_statements.track_utility'),
			'entries',COALESCE((SELECT jsonb_agg(to_jsonb(entries)) FROM entries),'[]'::jsonb))
			FROM pg_stat_statements_info AS info`).Scan(&raw))
		if len(raw) > 32*1024*1024 {
			t.Fatal("bounded SQL statement snapshot exceeded its byte cap")
		}
		value = &legacyTargetSqlState{}
		server.Raise(json.Unmarshal(raw, value))
		if (value.Version != "1.10" && value.Version != "1.12") || len(value.Entries) > 8192 || value.DatabaseOID == "" || value.Reset <= 0 || value.Postmaster <= 0 || value.Dealloc < 0 || (value.Track != "top" && value.Track != "all") || value.Utility != "on" {
			t.Fatal("unqualified SQL statement snapshot lifetime, capability or row cap")
		}
	})
	return value
}

func legacyTargetSqlDelta(t testing.TB, before, after *legacyTargetSqlState) *legacyTargetSqlWorkDelta {
	t.Helper()
	if before == nil && after == nil {
		return nil
	}
	if before == nil || after == nil || before.Version != after.Version || before.DatabaseOID != after.DatabaseOID || before.Reset != after.Reset || before.Dealloc != after.Dealloc || before.Postmaster != after.Postmaster || before.Track != after.Track || before.Utility != after.Utility {
		t.Fatal("isolated SQL statement measurement lifetime changed")
	}
	key := func(entry legacyTargetSqlEntry) string {
		return fmt.Sprintf("%s/%s/%t", entry.UserID, entry.QueryID, entry.TopLevel)
	}
	prior := map[string]legacyTargetSqlEntry{}
	for _, entry := range before.Entries {
		k := key(entry)
		if _, found := prior[k]; found {
			t.Fatal("duplicate SQL fingerprint before measurement")
		}
		prior[k] = entry
	}
	out := &legacyTargetSqlWorkDelta{Version: after.Version, Track: after.Track, Totals: map[string]float64{}, TopLevelTotals: map[string]float64{}, Categories: map[string]map[string]float64{}, Families: map[string]map[string]float64{}}
	add := func(dst map[string]float64, src map[string]float64) {
		for k, v := range src {
			dst[k] += v
		}
	}
	seen := map[string]bool{}
	for _, entry := range after.Entries {
		k := key(entry)
		if seen[k] {
			t.Fatal("duplicate SQL fingerprint after measurement")
		}
		seen[k] = true
		old, found := prior[k]
		if after.Version == "1.12" && (entry.StatsSince == nil || (found && (old.StatsSince == nil || *entry.StatsSince != *old.StatsSince))) {
			t.Fatal("SQL fingerprint selective-reset lifetime changed or missing")
		}
		if found && (entry.Query != old.Query || entry.QueryBytes != old.QueryBytes || len(entry.Metrics) != len(old.Metrics)) {
			t.Fatal("SQL fingerprint text or counter schema changed")
		}
		delta := map[string]float64{}
		for name, v := range entry.Metrics {
			if v < old.Metrics[name] {
				t.Fatal("SQL fingerprint cumulative counter regressed", name)
			}
			delta[name] = v - old.Metrics[name]
		}
		if delta["calls"] == 0 {
			continue
		}
		if entry.QueryBytes != len(entry.Query) || delta["calls"] < 0 || len(out.Statements) >= 512 {
			t.Fatal("changed SQL fingerprint exceeded a retained text or row bound")
		}
		entry.Metrics = delta
		category, family := legacyTargetSqlFamily(entry.Query)
		if !entry.TopLevel {
			category = "nested_sql"
		}
		if out.Categories[category] == nil {
			out.Categories[category] = map[string]float64{}
		}
		add(out.Categories[category], delta)
		if entry.TopLevel {
			add(out.TopLevelTotals, delta)
			if out.Families[family] == nil {
				out.Families[family] = map[string]float64{}
			}
			add(out.Families[family], delta)
		}
		add(out.Totals, delta)
		out.Statements = append(out.Statements, legacyTargetSqlWork{legacyTargetSqlEntry: entry, Category: category, Family: family})
	}
	for k := range prior {
		if !seen[k] {
			t.Fatal("SQL statement inventory lost a prior fingerprint")
		}
	}
	sort.Slice(out.Statements, func(i, j int) bool {
		return key(out.Statements[i].legacyTargetSqlEntry) < key(out.Statements[j].legacyTargetSqlEntry)
	})
	return out
}

// PGSS1.12 binds each fingerprint stats_since. Version1.10 lacks that field
// and additionally relies on the isolated runner excluding selective resets.
// Classification is descriptive; every changed raw normalized statement stays
// in the result, including unclassified or nested work. Precise ownership and
// financial families additionally have call/row invariants in the controls.
func legacyTargetSqlFamily(query string) (string, string) {
	s := strings.ToLower(strings.Join(strings.Fields(query), " "))
	c := strings.ReplaceAll(s, " ", "")
	switch {
	case strings.HasPrefix(s, "begin"), strings.HasPrefix(s, "commit"), strings.HasPrefix(s, "rollback"), strings.HasPrefix(s, "savepoint"), strings.HasPrefix(s, "release savepoint"):
		return "transaction", "transaction"
	case strings.HasPrefix(s, "set "), strings.HasPrefix(s, "reset "), strings.HasPrefix(s, "show "):
		return "session_and_timeout", "session_and_timeout"
	case strings.HasPrefix(c, "selectto_regclass("):
		return "fixture_observer", "measurement_probe"
	case strings.HasPrefix(c, "selectcount(*)fromlegacy_settlement_intentwherecontract_id=any("), strings.HasPrefix(c, "selectcount(*)frompending_taskwherefunction_name="), strings.HasPrefix(c, "selectcount(*)fromfinished_taskwheretask_id=any("), strings.HasPrefix(c, "updatepending_tasksetrun_at=$1,release_time=$1wherefunction_name=$2"):
		return "fixture_observer", "fixture_observer"
	case strings.HasPrefix(c, "selectnext_attempt_time,contract_id,") && strings.Contains(c, "fromlegacy_settlement_intent"):
		return "due_selection", "due_selection"
	case strings.HasPrefix(c, "selectoutcome,clear_disputefromlegacy_settlement_intentwherecontract_id=$1forupdateskiplocked"):
		return "ownership", "intent_ownership"
	case strings.HasPrefix(c, "selectoutcomeisnotnullfromtransfer_contractwherecontract_id=$1forupdateskiplocked"):
		return "ownership", "contract_ownership"
	case strings.HasPrefix(c, "selectcount(*)fromunnest(array[$1::uuid])asrequested_contract") && strings.Contains(c, "transfer_escrow") && strings.Contains(c, "transfer_balance"):
		return "ownership", "grant_membership"
	case strings.HasPrefix(c, "selectbalance.balance_idfromtransfer_balanceasbalanceinnerjointransfer_escrowasescrowusing(balance_id)whereescrow.contract_id=$1"):
		return "ownership", "grant_ownership"
	case strings.HasPrefix(c, "updatetransfer_contractsetoutcome="):
		return "accounting", "outcome_write"
	case strings.HasPrefix(c, "updatetransfer_balancesetbalance_byte_count=balance_byte_count-"):
		return "accounting", "grant_debit"
	case strings.Contains(c, "transfer_escrow_sweep"), strings.Contains(c, "transfer_debit_journal"), strings.Contains(c, "account_balance"):
		return "accounting", "accounting_other"
	case strings.Contains(c, "pending_task"), strings.Contains(c, "finished_task"):
		return "durable_task", "durable_task"
	case strings.Contains(c, "net_escrow"):
		return "escrow_mirror", "escrow_mirror"
	case strings.Contains(c, "contract_close"), strings.Contains(c, "contract_party"), strings.Contains(c, "transfer_contract"), strings.Contains(c, "transfer_escrow"), strings.Contains(c, "transfer_balance"), strings.Contains(c, "network_client"), strings.Contains(c, "stream"):
		return "financial_and_participant", "financial_and_participant_other"
	case strings.Contains(c, "legacy_settlement_intent"):
		return "intent_maintenance", "intent_maintenance"
	default:
		return "unclassified", "unclassified"
	}
}

func legacyTargetRequireSqlFamily(t testing.TB, delta *legacyTargetSqlWorkDelta, family string, calls, rows float64) {
	t.Helper()
	if delta == nil {
		t.Fatal("query-work control requires statement statistics")
	}
	actual := delta.Families[family]
	if actual["calls"] != calls || actual["rows"] != rows {
		t.Fatalf("query-work family %s: calls=%v rows=%v, want calls=%v rows=%v", family, actual["calls"], actual["rows"], calls, rows)
	}
}
