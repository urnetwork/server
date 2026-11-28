// Deterministic daemon seams test containment and cleanup without starting a
// container or claiming mocked observations as genuine runtime qualification.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Changes umask only in an isolated process, proving scorer traversal without
// changing the test runner's process-global permissions or relaxing its parent.
func TestCalibrationReplayCorpusPermissionsSurvivePrivateUmask(t *testing.T) {
	const flag = "SIM_LATENCY_TEST_PRIVATE_REPLAY_UMASK"
	if os.Getenv(flag) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(executable, "-test.run=^TestCalibrationReplayCorpusPermissionsSurvivePrivateUmask$")
		command.Env = append(os.Environ(), flag+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("private umask regression: %v\n%s", err, output)
		}
		return
	}
	oldMask := syscall.Umask(0077)
	defer syscall.Umask(oldMask)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "sample"), []byte("synthetic sample"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "copy")
	if err := copyCalibrationReplayCorpus(context.Background(), source, destination, defaultCalibrationAnalysisSettings()); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		root: 0700, destination: 0755, filepath.Join(destination, "nested"): 0755,
		filepath.Join(destination, "nested", "sample"): 0444,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("copied corpus permissions: got %04o, want %04o", info.Mode().Perm(), mode)
		}
	}
}

// Single-threaded fake daemon; cancellation occurs at the exact attach boundary.
type calibrationReplayDaemonFixture struct {
	settings         calibrationReplaySettings
	id               string
	owner            string
	image            string
	corpus           string
	command          []string
	createArgs       []string
	started          bool
	removed          bool
	lostCreate       bool
	foreignOwner     bool
	failRemove       bool
	cancel           context.CancelFunc
	mutateInspection func(map[string]any)
	output           string
}

// Returns the actual Docker protocol shapes, while independently enforcing that
// cleanup retains a live context after the parent has been canceled.
func (self *calibrationReplayDaemonFixture) run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	write := func(value any) error { return json.NewEncoder(stdout).Encode(value) }
	switch args[0] {
	case "create":
		self.createArgs = append([]string(nil), args...)
		for i, arg := range args {
			if strings.HasPrefix(arg, "--label=com.urnetwork.calibration.owner=") {
				self.owner = strings.TrimPrefix(arg, "--label=com.urnetwork.calibration.owner=")
			}
			if strings.HasPrefix(arg, "--mount=type=bind,source=") {
				self.corpus = strings.TrimSuffix(strings.TrimPrefix(arg, "--mount=type=bind,source="), ",target=/artifacts,readonly")
			}
			if arg == "--entrypoint=/opt/urnetwork/bin/sim-latency" {
				self.image, self.command = args[i+1], append([]string(nil), args[i+2:]...)
			}
		}
		if self.lostCreate {
			return errors.New("synthetic lost create acknowledgment")
		}
		_, err := io.WriteString(stdout, self.id+"\n")
		return err
	case "inspect":
		if len(args) == 4 && args[1] == "--format" {
			switch args[2] {
			case "{{.Id}}":
				_, err := io.WriteString(stdout, self.id+"\n")
				return err
			case "{{json .Config.Labels}}":
				owner := self.owner
				if self.foreignOwner {
					owner = strings.Repeat("f", 64)
				}
				return write(map[string]string{"com.urnetwork.calibration.owner": owner})
			case "{{json .State}}":
				return write(map[string]any{"Status": "exited", "Running": false, "ExitCode": 0, "OOMKilled": false})
			}
		}
		inspection := map[string]any{
			"Id": self.id, "Image": self.image,
			"Config": map[string]any{"User": "65532:65532", "Entrypoint": []string{"/opt/urnetwork/bin/sim-latency"}, "Cmd": self.command,
				"Labels": map[string]string{"com.urnetwork.calibration.owner": self.owner}},
			"HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "Privileged": false,
				"CgroupnsMode": "private", "CpusetCpus": self.settings.CpuSet, "Memory": self.settings.MemoryBytes,
				"MemorySwap": self.settings.MemoryBytes, "PidsLimit": self.settings.Pids,
				"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}},
			"Mounts": []map[string]any{{"Type": "bind", "Source": self.corpus, "Destination": "/artifacts", "RW": false}},
		}
		if self.mutateInspection != nil {
			self.mutateInspection(inspection)
		}
		return write([]map[string]any{inspection})
	case "start":
		self.started = true
		if self.cancel != nil {
			self.cancel()
			return ctx.Err()
		}
		_, err := io.WriteString(stdout, self.output)
		return err
	case "rm":
		if len(args) != 4 || args[3] != self.id {
			return errors.New("synthetic daemon refused removal by nonimmutable identity")
		}
		if self.failRemove {
			return errors.New("synthetic removal failure")
		}
		self.removed = true
		return nil
	case "container":
		if !self.removed {
			_, err := io.WriteString(stdout, self.id)
			return err
		}
		return nil
	}
	return errors.New("unexpected synthetic daemon command")
}

// Only file plumbing is synthetic here; the fake daemon never runs a scorer.
func newCalibrationReplayFixture(t *testing.T) (string, ScoreInputs, *calibrationReplayDaemonFixture) {
	t.Helper()
	root := newCalibrationFileRoot(t)
	corpus := filepath.Join(root, "evidence", "scorer-input")
	if err := os.MkdirAll(corpus, 0700); err != nil {
		t.Fatal(err)
	}
	writeCalibrationFileFixture(t, corpus, "synthetic.txt", []byte("synthetic immutable bytes\n"))
	inputs := ScoreInputs{
		Runs:             []string{filepath.Join(corpus, "candidate", "results.csv")},
		Stderr:           []string{filepath.Join(corpus, "candidate", "stderr.log")},
		Accounting:       []string{filepath.Join(corpus, "candidate", "accounting.json")},
		Samples:          []string{filepath.Join(corpus, "candidate", "stats")},
		ResourceReports:  []string{filepath.Join(corpus, "candidate", "resources.json")},
		Markers:          []string{filepath.Join(corpus, "candidate", "complete.json")},
		BaselineManifest: filepath.Join(corpus, "baseline.json"),
	}
	settings := defaultCalibrationReplaySettings()
	settings.CpuSet = "0,1"
	daemon := &calibrationReplayDaemonFixture{settings: settings, id: strings.Repeat("a", 64), output: "{\"synthetic\":true}\n"}
	t.Cleanup(func() {
		if daemon.corpus != "" {
			if err := os.RemoveAll(filepath.Dir(daemon.corpus)); err != nil {
				t.Error(err)
			}
		}
	})
	return root, inputs, daemon
}

// Only one private read-only corpus enters the fixed scorer; original bytes
// and paths are not rewritten to make the production path guard pass.
func TestCalibrationReplayUsesContainedOriginalNamespace(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	observations := []calibrationReplayObservation{}
	settings := daemon.settings
	settings.Observe = func(observation calibrationReplayObservation) { observations = append(observations, observation) }
	content, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), settings, daemon.run)
	if err != nil || string(content) != daemon.output || !daemon.started || !daemon.removed {
		t.Fatalf("contained replay: %v, started=%v removed=%v", err, daemon.started, daemon.removed)
	}
	for _, arg := range []string{"--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--cgroupns=private",
		"--security-opt=no-new-privileges:true", "--memory=" + strconv.FormatInt(settings.MemoryBytes, 10),
		"--memory-swap=" + strconv.FormatInt(settings.MemoryBytes, 10), "--pids-limit=128", "--cpuset-cpus=0,1",
		"--run=/artifacts/candidate/results.csv", "--baseline=/artifacts/baseline.json"} {
		if !slices.Contains(daemon.createArgs, arg) {
			t.Errorf("missing required replay boundary %s", arg)
		}
	}
	if len(observations) != 1 || !observations[0].ContainerCleanupVerified ||
		!validSha256(observations[0].InspectionSha256) || !validSha256(observations[0].TerminalSha256) {
		t.Fatalf("missing actual protocol observations: %+v", observations)
	}
	if _, err := os.Stat(filepath.Dir(daemon.corpus)); !os.IsNotExist(err) {
		t.Fatalf("successful replay retained temporary corpus: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(root, "evidence", "scorer-input", "synthetic.txt"))
	if err != nil || string(original) != "synthetic immutable bytes\n" {
		t.Fatalf("original corpus changed: %v", err)
	}
}

// Cancellation at attach cannot cancel the independently owned cleanup call.
func TestCalibrationReplayCleansUpAfterCancellation(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemon.cancel = cancel
	_, err := replayCalibrationScore(ctx, root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
	if err == nil || !daemon.started || !daemon.removed {
		t.Fatalf("canceled replay lost cleanup: %v, %+v", err, daemon)
	}
}

// The daemon may create before losing its reply; owner-verified recovery must
// still remove that one container without authorizing any other name.
func TestCalibrationReplayRecoversLostCreationAcknowledgment(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	daemon.lostCreate = true
	_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
	if err == nil || daemon.started || !daemon.removed {
		t.Fatalf("lost create acknowledgment leaked or ran: %v, %+v", err, daemon)
	}
}

// A foreign label never authorizes destructive cleanup, even after a clean run.
func TestCalibrationReplayRefusesForeignCleanupOwner(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	daemon.foreignOwner = true
	_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
	if err == nil || daemon.removed {
		t.Fatalf("foreign container was removed: %v", err)
	}
	if _, err := os.Stat(daemon.corpus); err != nil {
		t.Fatalf("uncertain cleanup discarded its corpus: %v", err)
	}
}

// Effective daemon settings are checked before start, not inferred from argv.
func TestCalibrationReplayRejectsChangedEffectiveContainment(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(value map[string]any) { value["Image"] = "sha256:" + strings.Repeat("c", 64) },
		func(value map[string]any) { value["HostConfig"].(map[string]any)["Privileged"] = true },
		func(value map[string]any) { value["HostConfig"].(map[string]any)["NetworkMode"] = "host" },
		func(value map[string]any) { value["HostConfig"].(map[string]any)["MemorySwap"] = int64(0) },
		func(value map[string]any) { value["Mounts"].([]map[string]any)[0]["RW"] = true },
		func(value map[string]any) { value["Config"].(map[string]any)["Cmd"] = []string{"untrusted-command"} },
	} {
		root, inputs, daemon := newCalibrationReplayFixture(t)
		daemon.mutateInspection = mutate
		_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
			defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
		if err == nil || daemon.started || !daemon.removed {
			t.Fatalf("changed containment started or leaked: %v", err)
		}
	}
}

// Draining oversized output cannot turn a truncated result into a valid replay.
func TestCalibrationReplayRejectsOversizedOutputAndCleansUp(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	daemon.output = strings.Repeat("x", daemon.settings.OutputBytes+1)
	_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
	if err == nil || !daemon.removed {
		t.Fatalf("oversized replay accepted or leaked: %v", err)
	}
}

// No Docker operation follows a path that escapes the declared original mount.
func TestCalibrationReplayRejectsEscapingScoreInput(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	inputs.BaselineManifest = filepath.Join(root, "outside.json")
	_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
	if err == nil || len(daemon.createArgs) != 0 {
		t.Fatalf("outside input reached Docker: %v", err)
	}
}

// Linked source entries never become a way to copy arbitrary host files.
func TestCalibrationReplayRejectsLinkedCorpus(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "evidence", "scorer-input", "alias")); err != nil {
		t.Fatal(err)
	}
	_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), daemon.settings, daemon.run)
	if err == nil || len(daemon.createArgs) != 0 {
		t.Fatalf("linked corpus reached Docker: %v", err)
	}
}

// Cleanup failure preserves the private replay input for exact reconciliation.
func TestCalibrationReplayRetainsCorpusWhenCleanupFails(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	daemon.failRemove = true
	var observation calibrationReplayObservation
	settings := daemon.settings
	settings.Observe = func(value calibrationReplayObservation) { observation = value }
	_, err := replayCalibrationScore(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		defaultCalibrationAnalysisSettings(), settings, daemon.run)
	if err == nil || daemon.removed || observation.ContainerCleanupVerified {
		t.Fatalf("failed cleanup was accepted or lost context: %v, %+v", err, observation)
	}
	if _, err := os.Stat(daemon.corpus); err != nil {
		t.Fatalf("failed cleanup corpus removed: %v", err)
	}
	content, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(content, &fields); err != nil || fields["corpus_retained"] != true {
		t.Fatalf("retained corpus omitted from reconciliation output: %v, %s", err, content)
	}
}

// Control rebuilding has its own fixed original command and never consumes the
// retained baseline payload as if it were a fresh measurement reduction.
func TestCalibrationReplayBuildsControlWithOriginalScorerCommand(t *testing.T) {
	root, inputs, daemon := newCalibrationReplayFixture(t)
	settings := defaultCalibrationAnalysisSettings()
	_, err := replayCalibrationBundle(context.Background(), root, inputs, "sha256:"+strings.Repeat("b", 64),
		"synthetic-round", true, settings, daemon.settings, daemon.run)
	if err != nil || !daemon.removed || len(daemon.command) == 0 || daemon.command[0] != "score-baseline" {
		t.Fatalf("control replay command: %v, %v", err, daemon.command)
	}
	if slices.Contains(daemon.command, "--baseline=/artifacts/baseline.json") ||
		!slices.Contains(daemon.command, "--round-id=synthetic-round") || !slices.Contains(daemon.command, "--takeover-margin=0.161") {
		t.Fatalf("control replay used a retained payload or unbound policy: %v", daemon.command)
	}
}
