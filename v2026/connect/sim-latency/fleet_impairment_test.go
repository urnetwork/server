package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/docopt/docopt-go"
)

// Uses the actual executable grammar, not a smaller test-only parser. A fleet
// subprocess must be able to represent the same mode as its run parent.
func TestFleetCliAcceptsNoImpairSwitch(t *testing.T) {
	args := []string{"fleet", "--epoch=1", "--providers=synthetic-providers.yml", "--shard=0/4",
		"--api-url=https://api.example", "--ws-urls=wss://exchange.example", "--no-impair"}
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	opts, err := parser.ParseArgs(simulationUsage(), args, "synthetic-version")
	if err != nil || !optBool(opts, "--no-impair") {
		t.Fatalf("fleet cannot receive the parent's disabled impairment mode: %v", err)
	}
}

// Uses an independently initialized copy of the Go test executable to exercise
// the real child grammar/settings decoder, without starting services or dialing
// any supplied URL. The parent uses the production command builder unchanged.
func TestFleetImpairmentChildProcess(t *testing.T) {
	for index, argument := range os.Args {
		if argument != "synthetic-fleet-impairment-child" {
			continue
		}
		if len(os.Args) <= index+2 {
			t.Fatal("missing synthetic output path and fleet command")
		}
		parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
		opts, err := parser.ParseArgs(simulationUsage(), os.Args[index+2:], "synthetic-version")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		fleet, err := NewFleet(ctx, &Config{}, nil, optString(opts, "--api-url", ""), nil, nil, 0, *fleetSettingsFromOpts(opts))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := fleet.Wait(); err != nil {
			t.Fatal(err)
		}
		content, err := json.Marshal(struct {
			Enabled bool   `json:"enabled"`
			Shard   string `json:"shard"`
		}{Enabled: fleet.settings.ImpairmentEnabled, Shard: optString(opts, "--shard", "")})
		if err != nil || os.WriteFile(os.Args[index+1], content, 0o600) != nil {
			t.Fatal("synthetic child could not retain its decoded settings", err)
		}
		os.Exit(0)
	}
}

func TestFleetImpairmentSurvivesEveryShardProcessBoundary(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	services := &Services{apiUrl: "https://api.example", wsUrls: []string{"wss://exchange.example"}}
	root := t.TempDir()
	for _, enabled := range []bool{true, false} {
		settings := DefaultFleetSettings()
		settings.ImpairmentEnabled = enabled
		options := &RunOptions{Epoch: 1, SourceConfig: "synthetic-source.yml", RepositoriesRoot: root,
			ConfigPath: "synthetic-providers.yml", FleetShards: 4, FleetSettings: &settings, EvaluationId: "synthetic-evaluation"}
		for shard := 0; shard < options.FleetShards; shard++ {
			path := filepath.Join(root, fmt.Sprintf("mode-%t-shard-%d.json", enabled, shard))
			arguments := []string{"-test.run=^TestFleetImpairmentChildProcess$", "--", "synthetic-fleet-impairment-child", path}
			arguments = append(arguments, fleetProcessArgs(options, services, shard)...)
			if output, err := exec.Command(executable, arguments...).CombinedOutput(); err != nil {
				t.Fatalf("fleet mode %t shard %d failed: %v\n%s", enabled, shard, err, output)
			}
			content, err := os.ReadFile(path)
			var observed struct {
				Enabled bool   `json:"enabled"`
				Shard   string `json:"shard"`
			}
			if err != nil || json.Unmarshal(content, &observed) != nil || observed.Enabled != enabled || observed.Shard != fmt.Sprintf("%d/4", shard) {
				t.Fatalf("fleet mode %t shard %d was lost in the real subprocess: %s %v", enabled, shard, content, err)
			}
		}
	}
}

func TestFleetImpairmentOwnersHaveIndependentSettingsAndLifetimes(t *testing.T) {
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	disabled := DefaultFleetSettings()
	disabled.ImpairmentEnabled = false
	first, err := NewFleet(firstCtx, &Config{}, nil, "https://api.example", nil, nil, 0, disabled)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFleet(secondCtx, &Config{}, nil, "https://api.example", nil, nil, 0, DefaultFleetSettings())
	if err != nil {
		t.Fatal(err)
	}
	firstCancel()
	if err := first.Wait(); err != nil || first.settings.ImpairmentEnabled || !second.settings.ImpairmentEnabled || second.ctx.Err() != nil {
		t.Fatal("disabling or retiring one fleet changed its healthy sibling", err)
	}
	secondCancel()
	if err := second.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestFleetImpairmentBaselineRunChildRoundTrip(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	opts, err := parser.ParseArgs(simulationUsage(), []string{"baseline", "--epoch=1", "--replicates=2", "--no-impair"}, "synthetic-version")
	if err != nil {
		t.Fatal(err)
	}
	run, err := parser.ParseArgs(simulationUsage(), replicateRunArgs(opts, "synthetic-run.json"), "synthetic-version")
	if err != nil || fleetSettingsFromOpts(run).ImpairmentEnabled {
		t.Fatal("baseline replicate lost its disabled mode before shard creation", err)
	}
	options := &RunOptions{Epoch: 1, SourceConfig: "synthetic-source.yml", RepositoriesRoot: "synthetic-repos",
		ConfigPath: "synthetic-providers.yml", FleetShards: 4, FleetSettings: fleetSettingsFromOpts(run), EvaluationId: "synthetic-evaluation"}
	child, err := parser.ParseArgs(simulationUsage(), fleetProcessArgs(options, &Services{apiUrl: "https://api.example", wsUrls: []string{"wss://exchange.example"}}, 0), "synthetic-version")
	if err != nil || fleetSettingsFromOpts(child).ImpairmentEnabled || !(&RunOptions{}).fleetSettings().ImpairmentEnabled {
		t.Fatal("baseline/run/fleet disagree on explicit mode or the enabled default", err)
	}
}
