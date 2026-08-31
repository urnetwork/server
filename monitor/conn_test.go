package monitor

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A top-level signal limit is insufficient because individual probes fan out
// internally. Every fresh probe environment must share the transport-level
// budget or their combined SSH handshakes can cross sshd MaxStartups.
func TestSSHCommandsSharePerHostLimitAcrossProbeEnvironments(t *testing.T) {
	settings := SignalSettings{
		SSHUser:     "monitor",
		AddressMode: AddressModeOverlay,
		Hosts: []HostSettings{
			{Name: "db", OverlayAddress: "192.0.2.10"},
			{Name: "edge", OverlayAddress: "192.0.2.11"},
		},
	}.withDefaults().withRuntime()

	newTestRunner := func() (*runner, *monitorConfig) {
		cfg := configFromSignalSettings(settings)
		return newRunner(cfg), cfg
	}
	runnerA, cfgA := newTestRunner()
	runnerB, cfgB := newTestRunner()
	if runnerA.remoteCommands != runnerB.remoteCommands {
		t.Fatal("fresh probe environments did not share their remote-command limiter")
	}

	started := make(chan struct{}, maxConcurrentRemoteCommandsPerHost)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	blockingSSH := func(ctx context.Context, _ []string, _ string) (string, string, error) {
		started <- struct{}{}
		select {
		case <-release:
			return "ok", "", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	runnerA.runSSH = blockingSSH
	runnerB.runSSH = blockingSSH

	var wait sync.WaitGroup
	for i := 0; i < maxConcurrentRemoteCommandsPerHost; i++ {
		selectedRunner := runnerA
		target := cfgA.hosts[0]
		if i%2 == 1 {
			selectedRunner = runnerB
			target = cfgB.hosts[0]
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := selectedRunner.sshTimeout(context.Background(), target, "true", "", time.Minute); err != nil {
				t.Errorf("bounded SSH command failed: %v", err)
			}
		}()
	}
	for i := 0; i < maxConcurrentRemoteCommandsPerHost; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out filling the shared per-host command budget")
		}
	}
	dbSlots := runnerA.remoteCommands.hostSlots(cfgA.hosts[0].overlayIp)
	if got := len(dbSlots); got != maxConcurrentRemoteCommandsPerHost {
		t.Fatalf("occupied db slots = %d, want %d", got, maxConcurrentRemoteCommandsPerHost)
	}

	// A fifth command for the same host waits at the transport boundary and
	// honors cancellation without invoking ssh or consuming a slot.
	blockedRunner, blockedCfg := newTestRunner()
	blockedCalled := make(chan struct{}, 1)
	blockedRunner.runSSH = func(context.Context, []string, string) (string, string, error) {
		blockedCalled <- struct{}{}
		return "", "", nil
	}
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	blockedDone := make(chan error, 1)
	go func() {
		_, err := blockedRunner.sshTimeout(blockedCtx, blockedCfg.hosts[0], "true", "", time.Minute)
		blockedDone <- err
	}()
	cancelBlocked()
	select {
	case err := <-blockedDone:
		if err == nil {
			t.Fatal("canceled command-slot wait returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled command-slot wait did not return")
	}
	select {
	case <-blockedCalled:
		t.Fatal("same-host command crossed a full transport budget")
	default:
	}
	if got := len(dbSlots); got != maxConcurrentRemoteCommandsPerHost {
		t.Fatalf("canceled wait changed occupied db slots to %d", got)
	}

	// Saturating one host does not consume another host's independent budget.
	otherRunner, otherCfg := newTestRunner()
	otherCalled := false
	otherRunner.runSSH = func(context.Context, []string, string) (string, string, error) {
		otherCalled = true
		return "ok", "", nil
	}
	if _, err := otherRunner.sshTimeout(context.Background(), otherCfg.hosts[1], "true", "", time.Minute); err != nil {
		t.Fatalf("other-host SSH command failed: %v", err)
	}
	if !otherCalled {
		t.Fatal("saturated db host serialized an unrelated edge host")
	}

	releaseAll()
	wait.Wait()
	if got := len(dbSlots); got != 0 {
		t.Fatalf("released db slots = %d, want 0", got)
	}
}
