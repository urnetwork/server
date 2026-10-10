package providertunnel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func setupTimingTest(t *testing.T, run func(*testing.T)) {
	t.Helper()
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, run)
}

func setupTimingClient(t *testing.T, id connect.Id) *connect.Client {
	t.Helper()
	settings := connect.DefaultClientSettings()
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = connect.NewNoopLogger()
	client := connect.NewClient(t.Context(), id, connect.NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return client
}

func setupTimingRegistered(t *testing.T, owner *SetupObservations, id connect.Id, returnedIDs ...connect.Id) (*providerSetupState, *connect.Client, *connect.MultiClientGeneratorClientArgs) {
	t.Helper()
	provider := connect.NewId()
	state := newProviderSetupState(owner, provider)
	args := &connect.MultiClientGeneratorClientArgs{ClientId: id, ClientAuth: &connect.ClientAuth{}}
	destination := connect.RequireMultiHopId(provider)
	credential := state.beginCredential()
	time.Sleep(2 * time.Second)
	state.finishCredential(credential, args, nil, &destination)
	time.Sleep(time.Second)
	constructor := state.beginConstructor(args)
	time.Sleep(3 * time.Second)
	returnedID := id
	if len(returnedIDs) > 0 {
		returnedID = returnedIDs[0]
	}
	client := setupTimingClient(t, returnedID)
	state.finishConstructor(constructor, client, nil)
	return state, client, args
}

func setupTimingEvent(state *providerSetupState, id connect.Id, kind connect.ProviderState, at time.Time) map[connect.Id]*connect.ProviderEvent {
	return map[connect.Id]*connect.ProviderEvent{id: {ClientId: id, EgressClientId: state.provider, State: kind, EventTime: at}}
}

func setupTimingFreeze(state *providerSetupState, err error) {
	state.mu.Lock()
	state.closeStarted = time.Now()
	state.mu.Unlock()
	state.finish(err)
}

func TestProviderSetupTimingSourceClocksAndTerminalSnapshot(t *testing.T) {
	setupTimingTest(t, func(t *testing.T) {
		owner := &SetupObservations{}
		id := connect.NewId()
		state, _, _ := setupTimingRegistered(t, owner, id)
		monitor := newFakeMonitor()
		stop := state.watch(monitor)
		time.Sleep(4 * time.Second)
		evaluation := setupTimingEvent(state, id, connect.ProviderStateInEvaluation, time.Now())
		time.Sleep(5 * time.Second)
		added := setupTimingEvent(state, id, connect.ProviderStateAdded, time.Now())
		time.Sleep(20 * time.Second)
		state.events(evaluation)
		// The callback may coalesce away Added; its current active snapshot
		// still contains the actual source timestamp, not this delivery time.
		monitor.events = added
		stop()
		if owner.Snapshot() != (SetupTiming{}) {
			t.Fatal("unsubscribe published before terminal cleanup")
		}
		state.events(added) // a family/extender callback racing unsubscribe
		state.finish(nil)
		got := owner.Snapshot()
		if got.Tunnels[SetupAdmissionMatched] != 1 || got.RouteStages != [4]time.Duration{2 * time.Second, time.Second, 3 * time.Second, 9 * time.Second} ||
			got.EvaluationCount != 1 || got.EvaluationToAdmission != 5*time.Second || got.Pending != [2]uint64{} {
			t.Fatalf("source-time join changed: %+v", got)
		}
		if got.Calls[0][0] != (SetupCallTiming{1, 2 * time.Second}) || got.Calls[1][0] != (SetupCallTiming{1, 3 * time.Second}) {
			t.Fatalf("actual call totals changed: %+v", got.Calls)
		}
		state.finish(errors.New("late close"))
		state.events(added)
		if owner.Snapshot() != got {
			t.Fatal("repeated Close or late callback changed frozen evidence")
		}
	})
}

func TestProviderSetupTimingCoalescedEventsStayQualified(t *testing.T) {
	for _, added := range []bool{false, true} {
		t.Run(map[bool]string{false: "added_missing", true: "evaluation_missing"}[added], func(t *testing.T) {
			setupTimingTest(t, func(t *testing.T) {
				owner := &SetupObservations{}
				id := connect.NewId()
				state, _, _ := setupTimingRegistered(t, owner, id)
				time.Sleep(time.Second)
				if added {
					state.events(setupTimingEvent(state, id, connect.ProviderStateAdded, time.Now()))
				} else {
					state.events(setupTimingEvent(state, id, connect.ProviderStateRemoved, time.Now()))
				}
				setupTimingFreeze(state, nil)
				got := owner.Snapshot()
				want := SetupAdmissionMissing
				if added {
					want = SetupAdmissionMatched
				}
				if got.Tunnels[want] != 1 || got.EvaluationCount != 0 || got.EvaluationToAdmission != 0 {
					t.Fatalf("coalesced event invented evidence: %+v", got)
				}
				if !added && got.RouteStages != [4]time.Duration{} {
					t.Fatal("missing Added entered duration denominator")
				}
			})
		})
	}
}

func TestProviderSetupTimingDuplicateConstructorsArePermanent(t *testing.T) {
	for _, afterAdded := range []bool{false, true} {
		t.Run(map[bool]string{false: "reordered_returns", true: "declined_replacement"}[afterAdded], func(t *testing.T) {
			setupTimingTest(t, func(t *testing.T) {
				owner := &SetupObservations{}
				id := connect.NewId()
				state, client, args := setupTimingRegistered(t, owner, id)
				if afterAdded {
					state.events(setupTimingEvent(state, id, connect.ProviderStateAdded, time.Now()))
				}
				older, newer := state.beginConstructor(args), state.beginConstructor(args)
				state.finishConstructor(newer, client, nil)
				time.Sleep(time.Second)
				state.finishConstructor(older, client, nil)
				state.events(setupTimingEvent(state, id, connect.ProviderStateAdded, time.Now()))
				setupTimingFreeze(state, nil)
				got := owner.Snapshot()
				if got.Tunnels[SetupAdmissionAmbiguous] != 1 || got.RouteStages != [4]time.Duration{} || got.Calls[1][0].Count != 3 {
					t.Fatalf("reused identity resurrected a route match: %+v", got)
				}
			})
		})
	}
}

func TestProviderSetupTimingDistinctCandidateAndIdentityCapFailClosed(t *testing.T) {
	for _, count := range []int{2, providerSetupIdentityLimit + 1} {
		setupTimingTest(t, func(t *testing.T) {
			owner := &SetupObservations{}
			state := newProviderSetupState(owner, connect.NewId())
			var first *connect.MultiClientGeneratorClientArgs
			for range count {
				args := &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId(), ClientAuth: &connect.ClientAuth{}}
				if first == nil {
					first = args
				}
				credential := state.beginCredential()
				state.finishCredential(credential, args, nil, nil)
				constructor := state.beginConstructor(args)
				state.finishConstructor(constructor, nil, errors.New("synthetic setup failure"))
			}
			if state.entryCount > providerSetupIdentityLimit {
				t.Fatal("identity bound expanded")
			}
			// Another appearance cannot evict an old tombstone or clear overflow.
			constructor := state.beginConstructor(first)
			state.finishConstructor(constructor, nil, context.Canceled)
			setupTimingFreeze(state, nil)
			got := owner.Snapshot()
			want := SetupAdmissionAmbiguous
			if count > providerSetupIdentityLimit {
				want = SetupAdmissionOverflow
			}
			if got.Tunnels[want] != 1 || got.RouteStages != [4]time.Duration{} || got.Calls[1][2].Count != uint64(count) || got.Calls[1][1].Count != 1 {
				t.Fatalf("bounded unknown lost call outcomes: %+v", got)
			}
		})
	}
}

func TestProviderSetupTimingForeignAndContradictoryEventsFailClosed(t *testing.T) {
	for _, kind := range []string{"foreign_id", "foreign_provider", "wrong_key", "zero", "future", "pre_constructor", "pre_registration", "reversed_evaluation", "mismatched_client"} {
		t.Run(kind, func(t *testing.T) {
			setupTimingTest(t, func(t *testing.T) {
				owner := &SetupObservations{}
				id := connect.NewId()
				var returnedIDs []connect.Id
				if kind == "mismatched_client" {
					returnedIDs = []connect.Id{connect.NewId()}
				}
				state, _, _ := setupTimingRegistered(t, owner, id, returnedIDs...)
				time.Sleep(time.Second)
				events := setupTimingEvent(state, id, connect.ProviderStateAdded, time.Now())
				event := events[id]
				switch kind {
				case "foreign_id":
					events = setupTimingEvent(state, connect.NewId(), connect.ProviderStateAdded, time.Now())
				case "foreign_provider":
					event.EgressClientId = connect.NewId()
				case "wrong_key":
					event.ClientId = connect.NewId()
				case "zero":
					event.EventTime = time.Time{}
				case "future":
					event.EventTime = time.Now().Add(time.Second)
				case "pre_constructor":
					event.EventTime = state.entries[0].constructorStart.Add(-time.Second)
				case "pre_registration":
					event.EventTime = state.entries[0].registrationEnd.Add(-time.Second)
				case "reversed_evaluation":
					time.Sleep(time.Second)
					state.events(setupTimingEvent(state, id, connect.ProviderStateInEvaluation, time.Now()))

				}
				state.events(events)
				if state.entryCount != 1 {
					t.Fatal("foreign event created identity state")
				}
				setupTimingFreeze(state, nil)
				got := owner.Snapshot()
				if got.Tunnels[SetupAdmissionInvalid] != 1 || got.RouteStages != [4]time.Duration{} {
					t.Fatalf("invalid source timing was matched: %+v", got)
				}
			})
		})
	}
}

func TestProviderSetupTimingPendingAndFailedCallsRemainIncomplete(t *testing.T) {
	for phase := range 2 {
		setupTimingTest(t, func(t *testing.T) {
			owner := &SetupObservations{}
			id := connect.NewId()
			state, client, args := setupTimingRegistered(t, owner, id)
			state.events(setupTimingEvent(state, id, connect.ProviderStateAdded, time.Now()))
			var call providerSetupCall
			if phase == 0 {
				call = state.beginCredential()
			} else {
				call = state.beginConstructor(args)
			}
			setupTimingFreeze(state, context.Canceled)
			got := owner.Snapshot()
			if got.Tunnels[SetupAdmissionIncomplete] != 1 || got.Pending[phase] != 1 || got.CloseErrors != 1 || got.RouteStages != [4]time.Duration{} {
				t.Fatalf("in-flight wrapper looked complete: %+v", got)
			}
			if phase == 0 {
				state.finishCredential(call, args, nil, nil)
			} else {
				state.finishConstructor(call, client, nil)
			}
			if owner.Snapshot() != got {
				t.Fatal("post-freeze return changed completed observation")
			}
		})
	}
	setupTimingTest(t, func(t *testing.T) {
		owner := &SetupObservations{}
		state := newProviderSetupState(owner, connect.NewId())
		call := state.beginConstructor(nil)
		state.finishConstructor(call, nil, nil)
		setupTimingFreeze(state, nil)
		if got := owner.Snapshot(); got.Tunnels[SetupAdmissionInvalid] != 1 || got.Calls[1][2].Count != 1 {
			t.Fatalf("nil constructor result invented success: %+v", got)
		}
	})
}

func TestProviderSetupTimingCallbackAcrossFreezeAndOwnerIsolation(t *testing.T) {
	setupTimingTest(t, func(t *testing.T) {
		id := connect.NewId()
		firstOwner, secondOwner := &SetupObservations{}, &SetupObservations{}
		first, _, _ := setupTimingRegistered(t, firstOwner, id)
		second, _, _ := setupTimingRegistered(t, secondOwner, id)
		monitor := newFakeMonitor()
		stop := first.watch(monitor)
		events := setupTimingEvent(first, id, connect.ProviderStateAdded, time.Now())
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		callback := monitor.callbacks[0]
		go func() { close(entered); <-release; callback(nil, events, false); close(done) }()
		<-entered
		stop()
		setupTimingFreeze(second, nil)
		first.finish(nil)
		before := firstOwner.Snapshot()
		close(release)
		<-done
		if firstOwner.Snapshot() != before || before.Tunnels[SetupAdmissionMissing] != 1 || secondOwner.Snapshot().Tunnels[SetupAdmissionMissing] != 1 {
			t.Fatal("late or cross-owner callback created admission evidence")
		}
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() { first.events(events); first.finish(nil); _ = firstOwner.Snapshot() })
		}
		wg.Wait()
		if firstOwner.Snapshot() != before {
			t.Fatal("concurrent late callbacks changed a frozen snapshot")
		}
	})
}

func TestProviderSetupTimingActualWrapperCallsPreserveErrors(t *testing.T) {
	setupTimingTest(t, func(t *testing.T) {
		owner := &SetupObservations{}
		want := errors.New("synthetic credential denial")
		generator, _ := credentialObservationGenerator(t, &syntheticProbeCredentials{auth: func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
			time.Sleep(time.Second)
			return nil, want
		}})
		provider := connect.NewId()
		generator.setup = newProviderSetupState(owner, provider)
		destination := connect.RequireMultiHopId(provider)
		calls := []func() (*connect.MultiClientGeneratorClientArgs, error){generator.NewClientArgs,
			func() (*connect.MultiClientGeneratorClientArgs, error) {
				return generator.NewClientArgsContext(t.Context())
			},
			func() (*connect.MultiClientGeneratorClientArgs, error) {
				return generator.NewClientArgsForDestination(destination)
			},
			func() (*connect.MultiClientGeneratorClientArgs, error) {
				return generator.NewClientArgsForDestinationContext(t.Context(), destination)
			},
		}
		for _, call := range calls {
			if args, err := call(); args != nil || !errors.Is(err, want) {
				t.Fatal("wrapper changed credential result")
			}
		}
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		args := &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId(), ClientAuth: &connect.ClientAuth{}}
		if client, err := generator.NewClient(t.Context(), args, connect.DefaultClientSettings()); client != nil || err == nil {
			t.Fatal("closed generator admitted a client")
		}
		if client, err := generator.NewClientContext(t.Context(), t.Context(), args, connect.DefaultClientSettings()); client != nil || err == nil {
			t.Fatal("closed generator admitted a contextual client")
		}
		setupTimingFreeze(generator.setup, nil)
		got := owner.Snapshot()
		if got.Calls[0][2] != (SetupCallTiming{4, 4 * time.Second}) || got.Calls[1][2].Count != 2 || !generator.registration.unavailable() {
			t.Fatalf("wrapper duplicated or missed real calls: %+v", got)
		}
	})
}

func TestProviderSetupTimingOpenCloseWiringAndDisabledObserver(t *testing.T) {
	owner := &SetupObservations{}
	cfg := dummyOpenConfig()
	cfg.SetupObservations = owner
	tunnel, err := Open(t.Context(), cfg, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.setup == nil || tunnel.unwatchSetup == nil || owner.Snapshot() != (SetupTiming{}) {
		t.Fatal("Open lost its private setup owner or published early")
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	got := owner.Snapshot()
	var tunnels uint64
	for _, count := range got.Tunnels {
		tunnels += count
	}
	// Real background enumeration can leave a wrapper pending at freeze.
	if tunnels != 1 || got.Tunnels[SetupAdmissionNoConstructor]+got.Tunnels[SetupAdmissionIncomplete] != 1 ||
		(got.Tunnels[SetupAdmissionIncomplete] == 1 && got.Pending == [2]uint64{}) || context.Cause(tunnel.Lost()) != ErrTunnelClosed {
		t.Fatalf("Close changed ownership or coverage: %+v", got)
	}
	if err := tunnel.Close(); err != nil || owner.Snapshot() != got {
		t.Fatal("repeated Close emitted twice")
	}
	if state := newProviderSetupState(nil, connect.NewId()); state != nil {
		t.Fatal("disabled diagnostic allocated an owner")
	}
	// No-constructor classification itself is deterministic when no real
	// window background work participates.
	empty := &SetupObservations{}
	setupTimingFreeze(newProviderSetupState(empty, connect.NewId()), nil)
	if empty.Snapshot().Tunnels[SetupAdmissionNoConstructor] != 1 {
		t.Fatal("empty owned tunnel lost no-constructor coverage")
	}
	var disabled *providerSetupState
	disabled.finishConstructor(disabled.beginConstructor(nil), nil, context.Canceled)
	disabled.finishCredential(disabled.beginCredential(), nil, context.Canceled, nil)
	disabled.events(nil)
	disabled.finish(nil)
}

func TestProviderSetupTimingRejectsForeignCredentialDestination(t *testing.T) {
	setupTimingTest(t, func(t *testing.T) {
		owner := &SetupObservations{}
		state := newProviderSetupState(owner, connect.NewId())
		args := &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId(), ClientAuth: &connect.ClientAuth{}}
		destination := connect.RequireMultiHopId(connect.NewId())
		call := state.beginCredential()
		state.finishCredential(call, args, nil, &destination)
		constructor := state.beginConstructor(args)
		state.finishConstructor(constructor, setupTimingClient(t, args.ClientId), nil)
		state.events(setupTimingEvent(state, args.ClientId, connect.ProviderStateAdded, time.Now()))
		setupTimingFreeze(state, nil)
		got := owner.Snapshot()
		if got.Tunnels[SetupAdmissionInvalid] != 1 || got.RouteStages != [4]time.Duration{} || got.Calls[0][0].Count != 1 {
			t.Fatalf("foreign destination acquired a valid route join: %+v", got)
		}
	})
}
