package connect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/redis/go-redis/v9"

	connectlib "github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The residents' half of the webrtc carrier's signaling relay
// (connect/EXTENDER.md S8, extender_webrtc_relay.go) over a fake store and a
// fake exchange, on a fixed clock: what is relayed, what is refused and why,
// what is posted back, and the listener's lifecycle. Nothing here reads Redis.
// TestExchangeRelaysAWebRtcOfferToTheClient runs the relay end to end.

// The offers a listener takes, and the answers it posts.
type testWebRtcRelayStore struct {
	stateLock sync.Mutex
	clientId  server.Id
	pending   []*model.ExtenderWebRtcOffer
	answers   map[server.Id]*model.ExtenderWebRtcAnswer
	ttls      []time.Duration
	// closed after each answer is posted
	posted chan server.Id
}

func newTestWebRtcRelayStore(clientId server.Id) *testWebRtcRelayStore {
	return &testWebRtcRelayStore{
		clientId: clientId,
		answers:  map[server.Id]*model.ExtenderWebRtcAnswer{},
		posted:   make(chan server.Id, 64),
	}
}

func (self *testWebRtcRelayStore) add(offers ...*model.ExtenderWebRtcOffer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pending = append(self.pending, offers...)
}

func (self *testWebRtcRelayStore) takeOffers(ctx context.Context, clientId server.Id, limit int) []*model.ExtenderWebRtcOffer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if clientId != self.clientId {
		return nil
	}
	n := min(limit, len(self.pending))
	taken := self.pending[:n]
	self.pending = self.pending[n:]
	return taken
}

func (self *testWebRtcRelayStore) putAnswer(
	ctx context.Context,
	clientId server.Id,
	requestId server.Id,
	answer *model.ExtenderWebRtcAnswer,
	ttl time.Duration,
) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.answers[requestId] = answer
		self.ttls = append(self.ttls, ttl)
	}()
	self.posted <- requestId
}

func (self *testWebRtcRelayStore) answer(requestId server.Id) *model.ExtenderWebRtcAnswer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.answers[requestId]
}

// waits for n answers to be posted
func (self *testWebRtcRelayStore) waitPosted(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-self.posted:
		case <-time.After(10 * time.Second):
			t.Fatal("no answer was posted")
		}
	}
}

// An exchange with the client that answers each offer with a fixed answer,
// optionally holding each until released, and logs the offers it saw.
type testWebRtcRelayExchange struct {
	stateLock sync.Mutex
	offers    []webrtc.SessionDescription
	err       error
	answerSdp string
	// when set, each exchange waits for one release or its context
	release chan struct{}
	entered chan struct{}
}

func newTestWebRtcRelayExchange() *testWebRtcRelayExchange {
	return &testWebRtcRelayExchange{
		answerSdp: "v=0\r\ns=answer\r\n",
		entered:   make(chan struct{}, 64),
	}
}

func (self *testWebRtcRelayExchange) exchange(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.offers = append(self.offers, offer)
	}()
	self.entered <- struct{}{}
	if self.release != nil {
		select {
		case <-self.release:
		case <-ctx.Done():
			return webrtc.SessionDescription{}, ctx.Err()
		}
	}
	if self.err != nil {
		return webrtc.SessionDescription{}, self.err
	}
	return webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: self.answerSdp}, nil
}

func (self *testWebRtcRelayExchange) offerCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.offers)
}

var testWebRtcRelayNow = time.Date(2026, time.November, 11, 0, 0, 0, 0, time.UTC)

func testWebRtcRelayOffer(t testing.TB, expireIn time.Duration) *model.ExtenderWebRtcOffer {
	offerBytes, err := json.Marshal(&webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\ns=offer\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	return &model.ExtenderWebRtcOffer{
		RequestId:    server.NewId(),
		ExpireTimeMs: testWebRtcRelayNow.Add(expireIn).UnixMilli(),
		Offer:        string(offerBytes),
	}
}

func newTestWebRtcRelayListener(
	t *testing.T,
	settings *ExtenderWebRtcRelaySettings,
	slots chan struct{},
) (*extenderWebRtcRelayListener, *testWebRtcRelayStore, *testWebRtcRelayExchange) {
	t.Helper()
	clientId := server.NewId()
	store := newTestWebRtcRelayStore(clientId)
	exchange := newTestWebRtcRelayExchange()
	if slots == nil {
		slots = make(chan struct{}, settings.MaxConcurrentRelays)
	}
	listener := newExtenderWebRtcRelayListener(
		context.Background(),
		clientId,
		settings,
		slots,
		store,
		exchange.exchange,
		func() time.Time { return testWebRtcRelayNow },
	)
	t.Cleanup(listener.CloseAndWait)
	return listener, store, exchange
}

// Root cause: no path carried an offer from the prober to the client's
// resident and its answer back (S8). Observable: an offer the store holds is
// relayed to the client on a kick, the client's answer is posted under the
// offer's request id with the answer ttl, and the client saw exactly the offer.
func TestExtenderWebRtcRelayListenerRelaysAnOfferAndPostsTheAnswer(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)
	offer := testWebRtcRelayOffer(t, 5*time.Second)
	store.add(offer)
	listener.Kick()
	store.waitPosted(t, 1)

	answer := store.answer(offer.RequestId)
	if answer == nil || answer.Error != "" {
		t.Fatalf("answer = %+v, want the client's", answer)
	}
	var description webrtc.SessionDescription
	if err := json.Unmarshal([]byte(answer.Answer), &description); err != nil || description.SDP != exchange.answerSdp {
		t.Fatalf("posted answer %q, %v", answer.Answer, err)
	}
	connect := exchange.offers[0]
	if connect.Type != webrtc.SDPTypeOffer || connect.SDP != "v=0\r\ns=offer\r\n" {
		t.Fatalf("the client saw %+v", connect)
	}
	store.stateLock.Lock()
	defer store.stateLock.Unlock()
	if len(store.ttls) != 1 || store.ttls[0] != settings.AnswerTtl {
		t.Fatalf("answer ttls = %v, want %s", store.ttls, settings.AnswerTtl)
	}
}

// Each refusal is posted with why, and never reaches the client: an offer
// past its deadline, an oversized offer, one that is not an offer, and an
// exchange that failed.
func TestExtenderWebRtcRelayListenerRefusesWithoutReachingTheClient(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	settings.MaxOfferByteCount = 256
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)

	expired := testWebRtcRelayOffer(t, 0)
	oversized := testWebRtcRelayOffer(t, 5*time.Second)
	oversized.Offer = `{"type":"offer","sdp":"` + strings.Repeat("a", 512) + `"}`
	notAnOffer := testWebRtcRelayOffer(t, 5*time.Second)
	notAnOffer.Offer = `{"type":"answer","sdp":"v=0"}`
	malformed := testWebRtcRelayOffer(t, 5*time.Second)
	malformed.Offer = "not json"
	store.add(expired, oversized, notAnOffer, malformed)
	listener.Kick()
	store.waitPosted(t, 4)

	for _, c := range []struct {
		offer *model.ExtenderWebRtcOffer
		want  string
	}{
		{offer: expired, want: model.ExtenderWebRtcRelayErrorExpired},
		{offer: oversized, want: model.ExtenderWebRtcRelayErrorOversized},
		{offer: notAnOffer, want: model.ExtenderWebRtcRelayErrorMalformed},
		{offer: malformed, want: model.ExtenderWebRtcRelayErrorMalformed},
	} {
		if answer := store.answer(c.offer.RequestId); answer == nil || answer.Error != c.want || answer.Answer != "" {
			t.Fatalf("answer = %+v, want %s", answer, c.want)
		}
	}
	if exchange.offerCount() != 0 {
		t.Fatalf("a refused offer reached the client")
	}

	// an exchange that fails, or an answer past the bound, is posted as such
	exchange.err = errors.New("no answer from the client")
	failed := testWebRtcRelayOffer(t, 5*time.Second)
	store.add(failed)
	listener.Kick()
	store.waitPosted(t, 1)
	if answer := store.answer(failed.RequestId); answer == nil || answer.Error != model.ExtenderWebRtcRelayErrorNoAnswer {
		t.Fatalf("answer = %+v, want no_answer", answer)
	}
	exchange.err = nil
	exchange.answerSdp = strings.Repeat("b", settings.MaxAnswerByteCount)
	big := testWebRtcRelayOffer(t, 5*time.Second)
	store.add(big)
	listener.Kick()
	store.waitPosted(t, 1)
	if answer := store.answer(big.RequestId); answer == nil || answer.Error != model.ExtenderWebRtcRelayErrorOversized {
		t.Fatalf("answer = %+v, want oversized", answer)
	}
}

// Root cause: a relay that forwards whatever is posted, as fast as it is
// posted, is an amplifier at the platform's expense. Observable: past the
// client's burst an offer is posted rate_limited without reaching the client,
// and with every exchange slot taken an offer is posted busy.
func TestExtenderWebRtcRelayListenerBoundsTheClientAndTheExchange(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	settings.ClientOfferBurst = 2
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)
	offers := []*model.ExtenderWebRtcOffer{
		testWebRtcRelayOffer(t, 5*time.Second),
		testWebRtcRelayOffer(t, 5*time.Second),
		testWebRtcRelayOffer(t, 5*time.Second),
	}
	store.add(offers...)
	listener.Kick()
	store.waitPosted(t, 3)
	for i, offer := range offers {
		answer := store.answer(offer.RequestId)
		want := ""
		if i == 2 {
			want = model.ExtenderWebRtcRelayErrorRateLimited
		}
		if answer == nil || answer.Error != want {
			t.Fatalf("offer %d: answer = %+v, want %q", i, answer, want)
		}
	}
	connect := exchange.offerCount()
	if connect != 2 {
		t.Fatalf("the client saw %d offers, want the burst of 2", connect)
	}

	// every exchange slot taken by others
	busySettings := DefaultExtenderWebRtcRelaySettings()
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	busyListener, busyStore, busyExchange := newTestWebRtcRelayListener(t, busySettings, slots)
	busy := testWebRtcRelayOffer(t, 5*time.Second)
	busyStore.add(busy)
	busyListener.Kick()
	busyStore.waitPosted(t, 1)
	if answer := busyStore.answer(busy.RequestId); answer == nil || answer.Error != model.ExtenderWebRtcRelayErrorBusy {
		t.Fatalf("answer = %+v, want busy", answer)
	}
	if busyExchange.offerCount() != 0 {
		t.Fatal("an offer past the exchange's bound reached the client")
	}
	connect = len(slots)
	if connect != 1 {
		t.Fatalf("slots = %d, want the other holder's alone", connect)
	}
}

// A relay holds its slot for the exchange and gives it back after, so the
// exchange's bound counts relays in flight, not relays ever made.
func TestExtenderWebRtcRelayListenerReturnsItsSlot(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	slots := make(chan struct{}, 1)
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, slots)
	exchange.release = make(chan struct{})
	offer := testWebRtcRelayOffer(t, 5*time.Second)
	store.add(offer)
	listener.Kick()
	<-exchange.entered
	if len(slots) != 1 {
		t.Fatalf("slots in use = %d during the exchange, want 1", len(slots))
	}
	close(exchange.release)
	store.waitPosted(t, 1)
	if len(slots) != 0 {
		t.Fatalf("slots in use = %d after the exchange, want 0", len(slots))
	}
}

// Every resident builds a listener and almost no client is an extender, so
// the listener holds no goroutine while idle: none before its first kick, one
// for a pass, none after. A kick after CloseAndWait starts nothing, and
// CloseAndWait joins a pass in flight.
func TestExtenderWebRtcRelayListenerHoldsNoWorkerWhileIdle(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)
	running := func() bool {
		listener.stateLock.Lock()
		defer listener.stateLock.Unlock()
		return listener.running
	}
	if running() {
		t.Fatal("a worker runs before any kick")
	}

	exchange.release = make(chan struct{})
	store.add(testWebRtcRelayOffer(t, 5*time.Second))
	listener.Kick()
	<-exchange.entered
	if !running() {
		t.Fatal("no worker runs during a pass")
	}
	// a kick during the pass is coalesced into one more pass
	listener.Kick()
	listener.Kick()
	close(exchange.release)
	store.waitPosted(t, 1)
	listener.workers.Wait()
	if running() {
		t.Fatal("the worker outlived its passes")
	}

	listener.CloseAndWait()
	store.add(testWebRtcRelayOffer(t, 5*time.Second))
	listener.Kick()
	listener.workers.Wait()
	if running() {
		t.Fatal("a kick after the close started a worker")
	}
	if exchange.offerCount() != 1 {
		t.Fatalf("offers relayed = %d, want the one before the close", exchange.offerCount())
	}
}

// The close of a resident ends a relay waiting on its client: the exchange's
// context ends, the outcome is posted, and CloseAndWait returns.
func TestExtenderWebRtcRelayListenerCloseEndsARelayInFlight(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)
	exchange.release = make(chan struct{})
	store.add(testWebRtcRelayOffer(t, 5*time.Second))
	listener.Kick()
	<-exchange.entered
	closed := make(chan struct{})
	go func() {
		listener.CloseAndWait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("CloseAndWait did not end the relay in flight")
	}
}

// The relay's defaults bound everything they claim to: positive, the answer
// and offer bounds far above a real session description, and the exchange
// timeout above the prober's answer wait.
func TestDefaultExtenderWebRtcRelaySettings(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	if settings.TakeBatchSize <= 0 || settings.MaxConcurrentRelays <= 0 ||
		settings.ClientOffersPerMinute <= 0 || settings.ClientOfferBurst <= 0 ||
		settings.MaxOfferByteCount < 4*1024 || settings.MaxAnswerByteCount < 4*1024 ||
		settings.MaxExchangeTimeout <= 0 || settings.AnswerTtl <= 0 {
		t.Fatalf("settings = %+v", settings)
	}
	if DefaultExchangeSettings().ExtenderWebRtcRelay != *settings {
		t.Fatal("the exchange settings do not carry the relay defaults")
	}
	_ = connectlib.ControlId
}

// Whether the wakes hold a relay listener for the client.
func (self *extenderWebRtcOfferWakes) testingHasListener(clientId server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.listeners[clientId])
}

// Root cause: every exchange process found its residents' offers by matching
// a `{*}w_offer` pattern against the keyspace notification of every set on
// Main. Observable: the key event subscriber psubscribes no offer key; the
// offers wake a process on its own channel (extender_webrtc_offer_wakes.go).
func TestKeyEventSubscriberMatchesNoWebRtcOfferKey(t *testing.T) {
	for _, recycle := range []bool{false, true} {
		for _, pattern := range (&keyEventSubscriber{clientRecyclePattern: recycle}).patterns(3) {
			if strings.Contains(pattern, "w_offer") {
				t.Fatalf("the key event subscriber psubscribes %s", pattern)
			}
		}
	}
}

// One synthetic socket of the offer channel's shard subscription.
type testWebRtcOfferWakeSocket struct {
	channel   string
	events    chan any
	closed    chan struct{}
	closeOnce sync.Once
}

func (self *testWebRtcOfferWakeSocket) ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-self.closed:
		return nil, errors.New("synthetic socket closed")
	case event := <-self.events:
		return event, nil
	case <-time.After(timeout):
		return nil, context.DeadlineExceeded
	}
}

func (self *testWebRtcOfferWakeSocket) SSubscribe(context.Context, ...string) error {
	return nil
}

func (self *testWebRtcOfferWakeSocket) Ping(context.Context, ...string) error {
	self.events <- &redis.Pong{}
	return nil
}

func (self *testWebRtcOfferWakeSocket) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

// The offers' wakes (connect/EXTENDER.md S8): the process's own channel,
// subscribed with its first listener; a start of the subscription kicks every
// listener, a wake kicks the named client's alone, a wake that names no
// client or comes on another channel kicks none, and a removed listener is
// woken no more.
func TestExtenderWebRtcOfferWakesKickTheNamedClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := model.ExtenderWebRtcOfferChannel("synthetic-exchange", "connect", "g1")
		opened := make(chan *testWebRtcOfferWakeSocket, 4)
		source := &server.RedisShardSubscriptionSource{
			Open: func(ctx context.Context, channel string) (server.RedisShardSubscription, error) {
				socket := &testWebRtcOfferWakeSocket{channel: channel, events: make(chan any, 64), closed: make(chan struct{})}
				opened <- socket
				return socket, nil
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		wakes := newExtenderWebRtcOfferWakes(ctx, channel, server.DefaultRedisShardSubscriptionSettings(), source, time.Second)
		synctest.Wait()
		select {
		case <-opened:
			t.Fatal("the wakes subscribed with no listener")
		default:
		}

		settings := DefaultExtenderWebRtcRelaySettings()
		listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)
		other, otherStore, otherExchange := newTestWebRtcRelayListener(t, settings, nil)
		// offers added before there was any subscription
		first, otherFirst := testWebRtcRelayOffer(t, 5*time.Second), testWebRtcRelayOffer(t, 5*time.Second)
		store.add(first)
		otherStore.add(otherFirst)
		removeListener := wakes.AddListener(listener.clientId, listener)
		removeOther := wakes.AddListener(other.clientId, other)
		defer removeOther()
		synctest.Wait()
		var socket *testWebRtcOfferWakeSocket
		select {
		case socket = <-opened:
		default:
			t.Fatal("the first listener opened no subscription")
		}
		if socket.channel != channel {
			t.Fatalf("subscribed %s, want the process's own %s", socket.channel, channel)
		}

		// the start of the subscription kicks every listener
		socket.events <- &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1}
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		store.waitPosted(t, 1)
		otherStore.waitPosted(t, 1)
		if store.answer(first.RequestId) == nil || otherStore.answer(otherFirst.RequestId) == nil {
			t.Fatal("the start of the subscription left an offer untaken")
		}

		// a wake kicks the named client alone
		second, otherSecond := testWebRtcRelayOffer(t, 5*time.Second), testWebRtcRelayOffer(t, 5*time.Second)
		store.add(second)
		otherStore.add(otherSecond)
		socket.events <- &redis.Message{Channel: channel, Payload: listener.clientId.String()}
		synctest.Wait()
		store.waitPosted(t, 1)
		if answer := store.answer(second.RequestId); answer == nil || answer.Error != "" {
			t.Fatalf("answer = %+v", answer)
		}
		if otherExchange.offerCount() != 1 {
			t.Fatalf("another client's offers relayed = %d, want only its first", otherExchange.offerCount())
		}

		// a wake that names no client, or comes on another channel, kicks none
		socket.events <- &redis.Message{Channel: channel, Payload: "not-a-client-id"}
		socket.events <- &redis.Message{Channel: model.ExtenderWebRtcOfferChannel("synthetic-exchange", "connect", "g2"), Payload: other.clientId.String()}
		synctest.Wait()
		if otherExchange.offerCount() != 1 {
			t.Fatalf("a wake for no one relayed another client's offer: %d", otherExchange.offerCount())
		}

		// a removed listener is woken no more
		removeListener()
		store.add(testWebRtcRelayOffer(t, 5*time.Second))
		socket.events <- &redis.Message{Channel: channel, Payload: listener.clientId.String()}
		synctest.Wait()
		if exchange.offerCount() != 2 {
			t.Fatalf("offers relayed = %d after the listener was removed, want 2", exchange.offerCount())
		}
		if wakes.testingHasListener(listener.clientId) {
			t.Fatal("the removed listener is still registered")
		}
	})
}
