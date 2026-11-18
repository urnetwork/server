// Which vault file a process's Postgres pools connect with (QD-992): its service's own role file,
// pg-<service>.yml, where its vault holds one, and the platform's pg.yml only where that file is proved absent.
// Each test's vault is a directory of synthetic files it writes itself; the resolution is read and nothing
// connects.
package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Points the process at a vault holding exactly files, by resource name, as a block's mount holds exactly its
// secret_files.
func pgRoleTestVault(t *testing.T, files map[string]string) (root string) {
	root = filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for resourceName, authority := range files {
		role := fmt.Sprintf("authority: %s\nuser: synthetic\npassword: synthetic\ndb: synthetic\n", authority)
		if err := os.WriteFile(filepath.Join(root, resourceName), []byte(role), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic")
	t.Cleanup(Config.PushSimpleResource(DefaultPgConfigResourceName, []byte("min_connections: 1\nmax_connections: 2\n")))
	return root
}

// The authority the pool would dial, or why it resolves none.
func pgRoleTestAuthority(pool *safePgPool) (authority string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if recovered, ok := r.(error); ok {
				err = recovered
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	vaultKeys, configKeys := pool.resolveResources()
	configKeys.RequireInt("max_connections")
	return vaultKeys.RequireString("authority"), nil
}

func TestPgRoleFileOfTheServiceComesBeforePgYml(t *testing.T) {
	pgRoleTestVault(t, map[string]string{
		"pg.yml":         "platform.example:6432",
		"pg-tlsedge.yml": "tlsedge.example:6432",
		"pg-mxedge.yml":  "mxedge.example:6432",
	})
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "tlsedge.example:6432" {
		t.Fatal("tlsedge, holding its own role and the platform's, connected with", got, err)
	}
	t.Setenv("WARP_SERVICE", "mxedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "mxedge.example:6432" {
		t.Fatal("mxedge, holding its own role and the platform's, connected with", got, err)
	}
	// a service with no file of its own reads the platform's, never another service's
	t.Setenv("WARP_SERVICE", "api")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "platform.example:6432" {
		t.Fatal("api, holding no role of its own, connected with", got, err)
	}
	// the maintenance pool reads no service's file
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safeMaintenancePool); err != nil || got != "platform.example:6432" {
		t.Fatal("tlsedge's maintenance pool connected with", got, err)
	}
}

func TestPgRoleFileAbsentReadsPgYml(t *testing.T) {
	pgRoleTestVault(t, map[string]string{
		"pg.yml": "platform.example:6432",
	})
	for _, service := range []string{"tlsedge", "mxedge", "api", "taskworker", ""} {
		t.Setenv("WARP_SERVICE", service)
		if got, err := pgRoleTestAuthority(safePool); err != nil || got != "platform.example:6432" {
			t.Fatalf("service %q, whose vault holds only pg.yml, connected with %q (%v)", service, got, err)
		}
	}
}

// A block's mount holds its own role and not the platform's. The block connects with it, and a process of any
// other service beside that file resolves nothing: not the file, which names another service, and not pg.yml,
// which is not there.
func TestPgRoleFileAloneIsReadOnlyByItsService(t *testing.T) {
	pgRoleTestVault(t, map[string]string{
		"pg-tlsedge.yml": "tlsedge.example:6432",
	})
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "tlsedge.example:6432" {
		t.Fatal("tlsedge, holding only its own role, connected with", got, err)
	}
	for _, service := range []string{"mxedge", "api", "tlsedge-1", "tls", "edge", ""} {
		t.Setenv("WARP_SERVICE", service)
		got, err := pgRoleTestAuthority(safePool)
		if err == nil {
			t.Fatalf("service %q connected with %q from a vault holding only tlsedge's role", service, got)
		}
		if !errors.Is(err, ErrResourceNotFound) {
			t.Fatalf("service %q beside only tlsedge's role was refused with %v; want pg.yml not found", service, err)
		}
	}
	// the block has no maintenance pool: its role file is the main pool's alone
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safeMaintenancePool); err == nil || !errors.Is(err, ErrResourceNotFound) {
		t.Fatal("tlsedge's maintenance pool, with no pg.yml or pg_maintenance.yml, resolved", got, err)
	}
}

// Two blocks' roles side by side, as on a host that runs both: each reads its own.
func TestPgRoleFilesOfTwoServicesStayApart(t *testing.T) {
	pgRoleTestVault(t, map[string]string{
		"pg-tlsedge.yml": "tlsedge.example:6432",
		"pg-mxedge.yml":  "mxedge.example:6432",
	})
	t.Setenv("WARP_SERVICE", "mxedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "mxedge.example:6432" {
		t.Fatal("mxedge connected with", got, err)
	}
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "tlsedge.example:6432" {
		t.Fatal("tlsedge connected with", got, err)
	}
	t.Setenv("WARP_SERVICE", "mxedge-platform")
	if got, err := pgRoleTestAuthority(safePool); err == nil || !errors.Is(err, ErrResourceNotFound) {
		t.Fatal("mxedge-platform, which holds no role, resolved", got, err)
	}
}

// A role file that is there but cannot be reached is an error. The platform's pg.yml beside it is not read in
// its place, which would connect a restricted block with the platform's role.
func TestPgRoleFileUnreachableNeverFallsBackToPgYml(t *testing.T) {
	root := pgRoleTestVault(t, map[string]string{
		"pg.yml": "platform.example:6432",
	})
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "platform.example:6432" {
		t.Fatal("before its role file exists, tlsedge connected with", got, err)
	}

	// a directory where the file belongs
	rolePath := filepath.Join(root, "pg-tlsedge.yml")
	if err := os.Mkdir(rolePath, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := pgRoleTestAuthority(safePool)
	if err == nil {
		t.Fatal("tlsedge, whose role file is a directory, connected with", got)
	}
	if !errors.Is(err, ErrResourceUnavailable) || !strings.Contains(err.Error(), "pg-tlsedge.yml") {
		t.Fatal("a role file that is a directory was refused with", err)
	}
	if err := os.Remove(rolePath); err != nil {
		t.Fatal(err)
	}

	// a link to a file that is gone
	if err := os.Symlink(filepath.Join(root, "absent.yml"), rolePath); err != nil {
		t.Fatal(err)
	}
	got, err = pgRoleTestAuthority(safePool)
	if err == nil {
		t.Fatal("tlsedge, whose role file is a dangling link, connected with", got)
	}
	if !errors.Is(err, ErrResourceUnavailable) {
		t.Fatal("a role file that is a dangling link was refused with", err)
	}
	if err := os.Remove(rolePath); err != nil {
		t.Fatal(err)
	}

	// a file with no role in it
	if err := os.WriteFile(rolePath, []byte("user: synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := pgRoleTestAuthority(safePool); err == nil {
		t.Fatal("tlsedge, whose role file names no authority, connected with", got)
	}

	// another service beside the bad file is untouched by it
	t.Setenv("WARP_SERVICE", "api")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "platform.example:6432" {
		t.Fatal("api, beside tlsedge's bad role file, connected with", got, err)
	}
}

// A service name that could not be a file name reads no file at all, as it names no push file
// (TestGrafanaPushSettingsRefusesAServiceNameThatIsNoFileName).
func TestPgRoleFileRefusesAServiceNameThatIsNoFileName(t *testing.T) {
	root := pgRoleTestVault(t, map[string]string{
		"pg.yml":         "platform.example:6432",
		"pg-tlsedge.yml": "tlsedge.example:6432",
	})
	// what a name with a path in it would reach if it were joined into one
	if err := os.MkdirAll(filepath.Join(root, "pg-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"../tlsedge", "x/../tlsedge", "x/tlsedge", "tlsedge.yml", "Tlsedge", "tls_edge", "-tlsedge", " tlsedge", "tlsedge\n"} {
		t.Setenv("WARP_SERVICE", service)
		got, err := pgRoleTestAuthority(safePool)
		if err == nil {
			t.Fatalf("service %q connected with %q", service, got)
		}
		if !strings.Contains(err.Error(), "names no Postgres role file") {
			t.Fatalf("service %q was refused with %v", service, err)
		}
	}
}

// A read-only process that holds its own role stays on it: the maintenance resource is the platform's.
func TestPgRoleFileOfTheServiceHoldsForAReadOnlyProcess(t *testing.T) {
	pgRoleTestVault(t, map[string]string{
		"pg.yml":             "platform.example:6432",
		"pg_maintenance.yml": "direct.example:5432",
		"pg-tlsedge.yml":     "tlsedge.example:6432",
	})
	pgReadOnly.Store(true)
	t.Cleanup(func() { pgReadOnly.Store(false) })
	t.Setenv("WARP_SERVICE", "tlsedge")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "tlsedge.example:6432" {
		t.Fatal("a read-only tlsedge connected with", got, err)
	}
	t.Setenv("WARP_SERVICE", "api")
	if got, err := pgRoleTestAuthority(safePool); err != nil || got != "direct.example:5432" {
		t.Fatal("a read-only api connected with", got, err)
	}
	if got, err := pgRoleTestAuthority(safeMaintenancePool); err != nil || got != "direct.example:5432" {
		t.Fatal("the maintenance pool connected with", got, err)
	}
}
