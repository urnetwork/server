// Every owned transaction and task claim validates against the maintenance
// resource. Production taskworker resolved pg_maintenance.yml and logged its
// interpolation on every use; it is now derived once per resource version and
// environment. The resource and environment are synthetic.
package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Owned transactions are how the taskworker claims and settles under
// ownership, and each validates its checkout against the maintenance resource.
// Through the real owned transaction, only the first may resolve
// pg_maintenance.yml while the vault is unchanged, and every one still commits.
func TestOwnedTxResolvesMaintenanceResourceOnce(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		// after the environment pushed its maintenance resource and opened its
		// pools, so only the owned transactions below read this vault
		root := filepath.Join(t.TempDir(), "absent", "vault")
		t.Setenv("WARP_VAULT_HOME", root)
		probe := resolverProbe(root, MaintenancePgVaultResourceName)
		committed := 0
		owned := func() {
			OwnedTx(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())}, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES($1,1)`, committed))
			})
			committed += 1
		}

		// the cold resolution is the control that the probe is observable at all
		if cold := strings.Count(captureProcessStderr(t, owned), probe); cold < 1 {
			t.Fatalf("the first owned transaction wrote %d resolver probes for %s; want at least 1 (glog must log to stderr)", cold, MaintenancePgVaultResourceName)
		}
		transactions := 8
		warm := strings.Count(captureProcessStderr(t, func() {
			for i := 0; i < transactions; i += 1 {
				owned()
			}
		}), probe)
		if warm != 0 {
			t.Fatalf("%d more owned transactions resolved %s %d more times; want 0 while the vault is unchanged", transactions, MaintenancePgVaultResourceName, warm)
		}
		Db(ctx, func(conn PgConn) {
			var rows int
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM owned_tx_effect`).Scan(&rows))
			if rows != committed || committed != transactions+1 {
				t.Fatalf("%d of %d owned transactions committed", rows, transactions+1)
			}
		}, OptNoRetry())
	})
}

// A maintenance resource under an absent vault root, whose authority is read
// from SYNTHETIC_OWNERSHIP_AUTHORITY.
func pushTemplatedOwnershipResource(t testing.TB, database string) (root string, pop func()) {
	root = filepath.Join(t.TempDir(), "absent", "vault")
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-main")
	pop = Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(
		"authority: '{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}'\ndb: "+database+"\nuser: synthetic\n"))
	return root, pop
}

// The interpolation line for the authority template.
const ownershipAuthorityLookup = "[env]lookup env var \"SYNTHETIC_OWNERSHIP_AUTHORITY\""

func TestPgOwnershipResourceResolvesOncePerVersion(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "synthetic-pg.example:5432")
	root, pop := pushTemplatedOwnershipResource(t, "synthetic")
	defer pop()
	want := pgOwnershipResource{host: "synthetic-pg.example", port: 5432, database: "synthetic", user: "synthetic"}
	uses := 16
	output := captureProcessStderr(t, func() {
		for i := 0; i < uses; i += 1 {
			if got := requirePgOwnershipResource(); got != want {
				t.Fatalf("ownership resource = %+v; want %+v", got, want)
			}
		}
	})
	resolutions := strings.Count(output, resolverProbe(root, MaintenancePgVaultResourceName))
	lookups := strings.Count(output, ownershipAuthorityLookup)
	if resolutions != 1 || lookups != 1 {
		t.Fatalf("%d ownership validations resolved pg_maintenance.yml %d times and interpolated it %d times; want once each", uses, resolutions, lookups)
	}
}

// A changed environment, override or invalid resource is seen by the next use,
// and an invalid resource still refuses every use without being reused.
func TestPgOwnershipResourceFollowsEnvironmentAndResource(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "synthetic-pg.example:5432")
	_, pop := pushTemplatedOwnershipResource(t, "synthetic")
	defer pop()
	if got := requirePgOwnershipResource(); got.host != "synthetic-pg.example" {
		t.Fatalf("ownership resource = %+v", got)
	}
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "moved-pg.example:6432")
	if got := requirePgOwnershipResource(); got.host != "moved-pg.example" || got.port != 6432 {
		t.Fatalf("a changed authority environment was not read again: %+v", got)
	}

	popOther := Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(
		"authority: '{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}'\ndb: other\nuser: synthetic\n"))
	if got := requirePgOwnershipResource(); got.database != "other" {
		t.Fatalf("a new maintenance resource was not read: %+v", got)
	}

	popInvalid := Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte("authority: synthetic-pg.example\ndb: synthetic\n"))
	for i := 0; i < 2; i += 1 {
		err := captureDbErrorPanic(func() { _ = requirePgOwnershipResource() })
		if err == nil || fmt.Sprint(err) != "invalid database ownership maintenance resource" {
			t.Fatalf("use %d of an invalid resource was accepted or lost sanitized refusal: %v", i, err)
		}
	}
	popInvalid()
	if got := requirePgOwnershipResource(); got.database != "other" {
		t.Fatalf("the restored maintenance resource was not read: %+v", got)
	}
	popOther()
	if got := requirePgOwnershipResource(); got.database != "synthetic" || got.host != "moved-pg.example" {
		t.Fatalf("the original maintenance resource was not read: %+v", got)
	}

	pop()
	err := captureDbErrorPanic(func() { _ = requirePgOwnershipResource() })
	if err == nil || fmt.Sprint(err) != "database ownership requires an explicit direct maintenance resource" {
		t.Fatalf("a missing maintenance resource = %v", err)
	}
}
