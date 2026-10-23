package model

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func newResidentMoveIntentForTest(kind ResidentMoveKind) *ResidentMoveIntent {
	return &ResidentMoveIntent{
		MoveId:         server.NewId(),
		Kind:           kind,
		FromResidentId: server.NewId(),
		FromExchangeId: server.NewId(),
		TargetHost:     "alpha-edge-1",
		Targets: []netip.Addr{
			netip.MustParseAddr("192.0.2.10"),
			netip.MustParseAddr("2001:db8::10"),
		},
	}
}

func residentMoveIntentTtlForTest(ctx context.Context, t testing.TB, clientId server.Id) time.Duration {
	t.Helper()
	var ttl time.Duration
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		ttl, err = r.PTTL(ctx, residentMoveIntentKey(clientId)).Result()
		if err != nil {
			t.Fatal(err)
		}
	})
	return ttl
}

// The intent round trips, lives for its arrival window, and is taken by
// exactly one arrival.
func TestResidentMoveIntentSetGetTake(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		intent, err := GetResidentMoveIntent(ctx, clientId)
		if err != nil || intent != nil {
			t.Fatalf("absent intent = %+v, %v", intent, err)
		}

		written := newResidentMoveIntentForTest(ResidentMoveKindAdvisory)
		if err := SetResidentMoveIntent(ctx, clientId, written, 3*time.Minute); err != nil {
			t.Fatal(err)
		}
		if ttl := residentMoveIntentTtlForTest(ctx, t, clientId); ttl <= 0 || 3*time.Minute < ttl {
			t.Fatalf("intent ttl = %s, want the arrival window", ttl)
		}
		read, err := GetResidentMoveIntent(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}
		assertResidentMoveIntentEqual(t, written, read)

		taken, err := TakeResidentMoveIntent(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}
		assertResidentMoveIntentEqual(t, written, taken)
		// the take removed it: a second arrival finds nothing
		if again, err := TakeResidentMoveIntent(ctx, clientId); err != nil || again != nil {
			t.Fatalf("second take = %+v, %v", again, err)
		}
		if read, err := GetResidentMoveIntent(ctx, clientId); err != nil || read != nil {
			t.Fatalf("taken intent still read = %+v, %v", read, err)
		}

		if err := SetResidentMoveIntent(ctx, clientId, written, 0); err == nil {
			t.Fatal("an intent without a ttl was written")
		}
	})
}

// An intent ends with its arrival window.
func TestResidentMoveIntentExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		if err := SetResidentMoveIntent(ctx, clientId, newResidentMoveIntentForTest(ResidentMoveKindDrain), 200*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		endTime := time.Now().Add(30 * time.Second)
		for {
			intent, err := GetResidentMoveIntent(ctx, clientId)
			if err != nil {
				t.Fatal(err)
			}
			if intent == nil {
				return
			}
			if endTime.Before(time.Now()) {
				t.Fatal("the intent outlived its window")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
}

// Deleting by move id removes only the intent of that move, so a newer move's
// intent survives a late report or a failed send of an older one.
func TestResidentMoveIntentDeleteKeepsNewerMove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		older := newResidentMoveIntentForTest(ResidentMoveKindCompulsory)
		if err := SetResidentMoveIntent(ctx, clientId, older, time.Minute); err != nil {
			t.Fatal(err)
		}
		if deleted, err := DeleteResidentMoveIntent(ctx, clientId, server.NewId()); err != nil || deleted {
			t.Fatalf("delete of an unknown move = %t, %v", deleted, err)
		}
		newer := newResidentMoveIntentForTest(ResidentMoveKindAdvisory)
		if err := SetResidentMoveIntent(ctx, clientId, newer, time.Minute); err != nil {
			t.Fatal(err)
		}
		if deleted, err := DeleteResidentMoveIntent(ctx, clientId, older.MoveId); err != nil || deleted {
			t.Fatalf("delete of the replaced move = %t, %v", deleted, err)
		}
		read, err := GetResidentMoveIntent(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}
		assertResidentMoveIntentEqual(t, newer, read)
		if deleted, err := DeleteResidentMoveIntent(ctx, clientId, newer.MoveId); err != nil || !deleted {
			t.Fatalf("delete of the current move = %t, %v", deleted, err)
		}
		if read, err := GetResidentMoveIntent(ctx, clientId); err != nil || read != nil {
			t.Fatalf("deleted intent still read = %+v, %v", read, err)
		}
		// nothing left to delete
		if deleted, err := DeleteResidentMoveIntent(ctx, clientId, newer.MoveId); err != nil || deleted {
			t.Fatalf("delete of an absent intent = %t, %v", deleted, err)
		}
	})
}

func TestResidentMoveIntentTargetByFamily(t *testing.T) {
	intent := newResidentMoveIntentForTest(ResidentMoveKindAdvisory)
	if target, ok := intent.Target(4); !ok || target != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("family 4 target = %s, %t", target, ok)
	}
	if target, ok := intent.Target(6); !ok || target != netip.MustParseAddr("2001:db8::10") {
		t.Fatalf("family 6 target = %s, %t", target, ok)
	}
	// no one family: ambiguous with two targets
	if target, ok := intent.Target(0); ok {
		t.Fatalf("family 0 target of two = %s", target)
	}
	if _, ok := intent.Target(5); ok {
		t.Fatal("an unknown family picked a target")
	}

	single := &ResidentMoveIntent{Targets: []netip.Addr{netip.MustParseAddr("2001:db8::20")}}
	if target, ok := single.Target(0); !ok || target != netip.MustParseAddr("2001:db8::20") {
		t.Fatalf("family 0 target of one = %s, %t", target, ok)
	}
	if _, ok := single.Target(4); ok {
		t.Fatal("family 4 picked a v6 target")
	}
}

func assertResidentMoveIntentEqual(t testing.TB, want *ResidentMoveIntent, got *ResidentMoveIntent) {
	t.Helper()
	if got == nil {
		t.Fatalf("intent = nil, want %+v", want)
	}
	if got.MoveId != want.MoveId || got.Kind != want.Kind || got.FromResidentId != want.FromResidentId ||
		got.FromExchangeId != want.FromExchangeId || got.TargetHost != want.TargetHost || len(got.Targets) != len(want.Targets) {
		t.Fatalf("intent = %+v, want %+v", got, want)
	}
	for i := range want.Targets {
		if got.Targets[i] != want.Targets[i] {
			t.Fatalf("intent targets = %v, want %v", got.Targets, want.Targets)
		}
	}
}
