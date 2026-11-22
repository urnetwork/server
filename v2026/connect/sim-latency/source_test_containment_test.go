// Synthetic daemon observations prove actual containment, not merely requested
// flags, is authenticated before candidate execution and retained on rejection.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// All filesystem paths and identities are synthetic. This is not a live Docker
// attestation; the opt-in real pipeline must exercise the same observer later.
func sourceContainmentInspectFixture(t *testing.T, input sourceBuildSandboxInput, settings sourceBuildSandboxSettings) sourceCorrectnessContainerInspect {
	t.Helper()
	args, err := sourceBuildSandboxArguments(input, settings)
	if err != nil {
		t.Fatal(err)
	}
	environment, tmpfs := []string{"WARP_ENV=local", "WARP_SERVICE=sim", "WARP_BLOCK=sim"}, map[string]string{}
	for _, arg := range args {
		if entry, ok := strings.CutPrefix(arg, "--env="); ok {
			environment = append(environment, entry)
		}
		if entry, ok := strings.CutPrefix(arg, "--tmpfs="); ok {
			path, options, _ := strings.Cut(entry, ":")
			tmpfs[path] = options
		}
	}
	content, err := json.Marshal(map[string]any{
		"Id": strings.Repeat("d", 64), "Image": input.ImageId,
		"Mounts": []map[string]any{
			{"Type": "bind", "Source": input.SourceDirectory, "Destination": "/workspace", "RW": false, "Propagation": "rprivate"},
			{"Type": "bind", "Source": input.MetadataDirectory, "Destination": "/input/server-metadata", "RW": false, "Propagation": "rprivate"},
		},
		"State": map[string]any{"Status": "created", "Running": false},
		"Config": map[string]any{"User": "65532:65532", "Entrypoint": []string{"/bin/bash"}, "Cmd": args[len(args)-2:], "Env": environment,
			"Labels": map[string]string{"com.urnetwork.review-build.owner": input.Owner, "com.urnetwork.review-build.phase": "correctness"}},
		"HostConfig": map[string]any{"Privileged": false, "ReadonlyRootfs": true, "Init": true, "NetworkMode": "none", "CgroupnsMode": "private",
			"CgroupParent": settings.CgroupParent, "CpusetCpus": settings.CpuSet, "Memory": settings.MemoryBytes, "MemorySwap": settings.MemoryBytes, "PidsLimit": settings.Pids,
			"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "IpcMode": "private",
			"Mounts": []map[string]any{
				{"Type": "bind", "Source": input.SourceDirectory, "Target": "/workspace", "ReadOnly": true},
				{"Type": "bind", "Source": input.MetadataDirectory, "Target": "/input/server-metadata", "ReadOnly": true},
			}, "Tmpfs": tmpfs,
			"Ulimits": []map[string]any{{"Name": "core", "Soft": 0, "Hard": 0}, {"Name": "fsize", "Soft": settings.BinaryBytes, "Hard": settings.BinaryBytes}, {"Name": "nofile", "Soft": settings.NoFile, "Hard": settings.NoFile}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var observed sourceCorrectnessContainerInspect
	if err := json.Unmarshal(content, &observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

// Select the fixed correctness phase and its own independent mount leaves.
func sourceContainmentInputFixture() sourceBuildSandboxInput {
	input := sourceSandboxFixture()
	input.Phase = "correctness"
	input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "metadata")
	input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "output")
	return input
}

// A complete positive observation guards against validators that reject every
// document, including Docker's canonical PascalCase property names.
func TestSourceCorrectnessContainmentAcceptsObservedFixedBoundary(t *testing.T) {
	input, settings := sourceContainmentInputFixture(), sourceSandboxTestSettings()
	observed := sourceContainmentInspectFixture(t, input, settings)
	content, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceCorrectnessContainment(content, observed.Id, input, settings); err != nil {
		t.Fatal("complete observed boundary rejected", err)
	}
}

// Each changed daemon fact must fail even though all requested argv are still
// correct. Missing values, duplicate targets and extra mounts fail closed.
func TestSourceCorrectnessContainmentRejectsChangedActualBoundary(t *testing.T) {
	input, settings := sourceContainmentInputFixture(), sourceSandboxTestSettings()
	for _, mutate := range []func(*sourceCorrectnessContainerInspect){
		func(o *sourceCorrectnessContainerInspect) { o.Id = strings.Repeat("e", 64) },
		func(o *sourceCorrectnessContainerInspect) { o.Image = "sha256:" + strings.Repeat("f", 64) },
		func(o *sourceCorrectnessContainerInspect) { o.State.Running = true },
		func(o *sourceCorrectnessContainerInspect) { o.State.Status = "running" },
		func(o *sourceCorrectnessContainerInspect) { o.Config.User = "0:0" },
		func(o *sourceCorrectnessContainerInspect) { o.Config.Entrypoint = []string{"/bin/sh"} },
		func(o *sourceCorrectnessContainerInspect) { o.Config.Cmd = []string{"-c", "true"} },
		func(o *sourceCorrectnessContainerInspect) {
			o.Config.Labels["com.urnetwork.review-build.owner"] = "wrong"
		},
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Privileged = true },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.ReadonlyRootfs = false },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Init = nil },
		func(o *sourceCorrectnessContainerInspect) { *o.HostConfig.Init = false },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.NetworkMode = "host" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.CgroupnsMode = "host" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.CgroupParent = "other.slice" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.CpusetCpus = "0-23" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Memory = 0 },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.MemorySwap = -1 },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.PidsLimit = nil },
		func(o *sourceCorrectnessContainerInspect) { *o.HostConfig.PidsLimit = -1 },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.CapDrop = nil },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.SecurityOpt = nil },
		func(o *sourceCorrectnessContainerInspect) {
			o.HostConfig.Binds = []string{"/synthetic/forbidden:/private:ro"}
		},
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.VolumesFrom = []string{"synthetic-other"} },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.GroupAdd = []string{"0"} },
		func(o *sourceCorrectnessContainerInspect) {
			o.HostConfig.Devices = []json.RawMessage{json.RawMessage(`{}`)}
		},
		func(o *sourceCorrectnessContainerInspect) {
			o.HostConfig.DeviceRequests = []json.RawMessage{json.RawMessage(`{}`)}
		},
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.DeviceCgroupRules = []string{"a *:* rwm"} },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.PidMode = "host" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.IpcMode = "host" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.UsernsMode = "host" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Mounts = o.HostConfig.Mounts[:1] },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Mounts[0].ReadOnly = false },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Mounts[0].Source = "/synthetic/config/main" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Mounts[1] = o.HostConfig.Mounts[0] },
		func(o *sourceCorrectnessContainerInspect) { o.Mounts[0].Rw = true },
		func(o *sourceCorrectnessContainerInspect) { o.Mounts[0].Type = "volume" },
		func(o *sourceCorrectnessContainerInspect) { o.Mounts[0].Source = "/synthetic/vault/main" },
		func(o *sourceCorrectnessContainerInspect) { o.Mounts[0].Propagation = "rshared" },
		func(o *sourceCorrectnessContainerInspect) { o.Mounts[1] = o.Mounts[0] },
		func(o *sourceCorrectnessContainerInspect) { o.Mounts = nil },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Tmpfs["/extra"] = "rw" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Tmpfs["/tmp"] = "rw,exec,size=1" },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Ulimits = nil },
		func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Ulimits[1] = o.HostConfig.Ulimits[0] },
		func(o *sourceCorrectnessContainerInspect) {
			o.Config.Env = append(o.Config.Env, "GOPROXY=https://synthetic.example")
		},
		func(o *sourceCorrectnessContainerInspect) {
			o.Config.Env = []string{"GOPROXY=https://synthetic.example"}
		},
		func(o *sourceCorrectnessContainerInspect) {
			o.Config.Env = append(o.Config.Env, "OPENAI_API_KEY=synthetic-forbidden")
		},
		func(o *sourceCorrectnessContainerInspect) {
			o.Config.Env = append(o.Config.Env, "BRINGYOUR_REDIS_HOSTNAME=cache.example")
		},
		func(o *sourceCorrectnessContainerInspect) { o.Config.Env[0] = "WARP_ENV=main" },
	} {
		observed := sourceContainmentInspectFixture(t, input, settings)
		mutate(&observed)
		content, err := json.Marshal(observed)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSourceCorrectnessContainment(content, strings.Repeat("d", 64), input, settings); err == nil {
			t.Fatal("changed actual containment accepted", string(content))
		}
	}
}

// Explicit daemon callbacks enforce ordering without sleeps: no start before
// a valid retained inspect result; rejection still invokes owner-bound cleanup.
func TestSourceCorrectnessContainmentObservesBeforeStartAndCleansRejectedContainer(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		settings := sourceSandboxTestSettings()
		settings.StateDirectory = t.TempDir()
		input := sourceContainmentInputFixture()
		root := filepath.Join(settings.StateDirectory, input.Owner)
		input.SourceDirectory, input.MetadataDirectory, input.OutputDirectory = filepath.Join(root, "source"), filepath.Join(root, "correctness", "metadata"), filepath.Join(root, "correctness", "output")
		if err := os.MkdirAll(input.OutputDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		observed := sourceContainmentInspectFixture(t, input, settings)
		observed.HostConfig.Privileged = invalid
		started, inspected := false, false
		daemon := &sourceSandboxDaemonFixture{input: input, startOutput: "synthetic test output"}
		underlying := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
			if args[0] == "inspect" && args[2] == "{{json .}}" {
				inspected = true
				return json.NewEncoder(out).Encode(observed)
			}
			if args[0] == "start" {
				if !inspected {
					return errors.New("candidate started before its actual containment was observed")
				}
				if _, err := os.Stat(filepath.Join(root, "correctness", "containment-before-start.json")); err != nil {
					return errors.New("candidate started before its containment observation was retained")
				}
				started = true
			}
			return daemon.run(ctx, args, out, diagnostics)
		}
		wrapped, err := sourceCorrectnessContainmentDocker(input, settings, underlying)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = executeSourceBuildSandbox(context.Background(), input, settings, wrapped)
		if (err != nil) != invalid || !inspected || started == invalid || !daemon.removed {
			t.Fatal("actual-containment admission or cleanup failed", invalid, inspected, started, daemon.removed, err)
		}
		if invalid && !strings.Contains(err.Error(), "observed correctness container lost its isolation") {
			t.Fatal("sandbox hid the deterministic containment root cause from private diagnostics", err)
		}
		content, err := os.ReadFile(filepath.Join(root, "correctness", "containment-before-start.json"))
		if err != nil {
			t.Fatal("failed and successful observations must both be retained", err)
		}
		var observation sourceContainmentObservation
		var retained sourceCorrectnessContainerInspect
		if json.Unmarshal(content, &observation) != nil || json.Unmarshal(observation.Content, &retained) != nil || retained.HostConfig.Privileged != invalid {
			t.Fatal("containment evidence changed the observed daemon facts")
		}
		execution := sourceTestExecution{ImageId: input.ImageId, SuiteSha256: sourceCorrectnessSuiteDigest(), ExitCode: 0, TestCount: 1, TestLogsSha256: strings.Repeat("a", 64)}
		digest, evidenceErr := retainSourceCorrectnessContainment(input, settings, execution)
		if (evidenceErr != nil) != invalid || (!invalid && !validSha256(digest)) {
			t.Fatal("containment artifact did not bind the actual creation, terminal and cleanup observations", invalid, evidenceErr)
		}
		if !invalid {
			replayed, err := retainSourceCorrectnessContainment(input, settings, execution)
			if err != nil || replayed != digest {
				t.Fatal("retained containment replay changed evidence", err)
			}
		}
	}
}

// A caller cannot bypass inspection by asking the wrapper to start directly.
func TestSourceCorrectnessContainmentCannotStartWithoutObservedCreate(t *testing.T) {
	called := false
	wrapped, err := sourceCorrectnessContainmentDocker(sourceContainmentInputFixture(), sourceSandboxTestSettings(), func(context.Context, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapped(context.Background(), []string{"start", "--attach", strings.Repeat("d", 64)}, io.Discard, io.Discard); err == nil || called {
		t.Fatal("unobserved candidate reached the daemon start operation", err, called)
	}
}
