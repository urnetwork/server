// The operator journal connection of a WARP_ENV=local process dials only
// launcher-attested endpoints, like every local PostgreSQL connection. The
// check runs in a child test process started in the local environment without
// an attestation and only builds the configuration, so nothing is dialed.
package stmonitor

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Marks the child test process.
const snapshotLocalGuardChildEnv = "URNETWORK_STMONITOR_LOCAL_GUARD_CHILD"

// A function's code pointer, equal for one function value or closure literal.
func snapshotFuncPointer(function any) uintptr {
	return reflect.ValueOf(function).Pointer()
}

// A monitor read from a shell that kept WARP_ENV=local must not reach a
// production journal through a loopback tunnel: the configuration every read
// attempt copies carries the local guard's dial and lookup.
func TestSnapshotConnectionDialsThroughLocalGuard(t *testing.T) {
	if os.Getenv(snapshotLocalGuardChildEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestSnapshotConnectionDialsThroughLocalGuard$", "-test.count=1")
		cmd.Env = []string{snapshotLocalGuardChildEnv + "=1", "WARP_ENV=local"}
		for _, entry := range os.Environ() {
			variable, _, _ := strings.Cut(entry, "=")
			if variable == "WARP_ENV" || variable == snapshotLocalGuardChildEnv || strings.HasPrefix(variable, "WARP_TEST_ENV_") {
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("local child: %v\n%s", err, output)
		}
		return
	}
	reference, err := pgx.ParseConfig("postgres://synthetic@192.0.2.1:5432/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	unguardedDial := snapshotFuncPointer(reference.DialFunc)
	server.ConfigureLocalPgDial(reference)
	if snapshotFuncPointer(reference.DialFunc) == unguardedDial {
		t.Fatal("the child is not a guarded local process")
	}
	source := Source{
		Database:     "operator_fixture_synthetic",
		User:         "observer_synthetic",
		DeploymentId: "synthetic-deployment",
		ChainId:      964,
		GenesisHash:  "0x" + strings.Repeat("1", 64),
		Coordinator:  "0x" + strings.Repeat("2", 40),
		OperatorId:   1,
		Accounts:     []string{"0x" + strings.Repeat("3", 40)},
	}
	cfg, err := connectionConfig("postgresql://observer_synthetic:synthetic-password@db.fixture.example:5432/operator_fixture_synthetic?sslmode=verify-full", source)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotFuncPointer(cfg.DialFunc) != snapshotFuncPointer(reference.DialFunc) ||
		snapshotFuncPointer(cfg.LookupFunc) != snapshotFuncPointer(reference.LookupFunc) {
		t.Fatal("the journal connection lacks the local guard's dial and lookup")
	}
	if attempt := cfg.Copy(); snapshotFuncPointer(attempt.DialFunc) != snapshotFuncPointer(reference.DialFunc) {
		t.Fatal("a read attempt's copy lost the local guard's dial")
	}
}
