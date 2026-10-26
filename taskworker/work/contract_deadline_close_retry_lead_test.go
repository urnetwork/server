// New deadlines that use every run's budget no longer hold the retry pass in
// place: the run after one whose new-deadline pass was cut leads with the
// retry and backlog passes.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Reports the fixture's escrow scheduledCloseOwnerEscrowBytes times this many
// from each party: more than the escrow holds.
const retryLeadShortReportMultiple = 5

// One contract minted at mintedAt with the stored deadline, the fixture's
// legacy escrow and bilateral final reports larger than that escrow.
func (self *scheduledCloseOwnerFixture) addShortEscrowDeadlineContract(ctx context.Context, payer *scheduledCloseOwnerPayer,
	mintedAt time.Time, deadline time.Time) server.Id {
	contractId := contractIdMintedAt(mintedAt)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,payer_network_id,usage_origin_is_source,create_time,expiration_time)
			VALUES ($1,$2,$3,$4,$5,$6,$2,true,$7,$8)`,
			contractId, payer.networkId, payer.clientId, self.providerNetworkId, self.providerClientId, scheduledCloseOwnerEscrowBytes, mintedAt, deadline))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count) VALUES ($1,$2,$3)`,
			contractId, payer.balanceId, scheduledCloseOwnerEscrowBytes))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			SELECT $1,party,$2,$3,false FROM (VALUES ('source'),('destination')) AS report(party)`,
			contractId, retryLeadShortReportMultiple*scheduledCloseOwnerEscrowBytes, mintedAt.Add(time.Minute)))
	})
	payer.contractIds = append(payer.contractIds, contractId)
	return contractId
}

// Production shape (Main, 2026-10-26): new deadlines arrived faster than one
// run closed them, so each run's new-deadline pass used the whole budget. The
// retry span kept its position all day, and the contracts in it, which
// earlier runs had left open, stayed open for days. Here every run gets
// budget for one page and two owners while three owners' contracts become due
// since the run before, and the retry span holds two payers' contracts that
// were due three hours ago: one closes with its reports, the other with
// reports larger than its legacy escrow, which pays the escrow and no more.
// The unchanged pass never moved the retry span, and all of them stayed open.
// Now the run after a cut new-deadline pass closes them at their stored
// deadlines, and the new deadlines the cut passes left close once a run has
// budget for them.
func TestContractDeadlinePassRetriesWhileNewDeadlinesUseEveryBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		start := server.NowUtc().Truncate(time.Millisecond)
		strandedMintedAt := start.Add(-4 * time.Hour)
		strandedDeadline := strandedMintedAt.Add(model.DefaultContractExpiration)
		stranded := f.newPayer(t, t.Context(), "synthetic-retry-lead-stranded-payer")
		f.addDeadlineContracts(t.Context(), stranded, strandedMintedAt, 3, &strandedDeadline)
		short := f.newPayer(t, t.Context(), "synthetic-retry-lead-short-escrow-payer")
		shortMintedAt := strandedMintedAt.Add(time.Second)
		shortDeadline := shortMintedAt.Add(model.DefaultContractExpiration)
		shortId := f.addShortEscrowDeadlineContract(t.Context(), short, shortMintedAt, shortDeadline)
		arriving := []*scheduledCloseOwnerPayer{}
		for index := range 3 {
			arriving = append(arriving, f.newPayer(t, t.Context(), fmt.Sprintf("synthetic-retry-lead-arriving-payer-%d", index)))
		}
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		// The position of a run a minute ago, with the earlier contracts in
		// its retry span.
		edge := func(now time.Time) time.Time {
			return now.Add(-model.DefaultContractExpiration - contractDeadlineCloseEdgeMargin)
		}
		position := fmt.Sprintf(`{"version":%d,"fresh":"%s","retry":{"lower":"%s","upper":"%s"},"backlog_start":"%s"}`,
			contractDeadlineCloseVersion, model.ContractIdTimeBound(edge(start.Add(-time.Minute))),
			model.ContractIdTimeBound(start.Add(-5*time.Hour)), model.ContractIdTimeBound(start.Add(-3*time.Hour)),
			model.ContractIdTimeBound(start.Add(-6*time.Hour)))
		args := &CloseContractDeadlinesArgs{}
		server.Raise(json.Unmarshal([]byte(position), args))

		arrived := map[*scheduledCloseOwnerPayer][]server.Id{}
		for run := range 4 {
			now := start.Add(time.Duration(run) * time.Minute)
			for index, payer := range arriving {
				mintedAt := edge(now).Add(-time.Duration(10*(index+1)) * time.Second)
				deadline := mintedAt.Add(model.DefaultContractExpiration)
				arrived[payer] = append(arrived[payer], f.addDeadlineContracts(t.Context(), payer, mintedAt, 1, &deadline)...)
			}
			result := runContractDeadlinePassWithin(t, ctx, args, now, 3)
			if !result.Exhausted {
				t.Fatalf("run %d did not use its whole budget: %+v", run, result)
			}
			args = &result.Next
		}

		server.Db(ctx, func(conn server.PgConn) {
			var open int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NULL`,
				append(append([]server.Id{}, stranded.contractIds...), shortId)).Scan(&open))
			if open != 0 {
				t.Fatalf("%d contracts in the retry span stayed open while new deadlines used every run's budget: %+v", open, args)
			}
		})
		requireScheduledCloseOwnerSettled(t, ctx, stranded, len(stranded.contractIds))
		requireContractsSettledAt(t, ctx, stranded.contractIds, strandedDeadline)
		requireContractsSettledAt(t, ctx, []server.Id{shortId}, shortDeadline)
		server.Db(ctx, func(conn server.PgConn) {
			var outcome *string
			var payout, swept, balance model.ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT outcome FROM transfer_contract WHERE contract_id=$1),
				(SELECT payout_byte_count FROM transfer_escrow WHERE contract_id=$1 AND settled),
				(SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=$1),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)`,
				shortId, short.balanceId).Scan(&outcome, &payout, &swept, &balance))
			if outcome == nil || *outcome != model.ContractOutcomeSettled || payout != scheduledCloseOwnerEscrowBytes ||
				swept != scheduledCloseOwnerEscrowBytes || balance != scheduledCloseOwnerInitialBytes-scheduledCloseOwnerEscrowBytes {
				t.Fatalf("short escrow did not settle for exactly its escrow: outcome=%v payout=%d swept=%d balance=%d", outcome, payout, swept, balance)
			}
		})

		// Nothing the cut passes left was dropped: runs with budget close every
		// contract that arrived, at its stored deadline.
		for run := range 3 {
			final := runContractDeadlinePass(t, ctx, args, start.Add(time.Duration(5+run)*time.Minute))
			args = &final.Next
		}
		if args.Retry != nil || args.Pending != nil {
			t.Fatalf("runs with budget left work behind: %+v", args)
		}
		for _, payer := range arriving {
			requireScheduledCloseOwnerSettled(t, ctx, payer, len(arrived[payer]))
			for _, contractId := range arrived[payer] {
				var deadline time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&deadline))
				})
				requireContractsSettledAt(t, ctx, []server.Id{contractId}, deadline)
			}
		}
	})
}
