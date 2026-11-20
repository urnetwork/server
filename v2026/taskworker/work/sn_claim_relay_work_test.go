package work

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
)

// The relay is one chain: however often it is scheduled there is one pending task,
// the Post keeps it going at the time its pass asked for, and a deployment that is
// no longer the configured one ends it. Task-level: the pass itself is
// controller/sn_claim_relay_test.go.
func TestSnClaimRelayIsOneChain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, Netuid: 521, NoId: 7, DeploymentId: "synthetic-claim-relay", ContractAddress: common.Address{0xc0}}
		controller.SetStConfig(cfg)
		t.Cleanup(func() { controller.SetStConfig(nil) })
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		deploymentKey := string(cfg.DeploymentKey())
		count := func() (tasks int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name LIKE '%SnClaimRelay'`).Scan(&tasks))
			})
			return
		}
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleSnClaimRelay(owner, tx)
			ScheduleSnClaimRelay(owner, tx)
		})
		if tasks := count(); tasks != 1 {
			t.Fatalf("%d pending tasks", tasks)
		}
		later := server.NowUtc().Add(5 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(SnClaimRelayPost(&SnClaimRelayArgs{DeploymentKey: deploymentKey}, &SnClaimRelayResult{RunAt: later}, owner, tx))
		})
		if tasks := count(); tasks != 1 {
			t.Fatalf("%d pending tasks after the post", tasks)
		}
		// an old deployment's chain ends
		before := count()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(SnClaimRelayPost(&SnClaimRelayArgs{DeploymentKey: "another"}, &SnClaimRelayResult{RunAt: later}, owner, tx))
		})
		if tasks := count(); tasks != before {
			t.Fatalf("%d pending tasks after another deployment's post", tasks)
		}
		result, err := SnClaimRelay(&SnClaimRelayArgs{DeploymentKey: "another"}, owner)
		if err != nil || !result.RunAt.IsZero() {
			t.Fatalf("another deployment's run: %+v (%v)", result, err)
		}
	})
}
