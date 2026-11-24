// The owner's registration bound waits a bounded time for a slot and refuses
// before any credential or database read, reads as a rate limit to the SDK,
// gives a device that cannot route its reserved slots, and never leaks a slot.
// Every identity here is synthetic and no database is needed.
package localclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// An authority with no credential: any call that gets past admission fails
// its local preflight with a 401 and never reaches the database. Its device
// can route unless mintAdmissionNoRoute says otherwise, and takes a Pro
// device's reserved registrations.
func mintAdmissionTestAuthority(admission *MintAdmission) (*Authority, *connect.AuthNetworkClientArgs) {
	clientId := server.NewId()
	parent := connect.Id(clientId)
	authority := &Authority{
		networkId:     server.NewId(),
		userId:        server.NewId(),
		clientId:      clientId,
		deviceId:      server.NewId(),
		pro:           true,
		apiUrl:        "https://control.example",
		mintAdmission: admission,
		cancel:        func() {},
	}
	authority.reservedMints.Store(admission.reservedCountPerDevice(true))
	return authority, &connect.AuthNetworkClientArgs{SourceClientId: &parent}
}

// The authority's device cannot route. Returns how often its route was read.
func mintAdmissionNoRoute(authority *Authority) *atomic.Int64 {
	reads := &atomic.Int64{}
	authority.SetRoutable(func() bool {
		reads.Add(1)
		return false
	})
	return reads
}

func mintAdmissionStatus(t *testing.T, err error) int {
	t.Helper()
	var status *connect.HttpStatusError
	if !errors.As(err, &status) {
		t.Fatalf("error %v carries no http status", err)
	}
	return status.StatusCode
}

// A registration in flight on a shared slot; the bound must have one free.
func holdSharedMintSlot(t *testing.T, admission *MintAdmission) mintLane {
	t.Helper()
	lane, err := admission.acquire(t.Context(), nil, nil)
	if err != nil || lane != mintLaneShared {
		t.Fatalf("holding a shared slot got lane %d, %v", lane, err)
	}
	return lane
}

// A bound with neither a wait nor reserved slots, as before 2026-11-18.
func instantMintAdmission(maxConcurrentCount int) *MintAdmission {
	return NewMintAdmissionWithSettings(MintAdmissionSettings{MaxConcurrentCount: maxConcurrentCount})
}

// No slot is held and nobody waits.
func assertMintAdmissionIdle(t *testing.T, admission *MintAdmission) {
	t.Helper()
	admission.stateLock.Lock()
	defer admission.stateLock.Unlock()
	if admission.sharedInUse != 0 || admission.reservedInUse != 0 || len(admission.waiters) != 0 {
		t.Fatalf("the bound holds %d shared and %d reserved slots and %d waiters, want none", admission.sharedInUse, admission.reservedInUse, len(admission.waiters))
	}
}

func mintAdmissionWaitCount(t *testing.T) uint64 {
	t.Helper()
	var metric dto.Metric
	if err := mintAdmissionWaitSeconds.Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount()
}

// A full bound with no wait refuses at once as a 429 the SDK window classifier
// reads as a rate limit, marked as a busy bound; a free slot admits the call to
// its preflight, and the slot is returned when the call ends.
func TestAuthorityMintAdmissionRefusesPastBound(t *testing.T) {
	admission := instantMintAdmission(1)
	authority, args := mintAdmissionTestAuthority(admission)
	lane := holdSharedMintSlot(t, admission)
	refusals := testutil.ToFloat64(mintAdmissionRefusals)
	_, err := authority.AuthNetworkClient(t.Context(), args)
	if mintAdmissionStatus(t, err) != http.StatusTooManyRequests {
		t.Fatalf("a full bound answered %v, want 429", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "429") {
		t.Fatalf("refusal %q does not read as a rate limit", err)
	}
	// the mark connect's window reads (connect ClientRegistrationBusyError),
	// named here by its method so neither build needs the other's version
	var busy interface {
		ClientRegistrationBusy() bool
	}
	if !errors.As(err, &busy) || !busy.ClientRegistrationBusy() {
		t.Fatalf("refusal %q is not marked as a busy bound", err)
	}
	if err.Error() != "429 Too Many Requests: hosted client registration is at its concurrent bound" {
		t.Fatalf("refusal reads %q", err)
	}
	if counted := testutil.ToFloat64(mintAdmissionRefusals) - refusals; counted != 1 {
		t.Fatalf("counted %v refusals, want 1", counted)
	}
	admission.release(lane)

	// admitted: the empty credential fails preflight, not admission
	_, err = authority.AuthNetworkClient(t.Context(), args)
	if mintAdmissionStatus(t, err) != http.StatusUnauthorized {
		t.Fatalf("an admitted call answered %v, want its preflight 401", err)
	}
	assertMintAdmissionIdle(t, admission)
}

// The hosted proxy's default bound of two admits two registrations in flight.
// A third from a device that can route waits its second for a slot and, with
// none returned, is refused as the 429 the SDK window reads as a rate limit; a
// returned slot admits the next call to its preflight.
func TestAuthorityMintAdmissionBoundOfTwoRefusesTheThird(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		authority, args := mintAdmissionTestAuthority(admission)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		refusedWaits := testutil.ToFloat64(mintAdmissionWaitsRefused)
		start := time.Now()
		_, err := authority.AuthNetworkClient(t.Context(), args)
		if mintAdmissionStatus(t, err) != http.StatusTooManyRequests {
			t.Fatalf("the third registration answered %v, want 429", err)
		}
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("the third registration was refused after %s, want its 1 s wait", waited)
		}
		if counted := testutil.ToFloat64(mintAdmissionWaitsRefused) - refusedWaits; counted != 1 {
			t.Fatalf("counted %v refused waits, want 1", counted)
		}
		admission.release(first)
		_, err = authority.AuthNetworkClient(t.Context(), args)
		if mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("a call admitted by a returned slot answered %v, want its preflight 401", err)
		}
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// A registration that finds the bound full waits for a slot instead of being
// refused at once: one returned 300 ms later admits it to its preflight, and
// the wait is counted as admitted. Before 2026-11-18 it was refused at once,
// and the SDK window waited 2.5-5 s to ask again.
func TestAuthorityMintAdmissionWaiterTakesAReturnedSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		authority, args := mintAdmissionTestAuthority(admission)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		refusals := testutil.ToFloat64(mintAdmissionRefusals)
		admittedWaits := testutil.ToFloat64(mintAdmissionWaitsAdmitted)
		waitCount := mintAdmissionWaitCount(t)

		start := time.Now()
		result := make(chan error, 1)
		go func() {
			_, err := authority.AuthNetworkClient(t.Context(), args)
			result <- err
		}()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("a registration that found the bound full answered %v at once, want a wait", err)
		default:
		}
		time.Sleep(300 * time.Millisecond)
		admission.release(first)
		err := <-result
		if mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("a waiting registration answered %v, want its preflight 401 once a slot was returned", err)
		}
		if waited := time.Since(start); waited != 300*time.Millisecond {
			t.Fatalf("the waiter was admitted after %s, want when the slot was returned", waited)
		}
		if counted := testutil.ToFloat64(mintAdmissionRefusals) - refusals; counted != 0 {
			t.Fatalf("counted %v refusals for an admitted wait", counted)
		}
		if counted := testutil.ToFloat64(mintAdmissionWaitsAdmitted) - admittedWaits; counted != 1 {
			t.Fatalf("counted %v admitted waits, want 1", counted)
		}
		if counted := mintAdmissionWaitCount(t) - waitCount; counted != 1 {
			t.Fatalf("observed %d wait times, want 1", counted)
		}
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// Returned slots go to the waiters in the order they arrived.
func TestAuthorityMintAdmissionServesWaitersInArrivalOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(1)
		held := holdSharedMintSlot(t, admission)
		grants := make(chan int, 3)
		releases := []chan struct{}{}
		for i := range 3 {
			release := make(chan struct{})
			releases = append(releases, release)
			go func() {
				lane, err := admission.acquire(t.Context(), nil, nil)
				if err != nil {
					t.Errorf("waiter %d: %v", i, err)
					return
				}
				grants <- i
				<-release
				admission.release(lane)
			}()
			// each arrives once the one before waits
			synctest.Wait()
		}
		admission.release(held)
		for i := range 3 {
			synctest.Wait()
			select {
			case waiter := <-grants:
				if waiter != i {
					t.Fatalf("slot %d went to waiter %d, want %d", i+1, waiter, i)
				}
			default:
				t.Fatalf("slot %d went to no waiter", i+1)
			}
			close(releases[i])
		}
		synctest.Wait()
		assertMintAdmissionIdle(t, admission)
	})
}

// Past MaxWaiterCount waiters a registration is refused at once; the queued
// ones are refused when their own wait runs out.
func TestAuthorityMintAdmissionRefusesAtOncePastTheWaiterBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		authority, args := mintAdmissionTestAuthority(admission)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		maxWaiterCount := admission.settings.MaxWaiterCount
		if maxWaiterCount != 32 {
			t.Fatalf("waiter bound %d, want 32", maxWaiterCount)
		}
		var waiters sync.WaitGroup
		for range maxWaiterCount {
			waiters.Add(1)
			go func() {
				defer waiters.Done()
				_, err := authority.AuthNetworkClient(t.Context(), args)
				var status *connect.HttpStatusError
				if !errors.As(err, &status) || status.StatusCode != http.StatusTooManyRequests {
					t.Errorf("a queued registration answered %v, want 429", err)
				}
			}()
		}
		synctest.Wait()
		start := time.Now()
		_, err := authority.AuthNetworkClient(t.Context(), args)
		if mintAdmissionStatus(t, err) != http.StatusTooManyRequests {
			t.Fatalf("a registration past the waiter bound answered %v, want 429", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("a registration past the waiter bound was refused after %s, want at once", waited)
		}
		waiters.Wait()
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("the queued registrations were refused after %s, want their 1 s wait", waited)
		}
		admission.release(first)
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// A waiter whose caller ends leaves at once with the caller's error, not a
// rate limit, and the next returned slot goes to the waiter behind it.
func TestAuthorityMintAdmissionCanceledWaiterLeavesTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(1)
		authority, args := mintAdmissionTestAuthority(admission)
		held := holdSharedMintSlot(t, admission)
		ctx, cancel := context.WithCancel(t.Context())
		canceledResult := make(chan error, 1)
		go func() {
			_, err := authority.AuthNetworkClient(ctx, args)
			canceledResult <- err
		}()
		synctest.Wait()
		nextResult := make(chan error, 1)
		go func() {
			_, err := authority.AuthNetworkClient(t.Context(), args)
			nextResult <- err
		}()
		synctest.Wait()
		refusals := testutil.ToFloat64(mintAdmissionRefusals)
		canceledWaits := testutil.ToFloat64(mintAdmissionWaitsCanceled)

		cancel()
		if err := <-canceledResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("a canceled waiter answered %v, want its caller's cancellation", err)
		}
		if counted := testutil.ToFloat64(mintAdmissionWaitsCanceled) - canceledWaits; counted != 1 {
			t.Fatalf("counted %v canceled waits, want 1", counted)
		}
		if counted := testutil.ToFloat64(mintAdmissionRefusals) - refusals; counted != 0 {
			t.Fatalf("counted %v refusals for a canceled wait", counted)
		}
		admission.release(held)
		if err := <-nextResult; mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("the waiter behind a canceled one answered %v, want its preflight 401", err)
		}
		assertMintAdmissionIdle(t, admission)
	})
}

// With the shared slots full, a device that cannot route takes a reserved slot
// at once, spending one of its six, while a device that can route waits for a
// shared slot. Before 2026-11-18 both were refused at once.
func TestAuthorityMintAdmissionReservedSlotAdmitsADeviceThatCannotRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		cold, coldArgs := mintAdmissionTestAuthority(admission)
		mintAdmissionNoRoute(cold)
		warm, warmArgs := mintAdmissionTestAuthority(admission)
		reservedAdmissions := testutil.ToFloat64(mintAdmissionReservedAdmissions)

		start := time.Now()
		_, err := cold.AuthNetworkClient(t.Context(), coldArgs)
		if mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("a device that cannot route answered %v, want its preflight 401", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("a device that cannot route waited %s for a reserved slot", waited)
		}
		if counted := testutil.ToFloat64(mintAdmissionReservedAdmissions) - reservedAdmissions; counted != 1 {
			t.Fatalf("counted %v reserved admissions, want 1", counted)
		}
		if left := cold.reservedMints.Load(); left != 5 {
			t.Fatalf("the device has %d reserved registrations left, want 5", left)
		}
		_, err = warm.AuthNetworkClient(t.Context(), warmArgs)
		if mintAdmissionStatus(t, err) != http.StatusTooManyRequests || time.Since(start) != time.Second {
			t.Fatalf("a device that can route answered %v after %s, want 429 after its wait", err, time.Since(start))
		}
		admission.release(first)
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// The bound holds two reserved slots. A returned reserved slot goes to the
// oldest waiter that may take one, past a device that can route queued ahead
// of it, which takes the next shared slot.
func TestAuthorityMintAdmissionReservedSlotsAreBoundedInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		reservedLanes := []mintLane{}
		for range 2 {
			cold, _ := mintAdmissionTestAuthority(admission)
			mintAdmissionNoRoute(cold)
			lane, err := admission.acquire(t.Context(), cold.noRoute, &cold.reservedMints)
			if err != nil || lane != mintLaneReserved {
				t.Fatalf("a device that cannot route got lane %d, %v, want a reserved slot", lane, err)
			}
			reservedLanes = append(reservedLanes, lane)
		}
		warm, warmArgs := mintAdmissionTestAuthority(admission)
		warmResult := make(chan error, 1)
		go func() {
			_, err := warm.AuthNetworkClient(t.Context(), warmArgs)
			warmResult <- err
		}()
		synctest.Wait()
		cold, coldArgs := mintAdmissionTestAuthority(admission)
		mintAdmissionNoRoute(cold)
		coldResult := make(chan error, 1)
		go func() {
			_, err := cold.AuthNetworkClient(t.Context(), coldArgs)
			coldResult <- err
		}()
		synctest.Wait()
		select {
		case err := <-warmResult:
			t.Fatalf("a device that can route answered %v with every slot held", err)
		case err := <-coldResult:
			t.Fatalf("a third device that cannot route answered %v with both reserved slots held", err)
		default:
		}

		admission.release(reservedLanes[0])
		synctest.Wait()
		select {
		case err := <-coldResult:
			if mintAdmissionStatus(t, err) != http.StatusUnauthorized {
				t.Fatalf("the device that cannot route answered %v, want its preflight 401", err)
			}
		default:
			t.Fatal("a returned reserved slot did not go to the waiting device that cannot route")
		}
		select {
		case err := <-warmResult:
			t.Fatalf("a device that can route answered %v on a reserved slot", err)
		default:
		}
		admission.release(first)
		if err := <-warmResult; mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("a device that can route answered %v on a returned shared slot, want its preflight 401", err)
		}
		admission.release(reservedLanes[1])
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// A device takes six reserved registrations in its life; past them it waits
// for a shared slot like any other. A reopened proxy builds a new device and
// authority, with six of its own.
func TestAuthorityMintAdmissionReservedSlotsAreBoundedPerDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		cold, args := mintAdmissionTestAuthority(admission)
		mintAdmissionNoRoute(cold)
		perDevice := admission.settings.ReservedCountPerDevice
		if perDevice != 6 {
			t.Fatalf("reserved registrations per device %d, want 6", perDevice)
		}
		start := time.Now()
		for i := range perDevice {
			if _, err := cold.AuthNetworkClient(t.Context(), args); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
				t.Fatalf("reserved registration %d answered %v, want its preflight 401", i+1, err)
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the reserved registrations waited %s", waited)
		}
		_, err := cold.AuthNetworkClient(t.Context(), args)
		if mintAdmissionStatus(t, err) != http.StatusTooManyRequests || time.Since(start) != time.Second {
			t.Fatalf("a seventh registration answered %v after %s, want 429 after its wait", err, time.Since(start))
		}
		reopened, reopenedArgs := mintAdmissionTestAuthority(admission)
		mintAdmissionNoRoute(reopened)
		if _, err := reopened.AuthNetworkClient(t.Context(), reopenedArgs); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("a reopened device answered %v, want its preflight 401 on its own reserved slot", err)
		}
		admission.release(first)
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// The device's route is read only when the shared slots are full, so the
// common registration does not touch the device's window status.
func TestAuthorityMintAdmissionReadsTheRouteOnlyWhenTheSharedSlotsAreFull(t *testing.T) {
	admission := NewMintAdmission(2)
	cold, args := mintAdmissionTestAuthority(admission)
	routeReads := mintAdmissionNoRoute(cold)
	if _, err := cold.AuthNetworkClient(t.Context(), args); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
		t.Fatalf("a registration with a free slot answered %v", err)
	}
	if reads := routeReads.Load(); reads != 0 {
		t.Fatalf("a registration with a free shared slot read the route %d times", reads)
	}
	if left := cold.reservedMints.Load(); left != 6 {
		t.Fatalf("a shared slot spent a reserved registration: %d left", left)
	}
	first := holdSharedMintSlot(t, admission)
	second := holdSharedMintSlot(t, admission)
	if _, err := cold.AuthNetworkClient(t.Context(), args); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
		t.Fatalf("a device that cannot route answered %v", err)
	}
	if reads := routeReads.Load(); reads != 1 {
		t.Fatalf("a registration past the shared slots read the route %d times, want 1", reads)
	}
	admission.release(first)
	admission.release(second)
	assertMintAdmissionIdle(t, admission)
}

// One owner's bound is shared by all of its authorities: the slots taken
// through one device are unavailable to another.
func TestAuthorityMintAdmissionIsSharedByOwner(t *testing.T) {
	admission := instantMintAdmission(2)
	first, firstArgs := mintAdmissionTestAuthority(admission)
	second, secondArgs := mintAdmissionTestAuthority(admission)
	held := []mintLane{holdSharedMintSlot(t, admission), holdSharedMintSlot(t, admission)}
	for _, c := range []struct {
		authority *Authority
		args      *connect.AuthNetworkClientArgs
	}{
		{authority: first, args: firstArgs},
		{authority: second, args: secondArgs},
	} {
		if _, err := c.authority.AuthNetworkClient(t.Context(), c.args); mintAdmissionStatus(t, err) != http.StatusTooManyRequests {
			t.Fatalf("a device of a full owner answered %v, want 429", err)
		}
	}
	for _, lane := range held {
		admission.release(lane)
	}
	assertMintAdmissionIdle(t, admission)
}

// Concurrent callers, half of them devices that cannot route, never leave a
// slot held or a waiter queued.
func TestAuthorityMintAdmissionConcurrentCallsReturnSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(3)
		warm, warmArgs := mintAdmissionTestAuthority(admission)
		cold, coldArgs := mintAdmissionTestAuthority(admission)
		mintAdmissionNoRoute(cold)
		var wait sync.WaitGroup
		for i := range 64 {
			authority, args := warm, warmArgs
			if i%2 == 0 {
				authority, args = cold, coldArgs
			}
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := authority.AuthNetworkClient(context.Background(), args)
				var status *connect.HttpStatusError
				if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized && status.StatusCode != http.StatusTooManyRequests {
					t.Errorf("unexpected result %v", err)
				}
			}()
		}
		wait.Wait()
		assertMintAdmissionIdle(t, admission)
	})
}

// No admission admits everything, and nonpositive capacities build none.
func TestAuthorityMintAdmissionNilAdmitsEverything(t *testing.T) {
	if NewMintAdmission(0) != nil || NewMintAdmission(-1) != nil {
		t.Fatal("a nonpositive bound built an admission")
	}
	authority, args := mintAdmissionTestAuthority(nil)
	mintAdmissionNoRoute(authority)
	for range 4 {
		if _, err := authority.AuthNetworkClient(t.Context(), args); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("an unbounded owner answered %v, want its preflight 401", err)
		}
	}
}

// A closed authority reports its lifecycle, not a rate limit.
func TestAuthorityMintAdmissionClosedAuthorityIsCanceled(t *testing.T) {
	admission := NewMintAdmission(1)
	authority, args := mintAdmissionTestAuthority(admission)
	authority.closed.Store(true)
	if _, err := authority.AuthNetworkClient(t.Context(), args); !errors.Is(err, context.Canceled) {
		t.Fatalf("a closed authority answered %v, want canceled", err)
	}
	assertMintAdmissionIdle(t, admission)
}

// The 2026-10-22 loop: with the database slow, hosted windows retired most new
// identities before they connected and kept minting replacements, and every
// admitted registration added database work to the loop; c2040f152 cut the
// shared slots from four to two. A device that never routes, its candidates
// failing, may take the reserved slots for its first six registrations only,
// so a churning fleet runs on the two shared slots as before. Here ten such
// devices register for a minute, each registration held 100 ms (a slow
// database) and each refusal asked again 250 ms later: never more than 2+2
// registrations in flight, and once the reserved registrations are spent never
// more than the 2 shared ones. The control, a reserved lane without the
// per-device bound, holds 4 in flight to the end: the bound before c2040f152.
func TestMintAdmissionHoldsChurningDevicesToTheSharedSlots(t *testing.T) {
	type churn struct {
		maxInFlight     int64
		maxLateInFlight int64
		reservedCount   int64
		admittedCount   int64
	}
	run := func(reservedCountPerDevice int) churn {
		var result churn
		synctest.Test(t, func(t *testing.T) {
			settings := DefaultMintAdmissionSettings(2)
			settings.ReservedCountPerDevice = reservedCountPerDevice
			admission := NewMintAdmissionWithSettings(settings)
			var inFlight atomic.Int64
			var maxInFlight atomic.Int64
			var maxLateInFlight atomic.Int64
			var reservedCount atomic.Int64
			var admittedCount atomic.Int64
			noteMax := func(m *atomic.Int64, n int64) {
				for {
					old := m.Load()
					if n <= old || m.CompareAndSwap(old, n) {
						return
					}
				}
			}
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			var devices sync.WaitGroup
			for range 10 {
				reserved := &atomic.Int64{}
				reserved.Store(admission.reservedCountPerDevice(true))
				noRoute := func() bool { return true }
				devices.Add(1)
				go func() {
					defer devices.Done()
					for ctx.Err() == nil {
						lane, err := admission.acquire(ctx, noRoute, reserved)
						if err != nil {
							select {
							case <-ctx.Done():
							case <-time.After(250 * time.Millisecond):
							}
							continue
						}
						n := inFlight.Add(1)
						noteMax(&maxInFlight, n)
						// every device has spent its reserved registrations
						// within seconds; the late half shows the steady state
						if 30*time.Second <= time.Since(start) {
							noteMax(&maxLateInFlight, n)
						}
						admittedCount.Add(1)
						if lane == mintLaneReserved {
							reservedCount.Add(1)
						}
						time.Sleep(100 * time.Millisecond)
						inFlight.Add(-1)
						admission.release(lane)
					}
				}()
			}
			devices.Wait()
			assertMintAdmissionIdle(t, admission)
			result = churn{
				maxInFlight:     maxInFlight.Load(),
				maxLateInFlight: maxLateInFlight.Load(),
				reservedCount:   reservedCount.Load(),
				admittedCount:   admittedCount.Load(),
			}
		})
		return result
	}

	bounded := run(DefaultMintAdmissionSettings(2).ReservedCountPerDevice)
	t.Logf("per-device bound: %+v", bounded)
	if 4 < bounded.maxInFlight {
		t.Fatalf("%d registrations in flight, want at most the 2 shared and 2 reserved slots", bounded.maxInFlight)
	}
	if 2 < bounded.maxLateInFlight {
		t.Fatalf("a churning fleet held %d registrations in flight once its reserved registrations were spent, want at most the 2 shared slots", bounded.maxLateInFlight)
	}
	if 10*6 < bounded.reservedCount {
		t.Fatalf("%d reserved registrations for 10 devices, want at most 6 each", bounded.reservedCount)
	}
	unbounded := run(1 << 30)
	t.Logf("no per-device bound (control): %+v", unbounded)
	if unbounded.maxLateInFlight != 4 {
		t.Fatalf("the control held %d registrations in flight late, want 4: the test no longer sees the loop", unbounded.maxLateInFlight)
	}
}
