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
		{Services: map[string]servicesServiceYaml{
			"taskworker": {Blocks: []map[string]int{{"g2": 25}, {"g1": 74}, {"g1": 1}}},
			"api":        {Blocks: []map[string]int{{"g1": 99}, {"beta": 1}}},
			"grafana":    {Blocks: []map[string]int{{"g1": 100}}},
			"lb":         {Blocks: []map[string]int{{"edge": 100}}},
		}},
		{Services: map[string]servicesServiceYaml{
			"historical": {Blocks: []map[string]int{{"old": 100}}},
		}},
	}}
	got, err := activeLogServicesFromServices(services)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api", "grafana", "taskworker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active log services = %#v, want %#v", got, want)
	}

	gotBlocks, err := activeLogServiceBlocksFromServices(services)
	if err != nil {
		t.Fatal(err)
	}
	wantBlocks := map[string][]string{
		"api":        {"beta", "g1"},
		"grafana":    {"g1"},
		"taskworker": {"g1", "g2"},
	}
	if !reflect.DeepEqual(gotBlocks, wantBlocks) {
		t.Fatalf("active log blocks = %#v, want %#v", gotBlocks, wantBlocks)
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
