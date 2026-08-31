package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWarpServicesParsesOnlyRepositoryLogLine(t *testing.T) {
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "warpctl" || strings.Join(args, " ") != "ls services synthetic" {
			return "", errors.New("unexpected discovery command")
		}
		return "2026/08/31 docker.go:393: Found repo names synthetic-api, other-web, synthetic-grafana\n" +
			"synthetic-api (100.0 2026.8.31)\n" +
			"synthetic-grafana (100.0 2026.8.31)\n", nil
	}}
	env, err := newProbeEnv(syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}

	got := warpServices(context.Background(), env)
	want := []string{"api", "grafana"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("services = %#v, want %#v", got, want)
	}
}

func TestWarpServicesRejectsPartialOutputFromFailedDiscovery(t *testing.T) {
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "warpctl" || strings.Join(args, " ") != "ls services synthetic" {
			return "", errors.New("unexpected discovery command")
		}
		return "Found repo names synthetic-api, synthetic-grafana\n" +
			"panic: invalid character 'e' looking for beginning of value\n", errors.New("exit status 2")
	}}
	env, err := newProbeEnv(syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}

	got := warpServices(context.Background(), env)
	want := []string{"api", "connect", "taskworker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("services from failed partial discovery = %#v, want fallback %#v", got, want)
	}
}
