package model

// An API router or Connect exchange owns this advisory notification service.
// Sixteen lazy sharded subscriptions multiplex pair waiters; a bounded publisher
// queue keeps Redis latency out of committed contract responses. PostgreSQL is
// authoritative: missed, dropped and mixed-version events require timed rechecks.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

const (
	contractOriginBucketCount          = 16
	contractOriginReceiveTimeout       = time.Second
	contractOriginAcknowledgementLimit = 2 * time.Second
	contractOriginIncomingLimit        = 2 * time.Second
)

var contractOriginNotificationCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_contract_origin_notifications_total",
	Help: "Advisory contract-origin notification operations by a fixed event class; drops require authoritative timed rechecks",
}, []string{"event"})

func init() {
	for _, event := range []string{"enqueued", "published", "publish_failed", "queue_full", "owner_closed", "unowned", "subscribed", "reconnected", "subscription_failed", "received", "invalid_message", "registration_declined"} {
		contractOriginNotificationCounter.WithLabelValues(event)
	}
	prometheus.MustRegister(contractOriginNotificationCounter)
}

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
}

// The route rejection bound holds about 6 MiB. Old sdks retry a refused
// route every 5s, and an edge host's load balancer spreads the retries over
// its 4-5 api processes, so a process sees one route only every 25-40s. At
// 12s the ring answered about a quarter of those retries; 60s answers most.
// An origin edge or a subscription acknowledgement still removes a refusal at
// once, so only an edge this owner never receives refuses an identical
// request for up to the full 60s.
//
// Terminal refusals hold at most two generations of 32768, about 21 MiB. On
// Main old sdks keep about 48k stale routes alive, retrying each every 5-6s
// for hours, and an edge host's load balancer spreads a client's retries over
// its 4-5 api processes. A process therefore holds roughly the 12k routes of
// its host's clients and sees each about every 30s, which keeps that route's
// refusal in the current generation. The hour matches the client state
// refusal cache (session/state_refusal_cache.go).
func DefaultContractOriginNotificationSettings() ContractOriginNotificationSettings {
	return ContractOriginNotificationSettings{
		MaxWaiters:                    8192,
		PublishQueueSize:              1024,
		PublishWorkers:                2,
		MaxRouteRejections:            16384,
		RouteRejectionTimeout:         60 * time.Second,
		TerminalRouteRejectionCount:   32768,
		TerminalRouteRejectionTimeout: time.Hour,
	}
}

type contractOriginSubscription = server.RedisShardSubscription

type contractOriginPair struct {
	version *connect.MonitorValue[uint64]
	refs    int
	lookup  contractOriginLookupState
}

type contractOriginBucket struct {
	needed  chan struct{}
	started bool
	pairs   map[[32]byte]*contractOriginPair
}

// All methods are safe for concurrent use. Close prevents new registrations and
// publications, cancels I/O, and joins every reader and publisher owned here.
type ContractOriginNotifications struct {
	ctx                context.Context
	cancel             context.CancelFunc
	settings           ContractOriginNotificationSettings
	subscriptionSource *server.RedisShardSubscriptionSource
	publish            func(context.Context, string, string) error
	queue              chan [32]byte
	workers            sync.WaitGroup
	closeOnce          sync.Once
	stateLock          sync.Mutex
	closed             bool
	waiters            int
	buckets            [contractOriginBucketCount]contractOriginBucket
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
	publish func(context.Context, string, string) error,
) *ContractOriginNotifications {
	if settings.MaxWaiters <= 0 || settings.PublishQueueSize <= 0 || settings.PublishWorkers <= 0 || settings.PublishWorkers > contractOriginBucketCount ||
		settings.MaxRouteRejections < 0 || settings.RouteRejectionTimeout < 0 ||
		settings.TerminalRouteRejectionCount < 0 || settings.TerminalRouteRejectionTimeout < 0 {
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
		notifications.buckets[i] = contractOriginBucket{needed: make(chan struct{}), pairs: map[[32]byte]*contractOriginPair{}}
		notifications.workers.Add(1)
		go func() { defer notifications.workers.Done(); notifications.runBucket(i) }()
	}
	for range settings.PublishWorkers {
		notifications.workers.Add(1)
		go func() { defer notifications.workers.Done(); notifications.runPublisher() }()
	}
	return notifications
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

func contractOriginChannel(bucket int) string {
	return fmt.Sprintf("contract-origin:v1:{%02x}", bucket)
}

// A watch retains one registration. Its monitor is armed immediately before the
// authoritative read; an acknowledgement or commit arriving during that read is
// therefore still visible afterward. Closing one watch never removes its peers.
type ContractOriginWatch struct {
	owner     *ContractOriginNotifications
	key       [32]byte
	pair      *contractOriginPair
	closeOnce sync.Once
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
		bucket := &self.buckets[int(key[0])%contractOriginBucketCount]
		pair := bucket.pairs[key]
		if pair == nil {
			pair = &contractOriginPair{version: connect.NewMonitorValue(uint64(0))}
			bucket.pairs[key] = pair
		}
		pair.refs++
		self.waiters++
		if !bucket.started {
			bucket.started = true
			close(bucket.needed)
		}
		watch = &ContractOriginWatch{owner: self, key: key, pair: pair}
	}()
	if watch == nil {
		contractOriginNotificationCounter.WithLabelValues("registration_declined").Inc()
	}
	return watch
}

func (self *ContractOriginWatch) Update() <-chan struct{} {
	if self == nil {
		return nil
	}
	_, update := self.pair.version.Get()
	return update
}

func (self *ContractOriginWatch) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.owner.stateLock.Lock()
		defer self.owner.stateLock.Unlock()
		bucket := &self.owner.buckets[int(self.key[0])%contractOriginBucketCount]
		if bucket.pairs[self.key] == self.pair {
			self.pair.refs--
			self.owner.waiters--
			if self.pair.refs == 0 {
				delete(bucket.pairs, self.key)
			}
		}
	})
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
	notifications.routeRejections.invalidate(key)
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
			err := self.publish(self.ctx, contractOriginChannel(int(key[0])%contractOriginBucketCount), hex.EncodeToString(key[:]))
			event := "published"
			if err != nil {
				event = "publish_failed"
			}
			contractOriginNotificationCounter.WithLabelValues(event).Inc()
		}
	}
}

// Each socket must acknowledge its owned channel and keep proving read-side
// liveness; a slot move, an error reply or missing evidence replaces it
// (server.RunRedisShardSubscription). Every acknowledgement that starts a
// subscription wakes the registered pairs to recheck missed commits.
func (self *ContractOriginNotifications) runBucket(index int) {
	select {
	case <-self.ctx.Done():
		return
	case <-self.buckets[index].needed:
	}
	channel := contractOriginChannel(index)
	server.RunRedisShardSubscription(
		self.ctx,
		channel,
		contractOriginSubscriptionSettings(),
		self.subscriptionSource,
		&server.RedisShardSubscriptionHandler{
			Subscribed: func(resubscribed bool) {
				label := "subscribed"
				if resubscribed {
					label = "reconnected"
				}
				contractOriginNotificationCounter.WithLabelValues(label).Inc()
				self.notifyBucket(index, nil)
			},
			Message: func(message server.RedisMessage) bool {
				if message.Channel != channel || len(message.Payload) != 64 {
					contractOriginNotificationCounter.WithLabelValues("invalid_message").Inc()
					return false
				}
				decoded, decodeErr := hex.DecodeString(message.Payload)
				if decodeErr != nil || int(decoded[0])%contractOriginBucketCount != index {
					contractOriginNotificationCounter.WithLabelValues("invalid_message").Inc()
					return false
				}
				var key [32]byte
				copy(key[:], decoded)
				contractOriginNotificationCounter.WithLabelValues("received").Inc()
				self.notifyBucket(index, &key)
				return true
			},
			Lost: func(error) {
				contractOriginNotificationCounter.WithLabelValues("subscription_failed").Inc()
			},
		},
	)
}

// Replacements keep the established strict one-second pace rather than the
// shared growing backoff: an owner holds at most sixteen sockets.
func contractOriginSubscriptionSettings() *server.RedisShardSubscriptionSettings {
	settings := server.DefaultRedisShardSubscriptionSettings()
	settings.AcknowledgementTimeout = contractOriginAcknowledgementLimit
	settings.IncomingTimeout = contractOriginIncomingLimit
	settings.ReceiveTimeout = contractOriginReceiveTimeout
	settings.MinReconnectTimeout = time.Second
	settings.MaxReconnectTimeout = time.Second
	return settings
}

// Snapshot under the registry lock, then notify outside it: monitor operations
// never nest an external lock inside the registry's state lock.
func (self *ContractOriginNotifications) notifyBucket(index int, key *[32]byte) {
	var pairs []*contractOriginPair
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if key != nil {
			if pair := self.buckets[index].pairs[*key]; pair != nil {
				pairs = append(pairs, pair)
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
	if key != nil {
		self.routeRejections.invalidate(*key)
	} else {
		self.routeRejections.invalidateBucket(index)
	}
}

func (self *ContractOriginNotifications) Close() {
	self.closeOnce.Do(func() {
		self.stateLock.Lock()
		self.closed = true
		self.stateLock.Unlock()
		// no edge can remove a route rejection once the readers stop
		self.routeRejections.close()
		self.terminalRouteRejections.close()
		self.cancel()
		self.workers.Wait()
		var pairs []*contractOriginPair
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for i := range self.buckets {
				for _, pair := range self.buckets[i].pairs {
					pairs = append(pairs, pair)
				}
				clear(self.buckets[i].pairs)
			}
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
// timeouts outlast this context. The contract request never waits on this work.
func publishContractOrigin(ctx context.Context, channel string, payload string) (returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errors.New("contract origin publisher unavailable")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	server.Redis(ctx, func(client server.RedisClient) { returnErr = client.SPublish(ctx, channel, payload).Err() }, server.OptNoRetry())
	return
}
