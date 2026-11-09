package sidecarhost

// The process's one key-event subscriber (host_redis.go's AddKeyEventCallback; R-OPS-8): one
// server.SubscribeKeyEvents per process, on every family any engine in it watches, never a subscription
// per client, the pattern that melted the cluster on 2026-07-15 (server/model/peer_model.go:31-36).
//
// The subscription runs while a callback is registered and stops with the last one. Its patterns are
// the families the callbacks name. A family it does not yet hold is subscribed with the larger set
// beside the live subscription, which delivers until its successor is acknowledged, so the families
// already held see no gap. AddCallback returns once a subscription holding its family is acknowledged,
// so an event fired after it returns is heard, and a new callback hears no Resync for its own start.
// Whatever is published while no subscription is acknowledged is gone, so after a loss (a lost master, a
// moved master set, an overflowed merge) every callback is told Resync at once and again once a
// successor is back, as server/connect/key_event_subscriber.go does. A family whose last callback goes
// stays subscribed until the next resubscription, and its events reach no one.
//
// Callbacks run on the subscriber's goroutine, outside any lock, and must not block.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The subscriber's tunables.
type RedisKeyEventsSettings struct {
	// How long to wait before subscribing again after a subscription failed to start: 1 s, so a node
	// that refuses is not asked in a loop.
	ResubscribeTimeout time.Duration
	// How often the subscription rechecks the cluster's masters, and resubscribes when they changed:
	// 60 s, as the exchange's subscriber does.
	TopologyRefreshInterval time.Duration
	// How long AddCallback waits for a subscription holding its family before it gives up and removes
	// the callback: 15 s, three of server's 5 s acknowledgement bounds.
	AddTimeout time.Duration
}

// The defaults.
func DefaultRedisKeyEventsSettings() *RedisKeyEventsSettings {
	return &RedisKeyEventsSettings{
		ResubscribeTimeout:      1 * time.Second,
		TopologyRefreshInterval: 60 * time.Second,
		AddTimeout:              15 * time.Second,
	}
}

// One message from a subscription: the channel pattern it matched and the channel itself.
type redisKeyEventMessage struct {
	pattern string
	channel string
}

// Subscribes on patterns, as server.SubscribeKeyEvents does: it returns once every pattern is
// acknowledged, then gives messages until done closes; unsubscribe ends it.
type redisKeyEventSubscribe func(ctx context.Context, topologyRefresh time.Duration, patterns ...string) (messages <-chan *redisKeyEventMessage, done <-chan struct{}, unsubscribe func(), err error)

// server.SubscribeKeyEvents, its messages as the subscriber's.
func serverSubscribeKeyEvents(ctx context.Context, topologyRefresh time.Duration, patterns ...string) (<-chan *redisKeyEventMessage, <-chan struct{}, func(), error) {
	messages, done, unsubscribe, err := server.SubscribeKeyEvents(ctx, topologyRefresh, patterns...)
	if err != nil {
		return nil, nil, nil, err
	}
	forwardCtx, forwardCancel := context.WithCancel(ctx)
	out := make(chan *redisKeyEventMessage)
	go server.HandleError(func() {
		for {
			select {
			case <-forwardCtx.Done():
				return
			case message, ok := <-messages:
				if !ok {
					return
				}
				select {
				case <-forwardCtx.Done():
					return
				case out <- &redisKeyEventMessage{pattern: message.Pattern, channel: message.Channel}:
				}
			}
		}
	})
	return out, done, func() {
		forwardCancel()
		unsubscribe()
	}, nil
}

// One run of the subscription loop, from the first callback to the removal of the last.
type redisKeyEventsRun struct {
	cancel context.CancelFunc
	// tells the loop a family was added, so it can check its subscription holds it
	patternAdded chan struct{}
	// the families the acknowledged subscription holds, empty while none is; under the subscriber's
	// stateLock
	subscribedKeyPatterns map[string]bool
	// closed and replaced at each change of subscribedKeyPatterns; under the subscriber's stateLock
	subscribedChanged chan struct{}
}

// The subscriber. One per process (processRedisKeyEvents); a test makes its own.
type redisKeyEvents struct {
	settings  *RedisKeyEventsSettings
	subscribe redisKeyEventSubscribe
	// the logical database whose keyspace channels the patterns name: server.RedisDb, read at each
	// subscription
	db func() int

	stateLock sync.Mutex
	// by key pattern, then by callback id
	patternCallbacks map[string]map[int64]sidecar.RedisKeyEventFunction
	nextCallbackId   int64
	// nil while no callback is registered
	run *redisKeyEventsRun
}

func newRedisKeyEvents(settings *RedisKeyEventsSettings, subscribe redisKeyEventSubscribe, db func() int) *redisKeyEvents {
	if settings == nil {
		settings = DefaultRedisKeyEventsSettings()
	}
	return &redisKeyEvents{
		settings:         settings,
		subscribe:        subscribe,
		db:               db,
		patternCallbacks: map[string]map[int64]sidecar.RedisKeyEventFunction{},
	}
}

// The process's one, made at first use.
var processRedisKeyEvents = sync.OnceValue(func() *redisKeyEvents {
	return newRedisKeyEvents(nil, serverSubscribeKeyEvents, server.RedisDb)
})

// A family is a glob with a * in it and something besides: a key pattern names many clients' keys, and
// one client's key is never subscribed to by itself.
func checkRedisKeyPattern(keyPattern string) error {
	if !strings.Contains(keyPattern, "*") {
		return fmt.Errorf("sidecarhost redis: key pattern %q names one key, not a family", keyPattern)
	}
	if strings.Trim(keyPattern, "*") == "" {
		return fmt.Errorf("sidecarhost redis: key pattern %q names every key", keyPattern)
	}
	return nil
}

// The keyspace channel pattern of keyPattern in db.
func redisKeyspacePattern(db int, keyPattern string) string {
	return fmt.Sprintf("__keyspace@%d__:%s", db, keyPattern)
}

// The key a keyspace channel names, or false for one that is no keyspace channel.
func redisKeyspaceChannelKey(channel string) (string, bool) {
	rest, found := strings.CutPrefix(channel, "__keyspace@")
	if !found {
		return "", false
	}
	_, key, found := strings.Cut(rest, "__:")
	if !found || key == "" {
		return "", false
	}
	return key, true
}

// Records what run's acknowledged subscription holds, nil for none, and wakes whoever waits on it.
func (self *redisKeyEvents) setSubscribed(run *redisKeyEventsRun, keyPatterns []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	subscribedKeyPatterns := map[string]bool{}
	for _, keyPattern := range keyPatterns {
		subscribedKeyPatterns[keyPattern] = true
	}
	run.subscribedKeyPatterns = subscribedKeyPatterns
	close(run.subscribedChanged)
	run.subscribedChanged = make(chan struct{})
}

// Registers callback for keyPattern's keys and returns once a subscription holding keyPattern is
// acknowledged. One that is not acknowledged within AddTimeout removes the callback and returns an
// error, so Redis being away fails the caller rather than leave it deaf. The returned remove is safe
// more than once.
func (self *redisKeyEvents) AddCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (func(), error) {
	if err := checkRedisKeyPattern(keyPattern); err != nil {
		return nil, err
	}
	if callback == nil {
		return nil, errors.New("sidecarhost redis: no key event callback")
	}
	var run *redisKeyEventsRun
	var callbackId int64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		callbackId = self.nextCallbackId
		self.nextCallbackId += 1
		callbacks, held := self.patternCallbacks[keyPattern]
		if !held {
			callbacks = map[int64]sidecar.RedisKeyEventFunction{}
			self.patternCallbacks[keyPattern] = callbacks
		}
		callbacks[callbackId] = callback
		if self.run == nil {
			ctx, cancel := context.WithCancel(context.Background())
			self.run = &redisKeyEventsRun{
				cancel:                cancel,
				patternAdded:          make(chan struct{}, 1),
				subscribedKeyPatterns: map[string]bool{},
				subscribedChanged:     make(chan struct{}),
			}
			started := self.run
			go server.HandleError(func() {
				self.runSubscription(ctx, started)
			})
		} else if !held {
			select {
			case self.run.patternAdded <- struct{}{}:
			default:
			}
		}
		run = self.run
	}()

	removed := false
	remove := func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if removed {
			return
		}
		removed = true
		callbacks := self.patternCallbacks[keyPattern]
		delete(callbacks, callbackId)
		if len(callbacks) == 0 {
			delete(self.patternCallbacks, keyPattern)
		}
		if len(self.patternCallbacks) == 0 && self.run != nil {
			self.run.cancel()
			self.run = nil
		}
	}

	timeout := time.After(self.settings.AddTimeout)
	for {
		var subscribed bool
		var changed chan struct{}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			subscribed = run.subscribedKeyPatterns[keyPattern]
			changed = run.subscribedChanged
		}()
		if subscribed {
			return remove, nil
		}
		select {
		case <-changed:
		case <-timeout:
			remove()
			return nil, fmt.Errorf("sidecarhost redis: no key event subscription for %q within %v", keyPattern, self.settings.AddTimeout)
		}
	}
}

// The key patterns callbacks name now, sorted.
func (self *redisKeyEvents) keyPatterns() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Sorted(maps.Keys(self.patternCallbacks))
}

// The callbacks of one key pattern, or of every pattern when keyPattern is empty.
func (self *redisKeyEvents) callbacks(keyPattern string) []sidecar.RedisKeyEventFunction {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	callbacks := []sidecar.RedisKeyEventFunction{}
	for pattern, patternCallbacks := range self.patternCallbacks {
		if keyPattern == "" || pattern == keyPattern {
			for _, callback := range patternCallbacks {
				callbacks = append(callbacks, callback)
			}
		}
	}
	return callbacks
}

// Tells every callback to reread what it watches.
func (self *redisKeyEvents) resync() {
	for _, callback := range self.callbacks("") {
		callback(sidecar.RedisKeyEvent{Resync: true})
	}
}

// One acknowledged subscription and the families it holds.
type redisKeyEventsSubscription struct {
	keyPatterns []string
	// the family each keyspace pattern names
	keyPatternsByPattern map[string]string
	messages             <-chan *redisKeyEventMessage
	done                 <-chan struct{}
	unsubscribe          func()
}

// What one subscribe call gave.
type redisKeyEventsSubscribed struct {
	subscription *redisKeyEventsSubscription
	err          error
}

// Whether subscription holds every family in keyPatterns.
func (self *redisKeyEventsSubscription) holds(keyPatterns []string) bool {
	for _, keyPattern := range keyPatterns {
		if !slices.Contains(self.keyPatterns, keyPattern) {
			return false
		}
	}
	return true
}

// Subscribes to every family callbacks name now, on its own goroutine, and gives the result on the
// returned channel.
func (self *redisKeyEvents) startSubscribe(ctx context.Context) <-chan *redisKeyEventsSubscribed {
	result := make(chan *redisKeyEventsSubscribed, 1)
	keyPatterns := self.keyPatterns()
	go server.HandleError(func() {
		db := self.db()
		patterns := make([]string, 0, len(keyPatterns))
		keyPatternsByPattern := map[string]string{}
		for _, keyPattern := range keyPatterns {
			pattern := redisKeyspacePattern(db, keyPattern)
			patterns = append(patterns, pattern)
			keyPatternsByPattern[pattern] = keyPattern
		}
		messages, done, unsubscribe, err := self.subscribe(ctx, self.settings.TopologyRefreshInterval, patterns...)
		if err != nil {
			result <- &redisKeyEventsSubscribed{err: err}
			return
		}
		result <- &redisKeyEventsSubscribed{subscription: &redisKeyEventsSubscription{
			keyPatterns:          keyPatterns,
			keyPatternsByPattern: keyPatternsByPattern,
			messages:             messages,
			done:                 done,
			unsubscribe:          unsubscribe,
		}}
	})
	return result
}

// Subscribes, delivers and subscribes again until ctx ends. A subscription that grows is made beside
// the live one, which delivers until its successor is acknowledged and only then ends, so a family
// already held sees no gap and hears no Resync; an event fired between the two may arrive twice, which a
// callback that notes keys absorbs. A lost subscription is a gap: each callback hears Resync at the
// loss and again once a successor is acknowledged.
func (self *redisKeyEvents) runSubscription(ctx context.Context, run *redisKeyEventsRun) {
	var live *redisKeyEventsSubscription
	// whether callbacks were live through a loss that no successor has closed
	lost := false
	pending := self.startSubscribe(ctx)
	var retry <-chan time.Time
	defer func() {
		if live != nil {
			live.unsubscribe()
		}
		if pending != nil {
			// a subscribe still in flight ends with ctx; whatever it made, nothing reads
			abandoned := pending
			go server.HandleError(func() {
				if subscribed := <-abandoned; subscribed.subscription != nil {
					subscribed.subscription.unsubscribe()
				}
			})
		}
		self.setSubscribed(run, nil)
	}()
	needed := func() bool {
		return live == nil || !live.holds(self.keyPatterns())
	}
	for {
		var messages <-chan *redisKeyEventMessage
		var done <-chan struct{}
		if live != nil {
			messages, done = live.messages, live.done
		}
		select {
		case <-ctx.Done():
			return
		case subscribed := <-pending:
			pending = nil
			if subscribed.err != nil {
				retry = time.After(self.settings.ResubscribeTimeout)
				continue
			}
			previous := live
			live = subscribed.subscription
			self.setSubscribed(run, live.keyPatterns)
			if previous != nil {
				previous.unsubscribe()
			}
			if lost {
				// back: what the gap held is gone, so every reader rereads
				lost = false
				self.resync()
			}
			if needed() {
				pending = self.startSubscribe(ctx)
			}
		case <-retry:
			retry = nil
			if pending == nil && needed() {
				pending = self.startSubscribe(ctx)
			}
		case <-run.patternAdded:
			if pending == nil && retry == nil && needed() {
				pending = self.startSubscribe(ctx)
			}
		case <-done:
			if ctx.Err() != nil {
				// the run's own end, not a loss
				return
			}
			live.unsubscribe()
			live = nil
			self.setSubscribed(run, nil)
			lost = true
			// lost: every reader rereads now rather than wait for the successor
			self.resync()
			if pending == nil && retry == nil {
				pending = self.startSubscribe(ctx)
			}
		case message, ok := <-messages:
			if !ok {
				// read as a loss at the next turn, through done
				live.messages = nil
				continue
			}
			if ctx.Err() != nil {
				return
			}
			keyPattern, ok := live.keyPatternsByPattern[message.pattern]
			if !ok {
				continue
			}
			key, ok := redisKeyspaceChannelKey(message.channel)
			if !ok {
				continue
			}
			for _, callback := range self.callbacks(keyPattern) {
				callback(sidecar.RedisKeyEvent{Key: key})
			}
		}
	}
}
