//go:build acklineagetrace

package connect

import (
	"context"
	"errors"
	"hash/crc64"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestH1RelayLineageScalarOwnership(t *testing.T) {
	wire := []byte("synthetic borrowed transfer frame")
	if n := testing.AllocsPerRun(100, func() {
		beginH1RelayLineage("test", server.Id{}, server.Id{}, wire, 0, 0).end("end", true, 0, 0)
	}); n != 0 {
		t.Fatalf("nil trace allocations=%g", n)
	}
	var events []H1RelayLineageEvent
	cleanup, ok := InstallH1RelayLineageObserver(func(event H1RelayLineageEvent) { events = append(events, event) })
	if !ok {
		t.Fatal("observer already owned")
	}
	defer cleanup()
	if _, ok := InstallH1RelayLineageObserver(func(H1RelayLineageEvent) {}); ok {
		t.Fatal("conflicting observer admitted")
	}
	client, peer := server.NewId(), server.NewId()
	wantHash := crc64.Checksum(wire, h1RelayLineageChecksum)
	span := beginH1RelayLineage("read", client, peer, wire, 1, 8)
	clear(wire)
	span.end("admit", true, 0, 8)
	if len(events) != 2 || events[0].WireHash != wantHash || events[1].WireHash != wantHash ||
		events[0].Bytes != len(wire) || events[1].Client != client || events[1].Peer != peer ||
		events[0].QueueLength != 1 || events[1].QueueLength != 0 || events[1].AtNS < events[0].AtNS || !events[1].Success {
		t.Fatalf("scalar owner mapping changed: %+v", events)
	}
	cleanup()
	span.end("retired", true, 0, 0)
	if len(events) != 2 {
		t.Fatal("retired span published after cleanup")
	}
	if next, ok := InstallH1RelayLineageObserver(func(H1RelayLineageEvent) { panic("contained") }); !ok {
		t.Fatal("observer not released")
	} else {
		defer next()
		cleanup()
		beginH1RelayLineage("panic", client, peer, nil, 0, 0)
	}
}

func TestH1RelayLineageFlushBoundary(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "error"}[fail], func(t *testing.T) {
			var lock sync.Mutex
			var events []H1RelayLineageEvent
			cleanup, ok := InstallH1RelayLineageObserver(func(event H1RelayLineageEvent) {
				lock.Lock()
				defer lock.Unlock()
				events = append(events, event)
			})
			if !ok {
				t.Fatal("observer already owned")
			}
			defer cleanup()
			boundary := &connectH1BatchTestBoundary{flushEntered: make(chan struct{}), flushRelease: make(chan struct{})}
			if fail {
				boundary.flushErr = errors.New("synthetic flush failure")
			}
			ready := make(chan []byte, 1)
			ready <- newConnectH1BatchTestMessage(73)
			finished := make(chan error, 1)
			go func() {
				_, err := writeConnectH1UserReadyBatch(context.Background(), &connectH1BatchTestWriter{}, boundary,
					ready, newConnectH1BatchTestMessage(72), true, time.Second, nil)
				finished <- err
			}()
			<-boundary.flushEntered
			lock.Lock()
			if len(events) != 2 || events[0].Stage != "edge_h1_write_begin" || events[1].Stage != "edge_h1_write_begin" {
				t.Errorf("queue admission was mistaken for physical completion: %+v", events)
			}
			lock.Unlock()
			close(boundary.flushRelease)
			err := <-finished
			cleanup()
			if (err != nil) != fail || len(events) != 4 {
				t.Fatalf("terminal flush evidence changed: err=%v events=%d", err, len(events))
			}
			for index := range 2 {
				begin, end := events[index], events[index+2]
				if end.Stage != "edge_h1_write_end" || end.Success == fail || begin.WireHash != end.WireHash || end.AtNS < begin.AtNS {
					t.Fatalf("flush result lost exact message: begin=%+v end=%+v", begin, end)
				}
			}
		})
	}
}
