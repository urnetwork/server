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

func TestActiveLogServicesUsesOnlyCurrentServicesVersion(t *testing.T) {
	services := servicesYaml{Versions: []servicesVersionYaml{
		{Services: map[string]any{
			"taskworker": map[string]any{"image": "current"},
			"api":        map[string]any{"image": "current"},
			"grafana":    map[string]any{"image": "current"},
		}},
		{Services: map[string]any{"historical": map[string]any{"image": "old"}}},
	}}
	got, err := activeLogServicesFromServices(services)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api", "grafana", "taskworker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active log services = %#v, want %#v", got, want)
	}
}

func TestWarpServicesPrefersConfiguredActiveInventory(t *testing.T) {
	source := &syntheticSource{localFn: func(string, ...string) (string, error) {
		return "", errors.New("artifact registry must not be queried")
	}}
	settings := syntheticSettings(source)
	settings.LogServices = []string{"taskworker", "api", "grafana"}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}

	got := warpServices(context.Background(), env)
	want := []string{"api", "grafana", "taskworker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("configured services = %#v, want %#v", got, want)
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
