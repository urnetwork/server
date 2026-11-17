package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A reviewer's screen_allow through §21's one action path (ABUSE-M5-08): recorded upheld under the review's case,
// applied with nothing to do outside Postgres, mailed off site like any action, owing no notice to the network,
// since an allow is no action against it, and letting the held name's retry pass.
func TestAbuseScreenAllowTakesTheActionPath(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, mailer := newTestAbuseDeps(t)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "screen-allow-network", server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET create_time = $2 WHERE network_id = $1`, networkId, server.NowUtc().Add(-30*24*time.Hour)))
		})
		review := model.AbuseScreenReviewOf(model.ScreenNetworkName(ctx, networkId, "paypal-support"))
		if review == nil || review.CaseId == nil {
			t.Fatalf("the review: %+v", review)
		}
		messages := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, networkId).Scan(&count))
			})
			return
		}
		reviewerId := server.NewId()
		result, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
			ActionId:     server.NewId(),
			CaseId:       *review.CaseId,
			Kind:         model.AbuseActionKindScreenAllow,
			Target:       "paypal-support",
			ReviewerId:   &reviewerId,
			ReviewReason: "the network's own support desk",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Inserted || !result.Applied || !result.Mailed || result.Action.ReviewState != model.AbuseReviewStateUpheld {
			t.Fatalf("the allow: %+v", result)
		}
		if count := messages(); count != 0 {
			t.Fatalf("an allow owed %d notices", count)
		}
		if bodies := mailer.mailed(); len(bodies) != 1 || !strings.Contains(string(bodies[0]), string(model.AbuseActionKindScreenAllow)) {
			t.Fatalf("%d records mailed", len(bodies))
		}
		if err := model.ScreenNetworkName(ctx, networkId, "paypal-support"); err != nil {
			t.Fatalf("the retry after the allow: %v", err)
		}
	})
}
