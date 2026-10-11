package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
)

type committedIdentityCacheContextKey struct{}

type committedIdentityCacheCancelHook struct {
	enabled   atomic.Bool
	fired     atomic.Bool
	cancel    context.CancelFunc
	source    server.Id
	committed atomic.Int64
}

func (h *committedIdentityCacheCancelHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}
func (h *committedIdentityCacheCancelHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *committedIdentityCacheCancelHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if h.enabled.Load() && ctx.Value(committedIdentityCacheContextKey{}) == h && command.Name() == "ping" && !h.fired.Load() {
			// This separate primary connection observes only committed rows.
			// Redis calls before the transaction are allowed through; the first
			// cache acquisition after the child becomes durable cancels the caller.
			checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var committed int64
			server.Db(checkCtx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(checkCtx, `SELECT count(*) FROM network_client WHERE source_client_id=$1`, h.source).Scan(&committed))
			})
			if committed > 0 {
				h.committed.Store(committed)
				h.fired.Store(true)
				h.cancel()
				return ctx.Err()
			}
		}
		return next(ctx, command)
	}
}

func TestAuthClientCommittedIdentitySurvivesCacheCancellation(t *testing.T) {
	for _, scenario := range []string{"healthy", "before_transaction", "after_commit_before_cache"} {
		t.Run(scenario, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(tb testing.TB) {
				rootCtx := context.Background()
				network, user, parentClient, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
				Testing_CreateNetwork(rootCtx, network, "synthetic-cache-boundary", user)
				Testing_CreateDevice(rootCtx, network, device, parentClient, "synthetic-parent", "synthetic")
				claims := session.NewByJwt(network, user, "synthetic-cache-boundary", false, false).Client(device, parentClient)
				callCtx, cancel := context.WithCancel(rootCtx)
				defer cancel()
				hook := &committedIdentityCacheCancelHook{cancel: cancel, source: parentClient}
				hook.enabled.Store(scenario == "after_commit_before_cache")
				defer hook.enabled.Store(false)
				callCtx = context.WithValue(callCtx, committedIdentityCacheContextKey{}, hook)
				server.Redis(rootCtx, func(client server.RedisClient) { client.AddHook(hook) })
				if scenario == "before_transaction" {
					cancel()
				}
				clientSession := session.NewLocalClientSession(callCtx, "0.0.0.0:0", claims)
				defer clientSession.Cancel()
				result, err := server.HandleError2(func() (*AuthNetworkClientResult, error) {
					return AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: &parentClient}, clientSession)
				}, func(err error) (*AuthNetworkClientResult, error) { return nil, err })
				hook.enabled.Store(false)
				var committed, active int64
				server.Db(rootCtx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(rootCtx, `SELECT count(*),count(*) FILTER (WHERE active) FROM network_client WHERE source_client_id=$1`, parentClient).Scan(&committed, &active))
				})
				if scenario == "before_transaction" {
					if err == nil || result != nil || committed != 0 || hook.fired.Load() {
						tb.Fatal("pre-commit cancellation created or returned an identity")
					}
					return
				}
				if committed != 1 || active != 1 {
					tb.Fatalf("durable mint control: committed=%d active=%d", committed, active)
				}
				if scenario == "after_commit_before_cache" {
					if !hook.fired.Load() || hook.committed.Load() != 1 || !errors.Is(callCtx.Err(), context.Canceled) {
						tb.Fatal("cancellation failed to cross the committed-child/cache boundary")
					}
				} else if hook.fired.Load() || callCtx.Err() != nil {
					tb.Fatal("healthy control was canceled")
				}
				if err != nil || result == nil || result.ClientId == nil || result.ByClientJwt == nil || *result.ByClientJwt == "" {
					tb.Errorf("post-commit result lost: committed=%d active=%d cancellation_after_commit=%t result_present=%t error_present=%t", committed, active, hook.fired.Load(), result != nil, err != nil)
					return
				}
				// A returned committed identity remains available to normal
				// generation/retirement ownership even after its caller canceled.
				retireSession := session.NewLocalClientSession(rootCtx, "0.0.0.0:0", claims)
				defer retireSession.Cancel()
				retired, retireErr := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *result.ClientId}, retireSession)
				if retireErr != nil || retired == nil || retired.Error != nil {
					tb.Fatal("returned child could not follow normal network-fenced retirement")
				}
				var retiredChildren int64
				server.Db(rootCtx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(rootCtx, `SELECT count(*) FROM network_client WHERE source_client_id=$1 AND client_id=$2 AND NOT active AND deactivate_time IS NOT NULL`, parentClient, *result.ClientId).Scan(&retiredChildren))
				})
				if retiredChildren != 1 {
					tb.Fatal("returned identity did not durably retire the committed child")
				}
			})
		})
	}
}
