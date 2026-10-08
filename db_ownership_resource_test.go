// Ownership validation reads the same normalized resource as the direct pool.
package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Literal, list and environment-backed resources are all accepted by ordinary
// pool construction. Raw YAML assertions used to reject the latter two forms.
func TestPgOwnershipResourceMatchesPoolStringResolution(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "synthetic-pg.example:5432")
	t.Setenv("SYNTHETIC_OWNERSHIP_DATABASE", "synthetic")
	t.Setenv("SYNTHETIC_OWNERSHIP_USER", "synthetic")
	for _, raw := range []string{
		"authority: synthetic-pg.example:5432\ndb: synthetic\nuser: synthetic\n",
		"authority: [synthetic-pg.example:5432]\ndb: [synthetic]\nuser: [synthetic]\n",
		"authority: '{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}'\ndb: '{{ env:SYNTHETIC_OWNERSHIP_DATABASE }}'\nuser: '{{ env:SYNTHETIC_OWNERSHIP_USER }}'\n",
		"authority: ['{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}']\ndb: ['{{ env:SYNTHETIC_OWNERSHIP_DATABASE }}']\nuser: ['{{ env:SYNTHETIC_OWNERSHIP_USER }}']\n",
	} {
		func() {
			defer Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(raw))()
			resource := Vault.RequireSimpleResource(MaintenancePgVaultResourceName)
			config, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://%s:synthetic@%s/%s",
				resource.RequireString("user"), resource.RequireString("authority"), resource.RequireString("db")))
			if err != nil {
				t.Fatal("fixture is not accepted by the pool's resource reader", err)
			}
			got := requirePgOwnershipResource()
			want := pgOwnershipResource{host: config.ConnConfig.Host, port: config.ConnConfig.Port,
				database: config.ConnConfig.Database, user: config.ConnConfig.User}
			if got != want {
				t.Fatal("ownership validation differs from direct pool configuration")
			}
		}()
	}
}

// Normalization cannot authorize missing, ambiguous or malformed resources,
// and parser/translation panics must not disclose their input in status errors.
func TestPgOwnershipResourceRejectsInvalidNormalizedValues(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_MISSING", "")
	t.Setenv("SYNTHETIC_OWNERSHIP_INVALID", "synthetic-pg.example:70000")
	for index, raw := range []string{
		"authority: [synthetic-private-canary\n",
		"authority: synthetic-pg.example\ndb: synthetic\n",
		"authority: synthetic-pg.example\ndb: ''\nuser: synthetic\n",
		"authority: [synthetic-pg.example, second-pg.example]\ndb: synthetic\nuser: synthetic\n",
		"authority: synthetic-pg.example\ndb: synthetic\nuser: [one, two]\n",
		"authority: synthetic-pg.example\ndb: [one, two]\nuser: synthetic\n",
		"authority: 123\ndb: synthetic\nuser: synthetic\n",
		"authority: '{{ env:SYNTHETIC_OWNERSHIP_MISSING }}'\ndb: synthetic\nuser: synthetic\n",
		"authority: '{{ env:SYNTHETIC_OWNERSHIP_INVALID }}'\ndb: synthetic\nuser: synthetic\n",
		"authority: synthetic-private-canary@synthetic-pg.example\ndb: synthetic\nuser: synthetic\n",
		"authority: synthetic-pg.example:0\ndb: synthetic\nuser: synthetic\n",
	} {
		func() {
			defer Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(raw))()
			err := captureDbErrorPanic(func() { _ = requirePgOwnershipResource() })
			if err == nil || fmt.Sprint(err) != "invalid database ownership maintenance resource" {
				t.Fatalf("invalid resource case %d was accepted or lost sanitized refusal", index)
			}
		}()
	}
}

// The real raw-transaction bridge must reach and commit ownership SQL using
// the resolved declaration; testing only a parser helper misses this consumer.
func TestPgTaskClaimAcceptsNormalizedDirectResource(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "synthetic-pg.example:5432")
	t.Setenv("SYNTHETIC_OWNERSHIP_DATABASE", "synthetic")
	t.Setenv("SYNTHETIC_OWNERSHIP_USER", "synthetic")
	defer Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(
		"authority: ['{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}']\ndb: ['{{ env:SYNTHETIC_OWNERSHIP_DATABASE }}']\nuser: ['{{ env:SYNTHETIC_OWNERSHIP_USER }}']\n"))()
	_, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = taskClaimWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	if err := ValidatePgTaskClaimTransaction(ctx, conn, tx); err != nil {
		t.Fatal("normalized direct resource was refused before task admission", err)
	}
	if admitted, err := TryPgTaskClaimOwnership(ctx, tx, NewPgOwnershipKey("synthetic-resource-claim", NewId())); err != nil || !admitted {
		t.Fatal("normalized direct resource failed exact task ownership", admitted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("normalized direct claim did not commit", err)
	}
}

// Resolving a supported template does not relax the already-open pool check.
func TestPgOwnershipNormalizedResourceStillRejectsDifferentBackend(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "different-pg.example:5432")
	defer Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(
		"authority: ['{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}']\ndb: [synthetic]\nuser: [synthetic]\n"))()
	_, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	if err := requirePgOwnershipResource().validate(conn); err == nil {
		t.Fatal("normalized resource authorized a different checked-out backend")
	}
}
