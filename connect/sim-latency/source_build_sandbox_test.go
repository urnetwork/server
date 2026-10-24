// Deterministic Docker seams force lost acknowledgments, cancellation and owner
// changes without starting real containers or relying on scheduling/timeouts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// All identities and paths are synthetic; production mounts never enter tests.
func sourceSandboxFixture() sourceBuildSandboxInput {
	root := filepath.Join("/synthetic/build", strings.Repeat("a", 64))
	return sourceBuildSandboxInput{Owner: strings.Repeat("a", 64), Phase: "check", ImageId: "sha256:" + strings.Repeat("b", 64), Commit: strings.Repeat("c", 40),
		SourceDirectory: filepath.Join(root, "source"), MetadataDirectory: filepath.Join(root, "check", "metadata"), OutputDirectory: filepath.Join(root, "check", "output")}
}

// Synthetic layout uses the production defaults without live filesystem paths.
func sourceSandboxTestSettings() sourceBuildSandboxSettings {
	settings := defaultSourceBuildSandboxSettings()
	settings.StateDirectory = "/synthetic/build"
	return settings
}

func TestSourceBuildSandboxUsesFixedLimitedIndependentPhases(t *testing.T) {
	for _, phase := range []string{"check", "compile"} {
		input := sourceSandboxFixture()
		input.Phase = phase
		input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), phase, "metadata")
		input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), phase, "output")
		args, err := sourceBuildSandboxArguments(input, sourceSandboxTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"--init", "--pull=never", "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--cgroupns=private", "--ulimit=core=0:0",
			"--memory=77309411328", "--memory-swap=77309411328", "--pids-limit=65536", "--cpuset-cpus=0,2,4,6,8,10,12,14,16,18",
			"--tmpfs=/work:rw,exec,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=8589934592",
			"--mount=type=bind,source=" + input.SourceDirectory + ",target=/workspace,readonly"} {
			if !slices.Contains(args, required) {
				t.Fatal("required boundary missing", required)
			}
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--mount=") && !strings.HasSuffix(arg, ",readonly") {
				t.Fatal("candidate gained a writable host mount")
			}
		}
		if args[len(args)-3] != input.ImageId || args[len(args)-2] != "-ceu" {
			t.Fatal("candidate controlled the fixed entrypoint")
		}
		if strings.Contains(args[len(args)-1], input.SourceDirectory) || strings.Contains(args[len(args)-1], input.Commit) {
			t.Fatal("data interpolated into shell syntax")
		}
	}
}

func TestSourceBuildSandboxRejectsUnsafeMountsAndCommands(t *testing.T) {
	for _, mutate := range []func(*sourceBuildSandboxInput){
		func(i *sourceBuildSandboxInput) { i.Phase = "candidate-command" },
		func(i *sourceBuildSandboxInput) { i.SourceDirectory = "/" },
		func(i *sourceBuildSandboxInput) { i.SourceDirectory = "relative" },
		func(i *sourceBuildSandboxInput) { i.SourceDirectory = "/synthetic/source,readonly=false" },
		func(i *sourceBuildSandboxInput) { i.OutputDirectory = i.SourceDirectory + "/output" },
		func(i *sourceBuildSandboxInput) { i.MetadataDirectory = i.OutputDirectory },
		func(i *sourceBuildSandboxInput) { i.ImageId = "synthetic:mutable" },
		func(i *sourceBuildSandboxInput) { i.Commit = "$(untrusted)" },
		func(i *sourceBuildSandboxInput) { i.Phase = "compile" },
	} {
		input := sourceSandboxFixture()
		mutate(&input)
		if _, err := sourceBuildSandboxArguments(input, sourceSandboxTestSettings()); err == nil {
			t.Fatal("unsafe sandbox input accepted")
		}
	}
}

// The fake daemon can create before losing its acknowledgment, exactly as a
// real daemon can; cleanup authentication remains independent of that reply.
type sourceSandboxDaemonFixture struct {
	input        sourceBuildSandboxInput
	lostCreate   bool
	changedOwner bool
	badExit      bool
	removed      bool
	cancel       context.CancelFunc
	startOutput  string
}

// Each branch returns the real Docker command's shape without network/process I/O.
func (self *sourceSandboxDaemonFixture) run(ctx context.Context, args []string, out, _ io.Writer) error {
	id := strings.Repeat("d", 64)
	switch args[0] {
	case "create":
		if self.lostCreate {
			return errors.New("synthetic lost creation acknowledgment")
		}
		_, err := io.WriteString(out, id+"\n")
		return err
	case "start":
		if self.cancel != nil {
			self.cancel()
			return ctx.Err()
		}
		_, err := io.WriteString(out, self.startOutput)
		return err
	case "inspect":
		if ctx.Err() != nil {
			return errors.New("cleanup reused canceled context")
		}
		switch args[2] {
		case "{{.Id}}":
			if args[3] != "urnetwork-reviewed-"+self.input.Owner+"-"+self.input.Phase {
				return errors.New("wrong recovery name")
			}
			_, err := io.WriteString(out, id)
			return err
		case "{{json .State}}":
			if self.badExit {
				_, err := io.WriteString(out, `{"Status":"exited","Running":false,"ExitCode":137,"OOMKilled":true}`)
				return err
			}
			_, err := io.WriteString(out, `{"Status":"exited","Running":false,"ExitCode":0,"OOMKilled":false}`)
			return err
		case "{{json .Config.Labels}}":
			owner := self.input.Owner
			if self.changedOwner {
				owner = strings.Repeat("e", 64)
			}
			return json.NewEncoder(out).Encode(map[string]string{"com.urnetwork.review-build.owner": owner, "com.urnetwork.review-build.phase": self.input.Phase})
		}
	case "rm":
		if ctx.Err() != nil || !reflect.DeepEqual(args, []string{"rm", "--force", "--volumes", id}) {
			return errors.New("unsafe cleanup target/context")
		}
		self.removed = true
		return nil
	case "container":
		if !self.removed {
			return errors.New("residual container")
		}
		return nil
	}
	return errors.New("unexpected synthetic Docker command")
}

func TestSourceBuildSandboxCleansCanceledContainerOutsideCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := sourceSandboxFixture()
	daemon := &sourceSandboxDaemonFixture{input: input, cancel: cancel}
	_, _, err := executeSourceBuildSandbox(ctx, input, sourceSandboxTestSettings(), daemon.run)
	if !errors.Is(err, context.Canceled) || !daemon.removed {
		t.Fatal("canceled build escaped authenticated cleanup", err)
	}
}

func TestSourceBuildSandboxRecoversLostCreationAcknowledgment(t *testing.T) {
	input := sourceSandboxFixture()
	daemon := &sourceSandboxDaemonFixture{input: input, lostCreate: true}
	if _, _, err := executeSourceBuildSandbox(context.Background(), input, sourceSandboxTestSettings(), daemon.run); err == nil || !daemon.removed {
		t.Fatal("lost acknowledgment leaked container or passed build", err)
	}
}

func TestSourceBuildSandboxRefusesChangedOwner(t *testing.T) {
	input := sourceSandboxFixture()
	daemon := &sourceSandboxDaemonFixture{input: input, changedOwner: true}
	if _, _, err := executeSourceBuildSandbox(context.Background(), input, sourceSandboxTestSettings(), daemon.run); err == nil || daemon.removed {
		t.Fatal("cleanup mutated a container with changed ownership", err)
	}
}

func TestSourceBuildSandboxRejectsOomAndTruncatedOutput(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		input := sourceSandboxFixture()
		settings := sourceSandboxTestSettings()
		daemon := &sourceSandboxDaemonFixture{input: input, badExit: !overflow}
		// The create acknowledgment is a protocol ID, not candidate output.
		settings.OutputBytes = 128
		if overflow {
			daemon.startOutput = strings.Repeat("x", settings.OutputBytes+1)
		}
		if _, _, err := executeSourceBuildSandbox(context.Background(), input, settings, daemon.run); err == nil || !daemon.removed {
			t.Fatal("failed build passed or leaked", err)
		}
	}
}

func TestSourceBuildSandboxSuccessRequiresAuthenticatedCleanup(t *testing.T) {
	input := sourceSandboxFixture()
	daemon := &sourceSandboxDaemonFixture{input: input, startOutput: "synthetic check passed"}
	stdout, _, err := executeSourceBuildSandbox(context.Background(), input, sourceSandboxTestSettings(), daemon.run)
	if err != nil || !daemon.removed || string(stdout) != daemon.startOutput {
		t.Fatal("successful check lacked its exact output and cleanup", err)
	}
}

// A clean container exit and convincing text are not a compiled-source receipt.
func TestSourceBuildSandboxRejectsSuccessfulCompilerWithoutBoundExecutable(t *testing.T) {
	input := sourceSandboxFixture()
	input.Phase = "compile"
	input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "metadata")
	input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "output")
	daemon := &sourceSandboxDaemonFixture{input: input, startOutput: "synthetic compilation succeeded"}
	if _, _, err := executeSourceBuildSandbox(context.Background(), input, sourceSandboxTestSettings(), daemon.run); err == nil || !daemon.removed {
		t.Fatal("successful exit bypassed binary authentication or cleanup", err)
	}
}
