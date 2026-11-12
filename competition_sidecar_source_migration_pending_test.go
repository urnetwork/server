//go:build sidecar_pending

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// The competition's permissive step in MG0 (QD-128, QD-752), on a private
// database: what the registered guards admit they still admit once it applies,
// and the one graph it adds is the ten with sidecar.

var competitionSidecarTestRepositoryNames = []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"}

// A graph of the names, each pinned to its own well-formed commit.
func competitionSidecarTestCommits(names ...string) map[string]any {
	commits := map[string]any{}
	for _, name := range names {
		digest := sha256.Sum256([]byte(name))
		commits[name] = hex.EncodeToString(digest[:20])
	}
	return commits
}

type competitionSidecarGraphCase struct {
	name string
	// the graph's JSON
	commits string
	// whether the registered guard and the permissive one admit it
	registered bool
	permissive bool
}

// Graphs around the two that pass: the ten, and the ten with sidecar.
func competitionSidecarGraphCases(t testing.TB) []competitionSidecarGraphCase {
	t.Helper()
	encode := func(commits any) string {
		encoded, err := json.Marshal(commits)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	with := func(base map[string]any, name string, commit any) map[string]any {
		commits := maps.Clone(base)
		commits[name] = commit
		return commits
	}
	ten := competitionSidecarTestCommits(competitionSidecarTestRepositoryNames...)
	eleven := competitionSidecarTestCommits(append(slices.Clone(competitionSidecarTestRepositoryNames), "sidecar")...)
	cases := []competitionSidecarGraphCase{
		{name: "the ten", commits: encode(ten), registered: true, permissive: true},
		{name: "the ten with sidecar", commits: encode(eleven), permissive: true},
		{name: "the ten with an unknown eleventh", commits: encode(with(ten, "synthetic-unknown", eleven["sidecar"]))},
		{name: "the ten with the retired operator-proxy", commits: encode(with(ten, "operator-proxy", eleven["sidecar"]))},
		{name: "the ten with sidecar and an unknown twelfth", commits: encode(with(eleven, "synthetic-unknown", eleven["sidecar"]))},
		{name: "the ten with a malformed commit", commits: encode(with(ten, "gvisor", "invalid-commit"))},
		{name: "the ten with a malformed sidecar commit", commits: encode(with(ten, "sidecar", "invalid-commit"))},
		{name: "the ten with a short sidecar commit", commits: encode(with(ten, "sidecar", strings.Repeat("a", 39)))},
		{name: "the ten with a null sidecar commit", commits: encode(with(ten, "sidecar", nil))},
		{name: "sidecar alone", commits: encode(competitionSidecarTestCommits("sidecar"))},
		{name: "no repository", commits: `{}`},
		{name: "a list of the eleven", commits: encode(append(slices.Clone(competitionSidecarTestRepositoryNames), "sidecar"))},
		{name: "a JSON null", commits: `null`},
	}
	for _, name := range competitionSidecarTestRepositoryNames {
		without := maps.Clone(ten)
		delete(without, name)
		cases = append(cases,
			competitionSidecarGraphCase{name: "the ten without " + name, commits: encode(without)},
			// ten keys again, with sidecar standing in for a live repository
			competitionSidecarGraphCase{name: "sidecar in place of " + name, commits: encode(with(without, "sidecar", eleven["sidecar"]))},
			competitionSidecarGraphCase{name: "sidecar and an unknown in place of " + name, commits: encode(with(with(without, "sidecar", eleven["sidecar"]), "synthetic-unknown", eleven["sidecar"]))},
		)
	}
	return cases
}

// What the function of that name answers for each case in each mode, keyed by
// the case's name and mode.
func competitionSidecarGraphAnswers(t testing.TB, ctx context.Context, function string, cases []competitionSidecarGraphCase) map[string]bool {
	t.Helper()
	answers := map[string]bool{}
	MaintenanceDb(ctx, func(conn PgConn) {
		for _, c := range cases {
			for _, staging := range []bool{false, true} {
				var valid bool
				Raise(conn.QueryRow(ctx, `SELECT `+function+`($1::jsonb, $2)`, c.commits, staging).Scan(&valid))
				answers[fmt.Sprintf("%s, staging %t", c.name, staging)] = valid
			}
		}
	})
	return answers
}

// The cases a replacement refuses that the registered guard admitted: empty for
// a replacement that only widens.
func competitionSidecarNarrowedCases(registered map[string]bool, replacement map[string]bool) []string {
	narrowed := []string{}
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		if registered[name] && !replacement[name] {
			narrowed = append(narrowed, name)
		}
	}
	return narrowed
}

// The permissive step is MG0's, ahead of the group's marker, so it registers with
// MG0 at M0's Gn-out and a binary that reports MG0 linked requires it.
func TestCompetitionSidecarSourceMigrationIsInMg0(t *testing.T) {
	running := MigrationCount() - len(sidecarPendingMigrations())
	version := CompetitionSidecarSourceMigrationVersion()
	marker := CodeMigrationVersion(sidecarMg0MigrationId)
	if version <= running || marker <= version {
		t.Fatalf("the permissive step is at version %d, outside MG0's %d to %d", version, running+1, marker)
	}
	held := 0
	for _, group := range sidecarPendingGroups() {
		for _, migration := range group.group {
			if value, ok := migration.(*SqlMigration); ok && value.sql == competitionSidecarSourceMigration() {
				held++
				if group.name != "MG0" {
					t.Errorf("%s holds the permissive step", group.name)
				}
			}
		}
	}
	if held != 1 {
		t.Fatalf("the pending groups hold the permissive step %d times", held)
	}
}

// The registered graph guard refuses every graph with a sidecar key, which is
// the condition the permissive step changes it under
// (sidecarGroupRefusedConditions). Once MG0 applies, every graph it admitted
// still passes, in both modes, and the only graph added is the ten with sidecar.
// The cleanup step's guard, which requires sidecar, fails the same comparison.
func TestCompetitionSidecarSourceGraphOnlyWidens(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases := competitionSidecarGraphCases(t)
		running := MigrationCount() - len(sidecarPendingMigrations())
		ApplyDbMigrationsUpTo(ctx, running)

		registered := competitionSidecarGraphAnswers(t, ctx, "competition_v2_source_graph_valid", cases)
		for _, c := range cases {
			for _, staging := range []bool{false, true} {
				name := fmt.Sprintf("%s, staging %t", c.name, staging)
				if registered[name] != c.registered {
					t.Errorf("the registered guard answers %t for %s", registered[name], name)
				}
				var graph map[string]any
				if json.Unmarshal([]byte(c.commits), &graph) == nil {
					if _, sidecar := graph["sidecar"]; sidecar && registered[name] {
						t.Errorf("the registered guard admits a graph with a sidecar key: %s", name)
					}
				}
			}
		}

		ApplyDbMigrations(ctx)
		if got := DbVersion(ctx); got != MigrationCount() {
			t.Fatalf("version = %d of %d", got, MigrationCount())
		}
		permissive := competitionSidecarGraphAnswers(t, ctx, "competition_v2_source_graph_valid", cases)
		for _, c := range cases {
			for _, staging := range []bool{false, true} {
				name := fmt.Sprintf("%s, staging %t", c.name, staging)
				if permissive[name] != c.permissive {
					t.Errorf("the permissive guard answers %t for %s", permissive[name], name)
				}
			}
		}
		if narrowed := competitionSidecarNarrowedCases(registered, permissive); len(narrowed) != 0 {
			t.Errorf("the permissive guard refuses what the registered guard admitted: %q", narrowed)
		}

		// the cleanup step's guard, under another name so the database keeps MG0's
		cleanup := strings.NewReplacer(
			"competition_v2_source_graph_valid", "competition_test_cleanup_source_graph_valid",
			"jsonb_object_keys(commits))=10", "jsonb_object_keys(commits))=11",
			"'warp','gvisor']", "'warp','gvisor','sidecar']",
		).Replace(competitionSourceRetirementGraph)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, cleanup))
		})
		required := competitionSidecarGraphAnswers(t, ctx, "competition_test_cleanup_source_graph_valid", cases)
		if !required["the ten with sidecar, staging false"] || !required["the ten with sidecar, staging true"] {
			t.Fatal("the cleanup guard refuses the eleven")
		}
		narrowed := competitionSidecarNarrowedCases(registered, required)
		if !slices.Equal(narrowed, []string{"the ten, staging false", "the ten, staging true"}) {
			t.Fatalf("the comparison finds the cleanup guard narrowing %q, not the ten", narrowed)
		}
	})
}

// A baseline source intent over the commits, with a git ref for each name of
// refs and for config, and the transaction settings a baseline is checked
// against.
func competitionSidecarBaselineIntent(t testing.TB, commits map[string]any, refs []string) (string, map[string]string) {
	t.Helper()
	hex40 := func(seed string) string {
		digest := sha256.Sum256([]byte(seed))
		return hex.EncodeToString(digest[:20])
	}
	hex64 := func(seed string) string {
		digest := sha256.Sum256([]byte(seed))
		return hex.EncodeToString(digest[:])
	}
	// the guard's canonical lock: keys in order, no spaces
	names := slices.Sorted(maps.Keys(commits))
	pins := []string{}
	for _, name := range names {
		pins = append(pins, fmt.Sprintf("%q:%q", name, commits[name]))
	}
	lock := hex64(`{"schema":1,"development_snapshot":false,"repositories":{` + strings.Join(pins, ",") + `}}`)
	manifest := []byte("schema: 2\n")
	manifestDigest := sha256.Sum256(manifest)
	gitRefs := map[string]any{
		"config": map[string]any{"commit": hex40("manifest"), "expected_commit": hex40("config-expected"), "branch": "main"},
	}
	for _, name := range refs {
		gitRefs[name] = map[string]any{"commit": commits[name], "expected_commit": hex40("expected-" + name), "branch": "sim-latency"}
	}
	intent, err := json.Marshal(map[string]any{
		"environment":                "test",
		"competition_id":             "competition-test",
		"season_id":                  "season-test",
		"kind":                       "baseline",
		"generation":                 1,
		"expected_generation":        0,
		"target_epoch":               1,
		"source_generation":          0,
		"commits":                    commits,
		"git_refs":                   gitRefs,
		"source_lock_sha256":         lock,
		"manifest":                   base64.StdEncoding.EncodeToString(manifest),
		"manifest_sha256":            hex.EncodeToString(manifestDigest[:]),
		"manifest_commit":            hex40("manifest"),
		"config_expected_commit":     hex40("config-expected"),
		"source_decision_digest":     hex64("decision"),
		"publication_journal_sha256": hex64("journal"),
		"threshold_evidence_sha256":  hex64("threshold"),
		"evaluator_image_digest":     "sha256:" + hex64("image"),
		"host_qualification_sha256":  hex64("host"),
		"threshold":                  0.05,
		"prepared_at":                "2026-01-01T00:00:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := commits["server"].(string)
	return string(intent), map[string]string{
		"urnetwork.competition_v2_bootstrap_lock":   lock,
		"urnetwork.competition_v2_bootstrap_base":   server,
		"urnetwork.competition_v2_bootstrap_margin": "0.05",
		"urnetwork.competition_v2_policy_approval":  hex64("approval"),
	}
}

// Runs the source authorization on the intent in a transaction of its own, under
// the settings, and returns its refusal.
func competitionSidecarAuthorize(t testing.TB, ctx context.Context, intent string, settings map[string]string) error {
	t.Helper()
	var refusal error
	MaintenanceDb(ctx, func(conn PgConn) {
		tx, err := conn.Begin(ctx)
		Raise(err)
		defer tx.Rollback(ctx)
		for _, name := range slices.Sorted(maps.Keys(settings)) {
			RaisePgResult(tx.Exec(ctx, `SELECT set_config($1, $2, true)`, name, settings[name]))
		}
		_, refusal = tx.Exec(ctx, `SELECT competition_v2_source_authorized($1::jsonb, 'test', 'competition-test', 'season-test')`, intent)
	})
	return refusal
}

// The source authorization admits a baseline of the ten before and after MG0. A
// baseline of the ten with sidecar is refused by the registered guard, at its
// graph, and admitted after MG0 with a git ref for sidecar and for nothing else.
func TestCompetitionSidecarSourceAuthorizationOnlyWidens(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		tenNames := competitionSidecarTestRepositoryNames
		elevenNames := append(slices.Clone(tenNames), "sidecar")
		ten := competitionSidecarTestCommits(tenNames...)
		eleven := competitionSidecarTestCommits(elevenNames...)
		unknown := competitionSidecarTestCommits(append(slices.Clone(tenNames), "synthetic-unknown")...)

		graphRefusal := "v2 source mode repository graph disagrees with its explicit staging flag"
		referenceRefusal := "v2 source content or publication recovery identity is invalid"
		cases := []struct {
			name       string
			commits    map[string]any
			refs       []string
			registered string
			permissive string
		}{
			{name: "the ten", commits: ten, refs: tenNames},
			{name: "the ten with sidecar", commits: eleven, refs: elevenNames, registered: graphRefusal},
			{name: "the ten with sidecar and no sidecar ref", commits: eleven, refs: tenNames, registered: graphRefusal, permissive: referenceRefusal},
			{name: "the ten with a sidecar ref", commits: ten, refs: elevenNames, registered: referenceRefusal, permissive: referenceRefusal},
			{name: "the ten with sidecar and one more ref", commits: eleven, refs: append(slices.Clone(elevenNames), "synthetic-unknown"), registered: graphRefusal, permissive: referenceRefusal},
			{name: "the ten with an unknown eleventh", commits: unknown, refs: append(slices.Clone(tenNames), "synthetic-unknown"), registered: graphRefusal, permissive: graphRefusal},
		}
		requireRefusal := func(guard string, name string, err error, want string) {
			t.Helper()
			switch {
			case want == "" && err != nil:
				t.Errorf("the %s guard refuses %s: %v", guard, name, err)
			case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
				t.Errorf("the %s guard answers %v for %s, not %q", guard, err, name, want)
			}
		}

		running := MigrationCount() - len(sidecarPendingMigrations())
		ApplyDbMigrationsUpTo(ctx, running)
		for _, c := range cases {
			intent, settings := competitionSidecarBaselineIntent(t, c.commits, c.refs)
			err := competitionSidecarAuthorize(t, ctx, intent, settings)
			requireRefusal("registered", c.name, err, c.registered)
			if _, sidecar := c.commits["sidecar"]; sidecar && err == nil {
				t.Errorf("the registered guard admits a graph with a sidecar key: %s", c.name)
			}
		}

		ApplyDbMigrations(ctx)
		for _, c := range cases {
			intent, settings := competitionSidecarBaselineIntent(t, c.commits, c.refs)
			requireRefusal("permissive", c.name, competitionSidecarAuthorize(t, ctx, intent, settings), c.permissive)
			if c.registered == "" && c.permissive != "" {
				t.Errorf("the permissive guard is expected to refuse what the registered guard admitted: %s", c.name)
			}
		}
	})
}
