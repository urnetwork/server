//go:build linux && keepaliveintegration

// Exercises real Linux TCP loss in an explicitly isolated network namespace.
// Build/run instructions are in CPUOPT-KEEPALIVE.md; ordinary suites exclude it.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// With no outstanding application data, the kernel can use idle probes.
func TestExchangeTcpBlackholeIdleProbes(t *testing.T) {
	t.Parallel()
	settings := DefaultExchangeSettings()
	exerciseExchangeTcpBlackhole(t, settings, exchangeKeepAliveConfig(settings), false)
}

// A buffered ping leaves unacknowledged data, so the application read deadline
// must remain the independent liveness bound even while writes succeed.
func TestExchangeTcpBlackholeWithHeartbeats(t *testing.T) {
	t.Parallel()
	settings := DefaultExchangeSettings()
	exerciseExchangeTcpBlackhole(t, settings, exchangeKeepAliveConfig(settings), true)
}

// The former aggressive probe configuration also depended on the read timeout
// under the same loss. Probe arithmetic alone never established a 16s bound.
func TestExchangeTcpBlackholeFormerProbesWithHeartbeats(t *testing.T) {
	t.Parallel()
	settings := DefaultExchangeSettings()
	former := net.KeepAliveConfig{Enable: true, Idle: time.Second, Interval: time.Second, Count: 15}
	exerciseExchangeTcpBlackhole(t, settings, former, true)
}

// Loss begins only after the firewall command completes. Each case owns its
// exact four-tuple and removes only its own rule, including on assertion failure.
func exerciseExchangeTcpBlackhole(t *testing.T, settings *ExchangeSettings, keepAlive net.KeepAliveConfig, heartbeat bool) {
	t.Helper()
	if os.Getenv("EXCHANGE_TEST_ISOLATED_NETWORK") != "1" {
		t.Skip("requires EXCHANGE_TEST_ISOLATED_NETWORK=1 inside a disposable isolated network namespace with NET_ADMIN")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	local, err := (&net.Dialer{KeepAliveConfig: keepAlive}).DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	remote, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.SetKeepAlive(false); err != nil {
		t.Fatal(err)
	}

	sender := NewDefaultExchangeBuffer(settings)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	// Exchange complete framed pings in both directions before introducing loss.
	if err := sender.WriteMessage(local, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(remote, make([]byte, exchangeIOFrameHeaderByteCount)); err != nil {
		t.Fatal(err)
	}
	if err := NewDefaultExchangeBuffer(settings).WriteMessage(remote, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.ReadMessage(local); err != nil {
		t.Fatal(err)
	}

	rule := []string{"INPUT", "-p", "tcp", "-s", "127.0.0.1", "-d", "127.0.0.1",
		"--sport", strconv.Itoa(remote.LocalAddr().(*net.TCPAddr).Port),
		"--dport", strconv.Itoa(local.LocalAddr().(*net.TCPAddr).Port), "-j", "DROP"}
	changeRule := func(operation string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := append([]string{"-w", operation}, rule...)
		output, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
		if err != nil {
			t.Logf("iptables %s: %s", operation, output)
		}
		return err
	}
	if err := changeRule("-A"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := changeRule("-D"); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	stop := make(chan struct{})
	writerDone := make(chan int, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, _ = io.Copy(io.Discard, remote)
	}()
	go func() {
		written := 0
		defer func() { writerDone <- written }()
		if !heartbeat {
			return
		}
		timer := time.NewTicker(settings.ExchangePingTimeout)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				if err := sender.WriteMessage(local, nil); err != nil {
					return
				}
				written++
			}
		}
	}()
	_, readErr := receiver.ReadMessage(local)
	elapsed := time.Since(start)
	close(stop)
	local.Close()
	remote.Close()
	written := <-writerDone
	<-readerDone
	t.Logf("heartbeat=%t probes=%+v writes=%d elapsed=%s error=%v", heartbeat, keepAlive, written, elapsed, readErr)
	if heartbeat {
		if written < 2 || !errors.Is(readErr, os.ErrDeadlineExceeded) {
			t.Fatalf("active blackhole must reach the application timeout with successful pings: writes=%d error=%v", written, readErr)
		}
		if elapsed < settings.ExchangeReadTimeout-time.Second || settings.ExchangeReadTimeout+3*time.Second < elapsed {
			t.Fatalf("application timeout after %s, want %s", elapsed, settings.ExchangeReadTimeout)
		}
	} else {
		nominal := keepAlive.Idle + time.Duration(keepAlive.Count)*keepAlive.Interval
		if readErr == nil || errors.Is(readErr, os.ErrDeadlineExceeded) || elapsed < nominal-time.Second || nominal+3*time.Second < elapsed {
			t.Fatalf("idle probes returned %v after %s, expected kernel timeout near %s", readErr, elapsed, nominal)
		}
	}
}
