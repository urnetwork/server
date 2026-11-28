// A census of the runtime timers one simulated connection holds on a connect
// process, by the code that made them (LBALTOPT2.md §8.1, CPUOPT7.md §11). A
// connection here is a client's resident, nominated on the production path
// with its idle watcher, registration refresh, stream authorizations, and its
// stream hop and network peer listeners' backstop polls; one forward from it
// with its idle watcher; and the transport's announcement, with verify egress
// refreshes on, and its authorization lease, with a credential horizon. These
// are the owners whose waits move to the process's timer sweep, wired as
// newExchange and NewConnectHandler wire them (timer_census_shim_test.go). It
// runs on the test DB env, since the announcement registers on the database
// and the nomination on Redis.
//
// At the census every owner waits in its select, so each live channel timer
// it holds is in a P's heap. Live timer allocations are counted from the
// allocation profile after three collections, against a count taken before
// the connections were made.
package connect

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

const timerCensusConnectionCount = 64

// The census groups, by the code that made the timer.
var timerCensusOwners = []struct {
	name     string
	creators []string
}{
	{"announce rounds and test schedule", []string{"connect.(*ConnectionAnnounce).run"}},
	{"announce verify egress refresh", []string{"connect.(*ConnectionAnnounce).run.func"}},
	{"announce passive samples", []string{"connect.(*ConnectionAnnounce).runMeasurementPublisher"}},
	{"lease horizon", []string{"session.(*AuthorizationLease).runTimer"}},
	{"lease rechecks", []string{"session.(*AuthorizationLease).runChecks"}},
	{"resident idle watch", []string{"connect.(*Resident).runIdleWatcher"}},
	// the loop is a closure of nominateLocalResident before this change
	{"resident registration refresh", []string{"connect.(*Exchange).runResidentRegistrationRefresh", "connect.(*Exchange).nominateLocalResident.func"}},
	{"resident stream authorizations", []string{"connect.(*Resident).runStreamAuthorizations"}},
	{"forward idle watch", []string{"connect.(*ResidentForward).runIdleWatcher"}},
	{"stream hop listener poll", []string{"model.(*StreamHopListener).run"}},
	{"network peer listener poll", []string{"model.(*NetworkPeerListener).run"}},
}

// A creator ending in ".func" names a function's closures.
func timerCensusOwner(creator string) string {
	creator = strings.TrimPrefix(creator, "github.com/urnetwork/server/v2026/")
	for _, group := range timerCensusOwners {
		for _, name := range group.creators {
			if strings.HasSuffix(name, ".func") && strings.HasPrefix(creator, name) || creator == name {
				return group.name
			}
		}
	}
	return ""
}

// The function that made a runtime timer, past the time and context
// packages, for the allocation of a runtime timer, or "" for any other.
func runtimeTimerCreator(stack []uintptr) string {
	frames := runtime.CallersFrames(stack)
	timerAllocation := false
	for {
		frame, more := frames.Next()
		switch {
		case frame.Function == "runtime.newTimer" || frame.Function == "time.newTimer":
			timerAllocation = true
		case !timerAllocation:
			if !strings.HasPrefix(frame.Function, "runtime.") {
				return ""
			}
		case strings.HasPrefix(frame.Function, "time.") || strings.HasPrefix(frame.Function, "context.") ||
			strings.HasPrefix(frame.Function, "runtime."):
		default:
			return frame.Function
		}
		if !more {
			return ""
		}
	}
}

// Live runtime timers by the function that made them.
func liveRuntimeTimersByCreator() map[string]int64 {
	for range 3 {
		runtime.GC()
	}
	records := []runtime.MemProfileRecord{}
	for {
		n, ok := runtime.MemProfile(records, false)
		if ok {
			records = records[:n]
			break
		}
		records = make([]runtime.MemProfileRecord, n+64)
	}
	counts := map[string]int64{}
	for _, record := range records {
		if creator := runtimeTimerCreator(record.Stack()); creator != "" {
			counts[creator] += record.InUseObjects()
		}
	}
	return counts
}

type timerCensusConnection struct {
	forward     *ResidentForward
	forwardDone chan struct{}
	announce    *ConnectionAnnounce
	lease       *session.AuthorizationLease
}

func TestConnectionTimerCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		// every census client is a top-level client whose resident runs a
		// network peer listener
		exchange.settings.EnableNetworkPeers = true
		censusInstallExchangeSweep(exchange)
		defer func() {
			exchange.Close()
			idleCtx, idleCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer idleCancel()
			if !exchange.WaitForIdle(idleCtx) {
				t.Error("the census exchange did not stop")
			}
		}()

		rate := runtime.MemProfileRate
		runtime.MemProfileRate = 1
		defer func() {
			runtime.MemProfileRate = rate
		}()

		verifySettings := model.DefaultVerifySettings()
		settings := DefaultConnectionAnnounceSettings()
		settings.LocationRetryTimeout = 0
		settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
			return verifySettings, true
		}
		registered := make(chan struct{}, timerCensusConnectionCount)
		settings.connectionRegisteredForTest = func(*ConnectionAnnounce) {
			registered <- struct{}{}
		}
		censusInstallAnnounceSweep(settings, exchange)

		// made before the first count, so the census counts none of the
		// test's own timers
		registerCtx, registerCancel := context.WithTimeout(ctx, 5*time.Minute)
		defer registerCancel()
		before := liveRuntimeTimersByCreator()
		sweepBefore := censusSweepArmedCount(exchange)

		var connectionsLock sync.Mutex
		connections := []*timerCensusConnection{}
		defer func() {
			connectionsLock.Lock()
			defer connectionsLock.Unlock()
			for _, connection := range connections {
				if connection.lease != nil {
					connection.lease.Close()
				}
				if connection.announce != nil {
					connection.announce.CloseAndWait()
				}
				connection.forward.Cancel()
				<-connection.forwardDone
			}
		}()
		for range timerCensusConnectionCount {
			networkId := server.NewId()
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			if !exchange.NominateLocalResidentWithContext(ctx, clientId, server.NewId(), nil) {
				t.Fatal("the census resident's nomination was refused")
			}
			connection := &timerCensusConnection{
				forward:     NewResidentForward(ctx, exchange, server.NewId()),
				forwardDone: make(chan struct{}),
			}
			func() {
				connectionsLock.Lock()
				defer connectionsLock.Unlock()
				connections = append(connections, connection)
			}()
			go func() {
				defer close(connection.forwardDone)
				connection.forward.runIdleWatcher(clientId)
			}()

			announceCtx, announceCancel := context.WithCancel(ctx)
			connection.announce = NewConnectionAnnounceWithIpFamily(
				controller.Testing_WithConnectionLocationWriter(
					announceCtx,
					func(context.Context, server.Id, string) error {
						return nil
					},
				),
				announceCancel,
				networkId,
				clientId,
				locationAnnounceClientAddress,
				0,
				"",
				server.NewId(),
				0,
				V0TestConfig(),
				settings,
			)
			select {
			case <-registered:
			case <-connection.announce.done:
				t.Fatal("the census announcement ended before it registered")
			case <-registerCtx.Done():
				t.Fatal("timed out waiting for the census announcements to register")
			}

			credential := session.NewByJwt(networkId, server.NewId(), "census", false, false).Client(server.NewId(), clientId)
			sessionId := server.NewId()
			credential.SessionId = &sessionId
			credential.ExpiresAt = gojwt.NewNumericDate(time.Now().Add(time.Hour))
			lease, err := censusStartLease(ctx, credential, exchange, func(int) {})
			if err != nil {
				t.Fatalf("the census lease was refused: %v", err)
			}
			connection.lease = lease
		}
		// every owner waits in its select, and every resident's stream hop
		// listener has made its first poll, at most 6 s in, and sent its
		// client the resulting stream reset, as an older resident has
		time.Sleep(8 * time.Second)

		after := liveRuntimeTimersByCreator()
		sweepRegistrations := censusSweepArmedCount(exchange) - sweepBefore

		perOwner := map[string]int64{}
		otherByCreator := map[string]int64{}
		total := int64(0)
		for creator, count := range after {
			delta := count - before[creator]
			if delta == 0 {
				continue
			}
			total += delta
			if owner := timerCensusOwner(creator); owner != "" {
				perOwner[owner] += delta
			} else {
				otherByCreator[creator] += delta
			}
		}
		perConnection := func(count int64) string {
			return fmt.Sprintf("%.2f", float64(count)/timerCensusConnectionCount)
		}
		ownersTotal := int64(0)
		lines := []string{fmt.Sprintf("%d simulated connections", timerCensusConnectionCount)}
		for _, group := range timerCensusOwners {
			ownersTotal += perOwner[group.name]
			lines = append(lines, fmt.Sprintf("  %-34s %6d live runtime timers, %s a connection", group.name, perOwner[group.name], perConnection(perOwner[group.name])))
		}
		lines = append(lines, fmt.Sprintf("  %-34s %6d, %s a connection", "these owners' runtime timers", ownersTotal, perConnection(ownersTotal)))
		lines = append(lines, fmt.Sprintf("  %-34s %6d, %s a connection", "their sweep timers", sweepRegistrations, perConnection(int64(sweepRegistrations))))
		others := make([]string, 0, len(otherByCreator))
		for creator := range otherByCreator {
			others = append(others, creator)
		}
		slices.SortFunc(others, func(a, b string) int {
			return int(otherByCreator[b] - otherByCreator[a])
		})
		for _, creator := range others {
			lines = append(lines, fmt.Sprintf("  other: %-60s %6d", strings.TrimPrefix(creator, "github.com/urnetwork/"), otherByCreator[creator]))
		}
		lines = append(lines, fmt.Sprintf("  %-34s %6d, %s a connection", "all runtime timers made", total, perConnection(total)))
		t.Log(strings.Join(lines, "\n"))

		if censusExpectsSweep() {
			if ownersTotal != 0 {
				t.Fatalf("the converted owners hold %d runtime timers, want none", ownersTotal)
			}
			// rounds, test schedule, verify refresh, passive samples, lease
			// horizon and rechecks, resident idle watch, refresh and stream
			// authorizations, forward idle watch, and the resident's stream
			// hop and network peer listener polls
			if want := 12 * timerCensusConnectionCount; sweepRegistrations != want {
				t.Fatalf("%d sweep timers armed for %d connections, want %d", sweepRegistrations, timerCensusConnectionCount, want)
			}
		}
	})
}
