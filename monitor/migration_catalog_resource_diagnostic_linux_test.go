// Explicit isolated diagnostics measure the complete emitted catalog query.
// This does not change production predicates, connection settings or budgets.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The operator supplies a fresh private evidence directory; all names and
// session changes below are closed and apply only to TestEnv's own database.
func TestMigrationCatalogResourceDiagnostic(t *testing.T) {
	if os.Getenv("URNETWORK_MIGRATION_RESOURCE_DIAGNOSTIC") != "1" {
		t.Skip("requires explicitly owned isolated PostgreSQL resource diagnostic")
	}
	directory := os.Getenv("URNETWORK_MIGRATION_RESOURCE_EVIDENCE")
	resolved, resolveErr := filepath.EvalSymlinks(directory)
	info, statErr := os.Lstat(directory)
	if resolveErr != nil || statErr != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || resolved != directory ||
		!info.IsDir() || info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Fatal("diagnostic evidence requires an owned private physical directory")
	}
	write := func(name string, content []byte) {
		t.Helper()
		file, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			t.Fatal("create exclusive diagnostic evidence", name, err)
		}
		_, writeErr := file.Write(content)
		syncErr, closeErr := file.Sync(), file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			t.Fatal("retain diagnostic evidence", name, writeErr, syncErr, closeErr)
		}
	}
	_, query, columns := migrationPublicColumnQueries(t)
	if len(query) > 32*1024*1024 {
		t.Fatal("compiled diagnostic query exceeds its bounded evidence inventory")
	}
	write("catalog-query.sql", []byte(query))
	digest := sha256.Sum256([]byte(query))
	version := server.CompetitionDistributedEvaluationMigrationVersion()
	t.Logf("catalog_resource query_bytes=%d query_sha256=%s columns=%d compiled_head=%d failed_prefix=%d", len(query), hex.EncodeToString(digest[:]), columns, server.MigrationCount(), version-1)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx, stop := context.WithTimeout(tb.Context(), 3*time.Minute)
		defer stop()
		server.ApplyDbMigrationsUpTo(ctx, version-1)
		var baseline []Row
		for _, mode := range []string{"off", "on"} {
			server.Db(ctx, func(conn server.PgConn) {
				if _, err := conn.Exec(ctx, "SET jit="+mode+"; SET statement_timeout='30s'", pgx.QueryExecModeSimpleProtocol); err != nil {
					tb.Fatal("configure owned diagnostic session", mode, err)
				}
				defer func() {
					cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer cancel()
					if _, err := conn.Exec(cleanupCtx, "RESET jit; RESET statement_timeout", pgx.QueryExecModeSimpleProtocol); err != nil {
						tb.Error("restore owned diagnostic session", mode, err)
					}
				}()
				var backend int
				if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backend); err != nil {
					tb.Fatal(err)
				}
				tb.Logf("catalog_resource phase=explain jit=%s backend=%d at=%s", mode, backend, time.Now().UTC().Format(time.RFC3339Nano))
				var plan []byte
				started := time.Now()
				if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON, SETTINGS, SUMMARY) "+query, pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
					tb.Fatal("explain complete catalog query", mode, err)
				}
				if len(plan) > 64*1024*1024 {
					tb.Fatal("diagnostic plan exceeds its bounded evidence inventory")
				}
				write("explain-jit-"+mode+".json", plan)
				decoder := json.NewDecoder(bytes.NewReader(plan))
				decoder.UseNumber()
				var documents []map[string]any
				if err := decoder.Decode(&documents); err != nil || len(documents) != 1 {
					tb.Fatal("decode exact diagnostic plan", mode, err)
				}
				root, present := documents[0]["Plan"].(map[string]any)
				if !present || root["Total Cost"] == nil {
					tb.Fatal("diagnostic plan omitted its root cost", mode)
				}
				nodes := 0
				var count func(any)
				count = func(value any) {
					switch value := value.(type) {
					case map[string]any:
						if _, present := value["Node Type"]; present {
							nodes++
						}
						for _, child := range value {
							count(child)
						}
					case []any:
						for _, child := range value {
							count(child)
						}
					}
				}
				count(documents[0]["Plan"])
				planDigest := sha256.Sum256(plan)
				tb.Logf("catalog_resource phase=explained jit=%s elapsed=%s plan_bytes=%d plan_sha256=%s nodes=%d total_cost=%v planning_ms=%v jit_plan=%v", mode, time.Since(started), len(plan), hex.EncodeToString(planDigest[:]), nodes, root["Total Cost"], documents[0]["Planning Time"], documents[0]["JIT"])
				tb.Logf("catalog_resource phase=execute jit=%s backend=%d at=%s", mode, backend, time.Now().UTC().Format(time.RFC3339Nano))
				started = time.Now()
				result, err := conn.Query(ctx, query, pgx.QueryExecModeSimpleProtocol)
				if err != nil {
					tb.Fatal("execute complete catalog query", mode, err)
				}
				defer result.Close()
				rows := []Row{}
				for result.Next() {
					values, err := result.Values()
					if err != nil {
						tb.Fatal("decode complete catalog row", mode, err)
					}
					row := make(Row, len(values))
					for index, value := range values {
						switch value := value.(type) {
						case nil:
							row[index] = ""
						case bool:
							row[index] = "f"
							if value {
								row[index] = "t"
							}
						default:
							row[index] = fmt.Sprint(value)
						}
					}
					rows = append(rows, row)
				}
				if err := result.Err(); err != nil || len(rows) != 1 || len(rows[0]) != columns {
					tb.Fatal("complete catalog census failed or changed row shape", mode, err, len(rows))
				}
				encoded, err := json.Marshal(rows)
				if err != nil {
					tb.Fatal(err)
				}
				write("artifact-row-jit-"+mode+".json", encoded)
				if mode == "off" {
					baseline = rows
				} else if !reflect.DeepEqual(baseline, rows) {
					tb.Fatal("JIT session selection changed the complete artifact census")
				}
				tb.Logf("catalog_resource phase=executed jit=%s elapsed=%s columns=%d", mode, time.Since(started), columns)
			}, server.OptNoRetry())
		}
	})
}
