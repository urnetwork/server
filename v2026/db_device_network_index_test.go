// The probe-shard reap deletes a probe network's devices by network_id while
// it holds the endpoint fence of every client in that network.
package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// Gives device many small networks, as on Main, and returns one of them.
func seedDeviceNetworks(t testing.TB, ctx context.Context) Id {
	t.Helper()
	networkIds := []Id{}
	for range 1000 {
		networkIds = append(networkIds, NewId())
	}
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `INSERT INTO device (device_id, network_id, device_name, device_spec, create_time)
		 SELECT gen_random_uuid(), network_id, 'probe', 'test', $2
		 FROM unnest($1::uuid[]) AS network_id CROSS JOIN generate_series(1, 5)`, networkIds, NowUtc()))
	})
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `ANALYZE device`))
	})
	return networkIds[0]
}

// Plans the reap's delete as a custom plan and as the generic plan a cached
// statement settles on. Reports whether either plan reads device
// sequentially, and the indexes the plans read instead.
func deviceNetworkDeletePlans(t testing.TB, ctx context.Context, networkId Id) (seqScan bool, indexNames []string) {
	t.Helper()
	type planNode struct {
		NodeType     string     `json:"Node Type"`
		RelationName string     `json:"Relation Name"`
		IndexName    string     `json:"Index Name"`
		Plans        []planNode `json:"Plans"`
	}
	var visit func(node planNode)
	visit = func(node planNode) {
		if node.NodeType == "Seq Scan" && node.RelationName == "device" {
			seqScan = true
		}
		if node.IndexName != "" {
			indexNames = append(indexNames, node.IndexName)
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	read := func(raw string) {
		var plans []struct {
			Plan planNode `json:"Plan"`
		}
		Raise(json.Unmarshal([]byte(raw), &plans))
		for _, plan := range plans {
			visit(plan.Plan)
		}
	}
	Tx(ctx, func(tx PgTx) {
		var raw string
		Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) DELETE FROM device WHERE network_id=$1`, networkId).Scan(&raw))
		read(raw)
		// A prepared statement outlives a rolled-back transaction on its pooled
		// session, so each plan gets its own name.
		name := "probe_shard_device_delete_" + strings.ReplaceAll(NewId().String(), "-", "")
		RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_generic_plan`))
		RaisePgResult(tx.Exec(ctx, `PREPARE `+name+`(uuid) AS DELETE FROM device WHERE network_id=$1`))
		Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) EXECUTE `+name+`('`+networkId.String()+`')`).Scan(&raw))
		read(raw)
		RaisePgResult(tx.Exec(ctx, `DEALLOCATE `+name))
	})
	return
}

// Before the migration both plans read the whole table, which proves the
// fixture observes the scan; after it both find the network's devices
// through device_network_id.
func TestProbeShardDeviceDeleteUsesNetworkIndex(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		ApplyDbMigrationsUpTo(ctx, migrationIndex(t, "CREATE INDEX CONCURRENTLY device_network_id"))
		networkId := seedDeviceNetworks(t, ctx)
		if seqScan, _ := deviceNetworkDeletePlans(t, ctx, networkId); !seqScan {
			t.Fatal("control: the device delete avoided a sequential scan before its index existed")
		}
		ApplyDbMigrations(ctx)
		seqScan, indexNames := deviceNetworkDeletePlans(t, ctx, networkId)
		if seqScan {
			t.Fatalf("the probe-shard device delete still reads device sequentially (indexes used: %v)", indexNames)
		}
		if len(indexNames) != 2 || slices.ContainsFunc(indexNames, func(name string) bool { return name != "device_network_id" }) {
			t.Fatalf("the probe-shard device delete plans read %v, want device_network_id in both plans", indexNames)
		}
	})
}
