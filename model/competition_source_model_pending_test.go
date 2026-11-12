//go:build sidecar_pending

package model

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// On a database with MG0's permissive step, the lock digest and the database's
// graph guard admit the same graphs, the ten and the ten with sidecar, and for
// each the digest is the one the guards compute. So a source the control plane
// accepts with either graph passes the guards (QD-128, QD-752).
func TestCompetitionSourceLockDigestAgreesWithTheWidenedGuards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if !server.SidecarMg0Linked() {
			t.Fatal("a pending build does not link MG0")
		}
		ten := map[string]string{}
		for _, name := range CompetitionSourceRepositoryNamesBeforeSidecar() {
			ten[name] = strings.Repeat("a", 40)
		}
		eleven := map[string]string{}
		for _, name := range CompetitionSourceRepositoryNames() {
			eleven[name] = strings.Repeat("a", 40)
		}
		with := func(base map[string]string, name string, commit string) map[string]string {
			commits := maps.Clone(base)
			commits[name] = commit
			return commits
		}
		without := func(base map[string]string, name string) map[string]string {
			commits := maps.Clone(base)
			delete(commits, name)
			return commits
		}
		graphs := []map[string]string{
			ten,
			eleven,
			with(ten, "synthetic-unknown", strings.Repeat("a", 40)),
			with(ten, "operator-proxy", strings.Repeat("a", 40)),
			with(eleven, "synthetic-unknown", strings.Repeat("a", 40)),
			with(ten, "sidecar", "invalid-commit"),
			without(ten, "gvisor"),
			without(eleven, "gvisor"),
			with(without(eleven, "gvisor"), "synthetic-unknown", strings.Repeat("a", 40)),
			{},
		}
		admitted := 0
		server.Db(ctx, func(conn server.PgConn) {
			for _, commits := range graphs {
				encoded, err := json.Marshal(commits)
				if err != nil {
					t.Fatal(err)
				}
				digest, digestErr := CompetitionSourceLockDigest(commits)
				for _, staging := range []bool{false, true} {
					var valid bool
					server.Raise(conn.QueryRow(ctx, `SELECT competition_v2_source_graph_valid($1::jsonb, $2)`, string(encoded), staging).Scan(&valid))
					if valid != (digestErr == nil) {
						t.Errorf("the guard answers %t and the lock digest %v for %s", valid, digestErr, encoded)
					}
				}
				if digestErr != nil {
					continue
				}
				admitted++
				var guardDigest string
				server.Raise(conn.QueryRow(ctx, `SELECT competition_v2_instrument_lock_digest($1::jsonb)`, string(encoded)).Scan(&guardDigest))
				if guardDigest != digest {
					t.Errorf("the guards compute lock %s and the control plane %s for %s", guardDigest, digest, encoded)
				}
			}
		})
		if admitted != 2 {
			t.Fatalf("%d graphs are admitted, not the ten and the ten with sidecar", admitted)
		}
	})
}
