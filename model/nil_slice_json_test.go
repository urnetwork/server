package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A nil slice marshals as JSON `null`. The gomobile sdk binds these fields as
// pointers, so `null` becomes a nil object the android client dereferences
// inside a jni callback; the NPE cannot cross jni and ART aborts the process.
// An API slice that can legitimately be empty must serialise as `[]`.
//
// These assert on the marshalled bytes, because `[]` versus `null` is the
// property that actually reaches the client.

func TestGetLeaderboardEmptyMarshalsAsArrayNotNull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a fresh db has no payouts, so the query returns zero rows -- the
		// exact condition that crashed the android client
		earners, err := GetLeaderboard(ctx)
		if err != nil {
			t.Fatalf("GetLeaderboard: %s", err)
		}
		if earners == nil {
			t.Fatal("GetLeaderboard returned a nil slice; it marshals as null and crashes the client")
		}

		b, err := json.Marshal(LeaderboardResult{Earners: earners})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"earners":[]`) {
			t.Errorf("marshalled as %s, want \"earners\":[]", b)
		}
	})
}

func TestFetchAccountPointsEmptyMarshalsAsArrayNotNull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a network with no points at all -- feeds both `network_points` and
		// its `account_points` alias
		points := FetchAccountPoints(ctx, server.NewId())
		if points == nil {
			t.Fatal("FetchAccountPoints returned a nil slice; it marshals as null and crashes the client")
		}

		b, err := json.Marshal(map[string]any{"network_points": points})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "null") {
			t.Errorf("marshalled as %s, want an empty array", b)
		}
	})
}
