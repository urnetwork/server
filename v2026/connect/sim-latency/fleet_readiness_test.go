// Construction readiness is distinct from accounting and measurement dynamics.
package main

import (
	"bufio"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Exercise the real private command pipe and parsed-response queue. Only one
// command belongs to construction; cleanup closes and joins its reader.
func newFleetReadinessProcess(t *testing.T, index int) (*fleetProcess, <-chan string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		command, _ := bufio.NewReader(reader).ReadString('\n')
		requests <- command
	}()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
		<-readerDone
	})
	return &fleetProcess{
		index: index, done: make(chan struct{}),
		accountingCommandWriter: writer,
		accountingResponses:     make(chan fleetAccountingResponse),
	}, requests
}

// Keep the controller's lifetime separate from its one consumable result.
func runFleetReadinessForTest(t *testing.T, ctx context.Context, procs []*fleetProcess) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- waitFleetShardsReady(ctx, procs)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	return done
}

// Every request must be dispatched before waiting, and one reply never
// authorizes prewarming a partially constructed fleet.
func TestFleetReadinessWaitsForEveryConstructionReply(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, firstRequest := newFleetReadinessProcess(t, 0)
	second, secondRequest := newFleetReadinessProcess(t, 1)
	done := runFleetReadinessForTest(t, ctx, []*fleetProcess{first, second})
	for _, request := range []<-chan string{firstRequest, secondRequest} {
		if command := <-request; command != "ready\n" {
			t.Fatalf("construction received command %q", command)
		}
	}
	first.accountingResponses <- fleetAccountingResponse{}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("incomplete construction authorized prewarm: %v", err)
	}
	select {
	case second.accountingResponses <- fleetAccountingResponse{}:
		t.Fatal("canceled construction retained a response waiter")
	default:
	}
}

// Complete acknowledgements release only the construction phase, without a
// snapshot or start-dynamics command hidden in the handshake.
func TestFleetReadinessCompletesAfterAllReplies(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, firstRequest := newFleetReadinessProcess(t, 0)
	second, secondRequest := newFleetReadinessProcess(t, 1)
	done := runFleetReadinessForTest(t, ctx, []*fleetProcess{first, second})
	for _, request := range []<-chan string{firstRequest, secondRequest} {
		if command := <-request; command != "ready\n" {
			t.Fatalf("construction received command %q", command)
		}
	}
	first.accountingResponses <- fleetAccountingResponse{}
	second.accountingResponses <- fleetAccountingResponse{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A later child exiting must fail promptly even while an earlier child has
// no response. Error completion joins every remaining response owner.
func TestFleetReadinessJoinsWaitersWhenAnotherShardExits(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, firstRequest := newFleetReadinessProcess(t, 0)
	second, secondRequest := newFleetReadinessProcess(t, 1)
	done := runFleetReadinessForTest(t, ctx, []*fleetProcess{first, second})
	<-firstRequest
	<-secondRequest
	close(second.done)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "shard 1") {
		t.Fatalf("exited child did not fail construction: %v", err)
	}
	select {
	case first.accountingResponses <- fleetAccountingResponse{}:
		t.Fatal("failed construction retained another shard's waiter")
	default:
	}
}

// An accounting value, parse failure or closed pipe is not a readiness ack.
func TestFleetReadinessRejectsInvalidReplies(t *testing.T) {
	for _, response := range []fleetAccountingResponse{{byteCount: 17}, {err: errors.New("synthetic malformed reply")}} {
		ctx, cancel := context.WithCancel(t.Context())
		proc, request := newFleetReadinessProcess(t, 7)
		done := runFleetReadinessForTest(t, ctx, []*fleetProcess{proc})
		<-request
		proc.accountingResponses <- response
		err := <-done
		cancel()
		if err == nil || !strings.Contains(err.Error(), "shard 7") {
			t.Fatalf("invalid readiness reply accepted: %v", err)
		}
	}
}

// A completed pipe without an acknowledgement fails closed.
func TestFleetReadinessRejectsClosedReplyPipe(t *testing.T) {
	proc, request := newFleetReadinessProcess(t, 2)
	done := runFleetReadinessForTest(t, t.Context(), []*fleetProcess{proc})
	<-request
	close(proc.accountingResponses)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "pipe closed") {
		t.Fatalf("closed readiness pipe accepted: %v", err)
	}
}

// Invalid ownership must fail before any command is written.
func TestFleetReadinessRejectsInvalidOwners(t *testing.T) {
	for _, procs := range [][]*fleetProcess{nil, {nil}, {{}}} {
		if err := waitFleetShardsReady(t.Context(), procs); err == nil {
			t.Fatal("incomplete readiness ownership accepted")
		}
	}
	if err := waitFleetShardsReady(nil, nil); err == nil {
		t.Fatal("nil readiness context accepted")
	}
}

// This wiring assertion complements live pipe tests: Run must cross the new
// barrier before its ramp/prewarm clocks, not merely before measurement.
func TestRunPlacesFleetReadinessBeforeRampAndPrewarm(t *testing.T) {
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "run.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var spawned, ready, ramp, prewarm token.Pos
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "Run" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch name.Name {
			case "spawnFleetShards":
				spawned = call.Pos()
			case "waitFleetShardsReady":
				ready = call.Pos()
			case "waitPhase":
				if len(call.Args) == 2 {
					if argument, ok := call.Args[1].(*ast.Ident); ok && argument.Name == "rampWait" {
						ramp = call.Pos()
					}
				}
			case "provisionPrewarm":
				prewarm = call.Pos()
			}
			return true
		})
	}
	if !(spawned.IsValid() && spawned < ready && ready < ramp && ramp < prewarm) {
		t.Fatalf("fleet construction is not ordered before ramp/prewarm: spawn=%d ready=%d ramp=%d prewarm=%d", spawned, ready, ramp, prewarm)
	}
}
