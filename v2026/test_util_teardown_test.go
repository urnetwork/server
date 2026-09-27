package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The TestEnv.Run teardown is bounded: an attempt that fails an assertion
// while sibling goroutines still hold pool connections used to hang the
// whole package to its timeout (teardown's pool Close() waited forever on
// the leaked connections, and Goexit's LIFO defers meant close(done) never
// ran — the failure presented as a silent 1h+ package timeout, twice, before
// the mechanism was found). This meta-test re-creates exactly that shape in
// a subprocess with a 2s teardown bound and pins that the failure now FAILS
// fast instead of hanging.
func TestTeardownBoundedOnLeakedConnections(t *testing.T) {
	const dbNamePathEnv = "WARP_TEST_TEARDOWN_DB_NAME_PATH"
	if os.Getenv("WARP_TEST_TEARDOWN_SUBPROCESS") == "1" {
		// the inner (intentionally failing) test: single attempt, no
		// migrations, one goroutine parked on a pool connection when the
		// assertion fails
		(&TestEnv{
			ApplyDbMigrations: false,
			RerunCount:        0,
			RerunTimeout:      15 * time.Second,
		}).Run(t, func(tb testing.TB) {
			ctx := context.Background()
			acquired := make(chan struct{})
			go func() {
				Db(ctx, func(conn PgConn) {
					var datname string
					Raise(conn.QueryRow(ctx, `SELECT current_database()`).Scan(&datname))
					if _, ok := parseTestPgDbName(datname); !ok {
						panic(fmt.Errorf("invalid generated test database name %q", datname))
					}
					Raise(os.WriteFile(os.Getenv(dbNamePathEnv), []byte(datname), 0o600))
					close(acquired)
					// hold the pool connection past the attempt's death
					select {}
				})
			}()
			<-acquired
			tb.Fatal("intentional failure with a leaked pool connection")
		})
		return
	}

	// outer: run the inner in a subprocess so its failure doesn't fail us,
	// with a tight teardown bound so the abandon path runs in seconds. The
	// child's deliberately leaked connection makes its normal teardown
	// impossible, so record and remove that exact disposable database after the
	// process exits instead of leaving it for the age-based orphan reaper.
	dbNamePath := filepath.Join(t.TempDir(), "database-name")
	t.Cleanup(func() {
		datnameBytes, err := os.ReadFile(dbNamePath)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Errorf("read leaked test database name: %v", err)
			return
		}
		datname := string(datnameBytes)
		if _, ok := parseTestPgDbName(datname); !ok {
			t.Errorf("refuse to drop invalid test database name %q", datname)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, datname))
			if err != nil {
				t.Errorf("drop leaked test database %s: %v", datname, err)
			}
		}, OptReadWrite())
	})
	cmd := exec.Command(
		os.Args[0],
		"-test.run", "^TestTeardownBoundedOnLeakedConnections$",
		"-test.count=1",
		"-test.timeout=90s",
	)
	cmd.Env = append(
		os.Environ(),
		"WARP_TEST_TEARDOWN_SUBPROCESS=1",
		"WARP_TEST_TEARDOWN_BOUND_SECONDS=2",
		dbNamePathEnv+"="+dbNamePath,
	)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("the inner test must FAIL (it Fatals); it reported success:\n%s", out)
	}
	if strings.Contains(string(out), "test timed out") {
		t.Fatalf("the inner test hung to its timeout — the teardown footgun is back:\n%s", out)
	}
	if !strings.Contains(string(out), "intentional failure with a leaked pool connection") {
		t.Fatalf("the inner test failed for the wrong reason:\n%s", out)
	}
	// generous ceiling: setup + 2s bound + overhead, nowhere near the 90s
	// timeout the hang would hit
	if 60*time.Second < elapsed {
		t.Fatalf("inner run took %v — teardown abandon is not bounding", elapsed)
	}
}
