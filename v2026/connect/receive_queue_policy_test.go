package connect

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

func TestSendPooledReceiveUnreliableRefusesWithOwnership(t *testing.T) {
	destination := make(chan []byte, 1)
	destination <- clientconnect.MessagePoolGet(1)
	defer func() { clientconnect.MessagePoolReturn(<-destination) }()
	message := clientconnect.MessagePoolGet(173)
	witness := clientconnect.MessagePoolShareReadOnly(message)
	result := sendPooledReceive(
		make(chan struct{}),
		nil,
		destination,
		message,
		clientconnect.CarrierReliabilityUnreliable,
	)
	if result != pooledMessageSendDropped {
		t.Fatalf("full datagram queue result=%d, want dropped", result)
	}
	if !clientconnect.MessagePoolReturn(witness) {
		t.Fatal("datagram refusal retained pooled bytes")
	}
}

func TestSendPooledReceiveReliableWaitsThenDelivers(t *testing.T) {
	destination := make(chan []byte, 1)
	destination <- clientconnect.MessagePoolGet(1)
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
			destination,
			message,
			clientconnect.CarrierReliabilityReliable,
			func() { close(waiting) },
		)
	}()
	<-waiting
	select {
	case <-done:
		t.Fatal("reliable handoff returned while its queue was full")
	default:
	}
	clientconnect.MessagePoolReturn(<-destination)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reliable handoff did not resume when capacity opened")
	}
	if result != pooledMessageSendDelivered {
		t.Fatalf("resumed reliable handoff result=%d, want delivered", result)
	}
	clientconnect.MessagePoolReturn(<-destination)
	if !clientconnect.MessagePoolReturn(witness) {
		t.Fatal("delivered reliable message has an unexpected pooled owner")
	}
}

func TestSendPooledReceiveReliableCancellationReturnsOwnership(t *testing.T) {
	destination := make(chan []byte, 1)
	destination <- clientconnect.MessagePoolGet(1)
	defer func() { clientconnect.MessagePoolReturn(<-destination) }()
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
			destination,
			message,
			clientconnect.CarrierReliabilityReliable,
			func() { close(waiting) },
		)
	}()
	<-waiting
	select {
	case <-done:
		t.Fatal("reliable handoff returned before cancellation")
	default:
	}
	close(ctxDone)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reliable handoff ignored cancellation")
	}
	if result != pooledMessageSendDone {
		t.Fatalf("canceled reliable handoff result=%d, want done", result)
	}
	if !clientconnect.MessagePoolReturn(witness) {
		t.Fatal("canceled reliable handoff retained pooled bytes")
	}
}

func benchmarkSendPooledReceiveReady(
	benchmark *testing.B,
	reliability clientconnect.CarrierReliability,
) {
	destination := make(chan []byte, 1)
	message := []byte{1}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		result := sendPooledReceive(nil, nil, destination, message, reliability)
		if result != pooledMessageSendDelivered {
			benchmark.Fatalf("ready handoff result=%d, want delivered", result)
		}
		<-destination
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
	destination := make(chan []byte, 1)
	transport := &ResidentTransport{ctx: context.Background(), send: destination}
	message := []byte{1}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		result := transport.sendReceivedMessage(nil, message, reliability)
		if result != pooledMessageSendDelivered {
			benchmark.Fatalf("ready resident handoff result=%d, want delivered", result)
		}
		<-destination
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
	destination := make(chan []byte, 1)
	first := clientconnect.MessagePoolGet(8)
	first[0] = 0
	second := clientconnect.MessagePoolGet(8)
	second[0] = 1
	destination <- first
	result := pooledMessageSendDropped
	done := make(chan struct{})
	waiting := make(chan struct{})
	go func() {
		defer close(done)
		result = sendPooledReceive(
			make(chan struct{}),
			nil,
			destination,
			second,
			clientconnect.CarrierReliabilityReliable,
			func() { close(waiting) },
		)
	}()
	<-waiting
	select {
	case <-done:
		t.Fatal("second reliable frame bypassed full queue")
	default:
	}
	got := <-destination
	if got[0] != 0 {
		t.Fatalf("first framed marker=%d want=0", got[0])
	}
	clientconnect.MessagePoolReturn(got)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second reliable frame did not resume")
	}
	if result != pooledMessageSendDelivered {
		t.Fatalf("second framed result=%d want delivered", result)
	}
	got = <-destination
	if got[0] != 1 {
		t.Fatalf("second framed marker=%d want=1", got[0])
	}
	clientconnect.MessagePoolReturn(got)
}

func TestExchangeGenerationRetiresAfterAnyUndeliveredFrame(t *testing.T) {
	if !pooledMessageSendKeepsGeneration(pooledMessageSendDelivered) {
		t.Fatal("delivered exchange frame retired its generation")
	}
	for _, result := range []pooledMessageSendResult{
		pooledMessageSendDropped,
		pooledMessageSendDone,
	} {
		if pooledMessageSendKeepsGeneration(result) {
			t.Fatalf("undelivered exchange result=%d kept a gapped generation", result)
		}
	}
}

func TestResidentTransportReceiveAdmissionUsesExactLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	destination := make(chan []byte, 1)
	destination <- clientconnect.MessagePoolGet(1)
	defer func() { clientconnect.MessagePoolReturn(<-destination) }()
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

func TestResidentForwardCallbackRetiresFullIngressWithoutWaiting(t *testing.T) {
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
		ctx:            ctx,
		cancel:         cancel,
		clientId:       clientId,
		forwardIngress: []chan residentForwardIngress{queue},
	}
	message := clientconnect.MessagePoolGet(211)
	witness := clientconnect.MessagePoolShareReadOnly(message)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resident.handleClientForward(
			clientconnect.TransferPath{
				SourceId:      clientconnect.Id(clientId),
				DestinationId: clientconnect.Id(destinationId),
			},
			message,
		)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resident forward callback waited for ingress capacity")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("reliable forward refusal did not retire the resident generation")
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
				"MessagePoolShareReadOnly(transferFrameBytes)",
				"case self.forwardIngress[shardIndex] <- message:",
				"default:",
				"self.cancel()",
			},
			forbidden: []string{
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
}
