package server

// DESIGN-COMPUTE §14.4's groups in server's migrations (CSRV-M12-02): MG-C1, MG-C2 and MG-C3 after MG9, each linked
// exactly where the binary links the pending groups, MG-C1 refusing a database below PostgreSQL 15 before anything of
// it applies, and each owner's part read from sidecar/model's registry (CONTRACTS C-30). The groups' place in the
// slice and their additivity are TestSidecarGroupsEndTheMigrations' and TestSidecarGroupsAreAdditive's, under the
// pending tag, and a release build's probe linking none of them is TestSidecarPendingBuildTags'.

import (
	"context"
	"strings"
	"testing"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/model"
)

// Each compute group is linked exactly where the binary holds the pending groups: in a sidecar_pending test binary
// every one, in a release build none; and a linked group's marker stands after MG9's, MG-C1's floor first in its group.
func TestComputeGroupsLinkUnderThePendingTag(t *testing.T) {
	pending := 0 < len(sidecarPendingMigrations())
	for name, linked := range map[string]func() bool{"MG-C1": SidecarMgC1Linked, "MG-C2": SidecarMgC2Linked, "MG-C3": SidecarMgC3Linked} {
		if linked() != pending {
			t.Errorf("%s linked %v in a build whose pending groups are %v", name, linked(), pending)
		}
	}
	if !pending {
		return
	}
	mg9 := CodeMigrationVersion(sidecarMg9MigrationId)
	c1 := CodeMigrationVersion(sidecarMgC1MigrationId)
	c2 := CodeMigrationVersion(sidecarMgC2MigrationId)
	c3 := CodeMigrationVersion(sidecarMgC3MigrationId)
	if !(0 < mg9 && mg9 < c1 && c1 < c2 && c2 < c3 && c3 == MigrationCount()) {
		t.Fatalf("the markers stand at MG9 %d, MG-C1 %d, MG-C2 %d, MG-C3 %d of %d", mg9, c1, c2, c3, MigrationCount())
	}
	floor, ok := migrations[mg9].(*SqlMigration)
	if !ok || floor.sql != computeRequirePostgres15().sql {
		t.Fatal("MG-C1 does not start with PostgreSQL 15's floor")
	}
}

// A part's statements become the group's entries in their order, and a step that is no statement is refused.
func TestComputeMigrationPartsBecomeTheGroupsStatements(t *testing.T) {
	values := sidecarComputeMigrationValues(
		[]*operator.Migration{{Id: "a", Sql: "CREATE TABLE compute_a (id uuid NOT NULL)"}},
		nil,
		[]*operator.Migration{{Id: "b", Sql: "CREATE TABLE compute_b (id uuid NOT NULL)"}},
	)
	if len(values) != 2 || values[0].(*SqlMigration).sql != "CREATE TABLE compute_a (id uuid NOT NULL)" ||
		values[1].(*SqlMigration).sql != "CREATE TABLE compute_b (id uuid NOT NULL)" {
		t.Fatalf("the parts read %v", values)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a data step was taken")
			}
		}()
		sidecarComputeMigrationValues([]*operator.Migration{{Id: "c", Code: func(ctx context.Context, db operator.Db) error { return nil }}})
	}()
	// what each owner registered is what its part holds: none has landed one yet
	for _, group := range []string{model.ComputeMigrationGroupC1, model.ComputeMigrationGroupC2, model.ComputeMigrationGroupC3} {
		owners := model.ComputeMigrationGroupOwners(group)
		if len(owners) == 0 {
			t.Fatalf("%s has no owners", group)
		}
		for _, owner := range owners {
			if part := model.ComputeMigrationPart(group, owner); len(sidecarComputeMigrationValues(part)) != len(part) {
				t.Errorf("%s's part of %s did not convert whole", owner, group)
			}
		}
	}
}

// The floor's statement run with a floor above the test stack's version raises, and a table created after it in its
// transaction is not there afterwards; at PostgreSQL 15's floor it passes. So db migrate on an older database fails
// at MG-C1's first step with nothing of the group applied.
func TestComputeRequirePostgres15RaisesBelowItsFloor(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var raised error
		MaintenanceDb(ctx, func(conn PgConn) {
			_, err := conn.Exec(ctx, `BEGIN`)
			Raise(err)
			_, raised = conn.Exec(ctx, computePostgresFloorSql(99999999))
			if raised == nil {
				_, err = conn.Exec(ctx, `CREATE TABLE compute_floor_probe (id uuid NOT NULL)`)
				Raise(err)
			}
			_, err = conn.Exec(ctx, `ROLLBACK`)
			Raise(err)
		}, OptReadWrite(), OptNoRetry())
		if raised == nil || !strings.Contains(raised.Error(), "MG-C1 needs a server_version_num") {
			t.Fatalf("a floor above the database's version: %v", raised)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var found *string
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('compute_floor_probe')::text`).Scan(&found))
			if found != nil {
				t.Error("a table after the refused floor is in the database")
			}
			_, err := conn.Exec(ctx, computePostgresFloorSql(computePostgresMinVersionNum))
			if err != nil {
				t.Errorf("PostgreSQL 15's floor on the test stack: %v", err)
			}
		}, OptReadWrite(), OptNoRetry())
	})
}

// The private test stack runs PostgreSQL 15 or later, as §14.4 asks of every local, T3 and canary database.
func TestTheTestStackRunsPostgres15OrLater(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var versionNum int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&versionNum))
		})
		if versionNum < computePostgresMinVersionNum {
			t.Fatalf("the test stack runs server_version_num %d, below %d", versionNum, computePostgresMinVersionNum)
		}
	})
}
