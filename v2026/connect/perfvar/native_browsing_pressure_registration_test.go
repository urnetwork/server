// The ordinary pressure root consumes this exact plan through existing
// constructor seams. Registration cannot silently select an easier workload.
package perfvar

import (
	"reflect"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Pin the synthetic access shape, real page plan, native ingress, unchanged
// window policy and endpoint-owned retry budget across every rebuilt client.
func TestNativeBrowsingPressureRegistrationPreservesFrozenWorkload(t *testing.T) {
	plan := newNativeBrowsingPressurePlan()
	if nativeBrowsingPageCount != 100 || nativeBrowsingOriginCount != 4 ||
		nativeBrowsingAssetCount != 4 || nativeBrowsingConnectionsPerOrigin != 4 ||
		plan.maxLivePages != 4 || plan.deadline != 300*time.Second {
		t.Fatal("pressure registration changed its page, origin, connection or deadline contract")
	}
	if plan.profile.Name != cellEdge5mDown1mUpName || plan.profile.Seed != 4013 ||
		plan.profile.Forward.RateBitsPerSecond != 1_000_000 || plan.profile.Reverse.RateBitsPerSecond != 5_000_000 ||
		plan.profile.InnerMtu != 1320 {
		t.Fatalf("pressure access profile changed: %+v", plan.profile)
	}
	for _, direction := range []linkProfile{plan.profile.Forward, plan.profile.Reverse} {
		if direction.BaseDelay != 60*time.Millisecond || direction.Jitter != 25*time.Millisecond ||
			direction.LossModel != lossModelIndependent || direction.LossProbability != 0.005 ||
			direction.OuterMtu != 1400 || direction.Blackhole || direction.BurstLoss != nil ||
			direction.QueueByteCount != bandwidthDelayQueue(direction.RateBitsPerSecond, 100*time.Millisecond) {
			t.Fatalf("pressure link shape changed: %+v", direction)
		}
	}
	if !reflect.DeepEqual(plan.provider, initialNetworkProfiles(4013)["clean-lan"]) ||
		!reflect.DeepEqual(plan.resources, defaultTunResourceProfile()) || plan.resources.LogicalDataLaneCount != 0 {
		t.Fatal("pressure registration changed colocated provider or lane-0 host resources")
	}
	if err := (nativeBrowsingRunSettings{maxLivePages: plan.maxLivePages}).validate(); err != nil {
		t.Fatal(err)
	}
	settings := plan.runSettings(&nativeBrowsingPressureObservation{})
	if err := settings.validate(); err != nil || !settings.nextNavigationOverlap || settings.maxLivePages != plan.maxLivePages {
		t.Fatalf("pressure registration lost its revised next-navigation body overlap: %v", err)
	}
	plannedBytes := 0
	for page := range nativeBrowsingPageCount {
		pageBytes := nativeBrowsingPageBytes(page)
		if pageBytes < 50*1024 || 300*1024 < pageBytes {
			t.Fatalf("page %d left the fixed 50..300-KiB distribution: %d", page, pageBytes)
		}
		assetBytes := 0
		assets, next, err := nativeBrowsingReferences(nativeBrowsingHTML(page))
		if err != nil || next != nativeBrowsingURL(page+1, -1) {
			t.Fatalf("page %d lost its actual next-navigation link: %v", page, err)
		}
		for asset, target := range assets {
			if target != nativeBrowsingURL(page, asset) {
				t.Fatalf("page %d asset %d lost its generated reference", page, asset)
			}
			assetBytes += nativeBrowsingAssetBytes(page, asset)
		}
		if assetBytes+nativeBrowsingHTMLBytes != pageBytes {
			t.Fatalf("page %d omitted asset body bytes", page)
		}
		plannedBytes += pageBytes
	}
	if plannedBytes != 17_879_040 {
		t.Fatalf("the 100-page offered body plan changed: %d", plannedBytes)
	}
	hooks := plan.hooks()
	path := &fullTunPath{}
	if err := hooks.afterStage(fullTunConstructionStageMultiClient, path); err != nil || !path.nativeIngressForTest {
		t.Fatalf("pressure fixture lost native batch ingress: %v", err)
	}
	if hooks.configureDevicePlatformSettings != nil || hooks.configureProviderPlatformSettings != nil {
		t.Fatal("pressure fixture unexpectedly requires a platform observer")
	}
	applicationPacing := func(settings *clientconnect.SendBufferSettings) (bool, bool) {
		field := reflect.ValueOf(settings).Elem().FieldByName("ApplicationPacing")
		if !field.IsValid() {
			return false, false
		}
		if field.Kind() != reflect.Bool {
			t.Fatal("application pacing setting changed type")
		}
		return field.Bool(), true
	}
	for _, endpoint := range []struct {
		name      string
		configure func(*clientconnect.ClientSettings)
		budget    *clientconnect.TransferMemoryBudget
	}{
		{name: "device", configure: hooks.configureDeviceClientSettings, budget: plan.deviceRetryBudget},
		{name: "provider", configure: hooks.configureProviderClientSettings, budget: plan.providerRetryBudget},
	} {
		if endpoint.budget == nil || endpoint.budget.TotalByteCount() != 2*1024*1024 {
			t.Fatalf("%s retry pool lost its strict 2-MiB limit", endpoint.name)
		}
		for range 2 {
			settings := fullTunClientSettingsWithFeatures(fullTunRouteExchangeH1, nil, nil, nil,
				plan.resources.LogicalDataLaneCount, plan.resources.Features)
			before := *settings.SendBufferSettings
			beforePacing, beforePacingPresent := applicationPacing(settings.SendBufferSettings)
			endpoint.configure(settings)
			afterPacing, afterPacingPresent := applicationPacing(settings.SendBufferSettings)
			if settings.SendBufferSettings.ResendQueueBudget != endpoint.budget ||
				!settings.SendBufferSettings.ResendQueueRetainedByteAccounting ||
				settings.SendBufferSettings.LogicalDataLaneCount != 0 ||
				settings.SendBufferSettings.WindowSizing != before.WindowSizing ||
				afterPacing != beforePacing || afterPacingPresent != beforePacingPresent {
				t.Fatalf("%s rebuilt client lost retry ownership or changed production pacing/window policy", endpoint.name)
			}
		}
	}
	another := newNativeBrowsingPressurePlan()
	if plan.deviceRetryBudget == plan.providerRetryBudget ||
		plan.deviceRetryBudget == another.deviceRetryBudget || plan.providerRetryBudget == another.providerRetryBudget {
		t.Fatal("unrelated browsing endpoint owners share a mutable retry pool")
	}
}
