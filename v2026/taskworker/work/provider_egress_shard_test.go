package work

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProviderEgressShardCleanupOnSuccessFailureCancelAndPanic(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, outcome := range []string{"success", "error", "cancel", "panic"} {
			ctx, cancel := context.WithCancel(t.Context())
			key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: 1}
			args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("probe.example"), 0)
			args.ShardCount = 1
			failure := errors.New("probe callback failure")
			var networkId server.Id
			var recovered any
			var runErr error
			func() {
				defer func() { recovered = recover() }()
				_, runErr = runWithProviderEgressShard(ctx, key, args, func(identity *model.ProberIdentity) (*ProviderEgressProbeResult, error) {
					networkId = *identity.NetworkId
					credentials, err := newProviderEgressCredentials(identity)
					if err != nil {
						t.Fatal(err)
					}
					// A partially initialized tunnel can leave a derived identity.
					// Account cleanup must own it even when callback setup fails.
					source := connect.Id(*identity.ClientId)
					if _, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source}); err != nil {
						t.Fatal(err)
					}
					server.Db(ctx, func(conn server.PgConn) {
						var clients, grants, shared int
						server.Raise(conn.QueryRow(ctx, `SELECT
							(SELECT count(*) FROM network_client WHERE network_id=$1),
							(SELECT count(*) FROM transfer_balance WHERE network_id=$1),
							(SELECT count(*) FROM prober_identity)`, networkId).Scan(&clients, &grants, &shared))
						if clients != 2 || grants != 1 || shared != 0 {
							t.Fatal("pass did not use only its private account and one grant")
						}
					})
					switch outcome {
					case "error":
						return nil, failure
					case "cancel":
						cancel()
						return nil, ctx.Err()
					case "panic":
						panic(failure)
					default:
						return &ProviderEgressProbeResult{}, nil
					}
				})
			}()
			cancel()
			if outcome == "panic" && recovered != failure || outcome != "panic" && recovered != nil ||
				outcome == "error" && !errors.Is(runErr, failure) || outcome == "cancel" && !errors.Is(runErr, context.Canceled) || outcome == "success" && runErr != nil {
				t.Fatalf("%s: callback outcome changed: error=%v panic=%v", outcome, runErr, recovered)
			}
			server.Db(t.Context(), func(conn server.PgConn) {
				var retained int
				var state string
				server.Raise(conn.QueryRow(t.Context(), `SELECT
					(SELECT count(*) FROM network WHERE network_id=$1)+
					(SELECT count(*) FROM network_client WHERE network_id=$1)+
					(SELECT count(*) FROM transfer_balance WHERE network_id=$1),state
					FROM prober_shard_run WHERE task_id=$2 AND epoch=$3`, networkId, key.TaskId, key.Epoch).Scan(&retained, &state))
				if retained != 0 || state != "closed" {
					t.Fatalf("%s: pass left disposable resources after return: rows=%d state=%s", outcome, retained, state)
				}
			})
		}
	})
}

func TestProviderEgressShardCreditIsPerPassAndIndependentOfShardCount(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("probe.example"), 0)
	args.ShardCount = 1
	one, err := providerUrlProbeShardCredit(args)
	if err != nil {
		t.Fatal(err)
	}
	args.ShardCount = 256
	many, err := providerUrlProbeShardCredit(args)
	if err != nil || many != one {
		t.Fatal("private grant changed with another shard's exposure", err)
	}
	want := model.ByteCount(connect.DefaultContractManagerSettings().StandardContractTransferByteCount) * model.ByteCount(providerEgressFullSelectedLimit+providerUrlProbeBatch(args).Concurrency) * 6 * model.ByteCount(args.TunnelRecreateAttempts+1)
	if one < want || one < model.ProberShardTransferHeadroom {
		t.Fatal("one grant cannot cover the bounded pass")
	}
}

func TestProviderEgressShardRequiresDurableTaskExecution(t *testing.T) {
	if _, err := runProviderEgressProbe(t.Context(), nil); err == nil {
		t.Fatal("unowned execution reached credential acquisition")
	}
}
