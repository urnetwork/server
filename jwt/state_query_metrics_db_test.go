// Real SQL controls separate absent rows from rows rejected by Go rotation.
package jwt

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Signature/claim failures do not send state SQL. Once sent, every completed
// attempt has its own caller and verdict, including an unannotated caller.
func TestStateQueryMetricActualSqlOutcomes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'synthetic','password',true)`, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,'query-metrics',$2)`, networkId, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, deviceId, networkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description) VALUES($1,$2,$3,'synthetic')`, clientId, networkId, deviceId))
		})
		account := NewByJwt(networkId, userId, "query-metrics", false, false)
		client := account.Client(deviceId, clientId)
		for _, control := range []struct {
			source  StateQuerySource
			claims  *ByJwt
			active  bool
			rotated bool
			outcome stateQueryOutcome
		}{
			{StateQueryUnknown, account, true, false, stateQueryValid},
			{StateQueryConnectH1, client, true, false, stateQueryValid},
			{StateQueryConnectH3, client, false, false, stateQueryNoActiveRow},
			{StateQueryProberControl, client, true, true, stateQueryCredentialRotated},
			{StateQueryApiRefresh, account, true, true, stateQueryCredentialRotated},
			{StateQueryHostedControl, client, true, false, stateQueryValid},
		} {
			changed := time.Unix(0, 0).UTC()
			if control.rotated {
				changed = server.NowUtc().Add(time.Minute)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=$2 WHERE client_id=$1`, clientId, control.active))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, userId, changed))
			})
			isClient := control.claims.ClientId != nil
			before := stateQueryCount(control.source, isClient, control.outcome)
			queryCtx := ctx
			if control.source != StateQueryUnknown {
				queryCtx = WithStateQuerySource(queryCtx, control.source)
			}
			err := ValidateByJwtState(queryCtx, control.claims, false)
			if (err == nil) != (control.outcome == stateQueryValid) || stateQueryCount(control.source, isClient, control.outcome) != before+1 {
				t.Fatalf("SQL outcome changed: source=%d outcome=%d rejected=%t", control.source, control.outcome, err != nil)
			}
		}
		before := 0.0
		for _, client := range []bool{false, true} {
			for outcome := stateQueryOutcome(0); outcome < stateQueryOutcomeCount; outcome++ {
				before += stateQueryCount(StateQueryUnknown, client, outcome)
			}
		}
		if err := ValidateByJwtState(ctx, nil, false); err == nil {
			t.Fatal("missing credential accepted")
		}
		if err := ValidateByJwtState(ctx, account, true); err == nil {
			t.Fatal("account credential accepted as a client")
		}
		if _, err := ParseByJwtForAudience(ctx, "synthetic.invalid.signature", ByJwtAudienceApi); err == nil {
			t.Fatal("invalid signature accepted")
		}
		after := 0.0
		for _, client := range []bool{false, true} {
			for outcome := stateQueryOutcome(0); outcome < stateQueryOutcomeCount; outcome++ {
				after += stateQueryCount(StateQueryUnknown, client, outcome)
			}
		}
		if after != before {
			t.Fatal("rejection before SQL was reported as query work")
		}
	})
}
