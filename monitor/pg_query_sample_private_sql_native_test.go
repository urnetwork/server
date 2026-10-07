// Run the exact owning SQL against populated, connection-local catalog shadows.
// No production catalog or application row is read or changed by these controls.
package monitor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// This is the same catalog-shadow seam used by the history SQL regression.
func pgSampleSqlNativeFixture(t testing.TB, conn server.PgConn) int {
	t.Helper()
	var trackBytes int
	server.Raise(conn.QueryRow(t.Context(), `SELECT pg_size_bytes(current_setting('track_activity_query_size'))`).Scan(&trackBytes))
	if trackBytes < 16384 || trackBytes > 1048576 {
		t.Fatal("native fixture requires track_activity_query_size at least 16kB")
	}
	server.RaisePgResult(conn.Exec(t.Context(), `CREATE TEMP TABLE pg_stat_activity (
 pid int,query_id bigint,query text,state text,wait_event text,wait_event_type text,
 client_addr inet,application_name text,backend_type text,datid oid,
 query_start timestamptz,xact_start timestamptz,state_change timestamptz)`))
	return trackBytes
}

// Validate source shape before accessing its private cells, preserving a causal
// failure when the exact baseline SELECT still emits only family/token data.
func pgSampleSqlNativeRead(t testing.TB, conn server.PgConn) pgSampleWire {
	t.Helper()
	var raw []byte
	server.Raise(conn.QueryRow(t.Context(), pgSampleActivitySQL(0)).Scan(&raw))
	var frame pgSampleWire
	if json.Unmarshal(raw, &frame) != nil || frame.Kind != "activity" || frame.SqlCaptureVersion == nil || *frame.SqlCaptureVersion != 1 {
		t.Fatal("existing activity SELECT lost the private SQL source discriminator")
	}
	return frame
}

func TestPgQuerySamplePrivateSqlNativeSourceAndByteBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			trackBytes := pgSampleSqlNativeFixture(t, conn)
			if empty := pgSampleSqlNativeRead(t, conn); empty.Total != 0 || len(empty.Rows) != 0 {
				t.Fatal("empty native source gained a SQL representative")
			}
			text := []byte(strings.Repeat("x", 4095) + "界" + strings.Repeat("z", trackBytes-1-4098))
			server.RaisePgResult(conn.Exec(t.Context(), `INSERT INTO pg_stat_activity
 SELECT pg_backend_pid()+1,101,$1,'active',NULL,NULL,NULL,'','client backend',oid,
 clock_timestamp()-interval '45 seconds',clock_timestamp()-interval '50 seconds',clock_timestamp()
 FROM pg_database WHERE datname=current_database();`, string(text)))
			server.RaisePgResult(conn.Exec(t.Context(), `INSERT INTO pg_stat_activity
 SELECT pg_backend_pid()+2,NULL,NULL,'idle',NULL,NULL,NULL,'','client backend',oid,
 clock_timestamp(),clock_timestamp(),clock_timestamp() FROM pg_database WHERE datname=current_database();
 INSERT INTO pg_stat_activity SELECT pg_backend_pid(),999,'SELECT self_only','active',NULL,NULL,NULL,'','client backend',oid,
 clock_timestamp(),clock_timestamp(),clock_timestamp() FROM pg_database WHERE datname=current_database();`))
			frame := pgSampleSqlNativeRead(t, conn)
			if frame.Total != 2 || frame.Groups != 2 || len(frame.Rows) != 2 || frame.QueryTextTruncated == nil || *frame.QueryTextTruncated != 1 {
				t.Fatal("native representative changed source/self-exclusion coverage")
			}
			seenText, seenMissing := false, false
			for _, row := range frame.Rows {
				if len(row) != 13 {
					t.Fatal("native SQL column shape mismatch")
				}
				var query string
				server.Raise(json.Unmarshal(row[0], &query))
				capture, err := pgSampleParseSql(row[12], pgSampleLoad{}, 0, trackBytes)
				if err != nil {
					t.Fatal("native bounded column was rejected")
				}
				if query == "101" {
					seenText = bytes.Equal(capture.Prefix, text[:4096]) && capture.SourceBytes == trackBytes-1 && capture.PrefixTruncated && capture.ActivityBufferMaybeTruncated && !capture.Missing
				} else if query == "none" {
					seenMissing = capture.Missing && capture.SourceBytes == 0 && len(capture.Prefix) == 0
				}
			}
			if !seenText || !seenMissing {
				t.Fatal("native SQL byte/truncation/missing boundary was not preserved")
			}
		})
	})
}

func TestPgQuerySamplePrivateSqlNativeLoadAndAgeSelection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgSampleSqlNativeFixture(t, conn)
			server.RaisePgResult(conn.Exec(t.Context(), `INSERT INTO pg_stat_activity
 SELECT pg_backend_pid()+10000+i*100+j,i,
 CASE WHEN j=1 THEN 'COPY contract_close TO stdout /* synthetic representative */'
 ELSE 'SELECT * FROM contract_close /* synthetic alternative */' END,
 'active',NULL,NULL,NULL,'','client backend',(SELECT oid FROM pg_database WHERE datname=current_database()),
 '2026-01-01'::timestamptz-i*interval '1 second','2026-01-01'::timestamptz,'2026-01-01'::timestamptz
 FROM generate_series(1,80) i CROSS JOIN LATERAL generate_series(1,81-i) j;`))
			frame := pgSampleSqlNativeRead(t, conn)
			if frame.Total != 3240 || frame.Groups != 80 || len(frame.Rows) != 80 {
				t.Fatal("private extension changed existing count/age union")
			}
			captured := 0
			for _, row := range frame.Rows {
				var query string
				server.Raise(json.Unmarshal(row[0], &query))
				id, err := strconv.Atoi(query)
				if err != nil {
					t.Fatal("synthetic query key invalid")
				}
				present := string(row[12]) != "null"
				if present != (id <= 8 || id >= 73) {
					t.Fatal("private selection exceeded or lost the load/age split")
				}
				if present {
					captured++
					var column []json.RawMessage
					var encoded string
					server.Raise(json.Unmarshal(row[12], &column))
					server.Raise(json.Unmarshal(column[1], &encoded))
					text, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil || string(text) != "COPY contract_close TO stdout /* synthetic representative */" {
						t.Fatal("representative was not a member of its selected group")
					}
				}
			}
			if captured != 16 {
				t.Fatal("native private group cap changed")
			}
		})
	})
}
