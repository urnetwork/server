package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// S2 N2: the revoke script's eid event reached connect before the record
// tombstones, so a lease recheck the event started could read the session's
// positive projection and renew. The revocation event is now written after
// every shard of the network refuses the targets, so a recheck it starts
// refuses. The test observes the record from inside the event write, where
// connect's recheck would read it.
func TestSessionRevocationEventFollowsEveryRecordTombstone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		networkId := actor.ByJwt.NetworkId
		target := NewByJwt(networkId, actor.ByJwt.UserId, "revocation-event", false, false)
		sid := server.NewId()
		target.SessionId = &sid
		if _, err := RegisterNetworkSession(ctx, target, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		// a recheck before the revocation projects the session as valid
		if err := ValidateByJwtState(WithStateQuerySource(ctx, StateQueryConnectH1), target, false); err != nil {
			t.Fatal(err)
		}
		if value, ok := testingRevocationField(ctx, t, revocationRecordKey(networkId, sid), revocationSessionField(sid)); !ok || value != revocationActive {
			t.Fatalf("the valid session was not projected: %q %v", value, ok)
		}
		tombstoned := func() int {
			count := 0
			for shard := range revocationRecordShardCount {
				if value, ok := testingRevocationField(ctx, t, revocationRecordShardKey(networkId, shard), revocationSessionField(sid)); ok && value == revocationTombstone {
					count++
				}
			}
			return count
		}

		type observation struct {
			tombstones int
			recheck    error
		}
		observed := []observation{}
		write := writeSessionRevocationEvent
		writeSessionRevocationEvent = func(ctx context.Context, eventNetworkId server.Id) error {
			if eventNetworkId == networkId {
				observed = append(observed, observation{
					tombstones: tombstoned(),
					recheck:    ValidateByJwtState(WithStateQuerySource(ctx, StateQueryConnectH1), target, false),
				})
			}
			return write(ctx, eventNetworkId)
		}
		defer func() { writeSessionRevocationEvent = write }()

		op := server.NewId()
		if err := prepareSessionOperation(actor, op, "single", &sid, nil); err != nil {
			t.Fatal(err)
		}
		result, err := EnforceSessionOperation(ctx, networkId, op)
		if err != nil || result.State != "enforced" {
			t.Fatal("the revocation was not enforced", result, err)
		}
		if len(observed) != 1 {
			t.Fatalf("the revocation wrote %d revocation events, want 1", len(observed))
		}
		if observed[0].tombstones != revocationRecordShardCount {
			t.Fatalf("the revocation event came when %d of %d shards refused the session", observed[0].tombstones, revocationRecordShardCount)
		}
		if !errors.Is(observed[0].recheck, ErrSessionRevoked) {
			t.Fatalf("a recheck the revocation event starts answered %v", observed[0].recheck)
		}

		// the event is the key connect subscribes to, and it expires
		var count int64
		var ttl time.Duration
		if err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			if count, err = r.Get(ctx, SessionRevocationEventKey(networkId)).Int64(); err != nil {
				return err
			}
			ttl, err = r.PTTL(ctx, SessionRevocationEventKey(networkId)).Result()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if count != 1 || ttl <= 0 || sessionRevocationEventTtl < ttl {
			t.Fatalf("the revocation event key holds %d with ttl %s", count, ttl)
		}
	})
}

// Creating, extending and expiring sessions changes the inventory revision
// for the Sessions screen; it writes no revocation event, so it rechecks no
// lease (S1 W8).
func TestSessionMintWritesNoRevocationEvent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		networkId := actor.ByJwt.NetworkId
		events := 0
		write := writeSessionRevocationEvent
		writeSessionRevocationEvent = func(ctx context.Context, eventNetworkId server.Id) error {
			events++
			return write(ctx, eventNetworkId)
		}
		defer func() { writeSessionRevocationEvent = write }()
		for range 3 {
			credential := NewByJwt(networkId, actor.ByJwt.UserId, "revocation-event", false, false)
			sid := server.NewId()
			credential.SessionId = &sid
			if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, server.NowUtc()); err != nil {
				t.Fatal(err)
			}
		}
		if events != 0 {
			t.Fatalf("three new sessions wrote %d revocation events", events)
		}
		var exists int64
		if err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			exists, err = r.Exists(ctx, SessionRevocationEventKey(networkId)).Result()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if exists != 0 {
			t.Fatal("new sessions wrote the revocation event key")
		}
	})
}
