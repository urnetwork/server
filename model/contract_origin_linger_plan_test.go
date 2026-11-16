package model

// Plan proofs for the lingering origin reads through the side table
// (contract_origin_linger.go; OPEN-SOURCE-ID-RETIRE.md §5). Each test seeds the
// pair's lingering origins among a destination's fan-in and other pairs'
// recent closes, all captured by the trigger, plans every statement that reads
// closed origins with sequential scans disabled, with
// transfer_contract_open_source_id built again and without it, and walks the
// JSON plan: no read of that index, every closed arm a pair range of the side
// table's primary key joined to each contract's primary key.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Every node of a plan, depth first.
func lingerPlanNodes(node indexDietPlanNode) []indexDietPlanNode {
	nodes := []indexDietPlanNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, lingerPlanNodes(child)...)
	}
	return nodes
}

// Plans the statement in a transaction that rolls back, after the setup
// statements and with sequential scans disabled. Returns every node and the
// raw plan.
func planContractOriginLingerStatement(
	t testing.TB,
	ctx context.Context,
	setup []string,
	sql string,
	args ...any,
) ([]indexDietPlanNode, string) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	for _, statement := range append(setup, `SET LOCAL enable_seqscan = off`) {
		server.RaisePgResult(tx.Exec(ctx, statement))
	}
	var raw []byte
	server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw))
	var plans []struct {
		Plan indexDietPlanNode `json:"Plan"`
	}
	server.Raise(json.Unmarshal(raw, &plans))
	if len(plans) != 1 {
		t.Fatalf("%d plans for one statement", len(plans))
	}
	return lingerPlanNodes(plans[0].Plan), string(raw)
}

// The statements that read closed origins, as each origin read runs them, with
// arguments for the pair.
type lingerPlanStatement struct {
	name string
	sql  string
	args []any
}

func lingerPlanStatements(t testing.TB, sourceId server.Id, destinationId server.Id, payerNetworkId server.Id) []lingerPlanStatement {
	t.Helper()
	runtimeQueries := coveredCompanionRuntimeQueries(t)
	closeFloor := server.NowUtc().Add(-CompanionOriginCloseLinger)
	plainArgs := []any{sourceId, destinationId, closeFloor, payerNetworkId}
	if runtimeQueries["earliest_origin"].binds == 5 {
		plainArgs = append(plainArgs, ByteCount(1024))
	}
	chainedArgs := []any{sourceId, destinationId, closeFloor, payerNetworkId, server.NewId(), server.NewId()}
	if runtimeQueries["earliest_companion_origin"].binds == 7 {
		chainedArgs = append(chainedArgs, ByteCount(1024))
	}
	return []lingerPlanStatement{
		{name: "companion origin exists", sql: companionOriginExistsSql, args: []any{sourceId, destinationId, closeFloor}},
		{name: "plain origin selection", sql: runtimeQueries["earliest_origin"].sql, args: plainArgs},
		{name: "chained origin selection", sql: runtimeQueries["earliest_companion_origin"].sql, args: chainedArgs},
		{name: "zero escrow plain origin", sql: zeroEscrowPlainOriginSql, args: []any{sourceId, destinationId, closeFloor}},
		{name: "zero escrow chained origin", sql: zeroEscrowChainedOriginSql, args: []any{sourceId, destinationId, closeFloor, server.NewId(), server.NewId()}},
	}
}

// Seeds the pair's lingering origins, the destination's recent fan-in from
// other sources and other pairs' recent closes, every one closed a second
// after its creation and inside its deadline, so the trigger captured all of
// them. The payer is the prober, so the selections plan their maximum arms.
func seedContractOriginLingerPlan(t testing.TB, ctx context.Context) (sourceId server.Id, destinationId server.Id, payerNetworkId server.Id) {
	t.Helper()
	sourceId, destinationId, payerNetworkId = server.NewId(), server.NewId(), server.NewId()
	now := server.NowUtc()
	insertIndexDietClosedContracts(t, ctx, &sourceId, &destinationId, payerNetworkId, now, 50)
	insertIndexDietClosedContracts(t, ctx, nil, &destinationId, server.NewId(), now, 20000)
	insertIndexDietClosedContracts(t, ctx, nil, nil, server.NewId(), now, 20000)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO prober_identity (singleton, network_id, client_id)
			VALUES (true, $1, $2) ON CONFLICT (singleton) DO UPDATE
			SET network_id = excluded.network_id, client_id = excluded.client_id`, payerNetworkId, server.NewId()))
	})
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract`))
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE contract_origin_linger`))
	}, server.OptReadWrite(), server.OptNoRetry())
	var captured int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_origin_linger WHERE source_id = $1 AND destination_id = $2`, sourceId, destinationId).Scan(&captured))
	})
	if captured != 50 {
		t.Fatalf("the trigger captured %d of the pair's 50 lingering origins", captured)
	}
	return
}

// Every statement that reads closed origins reads no
// transfer_contract_open_source_id; each closed arm reads the side table's
// pair range and each contract by its primary key, joined by the side table's
// ids.
func TestContractOriginLingerReadsNeedNoClosedPairIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		if server.IsContractPartitioned(t.Context()) {
			t.Skip("plain contract tables only")
		}
		ctx := t.Context()
		sourceId, destinationId, payerNetworkId := seedContractOriginLingerPlan(t, ctx)
		// the index is gone at the head: present again, where the planner
		// could choose it, and absent
		for _, setup := range [][]string{
			{`CREATE INDEX transfer_contract_open_source_id ON transfer_contract (open, source_id, destination_id, contract_id)`},
			nil,
		} {
			for _, statement := range lingerPlanStatements(t, sourceId, destinationId, payerNetworkId) {
				nodes, raw := planContractOriginLingerStatement(t, ctx, setup, statement.sql, statement.args...)
				lingerReads := 0
				closedReads := 0
				for _, node := range nodes {
					if node.Index == "transfer_contract_open_source_id" {
						t.Fatalf("%s (setup %v) reads transfer_contract_open_source_id:\n%s", statement.name, setup, raw)
					}
					conditions := strings.Join([]string{node.IndexCondition, node.RecheckCondition, node.Filter}, " ")
					switch node.Relation {
					case "contract_origin_linger":
						lingerReads += 1
						reads := []indexDietIndexRead{{index: node.Index, condition: node.IndexCondition}}
						if node.NodeType == "Bitmap Heap Scan" {
							reads = indexDietBitmapReads(node)
						} else if !strings.HasPrefix(node.NodeType, "Index") {
							reads = nil
						}
						if len(reads) == 0 {
							t.Fatalf("%s (setup %v) reads the side table by %s, not its pair range:\n%s", statement.name, setup, node.NodeType, raw)
						}
						for _, read := range reads {
							if read.index != "contract_origin_linger_pkey" || !strings.Contains(read.condition, "source_id") || !strings.Contains(read.condition, "destination_id") {
								t.Fatalf("%s (setup %v) reads the side table by %s (%s), not its pair range:\n%s",
									statement.name, setup, read.index, read.condition, raw)
							}
						}
					case "transfer_contract":
						if !strings.Contains(conditions, "close_time") {
							continue
						}
						closedReads += 1
						// the lookup is keyed by the side table's id, whatever alias the
						// planner gives the side table
						if node.NodeType != "Index Scan" || node.Index != "transfer_contract_pkey" ||
							!(strings.Contains(node.IndexCondition, "(contract_id = contract_origin_linger") || strings.Contains(node.IndexCondition, "(contract_id = linger.contract_id)")) {
							t.Fatalf("%s (setup %v) reads closed contracts by %s %s (%s), not each side table id from the primary key:\n%s",
								statement.name, setup, node.NodeType, node.Index, node.IndexCondition, raw)
						}
					}
				}
				if lingerReads == 0 || lingerReads != closedReads {
					t.Fatalf("%s (setup %v): %d side table reads for %d closed arms:\n%s", statement.name, setup, lingerReads, closedReads, raw)
				}
			}
		}
	})
}

// The sweep's batch reads the prune time index and deletes by tuple id; its
// lag read is the index's lowest entry.
func TestContractOriginLingerSweepReadsThePruneTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO contract_origin_linger (source_id, destination_id, contract_id, prune_time)
				SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
					clock_timestamp() AT TIME ZONE 'UTC' + (n - 10000) * interval '1 second'
				FROM generate_series(1, 20000) AS n
			`))
		})
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE contract_origin_linger`))
		}, server.OptReadWrite(), server.OptNoRetry())
		for _, statement := range []struct {
			name string
			sql  string
			args []any
		}{
			{name: "sweep batch", sql: contractOriginLingerSweepBatchSql, args: []any{5000}},
			{name: "sweep lag", sql: contractOriginLingerSweepLagSql},
		} {
			nodes, raw := planContractOriginLingerStatement(t, ctx, nil, statement.sql, statement.args...)
			pruneIndexReads := 0
			for _, node := range nodes {
				if node.Relation != "contract_origin_linger" {
					continue
				}
				switch {
				case node.Index == "contract_origin_linger_prune_time":
					pruneIndexReads += 1
				case node.NodeType == "Tid Scan" || node.NodeType == "ModifyTable":
				default:
					t.Fatalf("%s reads the side table by %s %s:\n%s", statement.name, node.NodeType, node.Index, raw)
				}
			}
			if pruneIndexReads != 1 {
				t.Fatalf("%s reads the prune time index %d times:\n%s", statement.name, pruneIndexReads, raw)
			}
		}
	})
}
