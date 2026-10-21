// Real psql consumes the exact generated/substituted source on the admitted
// local TestEnv only. Temporary same-session catalog shadows carry synthetic
// COPY query text; these controls never connect to Main or create durable data.
package monitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const pgTailSyntheticCopy = "COPY synthetic_rows TO stdout"

// Setup runs only inside the new psql session, before the unchanged source.
// Read-only is restored before identity/authority checks; fixtures disappear
// with this connection. COPY-shaped text is a row value, never a metacommand.
// Version capability is synthetic, like the statistics rows; this is not proof
// that the fixture or any other server has an installed extension.
const pgTailPsqlShadowSetup = `SET default_transaction_read_only=off;
CREATE TEMP TABLE pg_stat_activity (
 pid int,query_id bigint,query text,state text,wait_event text,wait_event_type text,
 client_addr inet,application_name text,backend_type text,datid oid,
 query_start timestamptz,xact_start timestamptz,state_change timestamptz);
CREATE TEMP TABLE pg_stat_statements (
 queryid bigint,calls bigint,total_exec_time double precision,max_exec_time double precision,
 query text,dbid oid);
CREATE TEMP TABLE pg_extension(extname text,extversion text);
INSERT INTO pg_extension VALUES('pg_stat_statements','1.10');
CREATE TEMP TABLE pg_stat_statements_info(stats_reset timestamptz);
INSERT INTO pg_stat_statements_info VALUES(clock_timestamp());
COPY pg_stat_activity (pid,query_id,query,state,application_name,backend_type,datid) FROM STDIN;
2147480000	71	COPY synthetic_rows TO stdout	active	psql	client backend	0
\.

SET default_transaction_read_only=on;
`

// The wrapper execs the separately pinned real client without reading stdin;
// it changes only the fixed localhost fixture route. It fabricates no output.
// No connection is retained while the new psql connection is acquired.
func pgTailPsqlNativeRun(t *testing.T, failAtTailEntry bool) (string, string, error) {
	t.Helper()
	realPsql := os.Getenv("MONITOR_PG_TAIL_REAL_PSQL")
	if !filepath.IsAbs(realPsql) {
		t.Fatal("real psql requires an explicitly bound absolute tool path")
	}
	info, err := os.Stat(realPsql)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("bound real psql is unavailable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("qualified Python is required")
	}
	resolveCtx, resolveCancel := context.WithTimeout(t.Context(), 5*time.Second)
	resolved, err := exec.CommandContext(resolveCtx, python, "-c", "import sys;print(sys.executable)").Output()
	resolveCancel()
	if err != nil || !filepath.IsAbs(strings.TrimSpace(string(resolved))) {
		t.Fatal("qualified Python did not resolve")
	}
	python = strings.TrimSpace(string(resolved))
	var host, user, password, database string
	var port uint16
	server.Db(t.Context(), func(conn server.PgConn) {
		configuration := conn.Conn().Config()
		host, user, password, database, port = configuration.Host, configuration.User, configuration.Password, configuration.Database, configuration.Port
	})
	if host == "" || user == "" || database == "" || port == 0 {
		t.Fatal("local fixture connection authority is incomplete")
	}
	var input map[string]any
	if json.Unmarshal([]byte(pgSampleProgramInput(t)), &input) != nil {
		t.Fatal("generated reader input did not encode")
	}
	input["user"], input["password"], input["database"], input["port"] = user, password, database, int(port)
	input["sql"] = pgTailPsqlShadowSetup + pgQuerySampleSQL(database)
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal("local reader input did not encode")
	}
	dir := t.TempDir()
	wrapper := "#!" + python + "\nimport os,sys\na=sys.argv[1:]\nassert a.count('-h')==1 and a[a.index('-h')+1]=='localhost'\na[a.index('-h')+1]=os.environ['MONITOR_PG_TAIL_FIXTURE_HOST']\nos.execv(os.environ['MONITOR_PG_TAIL_REAL_PSQL'],[os.environ['MONITOR_PG_TAIL_REAL_PSQL']]+a)\n"
	if os.WriteFile(filepath.Join(dir, "psql"), []byte(wrapper), 0700) != nil {
		t.Fatal("real client routing wrapper was not written")
	}
	program := pgQuerySampleProgram
	if failAtTailEntry {
		const boundary = "    tail_sequence+=1\n"
		if strings.Count(program, boundary) != 1 {
			t.Fatal("reader marker-consumption boundary changed")
		}
		program = strings.Replace(program, boundary, boundary+"    if tail_sequence==1:raise Failure('child_process','owner_deadline')\n", 1)
	}
	ctx, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", program)
	cmd.Env = append(os.Environ(), "PATH="+dir, "MONITOR_PG_TAIL_REAL_PSQL="+realPsql, "MONITOR_PG_TAIL_FIXTURE_HOST="+host)
	cmd.Stdin = bytes.NewReader(raw)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("real-client fixture exhausted its safety owner instead of its marker barrier")
	}
	return string(out), stderr.String(), runErr
}

// Missing or invalid version capability remains refused by the unchanged
// parser. Numeric two/three-component synthetic versions preserve healthy data.
func TestPgTailPsqlSyntheticVersionCapabilityControls(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		name    string
		version any
		omit    bool
		valid   bool
	}{
		{name: "missing", omit: true, valid: false},
		{name: "null", version: nil, valid: false},
		{name: "empty", version: "", valid: false},
		{name: "malformed", version: "synthetic-private-version", valid: false},
		{name: "valid_two_component", version: "1.10", valid: true},
		{name: "valid_three_component", version: "1.10.1", valid: true},
	} {
		frames := pgSampleTestFrames(now)
		if item.omit {
			delete(frames[0], "pgss_version")
		} else {
			frames[0]["pgss_version"] = item.version
		}
		raw := pgSampleTestEncode(t, frames)
		receipt, err := parsePgQuerySample(raw, now)
		admission := pgTailNativeAdmission(raw, now, err)
		if item.valid {
			if err != nil || !receipt.Complete || receipt.Samples != 12 || admission.Stage != "first_activity_admitted" || !admission.Checks["identity_pgss_version"] {
				t.Fatal("synthetic valid version lost healthy parser admission")
			}
		} else {
			if err == nil || receipt.Complete || receipt.Samples != 0 || admission.Stage != "identity" || admission.ParseReason != "invalid_bounded_sample" {
				t.Fatal("missing or invalid synthetic version escaped parser refusal")
			}
			for key, value := range admission.Checks {
				if (key == "identity_pgss_version") == value {
					t.Fatal("synthetic invalid version did not isolate its exact predicate")
				}
			}
		}
		encoded, err := json.Marshal(admission)
		if err != nil || strings.Contains(string(encoded), "synthetic-private-version") {
			t.Fatal("synthetic version diagnostic disclosed source value")
		}
	}
}

// Real client metacommand parsing, nonce substitution, COPY-shaped row output,
// all17 legacy records and all18 stripped markers must compose in one stream.
func TestPgTailPsqlNativeGeneratedStreamAndCopyText(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		out, stderr, err := pgTailPsqlNativeRun(t, false)
		parseNow := time.Now()
		receipt, parseErr := parsePgQuerySample(out, parseNow)
		admission := pgTailNativeAdmission(out, parseNow, parseErr)
		admissionJson, admissionErr := json.Marshal(admission)
		if admissionErr != nil {
			tb.Fatal("fixed admission diagnostic did not encode")
		}
		tb.Logf("pg-tail admission diagnostic: %s", admissionJson)
		tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
		old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
		diagnostic := pgTailNativeFixedFailureDiagnostic(out, err)
		frames := -1
		if old.Frames != nil {
			frames = *old.Frames
		}
		tailStatus, tailPhase, tailEdge := "unknown", "unknown", "unknown"
		tailIndex, tailRecords := -1, 0
		if tail != nil {
			tailStatus, tailPhase, tailEdge = tail.Status, tail.Phase, tail.Edge
			tailRecords = tail.Records
			if tail.Index != nil {
				tailIndex = *tail.Index
			}
		}
		tb.Logf("pg-tail native diagnostic: reader_error=%t reader_exit_code=%d source_failure=%t source_phase=%s source_cause=%s full_parse_error=%t complete=%t samples=%d tail_status=%s tail_phase=%s tail_edge=%s tail_index=%d tail_records=%d legacy_status=%s legacy_frames=%d output_bytes=%d stderr_bytes=%d", diagnostic.ReaderError, diagnostic.ReaderExitCode, diagnostic.SourceFailure, diagnostic.SourcePhase, diagnostic.SourceCause, parseErr != nil, receipt.Complete, receipt.Samples, tailStatus, tailPhase, tailEdge, tailIndex, tailRecords, old.Status, frames, len(out), len(stderr))
		if err != nil || parseErr != nil || !receipt.Complete || receipt.Samples != 12 || tail.Status != "valid" || tail.Records != 18 || tail.Edge != "returned" || tail.Phase != "history_end" || old.Frames == nil || *old.Frames != 12 || len(stderr) != 2624 {
			tb.Fatal("real psql did not preserve generated source and bounded progress")
		}
		if strings.Contains(out, "monitor-pg-tail") || strings.Contains(out, "__MONITOR_PG_TAIL_NONCE__") || strings.Contains(out, pgTransportFixtureNonce) || !strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(pgTailSyntheticCopy))) {
			tb.Fatal("real-client stripping or synthetic COPY query-text interoperability failed")
		}
		if strings.Count(stderr, "monitor-pg-phase ") != 17 || strings.Count(stderr, "mpg3 ") != 18 {
			tb.Fatal("real psql lost exact legacy/compact protocol membership")
		}
	})
}

// The reader's existing failure/kill/Wait path is forced after receiving w10
// from real psql, never after a timing sleep or fabricated response. The result
// remains incomplete; no external cancellation/remote cleanup claim is made.
func TestPgTailPsqlNativePartialEntryFailureJoinsReaderChild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		out, stderr, err := pgTailPsqlNativeRun(t, true)
		failure, valid := parsePgSampleSourceFailure(out)
		tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
		old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
		if err == nil || !valid || failure.Phase != "child_process" || failure.Cause != "owner_deadline" || tail.Status != "valid" || tail.Records != 1 || tail.Phase != "sample_wait" || tail.Edge != "entered" || tail.Index == nil || *tail.Index != 10 || old.Frames == nil || *old.Frames != 10 {
			tb.Fatal("real psql did not reach the exact partial-entry failure boundary")
		}
		if strings.Contains(out, `"kind":"activity"`) || strings.Contains(out, pgTransportFixtureNonce) || len(stderr) > 4096 {
			tb.Fatal("failed real-client stream published buffered source or private correlation")
		}
	})
}
