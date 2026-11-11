package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	connectlib "github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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

// Whether the subscriber holds a relay listener for the client.
func (self *keyEventSubscriber) testingHasWebRtcRelayListener(clientId server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.webRtcRelayListeners[clientId])
}

// Root cause: an offer the prober posts has no way to the resident unless a
// key event reaches its listener (S8). Observable: the subscriber psubscribes
// the offer sets' channel, an sadd on a client's offer set kicks that
// client's listener alone, and the take's own spop does not kick it again.
func TestKeyEventSubscriberKicksTheWebRtcRelayListenerOnAnOffer(t *testing.T) {
	if !slices.Contains((&keyEventSubscriber{}).patterns(3), model.ExtenderWebRtcOfferKeyEventPattern(3)) {
		t.Fatal("the subscriber does not subscribe the webrtc offer sets")
	}

	settings := DefaultExtenderWebRtcRelaySettings()
	listener, store, exchange := newTestWebRtcRelayListener(t, settings, nil)
	otherListener, otherStore, otherExchange := newTestWebRtcRelayListener(t, settings, nil)
	subscriber := &keyEventSubscriber{
		ctx:                  context.Background(),
		webRtcRelayListeners: map[server.Id]map[int64]*extenderWebRtcRelayListener{},
	}
	removeListener := subscriber.AddWebRtcRelayListener(listener.clientId, listener)
	defer removeListener()
	removeOtherListener := subscriber.AddWebRtcRelayListener(otherListener.clientId, otherListener)
	defer removeOtherListener()

	offer := testWebRtcRelayOffer(t, 5*time.Second)
	store.add(offer)
	otherStore.add(testWebRtcRelayOffer(t, 5*time.Second))
	channel := fmt.Sprintf("__keyspace@0__:{%s}w_offer", listener.clientId)
	// the take's spop and the set's ttl are no new offer
	subscriber.dispatch(channel, "spop")
	subscriber.dispatch(channel, "expire")
	listener.workers.Wait()
	if exchange.offerCount() != 0 {
		t.Fatal("an event that adds no offer kicked the listener")
	}
	subscriber.dispatch(channel, "sadd")
	store.waitPosted(t, 1)
	if answer := store.answer(offer.RequestId); answer == nil || answer.Error != "" {
		t.Fatalf("answer = %+v", answer)
	}
	otherListener.workers.Wait()
	if otherExchange.offerCount() != 0 {
		t.Fatal("one client's offer kicked another client's listener")
	}

	// a removed listener is kicked no more
	removeListener()
	store.add(testWebRtcRelayOffer(t, 5*time.Second))
	subscriber.dispatch(channel, "sadd")
	listener.workers.Wait()
	if exchange.offerCount() != 1 {
		t.Fatalf("offers relayed = %d after the listener was removed, want 1", exchange.offerCount())
	}
	if subscriber.testingHasWebRtcRelayListener(listener.clientId) {
		t.Fatal("the removed listener is still registered")
	}
}
