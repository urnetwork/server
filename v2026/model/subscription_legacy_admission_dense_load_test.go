// The continuation workload keeps the ordinary grant/provider distribution and
// accounting oracle while placing 320 rows on each of four active shards.
// Initial public pages establish every continuation; no cursor is synthesized.
package model

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func legacyAdmissionSeedDenseLoad(t testing.TB, ctx context.Context) legacyTargetLoadFixture {
	t.Helper()
	f := legacyTargetLoadFixture{targets: map[server.Id]int{}, grantCounts: make([]int, 2), providerCounts: make([]int, 4)}
	for range 2 {
		f.payers = append(f.payers, newNetEscrowOrderingTestFixture(t, ctx))
	}
	for range 4 {
		f.providers = append(f.providers, newNetEscrowOrderingTestFixture(t, ctx))
	}
	prefix := server.NewId()
	groups := [2][4][]server.Id{}
	shards := [2][4][]int{}
	due := [2][4][]time.Time{}
	oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
	for shard := range 4 {
		for position := range 320 {
			id := prefix
			binary.BigEndian.PutUint32(id[11:15], uint32(1+320*shard+position))
			id[15] = byte(shard)
			grant := 0
			if position%16 == 15 {
				grant = 1
			}
			provider := position % 4
			f.ids = append(f.ids, id)
			groups[grant][provider] = append(groups[grant][provider], id)
			shards[grant][provider] = append(shards[grant][provider], shard)
			due[grant][provider] = append(due[grant][provider], oldest.Add(time.Duration(position)*time.Second))
			f.grantCounts[grant]++
			f.providerCounts[provider]++
			if position == 0 || position == 319 {
				f.targets[id] = 2*shard + grant
			}
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for grant, payer := range f.payers {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=4000,balance_byte_count=4000,net_revenue_nano_cents=8000 WHERE balance_id=$1`, payer.balanceId))
			for provider, destination := range f.providers {
				ids := groups[grant][provider]
				if len(ids) == 0 {
					continue
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
					SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS row(id)`, ids, payer.sourceNetworkId, payer.sourceId, destination.destinationNetworkId, destination.destinationId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					SELECT id,$2,2 FROM unnest($1::uuid[]) AS row(id)`, ids, payer.balanceId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS row(id)
					CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
					SELECT id,shard,'settled',false,due FROM unnest($1::uuid[],$2::smallint[],$3::timestamp[]) AS row(id,shard,due)`, ids, shards[grant][provider], due[grant][provider]))
			}
		}
	})
	for _, payer := range f.payers {
		legacy, posts := createNetEscrowOrderingTestContract(ctx, payer, 23)
		server.RunPosts(ctx, posts...)
		f.legacyNeighbors = append(f.legacyNeighbors, legacy.ContractId)
		redis := createRedisAdmissionTest(ctx, payer, 31)
		f.redisNeighbors = append(f.redisNeighbors, redis.ContractId)
		server.Redis(ctx, func(r server.RedisClient) {
			expiry, err := r.ZScore(ctx, redisContractReservationKeys(payer.balanceId)[2], redis.ContractId.String()).Result()
			server.Raise(err)
			f.redisExpiry = append(f.redisExpiry, expiry)
		})
		refreshNetEscrow(ctx, []server.Id{payer.balanceId})
	}
	if len(f.ids) != 1280 || len(f.targets) != 8 || f.grantCounts[0] != 1200 || f.grantCounts[1] != 80 {
		t.Fatal("loaded target fixture distribution changed")
	}
	return f
}

func legacyAdmissionAssertDenseLoadConservation(t testing.TB, ctx context.Context, f legacyTargetLoadFixture) {
	t.Helper()
	projectLegacyProviderTotalsForTest(t, ctx)
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=1280
			AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
			AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=1)=1280
			AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1280
			AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1280`, f.ids).Scan(&exact))
		if !exact {
			t.Fatal("loaded cohort outcome, escrow, debit or provider sweep did not conserve")
		}
		for index, payer := range f.payers {
			var balance int64
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, payer.balanceId).Scan(&balance))
			if balance != 4000-int64(f.grantCounts[index]) {
				t.Fatal("loaded grant debit mismatch", index, balance)
			}
			var neighbors bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$1)
				AND (SELECT NOT settled AND NOT redis_reserved AND balance_byte_count=23 FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$3)
				AND (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$2)
				AND (SELECT NOT settled AND redis_reserved AND balance_byte_count=31 FROM transfer_escrow WHERE contract_id=$2 AND balance_id=$3)`, f.legacyNeighbors[index], f.redisNeighbors[index], payer.balanceId).Scan(&neighbors))
			if !neighbors {
				t.Fatal("loaded settlement changed a surviving neighbor", index)
			}
		}
		for index, provider := range f.providers {
			var bytes, revenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1`, provider.destinationNetworkId).Scan(&bytes, &revenue))
			if bytes != int64(f.providerCounts[index]) || revenue != bytes {
				t.Fatal("loaded provider accounting mismatch", index, bytes, revenue)
			}
		}
	})
	for index, payer := range f.payers {
		if got := Testing_NetEscrowByteCount(ctx, payer.balanceId); got != 54 {
			t.Fatal("loaded settlement lost legacy23 or native31 neighbor", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			keys := redisContractReservationKeys(payer.balanceId)
			amount, err := r.HGet(ctx, keys[1], f.redisNeighbors[index].String()).Int64()
			server.Raise(err)
			expiry, err := r.ZScore(ctx, keys[2], f.redisNeighbors[index].String()).Result()
			server.Raise(err)
			if amount != 31 || expiry != f.redisExpiry[index] {
				t.Fatal("loaded mirror changed native neighbor token", index, amount, expiry)
			}
		})
	}
}
