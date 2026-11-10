package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// §7.9's authorization log at api (D-TEAM-19): the sequencer that appends each
// network's statements, bases and ticks to proj/<network_id>/<n>/_ on the first
// metadata cluster, and the reads readers and gates follow. api holds no config
// key, so it verifies as a gate does, with a rows-only ConfigLog at its tip:
// a statement by ConfigLog.Check, a base by ConfigLog.CheckBase, and a base's
// config head by walking its config_pushes from the newest base's head
// (ConfigLog.CheckConfigPushes, plan-TEAM-api-config-head-reach), since api
// never reads the config repo.
//
//	verify     the retry identity first: a statement whose id an applied entry
//	           among the last 4,096 holds, or a base this log holds, answers
//	           that entry's {n, A_n}; then the verdict at the tip, a refusal
//	           answering 403 (a base whose tip moved, 409) with the tip
//	append     PUT proj/<network_id>/<n>/_ If-None-Match at tip+1, the entry's
//	           time api's clock
//	412        another api instance won the position: read its entry, apply it
//	           and verify again at the new tip
//	unknown    a timeout or lost response: GET the position; the same bytes are
//	           success, another entry a loss (apply it and go on at the next
//	           position), 404 repeats the PUT with the same bytes
//	base       its config pushes walk to its head, then its rows (projrows/,
//	           an equal object answering 200), then the entry; base 1 comes
//	           with its Signed Genesis, written at position 0 after entry 1,
//	           the race's commit point, and a retry of base 1 repeats a missing
//	           step (plan-TEAM-genesis-custody)
//	after      network_acl_head advances to the new pin (§8.17)
//
// sidecartest.FakeApi copies these steps for T2, and sidecartest's
// RunConfigLogStoreSuite runs on both (TEAM-M2-12), so a schedule relies only
// on what both do.
//
// Each network's verifier lives in this process's memory; reads come from the
// store, so every api instance reads alike. An append holds its network's lock
// across its store requests, serializing this instance's appends to one
// network, while other instances race it at the store as §7.9 says. Writes are
// P0 and never queued (§11.13): with a write budget installed
// (SetWriteBudget), each proj/ and projrows/ PUT is issued to it as P0 of kind
// log before it is sent, which never waits or refuses, and carries P0 in its
// ctx, as a cluster evsservice.BudgetedBlobs wraps requires; without one, the
// bytes are counted in a metric alone.
// Holds no Postgres connection across store I/O: network_acl_head advances
// after the append, in its own transaction. Safe for concurrent use.
//
// After each entry this instance lands, settings' hooks run, best effort: the
// one script that writes {acl_<networkId>}e and INCRs {acl_<networkId>}v
// (config_log_redis.go), and the team hints (§7.17). Before a statement or base
// is appended, never a retry of one, settings' Limiter charges §7.9's append
// limits, answering 429, and a revocation already in the log answers its entry
// (config_log_limit.go). api's process installs a sequencer built with
// NewApiConfigLogSettings, which sets all three; a test's sequencer sets what it
// tests. Ticks' schedule and retention are config_log_tick.go's and
// config_log_retention.go's (TEAM-M2-14); reseed (§19.10), whose own walk from
// C_0 is TEAM-M3-02's, is not built.

// Tunables of the sequencer. §7.9's 4,096 is ConfigLog's.
type ConfigLogSettings struct {
	// How many positions one append tries before it answers 503: each try is
	// another instance's landed entry or a lost race, so this bounds only a
	// stream of them.
	AppendAttemptCount int
	// The most entries one read answers; more follow from the last one's
	// position. 1,024, §7.9's fold span, so a reader at the newest base's tail
	// takes it in one read.
	ReadPageCount int
	// How many networks' verifiers this process keeps in memory, the least
	// recently used dropped past it and loaded again from the store on its next
	// append.
	NetworkCount int
	// The clock entries' time_ms comes from. nil is real time.
	Clock sidecar.Clock
	// Whether a network is on the team tier, which a directory RepoCreate needs at api alone (§8.8). nil is
	// NetworkTierOf's answer, team; a test passes its own.
	TeamTier func(ctx context.Context, networkId server.Id) bool
	// Writes {acl_<networkId>}e and INCRs {acl_<networkId>}v after each entry this instance lands, with the
	// newest base's position at it (§7.9's read cache, §8.9). nil writes nothing.
	EntryCache func(ctx context.Context, networkId server.Id, n uint64, base uint64, entryBytes []byte) error
	// §7.9's append limits, charged once per statement or base before its append. nil limits nothing.
	Limiter ConfigLogLimiter
	// Hints the network's clients on topic team after each entry this instance lands (§7.17). nil hints
	// nothing.
	Hint func(ctx context.Context, networkId server.Id, pin *bridge.AuthPin, entryBytes []byte)
}

func DefaultConfigLogSettings() *ConfigLogSettings {
	return &ConfigLogSettings{
		AppendAttemptCount: 64,
		ReadPageCount:      1024,
		NetworkCount:       10000,
	}
}

var configLogAppendCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "config_log",
	Name:      "appends_total",
	Help:      "Authorization log appends by entry kind and outcome (§7.9)",
}, []string{"kind", "outcome"})

var configLogAppendByteCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "config_log",
	Name:      "p0_bytes_total",
	Help:      "Bytes of proj/ and projrows/ objects api sent to the store, each try counted, P0 and never queued (§11.13)",
})

func init() {
	prometheus.MustRegister(configLogAppendCounter, configLogAppendByteCounter)
}

// Called with the network and position just before an entry's PUT, and just
// after its outcome is known: the barriers T2 and T3 schedules hold an append
// at. nil in production.
var Testing_ConfigLogBeforePut func(networkId server.Id, n uint64)
var Testing_ConfigLogAfterPut func(networkId server.Id, n uint64)

// An append's refusal or failure: 403 refused at the tip, 409 a base whose tip
// moved or a network whose log has another genesis, 503 the store's outcome
// unknown or the store unavailable. Tip is the tip the signer re-evaluates at,
// nil before base 1. Answers as router.RaiseHttpError reads it: the status in
// the message and, for a 4xx, the JSON body with the code and the tip.
type ConfigLogError struct {
	Status int
	Code   string
	Tip    *bridge.AuthPin
	Err    error
}

// The status and a fixed message, which the router turns into the answer: the
// cause stays in the log.
func (self *ConfigLogError) Error() string {
	switch self.Status {
	case http.StatusForbidden:
		return fmt.Sprintf("%d The statement fails at the tip.", self.Status)
	case http.StatusConflict:
		return fmt.Sprintf("%d The tip moved.", self.Status)
	case http.StatusBadRequest:
		return fmt.Sprintf("%d The entry is malformed.", self.Status)
	}
	return fmt.Sprintf("%d The log is unavailable.", http.StatusServiceUnavailable)
}

// Why.
func (self *ConfigLogError) Unwrap() error {
	return self.Err
}

// The pin's JSON form.
type ConfigLogPin struct {
	N     uint64 `json:"n"`
	Chain []byte `json:"chain"`
}

func configLogPinJson(pin *bridge.AuthPin) *ConfigLogPin {
	if pin == nil {
		return nil
	}
	return &ConfigLogPin{N: pin.GetVersion(), Chain: pin.GetChain()}
}

// The refusal's JSON body: {"error", "code", "tip"}.
type ConfigLogRefusal struct {
	Error string        `json:"error"`
	Code  string        `json:"code"`
	Tip   *ConfigLogPin `json:"tip,omitempty"`
}

// The 4xx body RaiseHttpError answers with.
func (self *ConfigLogError) HttpErrorResultBody() any {
	refusal := &ConfigLogRefusal{Code: self.Code, Tip: configLogPinJson(self.Tip)}
	switch self.Status {
	case http.StatusForbidden:
		refusal.Error = "The statement fails at the tip."
	case http.StatusConflict:
		refusal.Error = "The tip moved."
	default:
		refusal.Error = "The entry is malformed."
	}
	return refusal
}

// One network's log at this instance.
type configLogNetwork struct {
	networkId server.Id

	// serializes this instance's loads and appends for the network, and guards
	// what follows
	stateLock sync.Mutex
	// the rows-only verifier at this instance's tip, nil until loaded
	log           *sidecar.ConfigLog
	signedGenesis []byte
	// each base the verifier applied, by the SHA-256 of its Signed bytes
	basePins map[[sha256.Size]byte]*bridge.AuthPin
	// each base the store held at load and this instance applied since, in
	// position order: what retention reads (config_log_retention.go)
	bases   []*configLogBaseInfo
	lastUse time.Time
}

// A base's facts retention needs.
type configLogBaseInfo struct {
	n             uint64
	timeMs        int64
	foldedThrough uint64
	rowsSha256    []byte
}

// api's log sequencer over the first metadata cluster.
type ConfigLogSequencer struct {
	cluster  sidecar.BlobCluster
	settings *ConfigLogSettings
	// advances network_acl_head after an append; AdvanceNetworkAclHead in api
	advanceHead func(ctx context.Context, networkId server.Id, version uint64, chain []byte, hasDocument bool) (bool, error)

	stateLock sync.Mutex
	networks  map[server.Id]*configLogNetwork
	// the process's write budget, nil for none, and how it prices an object's
	// create (§11.13's cost rows)
	budget sidecar.WriteBudget
	price  func(objectByteCount int64) int64
}

// A sequencer on cluster, which advances network_acl_head with advanceHead
// after each append; nil advances nothing (a test's). settings may be nil for
// the defaults.
func NewConfigLogSequencer(
	cluster sidecar.BlobCluster,
	settings *ConfigLogSettings,
	advanceHead func(ctx context.Context, networkId server.Id, version uint64, chain []byte, hasDocument bool) (bool, error),
) *ConfigLogSequencer {
	if settings == nil {
		settings = DefaultConfigLogSettings()
	}
	return &ConfigLogSequencer{
		cluster:     cluster,
		settings:    settings,
		advanceHead: advanceHead,
		networks:    map[server.Id]*configLogNetwork{},
	}
}

// Issues every proj/ and projrows/ write to budget before it is sent, as P0 of
// kind log on the cluster, priced by price, the bytes of each object it
// creates (§11.13's reports row; evsservice.PriceTable's create row). OPS
// installs api's process budget with its first metadata cluster. nil removes
// it.
func (self *ConfigLogSequencer) SetWriteBudget(budget sidecar.WriteBudget, price func(objectByteCount int64) int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.budget, self.price = budget, price
}

// ctx for one PUT of an object of byteCount bytes: counted, issued to the
// budget as P0 when one is installed, and carrying P0.
func (self *ConfigLogSequencer) issueP0(ctx context.Context, byteCount int) context.Context {
	configLogAppendByteCounter.Add(float64(byteCount))
	budget, price := func() (sidecar.WriteBudget, func(int64) int64) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.budget, self.price
	}()
	if budget == nil {
		return ctx
	}
	pricedByteCount := int64(byteCount)
	if price != nil {
		pricedByteCount = price(int64(byteCount))
	}
	return budget.IssueP0(ctx, self.cluster.ClusterId(), sidecar.WriteP0KindLog, pricedByteCount)
}

func (self *ConfigLogSequencer) now() time.Time {
	if self.settings.Clock != nil {
		return self.settings.Clock.Now()
	}
	return time.Now()
}

// The network's log, made empty on first use, the least recently used dropped
// past NetworkCount.
func (self *ConfigLogSequencer) network(networkId server.Id) *configLogNetwork {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	network := self.networks[networkId]
	if network == nil {
		if 0 < self.settings.NetworkCount && self.settings.NetworkCount <= len(self.networks) {
			var oldest *configLogNetwork
			for _, candidate := range self.networks {
				if oldest == nil || candidate.lastUse.Before(oldest.lastUse) {
					oldest = candidate
				}
			}
			// an append under way keeps its pointer and finishes; the next
			// request loads the network again from the store
			delete(self.networks, oldest.networkId)
		}
		network = &configLogNetwork{networkId: networkId, basePins: map[[sha256.Size]byte]*bridge.AuthPin{}}
		self.networks[networkId] = network
	}
	network.lastUse = self.now()
	return network
}

// A key's bytes, false for none.
func configLogGet(ctx context.Context, cluster sidecar.BlobCluster, key string) ([]byte, bool, error) {
	object, err := cluster.Get(ctx, key)
	if errors.Is(err, sidecar.ErrBlobNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer object.Body.Close()
	body, err := io.ReadAll(object.Body)
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// The rows a base names, from projrows/.
func (self *ConfigLogSequencer) rowsSource(ctx context.Context) sidecar.ConfigRowsOnlySource {
	return func(networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
		rows, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigRowsKey(networkId, rowsSha256))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("no rows at %x", rowsSha256[:8])
		}
		return rows, nil
	}
}

// The positions after after the store holds for the network, in order, at
// most limit, and whether more follow.
func (self *ConfigLogSequencer) listPositions(ctx context.Context, networkId server.Id, after uint64, limit int) ([]uint64, bool, error) {
	positions := []uint64{}
	startAfter := sidecar.ConfigEntryKey(sidecar.Id(networkId), after)
	for {
		infos, more, err := self.cluster.List(ctx, sidecar.ConfigEntryKeyPrefix(sidecar.Id(networkId)), startAfter, min(limit-len(positions), 999))
		if err != nil {
			return nil, false, err
		}
		for _, info := range infos {
			if n, ok := sidecar.ParseConfigEntryKey(sidecar.Id(networkId), info.Key); ok && after < n {
				positions = append(positions, n)
			}
			startAfter = info.Key
		}
		if !more || limit <= len(positions) {
			return positions, more, nil
		}
	}
}

// A rows-only log's settings, its refusals logged by the log.
func configLogVerifierSettings() *sidecar.ConfigLogSettings {
	settings := sidecar.DefaultConfigLogSettings()
	settings.Log = sidecar.NewNoopLogger()
	return settings
}

// Loads the verifier from the store when this instance holds none: entry 0's
// genesis, or genesisHint while entry 0 is unwritten and the hint is entry 1's
// prev_chain (a retry of base 1 whose genesis step failed), then the retained
// entries. Nil with no error when the network has no log.
func (self *configLogNetwork) loadWithLock(ctx context.Context, sequencer *ConfigLogSequencer, genesisHint []byte) error {
	if self.log != nil {
		return nil
	}
	networkId := sidecar.Id(self.networkId)
	signedGenesis, found, err := configLogGet(ctx, sequencer.cluster, sidecar.ConfigEntryKey(networkId, 0))
	if err != nil {
		return err
	}
	positions, _, err := sequencer.listPositions(ctx, self.networkId, 0, 1<<30)
	if err != nil {
		return err
	}
	entries := make([][]byte, 0, len(positions))
	for _, n := range positions {
		entryBytes, found, err := configLogGet(ctx, sequencer.cluster, sidecar.ConfigEntryKey(networkId, n))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("entry %d listed and gone", n)
		}
		entries = append(entries, entryBytes)
	}
	if !found {
		if len(entries) == 0 || genesisHint == nil {
			return nil
		}
		first := &bridge.AuthEntry{}
		if err := proto.Unmarshal(entries[0], first); err != nil {
			return err
		}
		genesisChain := bridge.GenesisChain(genesisHint)
		if first.GetN() != 1 || !bytes.Equal(first.GetPrevChain(), genesisChain[:]) {
			return nil
		}
		signedGenesis = genesisHint
	}
	var log *sidecar.ConfigLog
	if len(entries) == 0 || positions[0] == 1 {
		log, err = sidecar.NewConfigLogRowsOnly(configLogVerifierSettings(), signedGenesis)
		if err == nil && 0 < len(entries) {
			_, err = log.ApplyRun(entries, sequencer.rowsSource(ctx))
		}
	} else {
		retained := &sidecar.ConfigRetained{SignedGenesis: signedGenesis, Entries: entries}
		log, err = sidecar.RestoreConfigLogRowsOnly(configLogVerifierSettings(), retained, sequencer.rowsSource(ctx), nil)
	}
	if err != nil {
		return err
	}
	self.log, self.signedGenesis = log, signedGenesis
	for _, entryBytes := range entries {
		self.noteBaseWithLock(entryBytes)
	}
	return nil
}

// Remembers a base entry's pin by its Signed bytes, for a retry of the same
// base.
func (self *configLogNetwork) noteBaseWithLock(entryBytes []byte) {
	entry := &bridge.AuthEntry{}
	if proto.Unmarshal(entryBytes, entry) != nil || entry.GetKind() != bridge.AuthEntryKindBase {
		return
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		return
	}
	self.basePins[sha256.Sum256(entry.GetBody())] = &bridge.AuthPin{Version: entry.GetN(), Chain: chain[:]}
	signed := &bridge.Signed{}
	header := &bridge.ProjectionHeader{}
	if proto.Unmarshal(entry.GetBody(), signed) != nil || proto.Unmarshal(signed.GetBody(), header) != nil {
		return
	}
	if 0 < len(self.bases) && entry.GetN() <= self.bases[len(self.bases)-1].n {
		return
	}
	self.bases = append(self.bases, &configLogBaseInfo{
		n:             entry.GetN(),
		timeMs:        entry.GetTimeMs(),
		foldedThrough: header.GetFoldedThrough(),
		rowsSha256:    header.GetRowsSha256(),
	})
}

// Applies an entry this instance or another wrote to the verifier. One that
// does not apply, another genesis's base 1 that won a race or a fork, drops the
// verifier, which the next request loads again from the store: the store,
// never this memory, says what the log is.
func (self *configLogNetwork) applyWithLock(ctx context.Context, sequencer *ConfigLogSequencer, entryBytes []byte) error {
	if _, err := self.log.Apply(entryBytes, sequencer.rowsSource(ctx)); err != nil {
		glog.Errorf("[config]%s dropping the verifier: %v\n", self.networkId, err)
		self.log, self.signedGenesis = nil, nil
		clear(self.basePins)
		self.bases = nil
		return err
	}
	self.noteBaseWithLock(entryBytes)
	return nil
}

// One append as the sequencer runs it.
type configLogAppend struct {
	kind uint32
	body []byte
	// the genesis base 1 carries
	genesis []byte
	// the entry a retry finds, or the verdict at the tip
	verify func(network *configLogNetwork) (*bridge.AuthPin, error)
	// charged once, after the first verify finds no retry and passes: §7.9's
	// append limits, a 429, or an entry already in the log to answer with
	charge func(ctx context.Context, network *configLogNetwork) (*bridge.AuthPin, error)
	// written before the entry (a base's rows) and after it (base 1's
	// genesis), each again on a retry
	before func(ctx context.Context) error
	after  func(ctx context.Context) error
}

// §7.9's sequencing steps for one entry.
func (self *ConfigLogSequencer) append(ctx context.Context, networkId server.Id, request *configLogAppend) (pin *bridge.AuthPin, returnErr error) {
	kindLabel := fmt.Sprintf("%d", request.kind)
	defer func() {
		outcome := "appended"
		configLogError := &ConfigLogError{}
		if errors.As(returnErr, &configLogError) {
			outcome = fmt.Sprintf("%d", configLogError.Status)
		}
		configLogAppendCounter.WithLabelValues(kindLabel, outcome).Inc()
	}()
	unavailable := func(tip *bridge.AuthPin, err error) error {
		return &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: err}
	}

	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()

	if err := network.loadWithLock(ctx, self, request.genesis); err != nil {
		return nil, unavailable(nil, err)
	}
	if network.log == nil {
		if request.genesis == nil {
			return nil, &ConfigLogError{Status: http.StatusForbidden, Code: "config_no_log", Err: errors.New("only base 1 with its genesis starts a log")}
		}
		log, err := sidecar.NewConfigLogRowsOnly(configLogVerifierSettings(), request.genesis)
		if err != nil {
			return nil, &ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: err}
		}
		if log.State().NetworkId() != sidecar.Id(networkId) {
			return nil, &ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: errors.New("the genesis names another network")}
		}
		network.log, network.signedGenesis = log, request.genesis
	} else if request.genesis != nil && !bytes.Equal(request.genesis, network.signedGenesis) {
		// no network whose log exists takes a new base 1
		return nil, &ConfigLogError{Status: http.StatusConflict, Code: "config_tip_moved", Tip: network.log.Pin(), Err: errors.New("the network's log has another genesis")}
	}

	// landedBytes is the entry this call landed, nil for one a retry found
	finish := func(pin *bridge.AuthPin, landedBytes []byte) (*bridge.AuthPin, error) {
		if request.after != nil {
			if err := request.after(ctx); err != nil {
				return nil, unavailable(pin, err)
			}
		}
		self.advance(ctx, networkId, network, pin)
		if landedBytes != nil {
			self.published(ctx, networkId, network, pin, landedBytes)
		}
		return pin, nil
	}
	charged := false
	for range self.settings.AppendAttemptCount {
		found, err := request.verify(network)
		if err != nil {
			status, code := http.StatusForbidden, "config_refused"
			if errors.Is(err, sidecar.ErrConfigTipMoved) {
				status, code = http.StatusConflict, "config_tip_moved"
			}
			return nil, &ConfigLogError{Status: status, Code: code, Tip: network.log.Pin(), Err: err}
		}
		if found != nil {
			return finish(found, nil)
		}
		if !charged && request.charge != nil {
			charged = true
			existing, err := request.charge(ctx, network)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				return finish(existing, nil)
			}
		}
		tip := network.log.Pin()
		n := tip.GetVersion() + 1
		entryBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.AuthEntry{
			NetworkId: sidecar.Id(networkId).Bytes(),
			N:         n,
			PrevChain: tip.GetChain(),
			Kind:      request.kind,
			TimeMs:    self.now().UnixMilli(),
			Body:      request.body,
		})
		if err != nil {
			return nil, unavailable(tip, err)
		}
		if request.before != nil {
			if err := request.before(ctx); err != nil {
				return nil, unavailable(tip, err)
			}
		}
		if hook := Testing_ConfigLogBeforePut; hook != nil {
			hook(networkId, n)
		}
		landed, err := self.put(ctx, network, sidecar.ConfigEntryKey(sidecar.Id(networkId), n), entryBytes, tip)
		if hook := Testing_ConfigLogAfterPut; hook != nil {
			hook(networkId, n)
		}
		if err != nil {
			return nil, err
		}
		if landed != nil {
			return finish(landed, entryBytes)
		}
	}
	return nil, unavailable(network.log.Pin(), errors.New("too many lost positions"))
}

// After this instance landed entryBytes at pin: the read cache's script and the
// team hints, each best effort, since a lost cache costs cluster reads and a
// lost hint a client's resync.
func (self *ConfigLogSequencer) published(ctx context.Context, networkId server.Id, network *configLogNetwork, pin *bridge.AuthPin, entryBytes []byte) {
	if self.settings.EntryCache != nil {
		base := uint64(0)
		if network.log != nil {
			base = network.log.State().Base().GetVersion()
		}
		if err := self.settings.EntryCache(ctx, networkId, pin.GetVersion(), base, entryBytes); err != nil {
			glog.Warningf("[config]%s {acl_}e misses %d: %v\n", networkId, pin.GetVersion(), err)
		}
	}
	if self.settings.Hint != nil {
		self.settings.Hint(ctx, networkId, pin, entryBytes)
	}
}

// One entry's PUT at its position until its outcome is known: the new pin when
// it landed, nil when another entry took the position (applied, so the caller
// verifies again at the next), or an error.
func (self *ConfigLogSequencer) put(ctx context.Context, network *configLogNetwork, key string, entryBytes []byte, tip *bridge.AuthPin) (*bridge.AuthPin, error) {
	for range self.settings.AppendAttemptCount {
		_, putErr := self.cluster.PutIfNoneMatch(self.issueP0(ctx, len(entryBytes)), key, entryBytes, nil)
		var unknown *sidecar.BlobUnknownError
		switch {
		case putErr == nil:
			if err := network.applyWithLock(ctx, self, entryBytes); err != nil {
				return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: err}
			}
			return network.log.Pin(), nil
		case errors.Is(putErr, sidecar.ErrBlobPrecondition), errors.As(putErr, &unknown):
			held, found, err := configLogGet(ctx, self.cluster, key)
			switch {
			case err != nil:
				return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: fmt.Errorf("the outcome is unknown: %w", err)}
			case !found:
				// it never landed and never will, and a 412 over nothing held
				// nothing: the same bytes again
				continue
			case bytes.Equal(held, entryBytes):
				if err := network.applyWithLock(ctx, self, held); err != nil {
					return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: err}
				}
				return network.log.Pin(), nil
			}
			// another instance's entry: a loss here, applied, and verified
			// again at the next position
			if err := network.applyWithLock(ctx, self, held); err != nil {
				return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: err}
			}
			return nil, nil
		default:
			return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: putErr}
		}
	}
	return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: tip, Err: errors.New("the store never took the entry")}
}

// Raises network_acl_head to pin, with whether the rows there hold a document
// (§8.17). Best effort: the entry is committed in the store, which is what
// readers and the restore step read, so a failure here is logged and the head
// catches up at the next append.
func (self *ConfigLogSequencer) advance(ctx context.Context, networkId server.Id, network *configLogNetwork, pin *bridge.AuthPin) {
	if self.advanceHead == nil || network.log == nil {
		return
	}
	state, err := network.log.StateAt(pin)
	if err != nil {
		state = network.log.State()
	}
	hasDocument := false
	if acl, err := state.Acl(); err == nil {
		hasDocument = acl.HasDocument()
	}
	if _, err := self.advanceHead(ctx, networkId, pin.GetVersion(), pin.GetChain(), hasDocument); err != nil && !errors.Is(err, ErrNetworkAclHeadUnavailable) {
		glog.Warningf("[config]%s network_acl_head stays below %d: %v\n", networkId, pin.GetVersion(), err)
	}
}

// §7.9's statement row: the retry identity, then the verdict at the tip and,
// for a directory RepoCreate, the team tier, which api alone checks since
// readers know no tier (§8.8, §20.5), then the append.
func (self *ConfigLogSequencer) AppendStatement(ctx context.Context, networkId server.Id, signed []byte) (*bridge.AuthPin, error) {
	return self.append(ctx, networkId, &configLogAppend{
		kind: bridge.AuthEntryKindStatement,
		body: signed,
		verify: func(network *configLogNetwork) (*bridge.AuthPin, error) {
			verdict := network.log.Check(signed, self.now().UnixMilli())
			if pin, found := network.log.FindStatement(verdict.StatementId); found {
				return pin, nil
			}
			if verdict.Refusal == nil && sidecar.AclStatementNeedsTeamTier(signed) && !self.teamTier(ctx, networkId) {
				return nil, fmt.Errorf("%w: the directory is the team tier's", sidecar.ErrConfigRule)
			}
			return nil, verdict.Refusal
		},
		charge: func(ctx context.Context, network *configLogNetwork) (*bridge.AuthPin, error) {
			return self.chargeStatement(ctx, networkId, network, signed)
		},
	})
}

// Whether the network is on the team tier: the settings' check, else
// NetworkTierOf's answer.
func (self *ConfigLogSequencer) teamTier(ctx context.Context, networkId server.Id) bool {
	if self.settings.TeamTier != nil {
		return self.settings.TeamTier(ctx, networkId)
	}
	return NetworkTierOf(ctx, networkId) == NetworkTierTeam
}

// A base as POST /network/config/projection carries it.
type ConfigLogBase struct {
	// The Signed ProjectionHeader, verbatim.
	SignedHeader []byte
	// The ProjectionRows bytes, verbatim.
	Rows []byte
	// The Signed Genesis, base 1 only (plan-TEAM-genesis-custody).
	Genesis []byte
	// The Signed EvsPush of each config version the base adds, oldest first,
	// at most 4,096, which api walks from the newest base's head to this
	// one's (plan-TEAM-api-config-head-reach); empty when the version is
	// unchanged. Not stored.
	ConfigPushes [][]byte
}

// §7.9's base row: a retry of the same base answers its entry; else the
// verdict at the tip and the walk of its config pushes, the rows, then the
// entry, and for base 1 its genesis at position 0 after it.
func (self *ConfigLogSequencer) AppendBase(ctx context.Context, networkId server.Id, base *ConfigLogBase) (*bridge.AuthPin, error) {
	malformed := func(err error) error {
		return &ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: err}
	}
	signed := &bridge.Signed{}
	header := &bridge.ProjectionHeader{}
	if err := proto.Unmarshal(base.SignedHeader, signed); err != nil {
		return nil, malformed(err)
	}
	if err := proto.Unmarshal(signed.GetBody(), header); err != nil {
		return nil, malformed(err)
	}
	if (header.GetVersion() == 1) != (base.Genesis != nil) {
		return nil, malformed(errors.New("base 1 comes with its genesis, and no other base does"))
	}
	if base.Genesis != nil {
		genesisChain := bridge.GenesisChain(base.Genesis)
		if !bytes.Equal(header.GetPrevChain(), genesisChain[:]) {
			return nil, malformed(errors.New("base 1's prev_chain is not SHA-256 of its genesis"))
		}
	}
	bodyId := sha256.Sum256(base.SignedHeader)
	request := &configLogAppend{
		kind:    bridge.AuthEntryKindBase,
		body:    base.SignedHeader,
		genesis: base.Genesis,
		verify: func(network *configLogNetwork) (*bridge.AuthPin, error) {
			if pin := network.basePins[bodyId]; pin != nil {
				return pin, nil
			}
			if err := network.log.CheckBase(base.SignedHeader, base.Rows); err != nil {
				return nil, err
			}
			return nil, network.log.CheckConfigPushes(base.SignedHeader, base.ConfigPushes)
		},
		charge: func(ctx context.Context, network *configLogNetwork) (*bridge.AuthPin, error) {
			if self.settings.Limiter == nil {
				return nil, nil
			}
			return nil, self.settings.Limiter.ChargeBase(ctx, networkId)
		},
		before: func(ctx context.Context) error {
			rowsSha256 := sha256.Sum256(base.Rows)
			_, err := sidecar.PutImmutable(self.issueP0(ctx, len(base.Rows)), self.cluster, sidecar.ConfigRowsKey(sidecar.Id(networkId), rowsSha256[:]), base.Rows, nil)
			return err
		},
	}
	if base.Genesis != nil {
		request.after = func(ctx context.Context) error {
			_, err := sidecar.PutImmutable(self.issueP0(ctx, len(base.Genesis)), self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), 0), base.Genesis, nil)
			return err
		}
	}
	return self.append(ctx, networkId, request)
}

// §7.9's tick row: api's own facts, a reap or a recovery window's end, which
// readers check by content, so api appends only a tick its verifier takes at
// the tip (checkConfigLogTick). Ticks never count against the append limits.
// Their schedule, at most once a minute per network, is config_log_tick.go's.
func (self *ConfigLogSequencer) AppendTick(ctx context.Context, networkId server.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(tick)
	if err != nil {
		return nil, err
	}
	return self.append(ctx, networkId, &configLogAppend{
		kind: bridge.AuthEntryKindTick,
		body: body,
		verify: func(network *configLogNetwork) (*bridge.AuthPin, error) {
			if network.log.State().Base() == nil {
				return nil, errors.New("no tick before base 1")
			}
			return nil, checkConfigLogTick(network.log.State(), tick)
		},
	})
}

// What a read of the log answers (GET /network/config/projection).
type ConfigLogRead struct {
	// The Signed Genesis, verbatim, when the read starts at 0.
	SignedGenesis []byte
	// The entries after the position asked, verbatim and in order, from the
	// retention point when that position is before it; a page at most.
	Entries [][]byte
	// The rows of each base among Entries, verbatim.
	Rows [][]byte
	// The newest entry read, nil when none.
	Tip *bridge.AuthPin
	// More entries follow the last one.
	More bool
}

// Entry 1 exists and entry 0 does not yet: base 1's genesis step has not
// landed (plan-TEAM-genesis-custody #3), answered 503.
var ErrConfigLogGenesisMissing = errors.New("entry 1 exists without its genesis")

// Reads the log from the store: the entries after after, a page at most, with
// the rows of their bases and, from 0, the genesis.
func (self *ConfigLogSequencer) Read(ctx context.Context, networkId server.Id, after uint64) (*ConfigLogRead, error) {
	read := &ConfigLogRead{Entries: [][]byte{}, Rows: [][]byte{}}
	positions, more, err := self.listPositions(ctx, networkId, after, self.settings.ReadPageCount)
	if err != nil {
		return nil, err
	}
	read.More = more
	if after == 0 {
		signedGenesis, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), 0))
		if err != nil {
			return nil, err
		}
		if !found && 0 < len(positions) {
			return nil, ErrConfigLogGenesisMissing
		}
		read.SignedGenesis = signedGenesis
	}
	for _, n := range positions {
		entryBytes, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), n))
		if err != nil {
			return nil, err
		}
		if !found {
			read.More = true
			break
		}
		entry := &bridge.AuthEntry{}
		if err := proto.Unmarshal(entryBytes, entry); err != nil {
			return nil, err
		}
		chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
		if err != nil {
			return nil, err
		}
		read.Entries = append(read.Entries, entryBytes)
		read.Tip = &bridge.AuthPin{Version: n, Chain: chain[:]}
		if entry.GetKind() == bridge.AuthEntryKindBase {
			signed := &bridge.Signed{}
			header := &bridge.ProjectionHeader{}
			if proto.Unmarshal(entry.GetBody(), signed) == nil && proto.Unmarshal(signed.GetBody(), header) == nil {
				rows, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigRowsKey(sidecar.Id(networkId), header.GetRowsSha256()))
				if err != nil {
					return nil, err
				}
				if found {
					read.Rows = append(read.Rows, rows)
				}
			}
		}
	}
	return read, nil
}

// The newest entry's pin as the store holds it, nil before base 1.
func (self *ConfigLogSequencer) Tip(ctx context.Context, networkId server.Id) (*bridge.AuthPin, error) {
	positions, _, err := self.listPositions(ctx, networkId, 0, 1<<30)
	if err != nil || len(positions) == 0 {
		return nil, err
	}
	n := positions[len(positions)-1]
	entryBytes, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), n))
	if err != nil || !found {
		return nil, err
	}
	entry := &bridge.AuthEntry{}
	if err := proto.Unmarshal(entryBytes, entry); err != nil {
		return nil, err
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		return nil, err
	}
	return &bridge.AuthPin{Version: n, Chain: chain[:]}, nil
}

// Whether the client's sidecar stands admitted at its network's tip as this
// instance's verifier holds it: an admitted client may append anything the
// log takes, an unadmitted one may read the log and post a recovery-signed
// statement only (§7.9's Recovery `before admission` row).
func (self *ConfigLogSequencer) Admitted(ctx context.Context, networkId server.Id, sidecarId sidecar.Id) (bool, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return false, err
	}
	if network.log == nil {
		return false, nil
	}
	row := network.log.State().SidecarRow(sidecarId)
	return row != nil && row.GetRevokedAt() == 0 && row.GetParkedAt() == 0, nil
}

// The identity key the network's verified state admits sidecarId under, nil
// when it holds no live admission of it, revoked and parked rows included:
// what api asks before it takes a restored sidecar's binding again (§5.6,
// §19.10), so a binding re-registers only an identity key the log admits.
func (self *ConfigLogSequencer) AdmittedIdentityKey(ctx context.Context, networkId server.Id, sidecarId sidecar.Id) ([]byte, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	if network.log == nil {
		return nil, nil
	}
	row := network.log.State().SidecarRow(sidecarId)
	if row == nil || row.GetRevokedAt() != 0 || row.GetParkedAt() != 0 || len(row.GetIdentityPublicKey()) == 0 {
		return nil, nil
	}
	return bytes.Clone(row.GetIdentityPublicKey()), nil
}

// This process's sequencer, which api installs once its first metadata cluster
// is wired (OPS's ServiceHost.Blobs); until then every log route answers 503,
// so nothing is taken unverified.
var configLogSequencer atomic.Pointer[ConfigLogSequencer]

// Installs this process's sequencer; nil removes it.
func SetConfigLogSequencer(sequencer *ConfigLogSequencer) {
	configLogSequencer.Store(sequencer)
}

// This process's sequencer, or a 503 error while api has none.
func CurrentConfigLogSequencer() (*ConfigLogSequencer, error) {
	sequencer := configLogSequencer.Load()
	if sequencer == nil {
		return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: errors.New("api holds no first metadata cluster")}
	}
	return sequencer, nil
}

// Appends a statement to the network's log through this process's sequencer.
func AppendConfigStatement(ctx context.Context, networkId server.Id, signed []byte) (*bridge.AuthPin, error) {
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	return sequencer.AppendStatement(ctx, networkId, signed)
}

// Appends api's tick to the network's log through this process's sequencer.
func AppendApiTick(ctx context.Context, networkId server.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	return sequencer.AppendTick(ctx, networkId, tick)
}

// The network's newest entry's pin, nil before base 1.
func ConfigTip(ctx context.Context, networkId server.Id) (*bridge.AuthPin, error) {
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	return sequencer.Tip(ctx, networkId)
}
