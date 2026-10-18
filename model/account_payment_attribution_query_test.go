// Attribution must inspect every selected component once, preserve exact money
// arithmetic, and refuse incomplete evidence without reading unrelated history.
package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Only the attribution reader's columns are needed. Keys, nullability and
// payment lookup indexes match the production schema; write guards are covered
// separately by the migrated bonus, recovery and controller tests.
const providerAttributionTestSchema = `
CREATE TABLE account_payment (
 payment_id uuid PRIMARY KEY, payment_plan_id uuid NOT NULL, network_id uuid NOT NULL,
 payout_byte_count bigint NOT NULL, payout_nano_cents bigint NOT NULL,
 subsidy_payout_nano_cents bigint NOT NULL DEFAULT 0,
 reliability_subsidy_nano_cents bigint NOT NULL DEFAULT 0,
 bonus_payout_nano_cents bigint NOT NULL DEFAULT 0,
 completed boolean NOT NULL DEFAULT false, canceled boolean NOT NULL DEFAULT false,
 attribution_review_required boolean NOT NULL DEFAULT false);
CREATE TABLE transfer_contract (contract_id uuid PRIMARY KEY, close_time timestamp, outcome varchar(32));
CREATE TABLE transfer_escrow_sweep (
 contract_id uuid NOT NULL, balance_id uuid NOT NULL, network_id uuid NOT NULL,
 payout_byte_count bigint NOT NULL, payout_net_revenue_nano_cents bigint NOT NULL,
 payment_id uuid, PRIMARY KEY(contract_id,balance_id,network_id));
CREATE INDEX transfer_escrow_sweep_payment_id ON transfer_escrow_sweep(payment_id);
CREATE INDEX transfer_escrow_sweep_payment_contract ON transfer_escrow_sweep(payment_id,contract_id);
CREATE TABLE account_payment_bonus (
 operation_id uuid NOT NULL, origin_payment_id uuid NOT NULL, payment_id uuid NOT NULL,
 network_id uuid NOT NULL, amount_nano_cents bigint NOT NULL,
 latest_contract_close_time timestamp NOT NULL, subsidy_end_time timestamp,
 PRIMARY KEY(operation_id,origin_payment_id));
CREATE INDEX account_payment_bonus_payment ON account_payment_bonus(payment_id);
CREATE TABLE subsidy_payment (payment_plan_id uuid PRIMARY KEY, start_time timestamp NOT NULL, end_time timestamp NOT NULL);
`

// Fixed synthetic ids make SQL mutations and planner layouts reproducible.
func providerAttributionTestId(n uint64) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], n)
	return id
}

// One ordinary legacy payment is the common positive control for each mutation.
func resetProviderAttributionTest(ctx context.Context, conn server.PgConn, cutoff time.Time) {
	server.RaisePgResult(conn.Exec(ctx, `TRUNCATE account_payment, transfer_escrow_sweep, transfer_contract, account_payment_bonus, subsidy_payment`))
	server.RaisePgResult(conn.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents) VALUES($1,$2,$3,1000,100)`, providerAttributionTestId(1), providerAttributionTestId(2), providerAttributionTestId(3)))
	server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract VALUES($1,$2,'settled')`, providerAttributionTestId(5), cutoff.Add(-time.Hour)))
	server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow_sweep VALUES($1,$2,$3,1000,100,$4)`, providerAttributionTestId(5), providerAttributionTestId(4), providerAttributionTestId(3), providerAttributionTestId(1)))
}

// Every caller shares this admission: ordinary sends, canceled recovery, bonus
// authority and the locked replacement's explicit review override.
func TestProviderAttributionAccounting(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		const window = `INSERT INTO subsidy_payment SELECT payment_plan_id,'2020-01-06','2020-01-07' FROM account_payment;`
		const bonus = `INSERT INTO account_payment_bonus SELECT payment_id,payment_id,payment_id,network_id,7,'2020-01-06 23:00','2020-01-07' FROM account_payment; UPDATE account_payment SET bonus_payout_nano_cents=7,payout_nano_cents=107;`
		cases := []struct {
			name            string
			sql             string
			allowCanceled   bool
			allowReviewHeld bool
			withoutPolicy   bool
			wantAllowed     bool
			wantSubsidy     bool
			closeDelta      time.Duration
			subsidyDelta    time.Duration
		}{
			{name: "settled", wantAllowed: true},
			{name: "source dispute", sql: `UPDATE transfer_contract SET outcome='dispute_resolved_to_source'`, wantAllowed: true},
			{name: "destination dispute", sql: `UPDATE transfer_contract SET outcome='dispute_resolved_to_destination'`, wantAllowed: true},
			{name: "no payment", sql: `DELETE FROM account_payment`},
			{name: "no sweep", sql: `DELETE FROM transfer_escrow_sweep`},
			{name: "missing contract", sql: `DELETE FROM transfer_contract`},
			{name: "missing zero value component beside valid contract", sql: `INSERT INTO transfer_escrow_sweep SELECT '00000000-0000-0000-0000-000000000006',balance_id,network_id,0,0,payment_id FROM transfer_escrow_sweep`},
			{name: "null close", sql: `UPDATE transfer_contract SET close_time=NULL`},
			{name: "null outcome", sql: `UPDATE transfer_contract SET outcome=NULL`},
			{name: "nonterminal outcome", sql: `UPDATE transfer_contract SET outcome='pending'`},
			{name: "close at cutoff", sql: `UPDATE transfer_contract SET close_time='2020-01-07'`},
			{name: "close after cutoff", sql: `UPDATE transfer_contract SET close_time='2020-01-08'`},
			{name: "nil policy still needs provenance", sql: `DELETE FROM transfer_contract`, withoutPolicy: true},
			{name: "nil policy has no cutoff", sql: `UPDATE transfer_contract SET close_time='2020-01-08'`, withoutPolicy: true, wantAllowed: true, closeDelta: 25 * time.Hour},
			{name: "foreign sweep", sql: `UPDATE transfer_escrow_sweep SET network_id='00000000-0000-0000-0000-000000000009'`},
			{name: "negative sweep", sql: `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=-1`},
			{name: "negative sweep with matching gross", sql: `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=101; INSERT INTO transfer_escrow_sweep SELECT contract_id,'00000000-0000-0000-0000-000000000006',network_id,0,-1,payment_id FROM transfer_escrow_sweep`},
			{name: "negative payment", sql: `UPDATE account_payment SET payout_nano_cents=-1`},
			{name: "negative subsidy", sql: `UPDATE account_payment SET subsidy_payout_nano_cents=-1,payout_nano_cents=99`},
			{name: "negative reliability", sql: `UPDATE account_payment SET reliability_subsidy_nano_cents=-1,payout_nano_cents=99`},
			{name: "gross mismatch", sql: `UPDATE account_payment SET payout_nano_cents=101`},
			{name: "completed", sql: `UPDATE account_payment SET completed=true`, allowCanceled: true, allowReviewHeld: true},
			{name: "canceled not admitted", sql: `UPDATE account_payment SET canceled=true`},
			{name: "canceled complete census", sql: `UPDATE account_payment SET canceled=true`, allowCanceled: true, wantAllowed: true},
			{name: "canceled partial byte census", sql: `UPDATE account_payment SET canceled=true,payout_byte_count=1001`, allowCanceled: true},
			{name: "ordinary byte count unchanged", sql: `UPDATE account_payment SET payout_byte_count=1001`, wantAllowed: true},
			{name: "review held", sql: `UPDATE account_payment SET attribution_review_required=true`},
			{name: "explicit locked review override", sql: `UPDATE account_payment SET attribution_review_required=true`, allowReviewHeld: true, wantAllowed: true},
			{name: "override still refuses bad provenance", sql: `UPDATE account_payment SET attribution_review_required=true; DELETE FROM transfer_contract`, allowReviewHeld: true},
			{name: "two balances same contract", sql: `INSERT INTO transfer_escrow_sweep SELECT contract_id,'00000000-0000-0000-0000-000000000006',network_id,1000,100,payment_id FROM transfer_escrow_sweep; UPDATE account_payment SET payout_nano_cents=200,payout_byte_count=2000`, allowCanceled: true, wantAllowed: true},
			{name: "exact integer above float range", sql: `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=9007199254740993; UPDATE account_payment SET payout_nano_cents=9007199254740993`, wantAllowed: true},
			{name: "one nano cent above float range", sql: `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=9007199254740993; UPDATE account_payment SET payout_nano_cents=9007199254740992`},
			{name: "component arithmetic exceeds bigint", sql: window + `UPDATE account_payment SET subsidy_payout_nano_cents=9223372036854775807,reliability_subsidy_nano_cents=1,payout_nano_cents=9223372036854775807`},
			{name: "revenue sum exceeds bigint", sql: `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=9223372036854775807; INSERT INTO transfer_escrow_sweep SELECT contract_id,'00000000-0000-0000-0000-000000000006',network_id,0,1,payment_id FROM transfer_escrow_sweep; UPDATE account_payment SET payout_nano_cents=9223372036854775807`},
			{name: "canceled byte sum exceeds bigint", sql: `UPDATE transfer_escrow_sweep SET payout_byte_count=9223372036854775807; INSERT INTO transfer_escrow_sweep SELECT contract_id,'00000000-0000-0000-0000-000000000006',network_id,1,0,payment_id FROM transfer_escrow_sweep; UPDATE account_payment SET payout_byte_count=9223372036854775807`, allowCanceled: true},
			{name: "subsidy cutoff inclusive", sql: window + `UPDATE account_payment SET subsidy_payout_nano_cents=7,payout_nano_cents=107`, wantAllowed: true, wantSubsidy: true},
			{name: "reliability cutoff inclusive", sql: window + `UPDATE account_payment SET reliability_subsidy_nano_cents=7,payout_nano_cents=107`, wantAllowed: true, wantSubsidy: true},
			{name: "missing subsidy window", sql: `UPDATE account_payment SET subsidy_payout_nano_cents=7,payout_nano_cents=107`},
			{name: "empty subsidy window", sql: window + `UPDATE account_payment SET subsidy_payout_nano_cents=7,payout_nano_cents=107; UPDATE subsidy_payment SET start_time=end_time`},
			{name: "reversed subsidy window", sql: window + `UPDATE account_payment SET subsidy_payout_nano_cents=7,payout_nano_cents=107; UPDATE subsidy_payment SET start_time=end_time+interval '1 hour'`},
			{name: "subsidy after cutoff", sql: window + `UPDATE account_payment SET subsidy_payout_nano_cents=7,payout_nano_cents=107; UPDATE subsidy_payment SET end_time='2020-01-08'`},
			{name: "nil policy subsidy window", sql: window + `UPDATE account_payment SET subsidy_payout_nano_cents=7,payout_nano_cents=107; UPDATE subsidy_payment SET end_time='2020-01-08'`, withoutPolicy: true, wantAllowed: true, wantSubsidy: true, subsidyDelta: 24 * time.Hour},
			{name: "unused subsidy window ignored", sql: window, wantAllowed: true},
			{name: "bonus exact ledger", sql: bonus, wantAllowed: true},
			{name: "bonus ledger mismatch", sql: bonus + `UPDATE account_payment_bonus SET amount_nano_cents=6`},
			{name: "bonus foreign network", sql: bonus + `UPDATE account_payment_bonus SET network_id='00000000-0000-0000-0000-000000000009'`},
			{name: "bonus close at cutoff", sql: bonus + `UPDATE account_payment_bonus SET latest_contract_close_time='2020-01-07'`},
			{name: "bonus subsidy after cutoff", sql: bonus + `UPDATE account_payment_bonus SET subsidy_end_time='2020-01-08'`},
			{name: "nil policy bonus cutoff", sql: bonus + `UPDATE account_payment_bonus SET latest_contract_close_time='2020-01-08',subsidy_end_time='2020-01-09'`, withoutPolicy: true, wantAllowed: true},
			{name: "latest close across all contracts", sql: `INSERT INTO transfer_contract VALUES('00000000-0000-0000-0000-000000000006','2020-01-06 23:30','settled'); INSERT INTO transfer_escrow_sweep SELECT '00000000-0000-0000-0000-000000000006',balance_id,network_id,1000,100,payment_id FROM transfer_escrow_sweep; UPDATE account_payment SET payout_nano_cents=200,payout_byte_count=2000`, wantAllowed: true, closeDelta: 30 * time.Minute},
			{name: "zero revenue still requires complete census", sql: `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=0; UPDATE account_payment SET payout_nano_cents=0`, wantAllowed: true},
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, providerAttributionTestSchema))
			for _, testCase := range cases {
				resetProviderAttributionTest(ctx, conn, cutoff)
				if testCase.sql != "" {
					server.RaisePgResult(conn.Exec(ctx, testCase.sql))
				}
				policy := &server.ProviderPayoutTransition{Cutoff: cutoff}
				if testCase.withoutPolicy {
					policy = nil
				}
				close, subsidy, err := inspectProviderUsdcPaymentAttributionWithReview(ctx, conn, providerAttributionTestId(1), policy, testCase.allowCanceled, testCase.allowReviewHeld)
				if !testCase.wantAllowed {
					if !errors.Is(err, ErrProviderUsdcAttributionUnresolved) || close != nil || subsidy != nil {
						t.Errorf("%s: unresolved attribution must refuse with no authority: close=%v subsidy=%v error=%v", testCase.name, close, subsidy, err)
					}
					continue
				}
				wantClose := cutoff.Add(-time.Hour + testCase.closeDelta)
				if err != nil || close == nil || !close.Equal(wantClose) {
					t.Errorf("%s: close=%v want=%v error=%v", testCase.name, close, wantClose, err)
				}
				if testCase.wantSubsidy {
					if subsidy == nil || !subsidy.Equal(cutoff.Add(testCase.subsidyDelta)) {
						t.Errorf("%s: subsidy=%v", testCase.name, subsidy)
					}
				} else if subsidy != nil {
					t.Errorf("%s: unexpected subsidy=%v", testCase.name, subsidy)
				}
			}
		})
		t.Logf("checked %d accounting and refusal cases", len(cases))
	})
}

// Capture the real admission statement, so the work assertion cannot pass on a
// separately optimized copy that the payment path does not execute.
type providerAttributionRecordingQuery struct {
	query server.PgCanQuery
	sql   string
	args  []any
}

// Delegate on the caller's owned connection; never acquire another connection.
func (self *providerAttributionRecordingQuery) Query(ctx context.Context, sql string, args ...any) (server.PgResult, error) {
	self.sql = sql
	self.args = args
	return self.query.Query(ctx, sql, args...)
}

// Relation-level actual row visits count discarded rows as well as output.
// Buffer counts are evidence, not a latency or cache-hit performance promise.
type providerAttributionPlan struct {
	RelationName string                    `json:"Relation Name"`
	ActualRows   float64                   `json:"Actual Rows"`
	ActualLoops  float64                   `json:"Actual Loops"`
	FilteredRows float64                   `json:"Rows Removed by Filter"`
	RecheckRows  float64                   `json:"Rows Removed by Index Recheck"`
	SharedHits   int64                     `json:"Shared Hit Blocks"`
	SharedReads  int64                     `json:"Shared Read Blocks"`
	Plans        []providerAttributionPlan `json:"Plans"`
}

// PostgreSQL reports rows per loop; count each relation scan, not join output.
func (self providerAttributionPlan) visits(relation string) float64 {
	var count float64
	if self.RelationName == relation {
		count = (self.ActualRows + self.FilteredRows + self.RecheckRows) * self.ActualLoops
	}
	for _, plan := range self.Plans {
		count += plan.visits(relation)
	}
	return count
}

// A complete selected census must have one sweep pass and contract-key work
// proportional to that selection, independently of canceled admission.
func TestProviderAttributionSelectedRowWork(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, providerAttributionTestSchema))
			for _, selected := range []int{64, 128} {
				for _, interleaved := range []bool{false, true} {
					resetProviderAttributionTest(ctx, conn, cutoff)
					server.RaisePgResult(conn.Exec(ctx, `TRUNCATE transfer_escrow_sweep,transfer_contract`))
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract SELECT lpad(to_hex(n),32,'0')::uuid,$2::timestamp,'settled' FROM generate_series(1,$1::int) n`, selected*65, cutoff.Add(-time.Hour)))
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow_sweep SELECT lpad(to_hex(n),32,'0')::uuid,$1,$2,100,17,CASE WHEN CASE WHEN $4::boolean THEN n % 65=0 ELSE n <= $5::int END THEN $3::uuid ELSE $6::uuid END FROM generate_series(1,$5::int*65) n`, providerAttributionTestId(4), providerAttributionTestId(3), providerAttributionTestId(1), interleaved, selected, providerAttributionTestId(10)))
					server.RaisePgResult(conn.Exec(ctx, `UPDATE account_payment SET payout_nano_cents=$1,payout_byte_count=$2`, selected*17, selected*100))
					server.RaisePgResult(conn.Exec(ctx, `ANALYZE account_payment; ANALYZE transfer_escrow_sweep; ANALYZE transfer_contract`))
					for _, canceled := range []bool{false, true} {
						server.RaisePgResult(conn.Exec(ctx, `UPDATE account_payment SET canceled=$1`, canceled))
						query := &providerAttributionRecordingQuery{query: conn}
						close, _, err := inspectProviderUsdcPaymentAttribution(ctx, query, providerAttributionTestId(1), &server.ProviderPayoutTransition{Cutoff: cutoff}, canceled)
						if err != nil || close == nil || !close.Equal(cutoff.Add(-time.Hour)) {
							t.Fatalf("selected=%d interleaved=%v canceled=%v: real admission failed: %v", selected, interleaved, canceled, err)
						}
						var plans []struct {
							Plan providerAttributionPlan `json:"Plan"`
						}
						rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON, TIMING OFF) "+query.sql, query.args...)
						server.WithPgResult(rows, err, func() {
							if !rows.Next() {
								t.Fatal("missing attribution EXPLAIN")
							}
							var raw []byte
							server.Raise(rows.Scan(&raw))
							server.Raise(json.Unmarshal(raw, &plans))
						})
						if len(plans) != 1 {
							t.Fatalf("unexpected plan count: %d", len(plans))
						}
						plan := plans[0].Plan
						sweeps := plan.visits("transfer_escrow_sweep")
						contracts := plan.visits("transfer_contract")
						t.Logf("selected=%d unrelated=%d interleaved=%v canceled=%v sweep_visits=%.0f contract_visits=%.0f shared_hits=%d shared_reads=%d", selected, selected*64, interleaved, canceled, sweeps, contracts, plan.SharedHits, plan.SharedReads)
						if sweeps != float64(selected) || contracts != float64(selected) {
							t.Errorf("attribution repeats the selected census or visits unrelated contracts: sweep=%.0f contract=%.0f want=%d each", sweeps, contracts, selected)
						}
					}
				}
			}
		})
	})
}

// A defect at the end of a large selection must still refuse: this authority
// cannot use a bounded diagnostic sample, an arbitrary limit or a valid prefix.
func TestProviderAttributionRequiresCompleteCensus(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, providerAttributionTestSchema))
			resetProviderAttributionTest(ctx, conn, cutoff)
			server.RaisePgResult(conn.Exec(ctx, `TRUNCATE transfer_escrow_sweep,transfer_contract`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract SELECT lpad(to_hex(n),32,'0')::uuid,$1::timestamp,'settled' FROM generate_series(1,2049) n`, cutoff.Add(-time.Hour)))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow_sweep SELECT contract_id,$1,$2,1,1,$3 FROM transfer_contract`, providerAttributionTestId(4), providerAttributionTestId(3), providerAttributionTestId(1)))
			server.RaisePgResult(conn.Exec(ctx, `UPDATE account_payment SET payout_nano_cents=2049,payout_byte_count=2049`))
			policy := &server.ProviderPayoutTransition{Cutoff: cutoff}
			if _, _, err := inspectProviderUsdcPaymentAttribution(ctx, conn, providerAttributionTestId(1), policy, true); err != nil {
				t.Fatalf("complete large payment: %v", err)
			}
			server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET close_time=$1 WHERE contract_id=$2`, cutoff, providerAttributionTestId(2049)))
			if _, _, err := inspectProviderUsdcPaymentAttribution(ctx, conn, providerAttributionTestId(1), policy, true); !errors.Is(err, ErrProviderUsdcAttributionUnresolved) {
				t.Fatalf("post-cutoff final component must refuse: %v", err)
			}
			server.RaisePgResult(conn.Exec(ctx, `DROP TABLE transfer_contract`))
			if close, subsidy, err := inspectProviderUsdcPaymentAttribution(ctx, conn, providerAttributionTestId(1), policy, true); err == nil || errors.Is(err, ErrProviderUsdcAttributionUnresolved) || close != nil || subsidy != nil {
				t.Fatalf("query failure must remain unavailable, not debt-invalid evidence: close=%v subsidy=%v error=%v", close, subsidy, err)
			}
		})
	})
}
