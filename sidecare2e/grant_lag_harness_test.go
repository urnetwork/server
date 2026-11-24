//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The grant-lag case's server-side parts (grant_lag_test.go), gathered in one file as the e2e conventions ask, since
// QD-1028's layout move will move what they reach:
//
//   - api's gate behind the log: the token exchange's scopes come from a gate cache of the case's own, installed as
//     api's process installs the exchange's over its gate cache (oauth.SetExchangeScopeSource over
//     oauth.NewProjectionExchangeScopeSource), whose source hands the cache each entry of the log only once it is due:
//     Lag after the source first read it, or, for an entry first read while its network is held, once the case
//     releases the network. The cache loads at every {acl_}v event as api's does, so an entry is first read within a
//     load of its landing, and an api process whose loads run late is what the exchange then serves: the entry stands
//     in every follower and in evs's and gkm's gates, while the exchange still judges a device's scopes at the
//     position before it. The world's own api gate cache, which api's approvals, dns scopes, smtp routes and edge
//     logins read, stays in step, and every sync waits on it as before; the lagging cache is in no sync, so a
//     statement's filer returns while the exchange is still behind it, as §8.9's 5 s allows it to be
//   - api's record, in front of api's handler (T3Api.SetApiHook): every token exchange api answered, with the subject's
//     client id read from the subject token's claims unverified (never the token itself, which nothing here keeps or
//     logs, I-7), the resource, the scopes asked, the answer's status, its error code and the scopes granted; and every
//     statement api took (POST /network/config/delta) with the position it answered. Each is timed as the hook saw it:
//     an exchange's start before api's handler ran and its answer after
//
// Safe for concurrent use.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/sidecarhost"
)

// The lagging cache's source over the world's own (the file's header). Safe for concurrent use.
type grantLagSource struct {
	inner sidecar.AclGateSource
	// what loads a network again once its next entry comes due: the cache's Changed, set before run starts
	changed func(networkId sidecar.Id)
	// wakes run when a new due time is set
	wake chan struct{}

	stateLock sync.Mutex
	networks  map[sidecar.Id]*grantLagNetwork
	// the earliest due time of each network's held entries that run waits on
	dueAts map[sidecar.Id]time.Time
}

// One network's entries as the source handed them out.
type grantLagNetwork struct {
	// how long an entry the source reads for the first time waits from that read
	lag time.Duration
	// whether an entry the source reads for the first time waits for the network's release
	held bool
	// each entry the source has read, by its bytes' digest
	entries map[[sha256.Size]byte]*grantLagEntry
}

// One entry the source has read.
type grantLagEntry struct {
	firstReadAt time.Time
	// when the cache may take it: zero while it waits for its network's release
	dueAt time.Time
}

func newGrantLagSource(inner sidecar.AclGateSource) *grantLagSource {
	return &grantLagSource{
		inner:    inner,
		wake:     make(chan struct{}, 1),
		networks: map[sidecar.Id]*grantLagNetwork{},
		dueAts:   map[sidecar.Id]time.Time{},
	}
}

// The network's state, made with no lag and no hold.
func (self *grantLagSource) networkWithLock(networkId sidecar.Id) *grantLagNetwork {
	network := self.networks[networkId]
	if network == nil {
		network = &grantLagNetwork{entries: map[[sha256.Size]byte]*grantLagEntry{}}
		self.networks[networkId] = network
	}
	return network
}

func (self *grantLagSource) Genesis(ctx context.Context, networkId sidecar.Id) ([]byte, error) {
	return self.inner.Genesis(ctx, networkId)
}

// The inner source's entries after after, up to the first that is not due; run loads the network again once that one
// is. Every entry of the answer counts as read now, those past the first not due included, so an entry that lands
// behind a held one takes its lag from its own landing, not from its predecessor's due time.
func (self *grantLagSource) Entries(ctx context.Context, networkId sidecar.Id, after *bridge.AuthPin) ([][]byte, error) {
	entries, err := self.inner.Entries(ctx, networkId, after)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	dueCount, nextDueAt := func() (int, time.Time) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		network := self.networkWithLock(networkId)
		dueCount := len(entries)
		var nextDueAt time.Time
		for index, entry := range entries {
			digest := sha256.Sum256(entry)
			read := network.entries[digest]
			if read == nil {
				read = &grantLagEntry{firstReadAt: now}
				if !network.held {
					read.dueAt = now.Add(network.lag)
				}
				network.entries[digest] = read
			}
			if index < dueCount && (read.dueAt.IsZero() || now.Before(read.dueAt)) {
				dueCount, nextDueAt = index, read.dueAt
			}
		}
		return dueCount, nextDueAt
	}()
	if !nextDueAt.IsZero() {
		self.schedule(networkId, nextDueAt)
	}
	return entries[:dueCount], nil
}

func (self *grantLagSource) Rows(ctx context.Context, networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
	return self.inner.Rows(ctx, networkId, rowsSha256)
}

// Has run load the network again at dueAt, unless it will sooner.
func (self *grantLagSource) schedule(networkId sidecar.Id, dueAt time.Time) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if held, ok := self.dueAts[networkId]; ok && !dueAt.Before(held) {
			return
		}
		self.dueAts[networkId] = dueAt
	}()
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Sets the lag of the entries the source reads for the first time from now on.
func (self *grantLagSource) setLag(networkId sidecar.Id, lag time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkWithLock(networkId).lag = lag
}

// Holds every entry the source reads for the first time from now on until release.
func (self *grantLagSource) hold(networkId sidecar.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkWithLock(networkId).held = true
}

// Ends the hold: every entry it held is due now, and the network loads again.
func (self *grantLagSource) release(networkId sidecar.Id) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		network := self.networkWithLock(networkId)
		network.held = false
		now := time.Now()
		for _, read := range network.entries {
			if read.dueAt.IsZero() {
				read.dueAt = now
			}
		}
	}()
	self.changed(networkId)
}

// When the source first read the entry whose bytes hash to digest, false for one it has not read.
func (self *grantLagSource) firstReadAt(networkId sidecar.Id, digest [sha256.Size]byte) (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	read := self.networkWithLock(networkId).entries[digest]
	if read == nil {
		return time.Time{}, false
	}
	return read.firstReadAt, true
}

// Loads each network again at its next due time, until ctx ends.
func (self *grantLagSource) run(ctx context.Context) {
	for {
		nextDueAt, dueNetworkIds := func() (time.Time, []sidecar.Id) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			now := time.Now()
			var nextDueAt time.Time
			dueNetworkIds := []sidecar.Id{}
			for networkId, dueAt := range self.dueAts {
				if !now.Before(dueAt) {
					dueNetworkIds = append(dueNetworkIds, networkId)
					delete(self.dueAts, networkId)
					continue
				}
				if nextDueAt.IsZero() || dueAt.Before(nextDueAt) {
					nextDueAt = dueAt
				}
			}
			return nextDueAt, dueNetworkIds
		}()
		for _, networkId := range dueNetworkIds {
			self.changed(networkId)
		}
		var due <-chan time.Time
		if !nextDueAt.IsZero() {
			due = time.After(time.Until(nextDueAt))
		}
		select {
		case <-ctx.Done():
			return
		case <-self.wake:
		case <-due:
		}
	}
}

// One position api's lagging gate installed, and when.
type grantLagInstall struct {
	version uint64
	at      time.Time
}

// api's gate as the exchange reads it, behind the log (the file's header). Safe for concurrent use.
type grantLagGate struct {
	source      *grantLagSource
	cache       *sidecar.AclGateCache
	closeCache  func()
	popExchange func()
	cancel      context.CancelFunc
	runDone     chan struct{}
	closeOnce   sync.Once

	stateLock sync.Mutex
	// each network's installs, in position order
	installs map[sidecar.Id][]*grantLagInstall
	// counts every install, which a wait on one arms on
	installCount *sidecar.MonitorValue[uint64]
}

// A gate cache over the world's stores, its loads as the world's gates' (T3WorldSettings.Gate), lagging as its
// source says, installed as the exchange's scope source until close.
func newGrantLagGate(world *World) (*grantLagGate, error) {
	host := world.T3().Host()
	gateSettings := *world.T3().Settings().Gate
	self := &grantLagGate{
		installs:     map[sidecar.Id][]*grantLagInstall{},
		installCount: sidecar.NewMonitorValue(uint64(0)),
		runDone:      make(chan struct{}),
	}
	gateSettings.OnLoad = self.loaded
	self.source = newGrantLagSource(sidecar.NewAclGateDirectSource(host.Redis, host.Blobs, &gateSettings))
	ctx, cancel := context.WithCancel(world.T3().Context())
	self.cancel = cancel
	cache, closeCache, err := sidecarhost.WatchAclGate(ctx, host, self.source, &gateSettings)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("the lagging gate's cache: %w", err)
	}
	self.cache, self.closeCache = cache, closeCache
	self.source.changed = cache.Changed
	go func() {
		defer close(self.runDone)
		self.source.run(ctx)
	}()
	self.popExchange = oauth.SetExchangeScopeSource(oauth.NewProjectionExchangeScopeSource(cache, &oauth.ExchangeScopeSettings{Clock: host.Clock}))
	world.T3().OnClose(self.close)
	return self, nil
}

// Gives the exchange back the world's source, then ends the cache and its source's loop. Safe to call again.
func (self *grantLagGate) close() {
	self.closeOnce.Do(func() {
		self.popExchange()
		self.cancel()
		<-self.runDone
		self.closeCache()
	})
}

// The cache's OnLoad: keeps each position it installed newer than the last, and when.
func (self *grantLagGate) loaded(networkId sidecar.Id, pin *bridge.AuthPin, err error) {
	if pin == nil {
		return
	}
	installed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		installs := self.installs[networkId]
		if 0 < len(installs) && pin.GetVersion() <= installs[len(installs)-1].version {
			return false
		}
		self.installs[networkId] = append(installs, &grantLagInstall{version: pin.GetVersion(), at: time.Now()})
		return true
	}()
	if installed {
		self.installCount.Update(func(count uint64) uint64 { return count + 1 })
	}
}

// The position the gate holds the network at, 0 for none.
func (self *grantLagGate) applied(networkId sidecar.Id) uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	installs := self.installs[networkId]
	if len(installs) == 0 {
		return 0
	}
	return installs[len(installs)-1].version
}

// When the gate first held the network at version or past it, false while it has not.
func (self *grantLagGate) appliedAt(networkId sidecar.Id, version uint64) (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, install := range self.installs[networkId] {
		if version <= install.version {
			return install.at, true
		}
	}
	return time.Time{}, false
}

// Waits until the gate holds the network at version or past it, asking it to load first, within timeout.
func (self *grantLagGate) awaitApplied(ctx context.Context, networkId sidecar.Id, version uint64, timeout time.Duration) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	self.cache.Changed(networkId)
	for {
		// armed before the read, so an install between them wakes the wait
		_, installed := self.installCount.Get()
		if at, ok := self.appliedAt(networkId, version); ok {
			return at, nil
		}
		select {
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("api's lagging gate holds network %s at %d, not %d: %w", networkId, self.applied(networkId), version, ctx.Err())
		case <-installed:
		}
	}
}

// Sets the lag of every entry of the network the gate's source reads for the first time from now on.
func (self *grantLagGate) setLag(networkId sidecar.Id, lag time.Duration) {
	self.source.setLag(networkId, lag)
}

// Holds every entry of the network the gate's source reads for the first time from now on until release.
func (self *grantLagGate) hold(networkId sidecar.Id) {
	self.source.hold(networkId)
}

// Lets the gate take every entry the hold kept from it.
func (self *grantLagGate) release(networkId sidecar.Id) {
	self.source.release(networkId)
}

// Has the gate load the network now, as an {acl_}v event would: what a filer calls once its statement landed, so
// the source reads the entry within a load of its landing even where the event is slow.
func (self *grantLagGate) changed(networkId sidecar.Id) {
	self.cache.Changed(networkId)
}

// One token exchange api answered.
type grantLagExchange struct {
	// when the hook took the request, and when api's answer reached it
	at       time.Time
	answered time.Time
	// the subject token's client, zero where its claims did not name one
	clientId sidecar.Id
	resource string
	scopes   []string
	status   int
	// the answer's error code, empty for a token
	code    string
	granted []string
}

// Whether the exchange asked for scope alone at resource.
func (self *grantLagExchange) asks(resource string, scope string) bool {
	return self.resource == resource && len(self.scopes) == 1 && self.scopes[0] == scope
}

// Whether api refused the exchange invalid_scope.
func (self *grantLagExchange) scopeRefused() bool {
	return self.status == http.StatusBadRequest && self.code == "invalid_scope"
}

// Whether api answered the exchange with a token granting scope.
func (self *grantLagExchange) grants(scope string) bool {
	return self.status == http.StatusOK && slices.Contains(self.granted, scope)
}

// One line naming the exchange's times, relative to since, and its answer: never its token.
func (self *grantLagExchange) describe(since time.Time) string {
	answer := fmt.Sprintf("%d %s", self.status, self.code)
	if self.status == http.StatusOK {
		answer = fmt.Sprintf("200 granting %v", self.granted)
	}
	return fmt.Sprintf("asked %v at +%s, answered +%s: %s", self.scopes, self.at.Sub(since).Round(time.Millisecond), self.answered.Sub(since).Round(time.Millisecond), answer)
}

// One statement api took.
type grantLagDelta struct {
	at       time.Time
	answered time.Time
	status   int
	// the position api answered, 0 for a refusal
	version uint64
}

// api's record (the file's header). Safe for concurrent use.
type grantLagRecorder struct {
	stateLock sync.Mutex
	exchanges []*grantLagExchange
	deltas    []*grantLagDelta
}

// The record, in front of api's handler from now on until the world closes.
func installGrantLagRecorder(world *World) *grantLagRecorder {
	self := &grantLagRecorder{}
	world.T3().Api().SetApiHook(self.hook)
	world.T3().OnClose(func() { world.T3().Api().SetApiHook(nil) })
	return self
}

// The hook in front of api: the token exchanges and the statements, each kept with its answer; every other request
// passes untouched.
func (self *grantLagRecorder) hook(request *http.Request, serve func() (*http.Response, error)) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL == nil {
		return serve()
	}
	switch request.URL.Path {
	case "/oauth/token":
		return self.exchange(request, serve)
	case "/network/config/delta":
		return self.delta(serve)
	}
	return serve()
}

// A token exchange: the form's subject client, resource and scopes, then api's answer, its body read and handed on.
func (self *grantLagRecorder) exchange(request *http.Request, serve func() (*http.Response, error)) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return serve()
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("grant_type") != oauth.ExchangeGrantType {
		return serve()
	}
	exchange := &grantLagExchange{
		at:       time.Now(),
		clientId: grantLagSubjectClientId(form.Get("subject_token")),
		resource: form.Get("resource"),
		scopes:   strings.Fields(form.Get("scope")),
	}
	response, err := serve()
	exchange.answered = time.Now()
	if err == nil && response != nil {
		answer, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(answer))
		exchange.status = response.StatusCode
		if readErr == nil {
			// the answer's error code and granted scopes alone: the token is never read into anything kept
			parsed := &struct {
				Error string `json:"error"`
				Scope string `json:"scope"`
			}{}
			if json.Unmarshal(answer, parsed) == nil {
				exchange.code = parsed.Error
				exchange.granted = strings.Fields(parsed.Scope)
			}
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.exchanges = append(self.exchanges, exchange)
	return response, err
}

// A statement: api's answer and the position it names.
func (self *grantLagRecorder) delta(serve func() (*http.Response, error)) (*http.Response, error) {
	delta := &grantLagDelta{at: time.Now()}
	response, err := serve()
	delta.answered = time.Now()
	if err == nil && response != nil {
		answer, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(answer))
		delta.status = response.StatusCode
		result := &controller.NetworkConfigPinResult{}
		if readErr == nil && response.StatusCode == http.StatusOK && json.Unmarshal(answer, result) == nil {
			delta.version = result.N
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.deltas = append(self.deltas, delta)
	return response, err
}

// The client a subject token's claims name, read unverified: the exchange verified it. Zero where they name none.
func grantLagSubjectClientId(subjectToken string) sidecar.Id {
	parts := strings.Split(subjectToken, ".")
	if len(parts) != 3 {
		return sidecar.Id{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sidecar.Id{}
	}
	claims := &struct {
		ClientId string `json:"client_id"`
	}{}
	if json.Unmarshal(payload, claims) != nil || claims.ClientId == "" {
		return sidecar.Id{}
	}
	clientId, err := sidecar.ParseId(claims.ClientId)
	if err != nil {
		return sidecar.Id{}
	}
	return clientId
}

// When api answered the statement at version, false for a position no statement of the record took.
func (self *grantLagRecorder) landedAt(version uint64) (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, delta := range self.deltas {
		if delta.version == version {
			return delta.answered, true
		}
	}
	return time.Time{}, false
}

// The exchanges client asked for scope alone at resource whose answer came at or after from and whose request came
// at or before through, in the order api answered them.
func (self *grantLagRecorder) exchangesOf(clientId sidecar.Id, resource string, scope string, from time.Time, through time.Time) []*grantLagExchange {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	exchanges := []*grantLagExchange{}
	for _, exchange := range self.exchanges {
		if exchange.clientId == clientId && exchange.asks(resource, scope) && !exchange.answered.Before(from) && !exchange.at.After(through) {
			exchanges = append(exchanges, exchange)
		}
	}
	slices.SortFunc(exchanges, func(a *grantLagExchange, b *grantLagExchange) int { return a.answered.Compare(b.answered) })
	return exchanges
}

// Every exchange client asked, at any resource and of any scopes, whose answer came at or after from and whose request
// came at or before through, in the order api answered them: a failed act's evidence.
func (self *grantLagRecorder) allOf(clientId sidecar.Id, from time.Time, through time.Time) []*grantLagExchange {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	exchanges := []*grantLagExchange{}
	for _, exchange := range self.exchanges {
		if exchange.clientId == clientId && !exchange.answered.Before(from) && !exchange.at.After(through) {
			exchanges = append(exchanges, exchange)
		}
	}
	slices.SortFunc(exchanges, func(a *grantLagExchange, b *grantLagExchange) int { return a.answered.Compare(b.answered) })
	return exchanges
}

// How many exchanges api answered, and how many it refused invalid_scope.
func (self *grantLagRecorder) counts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	refused := 0
	for _, exchange := range self.exchanges {
		if exchange.scopeRefused() {
			refused += 1
		}
	}
	return len(self.exchanges), refused
}

// The code of the exchange's refusal err holds, "" for an err that holds none.
func grantLagExchangeCode(err error) string {
	var apiErr *sidecar.SidecarApiError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// A source over a fixed log: entry n is position n's bytes, and Entries answers every one after the pin asked from.
type grantLagFixedSource struct {
	stateLock sync.Mutex
	entries   [][]byte
}

func (self *grantLagFixedSource) append(entry string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.entries = append(self.entries, []byte(entry))
}

func (self *grantLagFixedSource) Genesis(ctx context.Context, networkId sidecar.Id) ([]byte, error) {
	return nil, errors.New("no genesis")
}

func (self *grantLagFixedSource) Entries(ctx context.Context, networkId sidecar.Id, after *bridge.AuthPin) ([][]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.entries[after.GetVersion():]), nil
}

func (self *grantLagFixedSource) Rows(ctx context.Context, networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
	return nil, errors.New("no rows")
}

// The lagging source hands the cache each entry only once due, in position order: at once with no lag; an entry read
// first while its network is held, and every entry after it, only after the release, which loads the network again;
// and an entry read first under a lag, with the entries after it, only once the lag has passed, which schedules the
// network's next load then. An entry's due time is fixed at its first read, and another network is untouched.
func TestE2eGrantLagSourceHandsOutEachEntryOnceDue(t *testing.T) {
	ctx := context.Background()
	networkId, otherNetworkId := sidecar.NewId(), sidecar.NewId()
	fixed := &grantLagFixedSource{}
	source := newGrantLagSource(fixed)
	changed := []sidecar.Id{}
	source.changed = func(networkId sidecar.Id) { changed = append(changed, networkId) }
	entries := func(networkId sidecar.Id, after uint64) []string {
		read, err := source.Entries(ctx, networkId, &bridge.AuthPin{Version: after})
		if err != nil {
			t.Fatal(err)
		}
		texts := []string{}
		for _, entry := range read {
			texts = append(texts, string(entry))
		}
		return texts
	}

	fixed.append("1")
	fixed.append("2")
	if read := entries(networkId, 0); !slices.Equal(read, []string{"1", "2"}) {
		t.Errorf("with no lag the source answered %v", read)
	}

	source.hold(networkId)
	fixed.append("3")
	if read := entries(networkId, 2); len(read) != 0 {
		t.Errorf("a held entry was answered: %v", read)
	}
	fixed.append("4")
	if read := entries(networkId, 2); len(read) != 0 {
		t.Errorf("an entry after a held one was answered: %v", read)
	}
	if read := entries(otherNetworkId, 2); !slices.Equal(read, []string{"3", "4"}) {
		t.Errorf("another network's entries were held: %v", read)
	}
	if len(changed) != 0 {
		t.Errorf("a held network was loaded again before its release: %v", changed)
	}
	source.release(networkId)
	if !slices.Equal(changed, []sidecar.Id{networkId}) {
		t.Errorf("the release loaded %v", changed)
	}
	if read := entries(networkId, 2); !slices.Equal(read, []string{"3", "4"}) {
		t.Errorf("after the release the source answered %v", read)
	}

	lag := time.Hour
	source.setLag(networkId, lag)
	fixed.append("5")
	readAt := time.Now()
	if read := entries(networkId, 4); len(read) != 0 {
		t.Errorf("an entry under a lag of %s was answered at once: %v", lag, read)
	}
	dueAt := func() time.Time {
		source.stateLock.Lock()
		defer source.stateLock.Unlock()
		return source.dueAts[networkId]
	}()
	if dueAt.Before(readAt.Add(lag)) || time.Now().Add(lag).Before(dueAt) {
		t.Errorf("the next load is scheduled at %s, not a lag from the first read at %s", dueAt, readAt)
	}
	// an entry that lands behind the lagging one is read with it, its own lag counting from then
	source.setLag(networkId, 0)
	fixed.append("6")
	sixReadAt := time.Now()
	if read := entries(networkId, 4); len(read) != 0 {
		t.Errorf("an entry after a lagging one was answered, or the lagging one's due time moved with the setting: %v", read)
	}
	if firstReadAt, ok := source.firstReadAt(networkId, sha256.Sum256([]byte("6"))); !ok || firstReadAt.Before(sixReadAt) {
		t.Errorf("an entry behind a lagging one was not read with it (%t, at %s), so its lag would start at its predecessor's due time", ok, firstReadAt)
	}
	if read := entries(networkId, 5); !slices.Equal(read, []string{"6"}) {
		t.Errorf("an entry read first with no lag was held: %v", read)
	}
}
