package controller

// The discard of a pack made only of PeerAudit frames
// (connect_control_discard.go), and the dispatch's no-op for an audit in any
// other pack. The discard reads only its argument, so most of these tests run
// without a database or Redis: a discard that reached either would fail here
// instead of answering.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
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

// The four discard counts, by abuse claimed and bearer presented.
func discardTestCounts() [2][2]float64 {
	counts := [2][2]float64{}
	for abuse, abuseLabel := range []string{"none", "claimed"} {
		for bearer, bearerLabel := range []string{"absent", "present"} {
			counts[abuse][bearer] = testutil.ToFloat64(connectControlDiscardCounter.WithLabelValues(abuseLabel, bearerLabel))
		}
	}
	return counts
}

// A control request with the Authorization header, none when empty.
func discardTestRequest(authorization string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/connect/control", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return request
}

// The largest audit connect can build: both ids and every counter at its
// widest encoding.
func discardTestLargestAudit() *protocol.PeerAudit {
	return &protocol.PeerAudit{
		PeerId:              server.NewId().Bytes(),
		StreamId:            server.NewId().Bytes(),
		Duration:            math.MaxUint64,
		Abuse:               true,
		BadContractCount:    math.MaxUint64,
		DiscardedByteCount:  math.MaxUint64,
		DiscardedCount:      math.MaxUint64,
		BadMessageByteCount: math.MaxUint64,
		BadMessageCount:     math.MaxUint64,
		SendByteCount:       math.MaxUint64,
		SendCount:           math.MaxUint64,
		ResendByteCount:     math.MaxUint64,
		ResendCount:         math.MaxUint64,
	}
}

// The request of one audit whose encoded pack is exactly length characters,
// padded through its peer id, and the same audit one byte longer.
func discardTestAuditPackAtLength(t testing.TB, length int) (atLength *ConnectControlArgs, past *ConnectControlArgs) {
	t.Helper()
	for peerIdLength := 0; peerIdLength < 2*length && past == nil; peerIdLength += 1 {
		audit := discardTestAudit(false)
		audit.PeerId = make([]byte, peerIdLength)
		args := discardTestArgs(t, audit)
		switch {
		case len(args.Pack) == length:
			atLength = args
		case length < len(args.Pack) && atLength != nil:
			past = args
		}
	}
	if atLength == nil || past == nil {
		t.Fatalf("no audit pack lands exactly at %d characters", length)
	}
	return atLength, past
}

// The request of the raw pack bytes, as a client could encode them.
func discardTestArgsOfBytes(packBytes []byte) *ConnectControlArgs {
	return &ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)}
}

// One frames record holding frameBytes.
func discardTestPackOfFrameBytes(frameBytes []byte) []byte {
	b := protowire.AppendTag(nil, connectControlDiscardPackFrames, protowire.BytesType)
	return protowire.AppendBytes(b, frameBytes)
}

// A frame's message type and message bytes, as fields.
func discardTestFrameFields(messageType protocol.MessageType, messageBytes []byte) []byte {
	b := protowire.AppendTag(nil, connectControlDiscardFrameMessageType, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(messageType))
	b = protowire.AppendTag(b, connectControlDiscardFrameMessageBytes, protowire.BytesType)
	return protowire.AppendBytes(b, messageBytes)
}

// A pack of one audit is answered with exactly what an authenticated pack
// that does nothing gets, and counted once by whether it claims abuse and
// whether the request presented a bearer credential, which is not verified.
func TestDiscardConnectControlAnswersAuditOnlyPacks(t *testing.T) {
	want, err := connectControlResult(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		args  *ConnectControlArgs
		abuse int
	}{
		{name: "one audit", args: discardTestArgs(t, discardTestAudit(false))},
		{name: "one abuse report", args: discardTestArgs(t, discardTestAudit(true)), abuse: 1},
		{name: "an audit with no fields", args: discardTestArgs(t, &protocol.PeerAudit{})},
		{name: "the largest audit", args: discardTestArgs(t, discardTestLargestAudit()), abuse: 1},
	} {
		for _, credential := range []struct {
			authorization string
			bearer        int
		}{
			{authorization: "", bearer: 0},
			{authorization: "Bearer ", bearer: 0},
			{authorization: "Basic synthetic", bearer: 0},
			{authorization: "Bearer synthetic-unverifiable-token", bearer: 1},
		} {
			before := discardTestCounts()
			answer, ok := DiscardConnectControl(discardTestRequest(credential.authorization), c.args)
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
			wantAfter := before
			wantAfter[c.abuse][credential.bearer] += 1
			if after := discardTestCounts(); after != wantAfter {
				t.Fatalf("%s with authorization %q: counts [abuse][bearer] = %v, want %v", c.name, credential.authorization, after, wantAfter)
			}
		}
	}
}

// Every connect version's largest audit fits the cap with room to spare.
func TestDiscardConnectControlCapFitsTheLargestAudit(t *testing.T) {
	if length := len(discardTestArgs(t, discardTestLargestAudit()).Pack); connectControlDiscardMaxPackLength/2 < length {
		t.Fatalf("the largest audit encodes to %d characters, more than half the %d cap", length, connectControlDiscardMaxPackLength)
	}
}

// Anything but a pack of exactly one decodable audit within the cap takes the
// ordinary path: more than one audit, every other message type alone and
// after an audit (each carrying an audit's own bytes, so only the type can
// refuse it), an action before or after an audit, an audit whose bytes do not
// decode, a frame or audit that repeats a field, carries a field outside its
// schema or a field with the wrong wire type, a pack with any field beside
// its frame, and empty, absent, malformed or oversized packs. None is
// counted.
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
		{name: "two audits", args: discardTestArgsOfFrames(t, auditFrame, auditFrame)},
		{name: "three audits with an abuse report", args: discardTestArgs(t, discardTestAudit(false), discardTestAudit(true), discardTestAudit(false))},
		{name: "an audit marked raw", args: discardTestArgsOfFrames(t, &protocol.Frame{MessageType: protocol.MessageType_TransferPeerAudit, MessageBytes: auditFrame.MessageBytes, Raw: true})},
		{name: "a frame with its message bytes twice", args: discardTestArgsOfBytes(discardTestPackOfFrameBytes(protowire.AppendBytes(protowire.AppendTag(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, auditFrame.MessageBytes), connectControlDiscardFrameMessageBytes, protowire.BytesType), auditFrame.MessageBytes)))},
		// proto decodes the last message type, so this frame is a CreateContract
		{name: "a frame typed audit then create", args: discardTestArgsOfBytes(discardTestPackOfFrameBytes(protowire.AppendVarint(protowire.AppendTag(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, createFrame.MessageBytes), connectControlDiscardFrameMessageType, protowire.VarintType), uint64(protocol.MessageType_TransferCreateContract))))},
		{name: "a frame with its message type as bytes", args: discardTestArgsOfBytes(discardTestPackOfFrameBytes(protowire.AppendBytes(protowire.AppendTag(nil, connectControlDiscardFrameMessageType, protowire.BytesType), []byte{byte(protocol.MessageType_TransferPeerAudit)})))},
		{name: "an audit with its peer id twice", args: discardTestArgsOfBytes(discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, protowire.AppendBytes(protowire.AppendTag(auditFrame.MessageBytes, 1, protowire.BytesType), server.NewId().Bytes()))))},
		{name: "an audit with a field outside its schema", args: discardTestArgsOfBytes(discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, protowire.AppendVarint(protowire.AppendTag(auditFrame.MessageBytes, 99, protowire.VarintType), 1))))},
		{name: "an audit with its duration as bytes", args: discardTestArgsOfBytes(discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), []byte{60}))))},
		{name: "a pack with a message id beside its audit", args: discardTestArgsOfBytes(append(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), server.NewId().Bytes()), discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, auditFrame.MessageBytes))...))},
		{name: "a pack with trailing bytes", args: discardTestArgsOfBytes(append(discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, auditFrame.MessageBytes)), 0x00))},
		{name: "a frames record that is a varint", args: discardTestArgsOfBytes(protowire.AppendVarint(protowire.AppendTag(nil, connectControlDiscardPackFrames, protowire.VarintType), 13))},
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
		if answer, ok := DiscardConnectControl(discardTestRequest("Bearer synthetic-unverifiable-token"), c.args); ok || answer != nil {
			t.Errorf("%s was discarded", c.name)
		}
		if after := discardTestCounts(); after != before {
			t.Errorf("%s was counted as a discard: %v -> %v", c.name, before, after)
		}
	}
}

// The cap is exactly 512 characters: one audit whose encoded pack is that
// long is discarded, and the same audit a byte longer is not. Every connect
// version's audit fits (TestDiscardConnectControlCapFitsTheLargestAudit); a
// longer pack authenticates and reaches the dispatch's no-op.
func TestDiscardConnectControlCapIsExact(t *testing.T) {
	atCap, pastCap := discardTestAuditPackAtLength(t, 512)
	if !DiscardConnectControlPack(atCap.Pack, true) {
		t.Fatalf("a pack of %d characters, the cap, was not discarded", len(atCap.Pack))
	}
	if DiscardConnectControlPack(pastCap.Pack, true) {
		t.Fatalf("a pack of %d characters, past the cap, was discarded", len(pastCap.Pack))
	}
}

// Inputs of exactly the cap's length that each push one part of the discard
// hardest: an answered audit, and refused frames, repeats and noise.
func discardTestInputsAtTheCap(t testing.TB) map[string]string {
	t.Helper()
	atCap, _ := discardTestAuditPackAtLength(t, connectControlDiscardMaxPackLength)
	packByteCount := base64.StdEncoding.DecodedLen(connectControlDiscardMaxPackLength)
	// the most records whose pack still fits the cap
	grow := func(build func(records int) []byte) []byte {
		records := 0
		for len(build(records+1)) <= packByteCount {
			records += 1
		}
		return build(records)
	}
	emptyFrames := grow(func(records int) []byte {
		b := []byte{}
		for range records {
			b = protowire.AppendBytes(protowire.AppendTag(b, connectControlDiscardPackFrames, protowire.BytesType), nil)
		}
		return b
	})
	repeatedMessageBytes := grow(func(records int) []byte {
		b := protowire.AppendVarint(protowire.AppendTag(nil, connectControlDiscardFrameMessageType, protowire.VarintType), uint64(protocol.MessageType_TransferPeerAudit))
		for range records {
			b = protowire.AppendBytes(protowire.AppendTag(b, connectControlDiscardFrameMessageBytes, protowire.BytesType), []byte{0})
		}
		return discardTestPackOfFrameBytes(b)
	})
	repeatedPeerIds := grow(func(records int) []byte {
		b := []byte{}
		for range records {
			b = protowire.AppendBytes(protowire.AppendTag(b, 1, protowire.BytesType), []byte{0})
		}
		return discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, b))
	})
	noise := make([]byte, packByteCount)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{
		"an audit":                      atCap.Pack,
		"empty frames":                  discardTestArgsOfBytes(emptyFrames).Pack,
		"a frame of repeated bytes":     discardTestArgsOfBytes(repeatedMessageBytes).Pack,
		"an audit of repeated peer ids": discardTestArgsOfBytes(repeatedPeerIds).Pack,
		"noise":                         discardTestArgsOfBytes(noise).Pack,
	}
	for name, input := range inputs {
		if len(input) < connectControlDiscardMaxPackLength-4 || connectControlDiscardMaxPackLength < len(input) {
			t.Fatalf("%s is %d characters, not at the %d cap", name, len(input), connectControlDiscardMaxPackLength)
		}
	}
	return inputs
}

// At the cap, before any credential is read, the discard allocates a fixed
// handful of small objects whatever the input: at most 8 allocations and
// 2 KiB a request. Before the one-frame scan, the empty frames cost one
// object each, several hundred at the old cap.
func TestDiscardConnectControlCostIsBoundedAtTheCap(t *testing.T) {
	const runs = 50
	for name, input := range discardTestInputsAtTheCap(t) {
		allocs := testing.AllocsPerRun(runs, func() {
			DiscardConnectControlPack(input, true)
		})
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range runs {
			DiscardConnectControlPack(input, true)
		}
		runtime.ReadMemStats(&after)
		allocatedBytes := float64(after.TotalAlloc-before.TotalAlloc) / runs
		if 8 < allocs || 2048 < allocatedBytes {
			t.Errorf("%s at the cap: %.0f allocations and %.0f bytes a request, want at most 8 and 2048", name, allocs, allocatedBytes)
		}
	}
}

// The discard at the cap, on each input that pushes one part of it hardest.
func BenchmarkDiscardConnectControlPack(b *testing.B) {
	inputs := discardTestInputsAtTheCap(b)
	inputs["the largest audit"] = discardTestArgs(b, discardTestLargestAudit()).Pack
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		input := inputs[name]
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				DiscardConnectControlPack(input, true)
			}
		})
	}
}

// Whatever the input, the discard either declines, or the pack it answered is
// within the cap and is exactly one frame, a PeerAudit that decodes. It never
// panics.
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
	f.Add([]byte{0x2a, 0x04, 0x08, 0x0d, 0x12, 0x00})
	f.Add(discardTestPackOfFrameBytes(discardTestFrameFields(protocol.MessageType_TransferPeerAudit, []byte{0x18, 0x01, 0x18, 0x00})))
	f.Add([]byte("not a pack"))
	f.Fuzz(func(t *testing.T, packBytes []byte) {
		for _, encodedPack := range []string{base64.StdEncoding.EncodeToString(packBytes), string(packBytes)} {
			if !DiscardConnectControlPack(encodedPack, false) {
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
			if err := proto.Unmarshal(decoded, pack); err != nil || len(pack.Frames) != 1 {
				t.Fatalf("a pack of %d frames was discarded: %v", len(pack.Frames), err)
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
