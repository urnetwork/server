package model

// An API router, a proxy device manager, a provider-egress pass or a Connect
// exchange owns this advisory notification service. When an origin contract
// commits anywhere in the fleet, it wakes the requests here that wait for that
// origin, and removes the route refusals here that the origin opens
// (contract_route_rejection.go). PostgreSQL is authoritative: a missed,
// dropped or mixed-version event costs only the optimization, and the
// requests' timed rechecks remain.
//
// Build A of CPUOPT7-ORIGIN-DELIVERY.md. A committed origin is published, in
// one pipeline to one node, on two channels of its pair's bucket: the bucket's
// own channel (v1), which every owner with a watch or a held pair in the
// bucket receives, and the pair's channel (v2), which only an owner holding the
// pair receives. Either alone wakes every waiter and removes every refusal, so
// owners and publishers from before Build A (v1 only) and from Build B (v2
// only) may run beside these.
//
// An owner holds a pair past a miss: a watch after a read that found no origin,
// a watch whose read was fenced, and each stored ring refusal. A held pair's
// channel is subscribed on its bucket's socket, and released when the last
// holder lets go. A read armed while the pair's subscription is acknowledged is
// fenced: every origin that commits after it reaches the owner while the pair
// stays held, or the socket was lost and the pair's resubscription resyncs it.
// Only a fenced read may record a missing-origin refusal.
//
// Each bucket has one manager goroutine. It opens the bucket's socket while a
// watch or a held pair needs it, writes the pair subscriptions on it (no
// request writes to Redis), replaces the socket when a pair's acknowledgement
// is overdue, and closes it after IdleBucketTimeout with neither. A bounded
// publisher queue keeps Redis latency out of committed contract responses.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

const contractOriginBucketCount = 16

// A received edge also counts what it found: matched_waiter when its pair has a
// watch here, matched_refusal when it removed a recorded route refusal, and
// unmatched when it found neither. An edge that finds both counts both.
// v2_received counts the edges read on a pair channel, and
// v1_for_acknowledged_pair the bucket channel's edges for a pair whose own
// channel was subscribed at that point of the stream: once every publisher
// runs Build A the two are equal.
var contractOriginNotificationCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_contract_origin_notifications_total",
	Help: "Advisory contract-origin notification operations by a fixed event class; drops require authoritative timed rechecks. A received edge also counts matched_waiter (its pair has a watch), matched_refusal (it removed a route refusal) or unmatched (neither). Pair channels: subscribed_pair, pair_acknowledged, pair_released, subscribe_failed (a failed write or an overdue acknowledgement replaced the socket), pair_cap (a hold declined at the owner's bound), bucket_closed (an idle socket closed), and the shadow pair v1_for_acknowledged_pair and v2_received.",
}, []string{"event"})

var contractOriginHeldPairsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_contract_origin_held_pairs",
	Help: "Contract-origin pairs this process's owners hold past a miss, each subscribed on its own channel: waits whose read missed, fenced reads and stored route refusals",
})

func init() {
	for _, event := range []string{
		"enqueued", "published", "publish_failed", "queue_full", "owner_closed", "unowned",
		"subscribed", "reconnected", "subscription_failed", "received", "invalid_message", "registration_declined",
		"matched_waiter", "matched_refusal", "unmatched",
		"subscribed_pair", "pair_acknowledged", "pair_released", "subscribe_failed", "pair_cap", "bucket_closed",
		"v1_for_acknowledged_pair", "v2_received",
	} {
		contractOriginNotificationCounter.WithLabelValues(event)
	}
	prometheus.MustRegister(contractOriginNotificationCounter, contractOriginHeldPairsGauge)
}

var (
	errContractOriginUnrequestedNotice = errors.New("a pair channel notice that nothing asked for")
	errContractOriginChannelCount      = errors.New("the socket's shard channel count differs from its acknowledged channels")
)

// These budgets belong to one lifecycle owner, never to the process. Exhausting
// them loses only an optimization: the request's bounded PostgreSQL wait remains.
// Route rejections (contract_route_rejection.go) are bounded by count and by a
// timeout; zero disables them. Terminal route rejections have their own
// generation bound and timeout.
type ContractOriginNotificationSettings struct {
	MaxWaiters                    int
	PublishQueueSize              int
	PublishWorkers                int
	MaxRouteRejections            int
	RouteRejectionTimeout         time.Duration
	TerminalRouteRejectionCount   int
	TerminalRouteRejectionTimeout time.Duration
	// The longest one publication of an origin may take; it is then dropped.
	PublishTimeout time.Duration
	// The most pairs the owner holds at once, each a subscription on its
	// bucket's socket. Past it a miss is not held: its wait polls, and no
	// refusal that would need a hold is stored. Zero holds none.
	MaxHeldPairs int
	// How long a Stream-fallback lookup that missed before its pair's
	// subscription was acknowledged waits for that acknowledgement before it
	// answers without storing its refusal. Non-positive does not wait.
	RegistrationRecheckTimeout time.Duration
	// How long a bucket's socket stays open with no watch and no held pair.
	// Non-positive keeps it open.
	IdleBucketTimeout time.Duration
	// How often an owner with held pairs releases expired route refusals, so
	// an idle owner lets go of their pairs too. Non-positive leaves them to
	// the ring's next operation.
	HeldPairExpiryInterval time.Duration
	// The most channels in one SSUBSCRIBE or SUNSUBSCRIBE.
	SubscribeBatchSize int
	// The bucket sockets' detection bounds (server.RedisShardSubscriptionSettings):
	// an acknowledgement, of the bucket channel or of a pair channel's subscribe
	// or unsubscribe, overdue past the first replaces the socket.
	SubscriptionAcknowledgementTimeout time.Duration
	SubscriptionIncomingTimeout        time.Duration
	SubscriptionReceiveTimeout         time.Duration
	// The pause before a replaced socket reopens.
	SubscriptionReconnectTimeout time.Duration
}

// The route rejection bound holds about 6 MiB. Old sdks retry a refused
// route every 5s, and an edge host's load balancer spreads the retries over
// its 4-5 api processes, so a process sees one route only every 25-40s. At
// 12s the ring answered about a quarter of those retries; 60s answers most.
// An origin edge or a subscription acknowledgement still removes a refusal at
// once, so only an edge this owner never receives refuses an identical
// request for up to the full 60s.
//
// Terminal refusals hold at most two generations of 131072, about 76 MiB at
// about 300 bytes a refusal, and only while a storm fills them. Old sdks
// retry each stale route every 5s for as long as they hold its return, and
// each retry is a new control request that DNS can send to any edge host, so
// every api process sees nearly every live route. On 2026-10-19 about 70k such
// routes to retired prober windows were live, more than two generations of
// 32768 hold: a process turned both over before most routes came back, about
// two of three retries missed, and each miss read PostgreSQL twice, about 20k
// reads a second. One generation now holds the 115k routes of the 10-12 peak.
// An inactive client is never reactivated, so the timeout only bounds the cost
// of a mistake. Six hours spans the hours an old sdk keeps retrying, which on
// 10-19 barely decayed over four hours without new probes.
//
// An api process holds about 150 pairs (11-23), so 8192 leaves room for
// bursts while bounding one owner's subscriptions. The re-check waits about
// one first-retry interval of the companion wait. A minute spans the api's
// gaps between waits in a bucket, so only the rarely waiting owners (the
// proxy, the taskworker) close their sockets. The sockets keep the strict
// one-second reconnect pace rather than the shared growing backoff: an owner
// holds at most sixteen.
func DefaultContractOriginNotificationSettings() ContractOriginNotificationSettings {
	return ContractOriginNotificationSettings{
		MaxWaiters:                         8192,
		PublishQueueSize:                   1024,
		PublishWorkers:                     2,
		MaxRouteRejections:                 16384,
		RouteRejectionTimeout:              60 * time.Second,
		TerminalRouteRejectionCount:        131072,
		TerminalRouteRejectionTimeout:      6 * time.Hour,
		PublishTimeout:                     time.Second,
		MaxHeldPairs:                       8192,
		RegistrationRecheckTimeout:         100 * time.Millisecond,
		IdleBucketTimeout:                  60 * time.Second,
		HeldPairExpiryInterval:             time.Second,
		SubscribeBatchSize:                 256,
		SubscriptionAcknowledgementTimeout: 2 * time.Second,
		SubscriptionIncomingTimeout:        2 * time.Second,
		SubscriptionReceiveTimeout:         time.Second,
		SubscriptionReconnectTimeout:       time.Second,
	}
}

type contractOriginSubscription = server.RedisShardSubscription

// A socket that can also end shard subscriptions; `*redis.PubSub` is one. On
// a socket without it a released pair keeps its subscription until the socket
// closes.
type contractOriginUnsubscriber interface {
	SUnsubscribe(ctx context.Context, channels ...string) error
}

// One published edge: a channel and its payload.
type contractOriginPublication struct {
	channel string
	payload string
}

// A pair's registrations, holds, and its channel's state on its bucket's
// socket, all under the owner's state lock except the version and lookup
// state, which have their own.
type contractOriginPair struct {
	version *connect.MonitorValue[uint64]
	// the registered watches
	refs   int
	lookup contractOriginLookupState
	// holders past a miss: watches that missed or read fenced, and refusals
	holds int
	// on the bucket's live socket: an SSUBSCRIBE was written, the stream
	// acknowledged it, and a SUNSUBSCRIBE was written after it
	subscribeSent   bool
	acked           bool
	unsubscribeSent bool
	// closed while the subscription is acknowledged on the live socket and no
	// SUNSUBSCRIBE has followed: a read armed then is fenced
	acknowledged chan struct{}
	// its subscription was acknowledged on a socket since lost, so the next
	// acknowledgement resyncs it
	resync bool
}

func newContractOriginPair() *contractOriginPair {
	return &contractOriginPair{
		version:      connect.NewMonitorValue(uint64(0)),
		acknowledged: make(chan struct{}),
	}
}

type contractOriginBucket struct {
	pairs map[[32]byte]*contractOriginPair
	// registered watches and held pairs; either needs the socket
	watches   int
	heldPairs int
	// wakes the bucket's manager
	wake chan struct{}
	// the manager runs a socket for the bucket
	open bool
	// the socket whose subscription started last, and the generation of the
	// sockets' pair state
	socket     contractOriginSubscription
	generation uint64
	// pair channels the socket acknowledged and has not unsubscribed
	ackedPairs int
	// pairs whose channel may need a command
	pendingKeys [][32]byte
	// commands written and not yet answered, by when they were written
	outstandingKeySentTimes map[[32]byte]time.Time
	// when the bucket last stopped needing a socket; zero while it needs one
	idleSince time.Time
}

func (self *contractOriginBucket) neededWithLock() bool {
	return 0 < self.watches || 0 < self.heldPairs
}

func wakeContractOriginBucket(bucket *contractOriginBucket) {
	select {
	case bucket.wake <- struct{}{}:
	default:
	}
}

// All methods are safe for concurrent use. Close prevents new registrations and
// publications, cancels I/O, and joins every manager, reader and publisher
// owned here. A pair's lookup lock may be taken inside the state lock, never
// the reverse.
type ContractOriginNotifications struct {
	ctx                context.Context
	cancel             context.CancelFunc
	settings           ContractOriginNotificationSettings
	subscriptionSource *server.RedisShardSubscriptionSource
	publish            func(context.Context, []contractOriginPublication) error
	queue              chan [32]byte
	workers            sync.WaitGroup
	closeOnce          sync.Once
	stateLock          sync.Mutex
	closed             bool
	waiters            int
	heldPairs          int
	// when expired refusals last released their pairs
	expiryTime time.Time
	buckets    [contractOriginBucketCount]contractOriginBucket
	// Each owns its own lock; never taken while holding stateLock.
	routeRejections         *contractRouteRejections
	terminalRouteRejections *contractTerminalRouteRejections
}

// The constructor starts the owner loops, but no subscription is opened until a
// request registers in its bucket. Settings are copied, not shared mutably.
func NewContractOriginNotifications(ctx context.Context, settings ContractOriginNotificationSettings) *ContractOriginNotifications {
	return newContractOriginNotifications(ctx, settings, server.DefaultRedisShardSubscriptionSource().Open, publishContractOrigin)
}

func newContractOriginNotifications(
	ctx context.Context,
	settings ContractOriginNotificationSettings,
	subscribe func(context.Context, string) (contractOriginSubscription, error),
	publish func(context.Context, []contractOriginPublication) error,
) *ContractOriginNotifications {
	if settings.MaxWaiters <= 0 || settings.PublishQueueSize <= 0 || settings.PublishWorkers <= 0 || settings.PublishWorkers > contractOriginBucketCount ||
		settings.MaxRouteRejections < 0 || settings.RouteRejectionTimeout < 0 ||
		settings.TerminalRouteRejectionCount < 0 || settings.TerminalRouteRejectionTimeout < 0 ||
		settings.PublishTimeout <= 0 || settings.MaxHeldPairs < 0 || settings.SubscribeBatchSize <= 0 ||
		settings.SubscriptionAcknowledgementTimeout <= 0 || settings.SubscriptionIncomingTimeout <= 0 ||
		settings.SubscriptionReceiveTimeout <= 0 || settings.SubscriptionReconnectTimeout <= 0 {
		panic("invalid contract origin notification budgets")
	}
	ctx, cancel := context.WithCancel(ctx)
	// a lost subscription reloads the process slot map before it reopens
	subscriptionSource := server.DefaultRedisShardSubscriptionSource()
	subscriptionSource.Open = subscribe
	notifications := &ContractOriginNotifications{
		ctx: ctx, cancel: cancel, settings: settings, subscriptionSource: subscriptionSource, publish: publish,
		queue:                   make(chan [32]byte, settings.PublishQueueSize),
		routeRejections:         newContractRouteRejections(settings.MaxRouteRejections, settings.RouteRejectionTimeout),
		terminalRouteRejections: newContractTerminalRouteRejections(settings.TerminalRouteRejectionCount, settings.TerminalRouteRejectionTimeout),
	}
	for i := range notifications.buckets {
		notifications.buckets[i] = contractOriginBucket{
			pairs:                   map[[32]byte]*contractOriginPair{},
			wake:                    make(chan struct{}, 1),
			outstandingKeySentTimes: map[[32]byte]time.Time{},
		}
		notifications.workers.Add(1)
		go func() { defer notifications.workers.Done(); notifications.runBucket(i) }()
	}
	for range settings.PublishWorkers {
		notifications.workers.Add(1)
		go func() { defer notifications.workers.Done(); notifications.runPublisher() }()
	}
	return notifications
}

// An owner whose subscription sockets and publications the caller supplies, for
// tests in other packages that must order an origin edge against a request.
// publish receives each channel of an origin's pipeline in turn.
func Testing_NewContractOriginNotifications(
	ctx context.Context,
	settings ContractOriginNotificationSettings,
	subscribe func(context.Context, string) (server.RedisShardSubscription, error),
	publish func(context.Context, string, string) error,
) *ContractOriginNotifications {
	return newContractOriginNotifications(ctx, settings, subscribe, func(ctx context.Context, publications []contractOriginPublication) error {
		var errs []error
		for _, publication := range publications {
			errs = append(errs, publish(ctx, publication.channel, publication.payload))
		}
		return errors.Join(errs...)
	})
}

// The channel and payload of each edge an origin committed from sourceId to
// destinationId is published as, in pipeline order, for tests in other
// packages that deliver or withhold them.
func Testing_ContractOriginPublications(sourceId server.Id, destinationId server.Id) [][2]string {
	var channelPayloads [][2]string
	for _, publication := range contractOriginPublications(contractOriginPairHash(sourceId, destinationId)) {
		channelPayloads = append(channelPayloads, [2]string{publication.channel, publication.payload})
	}
	return channelPayloads
}

type contractOriginNotificationsContextKey struct{}

// Only an explicit lifecycle owner installs this context value; bare contexts
// intentionally have no hidden singleton or lazily created background service.
func WithContractOriginNotifications(ctx context.Context, notifications *ContractOriginNotifications) context.Context {
	return context.WithValue(ctx, contractOriginNotificationsContextKey{}, notifications)
}

func GetContractOriginNotifications(ctx context.Context) *ContractOriginNotifications {
	notifications, _ := ctx.Value(contractOriginNotificationsContextKey{}).(*ContractOriginNotifications)
	return notifications
}

// Domain-separated ordered identities are never transmitted as plaintext IDs.
// Reversing a pair changes both its digest and the notification being awaited.
func contractOriginPairHash(sourceId server.Id, destinationId server.Id) [32]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("urnetwork/contract-origin/v1/"))
	_, _ = hash.Write(sourceId.Bytes())
	_, _ = hash.Write(destinationId.Bytes())
	var pair [32]byte
	copy(pair[:], hash.Sum(nil))
	return pair
}

func contractOriginBucketIndex(key [32]byte) int {
	return int(key[0]) % contractOriginBucketCount
}

// The bucket's channel: every edge of the bucket, as the pair's digest in hex.
// It is also the socket's anchor, the channel its liveness rules keep.
func contractOriginChannel(bucket int) string {
	return fmt.Sprintf("contract-origin:v1:{%02x}", bucket)
}

// A pair's channel carries the bucket's hash tag, so it shares the bucket
// channel's slot, node and socket. Its edges carry no payload.
func contractOriginPairChannelPrefix(bucket int) string {
	return fmt.Sprintf("contract-origin:v2:{%02x}:", bucket)
}

func contractOriginPairChannel(key [32]byte) string {
	return contractOriginPairChannelPrefix(contractOriginBucketIndex(key)) + hex.EncodeToString(key[:])
}

// The pair a channel of the bucket names, if it is one of the bucket's pair
// channels.
func contractOriginPairChannelKey(bucket int, prefix string, channel string) ([32]byte, bool) {
	var key [32]byte
	digest, ok := strings.CutPrefix(channel, prefix)
	if !ok || len(digest) != 64 {
		return key, false
	}
	if _, err := hex.Decode(key[:], []byte(digest)); err != nil || contractOriginBucketIndex(key) != bucket {
		return key, false
	}
	return key, true
}

// The bucket channel's edge, or a pair channel's of the same bucket. pairChannel
// is whether it came on a pair channel.
func contractOriginMessageKey(bucket int, channel string, prefix string, message server.RedisMessage) (key [32]byte, pairChannel bool, ok bool) {
	if message.Channel != channel {
		key, ok = contractOriginPairChannelKey(bucket, prefix, message.Channel)
		return key, true, ok
	}
	if len(message.Payload) != 64 {
		return key, false, false
	}
	if _, err := hex.Decode(key[:], []byte(message.Payload)); err != nil || contractOriginBucketIndex(key) != bucket {
		return key, false, false
	}
	return key, false, true
}

// The bucket channel carries the digest, for owners from before Build A and
// every owner with a socket in the bucket. The pair channel names the pair,
// so its edge carries nothing.
func contractOriginPublications(key [32]byte) []contractOriginPublication {
	return []contractOriginPublication{
		{channel: contractOriginChannel(contractOriginBucketIndex(key)), payload: hex.EncodeToString(key[:])},
		{channel: contractOriginPairChannel(key), payload: ""},
	}
}

// A watch retains one registration. Its monitor is armed immediately before the
// authoritative read; an acknowledgement or commit arriving during that read is
// therefore still visible afterward. Closing one watch never removes its peers.
// A watch holds its pair from its first fenced read or its first miss until it
// closes. Used by one request at a time.
type ContractOriginWatch struct {
	owner     *ContractOriginNotifications
	key       [32]byte
	pair      *contractOriginPair
	closeOnce sync.Once
	// under the owner's state lock
	holding bool
	closed  bool
	// whether the last Update was fenced, under the owner's state lock
	fenced bool
}

func (self *ContractOriginNotifications) Watch(sourceId server.Id, destinationId server.Id) *ContractOriginWatch {
	if self == nil {
		return nil
	}
	key := contractOriginPairHash(sourceId, destinationId)
	var watch *ContractOriginWatch
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.ctx.Err() != nil || self.waiters >= self.settings.MaxWaiters {
			return
		}
		bucket := &self.buckets[contractOriginBucketIndex(key)]
		pair := bucket.pairs[key]
		if pair == nil {
			pair = newContractOriginPair()
			bucket.pairs[key] = pair
		}
		pair.refs += 1
		self.waiters += 1
		bucket.watches += 1
		bucket.idleSince = time.Time{}
		if !bucket.open {
			wakeContractOriginBucket(bucket)
		}
		watch = &ContractOriginWatch{owner: self, key: key, pair: pair}
	}()
	if watch == nil {
		contractOriginNotificationCounter.WithLabelValues("registration_declined").Inc()
	}
	return watch
}

// The watch's bucket while the watch and its owner are live.
func (self *ContractOriginWatch) bucketWithLock() (*contractOriginBucket, bool) {
	if self.closed || self.owner.closed || self.owner.ctx.Err() != nil {
		return nil, false
	}
	bucket := &self.owner.buckets[contractOriginBucketIndex(self.key)]
	if bucket.pairs[self.key] != self.pair {
		return nil, false
	}
	return bucket, true
}

// Arms the pair's version channel immediately before an authoritative read.
// fenced is whether the pair's subscription was acknowledged when it was armed;
// the watch then holds the pair, so it stays subscribed through the read's
// verdict. Only a fenced read may record a missing-origin refusal.
func (self *ContractOriginWatch) Update() (<-chan struct{}, bool) {
	if self == nil {
		return nil, false
	}
	fenced := false
	capped := false
	func() {
		self.owner.stateLock.Lock()
		defer self.owner.stateLock.Unlock()
		self.fenced = false
		bucket, ok := self.bucketWithLock()
		if !ok {
			return
		}
		select {
		case <-self.pair.acknowledged:
		default:
			return
		}
		if !self.holding {
			var held bool
			held, capped = self.owner.holdWithLock(bucket, self.key, self.pair)
			if !held {
				return
			}
			self.holding = true
		}
		self.fenced = true
		fenced = true
	}()
	if capped {
		contractOriginNotificationCounter.WithLabelValues("pair_cap").Inc()
	}
	_, update := self.pair.version.Get()
	return update, fenced
}

// Called after a read armed by Update found no origin. Holds the pair from now
// until Close, and returns the channel that closes once the pair's subscription
// is acknowledged (already closed if another holder's acknowledgement came
// after the read was armed), or nil when that read was fenced. held is false
// when the pair cannot be held (the owner's MaxHeldPairs, or a closed owner or
// watch): no subscription is coming, and no read of this watch is fenced.
func (self *ContractOriginWatch) Missed() (registration <-chan struct{}, held bool) {
	if self == nil {
		return nil, false
	}
	capped := false
	func() {
		self.owner.stateLock.Lock()
		defer self.owner.stateLock.Unlock()
		bucket, ok := self.bucketWithLock()
		if !ok {
			return
		}
		if !self.holding {
			var holding bool
			holding, capped = self.owner.holdWithLock(bucket, self.key, self.pair)
			if !holding {
				return
			}
			self.holding = true
		}
		held = true
		if !self.fenced {
			registration = self.pair.acknowledged
		}
	}()
	if capped {
		contractOriginNotificationCounter.WithLabelValues("pair_cap").Inc()
	}
	return
}

// For a read that missed unfenced: holds the pair (see Missed) and waits up to
// the owner's RegistrationRecheckTimeout for its subscription's
// acknowledgement. Returns whether a read armed now is fenced.
func (self *ContractOriginWatch) AwaitRegistration(ctx context.Context) bool {
	registration, held := self.Missed()
	if !held {
		return false
	}
	if registration == nil {
		return true
	}
	select {
	case <-registration:
		return true
	default:
	}
	timeout := self.owner.settings.RegistrationRecheckTimeout
	if timeout <= 0 {
		return false
	}
	select {
	case <-registration:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(timeout):
		return false
	}
}

func (self *ContractOriginWatch) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.owner.stateLock.Lock()
		defer self.owner.stateLock.Unlock()
		self.closed = true
		if self.owner.closed {
			return
		}
		bucket := &self.owner.buckets[contractOriginBucketIndex(self.key)]
		if bucket.pairs[self.key] != self.pair {
			return
		}
		if self.holding {
			self.holding = false
			self.owner.releaseWithLock(bucket, self.key, self.pair)
		}
		self.pair.refs -= 1
		if self.pair.refs == 0 {
			// the missing hint belongs to the watches: a pair its holds keep
			// starts the next watch afresh (contract_origin_lookup.go)
			self.pair.lookup.invalidate()
		}
		self.owner.waiters -= 1
		bucket.watches -= 1
		self.owner.retireWithLock(bucket, self.key, self.pair)
	})
}

// Holds the pair, requesting its channel's subscription if no holder had.
// Declined at the owner's MaxHeldPairs, with capped set.
func (self *ContractOriginNotifications) holdWithLock(bucket *contractOriginBucket, key [32]byte, pair *contractOriginPair) (held bool, capped bool) {
	if pair.holds == 0 {
		if self.settings.MaxHeldPairs <= self.heldPairs {
			return false, true
		}
		self.heldPairs += 1
		bucket.heldPairs += 1
		contractOriginHeldPairsGauge.Inc()
		bucket.idleSince = time.Time{}
		// a pair whose subscription was never released keeps it; one being
		// released resubscribes once its unsubscribe is answered
		if !pair.subscribeSent {
			bucket.pendingKeys = append(bucket.pendingKeys, key)
		}
		wakeContractOriginBucket(bucket)
	}
	pair.holds += 1
	return true, false
}

// Releases one hold; the last releases the pair's subscription.
func (self *ContractOriginNotifications) releaseWithLock(bucket *contractOriginBucket, key [32]byte, pair *contractOriginPair) {
	pair.holds -= 1
	if 0 < pair.holds {
		return
	}
	self.heldPairs -= 1
	bucket.heldPairs -= 1
	contractOriginHeldPairsGauge.Dec()
	if pair.subscribeSent && !pair.unsubscribeSent {
		bucket.pendingKeys = append(bucket.pendingKeys, key)
		wakeContractOriginBucket(bucket)
	}
}

// Drops a pair nothing references any more, and notes when the bucket stops
// needing its socket.
func (self *ContractOriginNotifications) retireWithLock(bucket *contractOriginBucket, key [32]byte, pair *contractOriginPair) {
	if pair.refs == 0 && pair.holds == 0 && !pair.subscribeSent && !pair.unsubscribeSent {
		delete(bucket.pairs, key)
	}
	if !bucket.neededWithLock() && bucket.idleSince.IsZero() {
		bucket.idleSince = time.Now()
		wakeContractOriginBucket(bucket)
	}
}

// A refusal for the pair is about to be stored: it holds the pair first.
func (self *ContractOriginNotifications) holdRefusalPair(key [32]byte) bool {
	held := false
	capped := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.ctx.Err() != nil {
			return
		}
		bucket := &self.buckets[contractOriginBucketIndex(key)]
		pair := bucket.pairs[key]
		created := pair == nil
		if created {
			pair = newContractOriginPair()
			bucket.pairs[key] = pair
		}
		held, capped = self.holdWithLock(bucket, key, pair)
		if !held && created {
			delete(bucket.pairs, key)
		}
	}()
	if capped {
		contractOriginNotificationCounter.WithLabelValues("pair_cap").Inc()
	}
	return held
}

// Releases the holds of refusals the ring no longer keeps, one per refusal.
// After Close the registry is gone and there is nothing to release.
func (self *ContractOriginNotifications) releaseRefusalHolds(keys [][32]byte) {
	if len(keys) == 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	for _, key := range keys {
		bucket := &self.buckets[contractOriginBucketIndex(key)]
		pair := bucket.pairs[key]
		if pair == nil || pair.holds == 0 {
			continue
		}
		self.releaseWithLock(bucket, key, pair)
		self.retireWithLock(bucket, key, pair)
	}
}

// Releases the pairs of expired refusals, at most once an interval per owner.
func (self *ContractOriginNotifications) expireRouteRejections(now time.Time) {
	run := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.expiryTime.IsZero() || !now.Before(self.expiryTime.Add(self.settings.HeldPairExpiryInterval)) {
			self.expiryTime = now
			run = true
		}
	}()
	if run {
		self.releaseRefusalHolds(self.routeRejections.expire(now))
	}
}

// Enqueue only after a successful transaction commit. Queue saturation and Redis
// failures never change the already committed contract's response or accounting.
func notifyCommittedContractOrigin(ctx context.Context, sourceId server.Id, destinationId server.Id) {
	notifications := GetContractOriginNotifications(ctx)
	if notifications == nil {
		contractOriginNotificationCounter.WithLabelValues("unowned").Inc()
		return
	}
	key := contractOriginPairHash(sourceId, destinationId)
	// A route refused here for this origin's pair is open now. Other owners
	// learn it from the published edge (see `notifyBucket`).
	_, released := notifications.routeRejections.invalidate(key)
	notifications.releaseRefusalHolds(released)
	event := "owner_closed"
	func() {
		notifications.stateLock.Lock()
		defer notifications.stateLock.Unlock()
		if notifications.closed || notifications.ctx.Err() != nil {
			return
		}
		select {
		case notifications.queue <- key:
			event = "enqueued"
		default:
			event = "queue_full"
		}
	}()
	contractOriginNotificationCounter.WithLabelValues(event).Inc()
}

func (self *ContractOriginNotifications) runPublisher() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case key := <-self.queue:
			if self.ctx.Err() != nil {
				return
			}
			publishCtx, cancel := context.WithTimeout(self.ctx, self.settings.PublishTimeout)
			err := self.publish(publishCtx, contractOriginPublications(key))
			cancel()
			event := "published"
			if err != nil {
				event = "publish_failed"
			}
			contractOriginNotificationCounter.WithLabelValues(event).Inc()
		}
	}
}

// Opens the bucket's socket while a watch or a held pair needs it, and runs it
// until it is idle (see `runBucketSocket`).
func (self *ContractOriginNotifications) runBucket(index int) {
	bucket := &self.buckets[index]
	for {
		for {
			needed := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if !self.closed && bucket.neededWithLock() {
					bucket.open = true
					needed = true
				}
			}()
			if needed {
				break
			}
			select {
			case <-self.ctx.Done():
				return
			case <-bucket.wake:
			}
		}
		self.runBucketSocket(index)
		if self.ctx.Err() != nil {
			return
		}
	}
}

// Each socket must acknowledge the bucket channel and keep proving read-side
// liveness; a slot move, an error reply or missing evidence replaces it
// (server.RunRedisShardSubscription). The manager writes the held pairs'
// subscriptions and the released pairs' unsubscriptions on the live socket,
// replaces it when one is overdue or a write fails, and returns once the
// bucket has gone IdleBucketTimeout without a watch or a held pair. The
// socket's pair state is reset once its reader has stopped.
func (self *ContractOriginNotifications) runBucketSocket(index int) {
	bucket := &self.buckets[index]
	socketCtx, cancel := context.WithCancel(self.ctx)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		server.RunRedisShardSubscription(socketCtx, contractOriginChannel(index), self.subscriptionSettings(), self.subscriptionSource, self.bucketHandler(index))
	}()
	defer func() {
		cancel()
		<-readerDone
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.resetBucketSocketWithLock(bucket)
		bucket.open = false
	}()
	for {
		now := time.Now()
		var socket contractOriginSubscription
		var subscribeChannels []string
		var unsubscribeChannels []string
		idle := false
		overdue := false
		expire := false
		var wakeTime time.Time
		earliest := func(deadline time.Time) {
			if wakeTime.IsZero() || deadline.Before(wakeTime) {
				wakeTime = deadline
			}
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if !bucket.neededWithLock() && 0 < self.settings.IdleBucketTimeout {
				if bucket.idleSince.IsZero() {
					bucket.idleSince = now
				}
				closeTime := bucket.idleSince.Add(self.settings.IdleBucketTimeout)
				if !now.Before(closeTime) {
					idle = true
					return
				}
				earliest(closeTime)
			}
			socket, subscribeChannels, unsubscribeChannels = self.bucketCommandsWithLock(bucket, now)
			for _, sentTime := range bucket.outstandingKeySentTimes {
				deadline := sentTime.Add(self.settings.SubscriptionAcknowledgementTimeout)
				if !now.Before(deadline) {
					overdue = true
				} else {
					earliest(deadline)
				}
			}
			if 0 < bucket.heldPairs && 0 < self.settings.HeldPairExpiryInterval {
				expire = true
				earliest(now.Add(self.settings.HeldPairExpiryInterval))
			}
		}()
		if idle {
			contractOriginNotificationCounter.WithLabelValues("bucket_closed").Inc()
			return
		}
		if socket != nil && (overdue || !self.writeBucketCommands(socketCtx, socket, subscribeChannels, unsubscribeChannels)) {
			// the reader sees the closed socket, and its Lost resets the pairs
			contractOriginNotificationCounter.WithLabelValues("subscribe_failed").Inc()
			_ = socket.Close()
		}
		if expire {
			self.expireRouteRejections(now)
		}
		var timer <-chan time.Time
		if !wakeTime.IsZero() {
			timer = time.After(wakeTime.Sub(now))
		}
		select {
		case <-self.ctx.Done():
			return
		case <-bucket.wake:
		case <-timer:
		}
	}
}

// Takes the pending pairs' commands for the live socket, marking them written.
// A pair releasing its channel gets a fresh acknowledged channel at once: from
// the SUNSUBSCRIBE on, no read of it is fenced.
func (self *ContractOriginNotifications) bucketCommandsWithLock(bucket *contractOriginBucket, now time.Time) (socket contractOriginSubscription, subscribeChannels []string, unsubscribeChannels []string) {
	pendingKeys := bucket.pendingKeys
	bucket.pendingKeys = bucket.pendingKeys[:0]
	socket = bucket.socket
	if socket == nil {
		// the next socket's subscription start requests every held pair
		return nil, nil, nil
	}
	_, unsubscribable := socket.(contractOriginUnsubscriber)
	for _, key := range pendingKeys {
		pair := bucket.pairs[key]
		if pair == nil {
			continue
		}
		switch {
		case 0 < pair.holds && !pair.subscribeSent && !pair.unsubscribeSent:
			pair.subscribeSent = true
			bucket.outstandingKeySentTimes[key] = now
			subscribeChannels = append(subscribeChannels, contractOriginPairChannel(key))
		case pair.holds == 0 && pair.subscribeSent && !pair.unsubscribeSent && unsubscribable:
			pair.unsubscribeSent = true
			bucket.outstandingKeySentTimes[key] = now
			select {
			case <-pair.acknowledged:
				pair.acknowledged = make(chan struct{})
			default:
			}
			unsubscribeChannels = append(unsubscribeChannels, contractOriginPairChannel(key))
		}
	}
	return socket, subscribeChannels, unsubscribeChannels
}

// Writes the commands in batches of SubscribeBatchSize channels. Returns false
// when a write failed.
func (self *ContractOriginNotifications) writeBucketCommands(ctx context.Context, socket contractOriginSubscription, subscribeChannels []string, unsubscribeChannels []string) bool {
	if len(subscribeChannels) == 0 && len(unsubscribeChannels) == 0 {
		return true
	}
	writeCtx, cancel := context.WithTimeout(ctx, self.settings.SubscriptionAcknowledgementTimeout)
	defer cancel()
	batchSize := self.settings.SubscribeBatchSize
	for start := 0; start < len(subscribeChannels); start += batchSize {
		channels := subscribeChannels[start:min(start+batchSize, len(subscribeChannels))]
		if err := socket.SSubscribe(writeCtx, channels...); err != nil {
			return false
		}
		contractOriginNotificationCounter.WithLabelValues("subscribed_pair").Add(float64(len(channels)))
	}
	if len(unsubscribeChannels) == 0 {
		return true
	}
	unsubscriber, ok := socket.(contractOriginUnsubscriber)
	if !ok {
		return true
	}
	for start := 0; start < len(unsubscribeChannels); start += batchSize {
		if err := unsubscriber.SUnsubscribe(writeCtx, unsubscribeChannels[start:min(start+batchSize, len(unsubscribeChannels))]...); err != nil {
			return false
		}
	}
	return true
}

// The bucket's socket is gone, or replaced: every pair channel it carried went
// with it. A pair acknowledged on it gets a fresh acknowledged channel, so no
// read is fenced until its resubscription, which then resyncs it.
func (self *ContractOriginNotifications) resetBucketSocketWithLock(bucket *contractOriginBucket) {
	bucket.socket = nil
	bucket.generation += 1
	bucket.ackedPairs = 0
	bucket.pendingKeys = bucket.pendingKeys[:0]
	clear(bucket.outstandingKeySentTimes)
	for key, pair := range bucket.pairs {
		select {
		case <-pair.acknowledged:
			pair.acknowledged = make(chan struct{})
			pair.resync = true
		default:
		}
		pair.subscribeSent = false
		pair.acked = false
		pair.unsubscribeSent = false
		if pair.refs == 0 && pair.holds == 0 {
			delete(bucket.pairs, key)
		}
	}
}

func (self *ContractOriginNotifications) bucketHandler(index int) *server.RedisShardSubscriptionHandler {
	bucket := &self.buckets[index]
	channel := contractOriginChannel(index)
	pairChannelPrefix := contractOriginPairChannelPrefix(index)
	return &server.RedisShardSubscriptionHandler{
		Socket: func(subscription server.RedisShardSubscription) {
			replace := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				replace = bucket.socket == subscription
				self.resetBucketSocketWithLock(bucket)
				if !replace {
					bucket.socket = subscription
				}
			}()
			if replace {
				// An acknowledgement of the bucket channel that nothing asked
				// for, on the live socket: what its pair channels carry is
				// unknown, so the socket is replaced.
				_ = subscription.Close()
			}
		},
		Subscribed: func(resubscribed bool) {
			label := "subscribed"
			if resubscribed {
				label = "reconnected"
			}
			contractOriginNotificationCounter.WithLabelValues(label).Inc()
			// the bucket channel's edges published while no subscription
			// existed are gone
			self.notifyBucket(index, nil, false)
			// then every pair still held subscribes on the socket
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if bucket.socket == nil {
				return
			}
			for key, pair := range bucket.pairs {
				if 0 < pair.holds {
					bucket.pendingKeys = append(bucket.pendingKeys, key)
				}
			}
			wakeContractOriginBucket(bucket)
		},
		Message: func(message server.RedisMessage) bool {
			key, pairChannel, ok := contractOriginMessageKey(index, channel, pairChannelPrefix, message)
			if !ok {
				contractOriginNotificationCounter.WithLabelValues("invalid_message").Inc()
				return false
			}
			contractOriginNotificationCounter.WithLabelValues("received").Inc()
			if pairChannel {
				contractOriginNotificationCounter.WithLabelValues("v2_received").Inc()
			}
			self.notifyBucket(index, &key, !pairChannel)
			return true
		},
		Notice: func(notice server.RedisSubscriptionNotice) error {
			key, ok := contractOriginPairChannelKey(index, pairChannelPrefix, notice.Channel)
			if !ok {
				return nil
			}
			return self.pairNotice(index, key, notice)
		},
		Lost: func(error) {
			contractOriginNotificationCounter.WithLabelValues("subscription_failed").Inc()
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.resetBucketSocketWithLock(bucket)
		},
	}
}

// A pair channel's subscribe or unsubscribe notice on the bucket's socket. One
// nothing asked for, or a shard channel count that differs from the bucket
// channel and the acknowledged pair channels, replaces the socket.
//
// A pair's acknowledgement invalidates its missing hint first: a read armed
// before it is not fenced, and its absence must not be shared past it. A
// resubscription after a lost socket may have missed edges, so it also wakes
// the pair's waiters and removes its refusals, as the bucket channel's own
// acknowledgement does. The acknowledged channel closes last.
func (self *ContractOriginNotifications) pairNotice(index int, key [32]byte, notice server.RedisSubscriptionNotice) error {
	bucket := &self.buckets[index]
	var acknowledgedPair *contractOriginPair
	var generation uint64
	resync := false
	event := ""
	var err error
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		pair := bucket.pairs[key]
		switch notice.Kind {
		case "ssubscribe":
			if pair == nil || !pair.subscribeSent || pair.acked {
				err = errContractOriginUnrequestedNotice
				return
			}
			pair.acked = true
			bucket.ackedPairs += 1
			if notice.Count != 1+bucket.ackedPairs {
				err = errContractOriginChannelCount
				return
			}
			event = "pair_acknowledged"
			if !pair.unsubscribeSent {
				delete(bucket.outstandingKeySentTimes, key)
				acknowledgedPair = pair
				generation = bucket.generation
				resync = pair.resync
				pair.resync = false
			}
		case "sunsubscribe":
			if pair == nil || !pair.unsubscribeSent {
				err = errContractOriginUnrequestedNotice
				return
			}
			if pair.acked {
				pair.acked = false
				bucket.ackedPairs -= 1
			}
			if notice.Count != 1+bucket.ackedPairs {
				err = errContractOriginChannelCount
				return
			}
			event = "pair_released"
			pair.subscribeSent = false
			pair.unsubscribeSent = false
			delete(bucket.outstandingKeySentTimes, key)
			if 0 < pair.holds {
				bucket.pendingKeys = append(bucket.pendingKeys, key)
				wakeContractOriginBucket(bucket)
			} else {
				self.retireWithLock(bucket, key, pair)
			}
		}
	}()
	if err != nil {
		return err
	}
	if event != "" {
		contractOriginNotificationCounter.WithLabelValues(event).Inc()
	}
	if acknowledgedPair == nil {
		return nil
	}
	acknowledgedPair.lookup.invalidate()
	if resync {
		acknowledgedPair.version.Update(func(version uint64) uint64 { return version + 1 })
		_, released := self.routeRejections.invalidate(key)
		self.releaseRefusalHolds(released)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if bucket.generation == generation && bucket.pairs[key] == acknowledgedPair && acknowledgedPair.acked && !acknowledgedPair.unsubscribeSent {
		select {
		case <-acknowledgedPair.acknowledged:
		default:
			close(acknowledgedPair.acknowledged)
		}
	}
	return nil
}

func (self *ContractOriginNotifications) subscriptionSettings() *server.RedisShardSubscriptionSettings {
	settings := server.DefaultRedisShardSubscriptionSettings()
	settings.AcknowledgementTimeout = self.settings.SubscriptionAcknowledgementTimeout
	settings.IncomingTimeout = self.settings.SubscriptionIncomingTimeout
	settings.ReceiveTimeout = self.settings.SubscriptionReceiveTimeout
	settings.MinReconnectTimeout = self.settings.SubscriptionReconnectTimeout
	settings.MaxReconnectTimeout = self.settings.SubscriptionReconnectTimeout
	return settings
}

// Snapshot under the registry lock, then notify outside it: monitor operations
// never nest an external lock inside the registry's state lock. An edge (a
// non-nil key) counts what it found; a resync counts nothing. A bucket channel
// edge for a pair whose own channel is subscribed counts toward the shadow
// comparison with the pair channel's edges.
func (self *ContractOriginNotifications) notifyBucket(index int, key *[32]byte, bucketChannel bool) {
	var pairs []*contractOriginPair
	waiter := false
	shadow := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if key != nil {
			if pair := self.buckets[index].pairs[*key]; pair != nil {
				pairs = append(pairs, pair)
				waiter = 0 < pair.refs
				shadow = bucketChannel && pair.acked
			}
		} else {
			for _, pair := range self.buckets[index].pairs {
				pairs = append(pairs, pair)
			}
		}
	}()
	for _, pair := range pairs {
		pair.lookup.invalidate()
		pair.version.Update(func(version uint64) uint64 { return version + 1 })
	}
	// Remove route rejections only after the version edges: a verdict whose
	// fence is one of those channels is then either refused by its closed
	// fence or recorded before this removal (see `contractRouteRejections.put`).
	if key == nil {
		self.releaseRefusalHolds(self.routeRejections.invalidateBucket(index))
		return
	}
	removed, released := self.routeRejections.invalidate(*key)
	self.releaseRefusalHolds(released)
	if shadow {
		contractOriginNotificationCounter.WithLabelValues("v1_for_acknowledged_pair").Inc()
	}
	if waiter {
		contractOriginNotificationCounter.WithLabelValues("matched_waiter").Inc()
	}
	if removed {
		contractOriginNotificationCounter.WithLabelValues("matched_refusal").Inc()
	}
	if !waiter && !removed {
		contractOriginNotificationCounter.WithLabelValues("unmatched").Inc()
	}
}

func (self *ContractOriginNotifications) Close() {
	self.closeOnce.Do(func() {
		self.stateLock.Lock()
		self.closed = true
		self.stateLock.Unlock()
		// no edge can remove a route rejection once the readers stop; the
		// closed registry has no holds left to release
		self.routeRejections.close()
		self.terminalRouteRejections.close()
		self.cancel()
		self.workers.Wait()
		var pairs []*contractOriginPair
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for i := range self.buckets {
				bucket := &self.buckets[i]
				for _, pair := range bucket.pairs {
					pairs = append(pairs, pair)
				}
				clear(bucket.pairs)
				clear(bucket.outstandingKeySentTimes)
				bucket.pendingKeys = nil
				bucket.watches = 0
				bucket.heldPairs = 0
				bucket.socket = nil
			}
			contractOriginHeldPairsGauge.Sub(float64(self.heldPairs))
			self.heldPairs = 0
			self.waiters = 0
		}()
		for _, pair := range pairs {
			pair.lookup.invalidate()
			pair.version.Update(func(version uint64) uint64 { return version + 1 })
		}
		abandoned := 0
		for len(self.queue) > 0 {
			<-self.queue
			abandoned++
		}
		contractOriginNotificationCounter.WithLabelValues("owner_closed").Add(float64(abandoned))
	})
}

// The publisher pool bounds concurrent I/O even when shared Redis socket
// timeouts outlast its context. The contract request never waits on this work.
// The channels share a slot, so the pipeline is one round trip to one node.
func publishContractOrigin(ctx context.Context, publications []contractOriginPublication) (returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errors.New("contract origin publisher unavailable")
		}
	}()
	server.Redis(ctx, func(client server.RedisClient) {
		_, returnErr = client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, publication := range publications {
				pipe.SPublish(ctx, publication.channel, publication.payload)
			}
			return nil
		})
	}, server.OptNoRetry())
	return
}
