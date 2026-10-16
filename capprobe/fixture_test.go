// Fakes for the probe's tunnels, reads and write, on a synctest bubble's
// clock.
package capprobe

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Fakes for the probe's tunnels, reads and write. Every timed test runs in a
// synctest bubble, so the probe's own time.After and time.Now are the
// bubble's virtual clock and every wait ends at an exact instant.

// The never offset of a fakeProvider event.
const fakeNever = time.Duration(-1)

// What a synthetic provider does on the wire, as offsets from the tunnel's
// opening: when the send path accepts the tunnel's ClientHello, when the
// provider first acknowledges the pings, when a handshake completes and when
// it seals. fakeNever leaves an event out. It is read as connect's recorder
// records it: an answer or a seal implies the attempt, recorded at the answer
// when no earlier one was, and an acknowledgement records its window, if it
// carries one, in the same step.
type fakeProvider struct {
	attemptAt   time.Duration
	ackAt       time.Duration
	negotiateAt time.Duration
	sealAt      time.Duration
	postQuantum bool
	window      bool
}

// A provider that negotiates the hybrid group, seals and advertises windows.
func fakeModernProvider() fakeProvider {
	return fakeProvider{attemptAt: 500 * time.Millisecond, ackAt: time.Second, negotiateAt: 2 * time.Second, sealAt: 3 * time.Second, postQuantum: true, window: true}
}

// A provider without the end-to-end responder or windows: it acknowledges
// the pings and never answers the ClientHello.
func fakeLegacyProvider() fakeProvider {
	return fakeProvider{attemptAt: 500 * time.Millisecond, ackAt: time.Second, negotiateAt: fakeNever, sealAt: fakeNever}
}

// A provider that never acknowledges anything.
func fakeSilentProvider() fakeProvider {
	return fakeProvider{attemptAt: 500 * time.Millisecond, ackAt: fakeNever, negotiateAt: fakeNever, sealAt: fakeNever}
}

// The recorder's record elapsed after the tunnel opened at openedAt.
func (self fakeProvider) observe(openedAt time.Time, elapsed time.Duration) (connect.PeerCapabilityObservation, bool) {
	happened := func(at time.Duration) bool {
		return at != fakeNever && at <= elapsed
	}
	if !happened(self.attemptAt) && !happened(self.ackAt) && !happened(self.negotiateAt) {
		return connect.PeerCapabilityObservation{}, false
	}
	observation := connect.PeerCapabilityObservation{}
	// the first of the attempt and the answer: an answer recorded before its
	// sender recorded the hand-over is the attempt's time
	for _, at := range []time.Duration{self.attemptAt, self.negotiateAt} {
		if happened(at) && (!observation.E2eAttempted || openedAt.Add(at).Before(observation.E2eFirstAttemptTime)) {
			observation.E2eAttempted = true
			observation.E2eFirstAttemptTime = openedAt.Add(at)
		}
	}
	if happened(self.ackAt) {
		observation.AckObserved = true
		if self.window {
			observation.WindowAdvertised = true
			observation.MaxReceiveWindowByteCount = 4 * 1024 * 1024
		}
	}
	if happened(self.negotiateAt) {
		observation.E2eNegotiated = true
		observation.KeyExchange = "X25519"
		if self.postQuantum {
			observation.KeyExchange = "X25519MLKEM768"
			observation.PostQuantum = true
		}
		observation.E2eSealed = happened(self.sealAt)
	}
	return observation, true
}

// One open synthetic tunnel. Safe for concurrent use.
type fakeTunnel struct {
	provider fakeProvider
	openedAt time.Time
	lostDone chan struct{}
	closeErr error

	stateLock  sync.Mutex
	closeCount int
	onClose    func()
}

// Implements capabilityTunnel.
func (self *fakeTunnel) observation() (connect.PeerCapabilityObservation, bool) {
	return self.provider.observe(self.openedAt, time.Since(self.openedAt))
}

// Implements capabilityTunnel.
func (self *fakeTunnel) lost() <-chan struct{} {
	return self.lostDone
}

// Implements capabilityTunnel.
func (self *fakeTunnel) close() error {
	onClose := func() func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closeCount += 1
		return self.onClose
	}()
	if onClose != nil {
		onClose()
	}
	return self.closeErr
}

func (self *fakeTunnel) closes() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closeCount
}

// The tunnels of a fakeFleet: each provider's script, the tunnels opened in
// order, and how many were open at most at once. Safe for concurrent use.
type fakeFleet struct {
	stateLock     sync.Mutex
	providers     map[server.Id]fakeProvider
	openErr       map[server.Id]error
	opened        []*fakeTunnel
	openedIds     []server.Id
	open          int
	maxOpen       int
	openContexts  []context.Context
	beforeOpenFor func(providerId server.Id)
}

func newFakeFleet() *fakeFleet {
	return &fakeFleet{providers: map[server.Id]fakeProvider{}, openErr: map[server.Id]error{}}
}

// Implements openCapabilityTunnel.
func (self *fakeFleet) openTunnel(ctx context.Context, providerId server.Id) (capabilityTunnel, error) {
	before := func() func(server.Id) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.beforeOpenFor
	}()
	if before != nil {
		before(providerId)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.openContexts = append(self.openContexts, ctx)
	self.openedIds = append(self.openedIds, providerId)
	if err := self.openErr[providerId]; err != nil {
		return nil, err
	}
	provider, ok := self.providers[providerId]
	if !ok {
		provider = fakeSilentProvider()
	}
	tunnel := &fakeTunnel{provider: provider, openedAt: time.Now(), lostDone: make(chan struct{})}
	tunnel.onClose = func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.open -= 1
	}
	self.open += 1
	self.maxOpen = max(self.maxOpen, self.open)
	self.opened = append(self.opened, tunnel)
	return tunnel, nil
}

// The index-th tunnel opened.
func (self *fakeFleet) tunnel(index int) *fakeTunnel {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.opened[index]
}

func (self *fakeFleet) openedProviderIds() []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.openedIds)
}

func (self *fakeFleet) maxOpenTunnels() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.maxOpen
}

func (self *fakeFleet) openTunnels() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.open
}

// A synthetic population and evidence store standing in for the model: the
// connected providers in client id order, which are public top-level ones,
// which the egress rules admit, and the evidence recorded so far. Safe for
// concurrent use.
type fakeStore struct {
	stateLock   sync.Mutex
	clientIds   []server.Id
	notPublic   map[server.Id]bool
	ineligible  map[server.Id]bool
	evidence    map[server.Id]model.ProviderModernEvidence
	recordErr   map[server.Id]error
	pageReads   int
	readErr     error
	filterCalls [][]server.Id
	records     []server.Id
	// the providers whose reading was held back, in order
	held []server.Id
}

// count providers with sorted synthetic ids.
func newFakeStore(count int) *fakeStore {
	clientIds := make([]server.Id, count)
	for index := range clientIds {
		clientIds[index] = server.NewId()
	}
	slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return &fakeStore{
		clientIds:  clientIds,
		notPublic:  map[server.Id]bool{},
		ineligible: map[server.Id]bool{},
		evidence:   map[server.Id]model.ProviderModernEvidence{},
		recordErr:  map[server.Id]error{},
	}
}

// Implements passRunner.readPage as the page SQL reads it.
func (self *fakeStore) readPage(ctx context.Context, afterClientId server.Id, pageSize int, modernRefreshBefore time.Time, otherRefreshBefore time.Time) ([]model.ProviderCapabilityProbeCandidate, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pageReads += 1
	if self.readErr != nil {
		return nil, self.readErr
	}
	page := []model.ProviderCapabilityProbeCandidate{}
	for _, clientId := range self.clientIds {
		if clientId.Cmp(afterClientId) <= 0 {
			continue
		}
		if len(page) == pageSize {
			break
		}
		due := !self.notPublic[clientId]
		if evidence, ok := self.evidence[clientId]; ok && due {
			refreshBefore := otherRefreshBefore
			if modernOutcomeOf(evidence) == model.ProviderModernOutcomeModern {
				refreshBefore = modernRefreshBefore
			}
			due = evidence.ObservedAt.Before(refreshBefore)
		}
		page = append(page, model.ProviderCapabilityProbeCandidate{ClientId: clientId, Due: due})
	}
	return page, nil
}

// Implements passRunner.filterEligible.
func (self *fakeStore) filterEligible(ctx context.Context, clientIds []server.Id) ([]server.Id, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.filterCalls = append(self.filterCalls, slices.Clone(clientIds))
	eligible := []server.Id{}
	for _, clientId := range clientIds {
		if !self.ineligible[clientId] {
			eligible = append(eligible, clientId)
		}
	}
	return eligible, nil
}

// Implements passRunner.record, refusing what the model refuses and holding
// back what it holds back: a newer reading that is not modern, over modern
// evidence still fresh at the write, unless downgradeConfirmed.
func (self *fakeStore) record(ctx context.Context, clientId server.Id, evidence model.ProviderModernEvidence, downgradeConfirmed bool) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.recordErr[clientId]; err != nil {
		return false, err
	}
	if !evidence.Observed() {
		return false, errors.New("synthetic store refuses evidence that observed nothing")
	}
	if stored, ok := self.evidence[clientId]; ok && !downgradeConfirmed &&
		modernOutcomeOf(stored) == model.ProviderModernOutcomeModern &&
		stored.ObservedAt.After(time.Now().Add(-model.ProviderModernEvidenceMaxAge)) &&
		stored.ObservedAt.Before(evidence.ObservedAt) &&
		modernOutcomeOf(evidence) != model.ProviderModernOutcomeModern {
		self.held = append(self.held, clientId)
		return true, nil
	}
	self.evidence[clientId] = evidence
	self.records = append(self.records, clientId)
	return false, nil
}

func (self *fakeStore) recordedIds() []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.records)
}

// Settings for synthetic passes: the defaults switched on, with a small
// geometry.
func fakeSettings(concurrency int, limit int, pageSize int, maxPages int) Settings {
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Concurrency = concurrency
	settings.Limit = limit
	settings.PageSize = pageSize
	settings.MaxPagesPerPass = maxPages
	return settings
}

// A pass runner over the fakes, on the bubble's clock.
func newFakeRunner(t *testing.T, settings Settings, store *fakeStore, fleet *fakeFleet) *passRunner {
	t.Helper()
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	return &passRunner{
		settings:       settings,
		tlsTimeout:     60 * time.Second,
		readPage:       store.readPage,
		filterEligible: store.filterEligible,
		open:           fleet.openTunnel,
		record:         store.record,
		now:            time.Now,
		after:          time.After,
	}
}
