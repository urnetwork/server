package session

import (
	"context"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/server/v2026"
	"testing"
	"time"
)

func TestSessionMaintenanceExpiryRepairAndPublicationRace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network, other := server.NewId(), server.NewId()
		early := sessionFixtureCredential(network, now)
		late := sessionFixtureCredential(network, now)
		foreign := sessionFixtureCredential(other, now)
		early.ExpiresAt = gojwt.NewNumericDate(now.Add(time.Hour - SessionGrace - clockLeeway))
		late.ExpiresAt = gojwt.NewNumericDate(now.Add(3*time.Hour - SessionGrace - clockLeeway))
		foreign.ExpiresAt = early.ExpiresAt
		for _, credential := range []*ByJwt{early, late, foreign} {
			if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		// The earlier network index disappeared before a later mint queued repair.
		if err := sessionTx(ctx, func(tx server.PgTx) error { return QueueSessionIndexInTx(ctx, tx, network, late.AcceptUntil()) }); err != nil {
			t.Fatal(err)
		}
		if err := RepairSessionIndexes(ctx, 1); err != nil {
			t.Fatal(err)
		}
		keys := sessionIndexKeys(network)
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			due, err := r.ZScore(ctx, keys[0], network.String()).Result()
			if err != nil {
				return err
			}
			if int64(due) != early.AcceptUntil().UnixMilli() {
				t.Fatal("repair missed earlier live deadline", due)
			}
			return nil
		}))
		// Invoke the worker's review directly at an explicit future time: no list or
		// mint is allowed to hide a missing maintenance prune.
		reviewed, err := SweepSessionIndexShard(ctx, int(network[15])%SessionIndexShards, 32, early.AcceptUntil())
		if err != nil || reviewed != 1 {
			t.Fatal(reviewed, err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			members, err := r.ZRange(ctx, SessionKey(network, "z"), 0, -1).Result()
			if err != nil {
				return err
			}
			if len(members) != 1 || members[0] != late.SessionId.String() {
				t.Fatal("worker did not remove exactly expired session", members)
			}
			if count := r.ZCard(ctx, SessionKey(other, "z")).Val(); count != 1 {
				t.Fatal("worker changed another network")
			}
			return nil
		}))
		if _, err = SweepSessionIndexShard(ctx, int(network[15])%SessionIndexShards, 32, early.AcceptUntil()); err != nil {
			t.Fatal(err)
		}
		// Publication refreshes the revision even when the score remains unchanged.
		oldRevision := "old"
		newRevision := "new"
		if err = publishSessionIndex(ctx, network, now, oldRevision); err != nil {
			t.Fatal(err)
		}
		if err = publishSessionIndex(ctx, network, now.Add(-time.Second), newRevision); err != nil {
			t.Fatal(err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			changed, err := r.Eval(ctx, sessionIndexCasLua, keys, network.String(), oldRevision, "").Int()
			if err != nil {
				return err
			}
			if changed != 0 || r.ZCard(ctx, keys[0]).Val() == 0 {
				t.Fatal("stale sweep erased a concurrent earlier publication")
			}
			return nil
		}))
		// Revocation removes live membership in the same authority script, without
		// waiting for this worker, SQL cleanup, or a listing read.
		if _, err = enforceSessionRevoke(ctx, network, server.NewId(), "single", late.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			if r.ZCard(ctx, SessionKey(network, "z")).Val() != 0 || r.Exists(ctx, SessionMarkerKey(network, *late.SessionId)).Val() != 1 {
				t.Fatal("revoke did not atomically retire membership and retain marker")
			}
			return nil
		}))
	})
}
func TestSessionEmptyInventoryRevisionExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := server.NewId()
		inv, err := readSessionInventory(ctx, network, server.NowUtc())
		if err != nil || inv.Generation == "" || inv.EventId < 1 {
			t.Fatal(inv, err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			for _, suffix := range []string{"generation", "eid"} {
				if ttl := r.PTTL(ctx, SessionKey(network, suffix)).Val(); ttl <= 0 || ttl > 25*time.Hour {
					t.Fatal("empty inventory leaked revision", ttl)
				}
			}
			return nil
		}))
	})
}
func TestAuthorizationLeaseDeadlineAndRetiredGeneration(t *testing.T) {
	now := time.Unix(1000, 0)
	state := authorizationLeaseState{deadline: now.Add(90 * time.Second), generation: 7}
	// Renew from check start, never from its delayed completion.
	if !state.renew(now.Add(60*time.Second), now.Add(62*time.Second), time.Time{}, 7) || !state.deadline.Equal(now.Add(150*time.Second)) {
		t.Fatal(state)
	}
	if state.expire(now.Add(149*time.Second)) || !state.expire(now.Add(150*time.Second)) {
		t.Fatal("lease timer boundary")
	}
	if state.renew(now.Add(145*time.Second), now.Add(151*time.Second), time.Time{}, 7) {
		t.Fatal("late authoritative result resurrected retired generation")
	}
	state = authorizationLeaseState{deadline: now.Add(90 * time.Second), generation: 8}
	if state.renew(now.Add(30*time.Second), now.Add(31*time.Second), time.Time{}, 7) {
		t.Fatal("old generation renewed new connection")
	}
	if !state.renew(now.Add(30*time.Second), now.Add(31*time.Second), now.Add(50*time.Second), 8) || !state.deadline.Equal(now.Add(50*time.Second)) {
		t.Fatal("lease exceeded credential horizon")
	}
}
