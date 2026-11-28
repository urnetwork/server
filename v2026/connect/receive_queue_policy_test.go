package connect

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// A receive boundary's destination for a policy test: an edge queue, or a
// resident's route channel, with a channel's send and receive.
type receivePolicyDestination struct {
	name        string
	capacity    int
	destination pooledMessageDestination
	send        func([]byte)
	receive     func() []byte
}

// Each kind of destination at each capacity: a queue of capacity 1 is a
// channel, and above 1 it keeps its messages after the first in its ring.
func receivePolicyDestinationsForTest() []receivePolicyDestination {
	destinations := []receivePolicyDestination{}
	for _, capacity := range []int{1, 3} {
		queue := newMessageQueue(capacity)
		channel := make(chan []byte, capacity)
		destinations = append(destinations, receivePolicyDestination{
			name:        fmt.Sprintf("queue/%d", capacity),
			capacity:    capacity,
			destination: queue,
			send: func(message []byte) {
				queue.sendForTest(message)
			},
			receive: func() []byte {
				message, _ := queue.receiveForTest()
				return message
			},
		}, receivePolicyDestination{
			name:        fmt.Sprintf("channel/%d", capacity),
			capacity:    capacity,
			destination: pooledMessageChannel(channel),
			send: func(message []byte) {
				channel <- message
			},
			receive: func() []byte {
				return <-channel
			},
		})
	}
	return destinations
}

// Fills the destination with capacity messages.
func (self receivePolicyDestination) fill() {
	for range self.capacity {
		self.send(clientconnect.MessagePoolGet(1))
	}
}

// Returns capacity messages to the pool.
func (self receivePolicyDestination) drain() {
	for range self.capacity {
		clientconnect.MessagePoolReturn(self.receive())
	}
}

func TestSendPooledReceiveUnreliableRefusesWithOwnership(t *testing.T) {
	for _, d := range receivePolicyDestinationsForTest() {
		d.fill()
		message := clientconnect.MessagePoolGet(173)
		witness := clientconnect.MessagePoolShareReadOnly(message)
		result := sendPooledReceive(
			make(chan struct{}),
			nil,
			d.destination,
			message,
			clientconnect.CarrierReliabilityUnreliable,
		)
		if result != pooledMessageSendDropped {
			t.Fatalf("%s: full datagram queue result=%d, want dropped", d.name, result)
		}
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("%s: datagram refusal retained pooled bytes", d.name)
		}
		d.drain()
	}
}

func TestSendPooledReceiveReliableWaitsThenDelivers(t *testing.T) {
	for _, d := range receivePolicyDestinationsForTest() {
		d.fill()
		message := clientconnect.MessagePoolGet(181)
		witness := clientconnect.MessagePoolShareReadOnly(message)
		result := pooledMessageSendDropped
		done := make(chan struct{})
		waiting := make(chan struct{})
		go func() {
			defer close(done)
			result = sendPooledReceive(
				make(chan struct{}),
				nil,
				d.destination,
				message,
				clientconnect.CarrierReliabilityReliable,
				func() { close(waiting) },
			)
		}()
		<-waiting
		select {
		case <-done:
			t.Fatalf("%s: reliable handoff returned while its queue was full", d.name)
		default:
		}
		clientconnect.MessagePoolReturn(d.receive())
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s: reliable handoff did not resume when capacity opened", d.name)
		}
		if result != pooledMessageSendDelivered {
			t.Fatalf("%s: resumed reliable handoff result=%d, want delivered", d.name, result)
		}
		d.drain()
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("%s: delivered reliable message has an unexpected pooled owner", d.name)
		}
	}
}

func TestSendPooledReceiveReliableCancellationReturnsOwnership(t *testing.T) {
	for _, d := range receivePolicyDestinationsForTest() {
		d.fill()
		ctxDone := make(chan struct{})
		message := clientconnect.MessagePoolGet(191)
		witness := clientconnect.MessagePoolShareReadOnly(message)
		result := pooledMessageSendDelivered
		done := make(chan struct{})
		waiting := make(chan struct{})
		go func() {
			defer close(done)
			result = sendPooledReceive(
				ctxDone,
				nil,
				d.destination,
				message,
				clientconnect.CarrierReliabilityReliable,
				func() { close(waiting) },
			)
		}()
		<-waiting
		select {
		case <-done:
			t.Fatalf("%s: reliable handoff returned before cancellation", d.name)
		default:
		}
		close(ctxDone)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s: reliable handoff ignored cancellation", d.name)
		}
		if result != pooledMessageSendDone {
			t.Fatalf("%s: canceled reliable handoff result=%d, want done", d.name, result)
		}
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("%s: canceled reliable handoff retained pooled bytes", d.name)
		}
		d.drain()
	}
}

// A closed destination queue is a completed lifecycle: the boundary returns
// the message to the pool and reports done, on either lane.
func TestSendPooledReceiveClosedQueueReturnsOwnership(t *testing.T) {
	for _, capacity := range []int{0, 1, 4} {
		for _, reliability := range []clientconnect.CarrierReliability{
			clientconnect.CarrierReliabilityReliable,
			clientconnect.CarrierReliabilityUnreliable,
		} {
			queue := newMessageQueue(capacity)
			queue.close()
			message := clientconnect.MessagePoolGet(193)
			witness := clientconnect.MessagePoolShareReadOnly(message)
			if result := sendPooledReceive(nil, nil, queue, message, reliability); result != pooledMessageSendDone {
				t.Fatalf("capacity %d reliability %d: closed queue result=%d, want done", capacity, reliability, result)
			}
			if !clientconnect.MessagePoolReturn(witness) {
				t.Fatalf("capacity %d reliability %d: closed queue refusal retained pooled bytes", capacity, reliability)
			}
		}
	}
}

func benchmarkSendPooledReceiveReady(
	benchmark *testing.B,
	reliability clientconnect.CarrierReliability,
) {
	destination := newMessageQueue(1)
	message := []byte{1}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		result := sendPooledReceive(nil, nil, destination, message, reliability)
		if result != pooledMessageSendDelivered {
			benchmark.Fatalf("ready handoff result=%d, want delivered", result)
		}
		destination.tryReceive()
	}
}

func BenchmarkSendPooledReceiveReadyReliable(benchmark *testing.B) {
	benchmarkSendPooledReceiveReady(benchmark, clientconnect.CarrierReliabilityReliable)
}

func BenchmarkSendPooledReceiveReadyUnreliable(benchmark *testing.B) {
	benchmarkSendPooledReceiveReady(benchmark, clientconnect.CarrierReliabilityUnreliable)
}

func benchmarkResidentTransportSendReceivedReady(
	benchmark *testing.B,
	reliability clientconnect.CarrierReliability,
) {
	destination := newMessageQueue(1)
	transport := &ResidentTransport{ctx: context.Background(), send: destination}
	message := []byte{1}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		result := transport.sendReceivedMessage(nil, message, reliability)
		if result != pooledMessageSendDelivered {
			benchmark.Fatalf("ready resident handoff result=%d, want delivered", result)
		}
		destination.tryReceive()
	}
}

func BenchmarkResidentTransportSendReceivedReadyReliable(benchmark *testing.B) {
	benchmarkResidentTransportSendReceivedReady(
		benchmark,
		clientconnect.CarrierReliabilityReliable,
	)
}

func BenchmarkResidentTransportSendReceivedReadyUnreliable(benchmark *testing.B) {
	benchmarkResidentTransportSendReceivedReady(
		benchmark,
		clientconnect.CarrierReliabilityUnreliable,
	)
}

// Reproduces the internal-hop root cause with explicit sequence markers. A
// full reliable exchange queue must preserve 0,1 order instead of dropping 1
// and letting a later frame pin Transfer recovery behind an artificial gap.
func TestReliableExchangeQueueSaturationPreservesFramedOrder(t *testing.T) {
	for _, d := range receivePolicyDestinationsForTest() {
		for marker := range d.capacity {
			message := clientconnect.MessagePoolGet(8)
			message[0] = byte(marker)
			d.send(message)
		}
		last := clientconnect.MessagePoolGet(8)
		last[0] = byte(d.capacity)
		result := pooledMessageSendDropped
		done := make(chan struct{})
		waiting := make(chan struct{})
		go func() {
			defer close(done)
			result = sendPooledReceive(
				make(chan struct{}),
				nil,
				d.destination,
				last,
				clientconnect.CarrierReliabilityReliable,
				func() { close(waiting) },
			)
		}()
		<-waiting
		select {
		case <-done:
			t.Fatalf("%s: last reliable frame bypassed full queue", d.name)
		default:
		}
		got := d.receive()
		if got[0] != 0 {
			t.Fatalf("%s: first framed marker=%d want=0", d.name, got[0])
		}
		clientconnect.MessagePoolReturn(got)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s: last reliable frame did not resume", d.name)
		}
		if result != pooledMessageSendDelivered {
			t.Fatalf("%s: last framed result=%d want delivered", d.name, result)
		}
		for marker := 1; marker <= d.capacity; marker += 1 {
			got = d.receive()
			if int(got[0]) != marker {
				t.Fatalf("%s: framed marker=%d want=%d", d.name, got[0], marker)
			}
			clientconnect.MessagePoolReturn(got)
		}
	}
}

// A queue refusal consumes a complete message without writing its stream bytes.
// This policy control is not proof of either actual owner-loop recovery path.
func TestExchangeGenerationKeepsCompleteRefusalButStopsDone(t *testing.T) {
	for _, c := range []struct {
		result pooledMessageSendResult
		keep   bool
	}{
		{result: pooledMessageSendDelivered, keep: true},
		{result: pooledMessageSendDropped, keep: true},
		{result: pooledMessageSendDone, keep: false},
		{result: pooledMessageSendResult(-1), keep: false},
		{result: pooledMessageSendResult(99), keep: false},
	} {
		if got := pooledMessageSendKeepsGeneration(c.result); got != c.keep {
			t.Errorf("exchange result=%d keeps generation=%t, want %t", c.result, got, c.keep)
		}
	}
}

func TestResidentTransportReceiveAdmissionUsesExactLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	destination := newMessageQueue(1)
	destination.sendForTest(clientconnect.MessagePoolGet(1))
	defer func() {
		message, _ := destination.receiveForTest()
		clientconnect.MessagePoolReturn(message)
	}()
	transport := &ResidentTransport{ctx: ctx, send: destination}
	waiting := make(chan struct{})
	transport.beforeReliableReceiveWaitForTest = func() { close(waiting) }

	unreliable := clientconnect.MessagePoolGet(197)
	if result := transport.sendReceivedMessage(
		make(chan struct{}),
		unreliable,
		clientconnect.CarrierReliabilityUnreliable,
	); result != pooledMessageSendDropped {
		t.Fatalf("full resident DATAGRAM lane result=%d, want dropped", result)
	}

	reliable := clientconnect.MessagePoolGet(199)
	witness := clientconnect.MessagePoolShareReadOnly(reliable)
	result := pooledMessageSendDelivered
	done := make(chan struct{})
	go func() {
		defer close(done)
		result = transport.sendReceivedMessage(
			make(chan struct{}),
			reliable,
			clientconnect.CarrierReliabilityReliable,
		)
	}()
	<-waiting
	select {
	case <-done:
		t.Fatal("resident reliable stream did not backpressure")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resident reliable stream ignored generation cancellation")
	}
	if result != pooledMessageSendDone || !clientconnect.MessagePoolReturn(witness) {
		t.Fatal("resident cancellation did not return reliable message ownership")
	}
}

func TestProductionSocketReadersDeclareExactReceiveLanes(t *testing.T) {
	checks := []struct {
		path              string
		required          map[string]int
		forbiddenSnippets []string
	}{
		{
			path: "transport.go",
			required: map[string]int{
				"residentTransport.sendReceivedMessage(": 2,
				"connect.CarrierReliabilityUnreliable":   1,
			},
			forbiddenSnippets: []string{
				"residentTransport.trySendMessage(",
			},
		},
		{
			path: "resident.go",
			required: map[string]int{
				"sendPooledReceive(":                6, // declaration plus five receive boundaries
				"pooledMessageSendKeepsGeneration(": 3, // declaration plus transport/forward writers
				"ReceiveReliability:":               1, // resident's framed TCP receive route
			},
			forbiddenSnippets: []string{
				"trySendPooledReceive(",
			},
		},
	}
	for _, check := range checks {
		sourceBytes, err := os.ReadFile(check.path)
		if err != nil {
			t.Fatalf("read %s: %v", check.path, err)
		}
		source := string(sourceBytes)
		for token, want := range check.required {
			if got := strings.Count(source, token); got != want {
				t.Fatalf("%s contains %q %d time(s), want %d; audit the new receive boundary", check.path, token, got, want)
			}
		}
		for _, snippet := range check.forbiddenSnippets {
			if strings.Contains(source, snippet) {
				t.Fatalf("%s reintroduced blocking socket receive handoff %q", check.path, snippet)
			}
		}
	}
}

// A full forward shard refuses the one frame that found it full and keeps the
// generation. The callback returns on the receive loop's own goroutine with the
// shard still full, so it cannot have waited; the frame is counted at its
// boundary and its pooled bytes go back to the caller. Retiring here used to
// discard every frame the generation held, in both directions, for a refusal
// the frame's source recovers end to end by resending this one frame.
func TestResidentForwardCallbackRefusesFullIngressWithoutWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientId := server.NewId()
	destinationId := server.NewId()
	queue := make(chan residentForwardIngress, 1)
	queued := clientconnect.MessagePoolGet(1)
	queue <- residentForwardIngress{transferFrameBytes: queued}
	defer func() {
		message := <-queue
		clientconnect.MessagePoolReturn(message.transferFrameBytes)
	}()
	resident := &Resident{
		ctx:      ctx,
		cancel:   cancel,
		clientId: clientId,
		forwardIngress: []residentForwardIngressShard{
			{queue: queue},
		},
	}
	resident.forwardIngress[0].startOnce.Do(func() {})
	droppedMessages := receiveQueueDroppedMessagesCounter.WithLabelValues(receiveQueueBoundaryResidentClientForward)
	droppedBytes := receiveQueueDroppedBytesCounter.WithLabelValues(receiveQueueBoundaryResidentClientForward)
	messagesBefore, bytesBefore := testutil.ToFloat64(droppedMessages), testutil.ToFloat64(droppedBytes)
	message := clientconnect.MessagePoolGet(211)
	witness := clientconnect.MessagePoolShareReadOnly(message)
	resident.handleClientForward(
		clientconnect.TransferPath{
			SourceId:      clientconnect.Id(clientId),
			DestinationId: clientconnect.Id(destinationId),
		},
		message,
	)
	if len(queue) != 1 {
		t.Fatalf("forward shard holds %d frames, want the one it held before the refusal", len(queue))
	}
	if ctx.Err() != nil {
		t.Fatal("a full forward shard retired the resident generation")
	}
	if got := testutil.ToFloat64(droppedMessages) - messagesBefore; got != 1 {
		t.Errorf("forward refusal counted %v dropped messages, want 1", got)
	}
	if got := testutil.ToFloat64(droppedBytes) - bytesBefore; got != float64(len(message)) {
		t.Errorf("forward refusal counted %v dropped bytes, want %d", got, len(message))
	}
	clientconnect.MessagePoolReturn(message)
	if !clientconnect.MessagePoolReturn(witness) {
		t.Fatal("resident forward refusal retained callback-owned bytes")
	}
}

func TestResidentControlCallbackRefusesAndRetiresFullIngressWithoutWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clientSettings := clientconnect.DefaultClientSettings()
	clientSettings.ControlPingTimeout = 0
	clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	clientSettings.Log = clientconnect.NewNoopLogger()
	client := clientconnect.NewClient(
		ctx,
		clientconnect.ControlId,
		clientconnect.NewNoContractClientOob(),
		clientSettings,
	)
	queue := make(chan []*protocol.Frame, 1)
	queued := &protocol.Frame{MessageBytes: clientconnect.MessagePoolGet(1)}
	queue <- []*protocol.Frame{queued}
	clientId := server.NewId()
	resident := &Resident{
		ctx:            ctx,
		cancel:         cancel,
		clientId:       clientId,
		client:         client,
		controlIngress: queue,
	}
	message := clientconnect.MessagePoolGet(223)
	witness := clientconnect.MessagePoolShareReadOnly(message)
	frame := &protocol.Frame{
		MessageType:  protocol.MessageType_TestSimpleMessage,
		MessageBytes: message,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resident.handleClientReceive(
			clientconnect.SourceId(clientconnect.Id(clientId)),
			[]*protocol.Frame{frame},
			clientconnect.Peer{},
		)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resident control callback waited for ingress capacity")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("reliable control refusal did not retire the resident generation")
	}
	clientconnect.MessagePoolReturn(message)
	if !clientconnect.MessagePoolReturn(witness) {
		t.Fatal("resident control refusal retained callback-owned bytes")
	}
	returnResidentControlFrames(<-queue)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := client.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("close resident test client: %v", err)
	}
}

func TestResidentClientCallbacksContainOnlyZeroWaitIngressWork(t *testing.T) {
	sourceBytes, err := os.ReadFile("resident.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "resident.go", sourceBytes, 0)
	if err != nil {
		t.Fatal(err)
	}
	functionSource := map[string]string{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv == nil {
			continue
		}
		if function.Name.Name != "handleClientReceive" && function.Name.Name != "handleClientForward" {
			continue
		}
		start := fileSet.Position(function.Pos()).Offset
		end := fileSet.Position(function.End()).Offset
		functionSource[function.Name.Name] = string(sourceBytes[start:end])
	}
	checks := []struct {
		name      string
		required  []string
		forbidden []string
	}{
		{
			name: "handleClientReceive",
			required: []string{
				"controlIngressAdmission.start()",
				"shareResidentControlFrames(frames)",
				"case self.controlIngress <- shared:",
				"default:",
				"self.Cancel()",
			},
			forbidden: []string{
				"controlLimiter.delay()",
				"HandleControlFrames(",
				"time.After(",
				"stateLock.",
			},
		},
		{
			name: "handleClientForward",
			required: []string{
				"forwardIngressAdmission.start()",
				"shardIndex := int(destinationId[len(destinationId)-1]) % len(self.forwardIngress)",
				"queue := self.startClientForwardIngress(shardIndex)",
				"MessagePoolShareReadOnly(transferFrameBytes)",
				"case queue <- message:",
				"default:",
				"self.releaseForwardIngress(shared)",
				"recordReceiveQueueDrop(receiveQueueBoundaryResidentClientForward,",
			},
			forbidden: []string{
				"self.cancel()",
				"self.Cancel()",
				"stateLock.",
				"HasActiveContract(",
				"NewResidentForward(",
				"ForwardTimeout",
				"time.After(",
			},
		},
	}
	for _, check := range checks {
		source, ok := functionSource[check.name]
		if !ok {
			t.Fatalf("callback %s is missing from production audit", check.name)
		}
		for _, snippet := range check.required {
			if !strings.Contains(source, snippet) {
				t.Errorf("callback %s is missing zero-wait policy %q", check.name, snippet)
			}
		}
		for _, snippet := range check.forbidden {
			if strings.Contains(source, snippet) {
				t.Errorf("callback %s performs blocking work %q", check.name, snippet)
			}
		}
	}
	// A refused forward frame is recovered end to end by its source; retiring
	// the generation for it would discard everything else the generation holds.
	for _, snippet := range []string{"self.cancel()", "self.Cancel()"} {
		if strings.Contains(functionSource["handleClientForward"], snippet) {
			t.Errorf("callback handleClientForward retires the resident generation with %q on a refused frame", snippet)
		}
	}
}
