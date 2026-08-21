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

func TestTrySendPooledReceiveRefusesFullQueueWithoutWaiting(t *testing.T) {
	destination := make(chan []byte, 1)
	queued := clientconnect.MessagePoolGet(1)
	destination <- queued
	defer func() { clientconnect.MessagePoolReturn(<-destination) }()

	result := pooledMessageSendDelivered
	done := make(chan struct{})
	go func() {
		defer close(done)
		result = trySendPooledReceive(
			make(chan struct{}),
			nil,
			destination,
			clientconnect.MessagePoolGet(173),
		)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive-side queue offer waited for capacity")
	}
	if result != pooledMessageSendDropped {
		t.Fatalf("full receive queue result=%d, want dropped", result)
	}
}

func TestResidentTransportTrySendMessageRefusesWithoutWaiting(t *testing.T) {
	ctxDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := make(chan []byte, 1)
	queued := clientconnect.MessagePoolGet(1)
	destination <- queued
	defer func() { clientconnect.MessagePoolReturn(<-destination) }()

	transport := &ResidentTransport{
		ctx:  ctx,
		send: destination,
	}
	result := pooledMessageSendDelivered
	done := make(chan struct{})
	go func() {
		defer close(done)
		result = transport.trySendMessage(
			ctxDone,
			clientconnect.MessagePoolGet(197),
		)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resident transport receive handoff waited for capacity")
	}
	if result != pooledMessageSendDropped {
		t.Fatalf("full resident transport result=%d, want dropped", result)
	}
}

func TestProductionSocketReadersUseZeroWaitQueueAdmission(t *testing.T) {
	checks := []struct {
		path              string
		required          map[string]int
		forbiddenSnippets []string
	}{
		{
			path: "transport.go",
			required: map[string]int{
				"residentTransport.trySendMessage(": 2,
			},
			forbiddenSnippets: []string{
				"residentTransport.sendMessage(",
			},
		},
		{
			path: "resident.go",
			required: map[string]int{
				"trySendPooledReceive(": 6, // declaration plus five receive boundaries
			},
			forbiddenSnippets: []string{
				"case receive <- message:",
				"case forward <- message:",
				"case self.receive <- message:",
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
