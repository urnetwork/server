// Release image contracts join package cleanup to the actual Makefile exporter.
// A capture-only builder exercises expansion without Docker, a network or pushes.
package runtimepackages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Exercise each release recipe with a builder that can only retain arguments.
func captureRuntimeImageBuild(t *testing.T, directory, epoch string) ([]string, error) {
	t.Helper()
	root := t.TempDir()
	capture := filepath.Join(root, "arguments")
	stub := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > \"$RUNTIME_IMAGE_CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "make", "--no-print-directory", "warp_build_image",
		"SOURCE_DATE_EPOCH="+epoch, "WARP_ENV=synthetic",
		"WARP_DOCKER_NAMESPACE=registry.example/review", "WARP_DOCKER_IMAGE=service", "WARP_DOCKER_VERSION=fixture")
	command.Dir = directory
	command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "RUNTIME_IMAGE_CAPTURE="+capture)
	output, commandErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded Makefile probe: %v", ctx.Err())
	}
	raw, readErr := os.ReadFile(capture)
	if commandErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("rejected epoch reached builder: %v, %q", readErr, raw)
		}
		return nil, fmt.Errorf("make: %w: %s", commandErr, output)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), nil
}

// File timestamp normalization and config timestamp pinning are both required.
// Docker-backed builders must not unpack timestamp-rewritten image layers.
func checkRuntimeImageArguments(args []string, epoch string) error {
	if len(args) < 3 || args[0] != "buildx" || args[1] != "build" || args[len(args)-1] != "." {
		return errors.New("unexpected image builder")
	}
	epochCount, outputCount := 0, 0
	for index, arg := range args {
		if arg == "--push" || arg == "--load" {
			return errors.New("implicit exporter bypasses timestamp policy")
		}
		buildArg, isBuildArg := strings.CutPrefix(arg, "--build-arg=")
		if arg == "--build-arg" && index+1 < len(args) {
			buildArg, isBuildArg = args[index+1], true
		}
		if isBuildArg && strings.HasPrefix(buildArg, "SOURCE_DATE_EPOCH=") {
			if buildArg != "SOURCE_DATE_EPOCH="+epoch {
				return errors.New("image epoch changed or was omitted")
			}
			epochCount++
		}
		outputArg, isOutput := strings.CutPrefix(arg, "--output=")
		if (arg == "--output" || arg == "-o") && index+1 < len(args) {
			outputArg, isOutput = args[index+1], true
		}
		if isOutput {
			if outputArg != "type=image,push=true,rewrite-timestamp=true,unpack=false" {
				return errors.New("exporter must publish timestamp-normalized layers without unpacking")
			}
			outputCount++
		}
	}
	if epochCount != 1 || outputCount != 1 {
		return errors.New("missing or duplicate deterministic image inputs")
	}
	return nil
}

// Changing the wall-clock log or inode cache cannot be fixed by exporter mtimes.
// Also refuse an overbroad cleanup that discards required runtime state.
func TestRuntimeImageRejectsVolatileOrDestructiveInstall(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	for role := range lock.Services {
		raw, err := os.ReadFile(filepath.Join(cli, role, "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		good := string(raw)
		if err := checkRuntimePackageRecipe(lock, role, good); err != nil {
			t.Fatal(err)
		}
		for _, fault := range []string{"clock-log", "inode-cache", "runtime-linker", "package-database"} {
			changed := good
			switch fault {
			case "clock-log":
				changed = strings.Replace(changed, " /var/log/dpkg.log", "", 1)
			case "inode-cache":
				changed = strings.Replace(changed, " /var/cache/ldconfig/aux-cache", "", 1)
			case "runtime-linker":
				changed = strings.Replace(changed, "/var/cache/ldconfig/aux-cache", "/etc/ld.so.cache", 1)
			case "package-database":
				changed = strings.Replace(changed, "rm -f /var/cache", "rm -rf /var/lib/dpkg /var/cache", 1)
			}
			if changed == good || checkRuntimePackageRecipe(lock, role, changed) == nil {
				t.Fatalf("%s: %s escaped image admission", role, fault)
			}
		}
	}
}

// Execute all seven Makefiles with synthetic identity and an explicit epoch.
func TestRuntimeImageBuildPinsEpochAndRewritesLayers(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	const epoch = "1700000000"
	for role := range lock.Services {
		args, err := captureRuntimeImageBuild(t, filepath.Join(cli, role), epoch)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if err := checkRuntimeImageArguments(args, epoch); err != nil {
			t.Fatalf("%s: %v: %q", role, err, args)
		}
		if role == "api" && (!slices.Contains(args, "--provenance=mode=max") || !slices.Contains(args, "--sbom=true")) {
			t.Fatal("determinism must not drop API publication attestations")
		}
		for _, fault := range []string{"epoch", "rewrite", "unpack-default", "unpack-enabled", "implicit-exporter", "duplicate-exporter", "duplicate-epoch"} {
			changed := slices.Clone(args)
			for index, arg := range changed {
				if fault == "epoch" && arg == "SOURCE_DATE_EPOCH="+epoch {
					changed[index] = "SOURCE_DATE_EPOCH="
				}
				if arg == "type=image,push=true,rewrite-timestamp=true,unpack=false" {
					switch fault {
					case "rewrite":
						changed[index] = "type=image,push=true,unpack=false"
					case "unpack-default":
						changed[index] = "type=image,push=true,rewrite-timestamp=true"
					case "unpack-enabled":
						changed[index] = "type=image,push=true,rewrite-timestamp=true,unpack=true"
					}
				}
			}
			if fault == "implicit-exporter" {
				changed = append(changed[:len(changed)-1], "--push", ".")
			}
			if fault == "duplicate-exporter" {
				changed = append(changed[:len(changed)-1], "--output=type=image,push=true", ".")
			}
			if fault == "duplicate-epoch" {
				changed = append(changed[:len(changed)-1], "--build-arg=SOURCE_DATE_EPOCH=0", ".")
			}
			if checkRuntimeImageArguments(changed, epoch) == nil {
				t.Fatalf("%s: %s escaped exporter admission", role, fault)
			}
		}
	}
}

// A missing or malformed caller override must fail before any image operation.
func TestRuntimeImageRejectsInvalidEpochBeforeBuilder(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	for role := range lock.Services {
		for _, epoch := range []string{"", "not-a-time", "-1", "1.5"} {
			if _, err := captureRuntimeImageBuild(t, filepath.Join(cli, role), epoch); err == nil {
				t.Fatalf("%s accepted invalid epoch %q", role, epoch)
			}
		}
	}
}
