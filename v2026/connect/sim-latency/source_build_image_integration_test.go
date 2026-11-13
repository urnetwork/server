// Explicit real-Docker assembly probe reuses an authenticated trusted binary.
// It creates no reviewed-source approval, season record or source publication.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceBuildImageRealCopyOnlyAssembly(t *testing.T) {
	if os.Getenv("SIM_LATENCY_IMAGE_ASSEMBLY_REAL") != "1" {
		t.Skip("requires explicit authenticated trusted-base artifact")
	}
	settings := defaultSourceBuildSandboxSettings()
	settings.StateDirectory = os.Getenv("SIM_LATENCY_IMAGE_ASSEMBLY_ROOT")
	base := os.Getenv("SIM_LATENCY_IMAGE_ASSEMBLY_BASE")
	if !filepath.IsAbs(settings.StateDirectory) || !strings.HasPrefix(base, "sha256:") || !validSha256(strings.TrimPrefix(base, "sha256:")) {
		t.Fatal("assembly probe requires exact private root and immutable base image")
	}
	binary, err := os.ReadFile(os.Getenv("SIM_LATENCY_IMAGE_ASSEMBLY_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := verifySourceBuildBinary(binary, os.Getenv("SIM_LATENCY_IMAGE_ASSEMBLY_COMMIT"))
	if err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	owner := hex.EncodeToString(nonce[:])
	directory := filepath.Join(settings.StateDirectory, owner)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// This is deliberately a trusted-base copy probe, not a fabricated future
	// generation. The unchanged binary retains the incumbent's real identity.
	dockerfile := fmt.Sprintf("FROM urnetwork/sim-latency-evaluator-base-id:%s\nCOPY --chmod=0555 sim-latency /opt/urnetwork/bin/sim-latency\nLABEL com.urnetwork.competition.image-kind=\"evaluator-base\"\n", base[len("sha256:"):])
	for name, data := range map[string][]byte{"Dockerfile": []byte(dockerfile), ".dockerignore": []byte("**\n!Dockerfile\n!.dockerignore\n!sim-latency\n"), "sim-latency": binary,
		"excluded-synthetic-secret": []byte("synthetic-not-an-operator-secret")} {
		if err := writePrivateReviewFile(filepath.Join(directory, name), data); err != nil {
			t.Fatal(err)
		}
	}
	docker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
		command := exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "/usr/bin/docker", "--host=unix:///var/run/docker.sock"}, args...)...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		command.Stdout, command.Stderr = out, diagnostics
		if args[0] != "build" {
			return command.Run()
		}
		output, diagnosticOutput := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
		command.Stdout, command.Stderr = io.MultiWriter(out, output), io.MultiWriter(diagnostics, diagnosticOutput)
		commandErr := command.Run()
		for name, data := range map[string][]byte{"build.stdout.log": output.content, "build.stderr.log": diagnosticOutput.content} {
			if err := writePrivateReviewFile(filepath.Join(directory, name), data); err != nil {
				return err
			}
		}
		return commandErr
	}
	image, err := executeSourceBuildImage(context.Background(), owner, base, directory, settings, docker)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), settings.CleanupTimeout)
		defer cancel()
		if err := docker(ctx, []string{"image", "rm", "--no-prune", image}, io.Discard, io.Discard); err != nil {
			t.Error("owned assembly probe image cleanup failed", err)
		}
	}()
	if err := writePrivateReviewFile(filepath.Join(directory, "assembly-result.json"), []byte(fmt.Sprintf("{\"probe\":true,\"image_id\":%q,\"simulator_sha256\":%q}\n", image, digest))); err != nil {
		t.Fatal(err)
	}
	t.Logf("copy-only trusted-base probe image=%s simulator_sha256=%s; owned image removed after probe", image, digest)
}
