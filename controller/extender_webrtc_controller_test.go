package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The prober's half of the webrtc carrier's signaling relay (connect/EXTENDER.md
// S8, extender_webrtc_controller.go) over a fake relay: what the exchanger
// posts, how long it waits, what it refuses, and what of an answer reaches
// this host's ICE. Nothing here reads Redis or opens a peer connection.

// A relay that holds the offers posted and answers from a table.
type testWebRtcProbeRelay struct {
	stateLock sync.Mutex
	connected bool
	// whether an add finds room
	room      bool
	offers    []*model.ExtenderWebRtcOffer
	members   []string
	removed   []string
	waits     []time.Duration
	answer    *model.ExtenderWebRtcAnswer
	clientIds []server.Id
}

func (self *testWebRtcProbeRelay) clientConnected(ctx context.Context, clientId server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientIds = append(self.clientIds, clientId)
	return self.connected
}

func (self *testWebRtcProbeRelay) addOffer(
	ctx context.Context,
	clientId server.Id,
	offer *model.ExtenderWebRtcOffer,
	maxPendingCount int,
	ttl time.Duration,
) (string, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientIds = append(self.clientIds, clientId)
	if !self.room {
		return "", false
	}
	self.offers = append(self.offers, offer)
	member := "member-" + offer.RequestId.String()
	self.members = append(self.members, member)
	return member, true
}

func (self *testWebRtcProbeRelay) removeOffer(ctx context.Context, clientId server.Id, member string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removed = append(self.removed, member)
}

func (self *testWebRtcProbeRelay) waitAnswer(
	ctx context.Context,
	clientId server.Id,
	requestId server.Id,
	timeout time.Duration,
) (*model.ExtenderWebRtcAnswer, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.waits = append(self.waits, timeout)
	if self.answer == nil {
		return nil, false
	}
	return self.answer, true
}

const testWebRtcAnswerSdp = "v=0\r\n" +
	"o=- 1 1 IN IP4 0.0.0.0\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=candidate:1 1 udp 2130706431 10.0.0.5 50000 typ host\r\n" +
	"a=candidate:2 1 udp 2130706431 192.168.1.5 50001 typ host\r\n" +
	"a=candidate:3 1 udp 1694498815 203.0.113.10 50002 typ srflx raddr 0.0.0.0 rport 0\r\n" +
	"a=candidate:4 1 udp 1694498815 2001:db8::10 50003 typ srflx raddr :: rport 0\r\n" +
	"a=candidate:5 1 udp 1694498815 198.51.100.20 50004 typ srflx raddr 0.0.0.0 rport 0\r\n" +
	"a=candidate:6 1 udp 2130706431 127.0.0.1 50005 typ host\r\n" +
	"a=end-of-candidates\r\n"

func testWebRtcRelayAnswer(t *testing.T, sdp string) *model.ExtenderWebRtcAnswer {
	t.Helper()
	answerBytes, err := json.Marshal(&webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
	if err != nil {
		t.Fatal(err)
	}
	return &model.ExtenderWebRtcAnswer{Answer: string(answerBytes)}
}

var testWebRtcProbeOffer = webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\ns=offer\r\n"}

func newTestWebRtcRelayExchanger(relay *testWebRtcProbeRelay, settings *ExtenderWebRtcProbeSettings) (*extenderWebRtcRelayExchanger, server.Id, time.Time) {
	clientId := server.NewId()
	now := time.Date(2026, time.November, 11, 0, 0, 0, 0, time.UTC)
	return &extenderWebRtcRelayExchanger{
		clientId: clientId,
		settings: settings,
		relay:    relay,
		now:      func() time.Time { return now },
	}, clientId, now
}

// Root cause: the probe had no path to the extender's client at all (S5's
// seam answered nothing). Observable: the offer is posted for the one client
// the exchanger was built for, with the probe's deadline cut to the whole
// seconds Redis waits, the answer comes back, and only its public candidates,
// at most the bound of them, reach this host's ICE.
func TestExtenderWebRtcRelayExchangerRelaysTheOfferAndBoundsTheAnswer(t *testing.T) {
	relay := &testWebRtcProbeRelay{connected: true, room: true}
	settings := DefaultExtenderWebRtcProbeSettings()
	settings.MaxAnswerCandidateCount = 2
	relay.answer = testWebRtcRelayAnswer(t, testWebRtcAnswerSdp)
	exchanger, clientId, now := newTestWebRtcRelayExchanger(relay, settings)

	ctx, cancel := context.WithDeadline(context.Background(), now.Add(5500*time.Millisecond))
	defer cancel()
	answer, err := exchanger.ExchangeOffer(ctx, testWebRtcProbeOffer)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, answer.Type, webrtc.SDPTypeAnswer)
	// the private and loopback candidates are dropped, and of the public ones
	// only the first two are kept
	for _, dropped := range []string{"10.0.0.5", "192.168.1.5", "198.51.100.20", "127.0.0.1"} {
		if strings.Contains(answer.SDP, dropped) {
			t.Fatalf("the answer kept candidate %s:\n%s", dropped, answer.SDP)
		}
	}
	for _, kept := range []string{"203.0.113.10", "2001:db8::10", "a=end-of-candidates", "s=-"} {
		if !strings.Contains(answer.SDP, kept) {
			t.Fatalf("the answer lost %q:\n%s", kept, answer.SDP)
		}
	}

	if len(relay.offers) != 1 {
		t.Fatalf("offers posted = %d, want 1", len(relay.offers))
	}
	for _, seenClientId := range relay.clientIds {
		connect.AssertEqual(t, seenClientId, clientId)
	}
	offer := relay.offers[0]
	var postedOffer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(offer.Offer), &postedOffer); err != nil || postedOffer.SDP != testWebRtcProbeOffer.SDP {
		t.Fatalf("posted offer = %q, %v", offer.Offer, err)
	}
	// 5.5 s left of the probe, cut to the 5 s Redis waits, and the offer
	// expires when the wait ends
	connect.AssertEqual(t, relay.waits, []time.Duration{5 * time.Second})
	connect.AssertEqual(t, offer.ExpireTime(), now.Add(5*time.Second))
	connect.AssertEqual(t, len(relay.removed), 0)
}

// Every way the relay has no answer fails the probe with why, at once where it
// can: a client with no resident posts nothing, a client with offers pending
// posts nothing, an offer nobody answered is taken back so no resident relays
// it to a probe that is gone, and a resident's refusal and an oversized or
// malformed answer are named.
func TestExtenderWebRtcRelayExchangerFailsWithoutAnAnswer(t *testing.T) {
	cases := []struct {
		name        string
		relay       *testWebRtcProbeRelay
		settings    func(settings *ExtenderWebRtcProbeSettings)
		wantErr     error
		wantText    string
		wantOffers  int
		wantRemoved int
	}{
		{
			name:    "not connected",
			relay:   &testWebRtcProbeRelay{connected: false, room: true},
			wantErr: ErrExtenderWebRtcClientNotConnected,
		},
		{
			name:    "offers pending",
			relay:   &testWebRtcProbeRelay{connected: true, room: false},
			wantErr: ErrExtenderWebRtcOffersPending,
		},
		{
			name:        "no answer",
			relay:       &testWebRtcProbeRelay{connected: true, room: true},
			wantErr:     ErrExtenderWebRtcNoAnswer,
			wantOffers:  1,
			wantRemoved: 1,
		},
		{
			name: "the client did not answer",
			relay: &testWebRtcProbeRelay{connected: true, room: true, answer: &model.ExtenderWebRtcAnswer{
				Error: model.ExtenderWebRtcRelayErrorNoAnswer,
			}},
			wantErr:    ErrExtenderWebRtcNoAnswer,
			wantOffers: 1,
		},
		{
			name: "rate limited",
			relay: &testWebRtcProbeRelay{connected: true, room: true, answer: &model.ExtenderWebRtcAnswer{
				Error: model.ExtenderWebRtcRelayErrorRateLimited,
			}},
			wantText:   "the webrtc relay refused the offer: rate_limited",
			wantOffers: 1,
		},
		{
			name: "an answer that is not one",
			relay: &testWebRtcProbeRelay{connected: true, room: true, answer: &model.ExtenderWebRtcAnswer{
				Answer: `{"type":"offer","sdp":"v=0"}`,
			}},
			wantText:   "the webrtc answer is not an answer",
			wantOffers: 1,
		},
		{
			name: "an oversized answer",
			relay: &testWebRtcProbeRelay{connected: true, room: true, answer: &model.ExtenderWebRtcAnswer{
				Answer: `{"type":"answer","sdp":"` + strings.Repeat("a", 64) + `"}`,
			}},
			settings: func(settings *ExtenderWebRtcProbeSettings) {
				settings.MaxAnswerByteCount = 32
			},
			wantText:   "more than the relay's 32",
			wantOffers: 1,
		},
		{
			name:  "an oversized offer",
			relay: &testWebRtcProbeRelay{connected: true, room: true},
			settings: func(settings *ExtenderWebRtcProbeSettings) {
				settings.MaxOfferByteCount = 8
			},
			wantText: "more than the relay's 8",
		},
	}
	for _, c := range cases {
		settings := DefaultExtenderWebRtcProbeSettings()
		if c.settings != nil {
			c.settings(settings)
		}
		exchanger, _, now := newTestWebRtcRelayExchanger(c.relay, settings)
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(9*time.Second))
		_, err := exchanger.ExchangeOffer(ctx, testWebRtcProbeOffer)
		cancel()
		if c.wantErr != nil && !errors.Is(err, c.wantErr) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.wantErr)
		}
		if c.wantText != "" && (err == nil || !strings.Contains(err.Error(), c.wantText)) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.wantText)
		}
		connect.AssertEqual(t, len(c.relay.offers), c.wantOffers)
		connect.AssertEqual(t, len(c.relay.removed), c.wantRemoved)
		if 0 < c.wantRemoved {
			connect.AssertEqual(t, c.relay.removed, c.relay.members)
		}
		// the default answer wait bounds a probe with more time left
		if 0 < len(c.relay.waits) {
			connect.AssertEqual(t, c.relay.waits[0], settings.AnswerTimeout)
		}
	}
}

// A probe whose budget is gone posts nothing; one with less than a second
// left still waits the one second Redis can.
func TestExtenderWebRtcRelayExchangerWaitsWithinTheProbeBudget(t *testing.T) {
	relay := &testWebRtcProbeRelay{connected: true, room: true}
	exchanger, _, now := newTestWebRtcRelayExchanger(relay, DefaultExtenderWebRtcProbeSettings())

	spent, cancel := context.WithDeadline(context.Background(), now)
	defer cancel()
	if _, err := exchanger.ExchangeOffer(spent, testWebRtcProbeOffer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the spent budget", err)
	}
	connect.AssertEqual(t, len(relay.offers), 0)

	short, shortCancel := context.WithDeadline(context.Background(), now.Add(300*time.Millisecond))
	defer shortCancel()
	_, _ = exchanger.ExchangeOffer(short, testWebRtcProbeOffer)
	connect.AssertEqual(t, relay.waits, []time.Duration{time.Second})
	connect.AssertEqual(t, relay.offers[0].ExpireTime(), now.Add(time.Second))
}

// The candidate bound keeps every line that is not a candidate, drops host
// names, and keeps at most the bound of the public candidates in order.
func TestBoundExtenderWebRtcAnswerCandidates(t *testing.T) {
	bounded := boundExtenderWebRtcAnswerCandidates(testWebRtcAnswerSdp, 8)
	for _, kept := range []string{"203.0.113.10", "2001:db8::10", "198.51.100.20"} {
		if !strings.Contains(bounded, kept) {
			t.Fatalf("lost %s", kept)
		}
	}
	for _, dropped := range []string{"10.0.0.5", "192.168.1.5", "127.0.0.1"} {
		if strings.Contains(bounded, dropped) {
			t.Fatalf("kept %s", dropped)
		}
	}
	named := "a=candidate:7 1 udp 2130706431 extender.example 50006 typ host\r\n"
	if strings.Contains(boundExtenderWebRtcAnswerCandidates(named, 8), "extender.example") {
		t.Fatal("kept a host name candidate")
	}
	if none := boundExtenderWebRtcAnswerCandidates(testWebRtcAnswerSdp, 0); strings.Contains(none, "a=candidate:") {
		t.Fatal("a bound of zero kept a candidate")
	}
}

// Root cause: an extender's answer to a relayed offer travels to the control
// id like a control message, and the resident's controller took it for an
// unhandled one, counting a control failure for every webrtc probe (S8).
// Observable: a flagged ExchangeSignals among control frames is no error and
// no reply, and the frames beside it are still handled.
func TestConnectControlFramesTakesAnExtenderAnswerAsANoOp(t *testing.T) {
	answerFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.ExchangeSignals{
		StreamId:        server.NewId().Bytes(),
		ExtenderCarrier: true,
		Signals: []*protocol.ExchangeSignal{
			{
				SignalType: protocol.SignalType_SdpAnswer,
				Sdp:        []byte(`{"type":"answer","sdp":"v=0"}`),
			},
		},
	})
	pingFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.ControlPing{})
	outFrames, err := ConnectControlFrames(
		context.Background(),
		server.NewId(),
		[]*protocol.Frame{answerFrame, pingFrame},
		connect.DefaultContractManagerSettings(),
	)
	if err != nil {
		t.Fatalf("err = %v, want none", err)
	}
	connect.AssertEqual(t, len(outFrames), 0)
}
