// Hosted control authenticates child ownership and revocable state with one
// borrow. Real pool barriers preserve deadline and refusal ownership.
package localclient

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"google.golang.org/protobuf/proto"
)

// Owns one synthetic ping's pooled bytes until the test has joined its calls.
func authorityControlPingArgs(t testing.TB) *connect.ConnectControlArgs {
	t.Helper()
	frame, err := connect.ToFrame(&protocol.ControlPing{}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connect.MessagePoolReturn(frame.MessageBytes) })
	pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
	if err != nil {
		t.Fatal(err)
	}
	return &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)}
}

// Every complete child credential reaches one durable snapshot. Local token
// refusals still precede acquisition, and no successful result caches authority.
func TestAuthorityControlOwnershipUsesOneLiveStateRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, parent := authorityMintTestOwner(t, ctx)
		_, child := authorityTestMint(t, ctx, owner)
		otherDevice, otherClient := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, parent.NetworkId, otherDevice, otherClient, "other", "other")
		args := authorityControlPingArgs(t)
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		for _, control := range []struct {
			name   string
			change func(*session.ByJwt)
			active bool
			owned  bool
			rotate bool
			local  bool
			valid  bool
		}{
			{name: "owned", active: true, owned: true, valid: true},
			{name: "foreign parent", active: true},
			{name: "owned after foreign", active: true, owned: true, valid: true},
			{name: "retired", owned: true},
			{name: "rotated", active: true, owned: true, rotate: true},
			{name: "child device mismatch", active: true, owned: true, change: func(claims *session.ByJwt) { claims.DeviceId = &otherDevice }},
			{name: "missing child device", active: true, owned: true, local: true, change: func(claims *session.ByJwt) { claims.DeviceId = nil }},
			{name: "foreign account", active: true, owned: true, local: true, change: func(claims *session.ByJwt) { claims.UserId = server.NewId(); claims.Subject = claims.UserId.String() }},
			{name: "foreign network", active: true, owned: true, local: true, change: func(claims *session.ByJwt) { claims.NetworkId = server.NewId() }},
			{name: "wrong audience", active: true, owned: true, local: true, change: func(claims *session.ByJwt) { claims.Audience = []string{session.ByJwtAudienceConnect} }},
			{name: "restored", active: true, owned: true, valid: true},
		} {
			source := otherClient
			if control.owned {
				source = owner.clientId
			}
			changed := time.Unix(0, 0).UTC()
			if control.rotate {
				changed = child.CreateTime.Add(time.Minute)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id=$2, active=$3 WHERE client_id=$1`, *child.ClientId, source, control.active))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, child.UserId, changed))
			})
			claims := *child
			if control.change != nil {
				control.change(&claims)
			}
			token := claims.Testing_Sign()
			before := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
			queryBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
			result, err := owner.ConnectControl(ctx, token, args)
			if control.valid {
				if err != nil || result == nil || result.Error != nil {
					t.Fatalf("%s: owned current credential failed", control.name)
				}
			} else {
				authorityTestUnauthorized(t, err)
				if result != nil {
					t.Fatalf("%s: refused credential reached control", control.name)
				}
			}
			want := 1.0
			if control.local {
				want = 0
			}
			if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != before+want ||
				authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queryBefore+want {
				t.Fatalf("%s: control did not use exactly its one combined durable read", control.name)
			}
		}
	})
}

// The real sole-slot holder fixes the causal ordering. A request that enters
// live with a subsecond budget expires before SQL; a later healthy request
// reuses the released pool. Already-expired entry never attempts acquisition.
func TestAuthorityControlHeldPoolRetainsEntryDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		owner, _ := authorityMintTestOwner(t, ctx)
		child, _ := authorityTestMint(t, ctx, owner)
		args := authorityControlPingArgs(t)
		ready, release := make(chan struct{}), make(chan struct{})
		joined := make(chan error, 1)
		var releaseOnce sync.Once
		go func() {
			joined <- server.HandleError1(func() error {
				server.Db(ctx, func(server.PgConn) {
					close(ready)
					select {
					case <-release:
					case <-ctx.Done():
					}
				})
				return nil
			}, func(err error) error { return err })
		}()
		join := func() {
			releaseOnce.Do(func() {
				close(release)
				if err := <-joined; err != nil {
					t.Error("synthetic pool holder failed")
				}
			})
		}
		defer join()
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("synthetic pool holder did not acquire")
		}
		if authorityMintPoolGauge(t, "maximum") != 1 || authorityMintPoolGauge(t, "acquired") != 1 {
			t.Fatal("sole connection was not held")
		}
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		canceledLabels := map[string]string{"pool": "default", "outcome": "canceled"}
		before := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		canceledBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", canceledLabels)
		queryBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		expired, stopExpired := context.WithDeadline(ctx, time.Unix(1, 0))
		defer stopExpired()
		if result, err := owner.ConnectControl(expired, child.ByClientJwt, args); result != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("expired entry lost its deadline refusal")
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", canceledLabels) != canceledBefore {
			t.Fatal("already-expired local preflight attempted acquisition")
		}
		requestCtx, requestCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer requestCancel()
		if requestCtx.Err() != nil {
			t.Fatal("short-budget request did not start live")
		}
		result, err := owner.ConnectControl(requestCtx, child.ByClientJwt, args)
		if result != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			t.Fatal("held-pool request did not retain its original deadline")
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != before ||
			authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", canceledLabels) != canceledBefore+1 ||
			authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queryBefore ||
			authorityMintPoolGauge(t, "acquired") != 1 {
			t.Fatal("held-pool deadline crossed into SQL or lost its acquisition boundary")
		}
		join()
		result, err = owner.ConnectControl(ctx, child.ByClientJwt, args)
		if err != nil || result == nil || result.Error != nil ||
			authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != before+1 ||
			authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queryBefore+1 {
			t.Fatal("released holder did not restore one-borrow control")
		}
	})
}
