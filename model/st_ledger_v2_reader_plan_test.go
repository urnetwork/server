package model

// The v2 earnings readers' attribution read on epochs with many wallet
// resolutions (st_ledger_v2_reader_model.go).
//
// On Main, 2026-11-26, GET /account/epochs (AccountEpochs, through
// stLedgerV2AccountEpochs) read the recorded amounts of three epochs, about
// 300 rows, and attributed them with a DISTINCT ON over every wallet
// resolution of those epochs, about 0.9M rows, sorted by (epoch, coldkey,
// client_id) to keep the smallest client of each coldkey: a sequential scan
// and a sort of the whole table, 3.6 s and 31k buffers a call, 23 calls a
// minute, growing with every epoch. This fixture is that shape at test size:
// an epoch's resolutions outnumber its amounts many times over.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

const (
	attributionPlanFirstEpoch = 11
	attributionPlanEpochs     = 3
	// resolutions per epoch, one per client
	attributionPlanResolutions = 3000
	// coldkeys the resolutions select, and amounts whose coldkey no
	// resolution selects
	attributionPlanColdkeys        = 30
	attributionPlanUnresolvedCkeys = 10
	attributionPlanNetworks        = 5
)

// The fixture's ids and coldkeys: deterministic, one family per prefix.
func attributionPlanUuidSql(prefix string, expression string) string {
	return fmt.Sprintf(`('%s-0000-4000-8000-' || lpad(to_hex(%s), 12, '0'))::uuid`, prefix, expression)
}

func attributionPlanColdkeySql(expression string) string {
	return fmt.Sprintf(`decode(lpad(to_hex(%s), 64, '0'), 'hex')`, expression)
}

func attributionPlanNetworkId(n int) server.Id {
	return server.RequireParseId(fmt.Sprintf("a2000000-0000-4000-8000-%012x", n))
}

// Each epoch: one resolution per client; client i selects coldkey i mod 30 and
// belongs to network i mod 5, so a coldkey's smallest client is the coldkey's
// own number and its network that number mod 5. Each epoch records an amount
// for every selected coldkey and for ten that no resolution selects.
func buildAttributionPlanFixture(t testing.TB) {
	t.Helper()
	ctx := t.Context()
	key := string(testStDeploymentKey)
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO st_payout_wallet_resolution (
				deployment_key, epoch, no_id, client_id, network_id, mode, coldkey,
				consent_hash, consent_generation, head_hash, head_generation
			)
			SELECT $1, epoch.epoch, 1, %s, %s, 'network', %s,
				decode(repeat('01', 32), 'hex'), 1, decode(repeat('01', 32), 'hex'), 1
			FROM generate_series(%d, %d) AS epoch(epoch)
			CROSS JOIN generate_series(0, %d) AS i`,
			attributionPlanUuidSql("a1000000", "i"),
			attributionPlanUuidSql("a2000000", fmt.Sprintf("i %% %d", attributionPlanNetworks)),
			attributionPlanColdkeySql(fmt.Sprintf("1 + i %% %d", attributionPlanColdkeys)),
			attributionPlanFirstEpoch, attributionPlanFirstEpoch+attributionPlanEpochs-1,
			attributionPlanResolutions-1,
		), key))
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO st_ledger_v2_amount (deployment_key, no_id, epoch, coldkey, amount, root_index)
			SELECT $1, 1, epoch.epoch, %s, 1000 + k, 0
			FROM generate_series(%d, %d) AS epoch(epoch)
			CROSS JOIN generate_series(0, %d) AS k`,
			attributionPlanColdkeySql("1 + k"),
			attributionPlanFirstEpoch, attributionPlanFirstEpoch+attributionPlanEpochs-1,
			attributionPlanColdkeys+attributionPlanUnresolvedCkeys-1,
		), key))
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_payout_wallet_resolution`))
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_ledger_v2_amount`))
	}, server.OptReadWrite(), server.OptNoRetry())
}

type attributionPlanNode struct {
	NodeType    string                `json:"Node Type"`
	Relation    string                `json:"Relation Name"`
	Index       string                `json:"Index Name"`
	ActualRows  float64               `json:"Actual Rows"`
	ActualLoops float64               `json:"Actual Loops"`
	Plans       []attributionPlanNode `json:"Plans"`
}

// The rows the read's scans of st_payout_wallet_resolution returned, and how
// each scan read it (an index name, or the node type of a scan without one),
// with the read run under EXPLAIN ANALYZE as a custom plan of its arguments,
// and as the generic plan a cached statement can switch to.
func attributionPlanResolutionReads(t testing.TB, sql string, types string, args []any, literals string) (customRows float64, customScans []string, genericRows float64, genericScans []string) {
	t.Helper()
	ctx := t.Context()
	read := func(raw []byte) (rows float64, scans []string) {
		var plans []struct {
			Plan attributionPlanNode
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("invalid plan: %v %s", err, raw)
		}
		var walk func(attributionPlanNode)
		walk = func(node attributionPlanNode) {
			if node.Relation == "st_payout_wallet_resolution" {
				rows += node.ActualRows * node.ActualLoops
				if node.Index != "" {
					scans = append(scans, node.Index)
				} else {
					scans = append(scans, node.NodeType)
				}
			}
			for _, child := range node.Plans {
				walk(child)
			}
		}
		walk(plans[0].Plan)
		return
	}

	var raw []byte
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+sql, args...).Scan(&raw))
	}, server.OptReadOnly())
	customRows, customScans = read(raw)

	raw = nil
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		results, err := conn.Conn().PgConn().Exec(ctx, fmt.Sprintf(
			`BEGIN READ ONLY; SET LOCAL plan_cache_mode = force_generic_plan; `+
				`PREPARE attribution_plan_read (%s) AS %s; `+
				`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE attribution_plan_read (%s); `+
				`DEALLOCATE attribution_plan_read; COMMIT`,
			types, sql, literals,
		)).ReadAll()
		server.Raise(err)
		for _, result := range results {
			if 0 < len(result.Rows) {
				raw = result.Rows[0][0]
			}
		}
	}, server.OptReadOnly())
	genericRows, genericScans = read(raw)
	return
}

// The attribution reads at most one wallet resolution per amount, the
// smallest client of the amount's coldkey and epoch, custom and generic,
// instead of every resolution of its epochs; and the amounts it returns are
// the ones the smallest client's network earns.
func TestStLedgerV2AttributionReadsOneResolutionPerAmount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		buildAttributionPlanFixture(t)
		forgetStLedgerV2AttributionIndex()
		t.Cleanup(forgetStLedgerV2AttributionIndex)
		server.Db(ctx, func(conn server.PgConn) {
			if !stLedgerV2AttributionIndexReady(ctx, conn) {
				t.Fatal("the migrated database lacks a valid st_payout_wallet_resolution_coldkey")
			}
		})

		key := testStDeploymentKey
		epochs := []uint64{}
		epochLiterals := []string{}
		for epoch := uint64(attributionPlanFirstEpoch); epoch < attributionPlanFirstEpoch+attributionPlanEpochs; epoch += 1 {
			epochs = append(epochs, epoch)
			epochLiterals = append(epochLiterals, fmt.Sprint(epoch))
		}
		epochArray := "'{" + strings.Join(epochLiterals, ",") + "}'"
		amountsPerEpoch := attributionPlanColdkeys + attributionPlanUnresolvedCkeys
		network := attributionPlanNetworkId(2)

		for _, read := range []struct {
			name     string
			sql      string
			types    string
			args     []any
			literals string
			amounts  int
		}{
			{
				"amounts for a network", stLedgerV2AmountsForNetworkSql, "varchar, bigint, bigint[], uuid",
				[]any{string(key), int64(1), stLedgerV2Epochs(epochs), network},
				fmt.Sprintf("'%s', 1, %s, '%s'", key, epochArray, network),
				attributionPlanEpochs * amountsPerEpoch,
			},
			{
				"an epoch's amounts by network", stLedgerV2NetworkAmountsSql, "varchar, bigint, bigint[]",
				[]any{string(key), int64(1), stLedgerV2Epochs(epochs[1:2])},
				fmt.Sprintf("'%s', 1, '{%d}'", key, epochs[1]),
				amountsPerEpoch,
			},
		} {
			customRows, customScans, genericRows, genericScans := attributionPlanResolutionReads(t, read.sql, read.types, read.args, read.literals)
			t.Logf("%s: custom read %.0f resolutions through %v, generic %.0f through %v, for %d amounts",
				read.name, customRows, customScans, genericRows, genericScans, read.amounts)
			if float64(read.amounts) < customRows || float64(read.amounts) < genericRows {
				t.Errorf("%s read %.0f (custom) and %.0f (generic) wallet resolutions for %d amounts, want at most one per amount",
					read.name, customRows, genericRows, read.amounts)
			}
		}

		checkAttributionPlanAmounts(t, epochs, network)
	})
}

// The readers' amounts on the fixture: coldkey c (1..30) has its smallest
// client c-1, of network (c-1) mod 5; coldkeys 31..40 have no resolution. Each
// epoch's amount of coldkey c is 999 + c.
func checkAttributionPlanAmounts(t testing.TB, epochs []uint64, network server.Id) {
	t.Helper()
	ctx := t.Context()
	key := testStDeploymentKey
	amountsPerEpoch := attributionPlanColdkeys + attributionPlanUnresolvedCkeys
	var total, networkAmount uint64
	byNetwork := map[server.Id]uint256.Int{}
	for c := 1; c <= amountsPerEpoch; c += 1 {
		amount := uint64(999 + c)
		total += amount
		if c <= attributionPlanColdkeys {
			owner := attributionPlanNetworkId((c - 1) % attributionPlanNetworks)
			sum := byNetwork[owner]
			sum.Add(&sum, uint256.NewInt(amount))
			byNetwork[owner] = sum
			if owner == network {
				networkAmount += amount
			}
		}
	}
	wantAmounts := map[uint64]*StLedgerV2EpochAmount{}
	for _, epoch := range epochs {
		wantAmounts[epoch] = &StLedgerV2EpochAmount{Amount: *uint256.NewInt(networkAmount), Total: *uint256.NewInt(total)}
	}
	if got := GetStLedgerV2AmountsForNetwork(ctx, key, network, 1, epochs); !reflect.DeepEqual(got, wantAmounts) {
		t.Fatalf("network amounts %+v, want %+v", got, wantAmounts)
	}
	gotByNetwork, gotTotal := GetStLedgerV2NetworkAmounts(ctx, key, 1, epochs[1])
	if !reflect.DeepEqual(gotByNetwork, byNetwork) || gotTotal.Uint64() != total {
		t.Fatalf("epoch amounts by network %+v total %s, want %+v total %d", gotByNetwork, gotTotal.Dec(), byNetwork, total)
	}
}

// A release that reaches a database before its migration builds the index,
// or after an interrupted build left it invalid, reads the same amounts by
// the sort, since a probe without the index walks whole epochs; it probes
// once the index is valid.
func TestStLedgerV2AttributionSortsUntilTheIndexIsValid(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		buildAttributionPlanFixture(t)
		epochs := []uint64{attributionPlanFirstEpoch, attributionPlanFirstEpoch + 1, attributionPlanFirstEpoch + 2}
		network := attributionPlanNetworkId(2)
		forgetStLedgerV2AttributionIndex()
		t.Cleanup(forgetStLedgerV2AttributionIndex)

		ready := func() (ready bool) {
			forgetStLedgerV2AttributionIndex()
			server.Db(ctx, func(conn server.PgConn) {
				ready = stLedgerV2AttributionIndexReady(ctx, conn)
			})
			return
		}
		for _, step := range []struct {
			name      string
			sql       string
			wantFail  bool
			wantReady bool
		}{
			{"absent", `DROP INDEX st_payout_wallet_resolution_coldkey`, false, false},
			// A concurrent build that fails leaves its index in the catalog,
			// invalid, as an interrupted one does: here a unique build over
			// the fixture's coldkeys of many clients.
			{"invalid", `CREATE UNIQUE INDEX CONCURRENTLY st_payout_wallet_resolution_coldkey
				ON st_payout_wallet_resolution (deployment_key, no_id, epoch, coldkey) INCLUDE (network_id)`, true, false},
			{"valid", `DROP INDEX st_payout_wallet_resolution_coldkey;
				CREATE INDEX st_payout_wallet_resolution_coldkey
				ON st_payout_wallet_resolution (deployment_key, no_id, epoch, coldkey, client_id) INCLUDE (network_id)`, false, true},
		} {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				_, err := conn.Exec(ctx, step.sql)
				if (err != nil) != step.wantFail {
					t.Fatalf("%s index: %s: error %v", step.name, step.sql, err)
				}
			}, server.OptReadWrite(), server.OptNoRetry())
			if got := ready(); got != step.wantReady {
				t.Fatalf("%s index: ready %t, want %t", step.name, got, step.wantReady)
			}
			checkAttributionPlanAmounts(t, epochs, network)
		}
	})
}
