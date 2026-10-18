// The census connection of a WARP_ENV=local process dials only launcher-attested
// endpoints, like every local PostgreSQL connection. The check runs in a child
// test process started in the local environment without an attestation and
// stops at the admission barrier, so no connection is ever dialed.
package strecovery

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Marks the child test process.
const censusLocalGuardChildEnv = "URNETWORK_STRECOVERY_LOCAL_GUARD_CHILD"

// A function's code pointer, equal for one function value or closure literal.
func censusFuncPointer(function any) uintptr {
	return reflect.ValueOf(function).Pointer()
}

// Runs one test of this binary in a child that starts with WARP_ENV=local and
// no attestation, the state of an operator shell that kept a local environment.
func runCensusLocalGuardChild(t *testing.T, name string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^"+name+"$", "-test.count=1")
	cmd.Env = []string{censusLocalGuardChildEnv + "=1", "WARP_ENV=local"}
	for _, entry := range os.Environ() {
		variable, _, _ := strings.Cut(entry, "=")
		if variable == "WARP_ENV" || variable == censusLocalGuardChildEnv || strings.HasPrefix(variable, "WARP_TEST_ENV_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local child: %v\n%s", err, output)
	}
}

// An operator census run from a shell that kept WARP_ENV=local must not reach a
// production database through a loopback tunnel: its connection carries the
// local guard's dial and lookup when it reaches connection admission.
func TestCensusReadDialsThroughLocalGuard(t *testing.T) {
	if os.Getenv(censusLocalGuardChildEnv) != "1" {
		runCensusLocalGuardChild(t, "TestCensusReadDialsThroughLocalGuard")
		return
	}
	reference, err := pgx.ParseConfig("postgres://synthetic_reader@192.0.2.1:5432/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	unguardedDial := censusFuncPointer(reference.DialFunc)
	server.ConfigureLocalPgDial(reference)
	if censusFuncPointer(reference.DialFunc) == unguardedDial {
		t.Fatal("the child is not a guarded local process")
	}
	source, limits := censusReadTestSource(t)
	// Custody paths refuse every symlink component, such as macOS's /var.
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("postgres://synthetic_reader:synthetic_password@192.0.2.1:5432/synthetic?sslmode=require\n")
	source.Connection.Path = filepath.Join(directory, "connection")
	if err := os.WriteFile(source.Connection.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	source.Connection.Sha256 = digest(raw)
	stopped := errors.New("synthetic admission stop")
	pinned := false
	reader := PostgresReader{beforeConnect: func(_ context.Context, config *pgx.ConnConfig) error {
		pinned = censusFuncPointer(config.DialFunc) == censusFuncPointer(reference.DialFunc) &&
			censusFuncPointer(config.LookupFunc) == censusFuncPointer(reference.LookupFunc)
		return stopped
	}}
	if _, err := reader.Snapshot(t.Context(), source, limits); !errors.Is(err, stopped) {
		t.Fatalf("census read = %v; want the admission barrier", err)
	}
	if !pinned {
		t.Fatal("the census connection reached admission without the local guard's dial and lookup")
	}
}
