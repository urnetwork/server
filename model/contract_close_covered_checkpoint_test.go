// Owner decision B at the model owners: a receiver checkpoint reported after
// the sender's final completes legacy-escrow, zero-escrow and failed
// settlements exactly as two finals would, leaves a contract whose expiry has
// started to the expiry task, and the closed companion origin reads stay a
// bounded index range once settled forward contracts linger in them until
// their deadlines. All data is synthetic.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A legacy-escrow contract whose receiver checkpoints after the sender's
// final records that checkpoint as a final and queues the same durable
// settlement intent two finals queue; its worker then settles it normally.
// A receiver checkpoint before the sender's final queues nothing.
func TestCoveredCheckpointQueuesLegacyEscrowSettlement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const byteCount = ByteCount(11)
		readState := func(contractId server.Id) (checkpoint bool, used ByteCount, pending, terminal bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT checkpoint,used_transfer_byte_count,
					EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
					(SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1)
					FROM contract_close WHERE contract_id=$1 AND party=$2`, contractId, ContractPartyDestination).Scan(&checkpoint, &used, &pending, &terminal))
			})
			return
		}

		early, posts := createNetEscrowOrderingTestContract(ctx, f, 40)
		server.RunPosts(ctx, posts...)
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: early.ContractId, ClientId: f.destinationId, AckedByteCount: byteCount, Checkpoint: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: early.ContractId, ClientId: f.sourceId, AckedByteCount: byteCount}); err != nil {
			t.Fatal(err)
		}
		if checkpoint, _, pending, terminal := readState(early.ContractId); !checkpoint || pending || terminal {
			t.Fatalf("receiver checkpoint before the sender's final changed the contract: checkpoint=%t pending=%t terminal=%t", checkpoint, pending, terminal)
		}

		escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 60)
		server.RunPosts(ctx, posts...)
		var redisReserved bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(bool_or(redis_reserved),false) FROM transfer_escrow WHERE contract_id=$1`, escrow.ContractId).Scan(&redisReserved))
		})
		if redisReserved {
			t.Fatal("fixture escrow is Redis reserved, not legacy")
		}
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: escrow.ContractId, ClientId: f.sourceId, AckedByteCount: byteCount}); err != nil {
			t.Fatal(err)
		}
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: escrow.ContractId, ClientId: f.destinationId, AckedByteCount: byteCount, Checkpoint: true}); err != nil {
			t.Fatal(err)
		}
		if checkpoint, used, pending, terminal := readState(escrow.ContractId); checkpoint || used != byteCount || !pending || terminal {
			t.Fatalf("covered checkpoint after the sender's final: checkpoint=%t bytes=%d pending=%t terminal=%t, want a final of %d bytes with a queued intent", checkpoint, used, pending, terminal, byteCount)
		}
		result, err := FlushLegacySettlements(ctx, int(escrow.ContractId[15])%LegacySettlementShardCount, nil, 64)
		if err != nil || result.Completed != 1 {
			t.Fatal("legacy worker did not settle the completed checkpoint", result, err)
		}
		if _, _, pending, terminal := readState(escrow.ContractId); pending || !terminal {
			t.Fatalf("legacy worker left the completed checkpoint unsettled: pending=%t terminal=%t", pending, terminal)
		}
	})
}

// A valve contract holds no escrow. Its receiver's checkpoint after the
// sender's final settles it through the free path with no payout, debit or
// provider totals, as two finals would.
func TestCoveredCheckpointSettlesZeroEscrowWithoutPayout(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		now := server.NowUtc()
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             clients.payerNetworkId,
			StartTime:             now.Add(-time.Minute),
			EndTime:               now.Add(time.Hour),
			StartBalanceByteCount: 64 * Mib,
			BalanceByteCount:      64 * Mib,
			NetRevenue:            UsdToNanoCents(1),
		})
		zero, err := CreateZeroEscrowContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 4*Mib)
		requireZeroEscrowTestResult(t, ctx, zero, err, 4*Mib)
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: zero.ContractId, ClientId: clients.payerId, AckedByteCount: 2 * Mib}); err != nil {
			t.Fatal(err)
		}
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: zero.ContractId, ClientId: clients.providerId, AckedByteCount: 2 * Mib, Checkpoint: true}); err != nil {
			t.Fatal(err)
		}
		state := readZeroEscrowTestState(t, ctx, zero.ContractId)
		if state.outcome == nil || *state.outcome != ContractOutcomeSettled {
			t.Fatalf("zero escrow contract outcome %v after its covered checkpoint, want settled", state.outcome)
		}
		if state.escrowCount != 0 || state.sweepCount != 0 || state.debitCount != 0 || state.providerTotalsTask {
			t.Fatalf("zero escrow checkpoint settlement took financial custody: %+v", state)
		}
	})
}

// The completed checkpoint commits only together with its settlement. A
// settlement refused because another owner holds the contract's provider
// totals publication leaves the receiver's checkpoint resumable and the
// contract open; the receiver's next report after the sender's final settles.
func TestCoveredCheckpointSettlementFailureKeepsCheckpoint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		const byteCount = ByteCount(11)
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: byteCount}); err != nil {
			t.Fatal(err)
		}
		readState := func() (checkpoint, terminal bool, receipts int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT checkpoint,
					(SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1),
					(SELECT count(*) FROM contract_close_report WHERE contract_id=$1 AND party=$2)
					FROM contract_close WHERE contract_id=$1 AND party=$2`, contract.ContractId, ContractPartyDestination).Scan(&checkpoint, &terminal, &receipts))
			})
			return
		}

		release := holdFinancialTestOwner(t, ctx, []server.PgOwnershipKey{
			task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contract.ContractId)),
		})
		_, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: byteCount, Checkpoint: true})
		release()
		if !errors.Is(err, errTransferBalanceOwnershipBusy) {
			t.Fatalf("covered checkpoint settlement did not report the busy owner: %v", err)
		}
		if checkpoint, terminal, receipts := readState(); !checkpoint || terminal || receipts != 1 {
			t.Fatalf("failed settlement kept a completed checkpoint: checkpoint=%t terminal=%t receipts=%d", checkpoint, terminal, receipts)
		}

		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 0, Checkpoint: true}); err != nil {
			t.Fatal(err)
		}
		if checkpoint, terminal, _ := readState(); checkpoint || !terminal {
			t.Fatalf("receiver's next report did not settle the contract: checkpoint=%t terminal=%t", checkpoint, terminal)
		}
	})
}

// The expiry task owns a contract once it retains the contract's original
// expiry proof (usage_unverified). A receiver checkpoint reported after the
// sender's final then stays a checkpoint for that task, which re-reads it; the
// report path never settles the contract against the earlier proof.
func TestCoveredCheckpointLeavesStartedExpiryToCloser(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := createRedisAdmissionTest(ctx, f, 100).ContractId
		const byteCount = ByteCount(11)
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: id, ClientId: f.sourceId, AckedByteCount: byteCount}); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			state, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(time.Hour))
			server.Raise(err)
			if state == nil {
				t.Fatal("fixture did not start the contract's expiry")
			}
		})
		proof, _ := readContractExpiryTestSnapshot(t, ctx, id)
		readState := func() (checkpoint, open bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT checkpoint,
					(SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$1)
					FROM contract_close WHERE contract_id=$1 AND party=$2`, id, ContractPartyDestination).Scan(&checkpoint, &open))
			})
			return
		}

		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: id, ClientId: f.destinationId, AckedByteCount: byteCount, Checkpoint: true}); err != nil {
			t.Fatal(err)
		}
		if checkpoint, open := readState(); !checkpoint || !open {
			t.Fatalf("report path settled a contract whose expiry had started: checkpoint=%t open=%t", checkpoint, open)
		}
		if after, _ := readContractExpiryTestSnapshot(t, ctx, id); !bytes.Equal(proof, after) {
			t.Fatal("report rewrote the retained expiry proof")
		}

		// A started expiry is due whatever its quiet period.
		count, _, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-24*time.Hour), 32, 1, 0, 0, nil)
		if err != nil || count != 1 {
			t.Fatalf("expiry task did not finish the contract it started: count=%d err=%v", count, err)
		}
		if checkpoint, open := readState(); checkpoint || open {
			t.Fatalf("expiry task left the receiver's checkpoint open: checkpoint=%t open=%t", checkpoint, open)
		}
	})
}

// Settled forward contracts now remain eligible closed origins until their
// deadlines, so the closed branch must stay an index range bounded in time,
// not a read of the pair's whole closed history. As in production, the pair
// has a long expired closed history, its provider has many contracts from
// other sources created days ago and closed minutes ago by the expiry task,
// and other pairs created and closed many contracts within the hour. That
// defeats the unbounded pair range, the provider's close-time range, an
// ordered scan of the provider's history and a range of recent contract ids
// alone. The actual model queries, the plain and chained origin reads of both
// escrow paths and the existence read before them, must read only the pair's
// recent contracts under custom and generic plans. The plain reads still
// return the anchor and the existence read still finds it; the pair has no
// companion origin for the chained reads. The existence read's open branch is
// outside this change, so only its closed branch is counted.
func TestClosedCompanionOriginReadIsBoundedByContractId(t *testing.T) {
	queries := coveredCompanionRuntimeQueries(t)
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newCoveredCompanionFixture(t, ctx, false, false)
		for _, origin := range f.origins {
			server.Raise(CloseContract(ctx, origin.ContractId, f.clients.payerId, 0, false))
			server.Raise(CloseContract(ctx, origin.ContractId, f.clients.providerId, 0, false))
		}
		const historyCount = 4096
		server.Tx(ctx, func(tx server.PgTx) {
			// Closed rows outside every origin read: expired disputes of the
			// pair from three days ago, with ids from their creation time.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 transfer_byte_count,dispute,create_time,expiration_time,close_time)
				SELECT (lpad(to_hex((extract(epoch FROM now() - interval '3 days') * 1000)::bigint), 12, '0') || lpad(to_hex(i), 20, '0'))::uuid,
				 $1,$2,$3,$4,512,true,
				 (now() - interval '3 days') AT TIME ZONE 'UTC',
				 (now() - interval '3 days' + interval '1 hour') AT TIME ZONE 'UTC',
				 (now() - interval '3 days') AT TIME ZONE 'UTC'
				FROM generate_series(1,$5) i`,
				f.clients.payerNetworkId, f.clients.payerId, f.clients.providerNetworkId, f.clients.providerId, historyCount))
			// Contracts of other sources to the same provider, created days
			// ago and closed minutes ago by the expiry task.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 transfer_byte_count,dispute,create_time,expiration_time,close_time)
				SELECT (lpad(to_hex((extract(epoch FROM now() - interval '2 days') * 1000)::bigint), 12, '0') || lpad(to_hex(i), 20, '0'))::uuid,
				 $1,gen_random_uuid(),$2,$3,512,true,
				 (now() - interval '2 days') AT TIME ZONE 'UTC',
				 (now() - interval '2 days' + interval '1 hour') AT TIME ZONE 'UTC',
				 (now() - interval '5 minutes') AT TIME ZONE 'UTC'
				FROM generate_series(1,$4) i`,
				f.clients.payerNetworkId, f.clients.providerNetworkId, f.clients.providerId, historyCount))
			// The whole network's recent contracts: other pairs created and
			// settled within the hour, still before their deadlines and many
			// times more than the pair's history, so a range of recent
			// contract ids or of recent close times alone is not bounded
			// either. As in production, each of their providers and clients
			// holds many contracts.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 transfer_byte_count,payer_network_id,create_time,expiration_time,close_time,outcome,provider_usage)
				SELECT (lpad(to_hex((extract(epoch FROM now() - interval '10 minutes') * 1000)::bigint), 12, '0') || lpad(to_hex(i), 20, '0'))::uuid,
				 $1,md5('synthetic recent client ' || (i % 4096))::uuid,$2,md5('synthetic recent provider ' || (i % 512))::uuid,512,$1,
				 (now() - interval '10 minutes') AT TIME ZONE 'UTC',
				 (now() - interval '10 minutes' + interval '1 hour') AT TIME ZONE 'UTC',
				 (now() - interval '5 minutes') AT TIME ZONE 'UTC',
				 $3,'{"version":1,"byte_count":0,"providers":[]}'::jsonb
				FROM generate_series(1,$4) i`,
				f.clients.payerNetworkId, f.clients.providerNetworkId, ContractOutcomeSettled, 8*historyCount))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
			// The payer is the prober, so a request above the anchor also reads
			// the prober's maximum over every eligible origin.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO prober_identity(singleton,network_id,client_id)
				VALUES(true,$1,$2) ON CONFLICT(singleton) DO UPDATE
				SET network_id=excluded.network_id,client_id=excluded.client_id`, f.clients.payerNetworkId, server.NewId()))
		})

		// Each actual read returns the earliest closed plain origin, finds that
		// one exists, or finds no closed companion origin: the pair has none.
		type actualQuery struct {
			name    string
			sql     string
			args    string
			columns int
			result  string
			// the prober's maximum over the eligible origins, when read
			maximum int64
			// count only the scans of the closed branch, which test close_time
			closedBranchOnly bool
		}
		pair := fmt.Sprintf("'%s'::uuid,'%s'::uuid,now()-interval '1 hour'", f.clients.payerId, f.clients.providerId)
		plainArgs := fmt.Sprintf("%s,'%s'::uuid", pair, f.clients.payerNetworkId)
		if queries["earliest_origin"].binds == 5 {
			plainArgs += ",17::bigint"
		}
		chainedArgs := fmt.Sprintf("%s,'%s'::uuid,'%s'::uuid,'%s'::uuid", pair, f.clients.payerNetworkId, f.clients.providerNetworkId, f.clients.payerNetworkId)
		if queries["earliest_companion_origin"].binds == 7 {
			chainedArgs += ",17::bigint"
		}
		checks := []actualQuery{
			{name: "escrow plain origin", sql: queries["earliest_origin"].sql, args: plainArgs, columns: 2, result: "anchor"},
			{name: "escrow plain origin prober maximum", sql: queries["earliest_origin"].sql,
				args: fmt.Sprintf("%s,'%s'::uuid,%d::bigint", pair, f.clients.payerNetworkId, f.grown.TransferByteCount+1), columns: 2, result: "anchor",
				maximum: int64(f.grown.TransferByteCount)},
			{name: "escrow chained origin", sql: queries["earliest_companion_origin"].sql, args: chainedArgs, columns: 3, result: "none"},
			{name: "zero escrow plain origin", sql: zeroEscrowPlainOriginSql, args: pair, columns: 1, result: "anchor"},
			{name: "zero escrow chained origin", sql: zeroEscrowChainedOriginSql, args: fmt.Sprintf("%s,'%s'::uuid,'%s'::uuid", pair, f.clients.providerNetworkId, f.clients.payerNetworkId), columns: 2, result: "none"},
			{name: "origin existence", sql: companionOriginExistsSql, args: pair, columns: 1, result: "exists", closedBranchOnly: true},
		}
		server.Db(ctx, func(conn server.PgConn) {
			defer conn.Exec(t.Context(), `RESET plan_cache_mode`)
			for index, check := range checks {
				name := fmt.Sprintf("closed_origin_plan_%d", index)
				server.RaisePgResult(conn.Exec(ctx, `PREPARE `+name+` AS `+check.sql))
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
					values := make([]any, check.columns)
					destinations := make([]any, check.columns)
					for i := range values {
						destinations[i] = &values[i]
					}
					err := conn.QueryRow(ctx, `EXECUTE `+name+`(`+check.args+`)`).Scan(destinations...)
					switch check.result {
					case "anchor":
						server.Raise(err)
						anchor, _ := values[0].([16]byte)
						if server.Id(anchor) != f.anchor.ContractId {
							t.Fatalf("%s %s returned anchor %v, want the earliest closed origin %s", check.name, mode, values[0], f.anchor.ContractId)
						}
						if check.maximum != 0 {
							if maximum, _ := values[1].(int64); maximum != check.maximum {
								t.Fatalf("%s %s returned prober maximum %v, want %d over the closed origins", check.name, mode, values[1], check.maximum)
							}
						}
					case "exists":
						server.Raise(err)
						if exists, _ := values[0].(bool); !exists {
							t.Fatalf("%s %s found no origin, want the closed origins", check.name, mode)
						}
					default:
						if !errors.Is(err, pgx.ErrNoRows) {
							t.Fatalf("%s %s = %v, %v; the pair has no companion origin", check.name, mode, values, err)
						}
					}
					var raw []byte
					server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE `+name+`(`+check.args+`)`).Scan(&raw))
					var plans []map[string]any
					server.Raise(json.Unmarshal(raw, &plans))
					var contractRows, allContractRows float64
					indexNames := []string{}
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if node["Relation Name"] == "transfer_contract" {
							loops, _ := node["Actual Loops"].(float64)
							rows, _ := node["Actual Rows"].(float64)
							removed, _ := node["Rows Removed by Filter"].(float64)
							allContractRows += (rows + removed) * loops
							closedBranch := false
							for _, condition := range []string{"Index Cond", "Filter", "Recheck Cond"} {
								if text, _ := node[condition].(string); strings.Contains(text, "close_time") {
									closedBranch = true
								}
							}
							if closedBranch || !check.closedBranchOnly {
								contractRows += (rows + removed) * loops
							}
							indexName, _ := node["Index Name"].(string)
							indexNames = append(indexNames, fmt.Sprintf("%v:%s:closed=%t", node["Node Type"], indexName, closedBranch))
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								inspect(child.(map[string]any))
							}
						}
					}
					inspect(plans[0]["Plan"].(map[string]any))
					if 64 < contractRows {
						t.Fatalf("%s %s read %g contract rows through %v; the closed origin branch is not bounded by recent contract ids", check.name, mode, contractRows, indexNames)
					}
					t.Logf("%s mode=%s contract_rows=%g all_contract_rows=%g scans=%v", check.name, mode, contractRows, allContractRows, indexNames)
				}
				server.RaisePgResult(conn.Exec(ctx, `DEALLOCATE `+name))
			}
		})
	})
}
