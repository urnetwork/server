package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The legacy owner retains its reservation while ordinary later contracts
// remain eligible for the close sweep's existing independent per-scan cap.
func TestForceClosePendingIntentCannotOccupySelectionHead(t *testing.T) {
	for _, disputed := range []bool{false, true} {
		name := "open"
		if disputed {
			name = "disputed"
		}
		t.Run(name, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
				defer cancel()
				f, pending := legacySettlementTestIntent(t, ctx)
				later, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
				if err != nil {
					t.Fatal(err)
				}
				aged := server.NowUtc().Add(-2 * time.Hour)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,dispute=$3 WHERE contract_id=$1`, pending, aged, disputed))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, pending, aged))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,dispute=$3 WHERE contract_id=$1`, later, aged.Add(time.Minute), disputed))
					if disputed {
						// A no-escrow zero-usage dispute exercises the other selector
						// without adding a second legacy financial owner.
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
                            VALUES($1,'source',0,$2,false),($1,'destination',0,$2,false)`, later, aged))
					}
				})
				closed, cursor, err := ForceCloseOpenContractIdsPage(ctx, aged.Add(time.Hour), 1, 1, 0, 0, nil)
				if err != nil || closed != 0 || cursor == nil {
					t.Fatalf("owned head did not advance its bounded cursor: closed=%d cursor=%t err=%v", closed, cursor != nil, err)
				}
				closed, cursor, err = ForceCloseOpenContractIdsPage(ctx, aged.Add(time.Hour), 1, 1, 0, 0, cursor)
				if err != nil || closed != 1 {
					t.Fatalf("pending head starved later %s contract: verified=%d err=%v", name, closed, err)
				}
				requireLegacySettlementTestState(t, ctx, f, pending, true, false, 1000, 100)
				server.Db(ctx, func(conn server.PgConn) {
					var terminal bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, later).Scan(&terminal))
					if !terminal {
						t.Fatal("later ordinary contract did not reach a final outcome")
					}
				})
			})
		})
	}
}

// Equal creation timestamps need the contract-id tie-breaker. A pass also
// retains its upper time bound, so later arrivals cannot prevent completion.
func TestForceCloseCursorTiesAndFixedPassBoundary(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := server.NewId()
		source, destination := server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{source: network, destination: network})
		aged := server.NowUtc().Add(-time.Hour)
		var ids []server.Id
		for range 5 {
			id, err := CreateContractNoEscrow(ctx, network, source, network, destination, 100)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, id, aged))
			})
		}
		count, cursor, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc(), 2, 1, 0, 0, nil)
		if err != nil || count != 2 || cursor == nil {
			t.Fatal("first tied page failed", count, err)
		}
		bound := cursor.ScanBefore
		later, err := CreateContractNoEscrow(ctx, network, source, network, destination, 100)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, later, bound.Add(time.Minute)))
		})
		for page := 0; cursor != nil && page < 3; page++ {
			if !cursor.ScanBefore.Equal(bound) {
				t.Fatal("continuation moved its pass boundary")
			}
			c, next, err := ForceCloseOpenContractIdsPage(ctx, bound.Add(2*time.Minute), 2, 1, 0, 0, cursor)
			if err != nil {
				t.Fatal(err)
			}
			count += c
			cursor = next
		}
		if count != 5 || cursor != nil {
			t.Fatal("tied cursor skipped, duplicated, or failed to finish", count, cursor != nil)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var closed int
			var laterClosed bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome IS NOT NULL`, ids).Scan(&closed))
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, later).Scan(&laterClosed))
			if closed != 5 || laterClosed {
				t.Fatal("pass outcome crossed fixed boundary", closed, laterClosed)
			}
		})
		c, next, err := ForceCloseOpenContractIdsPage(ctx, bound.Add(2*time.Minute), 2, 1, 0, 0, &ContractExpiryCursor{ScanBefore: bound.Add(2 * time.Minute)})
		if err != nil || c != 1 || next != nil {
			t.Fatal("fresh pass did not recover later eligible arrival", c, err)
		}
	})
}

// Execute the actual two runtime statements, including their original caps,
// past a 32k intent-owned head each. The input remains a bounded test fixture;
// this checks local planner cost and index use, not production scan duration.
func TestForceCloseIntentLookupsRemainIndexed(t *testing.T) {
	queries := []string{forceCloseOpenContractPageSql, forceCloseDisputedContractPageSql}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `WITH inserted AS (
              INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,dispute)
              SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,
                  now()-interval '2 hours'+g*interval '1 microsecond',kind.dispute
              FROM generate_series(1,32768) g CROSS JOIN (VALUES(false),(true)) kind(dispute)
              RETURNING contract_id
            ) INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
              SELECT contract_id,get_byte(uuid_send(contract_id),15)%16,'settled' FROM inserted`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,dispute)
              SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,
                  now()-interval '1 hour'+g*interval '1 microsecond',kind.dispute
              FROM generate_series(1,32) g CROSS JOIN (VALUES(false),(true)) kind(dispute)`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract; ANALYZE legacy_settlement_intent; ANALYZE contract_close`))
		})
		for i, query := range queries {
			args := []any{server.NowUtc().Add(-5 * time.Minute), 25000, time.Time{}, server.Id{}, server.NowUtc()}
			if i == 0 {
				args = []any{ContractPartySource, ContractPartyDestination, args[0], args[1], args[2], args[3], args[4]}
			}
			var raw []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query, args...).Scan(&raw))
			})
			var plans []map[string]any
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 {
				t.Fatal("missing runtime plan")
			}
			root := plans[0]["Plan"].(map[string]any)
			if root["Actual Rows"].(float64) != 25000 {
				t.Fatalf("bounded raw selector rows=%v want25000", root["Actual Rows"])
			}
			var primaryProbe, orderedScan bool
			var walk func(map[string]any)
			walk = func(n map[string]any) {
				if n["Relation Name"] == "legacy_settlement_intent" {
					if n["Node Type"] != "Index Only Scan" && n["Node Type"] != "Index Scan" {
						t.Fatalf("intent lookup became %v", n["Node Type"])
					}
					if n["Actual Rows"].(float64) > 1 {
						t.Fatal("intent probe returned more than its unique-key owner")
					}
					primaryProbe = true
				}
				if n["Relation Name"] == "transfer_contract" && strings.Contains(n["Node Type"].(string), "Index") {
					orderedScan = true
				}
				if children, ok := n["Plans"].([]any); ok {
					for _, child := range children {
						walk(child.(map[string]any))
					}
				}
			}
			walk(root)
			if !primaryProbe || !orderedScan {
				t.Fatal("runtime selector lost indexed ownership or ordered traversal")
			}
			t.Logf("selector=%d pending_head=32768 raw_rows=25000 execution_ms=%v", i, plans[0]["Execution Time"])
		}
	})
}
