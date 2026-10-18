package connect

// The provider liveness lease writes of connect (model/provider_liveness_lease.go):
// one writer per process that writes the leases of all its connections, shard
// by shard, and each process's stamp of its handler in the writer set, which
// tells the API readers that every lease this process's connections hold is
// being written.
//
// The writer is one goroutine. Every refresh interval, plus a few seconds of
// jitter, it writes each shard once, one bounded script for all of the
// shard's clients of this process, and it writes the connections new to it
// at once. So the lease path holds at most one connection of the
// deadline-bound pool, whatever the number of connections, and never starves
// the admission and debit calls that share it. A write does not wait for any
// connection's sync rounds, which can hold for minutes.
//
// A connection is registered when its sync rounds start, pending its first
// read: the writer extends its client's lease if the shard holds one, so a
// reconnecting provider keeps the lease its last connection wrote however
// long the new connection's first reads take, and creates none for a client
// that may not provide. Its first read that finds the client providing has it
// scored from then on. The process vouches for a connection from its
// registration until its first write says otherwise, and from each successful
// write that scored its client or extended a lease the shard held, counted
// from that shard write's start; a pending connection whose client the shard
// holds no lease for is not vouched for, so a consumer whose reads keep
// failing cannot hold the stamp back. The stamp loop withholds the stamp while
// any vouched connection is more than model.ProviderLivenessWriterWithholdAge
// old, so readers fail open before any lease the process vouches for lapses.
// A pass that raises is logged and the next runs as planned.

import (
	"context"
	mathrand "math/rand"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var connectionLivenessLeaseWritesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "liveness_lease_writes_total",
		Help:      "Provider liveness lease writes by outcome, per client: refreshed and refresh_failed, every 30 s for each client of the process that provides for discovery, or is pending its first read; extended, a pending client whose lease the shard held; released and release_failed, when a read finds the client no longer providing; and per process stamp: stamped, stamp_failed and stamp_withheld, the handler stamp in the writer set every 5 s, withheld while any connection has gone 75 s without a successful write",
	},
	[]string{"outcome"},
)

const (
	livenessLeaseOutcomeRefreshed     = "refreshed"
	livenessLeaseOutcomeRefreshFailed = "refresh_failed"
	livenessLeaseOutcomeExtended      = "extended"
	livenessLeaseOutcomeReleased      = "released"
	livenessLeaseOutcomeReleaseFailed = "release_failed"
	livenessLeaseOutcomeStamped       = "stamped"
	livenessLeaseOutcomeStampFailed   = "stamp_failed"
	livenessLeaseOutcomeStampWithheld = "stamp_withheld"
)

func init() {
	prometheus.MustRegister(connectionLivenessLeaseWritesCounter)
	for _, outcome := range []string{
		livenessLeaseOutcomeRefreshed,
		livenessLeaseOutcomeRefreshFailed,
		livenessLeaseOutcomeExtended,
		livenessLeaseOutcomeReleased,
		livenessLeaseOutcomeReleaseFailed,
		livenessLeaseOutcomeStamped,
		livenessLeaseOutcomeStampFailed,
		livenessLeaseOutcomeStampWithheld,
	} {
		connectionLivenessLeaseWritesCounter.WithLabelValues(outcome)
	}
}

// Counts outcomes in the writes metric.
func observeLivenessLeaseOutcome(outcome string, count int) {
	if 0 < count {
		connectionLivenessLeaseWritesCounter.WithLabelValues(outcome).Add(float64(count))
	}
}

// Whether the client is in the pool FindProviders2 draws from: Public for any
// caller, Network for callers in its own network.
func providesForDiscovery(provideModes map[model.ProvideMode]bool) bool {
	return provideModes[model.ProvideModePublic] || provideModes[model.ProvideModeNetwork]
}

// What a connection's last successful read said of its client.
const (
	// no successful read yet
	livenessLeasePending = iota
	livenessLeaseProviding
	livenessLeaseNotProviding
)

// One announcement's part in its client's liveness lease, written by its
// process's writer. The round goroutine reports each successful sync read
// (afterRead) and the connection's end (end). Its fields are guarded by the
// writer's stateLock.
type providerLivenessLeaseConnection struct {
	writer   *providerLivenessLeaseWriter
	clientId server.Id
	// observes each write's outcome for this connection; nil in production
	writtenForTest func(outcome string)

	state int
	// the start of the last write that succeeded for it, or its registration;
	// zero while the process does not vouch for it
	writeTime time.Time
	// an extension found its client's lease while it was pending
	held bool
	// a requested release failed; the next read that finds the client not
	// providing requests it again
	releasePending bool
	// not yet in a write since it was registered or started providing
	dirty bool
	ended bool
}

// The process's writer of lease writes. Safe for concurrent use.
type providerLivenessLeaseWriter struct {
	refreshInterval time.Duration
	jitter          func() time.Duration
	now             func() time.Time
	writeShard      func(ctx context.Context, shard int, scoredClientIds []server.Id, extendedClientIds []server.Id, removedClientIds []server.Id) ([]bool, error)
	observe         func(outcome string, count int)
	wake            chan struct{}
	startOnce       sync.Once

	stateLock        sync.Mutex
	connections      map[*providerLivenessLeaseConnection]bool
	releaseClientIds map[server.Id]bool
}

// A writer through the model that writes every connection each
// refreshInterval plus up to model.ProviderLivenessLeaseRefreshJitter.
func newProviderLivenessLeaseWriter(refreshInterval time.Duration) *providerLivenessLeaseWriter {
	return &providerLivenessLeaseWriter{
		refreshInterval: refreshInterval,
		jitter: func() time.Duration {
			return time.Duration(mathrand.Int63n(int64(model.ProviderLivenessLeaseRefreshJitter) + 1))
		},
		now:              time.Now,
		writeShard:       model.WriteProviderLivenessLeaseShard,
		observe:          observeLivenessLeaseOutcome,
		wake:             make(chan struct{}, 1),
		connections:      map[*providerLivenessLeaseConnection]bool{},
		releaseClientIds: map[server.Id]bool{},
	}
}

var defaultProviderLivenessLeaseWriter = newProviderLivenessLeaseWriter(model.ProviderLivenessLeaseRefreshInterval)

// Starts the writer's goroutine once, until ctx ends.
func (self *providerLivenessLeaseWriter) start(ctx context.Context) {
	self.startOnce.Do(func() {
		go server.HandleError(func() {
			self.run(ctx)
		})
	})
}

func (self *providerLivenessLeaseWriter) wakeUp() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Registers a connection of clientId as its sync rounds start: pending, vouched
// for from now, and written at once. Starts the writer, for the life of the
// process, if nothing has.
func (self *providerLivenessLeaseWriter) begin(clientId server.Id, writtenForTest func(string)) *providerLivenessLeaseConnection {
	self.start(context.Background())
	connection := &providerLivenessLeaseConnection{
		writer:         self,
		clientId:       clientId,
		writtenForTest: writtenForTest,
		state:          livenessLeasePending,
		writeTime:      self.now(),
		dirty:          true,
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.connections[connection] = true
	}()
	self.wakeUp()
	return connection
}

// Takes the provide modes of a round's successful read.
func (self *providerLivenessLeaseConnection) afterRead(provideModes map[model.ProvideMode]bool) {
	providing := providesForDiscovery(provideModes)
	writer := self.writer
	wake := false
	func() {
		writer.stateLock.Lock()
		defer writer.stateLock.Unlock()
		if self.ended {
			return
		}
		if providing {
			self.releasePending = false
			if self.state != livenessLeaseProviding {
				if self.writeTime.IsZero() {
					// vouched for again from now
					self.writeTime = writer.now()
				}
				self.state = livenessLeaseProviding
				self.dirty = true
				wake = true
			}
			return
		}
		// a read that finds the client not providing releases its lease once:
		// one this connection wrote, or found, or failed to release
		if self.state == livenessLeaseProviding || self.releasePending || (self.state == livenessLeasePending && self.held) {
			writer.releaseClientIds[self.clientId] = true
			self.releasePending = false
			wake = true
		}
		self.state = livenessLeaseNotProviding
		self.writeTime = time.Time{}
	}()
	if wake {
		writer.wakeUp()
	}
}

// Ends the connection's part: the process no longer vouches for it, and
// nothing more is written for it but a release its last read already
// requested; a release that failed is not tried again. The lease is the
// client's, another connection of the client or the one it reconnects with
// may hold it, and it lapses within the window.
func (self *providerLivenessLeaseConnection) end() {
	writer := self.writer
	writer.stateLock.Lock()
	defer writer.stateLock.Unlock()
	self.ended = true
	delete(writer.connections, self)
}

// The earliest vouch time of a pending or providing connection, and whether
// there is one.
func (self *providerLivenessLeaseWriter) oldest() (oldestTime time.Time, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for connection := range self.connections {
		if connection.writeTime.IsZero() {
			continue
		}
		if !ok || connection.writeTime.Before(oldestTime) {
			oldestTime, ok = connection.writeTime, true
		}
	}
	return
}

// Writes every connection each refresh interval plus jitter, from the start
// of one full pass to the next, and the new ones whenever woken, until ctx
// ends. One pass at a time.
func (self *providerLivenessLeaseWriter) run(ctx context.Context) {
	next := time.Now().Add(self.refreshInterval + self.jitter())
	for {
		select {
		case <-ctx.Done():
			return
		case <-self.wake:
			self.pass(ctx, false)
		case <-time.After(time.Until(next)):
			passStart := time.Now()
			self.pass(ctx, true)
			next = passStart.Add(self.refreshInterval + self.jitter())
		}
	}
}

// One write within model.ProviderLivenessLeasePassTimeout. A raise is logged
// and ends this pass only.
func (self *providerLivenessLeaseWriter) pass(ctx context.Context, all bool) {
	passCtx, passCancel := context.WithTimeout(ctx, model.ProviderLivenessLeasePassTimeout)
	defer passCancel()
	server.HandleError(func() {
		self.write(passCtx, all)
	})
}

// One shard's part of a write.
type providerLivenessLeaseShardWrite struct {
	scoredClientIds   []server.Id
	extendedClientIds []server.Id
	removedClientIds  []server.Id
}

// One write of every connection, or of those not yet written, shard by shard,
// in bounded scripts. A providing connection's client is scored, a pending
// one's extended if its shard holds it, and each requested release removed,
// unless a connection of the process has the client providing: its read may
// be older, but deleting a fresh lease is worse. Each connection whose shard
// write succeeds is vouched for from that shard write's start.
func (self *providerLivenessLeaseWriter) write(ctx context.Context, all bool) {
	shardWrites := map[int]*providerLivenessLeaseShardWrite{}
	shardWrite := func(clientId server.Id) *providerLivenessLeaseShardWrite {
		shard := model.ProviderLivenessLeaseShard(clientId)
		if shardWrites[shard] == nil {
			shardWrites[shard] = &providerLivenessLeaseShardWrite{}
		}
		return shardWrites[shard]
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		providingClientIds := map[server.Id]bool{}
		for connection := range self.connections {
			if connection.state == livenessLeaseProviding {
				providingClientIds[connection.clientId] = true
			}
		}
		scoredClientIds := map[server.Id]bool{}
		extendedClientIds := map[server.Id]bool{}
		for connection := range self.connections {
			if !all && !connection.dirty {
				continue
			}
			connection.dirty = false
			clientId := connection.clientId
			switch {
			case providingClientIds[clientId] && (connection.state == livenessLeaseProviding || connection.state == livenessLeasePending):
				if !scoredClientIds[clientId] {
					scoredClientIds[clientId] = true
					shardWrite(clientId).scoredClientIds = append(shardWrite(clientId).scoredClientIds, clientId)
				}
			case connection.state == livenessLeasePending:
				if !extendedClientIds[clientId] {
					extendedClientIds[clientId] = true
					shardWrite(clientId).extendedClientIds = append(shardWrite(clientId).extendedClientIds, clientId)
				}
			}
		}
		for clientId := range self.releaseClientIds {
			if !providingClientIds[clientId] {
				shardWrite(clientId).removedClientIds = append(shardWrite(clientId).removedClientIds, clientId)
			}
		}
		self.releaseClientIds = map[server.Id]bool{}
	}()

	shards := []int{}
	for shard := range shardWrites {
		shards = append(shards, shard)
	}
	slices.Sort(shards)
	for _, shard := range shards {
		if ctx.Err() != nil {
			return
		}
		write := shardWrites[shard]
		start := self.now()
		held, err := self.writeShard(ctx, shard, write.scoredClientIds, write.extendedClientIds, write.removedClientIds)
		if err != nil && ctx.Err() != nil {
			return
		}
		if err != nil && glog.V(1) {
			glog.Infof("[t]liveness lease shard %d write failed: %s\n", shard, err)
		}
		self.written(start, write, held, err)
	}
}

// Takes one shard write's result: on success the connections of its scored
// clients, and of its extended clients the shard held, are vouched for from
// start, and pending connections whose client the shard did not hold are not
// vouched for at all; a failed release is left for the next read that finds
// the client not providing.
func (self *providerLivenessLeaseWriter) written(start time.Time, write *providerLivenessLeaseShardWrite, held []bool, err error) {
	scoredClientIds := map[server.Id]bool{}
	for _, clientId := range write.scoredClientIds {
		scoredClientIds[clientId] = true
	}
	heldClientIds := map[server.Id]bool{}
	unheldClientIds := map[server.Id]bool{}
	for i, clientId := range write.extendedClientIds {
		if err == nil && i < len(held) {
			if held[i] {
				heldClientIds[clientId] = true
			} else {
				unheldClientIds[clientId] = true
			}
		}
	}
	removedClientIds := map[server.Id]bool{}
	for _, clientId := range write.removedClientIds {
		removedClientIds[clientId] = true
	}
	type connectionOutcome struct {
		writtenForTest func(string)
		outcome        string
	}
	connectionOutcomes := []connectionOutcome{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for connection := range self.connections {
			clientId := connection.clientId
			outcome := ""
			switch {
			case scoredClientIds[clientId] && connection.state != livenessLeaseNotProviding:
				if err == nil {
					connection.writeTime = start
					outcome = livenessLeaseOutcomeRefreshed
				} else {
					outcome = livenessLeaseOutcomeRefreshFailed
				}
			case heldClientIds[clientId] && connection.state == livenessLeasePending:
				connection.writeTime = start
				connection.held = true
				outcome = livenessLeaseOutcomeExtended
			case unheldClientIds[clientId] && connection.state == livenessLeasePending:
				// no lease to vouch for until a read finds the client providing
				connection.writeTime = time.Time{}
				connection.held = false
			case removedClientIds[clientId] && connection.state == livenessLeaseNotProviding:
				if err == nil {
					outcome = livenessLeaseOutcomeReleased
				} else {
					connection.releasePending = true
					outcome = livenessLeaseOutcomeReleaseFailed
				}
			}
			if outcome != "" && connection.writtenForTest != nil {
				connectionOutcomes = append(connectionOutcomes, connectionOutcome{writtenForTest: connection.writtenForTest, outcome: outcome})
			}
		}
	}()
	if err == nil {
		self.observe(livenessLeaseOutcomeRefreshed, len(write.scoredClientIds))
		self.observe(livenessLeaseOutcomeExtended, len(heldClientIds))
		self.observe(livenessLeaseOutcomeReleased, len(write.removedClientIds))
	} else {
		self.observe(livenessLeaseOutcomeRefreshFailed, len(write.scoredClientIds)+len(write.extendedClientIds))
		self.observe(livenessLeaseOutcomeReleaseFailed, len(write.removedClientIds))
	}
	for _, connectionOutcome := range connectionOutcomes {
		connectionOutcome.writtenForTest(connectionOutcome.outcome)
	}
}

// Stamps handlerId in the writer set at once and then every interval until ctx
// ends, unless writer says a connection of this process has gone the withhold
// age without a successful write (stampProviderLivenessWriter). Each outcome
// goes to observe.
func runProviderLivenessWriter(
	ctx context.Context,
	handlerId server.Id,
	interval time.Duration,
	writer *providerLivenessLeaseWriter,
	stamp func(context.Context, server.Id) error,
	observe func(outcome string, count int),
) {
	for {
		if outcome := stampProviderLivenessWriter(ctx, handlerId, writer, time.Now(), stamp); outcome != "" {
			observe(outcome, 1)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// One stamp at now, or none: withheld while any pending or providing
// connection's last successful write started more than
// model.ProviderLivenessWriterWithholdAge before now. Returns the outcome, ""
// when ctx ended the stamp.
func stampProviderLivenessWriter(
	ctx context.Context,
	handlerId server.Id,
	writer *providerLivenessLeaseWriter,
	now time.Time,
	stamp func(context.Context, server.Id) error,
) string {
	if oldestTime, ok := writer.oldest(); ok && model.ProviderLivenessWriterWithholdAge < now.Sub(oldestTime) {
		return livenessLeaseOutcomeStampWithheld
	}
	if err := stamp(ctx, handlerId); err != nil {
		if ctx.Err() != nil {
			return ""
		}
		if glog.V(1) {
			glog.Infof("[t]liveness writer stamp failed: %s\n", err)
		}
		return livenessLeaseOutcomeStampFailed
	}
	return livenessLeaseOutcomeStamped
}
