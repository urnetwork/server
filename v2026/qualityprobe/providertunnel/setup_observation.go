package providertunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// SetupCallTiming describes completed wrapper calls. Calls can overlap and
// finish during cleanup; their sums are not a partition of probe wall time.
type SetupCallTiming struct {
	Count    uint64
	Duration time.Duration
}

const (
	SetupAdmissionMatched = iota
	SetupAdmissionMissing
	SetupAdmissionAmbiguous
	SetupAdmissionOverflow
	SetupAdmissionInvalid
	SetupAdmissionIncomplete
	SetupAdmissionNoConstructor
	SetupAdmissionCoverageCount
)

// SetupTiming is an identity-free, fixed-size aggregate of closed tunnels.
// Calls indexes credential_acquire/registered_constructor, then ok/canceled/error.
// The constructor waits for processed provide-secret registration and, only
// when configured, client-key registration. It does not isolate either RPC.
// RouteStages partitions credential entry to source Added only for a tunnel
// with exactly one credential and one constructor for the same unique ID:
// credential_acquire, credential_to_constructor, registered_constructor,
// registration_to_admission. EvaluationToAdmission overlaps the final stage;
// it includes send/contract admission and is not ping RTT. Missing events do
// not contribute zero measurements. Pending calls are counted at freeze.
type SetupTiming struct {
	Calls                 [2][3]SetupCallTiming
	Pending               [2]uint64
	Tunnels               [SetupAdmissionCoverageCount]uint64
	CloseErrors           uint64
	RouteStages           [4]time.Duration
	EvaluationCount       uint64
	EvaluationToAdmission time.Duration
}

// SetupObservations belongs to one provider probe, including its replacement
// tunnels. Each tunnel adds once after existing terminal Close joins. Read only
// after the probe joins current and retired tunnels. Never share across probes.
type SetupObservations struct {
	mu    sync.Mutex
	value SetupTiming
}

func (self *SetupObservations) Snapshot() SetupTiming {
	if self == nil {
		return SetupTiming{}
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.value
}

func (self *SetupObservations) add(value SetupTiming) {
	self.mu.Lock()
	defer self.mu.Unlock()
	for phase := range value.Calls {
		for outcome, call := range value.Calls[phase] {
			self.value.Calls[phase][outcome].Count += call.Count
			self.value.Calls[phase][outcome].Duration += call.Duration
		}
		self.value.Pending[phase] += value.Pending[phase]
	}
	for i, count := range value.Tunnels {
		self.value.Tunnels[i] += count
	}
	for i, duration := range value.RouteStages {
		self.value.RouteStages[i] += duration
	}
	self.value.CloseErrors += value.CloseErrors
	self.value.EvaluationCount += value.EvaluationCount
	self.value.EvaluationToAdmission += value.EvaluationToAdmission
}

const providerSetupIdentityLimit = 8

// Lifetime tombstones: never evict an identity and make its reuse look fresh.
// No credentials, provider identities, or local IDs leave this owner.
type providerSetupEntry struct {
	id                                                                connect.Id
	credentials, constructors                                         uint64
	token                                                             uint64
	registered                                                        bool
	invalid                                                           bool
	credentialStart, credentialEnd, constructorStart, registrationEnd time.Time
	evaluation, added                                                 time.Time
}

type providerSetupCall struct {
	start time.Time
	token uint64
	entry int
}

type providerSetupState struct {
	owner                     *SetupObservations
	provider                  connect.Id
	mu                        sync.Mutex
	entries                   [providerSetupIdentityLimit]providerSetupEntry
	entryCount                int
	nextToken                 uint64
	started                   [2]uint64
	value                     SetupTiming
	overflow, invalid, closed bool
	closeStarted              time.Time
}

func newProviderSetupState(owner *SetupObservations, provider connect.Id) *providerSetupState {
	if owner == nil {
		return nil
	}
	return &providerSetupState{owner: owner, provider: provider}
}

// Called only for actual credential returns or constructor entries. A foreign
// monitor event must never allocate a record, including an overflow record.
func (self *providerSetupState) entry(id connect.Id) int {
	for i := 0; i < self.entryCount; i++ {
		if self.entries[i].id == id {
			return i
		}
	}
	if self.entryCount == len(self.entries) {
		self.overflow = true
		return -1
	}
	i := self.entryCount
	self.entryCount++
	self.entries[i].id = id
	return i
}

func (self *providerSetupState) beginCredential() providerSetupCall {
	if self == nil {
		return providerSetupCall{}
	}
	now := time.Now()
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.closed {
		return providerSetupCall{}
	}
	self.started[0]++
	self.value.Pending[0]++
	return providerSetupCall{start: now, entry: -1}
}

func (self *providerSetupState) beginConstructor(args *connect.MultiClientGeneratorClientArgs) providerSetupCall {
	if self == nil {
		return providerSetupCall{}
	}
	now := time.Now()
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.closed {
		return providerSetupCall{}
	}
	self.started[1]++
	self.value.Pending[1]++
	self.nextToken++
	call := providerSetupCall{start: now, token: self.nextToken, entry: -1}
	if args == nil || args.ClientId == (connect.Id{}) {
		self.invalid = true
		return call
	}
	call.entry = self.entry(args.ClientId)
	if call.entry >= 0 {
		entry := &self.entries[call.entry]
		// Reuse is permanent, even if the older call returns later or Added
		// was observed before this replacement constructor began.
		entry.constructors++
		if entry.constructors == 1 {
			entry.token = call.token
			entry.constructorStart = now
		}
	}
	return call
}

func setupResult(ok bool, err error) int {
	if ok && err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 1
	}
	return 2
}

func (self *providerSetupState) finishCredential(call providerSetupCall, args *connect.MultiClientGeneratorClientArgs, err error, destination *connect.MultiHopId) {
	if self == nil || call.start.IsZero() {
		return
	}
	now := time.Now()
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.closed {
		return
	}
	self.value.Pending[0]--
	validArgs := args != nil && args.ClientId != (connect.Id{}) && args.ClientAuth != nil
	total := &self.value.Calls[0][setupResult(validArgs, err)]
	total.Count++
	total.Duration += now.Sub(call.start)
	if !validArgs || err != nil {
		return
	}
	i := self.entry(args.ClientId)
	if i < 0 {
		return
	}
	entry := &self.entries[i]
	entry.credentials++
	if destination != nil && (destination.Len() != 1 || destination.Tail() != self.provider) {
		entry.invalid = true
	}
	if entry.credentials == 1 {
		entry.credentialStart, entry.credentialEnd = call.start, now
	}
}

func (self *providerSetupState) finishConstructor(call providerSetupCall, client *connect.Client, err error) {
	if self == nil || call.start.IsZero() {
		return
	}
	now := time.Now()
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.closed {
		return
	}
	self.value.Pending[1]--
	ok := client != nil && err == nil
	if call.entry >= 0 && ok && client.ClientId() != self.entries[call.entry].id {
		ok = false
		self.entries[call.entry].invalid = true
	}
	total := &self.value.Calls[1][setupResult(ok, err)]
	total.Count++
	total.Duration += now.Sub(call.start)
	if call.entry >= 0 {
		entry := &self.entries[call.entry]
		if entry.constructors == 1 && entry.token == call.token {
			entry.registered = ok
			entry.registrationEnd = now
		}
	}
}

func (self *providerSetupState) events(events map[connect.Id]*connect.ProviderEvent) {
	if self == nil {
		return
	}
	now := time.Now()
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.closed {
		return
	}
	// The monitor owns its map. Work here stays bounded even for a foreign
	// snapshot; it cannot expand this diagnostic's identity table.
	if len(events) > providerSetupIdentityLimit {
		self.invalid = true
		return
	}
	for id, event := range events {
		if event == nil {
			self.invalid = true
			continue
		}
		if event.State != connect.ProviderStateInEvaluation && event.State != connect.ProviderStateAdded {
			continue
		}
		var entry *providerSetupEntry
		for i := 0; i < self.entryCount; i++ {
			if self.entries[i].id == id {
				entry = &self.entries[i]
				break
			}
		}
		if entry == nil {
			self.invalid = true
			continue
		}
		if event.ClientId != id || event.EgressClientId != self.provider || entry.constructors == 0 ||
			event.EventTime.IsZero() || event.EventTime.After(now) || event.EventTime.Before(entry.constructorStart) {
			entry.invalid = true
			continue
		}
		// Teardown events are outside the probe's setup interval. The terminal
		// freeze later also excludes callbacks that were still in flight.
		if !self.closeStarted.IsZero() && event.EventTime.After(self.closeStarted) {
			continue
		}
		previous := &entry.added
		if event.State == connect.ProviderStateInEvaluation {
			previous = &entry.evaluation
		}
		if previous.IsZero() || event.EventTime.Before(*previous) {
			*previous = event.EventTime
		}
	}
}

// Snapshot current active source timestamps before unsubscribe; do not wait
// for a monitor callback. A coalesced-away Added without an active snapshot
// stays missing. Callbacks already executing are fenced by finish's lock.
func (self *providerSetupState) watch(monitor connect.MultiClientMonitor) func() {
	if self == nil {
		return nil
	}
	unwatch := monitor.AddMonitorEventCallback(func(_ *connect.WindowExpandEvent, events map[connect.Id]*connect.ProviderEvent, _ bool) {
		self.events(events)
	})
	self.events(monitor.ProviderEvents())
	return func() {
		self.mu.Lock()
		self.closeStarted = time.Now()
		self.mu.Unlock()
		self.events(monitor.ProviderEvents())
		unwatch()
	}
}

func (self *providerSetupState) admission() (int, *providerSetupEntry) {
	if self.value.Pending != [2]uint64{} || self.closeStarted.IsZero() {
		return SetupAdmissionIncomplete, nil
	}
	if self.overflow {
		return SetupAdmissionOverflow, nil
	}
	if self.invalid {
		return SetupAdmissionInvalid, nil
	}
	if self.started[1] == 0 {
		return SetupAdmissionNoConstructor, nil
	}
	if self.started != [2]uint64{1, 1} {
		return SetupAdmissionAmbiguous, nil
	}
	if self.entryCount != 1 {
		return SetupAdmissionInvalid, nil
	}
	entry := &self.entries[0]
	if entry.invalid {
		return SetupAdmissionInvalid, nil
	}
	if entry.credentials != 1 || entry.constructors != 1 || !entry.registered || entry.added.IsZero() {
		return SetupAdmissionMissing, nil
	}
	if entry.credentialEnd.Before(entry.credentialStart) || entry.constructorStart.Before(entry.credentialEnd) ||
		entry.registrationEnd.Before(entry.constructorStart) || entry.added.Before(entry.registrationEnd) || entry.added.After(self.closeStarted) {
		return SetupAdmissionInvalid, nil
	}
	if !entry.evaluation.IsZero() && (entry.evaluation.Before(entry.registrationEnd) || entry.evaluation.After(entry.added)) {
		return SetupAdmissionInvalid, nil
	}
	return SetupAdmissionMatched, entry
}

// Existing cleanup owns all waiting. A wrapper can still be doing post-return
// bookkeeping when its real generator has joined: Pending records that case,
// without adding a diagnostic wait or accepting an incomplete route interval.
func (self *providerSetupState) finish(closeErr error) {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.closed {
		self.mu.Unlock()
		return
	}
	self.closed = true
	value := self.value
	coverage, entry := self.admission()
	value.Tunnels[coverage] = 1
	if closeErr != nil {
		value.CloseErrors = 1
	}
	if entry != nil {
		value.RouteStages = [4]time.Duration{
			entry.credentialEnd.Sub(entry.credentialStart),
			entry.constructorStart.Sub(entry.credentialEnd),
			entry.registrationEnd.Sub(entry.constructorStart),
			entry.added.Sub(entry.registrationEnd),
		}
		if !entry.evaluation.IsZero() {
			value.EvaluationCount = 1
			value.EvaluationToAdmission = entry.added.Sub(entry.evaluation)
		}
	}
	self.entries = [providerSetupIdentityLimit]providerSetupEntry{}
	self.entryCount = 0
	self.mu.Unlock()
	self.owner.add(value)
}
