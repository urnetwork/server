package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
)

// `bringyourctl network remove` drops a network's sign-in addresses with it.
// Before the 2026-11-13 review it queued no RemoveProductUpdates for them, as
// the account deletion does, so the Brevo contact stayed after a support
// deletion. It queues one per address now, in the removal's transaction; a
// refused removal queues none.
func TestNetworkRemoveQueuesTheBrevoContactRemoval(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		functionName := task.NewTaskTargetWithPost(controller.RemoveProductUpdates, controller.RemoveProductUpdatesPost).TargetFunctionName()
		queued := func(userAuth string) (n int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*) FROM pending_task WHERE function_name = $1 AND args_json::jsonb->>'user_auth' = $2`,
					functionName,
					userAuth,
				).Scan(&n))
			})
			return
		}

		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-remove-%s", networkId), userId)
		if n := queued(userAuth); n != 0 {
			t.Fatalf("%d removal(s) queued before the command", n)
		}
		var stdout bytes.Buffer
		if code := runNetworkRemove(ctx, &stdout, networkId, userId); code != 0 {
			t.Fatalf("network remove exited %d: %s", code, stdout.String())
		}
		if model.NetworkExists(ctx, networkId) {
			t.Fatal("the network was not removed")
		}
		if n := queued(userAuth); n != 1 {
			t.Fatalf("%d Brevo contact removal(s) queued for the removed address, want 1", n)
		}
		if out := stdout.String(); !strings.Contains(out, "1 sign-in address(es) queued for removal from Brevo") || strings.Contains(out, userAuth) {
			t.Fatalf("output = %q, want the count and never the address", out)
		}

		// an active Stripe renewal refuses the removal: nothing is queued
		refusedNetworkId := server.NewId()
		refusedUserId := server.NewId()
		refusedUserAuth := model.Testing_CreateNetwork(ctx, refusedNetworkId, fmt.Sprintf("synthetic-refused-%s", refusedNetworkId), refusedUserId)
		now := server.NowUtc()
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          refusedNetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-time.Hour),
			EndTime:            now.Add(24 * time.Hour),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      "in_synthetic_refused_remove",
		}); err != nil {
			t.Fatalf("add renewal: %v", err)
		}
		stdout.Reset()
		if code := runNetworkRemove(ctx, &stdout, refusedNetworkId, refusedUserId); code != 1 {
			t.Fatalf("a refused removal exited %d: %s", code, stdout.String())
		}
		if !model.NetworkExists(ctx, refusedNetworkId) {
			t.Fatal("a refused removal removed the network")
		}
		if n := queued(refusedUserAuth); n != 0 {
			t.Fatalf("a refused removal queued %d Brevo contact removal(s)", n)
		}
	})
}
