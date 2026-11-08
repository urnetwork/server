package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// FS-55's harness on fixture releases over a real scratch database, its guard,
// its report, and, when asked, a run on server binaries built from git. The
// database tests run through .stack/server-test.sh, which sources test-env.sh.

// Asks TestFs55OnServerBinaries to build and run real server binaries, which
// takes minutes: -fs55.base=<ref> -fs55.group=MG<n>.
var (
	fs55RealBase  = flag.String("fs55.base", "", "N's ref for a run on server binaries; empty skips it")
	fs55RealGroup = flag.String("fs55.group", "MG0", "the group a run on server binaries checks")
)

// A fixture release: SQL migrations, a startup write, and an HTTP server whose
// routes run the release's query shapes on the scratch database. It stands in
// for a server binary so the harness's phases are shown on a real schema in a
// test's time.
type fixtureRelease struct {
	name        string
	migrations  []string
	startSql    string
	routeSqls   map[string]string
	queryShapes []string
	starts      atomic.Int32
	lastYaml    atomic.Pointer[string]
}

func (self *fixtureRelease) Name() string {
	return self.name
}

func (self *fixtureRelease) QueryShapes() []string {
	return self.queryShapes
}

// Applies each migration not yet applied, as a release's migrate does: a
// fixture's statements are idempotent, so N+1's include N's.
func (self *fixtureRelease) Migrate(ctx context.Context) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("%v", r)
		}
	}()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		for _, sql := range self.migrations {
			server.RaisePgResult(tx.Exec(ctx, sql))
		}
	})
	return nil
}

func (self *fixtureRelease) Start(ctx context.Context, sidecarYaml string) (process fs55Process, returnErr error) {
	self.starts.Add(1)
	self.lastYaml.Store(&sidecarYaml)
	if self.startSql != "" {
		if err := fixtureExec(ctx, self.startSql); err != nil {
			return nil, fmt.Errorf("its startup write: %w", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for route, sql := range self.routeSqls {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if err := fixtureExec(r.Context(), sql); err != nil {
				http.Error(w, "query failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	}
	return &fixtureProcess{server: httptest.NewServer(mux)}, nil
}

// Runs one statement on the scratch database, a panic returned as its error.
func fixtureExec(ctx context.Context, sql string) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("%v", r)
		}
	}()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, sql))
	})
	return nil
}

type fixtureProcess struct {
	server *httptest.Server
}

func (self *fixtureProcess) BaseUrl() string {
	return self.server.URL
}

func (self *fixtureProcess) Stop() error {
	self.server.Close()
	return nil
}

// Plans a query on the scratch database with sequential scans off, as a generic
// plan, since a shape's parameters have no values.
func fixtureExplain(ctx context.Context, query string) error {
	return fixtureExec(ctx, "SET LOCAL enable_seqscan = off; EXPLAIN (GENERIC_PLAN) "+query)
}

// N: a table its routes read and write, and an instance row each start writes.
func fixtureN() *fixtureRelease {
	return &fixtureRelease{
		name: "N",
		migrations: []string{
			`CREATE TABLE IF NOT EXISTS fs55_item (item_id bigint PRIMARY KEY, name text NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS fs55_instance (instance_id bigserial PRIMARY KEY, host text NULL)`,
		},
		startSql: `INSERT INTO fs55_instance DEFAULT VALUES`,
		routeSqls: map[string]string{
			"GET /items":  `SELECT item_id, name FROM fs55_item WHERE item_id = 1`,
			"POST /items": `INSERT INTO fs55_item (item_id, name) VALUES ((SELECT COALESCE(MAX(item_id), 0) + 1 FROM fs55_item), 'smoke')`,
		},
		queryShapes: []string{
			`SELECT item_id, name FROM fs55_item WHERE item_id = $1`,
		},
	}
}

func fixtureRoutes() []fs55Route {
	return []fs55Route{
		{method: http.MethodGet, path: "/status", wantCodes: []int{http.StatusOK}},
		{method: http.MethodGet, path: "/items", wantCodes: []int{http.StatusOK}},
		{method: http.MethodPost, path: "/items", wantCodes: []int{http.StatusOK}},
	}
}

// N+1: N's schema with what the fixture group adds, and the group's own route.
func fixtureN1(groupMigrations ...string) *fixtureRelease {
	n := fixtureN()
	release := &fixtureRelease{
		name:       "N+1",
		migrations: append(slices.Clone(n.migrations), groupMigrations...),
		startSql:   n.startSql,
		routeSqls:  map[string]string{},
	}
	for route, sql := range n.routeSqls {
		release.routeSqls[route] = sql
	}
	release.routeSqls["GET /group"] = `SELECT count(*) FROM fs55_group`
	return release
}

func fixturePlan(n *fixtureRelease, n1 *fixtureRelease) *fs55Plan {
	return &fs55Plan{
		groupName:   "MGx",
		n:           n,
		n1:          n1,
		routes:      fixtureRoutes(),
		groupRoutes: []fs55Route{{method: http.MethodGet, path: "/group", wantCodes: []int{http.StatusOK}}},
		gateYaml:    "sidecar:\n  p0_enforce: {enabled: true}\n",
		explain:     fixtureExplain,
		settings:    defaultFs55Settings(),
	}
}

func phaseOutcomes(report *fs55Report) []string {
	outcomes := []string{}
	for _, phase := range report.phases {
		outcomes = append(outcomes, phase.outcome)
	}
	return outcomes
}

// A group that adds a table and a nullable column passes all four phases: N
// serves through N+1's migration, restarts on N+1's schema, and N+1 starts with
// the gate on and answers the group's route.
func TestFs55PassesAGroupThatAddsATableAndAColumn(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		n := fixtureN()
		n1 := fixtureN1(
			`CREATE TABLE IF NOT EXISTS fs55_group (group_id bigint PRIMARY KEY)`,
			`ALTER TABLE fs55_item ADD COLUMN IF NOT EXISTS label text NULL`,
		)
		report := runFs55(context.Background(), fixturePlan(n, n1))
		if !report.Passed() {
			var out bytes.Buffer
			report.Write(&out)
			t.Fatalf("phases %v:\n%s", phaseOutcomes(report), out.String())
		}
		if n.starts.Load() != 2 || n1.starts.Load() != 1 {
			t.Fatalf("N started %d times and N+1 %d", n.starts.Load(), n1.starts.Load())
		}
		if gate := n1.lastYaml.Load(); gate == nil || !strings.Contains(*gate, "p0_enforce") {
			t.Fatal("N+1 did not start with the group's gate on")
		}
	})
}

// A group that drops a column N reads fails at the second smoke, while N serves,
// and nothing after it runs.
func TestFs55FailsAGroupThatDropsAColumnNReads(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		n1 := fixtureN1(
			`CREATE TABLE IF NOT EXISTS fs55_group (group_id bigint PRIMARY KEY)`,
			`ALTER TABLE fs55_item DROP COLUMN IF EXISTS name`,
		)
		report := runFs55(context.Background(), fixturePlan(fixtureN(), n1))
		if got, want := phaseOutcomes(report), []string{fs55Passed, fs55Failed, fs55NotRun, fs55NotRun}; !slices.Equal(got, want) {
			t.Fatalf("phases %v, want %v", got, want)
		}
		if detail := strings.Join(report.phases[1].details, "\n"); !strings.Contains(detail, "N's smoke did not hold") {
			t.Fatalf("phase 2 failed for another reason:\n%s", detail)
		}
	})
}

// A group that tightens a constraint N's startup write breaks passes while N
// serves, and fails at the rollback, when N restarts on N+1's schema.
func TestFs55FailsAGroupThatTightensAConstraintAtTheRollback(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		n1 := fixtureN1(
			`CREATE TABLE IF NOT EXISTS fs55_group (group_id bigint PRIMARY KEY)`,
			`UPDATE fs55_instance SET host = 'fixture.example' WHERE host IS NULL`,
			`ALTER TABLE fs55_instance ALTER COLUMN host SET NOT NULL`,
		)
		report := runFs55(context.Background(), fixturePlan(fixtureN(), n1))
		if got, want := phaseOutcomes(report), []string{fs55Passed, fs55Passed, fs55Failed, fs55NotRun}; !slices.Equal(got, want) {
			t.Fatalf("phases %v, want %v", got, want)
		}
		if detail := strings.Join(report.phases[2].details, "\n"); !strings.Contains(detail, "did not restart on N+1's schema") {
			t.Fatalf("the rollback failed for another reason:\n%s", detail)
		}
	})
}

// N's query shapes plan on N+1's schema with sequential scans off, and a shape
// that no longer plans fails phase 2 even when every route held.
func TestFs55PlansNsQueryShapesOnTheMigratedSchema(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		n := fixtureN()
		n.queryShapes = append(n.queryShapes, `SELECT item_id FROM fs55_item WHERE retired_label = $1`)
		n1 := fixtureN1(`CREATE TABLE IF NOT EXISTS fs55_group (group_id bigint PRIMARY KEY)`)
		report := runFs55(context.Background(), fixturePlan(n, n1))
		if got, want := phaseOutcomes(report), []string{fs55Passed, fs55Failed, fs55NotRun, fs55NotRun}; !slices.Equal(got, want) {
			t.Fatalf("phases %v, want %v", got, want)
		}
		if detail := strings.Join(report.phases[1].details, "\n"); !strings.Contains(detail, "does not plan on N+1's schema") {
			t.Fatalf("phase 2 failed for another reason:\n%s", detail)
		}
	})
}

// Writes a vault home whose local resources name the given endpoints and no
// credential.
func writeGuardVault(t *testing.T, pgAuthority string, redisAuthority string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, authority := range map[string]string{"pg.yml": pgAuthority, "redis.yml": redisAuthority} {
		data, err := yaml.Marshal(map[string]string{"authority": authority})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "local", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// The guard refuses, before any build or dial, an attestation or vault naming a
// loopback port that can be a production data tunnel or any endpoint but the
// private stack's, and passes the private stack itself.
func TestFs55GuardRefusesAnythingButThePrivateStack(t *testing.T) {
	private := []string{
		"WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY=127.0.0.1:55432",
		"WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY=127.0.0.1:56379",
	}
	good := append(slices.Clone(private), "WARP_TEST_ENV_ATTESTED_ADDRS=127.0.0.1:55432,127.0.0.1:56379")
	if err := checkFs55Environment(good, writeGuardVault(t, "127.0.0.1:55432", "127.0.0.1:56379")); err != nil {
		t.Fatalf("the private stack is refused: %s", err)
	}
	for _, c := range []struct {
		name    string
		environ []string
		vault   string
		want    string
	}{
		{name: "no attestation", environ: private, want: "no attestation"},
		{name: "no private stack", environ: []string{"WARP_TEST_ENV_ATTESTED_ADDRS=127.0.0.1:55432"}, want: "authorities are not set"},
		{name: "an attested tunnel port", environ: append(slices.Clone(private), "WARP_TEST_ENV_ATTESTED_ADDRS=127.0.0.1:5432"), want: "production data tunnel"},
		{name: "an attested Redis tunnel", environ: append(slices.Clone(private), "WARP_TEST_ENV_ATTESTED_ADDRS=127.0.0.1:6390"), want: "production data tunnel"},
		{name: "a vault naming pgbouncer's port", environ: good, vault: writeGuardVault(t, "127.0.0.1:6432", "127.0.0.1:56379"), want: "production data tunnel"},
		{name: "a vault naming another host", environ: good, vault: writeGuardVault(t, "192.0.2.10:55432", "127.0.0.1:56379"), want: "not one of the private stack's"},
		{name: "a vault naming localhost's tunnel", environ: good, vault: writeGuardVault(t, "127.0.0.1:55432", "localhost:6379"), want: "production data tunnel"},
	} {
		err := checkFs55Environment(c.environ, c.vault)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// A refused port stops the command before anything is built or dialed: it
// returns at once with its refusal, and no worktree appears.
func TestFs55CommandStopsAtARefusedPortBeforeAnyWork(t *testing.T) {
	t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "127.0.0.1:55432")
	t.Setenv("WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY", "127.0.0.1:56379")
	t.Setenv("WARP_TEST_ENV_ATTESTED_ADDRS", "127.0.0.1:55432,127.0.0.1:56379")
	work := t.TempDir()
	var out, errOut bytes.Buffer
	code := run([]string{
		"-base", "HEAD", "-group", "MG0", "-server", "../..", "-work", work,
		"-vault", writeGuardVault(t, "127.0.0.1:5432", "127.0.0.1:56379"), "-config", t.TempDir(),
	}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "production data tunnel") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Fatalf("the refused run left %d entries in its work directory (%v)", len(entries), err)
	}
}

// The report's form on a recorded run, which server/local/sidecar-gate reads.
func TestFs55ReportOnARecordedRun(t *testing.T) {
	report := &fs55Report{
		groupName: "MG1",
		nName:     "N",
		n1Name:    "N+1",
		phases: []*fs55PhaseResult{
			{number: 1, name: "migrate with N and start N", outcome: fs55Passed, details: []string{"N GET /status: 200 holds"}},
			{number: 2, name: "migrate with N+1 while N serves", outcome: fs55Failed, details: []string{"N's smoke did not hold: GET /items: 500 does not hold, want [200]"}},
			{number: 3, name: "restart N on N+1's schema (rollback)", outcome: fs55NotRun},
			{number: 4, name: "start N+1 with the group's gate on", outcome: fs55NotRun},
		},
	}
	var out bytes.Buffer
	report.Write(&out)
	want := "## FS-55: MG1, N to N+1\n\n" +
		"| phase | outcome |\n|---|---|\n" +
		"| 1. migrate with N and start N | passed |\n" +
		"| 2. migrate with N+1 while N serves | failed |\n" +
		"| 3. restart N on N+1's schema (rollback) | not run |\n" +
		"| 4. start N+1 with the group's gate on | not run |\n" +
		"\n### 1. migrate with N and start N\n\n- N GET /status: 200 holds\n" +
		"\n### 2. migrate with N+1 while N serves\n\n- N's smoke did not hold: GET /items: 500 does not hold, want [200]\n" +
		"\nFS-55 failed\n"
	if out.String() != want {
		t.Fatalf("report:\n%s\nwant:\n%s", out.String(), want)
	}
	if report.Passed() {
		t.Fatal("a failed run reads passed")
	}
}

// N+1 promotes every pending group up to its own, in order, and a group the base
// does not hold pending is refused.
func TestFs55PromotesEveryGroupThroughItsOwn(t *testing.T) {
	groups, err := fs55PendingGroups("../..", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 || groups[0] != "MG0" {
		t.Fatalf("pending groups %v", groups)
	}
	through, err := fs55GroupsThrough("MG1", groups)
	if err != nil || !slices.Equal(through, []string{"MG0", "MG1"}) {
		t.Fatalf("through MG1: %v %v", through, err)
	}
	if _, err := fs55GroupsThrough("MG3", groups); err == nil {
		t.Fatal("MG3, which creates nothing, was promoted")
	}
}

// Writes the vault the built binaries read: the scratch database and Redis db
// that this test's TestEnv holds, as files, and the rest of the private stack's
// vault by link. The files hold the private stack's test credentials and live
// only in the test's own directory.
func writeScratchVault(t testing.TB) string {
	t.Helper()
	baseHome := os.Getenv("WARP_VAULT_HOME")
	if baseHome == "" {
		t.Fatal("no WARP_VAULT_HOME: run through .stack/server-test.sh")
	}
	home := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(home, "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(baseHome, "local"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target := filepath.Join(home, "local", entry.Name())
		switch entry.Name() {
		case "pg.yml", "pg_maintenance.yml", "redis.yml":
			data, err := yaml.Marshal(server.Vault.RequireSimpleResource(entry.Name()).Parse())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.Symlink(filepath.Join(baseHome, "local", entry.Name()), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	return home
}

// FS-55 on server binaries built from -fs55.base: N with no tag, N+1 with the
// pending groups through -fs55.group promoted, on this test's scratch database.
// It builds six binaries and runs api, so it runs only when asked.
func TestFs55OnServerBinaries(t *testing.T) {
	if *fs55RealBase == "" {
		t.Skip("give -fs55.base=<ref> to build and run server binaries")
	}
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		vaultHome := writeScratchVault(t)
		if err := checkFs55Environment(os.Environ(), vaultHome); err != nil {
			t.Fatalf("refused: %s", err)
		}
		configHome := os.Getenv("WARP_CONFIG_HOME")
		if configHome == "" {
			t.Fatal("no WARP_CONFIG_HOME: run through .stack/server-test.sh")
		}
		report, err := runFs55OnServerBinaries(context.Background(), &fs55ServerOptions{
			serverDirectory: "../..",
			base:            *fs55RealBase,
			groupName:       *fs55RealGroup,
			vaultHome:       vaultHome,
			configHome:      configHome,
			settings:        defaultFs55Settings(),
			explain:         fixtureExplain,
		})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		report.Write(&out)
		t.Log(out.String())
		if !report.Passed() {
			t.Fatal(errors.New("FS-55 failed on server binaries"))
		}
	})
}
