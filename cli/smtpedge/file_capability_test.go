// smtpedge's binary carries cap_net_admin=ep as a file capability (QD-1066, owner decision 597; DESIGN §19.4):
// the Dockerfile writes it in a stage of its own with filecap's helper from a bind mount, copies the binary on
// and checks it last, and the Makefile builds the helper static for both platforms; the release check's defaults
// name the same binary and platforms. Where a docker daemon answers, the image is built from the Dockerfile's own
// capability instructions on scratch, with no registry and nothing pulled, and the attribute is read back out of
// it as the release check reads a published image and as getcap prints it; the binary then runs as warp runs the
// block, uid 65532 with --cap-add=CAP_NET_ADMIN, and sets SO_MARK, and the kernel refuses its exec without the
// grant. A recipe without the write fails at its check, and an image without either is refused by the release
// check.

package main_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/cli/smtpedge/filecap"
)

const testBinaryPath = "/usr/local/sbin/bringyour-smtpedge"

// The capability stage and the final stage's copy and check, as the Dockerfile must hold them.
var testFileCapabilityStage = []string{
	"FROM scratch AS file-capability",
	"ARG TARGETPLATFORM",
	"COPY build/$TARGETPLATFORM/smtpedge " + testBinaryPath,
	`RUN --network=none --mount=type=bind,source=build/$TARGETPLATFORM/filecap,target=/filecap ["/filecap", "set", "` + testBinaryPath + `"]`,
}

var testFileCapabilityFinal = []string{
	"COPY --from=file-capability " + testBinaryPath + " " + testBinaryPath,
	`RUN --network=none --mount=type=bind,source=build/$TARGETPLATFORM/filecap,target=/filecap ["/filecap", "check", "` + testBinaryPath + `"]`,
}

// The Dockerfile's instructions by stage, each continued line joined, comments and blank lines dropped.
func testDockerfileStages(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	stages := [][]string{}
	pending := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			pending += strings.TrimSpace(strings.TrimSuffix(line, "\\")) + " "
			continue
		}
		instruction := pending + line
		pending = ""
		if strings.HasPrefix(instruction, "FROM ") {
			stages = append(stages, []string{instruction})
			continue
		}
		if len(stages) == 0 {
			t.Fatalf("%q comes before any FROM", instruction)
		}
		stages[len(stages)-1] = append(stages[len(stages)-1], instruction)
	}
	if pending != "" {
		t.Fatalf("the Dockerfile ends inside %q", pending)
	}
	return stages
}

func TestImageGivesTheBinaryItsFileCapability(t *testing.T) {
	stages := testDockerfileStages(t)
	capabilityAt := slices.IndexFunc(stages, func(stage []string) bool { return stage[0] == testFileCapabilityStage[0] })
	if capabilityAt < 0 || !slices.Equal(stages[capabilityAt], testFileCapabilityStage) {
		t.Fatalf("the file capability stage is not %q: %q", testFileCapabilityStage, stages)
	}
	final := stages[len(stages)-1]
	if capabilityAt == len(stages)-1 || strings.Contains(final[0], " AS ") {
		t.Fatalf("the final stage is %q", final[0])
	}
	copyAt := slices.Index(final, testFileCapabilityFinal[0])
	checkAt := slices.Index(final, testFileCapabilityFinal[1])
	if copyAt < 0 || checkAt != copyAt+1 {
		t.Fatalf("the final stage does not copy the binary and then check it: %q", final)
	}
	// nothing after the check may write the binary: a copy over it, or a chown, drops the attribute
	for _, instruction := range final[checkAt+1:] {
		verb, _, _ := strings.Cut(instruction, " ")
		if !slices.Contains([]string{"ENV", "ARG", "LABEL", "STOPSIGNAL", "CMD"}, verb) {
			t.Errorf("%q follows the check", instruction)
		}
	}
	// nor does any other instruction in the final stage name it
	for at, instruction := range final {
		if at != copyAt && at != checkAt && strings.Contains(instruction, testBinaryPath) && !strings.HasPrefix(instruction, "CMD ") {
			t.Errorf("%q names the binary", instruction)
		}
	}
	if cmd := final[len(final)-1]; !strings.HasPrefix(cmd, `CMD ["`+testBinaryPath+`"`) {
		t.Errorf("the image runs %q", cmd)
	}

	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/filecap ./filecap/cmd",
		"env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/filecap ./filecap/cmd",
		"--platform linux/arm64/v8,linux/amd64",
	} {
		if !strings.Contains(string(makefile), line) {
			t.Errorf("the Makefile lacks %q", line)
		}
	}
	// the release check reads the binary the image runs, on each platform the image is built for (arm64/v8 is
	// arm64's default variant, which docker names linux/arm64)
	settings := filecap.DefaultImageCheckSettings()
	if settings.BinaryPath != testBinaryPath {
		t.Errorf("the release check reads %s", settings.BinaryPath)
	}
	platforms := slices.Clone(settings.Platforms)
	slices.Sort(platforms)
	if !slices.Equal(platforms, []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("the release check reads %q", settings.Platforms)
	}
}

// The docker command line under a bound, its output answered.
func testDocker(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var output bytes.Buffer
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return output.String(), err
}

func TestFileCapabilityReadsBackOutOfTheBuiltImage(t *testing.T) {
	if testing.Short() {
		t.Skip("builds images")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker command here")
	}
	server, err := testDocker(t.Context(), "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		t.Skipf("no docker daemon answers: %v: %s", err, server)
	}
	platform := strings.TrimSpace(server)
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok || goos != "linux" {
		t.Skipf("the daemon runs %q", platform)
	}

	// the context the Dockerfile's stage reads: the helper, and as the binary the helper again, whose so-mark
	// shows what the attribute grants
	buildContext := t.TempDir()
	build := filepath.Join(buildContext, "build", goos, goarch)
	if err := os.MkdirAll(build, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"filecap", "smtpedge"} {
		command := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", filepath.Join(build, name), "./filecap/cmd")
		command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build the helper: %v: %s", err, output)
		}
	}

	stages := testDockerfileStages(t)
	capabilityAt := slices.IndexFunc(stages, func(stage []string) bool { return stage[0] == testFileCapabilityStage[0] })
	if capabilityAt < 0 {
		t.Fatal("the Dockerfile has no file capability stage")
	}
	final := stages[len(stages)-1]
	finalInstructions := []string{}
	for _, instruction := range final {
		if slices.Contains(testFileCapabilityFinal, instruction) {
			finalInstructions = append(finalInstructions, instruction)
		}
	}
	// the Dockerfile's own instructions, the final stage on scratch so nothing is pulled; drop leaves one out
	dockerfile := func(drop ...string) string {
		instructions := slices.Clone(stages[capabilityAt])
		instructions = append(instructions, "FROM scratch", "ARG TARGETPLATFORM")
		instructions = append(instructions, finalInstructions...)
		instructions = append(instructions, `CMD ["`+testBinaryPath+`"]`)
		instructions = slices.DeleteFunc(instructions, func(instruction string) bool { return slices.Contains(drop, instruction) })
		return strings.Join(instructions, "\n") + "\n"
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	image := func(name string) string {
		return "sidecar-test-smtpedge-filecap:" + name + "-" + hex.EncodeToString(suffix)
	}
	buildImage := func(name string, recipe string) (string, error) {
		path := filepath.Join(buildContext, name+".Dockerfile")
		if err := os.WriteFile(path, []byte(recipe), 0644); err != nil {
			t.Fatal(err)
		}
		// the test's context has ended when cleanups run
		t.Cleanup(func() { testDocker(context.Background(), "image", "rm", "--force", image(name)) })
		return testDocker(t.Context(), "build", "--progress=plain", "--no-cache", "--platform", platform, "--file", path, "--tag", image(name), buildContext)
	}
	settings := filecap.DefaultImageCheckSettings()
	settings.Pull = "never"
	settings.Platforms = []string{platform}

	// the recipe as it is: the attribute reads back as the release check reads it and as getcap prints it
	if output, err := buildImage("built", dockerfile()); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	lines, err := filecap.CheckImage(t.Context(), image("built"), settings)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{platform + " " + testBinaryPath + " cap_net_admin=ep"}; !slices.Equal(lines, want) {
		t.Fatalf("read back %q, want %q", lines, want)
	}

	// run as warp runs the block, the binary sets SO_MARK; without the grant the kernel refuses its exec
	run := func(name string, args ...string) (string, error) {
		return testDocker(t.Context(), append(append([]string{"run", "--rm", "--name", strings.ReplaceAll(image(name), ":", "-"),
			"--network", "none", "--user", "65532:65532"}, args...), image("built"), testBinaryPath, "so-mark")...)
	}
	if output, err := run("granted", "--cap-add=CAP_NET_ADMIN"); err != nil || !strings.Contains(output, "SO_MARK set") {
		t.Fatalf("as uid 65532 with CAP_NET_ADMIN granted: %v: %s", err, output)
	}
	if output, err := run("refused"); err == nil || !strings.Contains(output, "operation not permitted") {
		t.Fatalf("as uid 65532 with nothing granted: %v: %s", err, output)
	}

	// without the write the build stops at its check
	set := testFileCapabilityStage[len(testFileCapabilityStage)-1]
	if output, err := buildImage("unset", dockerfile(set)); err == nil || !strings.Contains(output, "no security.capability attribute") {
		t.Fatalf("a recipe that writes no attribute built: %v: %s", err, output)
	}
	// and an image built without either is refused by the release check
	if output, err := buildImage("unchecked", dockerfile(set, testFileCapabilityFinal[1])); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	if _, err := filecap.CheckImage(t.Context(), image("unchecked"), settings); err == nil || !strings.Contains(err.Error(), "no security.capability attribute") {
		t.Fatalf("the release check answered %v for an image without the attribute", err)
	}
	t.Logf("read back: %s", strings.Join(lines, "; "))
}
