package monitor

import (
	"context"
	"testing"
)

func TestRedisProcessSignalSyntheticRedisCPU(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "redis 1234 250.5 9000000 4000000", nil
	}}
	alerts, err := NewRedisProcessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "redis-cpu-sustained")
}

func TestRedisProcessSignalSyntheticKernelOOM(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "kernel_oom Out of memory: Killed process 1234 (redis-server)", nil
	}}
	alerts, err := NewRedisProcessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "redis-kernel-oom")
}
