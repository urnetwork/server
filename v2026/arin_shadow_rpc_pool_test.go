package server

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestArinRPCPipeHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--arin-pipe-test" {
			if len(os.Args) != i+2 {
				os.Exit(3)
			}
			delay, err := strconv.Atoi(os.Args[i+1])
			if err != nil {
				os.Exit(4)
			}
			for {
				packet, err := ReadArinShadowRPCFrame(os.Stdin, ArinShadowRPCRequestLimit)
				if err != nil {
					os.Exit(0)
				}
				time.Sleep(time.Duration(delay) * time.Millisecond)
				if WriteArinShadowRPCFrame(os.Stdout, packet, ArinShadowRPCResponseLimit) != nil {
					os.Exit(5)
				}
			}
		}
	}
}

func TestArinRPCPipePoolTwoSessionsCancellationAndNoFailedHostRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := func(delay string) []string {
		return []string{os.Args[0], "-test.run=^TestArinRPCPipeHelper$", "--", "--arin-pipe-test", delay}
	}
	p, err := NewArinShadowRPCPipePool(ctx, map[string][]string{"a": command("40"), "b": command("40"), "c": command("40"), "stalled": command("4000")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	packet := bytes.Repeat([]byte{42}, 64)
	var wg sync.WaitGroup
	for _, host := range []string{"a", "b", "c", "a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, err := p.RoundTrip(host)(ctx, packet)
			if err != nil || !bytes.Equal(reply, packet) {
				t.Error("healthy host did not progress")
			}
		}()
	}
	wg.Wait()
	started, peak := p.Counts()
	if started < 3 || peak != 2 {
		t.Fatal("two-session pool boundary", started, peak)
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, err := p.RoundTrip("stalled")(short, packet); err == nil {
		t.Fatal("stalled pipe ignored cancellation")
	}
	started, _ = p.Counts()
	if _, err := p.RoundTrip("stalled")(ctx, packet); err == nil {
		t.Fatal("failed host silently retried")
	}
	after, _ := p.Counts()
	if after != started {
		t.Fatal("failed host spawned again")
	}
	if _, err := p.RoundTrip("a")(ctx, packet); err != nil {
		t.Fatal("canceled host blocked independent progress")
	}
	p.Close()
	if _, err := p.RoundTrip("a")(ctx, packet); err == nil {
		t.Fatal("closed pool admitted work")
	}
}
