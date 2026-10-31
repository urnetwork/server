package controller

// The discard of a pack made only of PeerAudit frames
// (connect_control_discard.go), and the dispatch's no-op for an audit in any
// other pack. The discard reads only its argument, so most of these tests run
// without a database or Redis: a discard that reached either would fail here
// instead of answering.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// An audit as connect builds one, with synthetic ids and counts.
func discardTestAudit(abuse bool) *protocol.PeerAudit {
	return &protocol.PeerAudit{
		PeerId:              server.NewId().Bytes(),
		StreamId:            server.NewId().Bytes(),
		Duration:            60,
		Abuse:               abuse,
		DiscardedByteCount:  2048,
		DiscardedCount:      2,
		BadMessageByteCount: 512,
		BadMessageCount:     1,
		SendByteCount:       1024 * 1024,
		SendCount:           900,
		ResendByteCount:     4096,
		ResendCount:         3,
	}
}

// One frame per message. The caller returns the frames' pooled bytes.
func discardTestFrames(t testing.TB, messages ...proto.Message) []*protocol.Frame {
	t.Helper()
	frames := make([]*protocol.Frame, 0, len(messages))
	for _, message := range messages {
		frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// The control request carrying frames as given, which it neither takes nor
// returns to the pool.
func discardTestArgsOfFrames(t testing.TB, frames ...*protocol.Frame) *ConnectControlArgs {
	t.Helper()
	packBytes, err := proto.Marshal(&protocol.Pack{Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	return &ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)}
}

// The control request of one frame per message.
func discardTestArgs(t testing.TB, messages ...proto.Message) *ConnectControlArgs {
	t.Helper()
	frames := discardTestFrames(t, messages...)
	defer returnConnectControlFrames(frames)
	return discardTestArgsOfFrames(t, frames...)
}

// The two discard counts, claimed and none.
func discardTestCounts() [2]float64 {
	return [2]float64{
		testutil.ToFloat64(connectControlDiscardAbuseClaimed),
		testutil.ToFloat64(connectControlDiscardAbuseNone),
	}
}

// A pack of one or more audits, abuse reports among them, is answered with
// exactly what an authenticated pack that does nothing gets, and counted once
// by whether any frame claims abuse.
func TestDiscardConnectControlAnswersAuditOnlyPacks(t *testing.T) {
	want, err := connectControlResult(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		args  *ConnectControlArgs
		abuse bool
	}{
		{name: "one audit", args: discardTestArgs(t, discardTestAudit(false))},
		{name: "one abuse report", args: discardTestArgs(t, discardTestAudit(true)), abuse: true},
		{name: "three audits", args: discardTestArgs(t, discardTestAudit(false), discardTestAudit(false), discardTestAudit(false))},
		{name: "three audits with one abuse report", args: discardTestArgs(t, discardTestAudit(false), discardTestAudit(true), discardTestAudit(false)), abuse: true},
		{name: "an audit with no fields", args: discardTestArgs(t, &protocol.PeerAudit{})},
	} {
		before := discardTestCounts()
		answer, ok := DiscardConnectControl(c.args)
		if !ok || answer == nil {
			t.Fatalf("%s was not discarded", c.name)
		}
		result, err := ReplyConnectControl(t.Context(), answer)
		if err != nil || !reflect.DeepEqual(result, want) {
			t.Fatalf("%s: answer = %+v, %v; want %+v", c.name, result, err, want)
		}
		body, err := json.Marshal(result)
		if err != nil || string(body) != `{"pack":"","error":null}` {
			t.Fatalf("%s: encoded answer = %s, %v", c.name, body, err)
		}
		after := discardTestCounts()
		wantAfter := before
		if c.abuse {
			wantAfter[0] += 1
		} else {
			wantAfter[1] += 1
		}
		if after != wantAfter {
			t.Fatalf("%s: counts (claimed, none) = %v, want %v", c.name, after, wantAfter)
		}
	}
}

// Anything but a pack made only of decodable audits within the cap takes the
// ordinary path: every other message type alone and after an audit (each
// carrying an audit's own bytes, so only the type can refuse it), an action
// before or after an audit, an audit whose bytes do not decode, and empty,
// absent, malformed or oversized packs. None is counted.
func TestDiscardConnectControlRefusesEveryOtherShape(t *testing.T) {
	auditFrames := discardTestFrames(t, discardTestAudit(false))
	defer returnConnectControlFrames(auditFrames)
	auditFrame := auditFrames[0]
	actions := discardTestFrames(t,
		&protocol.CreateContract{DestinationId: server.NewId().Bytes(), TransferByteCount: 1024 * 1024},
		&protocol.CloseContract{ContractId: server.NewId().Bytes()},
		&protocol.Provide{},
	)
	defer returnConnectControlFrames(actions)
	createFrame, closeFrame, provideFrame := actions[0], actions[1], actions[2]

	cases := []struct {
		name string
		args *ConnectControlArgs
	}{
		{name: "no request", args: nil},
		{name: "an empty pack string", args: &ConnectControlArgs{Pack: ""}},
		{name: "a pack of no frames", args: discardTestArgsOfFrames(t)},
		{name: "bad base64", args: &ConnectControlArgs{Pack: "not a pack!"}},
		{name: "bad protobuf", args: &ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff})}},
		{name: "an audit and a create", args: discardTestArgsOfFrames(t, auditFrame, createFrame)},
		{name: "a create and an audit", args: discardTestArgsOfFrames(t, createFrame, auditFrame)},
		{name: "an audit and a close", args: discardTestArgsOfFrames(t, auditFrame, closeFrame)},
		{name: "two audits around a provide", args: discardTestArgsOfFrames(t, auditFrame, provideFrame, auditFrame)},
		// a length-delimited field that runs past the end
		{name: "an audit with undecodable bytes", args: discardTestArgsOfFrames(t, &protocol.Frame{MessageType: protocol.MessageType_TransferPeerAudit, MessageBytes: []byte{0x0a, 0x7f, 0x01}})},
		{name: "an audit then one with undecodable bytes", args: discardTestArgsOfFrames(t, auditFrame, &protocol.Frame{MessageType: protocol.MessageType_TransferPeerAudit, MessageBytes: []byte{0x0a, 0x7f, 0x01}})},
		{name: "an unknown message type", args: discardTestArgsOfFrames(t, &protocol.Frame{MessageType: protocol.MessageType(1 << 20), MessageBytes: auditFrame.MessageBytes})},
	}
	names := make([]string, 0, len(protocol.MessageType_value))
	for name := range protocol.MessageType_value {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		messageType := protocol.MessageType(protocol.MessageType_value[name])
		if messageType == protocol.MessageType_TransferPeerAudit {
			continue
		}
		other := &protocol.Frame{MessageType: messageType, MessageBytes: auditFrame.MessageBytes}
		cases = append(cases,
			struct {
				name string
				args *ConnectControlArgs
			}{name: name + " with an audit's bytes", args: discardTestArgsOfFrames(t, other)},
			struct {
				name string
				args *ConnectControlArgs
			}{name: "an audit and " + name, args: discardTestArgsOfFrames(t, auditFrame, other)},
		)
	}
	// audits alone, one past the cap
	oversized := discardTestArgsOfFrames(t)
	for audits := []*protocol.Frame{}; len(oversized.Pack) <= connectControlDiscardMaxPackLength; {
		audits = append(audits, auditFrame)
		oversized = discardTestArgsOfFrames(t, audits...)
	}
	cases = append(cases, struct {
		name string
		args *ConnectControlArgs
	}{name: "audits past the cap", args: oversized})

	for _, c := range cases {
		before := discardTestCounts()
		if answer, ok := DiscardConnectControl(c.args); ok || answer != nil {
			t.Errorf("%s was discarded", c.name)
		}
		if after := discardTestCounts(); after != before {
			t.Errorf("%s was counted as a discard: %v -> %v", c.name, before, after)
		}
	}
}

// The cap is exact: one audit whose encoded pack is the cap's length is
// discarded, and the same audit a byte longer is not.
func TestDiscardConnectControlCapIsExact(t *testing.T) {
	encodedLength := func(peerIdLength int) (*ConnectControlArgs, int) {
		audit := discardTestAudit(false)
		audit.PeerId = make([]byte, peerIdLength)
		args := discardTestArgs(t, audit)
		return args, len(args.Pack)
	}
	var atCap, pastCap *ConnectControlArgs
	for peerIdLength := 0; peerIdLength < 2*connectControlDiscardMaxPackLength && pastCap == nil; peerIdLength += 1 {
		args, length := encodedLength(peerIdLength)
		switch {
		case length == connectControlDiscardMaxPackLength:
			atCap = args
		case connectControlDiscardMaxPackLength < length && atCap != nil:
			pastCap = args
		}
	}
	if atCap == nil || pastCap == nil {
		t.Fatal("no pack lands exactly at the cap")
	}
	if !DiscardConnectControlPack(atCap.Pack) {
		t.Fatalf("a pack of %d characters, the cap, was not discarded", len(atCap.Pack))
	}
	if DiscardConnectControlPack(pastCap.Pack) {
		t.Fatalf("a pack of %d characters, past the cap, was discarded", len(pastCap.Pack))
	}
}

// Whatever the input, the discard either declines, or every frame of the pack
// it answered is a PeerAudit that decodes, within the cap. It never panics.
func FuzzDiscardConnectControlNeverAnswersAnAction(f *testing.F) {
	for _, messages := range [][]proto.Message{
		{discardTestAudit(false)},
		{discardTestAudit(true), discardTestAudit(false)},
		{&protocol.PeerAudit{}},
		{discardTestAudit(false), &protocol.CreateContract{DestinationId: server.NewId().Bytes()}},
		{&protocol.CloseContract{ContractId: server.NewId().Bytes()}, discardTestAudit(false)},
		{&protocol.Provide{}},
		{},
	} {
		args := discardTestArgs(f, messages...)
		packBytes, err := base64.StdEncoding.DecodeString(args.Pack)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(packBytes)
	}
	f.Add([]byte{0x0a, 0x04, 0x08, 0x0d, 0x12, 0x00})
	f.Add([]byte("not a pack"))
	f.Fuzz(func(t *testing.T, packBytes []byte) {
		for _, encodedPack := range []string{base64.StdEncoding.EncodeToString(packBytes), string(packBytes)} {
			if !DiscardConnectControlPack(encodedPack) {
				continue
			}
			if connectControlDiscardMaxPackLength < len(encodedPack) {
				t.Fatalf("a pack of %d characters, past the cap, was discarded", len(encodedPack))
			}
			decoded, err := base64.StdEncoding.DecodeString(encodedPack)
			if err != nil {
				t.Fatalf("a pack that is not base64 was discarded: %v", err)
			}
			pack := &protocol.Pack{}
			if err := proto.Unmarshal(decoded, pack); err != nil || len(pack.Frames) == 0 {
				t.Fatalf("a pack without frames was discarded: %v", err)
			}
			for _, frame := range pack.Frames {
				message, err := connect.FromFrame(frame)
				if _, ok := message.(*protocol.PeerAudit); err != nil || !ok || frame.MessageType != protocol.MessageType_TransferPeerAudit {
					t.Fatalf("a pack with a %s frame (%T, %v) was discarded", frame.MessageType, message, err)
				}
			}
		}
	})
}

// A PeerAudit beside a frame that answers, as a custom client might send,
// is a no-op: the answer survives, the pack reports no error, and no frame
// is counted as unhandled. Before, the audit's error made a client drop the
// replies beside it.
func TestConnectControlFramesKeepsRepliesBesideAPeerAudit(t *testing.T) {
	frames := discardTestFrames(t,
		discardTestAudit(false),
		&protocol.StreamAuthorization{ClockId: server.NewId().Bytes()},
		discardTestAudit(true),
	)
	defer returnConnectControlFrames(frames)
	unhandledBefore := controlFrameFailureCount("other", "unhandled_message")
	// the audits and the clock request all take the bounded other label
	okBefore := testutil.ToFloat64(defaultControlFrameMetrics.completions.WithLabelValues("internal", "other", "handler_ok"))
	errorBefore := testutil.ToFloat64(defaultControlFrameMetrics.completions.WithLabelValues("internal", "other", "error"))

	resultFrames, resultErr := ConnectControlFrames(t.Context(), server.NewId(), frames, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(resultFrames)
	if resultErr != nil {
		t.Fatalf("a pack with audits reported %v", resultErr)
	}
	if len(resultFrames) != 1 {
		t.Fatalf("%d replies, want the clock answer", len(resultFrames))
	}
	if message, err := connect.FromFrame(resultFrames[0]); err != nil {
		t.Fatal(err)
	} else if clock, ok := message.(*protocol.StreamAuthorization); !ok || clock.ClockUnixMillis == 0 {
		t.Fatalf("reply = %T %+v, want the clock answer", message, message)
	}
	result, err := connectControlResult(resultFrames, resultErr)
	if err != nil || result.Error != nil || result.Pack == "" {
		t.Fatalf("encoded answer = %+v, %v; want the reply and no error", result, err)
	}
	if unhandled := controlFrameFailureCount("other", "unhandled_message") - unhandledBefore; unhandled != 0 {
		t.Fatalf("%v audits were counted as unhandled", unhandled)
	}
	if handled := testutil.ToFloat64(defaultControlFrameMetrics.completions.WithLabelValues("internal", "other", "handler_ok")) - okBefore; handled != 3 {
		t.Fatalf("the frames completed handler_ok %v times, want 3", handled)
	}
	if failed := testutil.ToFloat64(defaultControlFrameMetrics.completions.WithLabelValues("internal", "other", "error")) - errorBefore; failed != 0 {
		t.Fatalf("the frames completed with an error %v times", failed)
	}
}

// In an [audit, close] pack the close applies, the pack reports no error and
// the audit is not counted as unhandled.
func TestConnectControlFramesTreatsPeerAuditAsNoOp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "ccf-audit", userId)
		sourceId := server.NewId()
		destinationId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "s", "s")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "d", "d")
		balanceCode, err := model.CreateBalanceCode(
			ctx, 1024*1024*1024, 365*24*60*60*1000000000, 0, "ccf-audit", "", "")
		connect.AssertEqual(t, err, nil)
		_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: networkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		contractId, _, err := model.CreateContract(
			ctx, networkId, sourceId, networkId, destinationId, 4*1024)
		connect.AssertEqual(t, err, nil)

		frames := append(discardTestFrames(t, discardTestAudit(false)), closeContractFrame(t, contractId))
		defer returnConnectControlFrames(frames)
		unhandledBefore := controlFrameFailureCount("other", "unhandled_message")
		outFrames, err := ConnectControlFrames(ctx, sourceId, frames, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(outFrames)
		if err != nil {
			t.Fatalf("an [audit, close] pack reported %v", err)
		}
		if unhandled := controlFrameFailureCount("other", "unhandled_message") - unhandledBefore; unhandled != 0 {
			t.Fatalf("the audit was counted as unhandled %v times", unhandled)
		}
		open := model.GetOpenContractIdsWithNoPartialClose(ctx, sourceId, destinationId)
		if _, stillOpen := open[contractId]; stillOpen {
			t.Fatal("the close beside the audit was not applied")
		}
	})
}

// A replay runs only the CreateContract frames it prepared. An audit in a
// replay is refused there like any other frame, ahead of the dispatch's
// no-op.
func TestReplayStillRefusesUnpreparedPeerAudit(t *testing.T) {
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	clientId, retiredId := server.NewId(), server.NewId()
	owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
	refusals, ok := PrepareConnectControlRefusals(ctx, clientId, refusalReplayArgs(t, refusalReplayCreate(retiredId, false)))
	if !ok {
		t.Fatal("the recorded refusal was not prepared")
	}
	frames := discardTestFrames(t, discardTestAudit(false), discardTestAudit(true))
	defer returnConnectControlFrames(frames)
	replayCtx := context.WithValue(ctx, connectControlRefusalsKey{}, refusals)
	resultFrames, err := ConnectControlFrames(replayCtx, clientId, frames, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(resultFrames)
	if len(resultFrames) != 0 {
		t.Fatalf("unprepared audits produced %d results", len(resultFrames))
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) != len(frames) {
		t.Fatalf("unprepared audits were not each refused: %v", err)
	}
	for _, frameErr := range joined.Unwrap() {
		if !errors.Is(frameErr, errConnectControlRefusalNotPrepared) {
			t.Fatalf("an unprepared audit failed for another reason: %v", frameErr)
		}
	}
}
