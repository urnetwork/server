// A separate browsing scenario uses the SDK's public connected boundary while
// ordinary probes continue. It never changes the strict six-provider scenario.
package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// Public readiness determines the threshold. Every location row must be owned,
// while independent local channels may share one remote provider identity.
func privateSdkProductionReadiness(snapshot privateSdkBrowsingReadinessSnapshot) (bool, error) {
	if snapshot.Unowned != 0 {
		return false, errors.New("production-readiness projection contains an unowned destination")
	}
	owned := 0 < snapshot.Exits && snapshot.LocalDistinct == snapshot.Exits &&
		snapshot.MissingLocalIdentities == 0 && snapshot.MissingIdentities == 0 &&
		0 < snapshot.DistinctOwned && snapshot.DistinctOwned <= snapshot.Locations &&
		snapshot.OwnedMask&^uint8(63) == 0 && bits.OnesCount8(snapshot.OwnedMask) == snapshot.DistinctOwned
	return snapshot.MinSatisfied && 0 < snapshot.Quality+snapshot.Speed && owned, nil
}

// A device generation change across independent public reads invalidates
// only that candidate snapshot. A later unchanged public minimum can qualify.
func privateSdkProductionReadinessSnapshotFrom(exits *sdk.ExitList, before, after *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList, providers *[6]privateSdkBrowsingProvider) (privateSdkBrowsingReadinessSnapshot, bool) {
	snapshot := privateSdkBrowsingReadinessSnapshotFrom(exits, after, locations, providers)
	stable := before != nil && after != nil && before.ConnectionGeneration == after.ConnectionGeneration
	return snapshot, stable
}

// The existing public generation brackets the local and remote identity pulls;
// no private state lookup or new production API is needed.
func privateSdkProductionReadinessSnapshot(fixture *privateSdkBrowsingFixture) (privateSdkBrowsingReadinessSnapshot, bool) {
	before := fixture.device.GetWindowStatus()
	exits := fixture.device.GetExits()
	locations := fixture.device.GetConnectedProviderLocations()
	after := fixture.device.GetWindowStatus()
	return privateSdkProductionReadinessSnapshotFrom(exits, before, after, locations, &fixture.providers)
}

// The original setup context remains authoritative before and after a ready
// read; deterministic controls supply barrier ticks instead of timer guesses.
func privateSdkProductionAwaitReadiness(ctx context.Context, tick <-chan time.Time, read func() (privateSdkBrowsingReadinessSnapshot, bool)) (privateSdkBrowsingReadinessSnapshot, error) {
	var snapshot privateSdkBrowsingReadinessSnapshot
	for {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		var generationStable bool
		snapshot, generationStable = read()
		ready, err := privateSdkProductionReadiness(snapshot)
		if err != nil {
			return snapshot, err
		}
		if ready && generationStable {
			return snapshot, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return snapshot, ctx.Err()
		case <-tick:
		}
	}
}

// Local channel ordinals are a separate namespace from remote socket-provider
// ordinals. Public APIs do not expose a direct join between those identities.
type privateSdkProductionRequestObservation struct {
	Attempts        uint32 `json:"attempts"`
	SocketProviders uint8  `json:"socket_provider_mask"`
	PublicChannels  uint64 `json:"public_channel_mask,string"`
	ProvenChannels  uint64 `json:"public_proven_channel_mask,string"`
}

// HTTP-boundary observations retain fixed arrays and at most 64 synthetic
// channel identities. No observer changes selection or keeps payload buffers.
type privateSdkProductionReadinessMode struct {
	midSession                 *privateSdkMidSession
	now                        func() time.Time
	started                    time.Time
	readyElapsed               time.Duration
	navigationElapsed          time.Duration
	firstByteElapsed           atomic.Int64
	ready                      privateSdkBrowsingReadinessSnapshot
	navigation                 privateSdkBrowsingReadinessSnapshot
	end                        privateSdkBrowsingReadinessSnapshot
	readyGenerationStable      bool
	navigationGenerationStable bool
	endGenerationStable        bool
	mixedGenerationReads       int
	stateLock                  sync.Mutex
	requests                   [nativeBrowsingPageCount][nativeBrowsingAssetCount + 1]privateSdkProductionRequestObservation
	channelOrdinals            map[string]int
	attempts                   int
	missingPublic              int
	invalidRequests            int
	overflow                   int
	finished                   bool
}

// The clock starts before SetDestination, never after setup or first traffic.
func newPrivateSdkProductionReadinessMode() *privateSdkProductionReadinessMode {
	mode := &privateSdkProductionReadinessMode{now: time.Now, channelOrdinals: map[string]int{}}
	mode.firstByteElapsed.Store(-1)
	return mode
}

// All six fixture providers already exist, as in the strict root. Their
// counters include consumer connection warmup instead of rebasing it away.
func (self *privateSdkProductionReadinessMode) captureStart(fixture *privateSdkBrowsingFixture) error {
	if fixture.defaultContracts == nil || !self.started.IsZero() {
		return errors.New("production-readiness constructor changed its nil-admission/default-credit scope")
	}
	if err := fixture.captureProviderBaselines(false); err != nil {
		return err
	}
	for index := range fixture.providerBaselineScope {
		fixture.providerBaselineScope[index] = "before_consumer_destination"
	}
	self.started = self.now()
	return nil
}

// Public views are separate pull snapshots. Incomplete publication waits under
// the original setup deadline; remote ownership mismatches refuse immediately.
func (self *privateSdkProductionReadinessMode) wait(fixture *privateSdkBrowsingFixture) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	snapshot, err := privateSdkProductionAwaitReadiness(fixture.setupCtx, ticker.C, func() (privateSdkBrowsingReadinessSnapshot, bool) {
		snapshot, stable := privateSdkProductionReadinessSnapshot(fixture)
		self.readyGenerationStable = stable
		if !stable {
			self.mixedGenerationReads++
		}
		return snapshot, stable
	})
	self.ready, self.readyElapsed = snapshot, self.now().Sub(self.started)
	if err != nil {
		return fmt.Errorf("SDK_PRODUCTION_READINESS_PREMISE: %s: %w", snapshot, err)
	}
	fixture.report.ReadyQuality, fixture.report.ReadySpeed, fixture.report.ReadyProven, fixture.report.ReadyDistinctExits = snapshot.Quality, snapshot.Speed, snapshot.Proven, snapshot.DistinctOwned
	fixture.report.ReadinessSnapshot = &snapshot
	return nil
}

// No synthetic TCP question, HTTP warmup, qualification setter or selection
// override runs here. Ordinary application traffic is the first owned request.
func (self *privateSdkProductionReadinessMode) prepareNavigation(ctx context.Context, fixture *privateSdkBrowsingFixture) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fixture.setupCtx.Err(); err != nil {
		return err
	}
	if self.started.IsZero() {
		return errors.New("production-readiness navigation lost its measured constructor")
	}
	self.navigation, self.navigationGenerationStable = privateSdkProductionReadinessSnapshot(fixture)
	self.navigationElapsed = self.now().Sub(self.started)
	fixture.cancelSetup()
	return nil
}

// Only local window identities are joined here: a destination binding must
// still have a positive flow on the same live local channel in the exit view.
func privateSdkProductionBindingIds(exits *sdk.ExitList, bindings *sdk.DestinationExitList, destination string) ([]string, map[string]bool) {
	ids, proven := []string{}, map[string]bool{}
	if exits == nil || bindings == nil {
		return ids, proven
	}
	seen := map[string]bool{}
	for index := 0; index < bindings.Len(); index++ {
		binding := bindings.Get(index)
		if binding == nil || binding.DestinationIp != destination || binding.ClientId == nil || binding.FlowCount <= 0 {
			continue
		}
		id := binding.ClientId.String()
		for exitIndex := 0; exitIndex < exits.Len(); exitIndex++ {
			exit := exits.Get(exitIndex)
			if exit != nil && exit.ClientId != nil && exit.ClientId.String() == id && !exit.Done && 0 < exit.FlowCount {
				if !seen[id] {
					ids = append(ids, id)
					seen[id] = true
				}
				proven[id] = exit.Proven
			}
		}
	}
	return ids, proven
}

// The provider ordinal comes from the actual successful NAT socket's source
// mapping. Public destination bindings remain independent readout evidence.
func (self *privateSdkProductionReadinessMode) observeRequest(fixture *privateSdkBrowsingFixture, provider int, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	page, asset := -1, -1
	if len(parts) == 2 && parts[0] == "page" {
		if parsed, err := strconv.Atoi(parts[1]); err == nil {
			page, asset = parsed, 0
		}
	} else if len(parts) == 3 && parts[0] == "asset" {
		var err error
		page, err = strconv.Atoi(parts[1])
		if err == nil {
			asset, err = strconv.Atoi(parts[2])
			if err == nil {
				asset++
			}
		}
		if err != nil {
			page, asset = -1, -1
		}
	}
	origin := -1
	for index := 0; index < nativeBrowsingOriginCount; index++ {
		if request.Host == fmt.Sprintf("origin%d.test", index) {
			origin = index
		}
	}
	if self.midSession != nil && self.midSession.cold && page == privateSdkMidPage && asset == 0 && request.Host == "origin4.test" {
		origin = nativeBrowsingOriginCount
	}
	if page < 0 || nativeBrowsingPageCount <= page || asset < 0 || nativeBrowsingAssetCount < asset || provider < 0 || 6 <= provider || origin < 0 {
		self.stateLock.Lock()
		self.invalidRequests++
		self.stateLock.Unlock()
		return
	}
	self.stateLock.Lock()
	self.attempts++
	if 4096 < self.attempts {
		self.overflow++
		self.stateLock.Unlock()
		return
	}
	self.stateLock.Unlock()
	ids, proven := privateSdkProductionBindingIds(fixture.device.GetExits(), fixture.device.GetDestinationExits(), fmt.Sprintf("203.0.113.%d", 10+origin))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	row := &self.requests[page][asset]
	row.Attempts++
	row.SocketProviders |= uint8(1) << provider
	if len(ids) == 0 {
		self.missingPublic++
	}
	for _, id := range ids {
		ordinal, known := self.channelOrdinals[id]
		if !known {
			if 64 <= len(self.channelOrdinals) {
				self.overflow++
				continue
			}
			ordinal = len(self.channelOrdinals)
			self.channelOrdinals[id] = ordinal
		}
		row.PublicChannels |= uint64(1) << ordinal
		if proven[id] {
			row.ProvenChannels |= uint64(1) << ordinal
		}
	}
}

// Composition through httptrace preserves the workload's existing traces and
// records the first real response byte, including every prior warmup interval.
func (self *privateSdkProductionReadinessMode) requestTrace(_ context.Context, target string) *httptrace.ClientTrace {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Path != "/page/0" {
		return nil
	}
	return &httptrace.ClientTrace{GotFirstResponseByte: func() {
		self.firstByteElapsed.CompareAndSwap(-1, int64(self.now().Sub(self.started)))
	}}
}

// This final view is captured before fixture shutdown; it is not retroactive
// authority for the earlier readiness or per-request selection observations.
func (self *privateSdkProductionReadinessMode) captureEnd(fixture *privateSdkBrowsingFixture) {
	self.end, self.endGenerationStable = privateSdkProductionReadinessSnapshot(fixture)
}

// Public signed grants and joined usage remain mandatory. Exact same-sequence
// debt and expiry causes are unavailable without the removed private bridge.
func (self *privateSdkProductionReadinessMode) finishOpening(t testing.TB, mode *privateSdkDefaultContractMode, observation *nativeBrowsingObservation) {
	t.Helper()
	mode.close()
	mode.stateLock.Lock()
	rows, dropped, overflows := append([]privateSdkDefaultContractRow(nil), mode.rows...), mode.dropped, mode.overflows
	mode.stateLock.Unlock()
	witness := privateSdkDefaultContractWitness(rows, dropped, overflows)
	report := struct {
		Version               string                         `json:"version"`
		Status                string                         `json:"status"`
		OpeningRolloverOwners int                            `json:"public_opening_keys"`
		Pages                 int                            `json:"completed_pages"`
		Dropped               int                            `json:"dropped"`
		CoreOverflows         int                            `json:"core_overflows"`
		Rows                  []privateSdkDefaultContractRow `json:"rows"`
	}{Version: "production-readiness-default-opening-v1", Status: witness.Status, OpeningRolloverOwners: witness.Owners, Pages: observation.CompletedNavigations, Dropped: dropped, CoreOverflows: witness.Overflows, Rows: rows}
	encoded, err := json.Marshal(report)
	if err != nil || 256*1024 < len(encoded) {
		t.Error("production-readiness opening report exceeded its bound")
		return
	}
	t.Logf("SDK_PRODUCTION_READINESS_OPENING_V1 %s", encoded)
	t.Log("SDK_PUBLIC_CONTRACT_CAUSALITY_UNAVAILABLE: exact Transfer sequence, opening debt and expiry cause are not exposed by the public grant/usage witness")
	if witness.Status != "EXERCISED_PUBLIC_PRODUCTION_OPENING_AND_USAGE" {
		t.Errorf("%s: public one-MiB signed opening grant, positive joined usage and distinct signed successor grant not established", witness.Status)
	}
}

// Count every page and asset observation without requiring an arbitrary spread
// across providers. Unused warming peers are reported, never forced into work.
func (self *privateSdkProductionReadinessMode) finish(t testing.TB, fixture *privateSdkBrowsingFixture, observation *nativeBrowsingObservation) {
	t.Helper()
	self.stateLock.Lock()
	requests, attempts, missing, invalid, overflow := self.requests, self.attempts, self.missingPublic, self.invalidRequests, self.overflow
	channels := len(self.channelOrdinals)
	self.finished = true
	self.stateLock.Unlock()
	html, assets := 0, 0
	var bodyMask, dialMask uint8
	for page := range requests {
		for asset, row := range requests[page] {
			bodyMask |= row.SocketProviders
			if row.Attempts != 0 && row.SocketProviders != 0 && row.PublicChannels != 0 {
				if asset == 0 {
					html++
				} else {
					assets++
				}
			}
		}
	}
	for index, provider := range fixture.report.Providers {
		if provider != nil && provider.OwnedTcpDials > 0 {
			dialMask |= uint8(1) << index
		}
	}
	firstByte := self.firstByteElapsed.Load()
	complete := observation.CompletedNavigations == nativeBrowsingPageCount && observation.OverlapProofs == nativeBrowsingPageCount-1 && html == nativeBrowsingPageCount && assets == nativeBrowsingPageCount*nativeBrowsingAssetCount && bodyMask != 0 && bodyMask & ^dialMask == 0 && missing == 0 && invalid == 0 && overflow == 0 && firstByte >= 0
	report := struct {
		Version                    string                                                                                        `json:"version"`
		ReadinessProtocol          string                                                                                        `json:"readiness_protocol"`
		Ready                      privateSdkBrowsingReadinessSnapshot                                                           `json:"public_ready"`
		Navigation                 privateSdkBrowsingReadinessSnapshot                                                           `json:"before_navigation"`
		End                        privateSdkBrowsingReadinessSnapshot                                                           `json:"before_shutdown"`
		WarmupObserved             bool                                                                                          `json:"selectable_unproven_at_public_ready"`
		ReadyGenerationStable      bool                                                                                          `json:"ready_generation_stable"`
		NavigationGenerationStable bool                                                                                          `json:"navigation_generation_stable"`
		EndGenerationStable        bool                                                                                          `json:"end_generation_stable"`
		MixedGenerationReads       int                                                                                           `json:"mixed_generation_readiness_reads"`
		PublicReadyNs              int64                                                                                         `json:"connect_to_public_ready_ns"`
		NavigationNs               int64                                                                                         `json:"connect_to_navigation_ns"`
		FirstByteNs                int64                                                                                         `json:"connect_to_first_html_byte_ns"`
		InclusiveElapsedNs         int64                                                                                         `json:"inclusive_elapsed_ns"`
		Html                       int                                                                                           `json:"html_observed"`
		Assets                     int                                                                                           `json:"assets_observed"`
		Overlap                    int                                                                                           `json:"overlap_proofs"`
		ActualDialMask             uint8                                                                                         `json:"actual_dial_provider_mask"`
		BodyMask                   uint8                                                                                         `json:"actual_body_provider_mask"`
		PublicChannels             int                                                                                           `json:"public_local_channel_count"`
		Attempts                   int                                                                                           `json:"request_attempts"`
		MissingPublic              int                                                                                           `json:"missing_public_binding_observations"`
		Invalid                    int                                                                                           `json:"invalid_requests"`
		Overflow                   int                                                                                           `json:"observer_overflow"`
		CompleteCensus             bool                                                                                          `json:"complete_observed_census"`
		Requests                   [nativeBrowsingPageCount][nativeBrowsingAssetCount + 1]privateSdkProductionRequestObservation `json:"requests"`
	}{Version: "production-readiness-default-opening-v1", Ready: self.ready, Navigation: self.navigation, End: self.end, WarmupObserved: self.ready.Proven < self.ready.Quality+self.ready.Speed, ReadyGenerationStable: self.readyGenerationStable, NavigationGenerationStable: self.navigationGenerationStable, EndGenerationStable: self.endGenerationStable, MixedGenerationReads: self.mixedGenerationReads, PublicReadyNs: int64(self.readyElapsed), NavigationNs: int64(self.navigationElapsed), FirstByteNs: firstByte, InclusiveElapsedNs: int64(self.now().Sub(self.started)), Html: html, Assets: assets, Overlap: observation.OverlapProofs, ActualDialMask: dialMask, BodyMask: bodyMask, PublicChannels: channels, Attempts: attempts, MissingPublic: missing, Invalid: invalid, Overflow: overflow, CompleteCensus: complete, Requests: requests}
	report.ReadinessProtocol = "owned-location-rows-distinct-remote-v2"
	encoded, err := json.Marshal(report)
	if err != nil || 128*1024 < len(encoded) {
		t.Error("production-readiness observation exceeded its bound")
		return
	}
	t.Logf("SDK_PRODUCTION_READINESS_V1 %s", encoded)
	if !complete {
		t.Errorf("production-readiness census incomplete: html=%d assets=%d overlaps=%d missing_public=%d invalid=%d overflow=%d", html, assets, observation.OverlapProofs, missing, invalid, overflow)
	}
	// The original setup, application and per-navigation failure bounds remain
	// authoritative. Inclusive warmup is reported, never reset or hidden behind
	// a newly invented connection-start deadline.
}

// Setup failures still expose elapsed warmup and the last readiness sample;
// zero pages cannot disappear behind the absence of a browser report.
func (self *privateSdkProductionReadinessMode) dumpIfUnfinished(t testing.TB) {
	self.stateLock.Lock()
	finished := self.finished
	self.stateLock.Unlock()
	if finished {
		return
	}
	elapsed := time.Duration(0)
	if !self.started.IsZero() {
		elapsed = self.now().Sub(self.started)
	}
	t.Logf("SDK_PRODUCTION_READINESS_INCOMPLETE elapsed_ns=%d generation_stable=%t mixed_generation_reads=%d last_public_readiness=%s", elapsed, self.readyGenerationStable, self.mixedGenerationReads, self.ready)
}

// The complete original inner-H1 workload remains mandatory; only this new
// root selects production readiness instead of strict per-provider warmup.
func TestPrivateFullTunH1Sdk32ProductionReadinessContractOpening100Pages(t *testing.T) {
	defaults := clientconnect.DefaultContractManagerSettings()
	if defaults.InitialContractTransferByteCount != 1024*1024 || defaults.InitialNetworkPeerContractTransferByteCount != 1024*1024 {
		t.Fatal("production opening-credit premise is no longer one MiB")
	}
	mode, readiness := newPrivateSdkDefaultContractMode(), newPrivateSdkProductionReadinessMode()
	defer mode.close()
	defer readiness.dumpIfUnfinished(t)
	testPrivateSdkProductionReadinessBrowsing(t, mode, readiness)
}

// This reproduces the old all-Proven gate with actual public-view types, while
// the new gate accepts exactly the same published state without marking peers.
func TestPrivateSdkProductionReadinessKeepsStrictSixQualificationDistinct(t *testing.T) {
	exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
	for index := 3; index < exits.Len(); index++ {
		exits.Get(index).Proven = false
	}
	snapshot := privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers)
	if ready, err := privateSdkBrowsingReadiness(snapshot, true); ready || err != nil {
		t.Fatal("strict-six qualification was weakened", ready, err)
	}
	if ready, err := privateSdkProductionReadiness(snapshot); !ready || err != nil {
		t.Fatal("publicly ready warming state was rejected", ready, err)
	}
	for index := 0; index < exits.Len(); index++ {
		exits.Get(index).Proven = false
	}
	snapshot = privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers)
	if ready, err := privateSdkProductionReadiness(snapshot); !ready || err != nil {
		t.Fatal("unproven was treated as failure", ready, err)
	}
}

// Six distinct local channels may name five or one owned remote providers.
// Preserve raw row counts without inventing six remote alternatives.
func TestPrivateSdkProductionReadinessAcceptsDuplicateOwnedRows(t *testing.T) {
	for _, distinct := range []int{5, 1} {
		exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
		if distinct == 5 {
			locations.Get(4).ClientId = locations.Get(0).ClientId
		} else {
			for index := 1; index < locations.Len(); index++ {
				locations.Get(index).ClientId = locations.Get(0).ClientId
			}
		}
		snapshot, stable := privateSdkProductionReadinessSnapshotFrom(exits, status, status, locations, providers)
		if !stable || snapshot.Exits != 6 || snapshot.LocalDistinct != 6 || snapshot.Locations != 6 || snapshot.DistinctOwned != distinct || bits.OnesCount8(snapshot.OwnedMask) != distinct {
			t.Fatalf("public identity namespaces were collapsed: distinct=%d snapshot=%s stable=%t", distinct, snapshot, stable)
		}
		if ready, err := privateSdkProductionReadiness(snapshot); !ready || err != nil {
			t.Fatalf("owned duplicate rows blocked ordinary readiness: distinct=%d ready=%t err=%v", distinct, ready, err)
		}
		reads := 0
		actual, err := privateSdkProductionAwaitReadiness(t.Context(), nil, func() (privateSdkBrowsingReadinessSnapshot, bool) {
			reads++
			return privateSdkProductionReadinessSnapshotFrom(exits, status, status, locations, providers)
		})
		if err != nil || reads != 1 || actual != snapshot {
			t.Fatalf("ordinary publication did not retain the owned row evidence: distinct=%d reads=%d err=%v", distinct, reads, err)
		}
		if ready, err := privateSdkBrowsingReadiness(snapshot, true); ready || err != nil {
			t.Fatal("ordinary readiness changed the strict six-provider premise", ready, err)
		}
		alternativesErr := privateSdkMidReadyAlternatives(snapshot, stable)
		if distinct == 1 && alternativesErr == nil || distinct == 5 && alternativesErr != nil {
			t.Fatalf("strict alternatives did not count unique owned providers: distinct=%d err=%v", distinct, alternativesErr)
		}
	}
}

// Deduplicating owned remote rows does not grant ownership to missing or
// foreign identities, including a malformed row after valid duplicates.
func TestPrivateSdkProductionReadinessRejectsForeignAndMissingOwnedRows(t *testing.T) {
	for _, name := range []string{"missing-local", "missing-remote", "nil-remote", "foreign-remote"} {
		exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
		locations.Get(4).ClientId = locations.Get(0).ClientId
		switch name {
		case "missing-local":
			exits.Get(0).ClientId = nil
		case "missing-remote":
			locations.Get(0).ClientId = nil
		case "nil-remote":
			locations.Add(nil)
		case "foreign-remote":
			locations.Get(0).ClientId = sdk.NewId()
		}
		snapshot := privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers)
		ready, err := privateSdkProductionReadiness(snapshot)
		if ready || (err != nil) != (name == "foreign-remote") {
			t.Fatalf("%s identity was accepted or misclassified: ready=%t err=%v snapshot=%s", name, ready, err, snapshot)
		}
		if privateSdkMidReadyAlternatives(snapshot, true) == nil {
			t.Fatalf("strict arming accepted %s identity", name)
		}
	}
}

// Remote provider projection and local channel counts need not form a public
// one-to-one mapping. Actual request ownership supplies the stronger evidence.
func TestPrivateSdkProductionReadinessDoesNotInventAnIdentityCountJoin(t *testing.T) {
	snapshot := privateSdkBrowsingReadinessSnapshot{Quality: 3, Speed: 1, Proven: 0, Exits: 4, LocalDistinct: 4, MinSatisfied: true, Locations: 3, DistinctOwned: 3, OwnedMask: 7}
	if ready, err := privateSdkProductionReadiness(snapshot); !ready || err != nil {
		t.Fatal("public readiness was replaced by a local/remote count equality", ready, err)
	}
	snapshot.Unowned = 1
	if ready, err := privateSdkProductionReadiness(snapshot); ready || err == nil {
		t.Fatal("foreign provider projection was accepted", ready, err)
	}
}

// The public boolean, not a local count, releases the actual waiting helper.
func TestPrivateSdkProductionReadinessWaitsForPublicMinimum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
		status.MinSatisfied = false
		ctx, cancel := context.WithCancel(t.Context())
		tick := make(chan time.Time)
		done, joined := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(joined)
			_, err := privateSdkProductionAwaitReadiness(ctx, tick, func() (privateSdkBrowsingReadinessSnapshot, bool) {
				return privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers), true
			})
			done <- err
		}()
		defer func() { cancel(); <-joined }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("counts released an unsatisfied public minimum")
		default:
		}
		status.MinSatisfied = true
		exits.Get(2).Proven = false
		tick <- time.Now()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// An empty remote projection is unknown ownership, even with a satisfied
// public minimum. Publication, not elapsed time, releases the same owner.
func TestPrivateSdkProductionReadinessWaitsForOwnedProjection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exits, status, complete, providers := privateSdkBrowsingReadinessViews(t)
		locations := sdk.NewConnectedProviderLocationList()
		ctx, cancel := context.WithCancel(t.Context())
		tick := make(chan time.Time)
		done, joined := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(joined)
			_, err := privateSdkProductionAwaitReadiness(ctx, tick, func() (privateSdkBrowsingReadinessSnapshot, bool) {
				return privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers), true
			})
			done <- err
		}()
		defer func() { cancel(); <-joined }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("empty owned projection released browsing")
		default:
		}
		locations = complete
		for index := 0; index < exits.Len(); index++ {
			exits.Get(index).Proven = false
		}
		tick <- time.Now()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// Cancellation during a ready read cannot publish a success after the original
// owner expired; this is forced directly, without a negative timeout.
func TestPrivateSdkProductionReadinessRejectsCancellationAtPublication(t *testing.T) {
	exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := privateSdkProductionAwaitReadiness(ctx, nil, func() (privateSdkBrowsingReadinessSnapshot, bool) {
		cancel()
		return privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers), true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled ready publication succeeded", err)
	}
}

// Positive public flow evidence requires the local channel join. A different
// remote identity with the same numeric counts cannot substitute for it.
func TestPrivateSdkProductionReadinessBindingsKeepIdentityNamespacesSeparate(t *testing.T) {
	local, remote := sdk.NewId(), sdk.NewId()
	exits, bindings := sdk.NewExitList(), sdk.NewDestinationExitList()
	exits.Add(&sdk.Exit{ClientId: local, WindowType: "quality", FlowCount: 1, Proven: false})
	bindings.Add(&sdk.DestinationExit{DestinationIp: "203.0.113.10", ClientId: remote, FlowCount: 1})
	if ids, _ := privateSdkProductionBindingIds(exits, bindings, "203.0.113.10"); len(ids) != 0 {
		t.Fatal("remote identity guessed to be local channel")
	}
	bindings.Get(0).ClientId = local
	ids, proven := privateSdkProductionBindingIds(exits, bindings, "203.0.113.10")
	if len(ids) != 1 || ids[0] != local.String() || proven[ids[0]] {
		t.Fatal("actual unproven live channel observation lost", ids, proven)
	}
	exits.Get(0).Done = true
	if ids, _ := privateSdkProductionBindingIds(exits, bindings, "203.0.113.10"); len(ids) != 0 {
		t.Fatal("done channel counted as live selection")
	}
}

// Clock injection changes only this observation control. Production always
// records the real connect-start clock and never replaces it at navigation.
func TestPrivateSdkProductionReadinessFirstByteIncludesWarmup(t *testing.T) {
	mode := newPrivateSdkProductionReadinessMode()
	mode.started = time.Unix(100, 0)
	mode.readyElapsed, mode.navigationElapsed = 3*time.Second, 3500*time.Millisecond
	mode.now = func() time.Time { return mode.started.Add(4 * time.Second) }
	trace := mode.requestTrace(t.Context(), "https://origin.example/page/0")
	trace.GotFirstResponseByte()
	if got := time.Duration(mode.firstByteElapsed.Load()); got != 4*time.Second {
		t.Fatal("warmup was excluded from first-byte observation", got)
	}
	mode.now = func() time.Time { return mode.started.Add(7 * time.Second) }
	trace.GotFirstResponseByte()
	if got := time.Duration(mode.firstByteElapsed.Load()); got != 4*time.Second {
		t.Fatal("first-byte observation was replaced by a retry", got)
	}
}

// Missing ownership evidence cannot become success merely because the public
// minimum is satisfied. Each malformed read remains waiting, without a timer.
func TestPrivateSdkProductionReadinessRejectsIncompleteViews(t *testing.T) {
	exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
	complete := privateSdkBrowsingReadinessSnapshotFrom(exits, status, locations, providers)
	cases := []struct {
		name   string
		change func(*privateSdkBrowsingReadinessSnapshot)
	}{
		{name: "minimum", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.MinSatisfied = false }},
		{name: "no selectable channel", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.Quality, view.Speed = 0, 0 }},
		{name: "empty local view", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.Exits, view.LocalDistinct = 0, 0 }},
		{name: "duplicate local", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.LocalDistinct-- }},
		{name: "missing local", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.MissingLocalIdentities++ }},
		{name: "empty remote view", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.Locations, view.DistinctOwned = 0, 0 }},
		{name: "inconsistent remote mask", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.DistinctOwned-- }},
		{name: "missing remote", change: func(view *privateSdkBrowsingReadinessSnapshot) { view.MissingIdentities++ }},
	}
	for _, testCase := range cases {
		snapshot := complete
		testCase.change(&snapshot)
		if ready, err := privateSdkProductionReadiness(snapshot); ready || err != nil {
			t.Errorf("%s malformed view classification: ready=%t err=%v", testCase.name, ready, err)
		}
	}
}

// Already ended owners never issue a read or revive themselves from a stale
// public-ready projection; both cancellation paths retain their exact error.
func TestPrivateSdkProductionReadinessDoesNotOutliveSetupOwner(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		want := error(context.Canceled)
		cancel()
		if deadline {
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		}
		reads := 0
		_, err := privateSdkProductionAwaitReadiness(ctx, nil, func() (privateSdkBrowsingReadinessSnapshot, bool) {
			reads++
			return privateSdkBrowsingReadinessSnapshot{Quality: 1, Exits: 1, LocalDistinct: 1, MinSatisfied: true, Locations: 1, DistinctOwned: 1, OwnedMask: 1}, true
		})
		cancel()
		if !errors.Is(err, want) || reads != 0 {
			t.Errorf("ended owner read or returned success: reads=%d err=%v want=%v", reads, err, want)
		}
	}
}

// A MinSatisfied result from another connection generation cannot qualify the
// intervening identity reads. The next coherent generation can still proceed.
func TestPrivateSdkProductionReadinessRejectsMixedGenerationPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
		before, after := *status, *status
		after.ConnectionGeneration++
		ctx, cancel := context.WithCancel(t.Context())
		tick := make(chan time.Time)
		done, joined := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(joined)
			_, err := privateSdkProductionAwaitReadiness(ctx, tick, func() (privateSdkBrowsingReadinessSnapshot, bool) {
				return privateSdkProductionReadinessSnapshotFrom(exits, &before, &after, locations, providers)
			})
			done <- err
		}()
		defer func() { cancel(); <-joined }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("mixed generation released readiness")
		default:
		}
		snapshot, stable := privateSdkProductionReadinessSnapshotFrom(exits, &before, &after, locations, providers)
		if stable || snapshot.MinSatisfied != after.MinSatisfied {
			t.Fatal("mixed generation was accepted or raw public minimum was altered", stable, snapshot)
		}
		before = after
		for index := 0; index < exits.Len(); index++ {
			exits.Get(index).Proven = false
		}
		tick <- time.Now()
		if err := <-done; err != nil {
			t.Fatal("coherent warming generation was rejected", err)
		}
	})
}
