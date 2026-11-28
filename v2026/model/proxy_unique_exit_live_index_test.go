// The allocator's read of a network's live exits probes the network's
// unreleased assignments, not every network's.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// A plan node of EXPLAIN (FORMAT JSON), with the actual counts when analyzed.
type networkLiveExitPlanNode struct {
	NodeType string                    `json:"Node Type"`
	Relation string                    `json:"Relation Name"`
	Index    string                    `json:"Index Name"`
	Rows     float64                   `json:"Actual Rows"`
	Loops    float64                   `json:"Actual Loops"`
	Removed  float64                   `json:"Rows Removed by Filter"`
	Recheck  float64                   `json:"Rows Removed by Index Recheck"`
	Plans    []networkLiveExitPlanNode `json:"Plans"`
}

// The indexes a plan reads, the assignment rows it examined (returned or
// filtered out, over all loops), and its scans of the assignment table.
func networkLiveExitPlan(raw []byte) (indexes map[string]bool, examined float64, scans []string) {
	var plans []struct{ Plan networkLiveExitPlanNode }
	server.Raise(json.Unmarshal(raw, &plans))
	indexes = map[string]bool{}
	var visit func(node networkLiveExitPlanNode)
	visit = func(node networkLiveExitPlanNode) {
		if node.Index != "" {
			indexes[node.Index] = true
		}
		if node.Relation == "proxy_exit_assignment" {
			examined += node.Loops * (node.Rows + node.Removed + node.Recheck)
			scans = append(scans, fmt.Sprintf("%s %s", node.NodeType, node.Index))
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	for _, plan := range plans {
		visit(plan.Plan)
	}
	return
}

// On Main (2026-11-26) the read walked proxy_exit_assignment_assigned_at_live,
// every unreleased assignment of every network, and filtered for its network:
// 28k to 33k buffer hits a call. Here 2,000 other networks hold 3 unreleased
// and 2 released assignments each, and the network under test 2 pending, 1
// active, 1 replacing and 3 released. The statement the allocator sends, in a
// custom plan for the network and in the generic plan a cached statement can
// switch to, must read proxy_exit_assignment_network_id_live and examine only
// the network's unreleased assignments, and it returns exactly their
// providers. Without the index the read scans every network's assignments.
func TestNetworkLiveExitProvidersReadTheNetworkLiveIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		now := server.NowUtc()
		wantProviders := []server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			// every other network: 3 unreleased and 2 released assignments
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO proxy_exit_assignment (assignment_id,network_id,proxy_client_id,provider_client_id,egress_key,state,assigned_at,released_at)
				SELECT gen_random_uuid(), network.network_id, gen_random_uuid(), gen_random_uuid(),
					decode(md5(network.n::text || ':' || slot.n::text), 'hex'),
					(ARRAY['pending','active','replacing','released','released'])[slot.n],
					$1::timestamp - slot.n * interval '1 minute',
					CASE WHEN 3 < slot.n THEN $1::timestamp END
				FROM (SELECT gen_random_uuid() AS network_id, n FROM generate_series(1, 2000) n) network
				CROSS JOIN generate_series(1, 5) AS slot(n)`, now))
			for index, state := range []string{"pending", "pending", "active", "replacing", "released", "released", "released"} {
				providerId := server.NewId()
				var releasedAt *time.Time
				if state == "released" {
					releasedAt = &now
				} else {
					wantProviders = append(wantProviders, providerId)
				}
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO proxy_exit_assignment (assignment_id,network_id,proxy_client_id,provider_client_id,egress_key,state,assigned_at,released_at)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
					server.NewId(), networkId, server.NewId(), providerId, []byte(fmt.Sprintf("network-under-test-%d", index)),
					state, now.Add(-time.Duration(index)*time.Minute), releasedAt))
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE proxy_exit_assignment`))
		})

		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			var raw []byte
			server.Db(ctx, func(conn server.PgConn) {
				defer func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer cancel()
					// by name: pgx keeps its own cached statements on this connection
					_, _ = conn.Exec(cleanup, `DEALLOCATE network_live_exits`)
					_, _ = conn.Exec(cleanup, `RESET plan_cache_mode`)
				}()
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode = `+mode))
				server.RaisePgResult(conn.Exec(ctx, `PREPARE network_live_exits(uuid) AS `+networkLiveExitProviderIdsSql))
				server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE network_live_exits('`+networkId.String()+`')`,
					pgx.QueryExecModeSimpleProtocol).Scan(&raw))
			})
			indexes, examined, scans := networkLiveExitPlan(raw)
			if !indexes["proxy_exit_assignment_network_id_live"] || indexes["proxy_exit_assignment_assigned_at_live"] {
				t.Fatalf("%s: the network's live exits read %v (%v), want proxy_exit_assignment_network_id_live", mode, scans, writePathTestIndexNames(indexes))
			}
			if float64(len(wantProviders)) < examined {
				t.Fatalf("%s: the network's live exits examined %.0f assignments (%v), want only its %d unreleased ones", mode, examined, scans, len(wantProviders))
			}
		}

		providers := networkLiveExitProviderIds(ctx, networkId)
		slices.SortFunc(providers, server.Id.Cmp)
		slices.SortFunc(wantProviders, server.Id.Cmp)
		if !slices.Equal(providers, wantProviders) {
			t.Fatalf("the network's live exits are %v, want %v", providers, wantProviders)
		}
	})
}
