package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func TestMaintainNetworkSessionsSchedulesEveryShardOnce(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx := t.Context()
		worker := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer worker.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleMaintainNetworkSessions(worker, tx)
			ScheduleMaintainNetworkSessions(worker, tx)
		})
		seen := map[int]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT args_json,run_once_key,run_at FROM pending_task WHERE function_name=$1`, task.NewTaskTarget(MaintainNetworkSessions).TargetFunctionName())
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var raw, key string
					var runAt time.Time
					server.Raise(rows.Scan(&raw, &key, &runAt))
					var args MaintainNetworkSessionsArgs
					server.Raise(json.Unmarshal([]byte(raw), &args))
					if seen[args.Shard] || args.Shard < 0 || args.Shard >= session.SessionIndexShards || key != task.RunOnce(fmt.Sprintf("maintain_network_sessions_%02d", args.Shard)).String() || runAt.Before(before.Add(29*time.Second)) || runAt.After(before.Add(35*time.Second)) {
						t.Fatal("invalid or duplicated shard schedule", args, key, runAt)
					}
					seen[args.Shard] = true
				}
			})
		})
		if len(seen) != session.SessionIndexShards {
			t.Fatal("maintenance omitted network shards", len(seen))
		}
	})
}

func TestMaintainNetworkSessionsWorkerPrunesOnlyExpiredNetworkSessions(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network, foreign := server.NewId(), server.NewId()
		// Use separate nonzero shards so this invocation tests expiry itself;
		// outbox repair is run explicitly before invoking the actual worker.
		network[15] = 1
		foreign[15] = 2
		makeCredential := func(id server.Id, expired bool) *session.ByJwt {
			credential := session.NewByJwt(id, server.NewId(), "worker-session", false, false)
			sid := server.NewId()
			credential.SessionId = &sid
			if expired {
				credential.ExpiresAt = gojwt.NewNumericDate(now.Add(-session.SessionGrace - 5*time.Minute))
			}
			return credential
		}
		expired := makeCredential(network, true)
		live := makeCredential(network, false)
		other := makeCredential(foreign, true)
		for _, credential := range []*session.ByJwt{expired, live, other} {
			registrationTime := now
			if credential != live {
				registrationTime = now.Add(-10 * time.Minute)
			}
			if _, err := session.RegisterNetworkSession(ctx, credential, "password", nil, false, registrationTime); err != nil {
				t.Fatal(err)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.QueueSessionIndexInTx(ctx, tx, network, expired.AcceptUntil()))
			server.Raise(session.QueueSessionIndexInTx(ctx, tx, foreign, other.AcceptUntil()))
		})
		if err := session.RepairSessionIndexes(ctx, 64); err != nil {
			t.Fatal(err)
		}
		// Repair legitimately prunes while discovering the earliest deadline.
		// Reinsert the idle membership after publication to isolate the worker.
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			if err := r.ZAdd(ctx, session.SessionKey(network, "z"), redis.Z{Score: float64(expired.AcceptUntil().UnixMilli()), Member: expired.SessionId.String()}).Err(); err != nil {
				return err
			}
			return r.ZAdd(ctx, session.SessionKey(foreign, "z"), redis.Z{Score: float64(other.AcceptUntil().UnixMilli()), Member: other.SessionId.String()}).Err()
		}))
		worker := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer worker.Cancel()
		result, err := MaintainNetworkSessions(&MaintainNetworkSessionsArgs{Shard: 1}, worker)
		if err != nil || result == nil || result.Reviewed != 1 {
			t.Fatal("worker did not review due network", result, err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			members, err := r.ZRange(ctx, session.SessionKey(network, "z"), 0, -1).Result()
			if err != nil {
				return err
			}
			if len(members) != 1 || members[0] != live.SessionId.String() || r.ZCard(ctx, session.SessionKey(foreign, "z")).Val() != 1 {
				t.Fatal("worker removed wrong sessions or changed another shard", members)
			}
			return nil
		}))
	})
}

func TestMaintainNetworkSessionsBlockedStageDoesNotStarveOtherOwners(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var order []string
	receiptFailure := errors.New("receipt cleanup unavailable")
	recovered := false
	result, err := runNetworkSessionMaintenance(ctx,
		func(ctx context.Context) (int, error) {
			order = append(order, "expiry")
			return 1, nil
		},
		func(ctx context.Context) error {
			order = append(order, "receipts")
			return receiptFailure
		},
		func(ctx context.Context) error {
			order = append(order, "repair")
			// Wait on the actual per-stage deadline, without a timing-dependent
			// sleep. A stalled Redis repair must release the recovery budget.
			<-ctx.Done()
			return ctx.Err()
		},
		func(ctx context.Context) error {
			order = append(order, "recovery")
			if ctx.Err() != nil {
				t.Fatal("repair consumed operation recovery's context")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 500*time.Millisecond {
				t.Fatal("recovery did not receive an independent bounded budget")
			}
			recovered = true
			return nil
		},
	)
	if result == nil || result.Reviewed != 1 || !recovered || fmt.Sprint(order) != "[expiry receipts repair recovery]" || !errors.Is(err, receiptFailure) || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("one failed stage starved expiry/recovery or hid failure", result, order, err)
	}
}
