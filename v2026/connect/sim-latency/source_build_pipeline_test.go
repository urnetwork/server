// Real private Git contexts and a deterministic daemon seam prove failures
// cannot advance to a later phase or manufacture a successful build artifact.
package main

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSourceBuildPipelineStopsAtFailedMetadataOrCheck(t *testing.T) {
	for _, badMetadata := range []bool{true, false} {
		request := sourceBuildContextFixture(t)
		settings := sourceSandboxTestSettings()
		settings.StateDirectory = t.TempDir()
		if err := os.Chmod(settings.StateDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		stateDirectory := settings.StateDirectory
		t.Cleanup(func() {
			_ = filepath.WalkDir(stateDirectory, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return os.Chmod(path, 0o700)
				}
				return nil
			})
		})
		owner := strings.Repeat("a", 64)
		phases := []string{}
		var daemon *sourceSandboxDaemonFixture
		docker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
			if args[0] == "create" {
				if daemon != nil && !daemon.removed {
					t.Fatal("new phase began before previous container cleanup")
				}
				phase := ""
				for _, arg := range args {
					if strings.HasPrefix(arg, "--label=com.urnetwork.review-build.phase=") {
						phase = strings.TrimPrefix(arg, "--label=com.urnetwork.review-build.phase=")
					}
				}
				phases = append(phases, phase)
				daemon = &sourceSandboxDaemonFixture{input: sourceBuildSandboxInput{Owner: owner, Phase: phase}}
				if phase == "metadata" {
					if badMetadata {
						daemon.startOutput = "synthetic invalid archive"
					} else {
						daemon.startOutput = string(sourceBuildMetadataArchive(t, []*tar.Header{{Name: "version.mod", Typeflag: tar.TypeReg, Size: 4}}))
					}
				} else if phase == "check" {
					daemon.badExit = true
				}
			}
			if daemon == nil {
				t.Fatal("daemon operation preceded owned container creation")
			}
			return daemon.run(ctx, args, out, diagnostics)
		}
		artifacts, err := executeSourceBuildPipeline(context.Background(), request, owner, settings, docker)
		want := []string{"metadata"}
		if !badMetadata {
			want = append(want, "check")
		}
		if err == nil || artifacts != nil || !reflect.DeepEqual(phases, want) || daemon == nil || !daemon.removed {
			t.Fatal("failed phase leaked, advanced or produced successful artifacts", phases, err)
		}
	}
}
