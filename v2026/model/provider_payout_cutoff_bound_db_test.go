package model

// The earning policy's payout read scans only the sweeps that can earn under it
// (providerPayoutStatsPolicySql). Only a contract that closed settled before the
// cutoff earns, and it was created before the cutoff, so its id is below the
// cutoff's contract id bound. Every window the statistics read on Main opens
// long after the cutoff, and there the read returned no rows while it joined
// each of the network's sweeps in the window to transfer_contract for its close
// time (2026-11-26: 3 to 4 s a call). This fixture is that shape at test size:
// one late sweep of a pre-cutoff contract, the network's other sweeps in the
// window of contracts created after the cutoff, and many more settled contracts
// from before the cutoff than sweeps in the window.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	// the network's sweeps in the window of contracts created after the cutoff
	providerPayoutLateSweeps = 20
	// settled contracts from before the cutoff with no sweep in the window
	providerPayoutPreCutoffContracts = 5000
)

type providerPayoutPlanNode struct {
	NodeType    string                   `json:"Node Type"`
	Relation    string                   `json:"Relation Name"`
	Subplan     string                   `json:"Subplan Name"`
	IndexCond   string                   `json:"Index Cond"`
	RecheckCond string                   `json:"Recheck Cond"`
	Filter      string                   `json:"Filter"`
	ActualRows  float64                  `json:"Actual Rows"`
	ActualLoops float64                  `json:"Actual Loops"`
	Plans       []providerPayoutPlanNode `json:"Plans"`
}

// The scans of the payout_sweeps CTE: the rows its transfer_escrow_sweep scans
// returned, each scan's conditions, and how many times it read
// transfer_contract.
type providerPayoutSweepScans struct {
	sweepRows      float64
	sweepCondition []string
	contractLoops  float64
}

func providerPayoutCteScans(node providerPayoutPlanNode, inCte bool, scans *providerPayoutSweepScans) {
	inCte = inCte || node.Subplan == "CTE payout_sweeps"
	if inCte {
		switch {
		case strings.HasPrefix(node.Relation, "transfer_escrow_sweep"):
			scans.sweepRows += node.ActualRows * node.ActualLoops
			scans.sweepCondition = append(scans.sweepCondition, strings.Join([]string{node.IndexCond, node.RecheckCond, node.Filter}, " "))
		case strings.HasPrefix(node.Relation, "transfer_contract"):
			scans.contractLoops += node.ActualLoops
		}
	}
	for _, child := range node.Plans {
		providerPayoutCteScans(child, inCte, scans)
	}
}

// Under the policy, the payout read's sweep scan returns only the late sweep of
// the pre-cutoff contract, bounded by the cutoff's contract id in the scan
// itself, and the statistics are that contract's payout alone. Before the
// bound, the scan returned every sweep of the network in the window, and each
// was joined to transfer_contract to be dropped by its close time.
func TestProviderPayoutPolicyReadScansOnlySweepsOfPreCutoffContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)

		// as on Main since the cutoff: the window opens long after it
		windowStart := payoutTestCutoff.Add(40 * 24 * time.Hour)
		windowEnd := windowStart.Add(24 * time.Hour)
		// the window's one earning sweep: a contract created and closed settled
		// before the cutoff, swept late
		closed := payoutTestCutoff.Add(-time.Minute)
		f.insertMinted(t, ctx, closed.Add(-time.Hour), &closed, windowStart.Add(time.Hour), 11, UsdToNanoCents(1))
		for i := range providerPayoutLateSweeps {
			created := windowStart.Add(time.Duration(i) * time.Minute)
			closed := created.Add(30 * time.Second)
			f.insertMinted(t, ctx, created, &closed, closed.Add(time.Minute), 1024, UsdToNanoCents(2))
		}
		// the settled history from before the cutoff, minted at creation
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				SELECT
					(lpad(to_hex(floor(extract(epoch FROM created) * 1000)::bigint), 12, '0') || lpad(to_hex(i), 20, '0'))::uuid,
					$1::uuid, $2::uuid, $3::uuid, $4::uuid, 1024, created, created + interval '1 second', 'settled',
					jsonb_build_object('version', 1, 'byte_count', 1024, 'providers',
						jsonb_build_array(jsonb_build_object('client_id', $4::uuid::text, 'network_id', $3::uuid::text, 'byte_count', 1024))),
					true
				FROM generate_series(1, $5::int) AS i,
					LATERAL (SELECT $6::timestamp + i * interval '1 minute' AS created) AS at`,
				f.sourceNetwork, f.source, f.network, f.client, providerPayoutPreCutoffContracts,
				payoutTestCutoff.Add(-10*24*time.Hour),
			))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract, transfer_escrow_sweep`))
		}, server.OptReadWrite(), server.OptNoRetry())

		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil || policy == nil {
			t.Fatalf("missing payout policy: %v", err)
		}
		sql, args := providerPayoutStatsRead(policy, f.network, windowStart, windowEnd)
		var raw []byte
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, unresolvedPayerPlanSettingsSql))
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+sql, args...).Scan(&raw))
		}, server.OptReadOnly())
		var plans []struct {
			Plan providerPayoutPlanNode
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("invalid plan: %v %s", err, raw)
		}
		scans := &providerPayoutSweepScans{}
		providerPayoutCteScans(plans[0].Plan, false, scans)
		t.Logf("payout_sweeps: sweep rows %.0f, transfer_contract reads %.0f, sweep conditions %q", scans.sweepRows, scans.contractLoops, scans.sweepCondition)

		bound := server.ContractIdFloor(payoutTestCutoff.Add(ContractIdCreateTimeMargin)).String()
		if len(scans.sweepCondition) == 0 {
			t.Fatalf("no transfer_escrow_sweep scan in payout_sweeps: %s", raw)
		}
		for _, condition := range scans.sweepCondition {
			if !strings.Contains(condition, "contract_id < '"+bound+"'::uuid") {
				t.Errorf("a sweep scan of payout_sweeps is not bounded by the cutoff's contract id %s: %s", bound, condition)
			}
		}
		if scans.sweepRows != 1 {
			t.Errorf("payout_sweeps scanned %.0f sweeps, want only the pre-cutoff contract's 1 (the window has %d more)", scans.sweepRows, providerPayoutLateSweeps)
		}
		if 1 < scans.contractLoops {
			t.Errorf("payout_sweeps read transfer_contract %.0f times, want at most once", scans.contractLoops)
		}

		// the statistics are unchanged: the pre-cutoff contract's payout alone
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := queryProviderPayoutStats(ctx, conn, f.network, windowStart, windowEnd)
			if err != nil {
				t.Fatal(err)
			}
			total := NanoCents(0)
			for _, amount := range rows[f.client] {
				total += amount
			}
			if total != UsdToNanoCents(1) {
				t.Fatalf("payout %d, want the pre-cutoff contract's %d", total, UsdToNanoCents(1))
			}
		})
	})
}
