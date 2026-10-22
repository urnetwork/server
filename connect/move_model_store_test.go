// The mover and the load publisher over the real Redis move model
// (moveModelStore): a publish lands in the table the mover reads, the control
// in Redis turns the mover on and off without a deploy, the moves it sends
// start real cooldowns that keep the moved clients from moving again, dry run
// writes no cooldown, and a stopping publisher removes its record. Runs on
// the local test env.
package connect

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestMoverOverTheRedisModel(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		settings := DefaultExchangeSettings()
		identity := MoveProcessIdentity{Host: moveTestOwnHost, Block: "g1", ExchangeId: server.NewId()}
		publisher := newMoveLoadPublisherWithStore(
			ctx,
			identity,
			settings,
			&moveTestLoadSource{counts: MoveLoadCounts{Residents: 1000, ProviderResidents: 600, Transports: 1100}},
			&moveTestCpuSampler{sample: moveCpuSample{processCpu: 2, hostCpuBusy: 0.6, hostCores: 10}},
			moveModelStore{},
		)
		defer publisher.Close()
		if err := publisher.publish(); err != nil {
			t.Fatal(err)
		}
		// another host's process, below the mean
		publishCtx, publishCancel := context.WithTimeout(ctx, 10*time.Second)
		defer publishCancel()
		if _, err := model.PublishConnectMoveLoad(publishCtx, &model.ConnectMoveLoad{
			Host:        moveTestCoolHost,
			Block:       "g1",
			ExchangeId:  server.NewId(),
			Residents:   200,
			HostCpuBusy: 0.3,
			HostCores:   10,
		}); err != nil {
			t.Fatal(err)
		}

		residents := &moveTestResidents{}
		executor := &moveTestExecutor{failClientIds: map[server.Id]bool{}}
		mover := newMoverWithStore(
			ctx,
			identity,
			settings,
			&MoverDependencies{
				Residents:  residents,
				LoadSource: &moveTestLoadSource{},
				Inventory: &moveTestInventory{inventory: map[string]MoveInventoryHost{
					moveTestOwnHost:  moveTestInventoryHost("192.0.2.10", "2001:db8::10"),
					moveTestCoolHost: moveTestInventoryHost("192.0.2.20", "2001:db8::20"),
				}},
				Executor:      executor,
				TargetBreaker: &moveTestBreaker{open: map[netip.Addr]bool{}},
				DbWait:        &moveTestDbWait{},
			},
			moveModelStore{},
			moveTestRandom(),
			time.Now,
		)
		defer mover.Close()
		now := time.Now()
		for i := 0; i < 30; i += 1 {
			residents.candidates = append(residents.candidates, &MoveCandidate{
				ClientId:          server.NewId(),
				ResidentStartTime: now.Add(-time.Hour),
				LastPayloadTime:   now.Add(-30 * time.Minute),
				MoveVersion:       1,
				DirectCarrier:     true,
				Ipv4:              true,
			})
		}

		// the control is absent: the code default is off
		if outcome := mover.tick(); outcome != moverTickOff {
			t.Fatalf("absent control outcome = %s", outcome)
		}

		// dry run: budget, no cooldowns, no sends
		controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
		defer controlCancel()
		if err := model.SetConnectMoveControl(controlCtx, map[string]string{model.ConnectMoveControlFieldMode: "dry_run"}); err != nil {
			t.Fatal(err)
		}
		if outcome := mover.tick(); outcome != moverTickMoved {
			t.Fatalf("dry run outcome = %s", outcome)
		}
		if len(executor.sentRequests()) != 0 {
			t.Fatal("dry run sent moves")
		}
		clientIds := []server.Id{}
		for _, candidate := range residents.candidates {
			clientIds = append(clientIds, candidate.ClientId)
		}
		readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
		defer readCancel()
		cooling, err := model.GetConnectMoveCooldowns(readCtx, clientIds)
		if err != nil {
			t.Fatal(err)
		}
		if len(cooling) != 0 {
			t.Fatalf("dry run started %d cooldowns", len(cooling))
		}

		// on
		if err := model.SetConnectMoveControl(controlCtx, map[string]string{model.ConnectMoveControlFieldMode: "on"}); err != nil {
			t.Fatal(err)
		}
		if outcome := mover.tick(); outcome != moverTickMoved {
			t.Fatalf("outcome = %s", outcome)
		}
		requests := executor.sentRequests()
		if len(requests) != settings.MoveMaxPerTick {
			t.Fatalf("sent %d moves", len(requests))
		}
		cooling, err = model.GetConnectMoveCooldowns(readCtx, clientIds)
		if err != nil {
			t.Fatal(err)
		}
		if len(cooling) != len(requests) {
			t.Fatalf("%d cooldowns for %d moves", len(cooling), len(requests))
		}
		for _, request := range requests {
			if !cooling[request.ClientId] || request.TargetHost != moveTestCoolHost {
				t.Errorf("request %+v has no cooldown", request)
			}
		}

		// moved clients are not moved again
		if outcome := mover.tick(); outcome != moverTickMoved {
			t.Fatalf("outcome = %s", outcome)
		}
		movedIds := map[server.Id]int{}
		for _, request := range executor.sentRequests() {
			movedIds[request.ClientId] += 1
			if 1 < movedIds[request.ClientId] {
				t.Fatalf("moved %s twice within its cooldown", request.ClientId)
			}
		}

		// the kill switch
		sentCount := len(executor.sentRequests())
		if err := model.SetConnectMoveControl(controlCtx, map[string]string{model.ConnectMoveControlFieldMode: "off"}); err != nil {
			t.Fatal(err)
		}
		if outcome := mover.tick(); outcome != moverTickOff {
			t.Fatalf("killed outcome = %s", outcome)
		}
		if len(executor.sentRequests()) != sentCount {
			t.Fatal("moved after the kill switch")
		}

		// a stopping publisher removes its record
		if err := publisher.remove(); err != nil {
			t.Fatal(err)
		}
		loads, _, err := model.GetConnectMoveLoads(readCtx)
		if err != nil {
			t.Fatal(err)
		}
		for _, load := range loads {
			if load.ExchangeId == identity.ExchangeId {
				t.Fatal("the publisher's record outlived its remove")
			}
		}
		if err := model.SetConnectMoveControl(controlCtx, map[string]string{model.ConnectMoveControlFieldMode: "on"}); err != nil {
			t.Fatal(err)
		}
		if outcome := mover.tick(); outcome != moverTickNoLoad {
			t.Fatalf("outcome without a record = %s", outcome)
		}
	})
}
