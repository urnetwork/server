package connect

// §6.8's resident publishers (PROTO-M1-09) without stores: while push is off a resident reads no mark
// and starts no publisher; a client without the strict_exp mark gets none; and a hint written while
// the subscription was down reaches the client once the resubscribe kicks its publisher. The publisher
// runs on sidecartest's FakeRedis and FakeClock; the exchange's tests with real stores are in
// sidecar_publisher_pending_test.go.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One push message a publisher sent, decoded as the client's dispatcher reads it.
type testSidecarPushMessage struct {
	reset *sidecarprotocol.PushReset
	hint  *sidecarprotocol.PushHint
}

// The PushReset or PushHint in a publisher's frame: a SubprotocolMessage for 21842 around one
// unchunked NO_REPLY envelope.
func decodeTestSidecarPushFrame(frame *protocol.Frame) (*testSidecarPushMessage, error) {
	if frame.MessageType != protocol.MessageType_Subprotocol {
		return nil, errors.New("not a subprotocol frame")
	}
	wrapper := &protocol.SubprotocolMessage{}
	if err := proto.Unmarshal(frame.MessageBytes, wrapper); err != nil {
		return nil, err
	}
	if wrapper.SubprotocolId != sidecar.SubprotocolId {
		return nil, errors.New("not subprotocol 21842")
	}
	envelope := &sidecarprotocol.Envelope{}
	if err := proto.Unmarshal(wrapper.MessageBytes, envelope); err != nil {
		return nil, err
	}
	return decodeTestSidecarPushEnvelope(envelope)
}

func decodeTestSidecarPushEnvelope(envelope *sidecarprotocol.Envelope) (*testSidecarPushMessage, error) {
	if envelope.Version != sidecar.WireVersionMajor || envelope.Flags != sidecar.EnvelopeFlagNoReply || envelope.Chunk != nil {
		return nil, errors.New("not a push envelope")
	}
	message := &testSidecarPushMessage{}
	switch sidecarprotocol.MessageType(envelope.Type) {
	case sidecarprotocol.MessageType_PUSH_RESET:
		message.reset = &sidecarprotocol.PushReset{}
		return message, proto.Unmarshal(envelope.Body, message.reset)
	case sidecarprotocol.MessageType_PUSH_HINT:
		message.hint = &sidecarprotocol.PushHint{}
		return message, proto.Unmarshal(envelope.Body, message.hint)
	}
	return nil, errors.New("not a push type")
}

// A resident's send that takes every frame, decodes it and returns its buffer.
func newTestSidecarPushSend(t testing.TB) (send func(frame *protocol.Frame, ack connectlib.AckFunction) bool, messages chan *testSidecarPushMessage) {
	messages = make(chan *testSidecarPushMessage, 256)
	return func(frame *protocol.Frame, ack connectlib.AckFunction) bool {
		message, err := decodeTestSidecarPushFrame(frame)
		if err != nil {
			t.Errorf("a publisher frame no client reads: %v", err)
			return false
		}
		connectlib.MessagePoolReturn(frame.MessageBytes)
		messages <- message
		return true
	}, messages
}

func awaitTestSidecarPushMessage(t testing.TB, messages chan *testSidecarPushMessage) *testSidecarPushMessage {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(10 * time.Second):
		t.Fatal("the publisher sent nothing")
	}
	return nil
}

// Waits for a timer timeout from now on clock and moves the clock past it.
func passTestSidecarPushTimer(t testing.TB, clock *sidecartest.FakeClock, timeout time.Duration) {
	t.Helper()
	at := clock.Now().Add(timeout)
	guard := time.After(10 * time.Second)
	for {
		pending, added := clock.WatchTimers()
		if slices.ContainsFunc(pending, at.Equal) {
			clock.Advance(timeout)
			return
		}
		select {
		case <-added:
		case <-guard:
			t.Fatalf("no timer at %s", at)
		}
	}
}

// FakeRedis, telling the test of each GET of n, which ends a read that found nothing.
type testSidecarPushRedis struct {
	*sidecartest.FakeRedis
	gets chan struct{}
}

func (self *testSidecarPushRedis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, found, err := self.FakeRedis.Get(ctx, key)
	self.gets <- struct{}{}
	return value, found, err
}

// An exchange with no run loops, a subscriber with no subscription, and push's settings.
func newTestSidecarPushExchange(ctx context.Context, enableSidecarPush bool) *Exchange {
	exchange := newResidentConstructionExchange(ctx)
	exchange.settings.EnableSidecarPush = enableSidecarPush
	exchange.keyEventSubscriber = newTestSidecarPushSubscriber(ctx, enableSidecarPush)
	return exchange
}

// While push is off for any reason, the resident's profile is read as it always was, and the marks are
// never asked: with EnableSidecarPush off, with sidecar.yml's switch off, and, where the binary links
// no MG0, with both on.
func TestResidentReadsNoMarkWhilePushIsOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cases := []struct {
		enableSidecarPush bool
		config            string
	}{
		{enableSidecarPush: false, config: "sidecar:\n  push: {enabled: true}\n"},
		{enableSidecarPush: true, config: "sidecar:\n  push: {enabled: false}\n"},
		{enableSidecarPush: true, config: ""},
	}
	if !server.SidecarMg0Linked() {
		cases = append(cases, struct {
			enableSidecarPush bool
			config            string
		}{enableSidecarPush: true, config: "sidecar:\n  push: {enabled: true}\n"})
	}
	for _, c := range cases {
		func() {
			defer server.Testing_PushSidecarConfig(c.config)()
			exchange := newTestSidecarPushExchange(ctx, c.enableSidecarPush)
			defer exchange.Close()
			networkId := server.NewId()
			profileReads := 0
			exchange.residentProfileForTest = func(context.Context, server.Id) (server.Id, bool, model.NetworkPeerCategory, *model.NetworkPeer, bool) {
				profileReads += 1
				return networkId, true, model.NetworkPeerCategoryClient, &model.NetworkPeer{}, true
			}
			exchange.residentPushMarksForTest = func(server.Id) model.SidecarPushMarks {
				t.Errorf("%t, %q: the marks were read", c.enableSidecarPush, c.config)
				return model.SidecarPushMarks{StrictExp: true}
			}
			readNetworkId, topLevel, _, _, _, marks := exchange.loadResidentProfile(ctx, server.NewId())
			if profileReads != 1 || readNetworkId != networkId || !topLevel || marks != (model.SidecarPushMarks{}) {
				t.Errorf("%t, %q: %d profile reads, marks %+v", c.enableSidecarPush, c.config, profileReads, marks)
			}
		}()
	}
}

// A resident starts a publisher only for a strict_exp client while push reads on: a client without the
// mark, or one whose switch went off after its marks were read, gets none and registers nothing.
func TestResidentWithoutTheMarkStartsNoPublisher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cases := []struct {
		marks  model.SidecarPushMarks
		config string
		start  bool
	}{
		{marks: model.SidecarPushMarks{}, config: "sidecar:\n  push: {enabled: true}\n", start: false},
		{marks: model.SidecarPushMarks{AclEnforcing: true}, config: "sidecar:\n  push: {enabled: true}\n", start: false},
		{marks: model.SidecarPushMarks{StrictExp: true}, config: "sidecar:\n  push: {enabled: false}\n", start: false},
		{marks: model.SidecarPushMarks{StrictExp: true}, config: "sidecar:\n  push: {enabled: true}\n", start: true},
	}
	for _, c := range cases {
		func() {
			defer server.Testing_PushSidecarConfig(c.config)()
			exchange := newTestSidecarPushExchange(ctx, true)
			defer exchange.Close()
			fakeRedis := sidecartest.NewFakeRedis(ctx, nil, nil, nil, nil)
			defer fakeRedis.Close()
			sidecartest.RegisterPushScripts(fakeRedis)
			exchange.sidecarPushRedis = fakeRedis
			started := []*pushservice.SidecarPublisher{}
			exchange.afterSidecarPublisherStartForTest = func(resident *Resident, publisher *pushservice.SidecarPublisher) {
				started = append(started, publisher)
			}
			clientId := server.NewId()
			residentCtx, residentCancel := context.WithCancel(ctx)
			defer residentCancel()
			client := connectlib.NewClientWithTag(residentCtx, connectlib.ControlId, "c(test)", connectlib.NewNoContractClientOob(), connectlib.DefaultClientSettings())
			defer client.Close()
			client.ContractManager().AddNoContractPeer(connectlib.Id(clientId))
			resident := &Resident{
				ctx:              residentCtx,
				cancel:           residentCancel,
				exchange:         exchange,
				clientId:         clientId,
				client:           client,
				sidecarPushMarks: c.marks,
			}
			stop := resident.startSidecarPublisher()
			registered := len(exchange.keyEventSubscriber.sidecarPushListeners[clientId])
			if (len(started) == 1) != c.start || (registered == 1) != c.start {
				t.Errorf("%+v, %q: %d started, %d registered", c.marks, c.config, len(started), registered)
			}
			// the resident's end removes the listener and joins the publisher's run
			residentCancel()
			stop()
			if len(exchange.keyEventSubscriber.sidecarPushListeners[clientId]) != 0 {
				t.Errorf("%+v: a listener outlived the resident", c.marks)
			}
		}()
	}
}

// A hint written while the subscription was down has no event of its own: the resubscribe's kick
// makes the publisher read it, and it reaches the client.
func TestSidecarPublisherHintWrittenInTheGapArrivesAtResubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeRedis := sidecartest.NewFakeRedis(ctx, clock, nil, nil, nil)
	defer fakeRedis.Close()
	sidecartest.RegisterPushScripts(fakeRedis)
	writer, err := pushservice.NewHintWriter(fakeRedis, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := pushservice.DefaultSidecarPublisherSettings()
	publisher, err := pushservice.NewSidecarPublisher(settings, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	subscriber := newTestSidecarPushSubscriber(ctx, true)
	clientId := server.NewId()
	remove := subscriber.AddSidecarPushListener(clientId, publisher)
	defer remove()
	send, messages := newTestSidecarPushSend(t)
	recording := &testSidecarPushRedis{FakeRedis: fakeRedis, gets: make(chan struct{}, 16)}
	publisher.Start(ctx, &pushservice.PublisherEnv{ClientId: connectlib.Id(clientId), Send: send, Redis: recording})
	if message := awaitTestSidecarPushMessage(t, messages); message.reset == nil || !message.reset.Complete {
		t.Fatalf("the first frame is %+v", message)
	}
	// the read after the reset finds nothing, and the publisher waits for a kick
	passTestSidecarPushTimer(t, clock, settings.HintCoalesceTimeout)
	select {
	case <-recording.gets:
	case <-time.After(10 * time.Second):
		t.Fatal("the read after the reset never ended")
	}

	// the subscription is down: the hint's INCR reaches no one
	if _, err := writer.HintClient(ctx, connectlib.Id(clientId), &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: 9}); err != nil {
		t.Fatal(err)
	}
	subscriber.resyncSidecarPushListeners()
	passTestSidecarPushTimer(t, clock, settings.HintCoalesceTimeout)
	message := awaitTestSidecarPushMessage(t, messages)
	if message.hint == nil || message.hint.Seq != 1 || message.hint.Topic != sidecar.PushTopicTeam || message.hint.Version != 9 {
		t.Errorf("the gap's hint arrived as %+v", message)
	}
}
