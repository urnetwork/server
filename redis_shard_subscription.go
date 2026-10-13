package server

// Sharded pub/sub subscriptions that follow their channel's slot.
//
// A cluster node that gives up a slot drops every SSUBSCRIBE for the channels
// in that slot and sends each subscriber a sunsubscribe. go-redis hands that
// notice to its reader but keeps the channel in its own subscription set and
// keeps the healthy socket to the old owner, so it never subscribes again:
// pings keep succeeding while SPUBLISH reaches no one. On 2026-10-12 a cluster
// rebalance left the proxy blocks' client change channels without subscribers
// this way until the blocks restarted.
//
// `RunRedisShardSubscription` owns its socket and reads it with explicit
// deadlines, so cancellation joins the exact reader instead of leaving go-redis
// channel goroutines. It replaces the socket whenever the subscription is not
// shown to exist on the slot's owner:
//   - a sunsubscribe for the channel, or any error reply (MOVED, ASK, ...)
//   - no acknowledgement of an SSUBSCRIBE within the acknowledgement timeout.
//     Other traffic does not stand in for it.
//   - no incoming evidence (pong, acknowledgement or valid message) within the
//     incoming timeout. An idle socket is pinged; the write alone is not
//     evidence.
//   - a periodic re-assertion: SSUBSCRIBE again on the live socket. The slot
//     owner acknowledges it without change, or restores a subscription it lost
//     without notice; a node that no longer owns the slot answers MOVED.
//
// Each replacement asks the cluster client to reload its slot map, waits a paced
// backoff that grows while replacements fail, and opens a new SSUBSCRIBE, which
// the client routes to the slot's current owner. The reload is required: go-redis
// refreshes its map on MOVED only for ordinary commands, never for pub/sub, so
// every new SSUBSCRIBE would otherwise reach the old owner and be answered MOVED
// until some other command meets a MOVED or the map ages out (a minute by
// default).
//
// Messages published while no subscription existed are gone. The handler hears
// of every acknowledgement that starts a subscription, so the consumer resyncs
// from its authoritative source there.

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
)

var redisShardSubscriptionCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_redis_shard_subscriptions_total",
	Help: "Sharded pub/sub subscription events: subscribed, resubscribed after a loss, reasserted on a live socket, lost after an acknowledgement, or failed before one",
}, []string{"event"})

func init() {
	for _, event := range []string{"subscribed", "resubscribed", "reasserted", "lost", "failed"} {
		redisShardSubscriptionCounter.WithLabelValues(event)
	}
	prometheus.MustRegister(redisShardSubscriptionCounter)
}

var (
	errRedisShardSubscriptionDropped        = errors.New("the slot owner dropped the subscription")
	errRedisShardSubscriptionUnacknowledged = errors.New("the subscription was not acknowledged")
	errRedisShardSubscriptionSilent         = errors.New("no incoming evidence of a live subscription")
)

// One shard subscription socket, used by a single reader goroutine.
// `*redis.PubSub` implements it.
type RedisShardSubscription interface {
	SSubscribe(ctx context.Context, channels ...string) error
	ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error)
	Ping(ctx context.Context, payload ...string) error
	Close() error
}

// Opens shard subscriptions and refreshes the routing they are opened with.
type RedisShardSubscriptionSource struct {
	// opens a subscription to the channel on the owner of its slot
	Open func(ctx context.Context, channel string) (RedisShardSubscription, error)
	// requests a fresh slot map after a subscription is lost, so the next open
	// reaches the slot's new owner. nil when there is nothing to refresh
	ReloadRoutes func(ctx context.Context)
}

// The process Redis client and its cluster slot map.
func DefaultRedisShardSubscriptionSource() *RedisShardSubscriptionSource {
	return &RedisShardSubscriptionSource{
		Open:         openRedisShardSubscription,
		ReloadRoutes: reloadRedisRoutes,
	}
}

// How a shard subscription proves it is live, and how fast it is replaced.
type RedisShardSubscriptionSettings struct {
	// an SSUBSCRIBE, first or re-asserted, must be acknowledged within this
	AcknowledgementTimeout time.Duration
	// the longest a socket may go without incoming evidence
	IncomingTimeout time.Duration
	// the longest single read before an idle socket is pinged
	ReceiveTimeout time.Duration
	// how often a live subscription is re-asserted. <= 0 disables it
	ReassertTimeout time.Duration
	// the pause after a socket is replaced. It starts at the minimum, doubles
	// while replacements fail to be acknowledged, and resets once one is
	MinReconnectTimeout time.Duration
	MaxReconnectTimeout time.Duration
}

// A lost or silent subscription is replaced within seconds, and a live one is
// re-asserted every 30s.
func DefaultRedisShardSubscriptionSettings() *RedisShardSubscriptionSettings {
	return &RedisShardSubscriptionSettings{
		AcknowledgementTimeout: 2 * time.Second,
		IncomingTimeout:        2 * time.Second,
		ReceiveTimeout:         time.Second,
		ReassertTimeout:        30 * time.Second,
		MinReconnectTimeout:    time.Second,
		MaxReconnectTimeout:    15 * time.Second,
	}
}

// Receives one subscription's events on its reader goroutine. The functions
// must not block. nil functions are skipped.
type RedisShardSubscriptionHandler struct {
	// an acknowledgement started a subscription. resubscribed is true after a
	// loss, when messages published in the gap are gone
	Subscribed func(resubscribed bool)
	// a message was read. Returns whether it is valid evidence that the
	// subscription is live
	Message func(message RedisMessage) bool
	// a subscription ended, or failed to start, while the context is live
	Lost func(cause error)
}

// Keeps a subscription to the channel until ctx is done, and returns after its
// socket is closed. The file comment lists when the socket is replaced. A nil
// source is the default source.
func RunRedisShardSubscription(
	ctx context.Context,
	channel string,
	settings *RedisShardSubscriptionSettings,
	source *RedisShardSubscriptionSource,
	handler *RedisShardSubscriptionHandler,
) {
	if settings.AcknowledgementTimeout <= 0 || settings.IncomingTimeout <= 0 || settings.ReceiveTimeout <= 0 ||
		settings.MinReconnectTimeout <= 0 || settings.MaxReconnectTimeout < settings.MinReconnectTimeout {
		panic(errors.New("invalid redis shard subscription settings"))
	}
	if source == nil {
		source = DefaultRedisShardSubscriptionSource()
	}
	if handler == nil {
		handler = &RedisShardSubscriptionHandler{}
	}

	subscriptionCount := 0
	startSubscription := func() {
		event := "subscribed"
		if 0 < subscriptionCount {
			event = "resubscribed"
		}
		redisShardSubscriptionCounter.WithLabelValues(event).Inc()
		if handler.Subscribed != nil {
			handler.Subscribed(0 < subscriptionCount)
		}
		subscriptionCount += 1
	}

	// reads one socket until the subscription is not shown to exist on the slot
	// owner. acknowledged reports whether a subscription started on it
	read := func(subscription RedisShardSubscription) (acknowledged bool, cause error) {
		lastIncomingTime := time.Now()
		// zero when no acknowledgement is outstanding
		acknowledgementDeadline := lastIncomingTime.Add(settings.AcknowledgementTimeout)
		// zero until an acknowledgement schedules the next re-assertion
		var reassertTime time.Time
		reasserting := false
		for ctx.Err() == nil {
			now := time.Now()
			if !reassertTime.IsZero() && !now.Before(reassertTime) {
				reassertTime = time.Time{}
				reasserting = true
				acknowledgementDeadline = now.Add(settings.AcknowledgementTimeout)
				reassertCtx, cancel := context.WithDeadline(ctx, acknowledgementDeadline)
				err := subscription.SSubscribe(reassertCtx, channel)
				cancel()
				if err != nil {
					return acknowledged, err
				}
			}
			deadline := lastIncomingTime.Add(settings.IncomingTimeout)
			if !acknowledgementDeadline.IsZero() {
				// while an SSUBSCRIBE is outstanding only its acknowledgement
				// counts
				deadline = acknowledgementDeadline
			}
			if !now.Before(deadline) {
				if !acknowledgementDeadline.IsZero() {
					return acknowledged, errRedisShardSubscriptionUnacknowledged
				}
				return acknowledged, errRedisShardSubscriptionSilent
			}
			wakeTime := deadline
			if !reassertTime.IsZero() && reassertTime.Before(wakeTime) {
				wakeTime = reassertTime
			}
			receiveTimeout := min(settings.ReceiveTimeout, wakeTime.Sub(now))
			if receiveTimeout <= 0 {
				// the re-assertion is due
				continue
			}
			event, err := subscription.ReceiveTimeout(ctx, receiveTimeout)
			if err != nil {
				var timeoutErr net.Error
				if !errors.As(err, &timeoutErr) || !timeoutErr.Timeout() || ctx.Err() != nil {
					return acknowledged, err
				}
				now = time.Now()
				if !acknowledgementDeadline.IsZero() || !now.Before(deadline) || (!reassertTime.IsZero() && !now.Before(reassertTime)) {
					// a pong cannot stand in for an outstanding acknowledgement,
					// and the next pass replaces the socket or re-asserts
					continue
				}
				pingCtx, cancel := context.WithDeadline(ctx, deadline)
				err = subscription.Ping(pingCtx)
				cancel()
				if err != nil {
					return acknowledged, err
				}
				continue
			}
			switch event := event.(type) {
			case *redis.Subscription:
				if event.Channel != channel {
					continue
				}
				switch event.Kind {
				case "sunsubscribe":
					// slot migration removes the subscription without closing
					// the socket
					return acknowledged, errRedisShardSubscriptionDropped
				case "ssubscribe":
					lastIncomingTime = time.Now()
					acknowledgementDeadline = time.Time{}
					if 0 < settings.ReassertTimeout {
						reassertTime = lastIncomingTime.Add(settings.ReassertTimeout)
					}
					if reasserting {
						reasserting = false
						redisShardSubscriptionCounter.WithLabelValues("reasserted").Inc()
						continue
					}
					// the first acknowledgement on this socket, or one that
					// nothing asked for. Either can follow a gap.
					acknowledged = true
					startSubscription()
				}
			case *redis.Pong:
				lastIncomingTime = time.Now()
			case *redis.Message:
				if handler.Message == nil || handler.Message(event) {
					lastIncomingTime = time.Now()
				}
			}
		}
		return acknowledged, ctx.Err()
	}

	reconnectTimeout := settings.MinReconnectTimeout
	for ctx.Err() == nil {
		acknowledged := false
		subscription, cause := source.Open(ctx, channel)
		if cause == nil && subscription == nil {
			cause = errors.New("no shard subscription opened")
		}
		if cause == nil {
			// a canceled context closes the socket, which ends a blocked read
			closed := make(chan struct{})
			stopClose := context.AfterFunc(ctx, func() {
				defer close(closed)
				_ = subscription.Close()
			})
			acknowledged, cause = read(subscription)
			if stopClose() {
				_ = subscription.Close()
			} else {
				<-closed
			}
		}
		if ctx.Err() != nil {
			return
		}

		if acknowledged {
			reconnectTimeout = settings.MinReconnectTimeout
			redisShardSubscriptionCounter.WithLabelValues("lost").Inc()
			glog.Infof("[redis]shard subscription %s lost (%s), resubscribing\n", channel, cause)
		} else {
			redisShardSubscriptionCounter.WithLabelValues("failed").Inc()
			if glog.V(1) {
				glog.Infof("[redis]shard subscription %s failed (%s)\n", channel, cause)
			}
		}
		if handler.Lost != nil {
			handler.Lost(cause)
		}
		// the slot may have moved; the next open must not route by a stale map
		if source.ReloadRoutes != nil {
			source.ReloadRoutes(ctx)
		}

		reconnect := connect.NewPacedReconnect(reconnectTimeout)
		select {
		case <-ctx.Done():
			return
		case <-reconnect.After():
		}
		if !acknowledged {
			reconnectTimeout = min(2*reconnectTimeout, settings.MaxReconnectTimeout)
		}
	}
}

// Opens through the process client, which sends the SSUBSCRIBE to the owner of
// the channel's slot by its current slot map. The acknowledgement is the
// reader's to await.
func openRedisShardSubscription(ctx context.Context, channel string) (subscription RedisShardSubscription, returnErr error) {
	defer func() {
		if recover() != nil {
			subscription = nil
			returnErr = errors.New("shard subscription unavailable")
		}
	}()
	openCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	Redis(openCtx, func(client RedisClient) {
		subscription = client.SSubscribe(openCtx, channel)
	}, OptNoRetry())
	return
}

// Asks the process cluster client to reload its slot map in the background.
// Standalone and unopened clients have no slot map.
func reloadRedisRoutes(ctx context.Context) {
	if clusterClient, ok := safeClient.current().(*redis.ClusterClient); ok {
		clusterClient.ReloadState(ctx)
	}
}
