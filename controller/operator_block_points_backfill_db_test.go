package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// An earning boundary at `cutoff`, prepared as Main's was.
func useOperatorBlockPointsBackfillTransition(t testing.TB, cutoff time.Time) {
	t.Helper()
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.UTC().Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
	policy, err := server.LoadProviderPayoutTransition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.PrepareProviderPayoutBoundary(context.Background(), policy.ConfigSha256); err != nil {
		t.Fatal(err)
	}
}

type operatorBlockPointsBackfillFixture struct {
	ctx                    context.Context
	payerNetwork, payer    server.Id
	balance                server.Id
	provider, providerUser server.Id
}

func newOperatorBlockPointsBackfillFixture(t testing.TB, ctx context.Context) *operatorBlockPointsBackfillFixture {
	t.Helper()
	f := &operatorBlockPointsBackfillFixture{
		ctx:          ctx,
		payerNetwork: server.NewId(),
		payer:        server.NewId(),
		provider:     server.NewId(),
		providerUser: server.NewId(),
	}
	payerUser := server.NewId()
	model.Testing_CreateNetwork(ctx, f.payerNetwork, "obp-payer", payerUser)
	model.Testing_CreateDevice(ctx, f.payerNetwork, server.NewId(), f.payer, "obp-payer", "synthetic")
	model.Testing_CreateNetwork(ctx, f.provider, "obp-provider", f.providerUser)
	model.Testing_CreateDevice(ctx, f.provider, server.NewId(), f.providerUser, "obp-provider", "synthetic")
	code, err := model.CreateBalanceCode(ctx, 1024*1024*1024*1024, 365*24*time.Hour, model.UsdToNanoCents(100), "obp-"+server.NewId().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: f.payerNetwork}, ctx)
	if err != nil || result == nil || result.Error != nil {
		t.Fatal("redeem", err, result)
	}
	balances := model.GetActiveTransferBalances(ctx, f.payerNetwork)
	if len(balances) != 1 {
		t.Fatal("missing fixture balance")
	}
	f.balance = balances[0].BalanceId
	return f
}

// A settled contract to the provider, closed and swept at `swept`.
func (self *operatorBlockPointsBackfillFixture) contract(swept time.Time, bytes model.ByteCount) {
	contractId := server.NewId()
	usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[{"client_id":%q,"network_id":%q,"byte_count":%d}]}`, bytes, self.providerUser, self.provider, bytes)
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO transfer_contract
		(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9::jsonb,true)`, contractId, self.payerNetwork, self.payer, self.provider, self.providerUser, bytes, swept.Add(-time.Hour), swept, usage))
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO transfer_escrow_sweep
		(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,$5,0,$6)`, contractId, self.balance, self.provider, self.providerUser, bytes, swept))
	})
}

func (self *operatorBlockPointsBackfillFixture) tables(t testing.TB) string {
	t.Helper()
	var counts [5]int64
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT
			(SELECT COUNT(*) FROM account_point),
			(SELECT COUNT(*) FROM account_payment),
			(SELECT COUNT(*) FROM account_payment_block),
			(SELECT COUNT(*) FROM payment_report),
			(SELECT COUNT(*) FROM audit_account_payment)
		`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4]))
	})
	return fmt.Sprint(counts)
}

// The points of the provider by payment plan, and their operator blocks.
func (self *operatorBlockPointsBackfillFixture) points(t testing.TB) map[server.Id][]int64 {
	t.Helper()
	plans := map[server.Id][]int64{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `
			SELECT point.payment_plan_id, block.block_number
			FROM account_point point
			INNER JOIN account_payment_block block ON block.payment_id = point.account_payment_id
			WHERE point.network_id = $1 AND point.point_value > 0
			ORDER BY block.block_number
		`, self.provider)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var planId server.Id
				var block int64
				server.Raise(result.Scan(&planId, &block))
				plans[planId] = append(plans[planId], block)
			}
		})
	})
	return plans
}

// The backfill's dry run writes nothing and prints a digest that binds its
// awards; a commit refuses any other content, commits exactly the dry run's
// awards with their operator blocks, and running it or the dry run again
// creates nothing new: blocks that have their points are skipped.
func TestOperatorBlockPointsBackfillDryRunDigestAndIdempotence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		current := int64(model.SubnetBlockNumber(now))
		// as on Main: the boundary is inside a block, and the payout plans'
		// last subsidy epoch ends at it
		first, second := current-3, current-2
		firstStart := model.SubnetBlockGenesis.Add(time.Duration(first-1) * model.SubnetBlockDuration)
		secondStart := firstStart.Add(model.SubnetBlockDuration)
		cutoff := firstStart.Add(36 * time.Hour)
		useOperatorBlockPointsBackfillTransition(t, cutoff)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO subsidy_payment (payment_plan_id, start_time, end_time, active_user_count, paid_user_count, net_payout_byte_count_paid, net_payout_byte_count_unpaid, net_revenue_nano_cents, net_payout_nano_cents)
				VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)`, server.NewId(), cutoff.Add(-4*24*time.Hour), cutoff.Add(-time.Microsecond)))
		})

		f := newOperatorBlockPointsBackfillFixture(t, ctx)
		// before the boundary in the first block: a payout plan's, not the backfill's
		f.contract(cutoff.Add(-time.Hour), 1024*1024*1024)
		f.contract(cutoff.Add(time.Hour), 1024*1024*1024)
		f.contract(secondStart.Add(time.Hour), 2*1024*1024*1024)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		before := f.tables(t)
		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		backfill, err := PlanOperatorBlockPointsBackfill(ctx, []int64{first, second}, now)
		writeRefused := false
		func() {
			defer func() {
				writeRefused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type) VALUES ($1, $1, 'synthetic_write_probe')`, server.NewId()))
			}, server.OptNoRetry())
		}()
		server.Testing_ReleasePgReadOnly()
		if err != nil {
			t.Fatal(err)
		}
		if !writeRefused {
			t.Fatal("the dry run's process accepted a write")
		}
		if after := f.tables(t); after != before {
			t.Fatal("the dry run wrote", before, after)
		}
		if len(backfill.Awards) != 2 || backfill.Awards[0].Block != first || backfill.Awards[1].Block != second ||
			!backfill.Awards[0].WindowStart.Equal(cutoff) || !backfill.Awards[0].WindowEnd.Equal(secondStart) {
			t.Fatalf("dry run awards %+v", backfill.Awards)
		}
		if backfill.Awards[0].SweepCount != 1 || backfill.Awards[1].SweepCount != 1 {
			t.Fatal("the backfill read traffic outside the blocks past the boundary")
		}
		if backfill.Digest != model.OperatorBlockPointsDigest(backfill.Awards) || len(backfill.Digest) != 64 {
			t.Fatal("the dry run's digest is not its awards'", backfill.Digest)
		}
		if !strings.Contains(backfill.Text(), backfill.Digest) || !strings.Contains(backfill.Text(), f.provider.String()) {
			t.Fatal("the report lacks the digest or the provider's points")
		}
		again, err := PlanOperatorBlockPointsBackfill(ctx, []int64{first, second}, now)
		if err != nil || again.Digest != backfill.Digest {
			t.Fatal("a second dry run has another digest", err)
		}

		// the dry run's file
		data, err := json.Marshal(backfill)
		if err != nil {
			t.Fatal(err)
		}
		parse := func() *OperatorBlockPointsBackfill {
			parsed := &OperatorBlockPointsBackfill{}
			if err := json.Unmarshal(data, parsed); err != nil {
				t.Fatal(err)
			}
			return parsed
		}

		// another digest, or other content, commits nothing
		if _, _, err := CommitOperatorBlockPointsBackfill(clientSession, parse(), strings.Repeat("0", 64)); !errors.Is(err, ErrOperatorBlockPointsBackfillDigest) {
			t.Fatal("a commit accepted another digest", err)
		}
		tampered := parse()
		tampered.Awards[1].Points[0].PointValue *= 2
		if _, _, err := CommitOperatorBlockPointsBackfill(clientSession, tampered, backfill.Digest); !errors.Is(err, ErrOperatorBlockPointsBackfillDigest) {
			t.Fatal("a commit accepted changed awards", err)
		}
		if after := f.tables(t); after != before {
			t.Fatal("a refused commit wrote", before, after)
		}

		committed, skipped, err := CommitOperatorBlockPointsBackfill(clientSession, parse(), backfill.Digest)
		if err != nil || !slices.Equal(committed, []int64{first, second}) || len(skipped) != 0 {
			t.Fatal("commit", committed, skipped, err)
		}
		points := f.points(t)
		if !slices.Equal(points[model.OperatorBlockPointsPlanId(first)], []int64{first}) ||
			!slices.Equal(points[model.OperatorBlockPointsPlanId(second)], []int64{second}) || len(points) != 2 {
			t.Fatalf("points by plan and block %v", points)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var triggered bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pending_task WHERE function_name LIKE '%RebuildPointsLeaderboard')`).Scan(&triggered))
			if !triggered {
				t.Fatal("the commit did not re-rank the points leaderboard")
			}
		})

		// idempotent
		afterCommit := f.tables(t)
		committed, skipped, err = CommitOperatorBlockPointsBackfill(clientSession, parse(), backfill.Digest)
		if err != nil || len(committed) != 0 || !slices.Equal(skipped, []int64{first, second}) {
			t.Fatal("second commit", committed, skipped, err)
		}
		rerun, err := PlanOperatorBlockPointsBackfill(ctx, []int64{first, second}, now)
		if err != nil || len(rerun.Awards) != 0 || !slices.Contains(rerun.AwardedBlocks, first) || !slices.Contains(rerun.AwardedBlocks, second) {
			t.Fatal("the dry run after the commit", rerun, err)
		}
		if after := f.tables(t); after != afterCommit {
			t.Fatal("a second commit wrote", afterCommit, after)
		}

		// a block that is not a completed block past the boundary is refused
		if _, err := PlanOperatorBlockPointsBackfill(ctx, []int64{current}, now); err == nil {
			t.Fatal("the backfill accepted an open block")
		}
	})
}
