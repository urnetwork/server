package main

import (
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
)

func TestProviderTransitionBonusCommandPropagatesRefusal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		opts := docopt.Opts{"--plan_id": server.NewId().String(), "--amount_usd": "1", "--adjustment_id": server.NewId().String(), "--reason": "explicit reviewed legacy correction"}
		if err := payoutPlanApplyBonus(opts); err == nil {
			t.Fatal("command reported success after rejected bonus mutation")
		}
	})
}
