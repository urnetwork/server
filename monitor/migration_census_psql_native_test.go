// Native qualification replaces only the SSH carrier with docker exec into
// the harness's exact owned PostgreSQL container. The production psql command,
// file-reader protocol, transaction and strict parser remain unchanged.
package monitor

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// This is an explicit fixture binding, not discovery or an arbitrary default
// daemon target. TestEnv separately authenticates its generated test database.
func migrationCensusNativeFixture(t *testing.T) string {
	t.Helper()
	if os.Getenv("URNETWORK_MIGRATION_CENSUS_NATIVE_PSQL") != "1" {
		t.Skip("requires explicitly owned native psql migration-census fixture")
	}
	container := os.Getenv("URNETWORK_MIGRATION_TEST_PG_CONTAINER")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(container) {
		t.Fatal("native psql requires the exact owned PostgreSQL container id")
	}
	return container
}

// No connection is retained while the production psql observer starts. The
// generated database/role must also exist in the exact fixture container;
// wrong binding fails authentication, never falls back to another endpoint.
func migrationCensusNativeRunner(t testing.TB, ctx context.Context) *runner {
	t.Helper()
	settings := syntheticSettings(nil).withDefaults()
	cfg := configFromSignalSettings(settings)
	server.Db(ctx, func(conn server.PgConn) {
		configuration := conn.Conn().Config()
		cfg.pgUser, cfg.pgPassword, cfg.pgDb = configuration.User, configuration.Password, configuration.Database
	})
	if !regexp.MustCompile(`^[a-zA-Z0-9_]+$`).MatchString(cfg.pgUser) || !regexp.MustCompile(`^[a-zA-Z0-9_]+$`).MatchString(cfg.pgDb) || cfg.pgPassword == "" {
		t.Fatal("native fixture SQL identity is incomplete or not source-safe")
	}
	cfg.pgPort = DefaultPostgreSQLSettings().Port
	cfg.hosts[0].overlayIp = "192.0.2.72"
	return newRunner(cfg)
}

// Run one real file-reader child and physically join it. Only finite private
// errors are returned; neither command credentials nor query text are logged.
func migrationCensusNativePsql(ctx context.Context, container, command, stdin string) (string, string, error) {
	child := exec.CommandContext(ctx, "docker", "exec", "-i", container, "/bin/sh", "-c", command)
	child.WaitDelay = 5 * time.Second
	child.Stdin = strings.NewReader(stdin)
	var output, diagnostic bytes.Buffer
	child.Stdout, child.Stderr = &output, &diagnostic
	err := child.Run()
	if err != nil {
		return "", "", fmt.Errorf("owned native psql child failed: %w", err)
	}
	return output.String(), "", nil
}

// This is the exact incident prefix and complete current head, not a subset of
// columns. Run with the original 1-GiB PostgreSQL cap and retain native peak/OOM
// counters in the external harness; no statement or command budget is raised.
func TestMigrationCensusNativePsqlCompletePublishedPrefixes(t *testing.T) {
	container := migrationCensusNativeFixture(t)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, head := range []int{server.CompetitionDistributedEvaluationMigrationVersion() - 1, server.MigrationCount()} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			runner := migrationCensusNativeRunner(tb, tb.Context())
			calls := 0
			runner.runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
				calls++
				if !strings.Contains(args[len(args)-1], " -f -") || !strings.Contains(stdin, "REPEATABLE READ READ ONLY;") {
					return "", "", errors.New("native observation did not use the production bounded session")
				}
				return migrationCensusNativePsql(ctx, container, args[len(args)-1], stdin)
			}
			env := &probeEnv{cfg: runner.cfg, runner: runner, now: time.Now}
			findings, err := (migrationsProbe{}).check(tb.Context(), env)
			if err != nil || calls != 1 {
				tb.Fatal("complete native census failed or used a separate identity snapshot", head, err, calls)
			}
			for _, finding := range findings {
				if !finding.healthy && finding.class == "migration-schema-drift" {
					tb.Fatal("bounded native census changed published artifact or identity semantics", head, finding.symptom)
				}
			}
			tb.Logf("native_census head=%d complete=true physical_sessions=%d original_statement_budget_ms=%d", head, calls, runner.cfg.pgStatementTimeout/time.Millisecond)
		}
	})
}

// The child pauses after the first actual batch response. An independent
// writer commits between batches; later reads and the identity census must
// still see the original repeatable-read snapshot on the same physical pid.
func TestMigrationCensusNativePsqlKeepsOneSnapshotThroughIdentities(t *testing.T) {
	container := migrationCensusNativeFixture(t)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(tb.Context(), time.Minute)
		defer cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE migration_census_snapshot_fixture(value integer NOT NULL);
INSERT INTO migration_census_snapshot_fixture VALUES(7);
CREATE TABLE migration_catalog(migration_index integer NOT NULL,identity_sha256 text NOT NULL);
INSERT INTO migration_catalog VALUES(0,'synthetic-original-identity')`))
		})
		runner := migrationCensusNativeRunner(tb, ctx)
		settings := DefaultMigrationCensusSettings()
		settings.MaximumColumnsPerStatement = 1
		query := `WITH version AS (SELECT 1 AS value) SELECT pg_backend_pid(),
(SELECT value FROM migration_census_snapshot_fixture),pg_backend_pid(),
current_setting('transaction_isolation'),current_setting('transaction_read_only') FROM version;`
		plan, err := planMigrationCensus(query, settings)
		if err != nil {
			tb.Fatal(err)
		}
		writeNow, writerDone := make(chan struct{}), make(chan struct{})
		var writerErr error
		go func() {
			defer close(writerDone)
			select {
			case <-writeNow:
			case <-ctx.Done():
				writerErr = ctx.Err()
				return
			}
			failure := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE migration_census_snapshot_fixture SET value=9;
UPDATE migration_catalog SET identity_sha256='synthetic-new-identity'`))
				}, server.OptNoRetry())
			})
			if failure != nil {
				writerErr = errors.New("independent owned snapshot writer failed")
			}
		}()
		defer func() { cancel(); <-writerDone }()
		calls := 0
		runner.runSSH = func(childCtx context.Context, args []string, stdin string) (string, string, error) {
			calls++
			boundary := strings.Index(stdin, plan.batches[0].query+"\n")
			if boundary < 0 {
				return "", "", errors.New("native snapshot lost first batch boundary")
			}
			boundary += len(plan.batches[0].query) + 1
			child := exec.CommandContext(childCtx, "docker", "exec", "-i", container, "/bin/sh", "-c", args[len(args)-1])
			child.WaitDelay = 5 * time.Second
			input, inputErr := child.StdinPipe()
			if inputErr != nil {
				return "", "", inputErr
			}
			output, outputErr := child.StdoutPipe()
			if outputErr != nil {
				_ = input.Close()
				return "", "", outputErr
			}
			defer output.Close()
			var diagnostics bytes.Buffer
			child.Stderr = &diagnostics
			if err := child.Start(); err != nil {
				_ = input.Close()
				return "", "", err
			}
			joined := false
			defer func() {
				_ = input.Close()
				if !joined {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			}()
			if _, err := io.WriteString(input, stdin[:boundary]); err != nil {
				return "", "", err
			}
			reader := csv.NewReader(output)
			var captured strings.Builder
			encoder := csv.NewWriter(&captured)
			for index := 0; index < 2; index++ {
				record, err := reader.Read()
				if err != nil {
					return "", "", errors.New("native snapshot did not produce its initial frames")
				}
				if err := encoder.Write(record); err != nil {
					return "", "", err
				}
			}
			close(writeNow)
			select {
			case <-writerDone:
			case <-childCtx.Done():
				return "", "", childCtx.Err()
			}
			if writerErr != nil {
				return "", "", writerErr
			}
			if _, err := io.WriteString(input, stdin[boundary:]); err != nil {
				return "", "", err
			}
			_ = input.Close()
			for {
				record, err := reader.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					return "", "", err
				}
				if err := encoder.Write(record); err != nil {
					return "", "", err
				}
			}
			encoder.Flush()
			waitErr := child.Wait()
			joined = true
			if waitErr != nil {
				return "", "", errors.New("native snapshot child did not join successfully")
			}
			return captured.String(), "", encoder.Error()
		}
		observation := &migrationCensusObservation{settings: settings}
		rows, err := runner.pg(context.WithValue(ctx, migrationCensusContextKey{}, observation), query)
		if err != nil || calls != 1 || len(rows) != 1 || len(rows[0]) != 5 {
			tb.Fatal("native snapshot observation failed", err, calls)
		}
		pid, pidErr := strconv.Atoi(rows[0][0])
		if pidErr != nil || pid <= 0 || rows[0][2] != rows[0][0] || rows[0][1] != "7" || rows[0][3] != "repeatable read" || rows[0][4] != "on" ||
			!observation.catalogComplete || len(observation.catalogRows) != 1 || observation.catalogRows[0][1] != "synthetic-original-identity" {
			tb.Fatal("later batches or identities escaped the original read-only physical snapshot", rows)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var value int
			var identity string
			if err := conn.QueryRow(ctx, "SELECT value,(SELECT identity_sha256 FROM migration_catalog) FROM migration_census_snapshot_fixture").Scan(&value, &identity); err != nil || value != 9 || identity != "synthetic-new-identity" {
				tb.Fatal("snapshot writer did not positively commit both newer values", err)
			}
		})
	})
}
