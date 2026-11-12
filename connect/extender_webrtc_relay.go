// The exchange's half of the webrtc extender carrier's signaling relay
// (connect/EXTENDER.md S8, model/extender_webrtc_relay_model.go).
//
// The operator's prober runs in api and taskworker processes, which have no
// path to the exchange resident holding an extender's client. So the prober
// posts its offer to the client's Redis offer set and wakes the process
// hosting the resident on that process's own offer channel
// (extender_webrtc_offer_wakes.go), and the process takes the offer and
// relays it the way a peers update reaches the client: from the resident's
// internal client, whose id is
// the control id, as the flagged `ExchangeSignals` the client's manager routes
// to its extender answerer (connect net_extender_webrtc_signal.go). The
// answer comes back on the client's exchange connection to the same internal
// client, whose manager hands it to the waiting exchange, and the listener
// posts it for the prober.
//
// The relay is no open relay: it takes offers only from the client's own
// offer set, which only the prober writes, and relays each only to the client
// the resident holds, whose answer alone the exchange waits for (the waiter is
// keyed by the client id and the offer's stream id). It is bounded so the
// prober cannot be turned into an amplifier either: an offer and an answer
// are size bounded, every offer carries its prober's deadline and is dropped
// once it passed, a client's offers are rate limited by a token bucket, and
// the exchange relays a bounded number at once over all its residents. A
// refused offer is answered with why, so its prober stops waiting at once.
package connect

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var extenderWebRtcRelayOffers = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_extender_webrtc_relay_offers_total",
	Help: "Webrtc extender carrier offers the exchange's residents took for their clients (connect/EXTENDER.md S8), by outcome: answered, the client answered and the answer was posted for the prober; " +
		"no_answer, the client sent no answer before the offer's deadline, or its answer failed; expired, the prober's deadline had passed when the offer was taken; " +
		"rate_limited, the client's offers came faster than its rate; busy, the exchange was relaying as many offers as it takes at once; " +
		"oversized, the offer or the answer was past the size bound; malformed, the offer did not decode.",
}, []string{"outcome"})

const extenderWebRtcRelayOutcomeAnswered = "answered"

func init() {
	for _, outcome := range []string{
		extenderWebRtcRelayOutcomeAnswered,
		model.ExtenderWebRtcRelayErrorNoAnswer,
		model.ExtenderWebRtcRelayErrorExpired,
		model.ExtenderWebRtcRelayErrorRateLimited,
		model.ExtenderWebRtcRelayErrorBusy,
		model.ExtenderWebRtcRelayErrorOversized,
		model.ExtenderWebRtcRelayErrorMalformed,
	} {
		extenderWebRtcRelayOffers.WithLabelValues(outcome)
	}
	prometheus.MustRegister(extenderWebRtcRelayOffers)
}

// ExtenderWebRtcRelaySettings bound the exchange's half of the relay. Every
// field is per exchange process.
type ExtenderWebRtcRelaySettings struct {
	// offers one take reads off a client's set before relaying them one at a
	// time; a full take is followed by another. Non-positive takes none,
	// which leaves the offers to expire.
	TakeBatchSize int
	// the offers the exchange relays at once over all its residents. Each
	// holds an exchange with a client for the seconds its answer takes to
	// gather, so one past the bound is answered busy rather than queued.
	// Non-positive relays none.
	MaxConcurrentRelays int
	// a client's offers a minute, and the burst, past which an offer is
	// answered rate_limited without reaching the client. The prober sends a
	// client an activation's probe (six an hour a family) and the uptime
	// task's (one an address every five minutes), so this binds only a prober
	// gone wrong. Non-positive relays none.
	ClientOffersPerMinute int
	ClientOfferBurst      int
	// the most bytes of an offer's session description json, and of an
	// answer's. An offer and an answer carry their gathered candidates (the
	// carrier trickles nothing), a few hundred bytes each.
	MaxOfferByteCount  int
	MaxAnswerByteCount int
	// the longest the resident waits for its client's answer, whatever the
	// offer's own deadline says
	MaxExchangeTimeout time.Duration
	// how long a posted answer waits for its prober; past it Redis drops it
	AnswerTtl time.Duration
}

func DefaultExtenderWebRtcRelaySettings() *ExtenderWebRtcRelaySettings {
	return &ExtenderWebRtcRelaySettings{
		TakeBatchSize:         4,
		MaxConcurrentRelays:   64,
		ClientOffersPerMinute: 12,
		ClientOfferBurst:      4,
		MaxOfferByteCount:     16 * 1024,
		MaxAnswerByteCount:    16 * 1024,
		MaxExchangeTimeout:    15 * time.Second,
		AnswerTtl:             30 * time.Second,
	}
}

// What a relay listener reads its offers from and posts its answers to: the
// client's Redis keys in production, a fake in a test.
type extenderWebRtcRelayStore interface {
	takeOffers(ctx context.Context, clientId server.Id, limit int) []*model.ExtenderWebRtcOffer
	putAnswer(ctx context.Context, clientId server.Id, requestId server.Id, answer *model.ExtenderWebRtcAnswer, ttl time.Duration)
}

type extenderWebRtcRelayRedisStore struct{}

func (extenderWebRtcRelayRedisStore) takeOffers(ctx context.Context, clientId server.Id, limit int) []*model.ExtenderWebRtcOffer {
	return model.TakeExtenderWebRtcOffers(ctx, clientId, limit)
}

func (extenderWebRtcRelayRedisStore) putAnswer(
	ctx context.Context,
	clientId server.Id,
	requestId server.Id,
	answer *model.ExtenderWebRtcAnswer,
	ttl time.Duration,
) {
	model.PutExtenderWebRtcAnswer(ctx, clientId, requestId, answer, ttl)
}

// The exchange of one offer with the client, which answers with the client's
// answer or fails when ctx ends first. In production it is the resident's
// internal client's exchanger to its client
// (WebRtcManager.ExtenderCarrierExchanger).
type extenderWebRtcRelayExchange func(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error)

// Relays one resident client's offers: each kick takes the pending offers and
// relays them one at a time. Every resident registers one, and almost none
// of their clients is an extender, so the listener holds no goroutine while
// idle: a kick starts a worker when none runs, kicks coalesce while it runs,
// and it ends once a pass finds no kick waiting. The offer channel's reader
// (extender_webrtc_offer_wakes.go) never blocks on it. Kick and CloseAndWait
// are safe for concurrent use.
type extenderWebRtcRelayListener struct {
	ctx      context.Context
	cancel   context.CancelFunc
	clientId server.Id
	settings *ExtenderWebRtcRelaySettings
	// the exchange's relay slots, shared by every resident's listener
	// (MaxConcurrentRelays)
	slots    chan struct{}
	bucket   *server.TokenBucket
	store    extenderWebRtcRelayStore
	exchange extenderWebRtcRelayExchange
	now      func() time.Time

	stateLock sync.Mutex
	// a kick came since the worker last started a pass
	kicked bool
	// a worker runs
	running bool
	closed  bool
	workers sync.WaitGroup
}

// Builds the resident-owned listener, which starts nothing until its first
// kick. slots is the exchange's, shared by every listener it builds.
func newExtenderWebRtcRelayListener(
	ctx context.Context,
	clientId server.Id,
	settings *ExtenderWebRtcRelaySettings,
	slots chan struct{},
	store extenderWebRtcRelayStore,
	exchange extenderWebRtcRelayExchange,
	now func() time.Time,
) *extenderWebRtcRelayListener {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &extenderWebRtcRelayListener{
		ctx:      cancelCtx,
		cancel:   cancel,
		clientId: clientId,
		settings: settings,
		slots:    slots,
		bucket: server.NewTokenBucket(
			float64(settings.ClientOffersPerMinute)/float64(time.Minute/time.Second),
			settings.ClientOfferBurst,
		),
		store:    store,
		exchange: exchange,
		now:      now,
	}
}

// Asks for a pass over the client's pending offers, starting the worker when
// none runs. It never waits.
func (self *extenderWebRtcRelayListener) Kick() {
	start := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.kicked = true
		if self.running {
			return false
		}
		self.running = true
		self.workers.Add(1)
		return true
	}()
	if start {
		go func() {
			defer self.workers.Done()
			server.HandleError(self.run, func() {
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					self.running = false
				}()
			})
		}()
	}
}

// Passes over the pending offers while kicks keep coming, then ends.
func (self *extenderWebRtcRelayListener) run() {
	for {
		pass := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if !self.kicked || self.closed || self.ctx.Err() != nil {
				self.running = false
				return false
			}
			self.kicked = false
			return true
		}()
		if !pass {
			return
		}
		self.deliver()
	}
}

// Takes and relays until a take comes back short of the batch. A taken offer
// whose answer cannot be posted is gone; its prober stops at its deadline.
func (self *extenderWebRtcRelayListener) deliver() {
	if self.settings.TakeBatchSize <= 0 {
		return
	}
	for {
		if self.ctx.Err() != nil {
			return
		}
		var offers []*model.ExtenderWebRtcOffer
		server.HandleError(func() {
			offers = self.store.takeOffers(self.ctx, self.clientId, self.settings.TakeBatchSize)
		})
		for _, offer := range offers {
			if self.ctx.Err() != nil {
				return
			}
			self.relay(offer)
		}
		if len(offers) < self.settings.TakeBatchSize {
			return
		}
	}
}

// Relays one offer to the client and posts the outcome for its prober. The
// checks run cheapest first, and every refusal is posted, so a prober never
// waits out its deadline for an offer that went nowhere.
func (self *extenderWebRtcRelayListener) relay(offer *model.ExtenderWebRtcOffer) {
	answer := &model.ExtenderWebRtcAnswer{}
	defer func() {
		outcome := answer.Error
		if outcome == "" {
			outcome = extenderWebRtcRelayOutcomeAnswered
		}
		extenderWebRtcRelayOffers.WithLabelValues(outcome).Inc()
		server.HandleError(func() {
			self.store.putAnswer(self.ctx, self.clientId, offer.RequestId, answer, self.settings.AnswerTtl)
		})
	}()

	now := self.now()
	timeout := min(offer.ExpireTime().Sub(now), self.settings.MaxExchangeTimeout)
	if timeout <= 0 {
		answer.Error = model.ExtenderWebRtcRelayErrorExpired
		return
	}
	if self.settings.MaxOfferByteCount < len(offer.Offer) {
		answer.Error = model.ExtenderWebRtcRelayErrorOversized
		return
	}
	var description webrtc.SessionDescription
	if err := json.Unmarshal([]byte(offer.Offer), &description); err != nil || description.Type != webrtc.SDPTypeOffer {
		answer.Error = model.ExtenderWebRtcRelayErrorMalformed
		return
	}
	if !self.bucket.Take(now) {
		answer.Error = model.ExtenderWebRtcRelayErrorRateLimited
		return
	}
	select {
	case self.slots <- struct{}{}:
	default:
		answer.Error = model.ExtenderWebRtcRelayErrorBusy
		return
	}
	defer func() {
		<-self.slots
	}()

	exchangeCtx, exchangeCancel := context.WithTimeout(self.ctx, timeout)
	defer exchangeCancel()
	answerDescription, err := self.exchange(exchangeCtx, description)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[wr]%s no answer = %s\n", self.clientId, err)
		}
		answer.Error = model.ExtenderWebRtcRelayErrorNoAnswer
		return
	}
	answerBytes, err := json.Marshal(&answerDescription)
	if err != nil {
		answer.Error = model.ExtenderWebRtcRelayErrorMalformed
		return
	}
	if self.settings.MaxAnswerByteCount < len(answerBytes) {
		answer.Error = model.ExtenderWebRtcRelayErrorOversized
		return
	}
	answer.Answer = string(answerBytes)
}

// Joins the worker, if one runs, after withdrawing its Redis and exchange
// context; no kick starts another after.
func (self *extenderWebRtcRelayListener) CloseAndWait() {
	self.cancel()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
	}()
	self.workers.Wait()
}
