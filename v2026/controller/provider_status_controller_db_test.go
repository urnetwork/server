package controller

// GET /network/provider-status against the database: what it caches and what
// it reads fresh.

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The admission and ranking are cached per network while the appearances are
// read fresh on every call, and the caller's own provider comes first. Run
// with `cd server/controller && ../test.sh -run GetProviderStatus`.
func TestGetProviderStatusCachesRankingAndReadsAppearancesFresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "provider-status-"+networkId.String(), userId)
		keys := map[model.ProvideMode][]byte{
			model.ProvideModePublic:  bytes.Repeat([]byte{1}, 32),
			model.ProvideModeNetwork: bytes.Repeat([]byte{2}, 32),
		}
		clientIds := []server.Id{}
		deviceIds := []server.Id{}
		for range 3 {
			deviceId := server.NewId()
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "provider", "test")
			model.SetProvide(ctx, clientId, keys)
			deviceIds = append(deviceIds, deviceId)
			clientIds = append(clientIds, clientId)
		}
		// the caller is the last client by creation, not by id
		callerClientId := clientIds[2]
		byJwt := jwt.NewByJwt(networkId, userId, "provider-status", false, false).Client(deviceIds[2], callerClientId)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		first, err := GetProviderStatus(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Providers) != 3 || first.Providers[0].ClientId != callerClientId || first.Truncated {
			t.Fatalf("first = %+v", first)
		}
		for _, status := range first.Providers {
			if status.Reason != model.ProviderStatusReasonNotConnected || status.Appearances == nil {
				t.Fatalf("status = %+v", status)
			}
			for _, count := range status.Appearances.AppearancesPerMinute {
				if count != 0 {
					t.Fatal("a provider never offered read a count")
				}
			}
		}

		appearances := model.NewProviderAppearances(ctx, model.DefaultProviderAppearanceSettings())
		appearances.Record([]server.Id{callerClientId, callerClientId}, server.NowUtc())
		appearances.Close()
		// a change the cached ranking must not see within its ttl
		model.SetProvide(ctx, callerClientId, map[model.ProvideMode][]byte{})

		second, err := GetProviderStatus(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Providers) != 3 || second.Providers[0].ClientId != callerClientId {
			t.Fatalf("second = %+v", second)
		}
		own := second.Providers[0]
		if !own.EvaluateTime.Equal(first.Providers[0].EvaluateTime) || own.Reason != model.ProviderStatusReasonNotConnected {
			t.Fatalf("the ranking was not reused: %s %s", own.EvaluateTime, own.Reason)
		}
		total := int64(0)
		for _, count := range own.Appearances.AppearancesPerMinute {
			total += count
		}
		if total != 2 {
			t.Fatalf("%d appearances read, want the 2 just written", total)
		}
	})
}
