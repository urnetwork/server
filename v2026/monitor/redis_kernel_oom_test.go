// Kernel OOM controls execute the signal's real shell selector with hermetic
// process/kernel sources, then inspect its public alert reduction.
package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No command can fall through to real ps, dmesg, SSH, or an inherited shell
// environment. The old and corrected selectors use the same fixture boundary.
func redisKernelOomNativeFixture(t *testing.T, kernelText, processText string) (SignalSettings, *string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"awk", "grep", "tail", "sed"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("native selector fixture requires %s", name)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{
		"ps":    "#!/bin/sh\nprintf 'ps\\n' >> \"$MONITOR_TEST_REDIS_CALLS\"\nprintf '%s' \"$MONITOR_TEST_REDIS_PROCESSES\"\n",
		"dmesg": "#!/bin/sh\nprintf 'dmesg\\n' >> \"$MONITOR_TEST_REDIS_CALLS\"\nprintf '%s' \"$MONITOR_TEST_REDIS_KERNEL\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	output := ""
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		callPath := filepath.Join(dir, "source-calls")
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Env = []string{
			"PATH=" + dir,
			"LC_ALL=C",
			"MONITOR_TEST_REDIS_CALLS=" + callPath,
			"MONITOR_TEST_REDIS_KERNEL=" + kernelText,
			"MONITOR_TEST_REDIS_PROCESSES=" + processText,
		}
		bytes, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("hermetic Redis selector command failed: %w", err)
		}
		calls, err := os.ReadFile(callPath)
		if err != nil || string(calls) != "ps\ndmesg\n" {
			t.Fatal("signal did not execute exactly one fake ps and one fake dmesg")
		}
		output = string(bytes)
		return output, nil
	}}
	return syntheticSettings(source), &output
}

// Unrelated victims must not acquire a Redis incident or an unknown result
// merely because their native kernel messages include generic OOM words.
func redisKernelOomRequireHealthy(t *testing.T, settings SignalSettings) {
	t.Helper()
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("unrelated or absent OOM victim produced %d alert(s)", len(alerts))
	}
}

// A victim record preserves the existing PAGE, host identity and first-sample
// threshold, while avoiding unsupported incident-role or memory-cause claims.
func redisKernelOomRequirePage(t *testing.T, settings SignalSettings) Alert {
	t.Helper()
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "redis-kernel-oom")
	if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 1 || alert.Target != "redis-1" || alert.SignalNumber != "3.4" {
		t.Fatal("explicit Redis victim lost its PAGE identity or threshold")
	}
	if alert.Evidence != "victim_comm=redis-server; source=kernel-oom-victim-record" {
		t.Fatal("Redis victim evidence was not reduced to its bounded attribution")
	}
	requireAlertOmits(t, alert,
		"Aggregate Redis allocation exceeded physical memory",
		"killed serving master leaves a shard unavailable",
	)
	return alert
}

// Ambiguous victim text is an observation gap, never Redis fault or health.
func redisKernelOomRequireUnknown(t *testing.T, settings SignalSettings) {
	t.Helper()
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if len(alerts) != 1 || unknown.Target != "redis-1/redis-kernel" || !unknown.unobservable {
		t.Fatal("ambiguous kernel victim did not preserve the observation gap")
	}
	requireAlertOmits(t, unknown, "synthetic-private-context")
}

// This is the causal RED: the original generic grep emits this worker kill,
// and its unconditional reducer falsely publishes a Redis PAGE.
func TestRedisProcessSignalKernelOtherVictimIsHealthy(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"[Fri Oct 16 12:00:00 2026] Out of memory: Killed process 31415 (worker) total-vm:9000kB\n", "")
	redisKernelOomRequireHealthy(t, settings)
}

// The allocating task's name is not the victim, even within the same event.
func TestRedisProcessSignalKernelRedisInvokerIsNotVictim(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"[Fri Oct 16 12:00:00 2026] redis-server invoked oom-killer: gfp_mask=0x0\n"+
			"[Fri Oct 16 12:00:00 2026] oom-kill:constraint=CONSTRAINT_NONE,global_oom,task=worker,pid=31415,uid=1000\n"+
			"[Fri Oct 16 12:00:00 2026] Out of memory: Killed process 31415 (worker) total-vm:9000kB\n", "")
	redisKernelOomRequireHealthy(t, settings)
}

// A Redis-looking cgroup, including a task-shaped substring, cannot override
// the terminal native victim fields.
func TestRedisProcessSignalKernelRedisContextIsNotVictim(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"oom-kill:constraint=CONSTRAINT_MEMCG,oom_memcg=/synthetic/redis-server,task=redis-server,pid=9,uid=0/path,task_memcg=/synthetic/redis-server,task=worker,pid=31415,uid=1000\n", "")
	redisKernelOomRequireHealthy(t, settings)
}

// The actual kill sentence still produces the existing Redis incident.
func TestRedisProcessSignalKernelRedisKilledVictimPages(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"[Fri Oct 16 12:00:00 2026] Out of memory: Killed process 31415 (redis-server) total-vm:9000kB\n", "")
	redisKernelOomRequirePage(t, settings)
}

// Linux's task/pid/uid suffix names the selected victim independently of
// whether a subsequent kill sentence survives in the ring buffer.
func TestRedisProcessSignalKernelRedisTaskVictimPages(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"oom-kill:constraint=CONSTRAINT_NONE,global_oom,task=redis-server,pid=31415,uid=1000\n", "")
	redisKernelOomRequirePage(t, settings)
}

// A memory-cgroup kill does not prove whole-host exhaustion or a master role.
func TestRedisProcessSignalKernelRedisMemcgVictimPages(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"Memory cgroup out of memory: Killed process 31415 (redis-server) total-vm:9000kB\n", "")
	redisKernelOomRequirePage(t, settings)
}

// A later unrelated OOM must not displace a confirmed Redis victim.
func TestRedisProcessSignalKernelLaterOtherVictimKeepsRedis(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"Out of memory: Killed process 31415 (redis-server) total-vm:9000kB\n"+
			"Out of memory: Killed process 27182 (worker) total-vm:9000kB\n", "")
	redisKernelOomRequirePage(t, settings)
}

// A truncated victim name is not positive Redis attribution or a healthy zero.
func TestRedisProcessSignalKernelMalformedVictimIsUnknown(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"Out of memory: Killed process 31415 (redis-server synthetic-private-context\n", "")
	redisKernelOomRequireUnknown(t, settings)
}

// Invocation without any selected victim does not demonstrate a Redis kill.
func TestRedisProcessSignalKernelInvokerOnlyIsUnknown(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"redis-server invoked oom-killer: synthetic-private-context\n", "")
	redisKernelOomRequireUnknown(t, settings)
}

// A later complete unrelated event cannot repair an earlier unresolved one.
func TestRedisProcessSignalKernelUnresolvedEarlierEventIsUnknown(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"redis-server invoked oom-killer: gfp_mask=0x0\n"+
			"worker invoked oom-killer: gfp_mask=0x0\n"+
			"Out of memory: Killed process 31415 (worker) total-vm:9000kB\n", "")
	redisKernelOomRequireUnknown(t, settings)
}

// Successful ordinary kernel text remains a healthy control.
func TestRedisProcessSignalKernelNoOomIsHealthy(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t, "kernel: ordinary synthetic boot message\n", "")
	redisKernelOomRequireHealthy(t, settings)
}

// Neither the selector result nor public alerts retain private kernel context.
func TestRedisProcessSignalKernelVictimEvidenceIsRedacted(t *testing.T) {
	settings, output := redisKernelOomNativeFixture(t,
		"oom-kill:constraint=CONSTRAINT_MEMCG,oom_memcg=/synthetic-private-context,task_memcg=/synthetic-private-context,task=redis-server,pid=31415,uid=1000\n"+
			"Memory cgroup out of memory: Killed process 31415 (redis-server) total-vm:9000kB private=synthetic-private-context\n", "")
	alert := redisKernelOomRequirePage(t, settings)
	for _, forbidden := range []string{"synthetic-private-context", "31415", "1000", "9000"} {
		if strings.Contains(*output, forbidden) {
			t.Fatal("native selector retained private kernel context")
		}
		requireAlertOmits(t, alert, forbidden)
	}
}

// Existing raw-row sources also pass the victim check at the reducer boundary.
func TestRedisProcessSignalKernelRawOtherVictimIsHealthy(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "kernel_oom Out of memory: Killed process 31415 (worker)\nkernel_source complete\n", nil
	}}
	redisKernelOomRequireHealthy(t, syntheticSettings(source))
}

// Unsupported source text must not turn into the old unconditional Redis PAGE.
func TestRedisProcessSignalKernelRawUnknownVictimIsUnknown(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "kernel_oom victim-unknown synthetic-private-context\nkernel_source complete\n", nil
	}}
	redisKernelOomRequireUnknown(t, syntheticSettings(source))
}

// A retained event is still relevant, but its observation time is not its
// occurrence time and does not prove a current unavailable shard.
func TestRedisProcessSignalKernelRetainedVictimKeepsHistoryQualifier(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"[Sat Jan 1 00:00:00 2000] Out of memory: Killed process 31415 (redis-server) total-vm:9000kB\n", "")
	alert := redisKernelOomRequirePage(t, settings)
	if !strings.Contains(alert.Context, "may predate this observation") {
		t.Fatal("retained kernel event lost its occurrence-time qualifier")
	}
}

// Ambiguous kernel attribution must preserve an independent process finding.
func TestRedisProcessSignalKernelUnknownPreservesCpuFinding(t *testing.T) {
	settings, _ := redisKernelOomNativeFixture(t,
		"Out of memory: Killed process synthetic-private-context\n",
		"1234 250.5 9000000 4000000 redis-server\n")
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("expected CPU and unknown findings, got %d", len(alerts))
	}
	requireAlertClass(t, alerts, "redis-cpu-sustained")
	requireAlertClass(t, alerts, "cannot-observe")
}

// Transport absence stays an execution error for the single-host adapter;
// source-status authority for successful transports is covered separately.
func TestRedisProcessSignalKernelTransportUnavailableIsNotHealthy(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "", fmt.Errorf("synthetic transport unavailable")
	}}
	alerts, err := NewRedisProcessSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || len(alerts) != 0 {
		t.Fatal("unavailable source became a successful observation")
	}
}
