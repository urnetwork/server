package server

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// D-OPS-6's pending migration groups, in whichever build runs this. A test
// binary may link a pending group, so what a binary that is not a test does is
// proved with a built probe. The static check every group passes is here too, so
// both builds compile it and its twins run in both.

// The groups' markers in §19.7's order, as the probe prints whether each is
// linked.
var sidecarGroupMarkerIds = []string{
	sidecarMg0MigrationId,
	sidecarMg1MigrationId,
	sidecarMg2MigrationId,
	sidecarMg5MigrationId,
	sidecarMg6MigrationId,
	sidecarMg7MigrationId,
	sidecarMg8MigrationId,
	sidecarMg9MigrationId,
}

func TestSidecarPendingRunsOnlyInLocalCanaryOrTests(t *testing.T) {
	for _, c := range []struct {
		env        string
		testBinary bool
		allowed    bool
	}{
		{env: "local", allowed: true},
		{env: "canary", allowed: true},
		{env: "main", testBinary: true, allowed: true},
		{env: "", testBinary: true, allowed: true},
		{env: "main"},
		{env: "community"},
		{env: "beta"},
		{env: "LOCAL"},
		{env: " local"},
		{env: ""},
	} {
		if got := sidecarPendingAllowed(c.env, c.testBinary); got != c.allowed {
			t.Errorf("env %q in a test binary %t: allowed = %t, want %t", c.env, c.testBinary, got, c.allowed)
		}
	}
}

func sidecarBuildProbe(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	binary := filepath.Join(dir, name)
	buildArgs := append([]string{"build"}, args...)
	buildArgs = append(buildArgs, "-o", binary, "./testdata/sidecarpendingprobe")
	output, err := exec.Command("go", buildArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %s\n%s", strings.Join(buildArgs, " "), err, output)
	}
	return binary
}

// Runs the probe with WARP_ENV set, or unset when env is "".
func sidecarRunProbe(t *testing.T, binary string, env string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	command.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "WARP_ENV=")
	})
	if env != "" {
		command.Env = append(command.Env, "WARP_ENV="+env)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// The probe's line: the migration count, then whether each group is linked.
func sidecarParseProbe(t *testing.T, output string) (int, []bool) {
	t.Helper()
	fields := strings.Fields(output)
	if len(fields) != 1+len(sidecarGroupMarkerIds) {
		t.Fatalf("the probe printed %q", output)
	}
	count := 0
	if _, err := fmt.Sscan(fields[0], &count); err != nil {
		t.Fatalf("the probe printed %q: %s", output, err)
	}
	linked := make([]bool, 0, len(sidecarGroupMarkerIds))
	for _, field := range fields[1:] {
		switch field {
		case "true":
			linked = append(linked, true)
		case "false":
			linked = append(linked, false)
		default:
			t.Fatalf("the probe printed %q", output)
		}
	}
	return count, linked
}

// Both build tags compile, a release build links main's migrations and no group
// whatever its env, a build with the tag links every pending group after main's,
// and a build with the tag refuses to start anywhere but local or canary (FS-54's
// first two cases). The tagged tests hold the pending tail entry by entry.
func TestSidecarPendingBuildTags(t *testing.T) {
	dir := t.TempDir()
	// an empty -tags, so a GOFLAGS that sets the tag cannot pass for a release build
	release := sidecarBuildProbe(t, dir, "release", "-tags", "")
	pending := sidecarBuildProbe(t, dir, "pending", "-tags", "sidecar_pending")

	// every package that reads a group, and the migration runner's own, in both
	for _, tags := range []string{"", "sidecar_pending"} {
		args := []string{"build", "-tags", tags, ".", "./session", "./model", "./router", "./controller", "./api", "./connect", "./bringyourctl"}
		output, err := exec.Command("go", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("go %s: %s\n%s", strings.Join(args, " "), err, output)
		}
	}

	releaseCount := 0
	for _, env := range []string{"main", "community", "canary", "local", ""} {
		output, stderr, err := sidecarRunProbe(t, release, env)
		if err != nil {
			t.Fatalf("release build, WARP_ENV %q: %s\n%s", env, err, stderr)
		}
		count, linked := sidecarParseProbe(t, output)
		for index, groupLinked := range linked {
			if groupLinked {
				t.Fatalf("release build, WARP_ENV %q links %s", env, sidecarGroupMarkerIds[index])
			}
		}
		if releaseCount != 0 && count != releaseCount {
			t.Fatalf("release build, WARP_ENV %q links %d migrations, not %d", env, count, releaseCount)
		}
		releaseCount = count
	}

	for _, env := range []string{"local", "canary"} {
		output, stderr, err := sidecarRunProbe(t, pending, env)
		if err != nil {
			t.Fatalf("pending build, WARP_ENV %q: %s\n%s", env, err, stderr)
		}
		count, linked := sidecarParseProbe(t, output)
		// each group adds at least its marker
		if count < releaseCount+len(sidecarGroupMarkerIds) {
			t.Fatalf("pending build, WARP_ENV %q links %d migrations over main's %d", env, count, releaseCount)
		}
		for index, groupLinked := range linked {
			if !groupLinked {
				t.Fatalf("pending build, WARP_ENV %q does not link %s", env, sidecarGroupMarkerIds[index])
			}
		}
	}

	for _, env := range []string{"main", "community", "beta", ""} {
		output, stderr, err := sidecarRunProbe(t, pending, env)
		if err == nil {
			t.Fatalf("pending build ran in WARP_ENV %q and printed %q", env, output)
		}
		if !strings.Contains(stderr, "sidecar_pending") || output != "" {
			t.Fatalf("pending build in WARP_ENV %q failed without the refusal: %q\n%s", env, output, stderr)
		}
	}
}

// What CODESTYLE's migration rules and §19.7 refuse in a sidecar group
// (connect/CODESTYLE.md, "Database migrations and deploy order"). A group applies
// while the previous release serves, so it may add tables, nullable columns and
// indexes of the tables it creates, and give any other table an index only
// online, built concurrently. It may drop, rename or empty nothing, and a column
// turns NOT NULL only through a validated NOT VALID check (precedent
// db_migrations.go's network_user_auth_wallet_user_fk), never SET NOT NULL,
// whose scan holds every writer of the table.
//
// A group may also replace a function that a registered migration created, which
// the running release calls, but only to widen it, as the permissive step of an
// additive-then-cleanup pair does (competitionSidecarSourceMigration). The
// replacement is the registered function's text with fragments put under
// conditional arms, each `(CASE WHEN condition THEN alternative ELSE fragment
// END)` with the registered fragment kept as the ELSE arm, so the function
// answers as before wherever every condition is false. Each condition is one
// the registered function is shown to refuse (sidecarGroupRefusedConditions),
// so what the function admitted it still admits.
var (
	sidecarGroupCreateTablePattern = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)
	sidecarGroupDestroyPattern     = regexp.MustCompile(`(?i)\b(DROP|RENAME|TRUNCATE)\b`)
	sidecarGroupSetNotNullPattern  = regexp.MustCompile(`(?i)\bSET\s+NOT\s+NULL\b`)
	sidecarGroupIndexPattern       = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:"?[a-z_][a-z0-9_]*"?\s+)?ON\s+(?:ONLY\s+)?"?([a-z_][a-z0-9_]*)"?`)
	sidecarGroupIndexNamePattern   = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s+ON\b`)
	sidecarGroupRecoveryPattern    = regexp.MustCompile(`(?i)^\s*DROP\s+INDEX\s+CONCURRENTLY\s+IF\s+EXISTS\s+([a-z_][a-z0-9_]*)\s*$`)
	sidecarGroupFunctionPattern    = regexp.MustCompile(`(?i)\bCREATE\s+(?:OR\s+REPLACE\s+)?(FUNCTION\s+"?([a-z_][a-z0-9_]*)"?\s*\()`)
	sidecarGroupFunctionTagPattern = regexp.MustCompile(`\$[A-Za-z_]*\$`)
	sidecarGroupConditionalPattern = regexp.MustCompile(`\(CASE WHEN ([^()]+) THEN ([^()]+) ELSE ([^()]+) END\)`)
)

// The conditions under which a registered function refuses every input, by
// function. A replacement that changes the function only under one of them
// admits everything the registered function admits. Each is proved on a
// database, against the registered function: the source guards' two by
// TestCompetitionSidecarSourceGraphOnlyWidens and
// TestCompetitionSidecarSourceAuthorizationOnlyWidens, under the pending tag.
var sidecarGroupRefusedConditions = map[string][]string{
	"competition_v2_source_graph_valid": {"commits ? 'sidecar'"},
	"competition_v2_source_authorized":  {"intent->'commits' ? 'sidecar'"},
}

// The migrations below every pending group: main's and each registered
// group's, which the release running when a pending group applies has applied.
func sidecarGroupRegisteredMigrations() []any {
	return migrations[:len(migrations)-len(sidecarPendingMigrations())]
}

// Each function a statement list creates, by lower-case name, as its text from
// the FUNCTION keyword to the end of its dollar-quoted body, so CREATE and
// CREATE OR REPLACE of one text compare equal. A function whose body is not
// dollar quoted maps to "", which equals no replacement.
func sidecarGroupFunctions(sql string) map[string]string {
	functions := map[string]string{}
	for _, match := range sidecarGroupFunctionPattern.FindAllStringSubmatchIndex(sql, -1) {
		name := strings.ToLower(sql[match[4]:match[5]])
		text := ""
		rest := sql[match[2]:]
		if open := sidecarGroupFunctionTagPattern.FindStringIndex(rest); open != nil {
			tag := rest[open[0]:open[1]]
			if end := strings.Index(rest[open[1]:], tag); 0 <= end {
				text = rest[:open[1]+end+len(tag)]
			}
		}
		functions[name] = text
	}
	return functions
}

// The registered function of a name: its latest text below the pending groups,
// and whether a registered migration creates it at all.
func sidecarGroupRegisteredFunction(name string) (string, bool) {
	text := ""
	found := false
	for _, migration := range sidecarGroupRegisteredMigrations() {
		if value, ok := migration.(*SqlMigration); ok {
			if function, ok := sidecarGroupFunctions(value.sql)[name]; ok {
				text = function
				found = true
			}
		}
	}
	return text, found
}

// The conditions a replacement puts fragments of the registered function under,
// and whether the two texts differ in nothing else. A conditional arm the
// registered text already holds is unchanged text, not an edit.
func sidecarGroupWidenedConditions(registered string, replacement string) ([]string, bool) {
	conditions := []string{}
	registeredAt := 0
	replacementAt := 0
	for _, arm := range sidecarGroupConditionalPattern.FindAllStringSubmatchIndex(replacement, -1) {
		between := replacement[replacementAt:arm[0]]
		if !strings.HasPrefix(registered[registeredAt:], between) {
			return nil, false
		}
		registeredAt += len(between)
		whole := replacement[arm[0]:arm[1]]
		kept := replacement[arm[6]:arm[7]]
		switch {
		case strings.HasPrefix(registered[registeredAt:], whole):
			registeredAt += len(whole)
		case strings.HasPrefix(registered[registeredAt:], kept):
			registeredAt += len(kept)
			conditions = append(conditions, replacement[arm[2]:arm[3]])
		default:
			return nil, false
		}
		replacementAt = arm[1]
	}
	if registered[registeredAt:] != replacement[replacementAt:] {
		return nil, false
	}
	return conditions, true
}

// What the static check refuses in one group, each finding naming the group and
// the entry. Only the tables this group creates are new: an earlier group's
// table holds rows by the time this one applies, so its index is built online.
func sidecarGroupFindings(groupName string, group []any) []string {
	createdTables := map[string]bool{}
	for _, migration := range group {
		if value, ok := migration.(*SqlMigration); ok {
			for _, match := range sidecarGroupCreateTablePattern.FindAllStringSubmatch(value.sql, -1) {
				createdTables[strings.ToLower(match[1])] = true
			}
		}
	}
	findings := []string{}
	finding := func(index int, format string, arguments ...any) {
		findings = append(findings, fmt.Sprintf("%s entry %d: %s", groupName, index, fmt.Sprintf(format, arguments...)))
	}
	refuseStatement := func(index int, sql string) {
		if match := sidecarGroupDestroyPattern.FindStringSubmatch(sql); match != nil {
			finding(index, "%s what the running release may use", strings.ToUpper(match[1]))
		}
		if sidecarGroupSetNotNullPattern.MatchString(sql) {
			finding(index, "sets a column NOT NULL, where §19.7 takes a validated NOT VALID check")
		}
	}
	for index, migration := range group {
		switch value := migration.(type) {
		case *SqlMigration:
			refuseStatement(index, value.sql)
			for _, match := range sidecarGroupIndexPattern.FindAllStringSubmatch(value.sql, -1) {
				if table := strings.ToLower(match[1]); !createdTables[table] {
					finding(index, "indexes the existing table %s in a transaction, where §19.7 builds it online", table)
				}
			}
			replaced := sidecarGroupFunctions(value.sql)
			for _, name := range slices.Sorted(maps.Keys(replaced)) {
				registered, ok := sidecarGroupRegisteredFunction(name)
				if !ok {
					// a function the group adds, which the running release never calls
					continue
				}
				conditions, ok := sidecarGroupWidenedConditions(registered, replaced[name])
				if !ok || registered == "" {
					finding(index, "replaces the function %s the running release calls outside a conditional arm that keeps its registered text", name)
					continue
				}
				for _, condition := range conditions {
					if !slices.Contains(sidecarGroupRefusedConditions[name], condition) {
						finding(index, "changes the function %s under the condition %q, which the registered function is not shown to refuse", name, condition)
					}
				}
			}
		case *OnlineSqlMigration:
			refuseStatement(index, value.sql)
			refuseStatement(index, value.auditSql)
			for _, name := range slices.Sorted(maps.Keys(sidecarGroupFunctions(value.sql + "\n" + value.auditSql))) {
				if _, ok := sidecarGroupRegisteredFunction(name); ok {
					finding(index, "replaces the function %s the running release calls outside a transaction", name)
				}
			}
			built := sidecarGroupIndexNamePattern.FindStringSubmatch(value.sql)
			if built == nil {
				finding(index, "runs online without CREATE INDEX CONCURRENTLY, so it locks the table it changes")
			}
			// a restart step drops only what an interrupted build of this index left
			if value.recoverySql != "" {
				recovered := sidecarGroupRecoveryPattern.FindStringSubmatch(value.recoverySql)
				if recovered == nil || built == nil || !strings.EqualFold(recovered[1], built[1]) {
					finding(index, "recovers with %q, which drops more than its own interrupted index", value.recoverySql)
				}
			}
		case *CodeMigration:
			// a group's code migrations are its marker, which changes nothing
		default:
			finding(index, "is a %T, which no group holds", migration)
		}
	}
	return findings
}

// Expects findings for a group that refuses, and none for its twin.
func requireSidecarGroupFindings(t *testing.T, refused []any, twin []any, want string) {
	t.Helper()
	findings := sidecarGroupFindings("MGx", refused)
	if !slices.ContainsFunc(findings, func(finding string) bool { return strings.Contains(finding, want) }) {
		t.Errorf("findings %q lack %q", findings, want)
	}
	if findings := sidecarGroupFindings("MGx", twin); len(findings) != 0 {
		t.Errorf("the twin is refused: %q", findings)
	}
}

func TestSidecarGroupCheckRefusesADrop(t *testing.T) {
	requireSidecarGroupFindings(t,
		[]any{newSqlMigration(`ALTER TABLE network_client DROP COLUMN description`)},
		[]any{newSqlMigration(`ALTER TABLE network_client ADD COLUMN description_v2 varchar(64) NULL`)},
		"DROP what the running release may use",
	)
	requireSidecarGroupFindings(t,
		[]any{newSqlMigration(`ALTER TABLE network_client RENAME COLUMN description TO label`)},
		[]any{newSqlMigration(`ALTER TABLE network_client ADD COLUMN label varchar(64) NULL`)},
		"RENAME what the running release may use",
	)
	requireSidecarGroupFindings(t,
		[]any{newOnlineSqlMigration(`DROP INDEX CONCURRENTLY network_client_network_id`, `DROP INDEX network_client_network_id`)},
		[]any{newOnlineSqlMigration(`CREATE INDEX CONCURRENTLY network_client_x ON network_client (x)`, `CREATE INDEX network_client_x ON network_client (x)`)},
		"DROP what the running release may use",
	)
}

// acl_subject_client_id's path (§19.7): a NOT VALID check validated later, never
// SET NOT NULL.
func TestSidecarGroupCheckRefusesSetNotNull(t *testing.T) {
	requireSidecarGroupFindings(t,
		[]any{newSqlMigration(`ALTER TABLE network_client ALTER COLUMN acl_subject_client_id SET NOT NULL`)},
		[]any{
			newSqlMigration(`ALTER TABLE network_client ADD CONSTRAINT network_client_acl_subject_client_id_not_null CHECK (acl_subject_client_id IS NOT NULL) NOT VALID`),
			newSqlMigration(`ALTER TABLE network_client VALIDATE CONSTRAINT network_client_acl_subject_client_id_not_null`),
		},
		"sets a column NOT NULL",
	)
	// the column keyword is optional, and the check does not lean on it
	requireSidecarGroupFindings(t,
		[]any{newSqlMigration(`ALTER TABLE network_client ALTER acl_subject_client_id SET NOT NULL`)},
		[]any{newSqlMigration(`ALTER TABLE network_client ADD COLUMN acl_subject_client_id uuid NULL`)},
		"sets a column NOT NULL",
	)
}

// An index on a table the group does not create is built online. The group's own
// tables, still empty when it applies, take a plain one.
func TestSidecarGroupCheckRefusesAPlainIndexOnAnExistingTable(t *testing.T) {
	requireSidecarGroupFindings(t,
		[]any{newSqlMigration(`CREATE INDEX transfer_balance_team_credit ON transfer_balance (network_id) WHERE team_credit`)},
		[]any{newOnlineSqlMigration(
			`CREATE INDEX CONCURRENTLY transfer_balance_team_credit ON transfer_balance (network_id) WHERE team_credit`,
			`CREATE INDEX transfer_balance_team_credit ON transfer_balance (network_id) WHERE team_credit`,
		)},
		"indexes the existing table transfer_balance in a transaction",
	)
	requireSidecarGroupFindings(t,
		[]any{newSqlMigration(`CREATE UNIQUE INDEX ON network_client (acl_subject_client_id)`)},
		[]any{
			newSqlMigration(`CREATE TABLE network_acl_head (network_id uuid NOT NULL, n bigint NOT NULL, PRIMARY KEY (network_id))`),
			newSqlMigration(`CREATE UNIQUE INDEX ON network_acl_head (n, network_id)`),
		},
		"indexes the existing table network_client in a transaction",
	)
}

func TestSidecarGroupCheckRefusesAnOnlineIndexThatLocks(t *testing.T) {
	requireSidecarGroupFindings(t,
		[]any{newOnlineSqlMigration(`CREATE INDEX network_client_x ON network_client (x)`, `CREATE INDEX network_client_x ON network_client (x)`)},
		[]any{newOnlineSqlMigration(`CREATE INDEX CONCURRENTLY network_client_x ON network_client (x)`, `CREATE INDEX network_client_x ON network_client (x)`)},
		"runs online without CREATE INDEX CONCURRENTLY",
	)
}

func TestSidecarGroupCheckRefusesARecoveryThatDropsAnotherIndex(t *testing.T) {
	requireSidecarGroupFindings(t,
		[]any{newRestartableOnlineSqlMigration(
			`DROP INDEX CONCURRENTLY IF EXISTS network_client_network_id`,
			`CREATE INDEX CONCURRENTLY network_client_x ON network_client (x)`,
			`CREATE INDEX network_client_x ON network_client (x)`,
		)},
		[]any{newRestartableOnlineSqlMigration(
			`DROP INDEX CONCURRENTLY IF EXISTS network_client_x`,
			`CREATE INDEX CONCURRENTLY network_client_x ON network_client (x)`,
			`CREATE INDEX network_client_x ON network_client (x)`,
		)},
		"drops more than its own interrupted index",
	)
}

// The registered source graph guard with its repository count rewritten, as a
// group entry.
func sidecarGroupSourceGraphTwin(t *testing.T, count string) any {
	t.Helper()
	anchor := "(SELECT count(*) FROM jsonb_object_keys(commits))=10"
	if strings.Count(competitionSourceRetirementGraph, anchor) != 1 {
		t.Fatal("the registered source graph guard lost its count")
	}
	return newSqlMigration(strings.Replace(competitionSourceRetirementGraph, anchor,
		"(SELECT count(*) FROM jsonb_object_keys(commits))="+count, 1))
}

// A group may replace a registered function only to widen it. The cleanup step
// that requires sidecar narrows what the running release writes, so no group
// holds it.
func TestSidecarGroupCheckRefusesANarrowedFunction(t *testing.T) {
	permissive := []any{newSqlMigration(competitionSidecarSourceMigration())}
	outside := "replaces the function competition_v2_source_graph_valid the running release calls outside a conditional arm"

	// eleven required: the ten the running release sends stop passing
	requireSidecarGroupFindings(t, []any{sidecarGroupSourceGraphTwin(t, "11")}, permissive, outside)
	narrowed := strings.Replace(competitionSourceRetirementGraph, "'warp','gvisor']", "'warp','gvisor','sidecar']", 1)
	if narrowed == competitionSourceRetirementGraph {
		t.Fatal("the registered source graph guard lost its names")
	}
	requireSidecarGroupFindings(t, []any{newSqlMigration(narrowed)}, permissive, outside)

	// the arms the other way round: ten is required of a graph with sidecar and
	// eleven of one without
	requireSidecarGroupFindings(t,
		[]any{sidecarGroupSourceGraphTwin(t, "(CASE WHEN commits ? 'sidecar' THEN 10 ELSE 11 END)")},
		[]any{sidecarGroupSourceGraphTwin(t, competitionSidecarSourceGraphCount)},
		outside,
	)

	// a conditional arm under a condition nothing shows the registered guard to
	// refuse, here one that always holds
	requireSidecarGroupFindings(t,
		[]any{sidecarGroupSourceGraphTwin(t, "(CASE WHEN commits ? 'server' THEN 11 ELSE 10 END)")},
		[]any{sidecarGroupSourceGraphTwin(t, competitionSidecarSourceGraphCount)},
		`under the condition "commits ? 'server'", which the registered function is not shown to refuse`,
	)

	// the permissive counts with one more change beside them
	loosened := strings.Replace(competitionSidecarSourceMigration(), " OR COALESCE(intent->>'manifest','')=''", "", 1)
	if loosened == competitionSidecarSourceMigration() {
		t.Fatal("the source authorization lost its manifest check")
	}
	requireSidecarGroupFindings(t, []any{newSqlMigration(loosened)}, permissive,
		"replaces the function competition_v2_source_authorized the running release calls outside a conditional arm")

	// a replacement outside a transaction, whatever its text
	requireSidecarGroupFindings(t,
		[]any{newOnlineSqlMigration(competitionSidecarSourceMigration(), competitionSidecarSourceMigration())},
		permissive,
		"replaces the function competition_v2_source_graph_valid the running release calls outside a transaction",
	)
}

// A function the group itself adds is no replacement: no registered migration
// creates it, so the running release never calls it.
func TestSidecarGroupCheckAdmitsANewFunction(t *testing.T) {
	added := []any{newSqlMigration(`
		CREATE FUNCTION sidecar_group_check_twin() RETURNS trigger LANGUAGE plpgsql AS $guard$
		BEGIN
		 RETURN OLD;
		END
		$guard$;
	`)}
	if findings := sidecarGroupFindings("MGx", added); len(findings) != 0 {
		t.Fatalf("a new function is refused: %q", findings)
	}
}

// The two texts differ only in conditional arms that keep the registered
// fragment, and the conditions come back in order.
func TestSidecarGroupWidenedConditions(t *testing.T) {
	for _, c := range []struct {
		registered  string
		replacement string
		conditions  []string
		refused     bool
	}{
		{registered: "a=10 AND b<>11", replacement: "a=10 AND b<>11", conditions: []string{}},
		{registered: "a=10 AND b<>11", replacement: "a=(CASE WHEN c THEN 11 ELSE 10 END) AND b<>(CASE WHEN d THEN 12 ELSE 11 END)", conditions: []string{"c", "d"}},
		// an arm the registered text already holds is unchanged text
		{registered: "a=(CASE WHEN c THEN 11 ELSE 10 END)", replacement: "a=(CASE WHEN c THEN 11 ELSE 10 END)", conditions: []string{}},
		{registered: "a=10", replacement: "a=11", refused: true},
		{registered: "a=10", replacement: "a=(CASE WHEN c THEN 10 ELSE 11 END)", refused: true},
		{registered: "a=10 AND b", replacement: "a=(CASE WHEN c THEN 11 ELSE 10 END)", refused: true},
		{registered: "a=10", replacement: "a=(CASE WHEN c THEN 11 ELSE 10 END) OR true", refused: true},
		{registered: "a=10", replacement: "true OR a=(CASE WHEN c THEN 11 ELSE 10 END)", refused: true},
	} {
		conditions, ok := sidecarGroupWidenedConditions(c.registered, c.replacement)
		if ok == c.refused || (ok && !slices.Equal(conditions, c.conditions)) {
			t.Errorf("%q to %q: conditions %q, ok %t", c.registered, c.replacement, conditions, ok)
		}
	}
}
