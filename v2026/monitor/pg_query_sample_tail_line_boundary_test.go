// Check exact line framing while retaining every preexisting SQL body byte.
package monitor

import (
	"fmt"
	"strings"
	"testing"
)

// The frozen pre-marker generator is an independent SQL-body/order oracle.
func pgTailLegacySqlFixture(database string) string {
	var b strings.Builder
	b.WriteString("-- monitor_query_sample_phase:identity\n")
	b.WriteString("SELECT json_build_object('kind','identity','sample',0,'at',extract(epoch FROM clock_timestamp()),'primary',NOT pg_is_in_recovery(),'read_only',current_setting('transaction_read_only')='on','track_activity_query_size',pg_size_bytes(current_setting('track_activity_query_size')),'pgss_version',(SELECT extversion FROM pg_extension WHERE extname='pg_stat_statements'),'database_ok',current_database()=" + "'" + strings.ReplaceAll(database, "'", "''") + "');\n")
	b.WriteString("-- monitor_query_sample_phase:authority\n")
	b.WriteString("DO $guard$ BEGIN IF pg_is_in_recovery() OR current_setting('transaction_read_only') <> 'on' OR current_database() <> '" + strings.ReplaceAll(database, "'", "''") + "' THEN RAISE EXCEPTION 'sample authority mismatch'; END IF; END $guard$;\n")
	b.WriteString("-- monitor_query_sample_phase:history_start\n")
	b.WriteString(pgSampleHistorySQL(0))
	for i := 0; i < pgQuerySampleCount; i++ {
		if i > 0 {
			b.WriteString("\n-- monitor_query_sample_phase:sample_wait\n")
			b.WriteString("SELECT pg_sleep(2);\n")
		}
		b.WriteString("\n-- monitor_query_sample_phase:activity\n")
		b.WriteString("SELECT pg_stat_clear_snapshot();\n")
		b.WriteString(pgSampleActivitySQL(i))
	}
	b.WriteString("\n-- monitor_query_sample_phase:blockers\n")
	b.WriteString("SELECT pg_stat_clear_snapshot();\n")
	b.WriteString(pgSampleBlockersSQL())
	b.WriteString("\n-- monitor_query_sample_phase:history_end\n")
	b.WriteString(pgSampleHistorySQL(1))
	return b.String()
}

// Returned query markers must begin on their own line even when the query
// template ends in whitespace. Removing complete marker insertions restores
// the exact pre-marker SQL, not merely equal query or sleep counts.
func TestPgTailSqlMarkersKeepLineBoundaryAndLegacyBody(t *testing.T) {
	const codes = "wWcCaAwWcCaAcCbBhH"
	const marker = "\\echo monitor-pg-tail __MONITOR_PG_TAIL_NONCE__ "
	sql := pgQuerySampleSQL("fixture")
	plain := sql
	markers := 0
	for _, line := range strings.Split(sql, "\n") {
		if strings.Contains(line, marker) {
			if !strings.HasPrefix(line, marker) {
				t.Fatal("generated marker did not begin at its own line boundary")
			}
			markers++
		}
	}
	for sequence := 0; sequence < len(codes); sequence++ {
		index := 10
		if sequence >= 6 {
			index = 11
		}
		if sequence >= 12 {
			index = 12
		}
		record := fmt.Sprintf("\n%s%02d %c %02d\n", marker, sequence, codes[sequence], index)
		if strings.Count(plain, record) != 1 {
			t.Fatal("generated marker insertion was not exact and unique")
		}
		plain = strings.Replace(plain, record, "", 1)
	}
	if markers != 18 || plain != pgTailLegacySqlFixture("fixture") {
		t.Fatal("marker framing changed SQL body, order or membership")
	}
	for _, boundary := range []struct {
		sequence int
		code     byte
		index    int
	}{{sequence: 5, code: 'A', index: 10}, {sequence: 11, code: 'A', index: 11}, {sequence: 15, code: 'B', index: 12}, {sequence: 17, code: 'H', index: 12}} {
		record := fmt.Sprintf("\n \n%s%02d %c %02d\n", marker, boundary.sequence, boundary.code, boundary.index)
		if !strings.Contains(sql, record) {
			t.Fatal("trailing-space SQL return boundary was not exercised")
		}
	}
}
