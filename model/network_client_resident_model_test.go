package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"maps"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestNominateResident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// run n parallel nominations
		// after nomination, each gets the current value
		// all the current values should agree

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var mutex sync.Mutex

		clientId := server.NewId()
		instanceId := server.NewId()

		var residentIdToReplace *server.Id

		for i := range 32 {
			nominationResidents := map[server.Id]*NetworkClientResident{}

			host := fmt.Sprintf("host_%d", i)
			service := fmt.Sprintf("service_%d", i)
			block := fmt.Sprintf("block_%d", i)
			internalPorts := map[int]bool{}
			for range mathrand.Intn(32) {
				internalPorts[mathrand.Intn(10000)] = true
			}

			if mathrand.Intn(4) == 0 {
				instanceId = server.NewId()
			}

			var wg sync.WaitGroup
			for range 128 {
				wg.Add(1)
				go func() {
					defer wg.Done()

					internalPorts_ := slices.Collect(maps.Keys(internalPorts))
					// note gob uses nil to encode empty slices
					if len(internalPorts_) == 0 {
						internalPorts_ = nil
					}

					ttl := 1 * time.Minute

					nomination := &NetworkClientResident{
						ClientId:              clientId,
						InstanceId:            instanceId,
						ResidentId:            server.NewId(),
						ResidentHost:          host,
						ResidentService:       service,
						ResidentBlock:         block,
						ResidentInternalPorts: internalPorts_,
					}
					nominated := NominateResident(
						ctx,
						residentIdToReplace,
						nomination,
						ttl,
					)
					if nominated {
						func() {
							mutex.Lock()
							defer mutex.Unlock()
							nominationResidents[nomination.ResidentId] = nomination
						}()
					} else {
						resident := GetResidentForClient(ctx, clientId, ttl)
						func() {
							mutex.Lock()
							defer mutex.Unlock()
							nominationResidents[nomination.ResidentId] = resident
						}()
					}
				}()
			}
			wg.Wait()

			residents := slices.Collect(maps.Values(nominationResidents))
			for i := 1; i < len(residents); i += 1 {
				connect.AssertEqual(t, *residents[i-1], *residents[i])
			}

			residentId := residents[0].ResidentId
			if residentIdToReplace != nil {
				connect.AssertNotEqual(t, *residentIdToReplace, residentId)
			}
			residentIdToReplace = &residentId
		}
	})
}

func TestResidentTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// nominate a resident
		// after ttl, it should go away

		// nominate a resident
		// get in a loop less than ttl
		// it should not go away

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		for i := range 2 {
			clientId := server.NewId()
			instanceId := server.NewId()

			host := fmt.Sprintf("host_%d", i)
			service := fmt.Sprintf("service_%d", i)
			block := fmt.Sprintf("block_%d", i)
			internalPorts := map[int]bool{}
			for range mathrand.Intn(32) {
				internalPorts[mathrand.Intn(10000)] = true
			}

			ttl := 1 * time.Second

			nomination := &NetworkClientResident{
				ClientId:              clientId,
				InstanceId:            instanceId,
				ResidentId:            server.NewId(),
				ResidentHost:          host,
				ResidentService:       service,
				ResidentBlock:         block,
				ResidentInternalPorts: slices.Collect(maps.Keys(internalPorts)),
			}
			nominated := NominateResident(
				ctx,
				nil,
				nomination,
				1*time.Second,
			)
			connect.AssertEqual(t, nominated, true)

			select {
			case <-time.After(4 * time.Second):
			}

			resident := GetResidentForClient(ctx, clientId, ttl)
			connect.AssertEqual(t, resident, nil)

			nomination = &NetworkClientResident{
				ClientId:              clientId,
				InstanceId:            instanceId,
				ResidentId:            server.NewId(),
				ResidentHost:          host,
				ResidentService:       service,
				ResidentBlock:         block,
				ResidentInternalPorts: slices.Collect(maps.Keys(internalPorts)),
			}
			nominated = NominateResident(
				ctx,
				nil,
				nomination,
				ttl,
			)
			connect.AssertEqual(t, nominated, true)

			// the get should keep the key alive
			for range 16 {
				select {
				case <-time.After(200 * time.Millisecond):
				}

				resident = GetResidentForClient(ctx, clientId, ttl)
				connect.AssertNotEqual(t, resident, nil)
			}

			RemoveResidentForClient(ctx, clientId, resident.ResidentId)

			resident = GetResidentForClient(ctx, clientId, ttl)
			connect.AssertEqual(t, resident, nil)
		}
	})
}
