package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestArinNativeRPCLeaseReleaseRetainsObserverForNextCapture(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		handler, closeOwner, err := NewArinShadowNativeRPC(ctx, 4)
		if err != nil {
			t.Fatal(err)
		}
		defer closeOwner()
		capture := currentArinShadowScoreCapture.Load()
		attempt := beginArinShadowScoreCapture()
		id := server.NewId()
		row := shadowScoreTestRow(id)
		country := "us"
		passing := true
		attempt.observe(row, &country, &providerEgressFacts{egressQuality: &passing}, true)
		row.PassesMinimums = map[string]bool{RankModeQuality: true, RankModeSpeed: true}
		row.Online = true
		target := map[server.Id]map[server.Id]*ClientScore{server.NewId(): {id: row}}
		now := server.NowUtc()
		census := newClientScoreNativeCensus(now.Add(-time.Second), now, now, map[server.Id]ProviderEgressHealthCounts{}, target)
		server.Raise(writeClientScoreNativeCensus(ctx, census, time.Minute))
		attempt.publish(census, target)
		for _, method := range []string{"native_end", "native_release", "native_end"} {
			value, err := handler(ctx, "native_acquire", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal("next capture observer lost", err)
			}
			lease := value.(ArinShadowNativeLeaseInfo)
			if _, err = handler(ctx, "native_acquire", json.RawMessage(`{}`)); err == nil {
				t.Fatal("competing lease admitted")
			}
			request, _ := json.Marshal(struct {
				Token server.Id `json:"token"`
			}{lease.Token})
			if _, err = handler(ctx, method, request); err != nil {
				t.Fatal("lease end failed", err)
			}
			if currentArinShadowScoreCapture.Load() != capture || capture.Snapshot() == nil {
				t.Fatal("capture end uninstalled observer")
			}
		}
		closeOwner()
		if currentArinShadowScoreCapture.Load() == capture || capture.Snapshot() != nil {
			t.Fatal("final owner close retained observer")
		}
	})
}
