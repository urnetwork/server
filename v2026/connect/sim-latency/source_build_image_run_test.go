// Fake Docker forces alias substitution, inherited execution and output loss
// before the production assembler can mistake a build for qualified evidence.
package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSourceBuildImageAssemblyAuthenticatesAliasAndOwner(t *testing.T) {
	for _, mutation := range []string{"", "base-id", "onbuild", "owner", "image-kind", "overflow"} {
		owner, base, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
		settings := sourceSandboxTestSettings()
		directory := filepath.Join(settings.StateDirectory, owner)
		built := false
		docker := func(_ context.Context, args []string, out, _ io.Writer) error {
			if args[0] == "build" {
				built = true
				for _, required := range []string{"--pull=false", "--network=none", "--no-cache", "--label=com.urnetwork.review-build.owner=" + owner, "--file=" + filepath.Join(directory, "Dockerfile")} {
					if !slices.Contains(args, required) {
						t.Fatal("assembly lost fixed boundary", required)
					}
				}
				if mutation == "overflow" {
					_, err := io.WriteString(out, strings.Repeat("x", settings.OutputBytes+1))
					return err
				}
				return nil
			}
			id := base
			labels := map[string]string{}
			var hooks []string
			if !built {
				if args[len(args)-1] != "urnetwork/sim-latency-evaluator-base-id:"+strings.TrimPrefix(base, "sha256:") {
					t.Fatal("inspected different base alias")
				}
				if mutation == "base-id" {
					id = image
				}
				if mutation == "onbuild" {
					hooks = []string{"RUN synthetic-untrusted-command"}
				}
			} else {
				id = image
				labels["com.urnetwork.review-build.owner"] = owner
				labels["com.urnetwork.competition.image-kind"] = "evaluator-base"
				if mutation == "owner" {
					labels["com.urnetwork.review-build.owner"] = "different-owner"
				}
				if mutation == "image-kind" {
					labels["com.urnetwork.competition.image-kind"] = "submission"
				}
			}
			return json.NewEncoder(out).Encode(map[string]any{"Id": id, "Config": map[string]any{"OnBuild": hooks, "Labels": labels}})
		}
		actual, err := executeSourceBuildImage(context.Background(), owner, base, directory, settings, docker)
		if mutation == "" {
			if err != nil || actual != image {
				t.Fatal("valid assembly failed", actual, err)
			}
		} else if err == nil || actual != "" || ((mutation == "base-id" || mutation == "onbuild") && built) {
			t.Fatal("unsafe image assembly passed or began", mutation, actual, err)
		}
	}
}

func TestSourceBuildImageContextExcludesAllNonAllowlistedInputs(t *testing.T) {
	ignore := string(sourceBuildImageIgnore())
	if !strings.HasPrefix(ignore, "**\n") || strings.Contains(ignore, "!check") || strings.Contains(ignore, "!compile") || strings.Contains(ignore, "!config") || strings.Contains(ignore, "!vault") {
		t.Fatal("context no longer denies surrounding logs and credentials")
	}
	for _, required := range []string{"!source/**\n", "!sim-latency\n", "!image-identity.json\n", "!canonical.patch\n"} {
		if !strings.Contains(ignore, required) {
			t.Fatal("assembly lost authenticated input", required)
		}
	}
}
