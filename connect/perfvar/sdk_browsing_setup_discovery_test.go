// Named fixture discovery preserves the real API authority and caller policy.
// One empty-read receipt observes setup; it never reserves a peer or wakes SDK work.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// Each request owns the same authenticated context-bound API path as the SDK.
type sdkBrowsingSetupApiDiscovery struct {
	strategy *clientconnect.ClientStrategy
	apiUrl   string
}

// No platform result, eligibility decision or credential is synthesized.
func (self *sdkBrowsingSetupApiDiscovery) FindProviders2(ctx context.Context, token string, args *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
	api := clientconnect.NewBringYourApi(ctx, self.strategy, self.apiUrl)
	defer api.Close()
	api.SetByJwt(token)
	return api.FindProviders2SyncWithCtx(ctx, args)
}

// Scalars identify an actual successful API response, not pending-owner absence.
type sdkBrowsingSetupEmptyReceipt struct {
	QualityCall        int   `json:"quality_call"`
	ReadyMask          uint8 `json:"ready_mask"`
	CallerExcludedMask uint8 `json:"caller_excluded_mask"`
}

// Counters and one immutable receipt are bounded to one fixture lifetime.
type sdkBrowsingSetupDiscoveryReport struct {
	QualityCalls         int                           `json:"quality_calls"`
	SpeedCalls           int                           `json:"speed_calls"`
	Errors               int                           `json:"errors"`
	InitialExcludedEmpty *sdkBrowsingSetupEmptyReceipt `json:"initial_excluded_empty,omitempty"`
}

// Ready IDs are copied before I/O; no installed/seen/reserved peer set exists.
type sdkBrowsingSetupDiscovery struct {
	readyProviderIds func() [sdkBrowsingProviderCount]clientconnect.Id
	base             clientconnect.NetworkProviderDiscovery
	stateLock        sync.Mutex
	report           sdkBrowsingSetupDiscoveryReport
	changed          chan struct{}
}

// Construction publishes the complete fixed-size observation owner.
func newSdkBrowsingSetupDiscovery(ready func() [sdkBrowsingProviderCount]clientconnect.Id, base clientconnect.NetworkProviderDiscovery) *sdkBrowsingSetupDiscovery {
	return &sdkBrowsingSetupDiscovery{readyProviderIds: ready, base: base, changed: make(chan struct{}, 1)}
}

// Quality and speed use disjoint named pools; the server still applies actual
// authorization, common eligibility, caller exclusions and family filtering.
func (self *sdkBrowsingSetupDiscovery) FindProviders2(ctx context.Context, token string, args *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args == nil || token == "" {
		return nil, errors.New("SDK browsing discovery lacks caller authority")
	}
	if args.Count <= 0 || args.IpFamily != clientconnect.IpFamilyFilterDefault && args.IpFamily != clientconnect.IpFamilyFilterV4Capable && args.IpFamily != clientconnect.IpFamilyFilterV4Only {
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
	}
	start, end := 0, sdkBrowsingProviderCount
	if args.RankMode == "speed" {
		start, end = 5, 6
	} else if args.RankMode != "" && args.RankMode != "quality" {
		return nil, errors.New("unsupported SDK browsing rank mode")
	}
	ready := self.readyProviderIds()
	query := *args
	query.ExcludeClientIds = slices.Clone(args.ExcludeClientIds)
	query.ExcludeDestinations = make([][]clientconnect.Id, len(args.ExcludeDestinations))
	for index, path := range args.ExcludeDestinations {
		query.ExcludeDestinations[index] = slices.Clone(path)
	}
	query.Specs = nil
	for _, spec := range args.Specs {
		if spec == nil || spec.LocationId != nil || spec.LocationGroupId != nil || !spec.BestAvailable && spec.ClientId == nil {
			return nil, errors.New("SDK browsing discovery received an unowned scope")
		}
		if spec.ClientId != nil {
			owned := false
			for ordinal := start; ordinal < end; ordinal++ {
				if start == 0 && ordinal == 5 {
					continue
				}
				owned = owned || ready[ordinal] != (clientconnect.Id{}) && ready[ordinal] == *spec.ClientId
			}
			if !owned {
				return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
			}
			id := *spec.ClientId
			query.Specs = append(query.Specs, &clientconnect.ProviderSpec{ClientId: &id})
		}
	}
	if len(query.Specs) == 0 {
		for ordinal := start; ordinal < end; ordinal++ {
			if start == 0 && ordinal == 5 {
				continue
			}
			if id := ready[ordinal]; id != (clientconnect.Id{}) {
				query.Specs = append(query.Specs, &clientconnect.ProviderSpec{ClientId: &id})
			}
		}
	}
	if len(query.Specs) == 0 {
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
	}
	self.stateLock.Lock()
	qualityCall := 0
	if start == 0 {
		self.report.QualityCalls++
		qualityCall = self.report.QualityCalls
	} else {
		self.report.SpeedCalls++
	}
	self.stateLock.Unlock()
	result, err := self.base.FindProviders2(ctx, token, &query)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && result == nil {
		err = errors.New("actual SDK browsing discovery returned no envelope")
	}
	if err != nil {
		self.stateLock.Lock()
		self.report.Errors++
		self.stateLock.Unlock()
		return result, err
	}
	for _, candidate := range result.Providers {
		if candidate == nil {
			return nil, errors.New("actual SDK browsing discovery returned an empty provider")
		}
		owned := false
		for ordinal := start; ordinal < end; ordinal++ {
			if start == 0 && ordinal == 5 {
				continue
			}
			owned = owned || ready[ordinal] != (clientconnect.Id{}) && candidate.ClientId == ready[ordinal]
		}
		if !owned {
			return nil, errors.New("actual SDK browsing discovery returned an unowned provider")
		}
	}
	copyResult := *result
	copyResult.Providers = slices.Clone(result.Providers)
	if args.Count < len(copyResult.Providers) {
		copyResult.Providers = copyResult.Providers[:args.Count]
	}
	if qualityCall != 0 && len(result.Providers) == 0 {
		var readyMask, excludedMask uint8
		for ordinal, id := range ready {
			if id == (clientconnect.Id{}) {
				continue
			}
			readyMask |= uint8(1) << ordinal
			excluded := slices.Contains(args.ExcludeClientIds, id)
			for _, path := range args.ExcludeDestinations {
				excluded = excluded || len(path) > 0 && path[len(path)-1] == id
			}
			if excluded {
				excludedMask |= uint8(1) << ordinal
			}
		}
		if readyMask&sdkBrowsingInitialQualityMask == sdkBrowsingInitialQualityMask && readyMask&0x10 == 0 && excludedMask&sdkBrowsingInitialQualityMask == sdkBrowsingInitialQualityMask {
			self.stateLock.Lock()
			if self.report.InitialExcludedEmpty == nil {
				self.report.InitialExcludedEmpty = &sdkBrowsingSetupEmptyReceipt{QualityCall: qualityCall, ReadyMask: readyMask, CallerExcludedMask: excludedMask}
				select {
				case self.changed <- struct{}{}:
				default:
				}
			}
			self.stateLock.Unlock()
		}
	}
	return &copyResult, nil
}

// The first receipt is immutable, so a value copy cannot observe later mutation.
func (self *sdkBrowsingSetupDiscovery) snapshot() sdkBrowsingSetupDiscoveryReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.report
}

// Public pull views are independent; these fields describe observed geometry.
type sdkBrowsingSetupSnapshot struct {
	Quality           int                           `json:"quality"`
	Speed             int                           `json:"speed"`
	ExitRows          int                           `json:"exit_rows"`
	DistinctLocal     int                           `json:"distinct_local"`
	Done              int                           `json:"done"`
	Warnings          int                           `json:"warnings"`
	MissingLocal      int                           `json:"missing_local"`
	Target            int                           `json:"target"`
	InEvaluation      int                           `json:"in_evaluation"`
	Generation        int64                         `json:"generation"`
	LocationRows      int                           `json:"location_rows"`
	DistinctProviders int                           `json:"distinct_providers"`
	ProviderMask      uint8                         `json:"provider_mask"`
	ProviderCounts    [sdkBrowsingProviderCount]int `json:"provider_counts"`
	MissingProviders  int                           `json:"missing_providers"`
	ForeignProviders  int                           `json:"foreign_providers"`
}

// Diagnostics retain counts and fixture ordinals, never credentials or IDs.
func (self sdkBrowsingSetupSnapshot) String() string {
	return fmt.Sprintf("quality=%d speed=%d exits=%d local=%d done=%d warnings=%d missing_local=%d target=%d evaluating=%d generation=%d locations=%d providers=%d mask=%02x counts=%v missing=%d foreign=%d", self.Quality, self.Speed, self.ExitRows, self.DistinctLocal, self.Done, self.Warnings, self.MissingLocal, self.Target, self.InEvaluation, self.Generation, self.LocationRows, self.DistinctProviders, self.ProviderMask, self.ProviderCounts, self.MissingProviders, self.ForeignProviders)
}

// Local window-client IDs and physical provider-tail IDs have separate joins.
func sdkBrowsingSetupSnapshotFrom(exits *sdk.ExitList, status *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList, ready [sdkBrowsingProviderCount]clientconnect.Id) sdkBrowsingSetupSnapshot {
	var snapshot sdkBrowsingSetupSnapshot
	if status != nil {
		snapshot.Target, snapshot.InEvaluation, snapshot.Generation = status.TargetSize, status.ProviderStateInEvaluation, status.ConnectionGeneration
	}
	localIds := map[string]bool{}
	if exits != nil {
		snapshot.ExitRows = exits.Len()
		for index := 0; index < exits.Len(); index++ {
			exit := exits.Get(index)
			if exit == nil || exit.ClientId == nil {
				snapshot.MissingLocal++
			}
			if exit == nil {
				continue
			}
			if exit.ClientId != nil {
				localIds[exit.ClientId.String()] = true
			}
			if exit.Done {
				snapshot.Done++
			}
			if exit.Warning || exit.Quarantined {
				snapshot.Warnings++
			}
			if exit.WindowType == "quality" {
				snapshot.Quality++
			} else if exit.WindowType == "speed" {
				snapshot.Speed++
			}
		}
	}
	snapshot.DistinctLocal = len(localIds)
	if locations != nil {
		snapshot.LocationRows = locations.Len()
		for index := 0; index < locations.Len(); index++ {
			location := locations.Get(index)
			if location == nil || location.ClientId == nil {
				snapshot.MissingProviders++
				continue
			}
			owned := false
			for ordinal, id := range ready {
				if id != (clientconnect.Id{}) && location.ClientId.String() == id.String() {
					bit := uint8(1) << ordinal
					snapshot.ProviderCounts[ordinal]++
					if snapshot.ProviderMask&bit == 0 {
						snapshot.DistinctProviders++
					}
					snapshot.ProviderMask |= bit
					owned = true
					break
				}
			}
			if !owned {
				snapshot.ForeignProviders++
			}
		}
	}
	return snapshot
}

// Five genuine initial quality authorities plus speed make the six local slots
// feasible even when each discovery response is consumed before the next call.
// Evaluation is an asynchronous monitor projection, never a quiescence proof.
// An excluded empty result is retained if observed; a full window need not read
// again, so its absence is unknown rather than a reason to force more discovery.
func sdkBrowsingSetupReady(snapshot sdkBrowsingSetupSnapshot, empty *sdkBrowsingSetupEmptyReceipt) (bool, error) {
	if snapshot.Quality > 5 || snapshot.Speed > 1 || snapshot.ExitRows > 6 || snapshot.ForeignProviders != 0 {
		return false, fmt.Errorf("SDK_BROWSING_SETUP_OWNERSHIP: %s", snapshot)
	}
	geometry := snapshot.Quality == 5 && snapshot.Speed == 1 && snapshot.ExitRows == 6 && snapshot.DistinctLocal == 6 && snapshot.Done == 0 && snapshot.Warnings == 0 && snapshot.MissingLocal == 0 && snapshot.Target == 6 && snapshot.InEvaluation >= 0 && snapshot.Generation > 0 && snapshot.LocationRows == 6 && snapshot.DistinctProviders >= 2 && snapshot.DistinctProviders <= 6 && snapshot.ProviderMask & ^uint8(0x6f) == 0 && snapshot.MissingProviders == 0 && snapshot.ProviderCounts[4] == 0 && snapshot.ProviderCounts[5] == 1
	qualityCount, qualityProviders := 0, 0
	expectedMask := uint8(1 << 5)
	for ordinal := range sdkBrowsingProviderCount {
		if !sdkBrowsingInitialQualityProvider(ordinal) {
			continue
		}
		count := snapshot.ProviderCounts[ordinal]
		geometry = geometry && 0 <= count && count <= 5
		qualityCount += count
		if count > 0 {
			qualityProviders++
			expectedMask |= uint8(1) << ordinal
		}
	}
	// Local quorum is exact; the default enumerator may legitimately prefetch
	// repeated physical tails. Availability is proven separately, not inferred
	// from one scheduler-dependent installed multiplicity.
	geometry = geometry && qualityCount == 5 && snapshot.DistinctProviders == qualityProviders+1 && snapshot.ProviderMask == expectedMask
	if empty != nil && (empty.QualityCall <= 0 || empty.ReadyMask&sdkBrowsingInitialQualityMask != sdkBrowsingInitialQualityMask || empty.ReadyMask&0x10 != 0 || empty.CallerExcludedMask&sdkBrowsingInitialQualityMask != sdkBrowsingInitialQualityMask) {
		return false, errors.New("SDK_BROWSING_SETUP_EMPTY_RECEIPT: invalid observed response")
	}
	return geometry, nil
}

// Listeners never call the SDK or block on the state-change callback stack.
type sdkBrowsingSetupListener struct{ changed chan struct{} }

// A coalesced notification is only a hint to reread the current public views.
func (self *sdkBrowsingSetupListener) notify() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// Preserve the public listener contract without retaining its borrowed view.
func (self *sdkBrowsingSetupListener) WindowStatusChanged(*sdk.WindowStatus) { self.notify() }

// The physical provider projection can be published after local exit state.
func (self *sdkBrowsingSetupListener) ConnectedProviderLocationsChanged() { self.notify() }
