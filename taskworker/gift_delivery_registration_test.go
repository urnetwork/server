package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/task"
)

// The production worker renders paid gift orders' cards and runs their daily
// retention, and installs the fonts and scenes the cards print with (they link
// into the taskworker only). Neither task is a removed target.
func TestGiftDeliveryTasksRegisteredInProduction(t *testing.T) {
	worker, err := InitTaskWorkerForProfile(t.Context(), nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	names := []string{
		task.NewTaskTargetWithPost(controller.GiftRenderCards, controller.GiftRenderCardsPost).TargetFunctionName(),
		task.NewTaskTargetWithPost(controller.GiftRetention, controller.GiftRetentionPost).TargetFunctionName(),
	}
	for _, name := range names {
		if !worker.HasTarget(name) {
			t.Fatalf("the production worker does not register %s", name)
		}
		for _, removed := range removedTaskTargets {
			if removed == name {
				t.Fatalf("%s is listed as a removed target", name)
			}
		}
	}
	if !controller.GiftCardAssetsInstalled() {
		t.Fatal("the production worker did not install the gift card assets")
	}
	// the daily retention runs at 04:15 UTC, the next one after now
	now := time.Date(2026, 11, 13, 4, 15, 0, 0, time.UTC)
	if next := controller.NextGiftRetentionAt(now); !next.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("next retention after %s is %s", now, next)
	}
	if next := controller.NextGiftRetentionAt(now.Add(-time.Minute)); !next.Equal(now) {
		t.Fatalf("next retention before 04:15 is %s", next)
	}
}
