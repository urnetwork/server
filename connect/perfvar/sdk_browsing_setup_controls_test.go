// Deterministic controls pin the fixture's public discovery and readiness rules.
// Synthetic pull views are component inputs; only the separate setup root runs SDK.
package perfvar

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// The same public discovery contract accepts a bounded controlled backend.
type sdkBrowsingSetupDiscoveryFunc func(context.Context, string, *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error)

// Delegate without changing caller context, credentials or arguments.
func (self sdkBrowsingSetupDiscoveryFunc) FindProviders2(ctx context.Context, token string, args *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
	return self(ctx, token, args)
}

// Both local IDs and provider IDs are generated independently as in the SDK.
func sdkBrowsingSetupControlViews(t *testing.T) ([sdkBrowsingProviderCount]clientconnect.Id, *sdk.ExitList, *sdk.WindowStatus, *sdk.ConnectedProviderLocationList) {
	t.Helper()
	var ready [sdkBrowsingProviderCount]clientconnect.Id
	exits, locations := sdk.NewExitList(), sdk.NewConnectedProviderLocationList()
	for _, ordinal := range []int{0, 1, 2, 3, 5, 6} {
		ready[ordinal] = clientconnect.NewId()
		window := "quality"
		if ordinal == 5 {
			window = "speed"
		}
		exits.Add(&sdk.Exit{ClientId: sdk.NewId(), WindowType: window})
		id, err := sdk.ParseId(ready[ordinal].String())
		if err != nil {
			t.Fatal(err)
		}
		locations.Add(&sdk.ConnectedProviderLocation{ClientId: id})
	}
	return ready, exits, &sdk.WindowStatus{TargetSize: 6, ProviderStateAdded: 6, ProviderStateInEvaluation: 1, ConnectionGeneration: 1}, locations
}

// Reoffers are deliberate: no pending/seen reservation is hidden in discovery.
func TestSdkBrowsingDiscoveryAllowsUninstalledReoffer(t *testing.T) {
	ready, _, _, _ := sdkBrowsingSetupControlViews(t)
	calls := 0
	discovery := newSdkBrowsingSetupDiscovery(func() [sdkBrowsingProviderCount]clientconnect.Id { return ready }, sdkBrowsingSetupDiscoveryFunc(func(context.Context, string, *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
		calls++
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{{ClientId: ready[0]}}}, nil
	}))
	args := &clientconnect.FindProviders2Args{Specs: []*clientconnect.ProviderSpec{{BestAvailable: true}}, Count: 1, RankMode: "quality"}
	for range 2 {
		result, err := discovery.FindProviders2(t.Context(), "synthetic-fixture-authority", args)
		if err != nil || result == nil || len(result.Providers) != 1 || result.Providers[0].ClientId != ready[0] {
			t.Fatalf("ordinary uninstalled reoffer changed: result=%v err=%v", result, err)
		}
	}
	if report := discovery.snapshot(); calls != 2 || report.QualityCalls != 2 || report.InitialExcludedEmpty != nil {
		t.Fatal("reoffer was reserved or fabricated an empty-read receipt")
	}
}

// The named-pool substitution must preserve all caller-owned filter authority.
func TestSdkBrowsingDiscoveryPreservesRequestScope(t *testing.T) {
	ready, _, _, _ := sdkBrowsingSetupControlViews(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	excluded := clientconnect.NewId()
	args := &clientconnect.FindProviders2Args{Specs: []*clientconnect.ProviderSpec{{BestAvailable: true}}, Count: 3, RankMode: "quality", ForceMinimum: true, IpFamily: clientconnect.IpFamilyFilterV4Only, ExcludeClientIds: []clientconnect.Id{excluded}, ExcludeDestinations: [][]clientconnect.Id{{clientconnect.NewId(), excluded}}}
	discovery := newSdkBrowsingSetupDiscovery(func() [sdkBrowsingProviderCount]clientconnect.Id { return ready }, sdkBrowsingSetupDiscoveryFunc(func(callCtx context.Context, token string, query *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
		if callCtx != ctx || token != "synthetic-fixture-authority" || query.Count != args.Count || query.RankMode != args.RankMode || query.ForceMinimum != args.ForceMinimum || query.IpFamily != args.IpFamily || !reflect.DeepEqual(query.ExcludeClientIds, args.ExcludeClientIds) || !reflect.DeepEqual(query.ExcludeDestinations, args.ExcludeDestinations) {
			t.Fatal("caller context/authority/filter changed")
		}
		if len(query.Specs) != 5 {
			t.Fatal("ready quality scope included an absent or speed provider")
		}
		foundFifth := false
		for _, spec := range query.Specs {
			if spec.ClientId == nil || *spec.ClientId == ready[5] || spec.BestAvailable {
				t.Fatal("named quality pool escaped its declared scope")
			}
			foundFifth = foundFifth || *spec.ClientId == ready[6]
		}
		if !foundFifth {
			t.Fatal("independent fifth quality authority was omitted")
		}
		query.ExcludeClientIds[0] = ready[0]
		query.ExcludeDestinations[0][1] = ready[0]
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{{ClientId: ready[0]}}}, nil
	}))
	if _, err := discovery.FindProviders2(ctx, "synthetic-fixture-authority", args); err != nil {
		t.Fatal(err)
	}
	if args.ExcludeClientIds[0] != excluded || args.ExcludeDestinations[0][1] != excluded || !args.Specs[0].BestAvailable {
		t.Fatal("backend mutated caller-owned discovery policy")
	}
}

// A held backend result never owns publication; the in-flight query keeps its
// original snapshot while the next call can observe a newly ready provider.
func TestSdkBrowsingDiscoverySnapshotDoesNotHoldBackendLock(t *testing.T) {
	ready, _, _, _ := sdkBrowsingSetupControlViews(t)
	var published atomic.Pointer[[sdkBrowsingProviderCount]clientconnect.Id]
	published.Store(&ready)
	lateId := clientconnect.NewId()
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	discovery := newSdkBrowsingSetupDiscovery(func() [sdkBrowsingProviderCount]clientconnect.Id { return *published.Load() }, sdkBrowsingSetupDiscoveryFunc(func(ctx context.Context, _ string, query *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
		calls++
		if calls == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		selected := ready[0]
		for _, spec := range query.Specs {
			if spec.ClientId != nil && *spec.ClientId == lateId {
				selected = lateId
			}
		}
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{{ClientId: selected}}}, nil
	}))
	args := &clientconnect.FindProviders2Args{Specs: []*clientconnect.ProviderSpec{{BestAvailable: true}}, Count: 1, RankMode: "quality"}
	result := make(chan *clientconnect.FindProviders2Result, 1)
	errResult := make(chan error, 1)
	go func() {
		value, err := discovery.FindProviders2(t.Context(), "synthetic-fixture-authority", args)
		result <- value
		errResult <- err
	}()
	select {
	case <-entered:
	case first := <-result:
		t.Fatalf("discovery returned before entering its held backend: result=%v err=%v", first, <-errResult)
	}
	newReady := ready
	newReady[4] = lateId
	published.Store(&newReady)
	close(release)
	first, err := <-result, <-errResult
	if err != nil || first == nil || len(first.Providers) != 1 || first.Providers[0].ClientId != ready[0] {
		t.Fatal("in-flight discovery did not retain its immutable ready snapshot")
	}
	second, err := discovery.FindProviders2(t.Context(), "synthetic-fixture-authority", args)
	if err != nil || second == nil || len(second.Providers) != 1 || second.Providers[0].ClientId != lateId {
		t.Fatal("independent publication was hidden behind the held API call")
	}
}

// Actual backend cancellation is joined and cannot publish a successful empty.
func TestSdkBrowsingDiscoveryCancellationKeepsCallerAuthority(t *testing.T) {
	ready, _, _, _ := sdkBrowsingSetupControlViews(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	discovery := newSdkBrowsingSetupDiscovery(func() [sdkBrowsingProviderCount]clientconnect.Id { return ready }, sdkBrowsingSetupDiscoveryFunc(func(callCtx context.Context, _ string, _ *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
		close(entered)
		<-callCtx.Done()
		return nil, callCtx.Err()
	}))
	done := make(chan error, 1)
	go func() {
		_, err := discovery.FindProviders2(ctx, "synthetic-fixture-authority", &clientconnect.FindProviders2Args{Count: 1, RankMode: "quality"})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("discovery returned before entering its cancelable backend: ", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("backend did not return the caller's cancellation: ", err)
	}
	if report := discovery.snapshot(); report.QualityCalls != 1 || report.Errors != 1 || report.InitialExcludedEmpty != nil {
		t.Fatal("canceled API work fabricated successful setup evidence")
	}
}

// Full local ownership can precede another discovery call. Only an actual
// excluded backend result creates an empty receipt; its absence stays unknown.
func TestSdkBrowsingSetupKeepsExcludedEmptyReadOptionalAndActual(t *testing.T) {
	ready, exits, status, locations := sdkBrowsingSetupControlViews(t)
	snapshot := sdkBrowsingSetupSnapshotFrom(exits, status, locations, ready)
	if qualified, err := sdkBrowsingSetupReady(snapshot, nil); !qualified || err != nil {
		t.Fatal("full observed ownership incorrectly required another API call")
	}
	discovery := newSdkBrowsingSetupDiscovery(func() [sdkBrowsingProviderCount]clientconnect.Id { return ready }, sdkBrowsingSetupDiscoveryFunc(func(context.Context, string, *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
	}))
	args := &clientconnect.FindProviders2Args{Count: 5, RankMode: "quality"}
	if _, err := discovery.FindProviders2(t.Context(), "synthetic-fixture-authority", args); err != nil {
		t.Fatal(err)
	}
	if discovery.snapshot().InitialExcludedEmpty != nil {
		t.Fatal("empty response without original exclusions was qualified")
	}
	for ordinal := range sdkBrowsingProviderCount {
		if !sdkBrowsingInitialQualityProvider(ordinal) {
			continue
		}
		args.ExcludeDestinations = append(args.ExcludeDestinations, []clientconnect.Id{clientconnect.NewId(), ready[ordinal]})
	}
	if _, err := discovery.FindProviders2(t.Context(), "synthetic-fixture-authority", args); err != nil {
		t.Fatal(err)
	}
	if report := discovery.snapshot(); report.InitialExcludedEmpty == nil || report.InitialExcludedEmpty.QualityCall != 2 || report.InitialExcludedEmpty.CallerExcludedMask&sdkBrowsingInitialQualityMask != sdkBrowsingInitialQualityMask {
		t.Fatal("actual excluded empty response did not retain its scalar evidence")
	}
}

// Unknown, duplicate and foreign pull views remain false or explicit failure.
func TestSdkBrowsingSetupRejectsIncompleteOrDuplicateViews(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*sdk.ExitList, *sdk.WindowStatus, *sdk.ConnectedProviderLocationList)
		wantErr bool
	}{
		{name: "duplicate-local", change: func(exits *sdk.ExitList, _ *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			exits.Get(1).ClientId = exits.Get(0).ClientId
		}},
		{name: "missing-local", change: func(exits *sdk.ExitList, _ *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			exits.Get(0).ClientId = nil
		}},
		{name: "speed-in-quality-slot", change: func(_ *sdk.ExitList, _ *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList) {
			locations.Get(1).ClientId = locations.Get(4).ClientId
		}},
		{name: "missing-provider-id", change: func(_ *sdk.ExitList, _ *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList) {
			locations.Get(1).ClientId = nil
		}},
		{name: "foreign-provider", change: func(_ *sdk.ExitList, _ *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList) {
			locations.Get(0).ClientId = sdk.NewId()
		}, wantErr: true},
		{name: "extra-quality-owner", change: func(exits *sdk.ExitList, _ *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			exits.Add(&sdk.Exit{ClientId: sdk.NewId(), WindowType: "quality"})
		}, wantErr: true},
		{name: "done-owner", change: func(exits *sdk.ExitList, _ *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			exits.Get(0).Done = true
		}},
		{name: "warning-owner", change: func(exits *sdk.ExitList, _ *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			exits.Get(0).Warning = true
		}},
		{name: "negative-evaluation-projection", change: func(_ *sdk.ExitList, status *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			status.ProviderStateInEvaluation = -1
		}},
		{name: "wrong-target", change: func(_ *sdk.ExitList, status *sdk.WindowStatus, _ *sdk.ConnectedProviderLocationList) {
			status.TargetSize = 5
		}},
	}
	for _, test := range cases {
		ready, exits, status, locations := sdkBrowsingSetupControlViews(t)
		test.change(exits, status, locations)
		snapshot := sdkBrowsingSetupSnapshotFrom(exits, status, locations, ready)
		qualified, err := sdkBrowsingSetupReady(snapshot, &sdkBrowsingSetupEmptyReceipt{QualityCall: 1, ReadyMask: 0x6f, CallerExcludedMask: sdkBrowsingInitialQualityMask})
		if qualified || (err != nil) != test.wantErr {
			t.Fatalf("%s: qualified=%t err=%v snapshot=%s", test.name, qualified, err, snapshot)
		}
	}
}

// Physical multiplicity is observed, not made a scheduling prerequisite.
// Every ready initial quality tail can back the exact five distinct local IDs.
func TestSdkBrowsingSetupAcceptsFullWindowMultiplicity(t *testing.T) {
	for ordinal := range sdkBrowsingProviderCount {
		if !sdkBrowsingInitialQualityProvider(ordinal) {
			continue
		}
		ready, exits, status, locations := sdkBrowsingSetupControlViews(t)
		id, err := sdk.ParseId(ready[ordinal].String())
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < locations.Len(); index++ {
			if locations.Get(index).ClientId.String() != ready[5].String() {
				locations.Get(index).ClientId = id
			}
		}
		snapshot := sdkBrowsingSetupSnapshotFrom(exits, status, locations, ready)
		qualified, err := sdkBrowsingSetupReady(snapshot, nil)
		if !qualified || err != nil || snapshot.ProviderCounts[ordinal] != 5 || snapshot.DistinctLocal != 6 || snapshot.DistinctProviders != 2 {
			t.Fatalf("quality ordinal=%d: valid installed multiplicity was rejected: %s err=%v", ordinal, snapshot, err)
		}
	}
}

// Monitor evaluation events can lag the independent installed-owner pull view.
// Their count is preserved exactly and never recast as absence of setup work.
func TestSdkBrowsingSetupRecordsEvaluationWithoutQuiescence(t *testing.T) {
	for _, evaluating := range []int{0, 1, 3} {
		ready, exits, status, locations := sdkBrowsingSetupControlViews(t)
		status.ProviderStateInEvaluation = evaluating
		snapshot := sdkBrowsingSetupSnapshotFrom(exits, status, locations, ready)
		qualified, err := sdkBrowsingSetupReady(snapshot, nil)
		if !qualified || err != nil || snapshot.InEvaluation != evaluating {
			t.Fatalf("evaluation projection=%d: qualified=%t err=%v snapshot=%s", evaluating, qualified, err, snapshot)
		}
	}
}
