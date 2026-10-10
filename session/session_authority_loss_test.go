package session

import (
	"context"
	"errors"
	"github.com/urnetwork/server"
	"testing"
)

func TestSessionJournalCannotInferAuthorityAfterRedisLoss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		target := sessionFixtureCredential(actor.ByJwt.NetworkId, server.NowUtc())
		if _, err := RegisterNetworkSession(t.Context(), target, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		operation, err := RevokeNetworkSession(&RevokeSessionArgs{SessionId: *target.SessionId, OperationId: server.NewId()}, actor)
		if err != nil {
			t.Fatal(err)
		}
		server.Raise(server.RedisAuth(t.Context(), func(ctx context.Context, r server.RedisClient) error {
			return r.Del(ctx, SessionKey(actor.ByJwt.NetworkId, "op:"+operation.OperationId.String())).Err()
		}))
		if _, err := GetSessionOperation(actor, operation.OperationId); err != nil {
			t.Fatal("retained marker still proves enforcement", err)
		}
		server.Raise(server.RedisAuth(t.Context(), func(ctx context.Context, r server.RedisClient) error {
			return r.Del(ctx, SessionMarkerKey(actor.ByJwt.NetworkId, *target.SessionId)).Err()
		}))
		if _, err := GetSessionOperation(actor, operation.OperationId); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatal("SQL result manufactured success after authority loss", err)
		}
		if _, err := EnforceSessionOperation(t.Context(), actor.ByJwt.NetworkId, operation.OperationId); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatal("recovery silently claimed missing cutoff", err)
		}
	})
}
