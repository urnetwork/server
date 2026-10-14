package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func arinScheduleClient(t *testing.T, host string) *server.ArinShadowRPCClient {
	t.Helper()
	client, err := server.NewArinShadowRPCClient(server.NewId(), [32]byte{1}, server.ArinShadowRPCIdentity{
		ProcessNonce: server.NewId(), StartedAt: time.Now(), Revision: strings.Repeat("a", 40), Host: host,
	}, func(context.Context, []byte) ([]byte, error) { return nil, server.ErrArinShadowInput })
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestArinCaptureBlockedHostLeavesSecondPipeUsable(t *testing.T) {
	first, second := arinScheduleClient(t, "host-0"), arinScheduleClient(t, "host-1")
	jobs := make([]arinShadowCaptureJob, 9)
	for i := range 8 {
		jobs[i].client = first
	}
	jobs[8].client = second
	blocked, progress, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var pipe sync.Mutex
	go func() {
		defer close(finished)
		runArinShadowCaptureJobs(jobs, func(i int) {
			if jobs[i].client == first {
				// A host has one serial framed SSH pipe, even when several
				// distinct process clients issue work to it.
				pipe.Lock()
				defer pipe.Unlock()
				<-blocked
			} else {
				once.Do(func() { close(progress) })
			}
		})
	}()
	t.Cleanup(func() { close(blocked); <-finished })
	select {
	case <-progress:
	case <-time.After(time.Second):
		t.Fatal("four workers queued behind one host while the second pipe was idle")
	}
}

func TestArinCaptureHostPairsAreBoundedJoinedAndExact(t *testing.T) {
	var jobs []arinShadowCaptureJob
	for _, host := range []string{"host-0", "host-1", "host-2", "host-3"} {
		for range 5 {
			jobs = append(jobs, arinShadowCaptureJob{client: arinScheduleClient(t, host)})
		}
	}
	var mu sync.Mutex
	active, peak := map[string]int{}, 0
	seen := make([]bool, len(jobs))
	runArinShadowCaptureJobs(jobs, func(i int) {
		host := jobs[i].client.Identity().Host
		mu.Lock()
		if seen[i] {
			t.Error("job repeated")
		}
		seen[i] = true
		active[host]++
		if active[host] != 1 {
			t.Error("multiple workers queued for one serial host")
		}
		if len(active) > peak {
			peak = len(active)
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		active[host]--
		if active[host] == 0 {
			delete(active, host)
		}
		mu.Unlock()
	})
	if peak > 2 || len(active) != 0 {
		t.Fatal("host worker budget or join failed", peak, len(active))
	}
	for _, value := range seen {
		if !value {
			t.Fatal("job disappeared")
		}
	}
}
