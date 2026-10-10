// Pair permission remains available only while some eligible grant is live.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Exercise the existing durable existence query directly in both directions.
// The expired row remains unresolved throughout: only its absolute deadline
// removes authority, while a live neighbor and legacy NULL retain their own.
func TestContractExpirationDurablePairKeepsLiveAndLegacyAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, shape := range []string{"pair", "self"} {
			source, destination := server.NewId(), server.NewId()
			if shape == "self" {
				destination = source
			}
			check := func(phase string, want bool) {
				for _, direction := range []struct {
					name                string
					source, destination server.Id
				}{
					{name: "forward", source: source, destination: destination},
					{name: "reverse", source: destination, destination: source},
				} {
					if got := HasOpenContractForPair(ctx, direction.source, direction.destination); got != want {
						t.Fatalf("%s %s %s: permission=%t want=%t", shape, direction.name, phase, got, want)
					}
				}
			}
			ancient := time.UnixMilli(1).UTC()
			insertContractHoleSourceRow(ctx, source, destination, ancient)
			check("only expired unresolved grant", false)
			live := insertContractHoleSourceRow(ctx, destination, source, server.NowUtc().Add(24*time.Hour))
			check("live reverse neighbor", true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, live, ancient))
			})
			check("all finite grants expired", false)
			legacy := insertContractHoleSourceRow(ctx, source, destination)
			check("legacy NULL neighbor", true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, legacy))
			})
			check("disputed legacy neighbor", false)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=false WHERE contract_id=$1`, legacy))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint)
					VALUES($1,'source',0,false)`, legacy))
			})
			check("final-closed legacy neighbor", false)
		}
	})
}
