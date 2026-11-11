package connect

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The webrtc carrier's signaling relay end to end (connect/EXTENDER.md S8),
// against the test Redis and a real exchange: the prober's offer posted to
// the client's offer set as the controller posts it, its key event, the
// resident's listener, the flagged ExchangeSignals from the control id to a
// real connect client whose manager has an extender answerer installed, the
// client's answer back to the control id over its exchange connection, and
// the answer posted for the prober. The answerer is a fake that answers with
// a fixed description, so no peer connection is opened; the carrier's own
// round trip is connect's (net_extender_webrtc_test.go).

// An extender answerer that hands each offer it is asked to answer to the
// test and answers with a fixed description.
type testRelayAnswerer struct {
	offers chan webrtc.SessionDescription
	answer webrtc.SessionDescription
}

func (self *testRelayAnswerer) AnswerWebRtcExtenderOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	select {
	case self.offers <- offer:
	default:
	}
	return self.answer, nil
}

// The offer a resident relays reaches its client from the control id, and
// the client's answer comes back to the prober (S8).
func TestExchangeRelaysAWebRtcOfferToTheClient(t *testing.T) {
	testExchangeRelaysAWebRtcOfferToTheClient(t, localDeliveryTestTcp)
}

// The same with the client's transport delivered in process to its resident.
func TestExchangeRelaysAWebRtcOfferToTheClientInProcess(t *testing.T) {
	testExchangeRelaysAWebRtcOfferToTheClient(t, localDeliveryTestInProcess)
}

func testExchangeRelaysAWebRtcOfferToTheClient(t *testing.T, delivery localDeliveryTestMode) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := newClientCloseTestEnv(ctx, t, delivery)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "extender a",
		})
		clientIdB, _ := env.authClient(&model.AuthNetworkClientArgs{
			Description: "extender b",
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		answerer := &testRelayAnswerer{
			offers: make(chan webrtc.SessionDescription, 4),
			answer: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "v=0\r\ns=extender-a-answer\r\n"},
		}
		clientA.WebRtcManager().SetExtenderCarrierAnswerer(answerer)
		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		if !waitFor(ctx, 10*time.Second, func() bool {
			_, resident := env.exchange.testingHoldsClient(clientIdA)
			return resident
		}) {
			t.Fatal("client a has no resident")
		}
		// the relay listener registers as the resident runs; a client's
		// close-request listener registers just before it, in the same
		// block, so its own first take marks the relay's registration
		if !waitFor(ctx, 10*time.Second, func() bool {
			return env.exchange.keyEventSubscriber.testingHasWebRtcRelayListener(clientIdA)
		}) {
			t.Fatal("client a's resident registered no webrtc relay listener")
		}

		offerBytes, err := json.Marshal(&webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\ns=probe-offer\r\n"})
		if err != nil {
			t.Fatal(err)
		}
		relayOffer := &model.ExtenderWebRtcOffer{
			RequestId:    server.NewId(),
			ExpireTimeMs: time.Now().Add(8 * time.Second).UnixMilli(),
			Offer:        string(offerBytes),
		}
		if _, added := model.AddExtenderWebRtcOffer(ctx, clientIdA, relayOffer, 2, 30*time.Second); !added {
			t.Fatal("the offer was not added")
		}
		answer, ok := model.WaitExtenderWebRtcAnswer(ctx, clientIdA, relayOffer.RequestId, 10*time.Second)
		if !ok {
			t.Fatal("no answer came back")
		}
		if answer.Error != "" {
			t.Fatalf("the relay failed: %s", answer.Error)
		}
		var description webrtc.SessionDescription
		if err := json.Unmarshal([]byte(answer.Answer), &description); err != nil {
			t.Fatal(err)
		}
		if description.Type != webrtc.SDPTypeAnswer || description.SDP != answerer.answer.SDP {
			t.Fatalf("answer = %+v, want client a's", description)
		}
		select {
		case offer := <-answerer.offers:
			if offer.Type != webrtc.SDPTypeOffer || offer.SDP != "v=0\r\ns=probe-offer\r\n" {
				t.Fatalf("client a was asked to answer %+v", offer)
			}
		default:
			t.Fatal("client a's answerer saw no offer")
		}
		// the offer was taken once, and the answer once
		if pending := model.TakeExtenderWebRtcOffers(ctx, clientIdA, 16); len(pending) != 0 {
			t.Fatalf("%d offers were left behind", len(pending))
		}

		// a client with no resident has nothing to relay its offers: they stay
		// pending and nobody answers, and client a is not asked
		otherOffer := &model.ExtenderWebRtcOffer{
			RequestId:    server.NewId(),
			ExpireTimeMs: time.Now().Add(8 * time.Second).UnixMilli(),
			Offer:        string(offerBytes),
		}
		if _, added := model.AddExtenderWebRtcOffer(ctx, clientIdB, otherOffer, 2, 30*time.Second); !added {
			t.Fatal("the other offer was not added")
		}
		if _, ok := model.WaitExtenderWebRtcAnswer(ctx, clientIdB, otherOffer.RequestId, time.Second); ok {
			t.Fatal("a client with no resident was answered")
		}
		if pending := model.TakeExtenderWebRtcOffers(ctx, clientIdB, 16); len(pending) != 1 {
			t.Fatalf("client b's pending offers = %d, want its one", len(pending))
		}
		select {
		case offer := <-answerer.offers:
			t.Fatalf("client a was asked to answer another client's offer %+v", offer)
		default:
		}
	})
}
