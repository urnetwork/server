package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// TestUpdateLocationGroup guards the member-replace path of UpdateLocationGroup,
// which had two latent runtime bugs: its UPDATE set a nonexistent column
// (`location_name` instead of `location_group_name`), and its
// `DELETE FROM location_group_member WHERE location_group_id = $1` was executed
// with no bound argument for $1. It creates a group, replaces its member set,
// and verifies the members were swapped.
func TestUpdateLocationGroup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		locA := server.NewId()
		locB := server.NewId()
		locC := server.NewId()

		// CreateLocationGroup mints its own id, keyed by name -- fetch it back
		CreateLocationGroup(ctx, &LocationGroup{
			Name:              "grp",
			Promoted:          false,
			MemberLocationIds: []server.Id{locA, locB},
		})
		var groupId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT location_group_id FROM location_group WHERE location_group_name = $1`,
				"grp",
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&groupId))
				}
			})
		})

		members := func() []server.Id {
			ids := []server.Id{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT location_id FROM location_group_member WHERE location_group_id = $1`,
					groupId,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var id server.Id
						server.Raise(result.Scan(&id))
						ids = append(ids, id)
					}
				})
			})
			return ids
		}

		if len(members()) != 2 {
			t.Fatalf("after create: want 2 members, got %d", len(members()))
		}

		// replace the member set -- exercises the previously-broken UPDATE + DELETE
		if !UpdateLocationGroup(ctx, &LocationGroup{
			LocationGroupId:   groupId,
			Name:              "grp",
			Promoted:          true,
			MemberLocationIds: []server.Id{locC},
		}) {
			t.Fatalf("UpdateLocationGroup returned false")
		}
		got := members()
		if len(got) != 1 || got[0] != locC {
			t.Fatalf("after update: want [%s], got %v", locC, got)
		}
	})
}
