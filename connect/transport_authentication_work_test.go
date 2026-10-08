package connect

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

func newConnectAuthenticationWorkClaims(ctx context.Context) *jwt.ByJwt {
	networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	networkName := "handshake-" + networkId.String()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,$2,'password',true)`, userId, "handshake-"+userId.String()))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, networkId, networkName, userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'handshake','synthetic')`, deviceId, networkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description) VALUES($1,$2,$3,'handshake')`, clientId, networkId, deviceId))
	})
	return jwt.NewByJwt(networkId, userId, networkName, false, false).Client(deviceId, clientId)
}

// Measure real pool acquisitions and live-state queries through the shared
// production H1/H3 admission owner, without resident/announce background work.
func TestConnectAuthenticationUsesOneLiveDatabaseAcquisition(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		claims := newConnectAuthenticationWorkClaims(ctx)
		for _, source := range []struct {
			caller string
			value  jwt.StateQuerySource
		}{{"connect_h1", jwt.StateQueryConnectH1}, {"connect_h3", jwt.StateQueryConnectH3}} {
			const count = 8
			poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
			acquiredBefore := connectObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
			validBefore := connectStateQueryMetric(t, source.caller, "handshake", "client", "state_valid")
			type result struct {
				id     server.Id
				want   server.Id
				status int
				err    error
			}
			results := make(chan result, count)
			var workers sync.WaitGroup
			for range count {
				workers.Go(func() {
					got := result{want: server.NewId()}
					server.HandleError(func() {
						got.id, got.status, got.err = connectClientAuthentication(
							jwt.WithStateQuerySource(ctx, source.value), claims, got.want.Bytes())
					}, func(err error) { got.err = err })
					results <- got
				})
			}
			workers.Wait()
			acquired := connectObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) - acquiredBefore
			validated := connectStateQueryMetric(t, source.caller, "handshake", "client", "state_valid") - validBefore
			for range count {
				got := <-results
				if got.err != nil || got.status != 0 || got.id != got.want {
					t.Fatal("live handshake lost its independent instance", source.caller, got.status, got.err)
				}
			}
			if acquired != count || validated != count {
				t.Errorf("%s handshake work: acquisitions=%.0f live_validations=%.0f, want %d each",
					source.caller, acquired, validated, count)
			}
		}
	})
}

// Commit a real authority change at the last-read boundary, after the former
// early state check. A weaker membership-only read must not admit that token.
func exerciseConnectAuthenticationLatestState(t testing.TB, source jwt.StateQuerySource) {
	t.Helper()
	for _, change := range []string{"inactive", "credential_rotation", "device", "administrator", "deleted"} {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			claims := newConnectAuthenticationWorkClaims(ctx)
			other := newConnectAuthenticationWorkClaims(ctx)
			arrived := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			queryCtx := context.WithValue(jwt.WithStateQuerySource(ctx, source), connectAuthFinalReadTestKey{}, func() {
				close(arrived)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			type result struct {
				status int
				err    error
			}
			finished := make(chan result, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				got := result{}
				server.HandleError(func() {
					_, got.status, got.err = connectClientAuthentication(queryCtx, claims, server.NewId().Bytes())
				}, func(err error) { got.err = err })
				finished <- got
			}()
			defer func() { cancel(); unblock(); <-joined }()
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatal("handshake did not reach its last-read boundary", change)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				switch change {
				case "inactive":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, *claims.ClientId))
				case "credential_rotation":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, claims.UserId, claims.CreateTime.Add(time.Minute)))
				case "device":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET device_id=$2 WHERE client_id=$1`, *claims.ClientId, *other.DeviceId))
				case "administrator":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET admin_user_id=$2 WHERE network_id=$1`, claims.NetworkId, other.UserId))
				case "deleted":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=$1`, *claims.ClientId))
				}
			})
			unblock()
			select {
			case got := <-finished:
				if got.status != http.StatusUnauthorized && got.status != http.StatusForbidden || got.err == nil {
					t.Errorf("%s committed before final read but handshake status=%d err=%v", change, got.status, got.err)
				}
			case <-ctx.Done():
				t.Fatal("handshake did not finish after authority change", change)
			}
		}()
	}
}

func TestConnectH1AuthenticationUsesLatestLiveState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseConnectAuthenticationLatestState(t, jwt.StateQueryConnectH1) })
}

func TestConnectH3AuthenticationUsesLatestLiveState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseConnectAuthenticationLatestState(t, jwt.StateQueryConnectH3) })
}

func TestConnectAuthenticationKeepsCredentialErrorBeforeMalformedInstance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		claims := newConnectAuthenticationWorkClaims(ctx)
		if _, status, err := connectClientAuthentication(ctx, claims, []byte{1}); status != http.StatusBadRequest || err == nil {
			t.Fatal("valid credential did not retain its malformed-instance response", status, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, *claims.ClientId))
		})
		if _, status, err := connectClientAuthentication(ctx, claims, []byte{1}); status != http.StatusUnauthorized || err == nil {
			t.Fatal("malformed instance hid the credential rejection", status, err)
		}
	})
}
