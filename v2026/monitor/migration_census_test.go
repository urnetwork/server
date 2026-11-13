// Closed source splitting and transport framing are deterministic completion
// gates. No fixture here depends on timing, PostgreSQL memory or a remote host.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Each expression has an independently recognizable source position.
func migrationCensusTestPlan(t testing.TB) (*migrationCensusPlan, string) {
	t.Helper()
	query := "WITH version AS (SELECT 1 AS value) SELECT version.value, true, 'private,quoted;cell' FROM version;"
	settings := DefaultMigrationCensusSettings()
	settings.MaximumColumnsPerStatement = 1
	plan, err := planMigrationCensus(query, settings)
	if err != nil {
		t.Fatal(err)
	}
	return plan, query
}

// The fixture independently encodes the public frame protocol. The actual
// native psql tests below obtain those frames from PostgreSQL instead.
func migrationCensusTestFrames(plan *migrationCensusPlan) []string {
	frames := []string{fmt.Sprintf(`{"kind":"begin","schema":1,"sha256":"%s","columns":3,"batches":3}`, plan.digest)}
	for index, value := range []string{"1", "true", `"private,quoted;cell"`} {
		frames = append(frames, fmt.Sprintf(`{"kind":"batch","index":%d,"offset":%d,"values":[%s]}`, index, index, value))
	}
	return append(frames, `{"kind":"catalog","present":true,"count":1}`, `{"kind":"identity","ordinal":0,"values":[0,"synthetic-hash"]}`,
		fmt.Sprintf(`{"kind":"complete","schema":1,"sha256":"%s"}`, plan.digest))
}

// CSV framing must preserve embedded field/record delimiters as source data.
func migrationCensusTestCsv(frames []string) string {
	var out strings.Builder
	writer := csv.NewWriter(&out)
	for _, frame := range frames {
		_ = writer.Write([]string{frame})
	}
	writer.Flush()
	return out.String()
}

// This assertion is shared with causal controls so a missing commit index or
// PGOPTIONS' own -c token cannot make a broken psql transport pass vacuously.
func migrationCensusTestTransportShape(command, stdin, password string) bool {
	_, psqlArgs, hasPsql := strings.Cut(command, "exec psql ")
	commit, complete := strings.Index(stdin, "COMMIT;\n"), strings.LastIndex(stdin, "'complete'")
	return hasPsql && strings.Contains(psqlArgs, " -f -") && strings.Contains(psqlArgs, " -X -q --csv -t ") &&
		!strings.Contains(psqlArgs, " -c ") && !strings.Contains(command, password) &&
		strings.HasPrefix(stdin, password+"\nBEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;\n") && commit >= 0 && complete > commit
}

// These are deliberately invalid transport variants of an otherwise complete
// response, not a timeout- or memory-dependent reproduction.
func TestMigrationCensusTransportControlRejectsGiantBufferAndMissingCommit(t *testing.T) {
	plan, _ := migrationCensusTestPlan(t)
	program, err := plan.script()
	if err != nil {
		t.Fatal(err)
	}
	command := "export PGOPTIONS='-c statement_timeout=30000'; exec psql -h localhost -X -q --csv -t -v ON_ERROR_STOP=1 -f -"
	stdin := "synthetic-password\n" + program
	if !migrationCensusTestTransportShape(command, stdin, "synthetic-password") {
		t.Fatal("transport assertion rejects genuine closed session")
	}
	for _, broken := range []struct{ command, stdin string }{
		{command: strings.Replace(command, " -f -", " -c 'one giant query'", 1), stdin: stdin},
		{command: command, stdin: strings.Replace(stdin, "COMMIT;\n", "", 1)},
		{command: command, stdin: strings.Replace(stdin, "COMMIT;\n", "SELECT 'complete';\n", 1)},
		{command: command, stdin: strings.Replace(stdin, "REPEATABLE READ READ ONLY", "READ COMMITTED READ ONLY", 1)},
	} {
		if migrationCensusTestTransportShape(broken.command, broken.stdin, "synthetic-password") {
			t.Fatal("broken command or missing completion boundary passed control")
		}
	}
}

// Every logical column remains present; source predicates and future suffixes
// are not removed to make the planner or a qualification fixture fit a cap.
func TestMigrationCensusBoundsCompletePublishedSource(t *testing.T) {
	_, query, columns := migrationPublicColumnQueries(t)
	settings := DefaultMigrationCensusSettings()
	plan, err := planMigrationCensus(query, settings)
	if err != nil {
		t.Fatal("complete source cannot enter bounded planning", err)
	}
	if plan.columns != columns || len(plan.batches) <= 1 || len(query) <= settings.MaximumStatementBytes {
		t.Fatal("root cause control did not exceed one statement or changed positional census", plan.columns, columns)
	}
	count, largest := 0, 0
	for index, batch := range plan.batches {
		if batch.index != index || batch.offset != count || batch.columns < 1 || batch.columns > settings.MaximumColumnsPerStatement || len(batch.query) > settings.MaximumStatementBytes {
			t.Fatal("bounded source lost an ordered column or complete-prefix byte admission", index)
		}
		count += batch.columns
		largest = max(largest, len(batch.query))
	}
	if count != columns {
		t.Fatal("bounded source truncated future or current columns")
	}
	program, err := plan.script()
	if err != nil || len(program) > settings.MaximumProgramBytes {
		t.Fatal("bounded source lost its finite program admission", err)
	}
	// The exact predecessor differs in SQL only by the additive suffix. This
	// comparison is offline and does not claim a production OOM observation.
	priorContracts := []snMainnetMigrationContract{}
	for _, contract := range migrationCurrentSuffixContracts {
		if contract.artifact.requiredVersion != server.CompetitionDistributedReevaluationMigrationVersion() {
			priorContracts = append(priorContracts, contract)
		}
	}
	currentSuffix := migrationCurrentSuffixArtifactQueries()
	if strings.Count(query, currentSuffix) != 1 {
		t.Fatal("predecessor comparison cannot locate the exact source-owned suffix")
	}
	prior := strings.Replace(query, currentSuffix, migrationCurrentSuffixVersionQueries(priorContracts), 1)
	priorPlan, err := planMigrationCensus(prior, settings)
	if err != nil {
		t.Fatal("predecessor source is not admitted by the same bounded planner", err)
	}
	priorDigest := sha256.Sum256([]byte(prior))
	t.Logf("census_bounds current_bytes=%d current_sha256=%s current_columns=%d prior_bytes=%d prior_sha256=%s prior_columns=%d batches=%d largest_statement_bytes=%d program_bytes=%d", len(query), plan.digest, columns, len(prior), hex.EncodeToString(priorDigest[:]), priorPlan.columns, len(plan.batches), largest, len(program))
}

// Separators inside every supported PostgreSQL quoting/depth form are data.
func TestMigrationCensusLexerKeepsExpressionPositions(t *testing.T) {
	for _, expression := range []string{
		`'a,b;from select'`, `E'a\';,b'`, `"synthetic,;identifier"`, `$$a,b;from select\quit$$`,
		`$body_1$a,b;from select$body_1$`, `ARRAY[1,2,3][1]`, `ARRAY[ARRAY[1,2],ARRAY[3,4]]`,
		`(SELECT 1 /* outer ,; /* nested from */ select */)`, `1 -- ,;from select
`, `synthetic$body$identifier`, `coalesce((SELECT 1),2)`,
	} {
		query := "WITH version AS (SELECT 1 AS value) SELECT " + expression + ", true FROM version;"
		settings := DefaultMigrationCensusSettings()
		settings.MaximumColumnsPerStatement = 1
		plan, err := planMigrationCensus(query, settings)
		if err != nil || plan.columns != 2 || len(plan.batches) != 2 || !strings.Contains(plan.batches[0].query, strings.TrimSpace(expression)) {
			t.Fatalf("quoted/depth expression changed source positions %q: %v", expression, err)
		}
	}
}

// Unexpected commands, mismatched delimiters and malformed quoting are not
// handed to psql, where a backslash could become a client-side command.
func TestMigrationCensusLexerRejectsUnclosedOrAdditionalSource(t *testing.T) {
	for _, query := range []string{
		`SELECT 1`, `WITH version AS (SELECT 1 value) SELECT 'unterminated FROM version;`,
		`WITH version AS (SELECT 1 value) SELECT $$unterminated FROM version;`,
		`WITH version AS (SELECT 1 value) SELECT E'a\' FROM version;`,
		`WITH version AS (SELECT 1 value) SELECT 1 /* unterminated FROM version;`,
		`WITH version AS (SELECT 1 value) SELECT ARRAY[1,2 FROM version;`,
		`WITH version AS (SELECT 1 value) SELECT ([1,2)] FROM version;`,
		`WITH version AS (SELECT 1 value) SELECT 1 FROM version; SELECT 2;`,
		`WITH version AS (SELECT 1 value) SELECT 1 FROM version WHERE true;`,
		"WITH version AS (SELECT 1 value) SELECT 1 \\gexec\n FROM version;",
	} {
		if _, err := planMigrationCensus(query, DefaultMigrationCensusSettings()); err == nil {
			t.Fatalf("malformed or additional source entered physical planner: %q", query)
		}
	}
}

// Bounds include the complete WITH prefix and fail before any transport call.
func TestMigrationCensusOwnerBoundsRefuseWithoutPartialPlan(t *testing.T) {
	_, query := migrationCensusTestPlan(t)
	for _, change := range []func(*MigrationCensusSettings){
		func(s *MigrationCensusSettings) { s.MaximumStatementBytes = 1 },
		func(s *MigrationCensusSettings) { s.MaximumColumns = 2; s.MaximumColumnsPerStatement = 1 },
		func(s *MigrationCensusSettings) { s.MaximumColumnsPerStatement = 1; s.MaximumBatches = 2 },
		func(s *MigrationCensusSettings) { s.MaximumInputBytes = len(query) - 1 },
		func(s *MigrationCensusSettings) { s.MaximumNesting = 0 },
	} {
		settings := DefaultMigrationCensusSettings()
		change(&settings)
		if plan, err := planMigrationCensus(query, settings); err == nil || plan != nil {
			t.Fatal("invalid owner bounds admitted a partial physical census")
		}
	}
	plan, _ := migrationCensusTestPlan(t)
	plan.settings.MaximumProgramBytes = 1
	if program, err := plan.script(); err == nil || program != "" {
		t.Fatal("program bytes escaped their independent owner admission")
	}
}

// Exact output reassembly preserves ordinary text, booleans and identities.
func TestMigrationCensusFramesReassembleOnlyCompleteObservation(t *testing.T) {
	plan, query := migrationCensusTestPlan(t)
	observation := &migrationCensusObservation{settings: plan.settings}
	rows, err := runMigrationCensus(t.Context(), query, observation, func(string) (string, error) {
		return migrationCensusTestCsv(migrationCensusTestFrames(plan)), nil
	})
	if err != nil || !reflect.DeepEqual(rows, []pgRow{{"1", "t", "private,quoted;cell"}}) || !observation.catalogComplete ||
		!reflect.DeepEqual(observation.catalogRows, []pgRow{{"0", "synthetic-hash"}}) {
		t.Fatal("complete framed observation changed source data", err, rows)
	}
}

// Every malformed variant begins with a reused successful observation: an
// unsuccessful successor must erase it, not publish an earlier snapshot.
func TestMigrationCensusFramesRejectNullMalformedOrderAndTruncation(t *testing.T) {
	plan, query := migrationCensusTestPlan(t)
	for _, mutate := range []func([]string) []string{
		func(f []string) []string { f[0] = `null`; return f },
		func(f []string) []string { f[0] = strings.Replace(f[0], `"kind":"begin"`, `"kind":null`, 1); return f },
		func(f []string) []string { f[0] = strings.Replace(f[0], `"schema":1`, `"schema":null`, 1); return f },
		func(f []string) []string { f[0] = strings.Replace(f[0], `"columns":3`, `"columns":3.0`, 1); return f },
		func(f []string) []string {
			f[0] = strings.Replace(f[0], `"sha256":"`+plan.digest+`"`, `"sha256":null`, 1)
			return f
		},
		func(f []string) []string {
			f[0] = strings.Replace(f[0], `"schema":1`, `"schema":1,"schema":1`, 1)
			return f
		},
		func(f []string) []string { f[0] = strings.Replace(f[0], `"schema":1`, `"schema":2`, 1); return f },
		func(f []string) []string { f[1], f[2] = f[2], f[1]; return f },
		func(f []string) []string { f[2] = f[1]; return f },
		func(f []string) []string { f[1] = strings.Replace(f[1], `"offset":0`, `"offset":1`, 1); return f },
		func(f []string) []string { f[1] = strings.Replace(f[1], `"values":[1]`, `"values":[{}]`, 1); return f },
		func(f []string) []string { f[1] = strings.Replace(f[1], `"values":[1]`, `"values":[1,2]`, 1); return f },
		func(f []string) []string {
			f[4] = strings.Replace(f[4], `"present":true`, `"present":null`, 1)
			return f
		},
		func(f []string) []string { f[4] = strings.Replace(f[4], `"count":1`, `"count":0`, 1); return f },
		func(f []string) []string { f[5] = strings.Replace(f[5], `"ordinal":0`, `"ordinal":1`, 1); return f },
		func(f []string) []string { f[6] = `{"kind":"complete","schema":1,"sha256":null}`; return f },
		func(f []string) []string {
			f[6] = strings.Replace(f[6], plan.digest, strings.Repeat("0", 64), 1)
			return f
		},
		func(f []string) []string { f[6] += `{}`; return f },
		func(f []string) []string { return f[:6] },
		func(f []string) []string { return append(f, f[6]) },
	} {
		observation := &migrationCensusObservation{settings: plan.settings, catalogComplete: true, catalogRows: []pgRow{{"stale"}}}
		rows, err := runMigrationCensus(t.Context(), query, observation, func(string) (string, error) {
			return migrationCensusTestCsv(mutate(migrationCensusTestFrames(plan))), nil
		})
		if err == nil || rows != nil || observation.catalogComplete || observation.catalogRows != nil {
			t.Fatal("malformed or stale observation became schema authority")
		}
	}
	for _, out := range []string{"", strings.TrimSuffix(migrationCensusTestCsv(migrationCensusTestFrames(plan)), "\n"), "one,two\n"} {
		if rows, _, err := plan.parse(out); err == nil || rows != nil {
			t.Fatal("missing CSV/completion structure became schema authority")
		}
	}
}

// Physical child success is necessary but not sufficient if the owner ended;
// independently live siblings and their observation budgets remain unaffected.
func TestMigrationCensusCanceledOrFailedTransportNeverPublishes(t *testing.T) {
	plan, query := migrationCensusTestPlan(t)
	parent := t.Context()
	for _, cancelOwner := range []bool{false, true} {
		ctx, cancel := context.WithCancel(parent)
		observation := &migrationCensusObservation{settings: plan.settings, catalogComplete: true}
		rows, err := runMigrationCensus(ctx, query, observation, func(string) (string, error) {
			if cancelOwner {
				cancel()
				return migrationCensusTestCsv(migrationCensusTestFrames(plan)), nil
			}
			return migrationCensusTestCsv(migrationCensusTestFrames(plan)), errors.New("synthetic authenticated transport failure")
		})
		cancel()
		if err == nil || rows != nil || observation.catalogComplete || observation.catalogRows != nil || parent.Err() != nil {
			t.Fatal("ended observation published a complete or cached census")
		}
	}
}

// The actual production runner must choose one stdin file-reader session,
// never a giant -c argument or one simple-query buffer containing all batches.
func TestMigrationCensusProductionTransportOwnsOneBoundedSession(t *testing.T) {
	plan, query := migrationCensusTestPlan(t)
	cfg := configFromSignalSettings(syntheticSettings(nil).withDefaults())
	cfg.hosts[0].overlayIp = "192.0.2.70"
	cfg.pgPassword = "synthetic-private-stdin"
	runner := newRunner(cfg)
	calls := 0
	runner.runSSH = func(_ context.Context, args []string, stdin string) (string, string, error) {
		calls++
		command := args[len(args)-1]
		if !migrationCensusTestTransportShape(command, stdin, cfg.pgPassword) {
			t.Fatal("census lost its one owned file-reader session")
		}
		for _, batch := range plan.batches {
			if !strings.Contains(stdin, batch.query+"\n") {
				t.Fatal("transport omitted a source-owned bounded command")
			}
		}
		return migrationCensusTestCsv(migrationCensusTestFrames(plan)), "", nil
	}
	observation := &migrationCensusObservation{settings: plan.settings}
	rows, err := runner.pg(context.WithValue(t.Context(), migrationCensusContextKey{}, observation), query)
	if err != nil || len(rows) != 1 || calls != 1 || !observation.catalogComplete {
		t.Fatal("bounded session was not physically joined before publication", err, calls)
	}
}

// Both old SQL and new census share the same exact owner timeout default and
// explicit override. Invalid budgets fail before starting any transport.
func TestMigrationCensusStatementTimeoutRemainsExactForBothPaths(t *testing.T) {
	plan, query := migrationCensusTestPlan(t)
	for _, timeout := range []time.Duration{0, 1234 * time.Millisecond, -time.Millisecond, time.Nanosecond, 2147483648 * time.Millisecond} {
		for _, bounded := range []bool{false, true} {
			cfg := configFromSignalSettings(syntheticSettings(nil).withDefaults())
			cfg.hosts[0].overlayIp = "192.0.2.71"
			cfg.pgStatementTimeout = timeout
			runner := newRunner(cfg)
			calls := 0
			runner.runSSH = func(_ context.Context, args []string, _ string) (string, string, error) {
				calls++
				want := int64(30000)
				if timeout != 0 {
					want = int64(timeout / time.Millisecond)
				}
				if !strings.Contains(args[len(args)-1], fmt.Sprintf("statement_timeout=%d ", want)) {
					t.Fatal("statement owner budget was rounded or changed")
				}
				if bounded {
					return migrationCensusTestCsv(migrationCensusTestFrames(plan)), "", nil
				}
				return "1\n", "", nil
			}
			ctx := t.Context()
			if bounded {
				ctx = context.WithValue(ctx, migrationCensusContextKey{}, &migrationCensusObservation{settings: plan.settings})
			}
			_, err := runner.pg(ctx, query)
			valid := timeout == 0 || timeout == 1234*time.Millisecond
			if valid && (err != nil || calls != 1) || !valid && (err == nil || calls != 0) {
				t.Fatal("timeout admission changed", timeout, bounded, err, calls)
			}
		}
	}
}

// Defaulting copies the caller's budget; observing or modifying one owner
// cannot silently move another independent migration signal's admission.
func TestMigrationCensusSettingsBelongToEachObservation(t *testing.T) {
	first, second := syntheticSettings(&syntheticSource{}).withDefaults(), syntheticSettings(&syntheticSource{}).withDefaults()
	first.MigrationCensus.MaximumStatementBytes = 1
	if first.MigrationCensus == second.MigrationCensus || second.MigrationCensus.MaximumStatementBytes != DefaultMigrationCensusSettings().MaximumStatementBytes {
		t.Fatal("independent signals share a mutable planner budget")
	}
	copy := first.withDefaults()
	copy.MigrationCensus.MaximumStatementBytes = 2
	if first.MigrationCensus.MaximumStatementBytes != 1 {
		t.Fatal("settings snapshot retained caller-mutable budget")
	}
	// Keep the decoder import coupled to a canonical scalar-value control.
	values, err := migrationCensusFrameValues(json.RawMessage(`[null,false,1234567890123456789,"opaque"]`), 4)
	if err != nil || !reflect.DeepEqual(values, pgRow{"", "f", "1234567890123456789", "opaque"}) {
		t.Fatal("scalar protocol changed precision or null behavior", err)
	}
}
