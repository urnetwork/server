//go:build linux

// The real compiler CREATE command crosses the installed CLI and full proxy.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// No compiler/container is run: an inert daemon acknowledges admitted CREATE.
func TestTrackedBuilderActualCliCreateAdmission(t *testing.T) {
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("supplementary Docker CLI serialization control")
	}
	content, err := os.ReadFile(trackedBuilderAsset(t, "tracked-submission-build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(content), `container="$(evaluation_docker create`)
	if start < 0 {
		t.Fatal("actual compiler CREATE missing")
	}
	fragment := string(content)[start:]
	end := strings.Index(fragment, " || infrastructure_failure")
	if end < 0 {
		t.Fatal("actual compiler CREATE boundary missing")
	}
	fragment = fragment[:end]
	for _, stage := range []string{"check", "compile"} {
		fixture := newCandidateSourceTest(t)
		writeCandidateCoverageIdentity(t, fixture, `{"kind":"seeded-crawl-coverage/v1","minimum_page_coverage_ratio":0.95,"minimum_byte_coverage_ratio":0.95}`)
		resource := testRunResourceIdentity()
		resource.BuildMemoryLimit, resource.BuildMemoryLimitBytes = "12g", 12*1024*1024*1024
		resource.BuildWorkLimit, resource.BuildWorkLimitBytes = "10g", 10*1024*1024*1024
		if err := os.Chmod(filepath.Join(fixture.source, ".evaluation-source.json"), 0444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fixture.source, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(fixture.source, 0700) })
		policy, err := newBuildRetirementPolicy(runSliceResources{ResourcePolicySha256: fixture.fixture.plan.ResourcePolicySha256, Identity: resource, BuildTasksMaximum: resource.BuildPidsLimit, CpuPeriodMicros: 100000}, fixture.fixture.plan.BuildCgroupName, int64(os.Getpagesize()))
		if err != nil {
			t.Fatal(err)
		}
		fixture.fixture.proxy.plan.BuildRetirement = policy
		cache := dockerPolicyTestDirectory(t, filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", stage+"-cache"))
		output := dockerPolicyTestDirectory(t, filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", "output"))
		fixture.fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if dockerProxyApiPath(r.URL.Path) == "/_ping" {
				w.Header().Set("API-Version", "1.56")
				return
			}
			if r.Method == http.MethodPost && dockerProxyApiPath(r.URL.Path) == "/containers/create" {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]string{"Id": strings.Repeat("f", 64)})
				return
			}
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && dockerProxyApiPath(r.URL.Path) == "/containers/create" {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				file := filepath.Join(t.TempDir(), "synthetic-create.json")
				if err := os.WriteFile(file, body, 0600); err != nil {
					t.Error(err)
					return
				}
				if _, err := fixture.fixture.proxy.verifyMutationScope(r.Context(), r, file); err != nil {
					t.Logf("composed CREATE refusal: %v", err)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			fixture.fixture.proxy.workerHandler(w, r)
		}))
		t.Cleanup(server.Close)
		body := `set -Eeuo pipefail
evaluation_docker() { "$TEST_DOCKER_CLI" --host "$TEST_DOCKER_HOST" "$@"; }
stage="$TEST_STAGE"
script=synthetic-no-execution
cache="$TEST_CACHE"
output="$TEST_OUTPUT"
output_args=()
if [[ "$stage" = compile ]]; then output_args=(--mount "type=bind,src=$output,dst=/candidate-output"); fi
subevaluation_run_id="$TEST_RUN"
build_cgroup_parent="$TEST_GROUP"
evaluation_cpuset="$TEST_CPUS"
build_memory_limit="$TEST_MEMORY"
tracked_build_pids="$TEST_PIDS"
tracked_build_work="$TEST_WORK"
source_root="$TEST_SOURCE"
base_image_id="$TEST_IMAGE"
` + fragment
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "DOCKER_API_VERSION=1.56", "TEST_DOCKER_CLI=" + cli, "TEST_DOCKER_HOST=tcp://" + strings.TrimPrefix(server.URL, "http://"), "TEST_STAGE=" + stage, "TEST_CACHE=" + cache, "TEST_OUTPUT=" + output, "TEST_RUN=" + fixture.fixture.plan.RunId, "TEST_GROUP=" + fixture.fixture.plan.BuildCgroupName, "TEST_CPUS=" + fixture.fixture.plan.CpuSet, "TEST_MEMORY=" + resource.BuildMemoryLimit, "TEST_PIDS=" + strconv.FormatInt(resource.BuildPidsLimit, 10), "TEST_WORK=" + resource.BuildWorkLimit, "TEST_SOURCE=" + fixture.source, "TEST_IMAGE=" + fixture.fixture.plan.BaseImageDigest, "URNETWORK_SUBEVALUATION_JOB_ID=" + fixture.fixture.plan.JobId, "URNETWORK_SUBEVALUATION_ROUND_ID=" + fixture.fixture.plan.RoundId, "URNETWORK_SUBEVALUATION_CORE_PROFILE=" + fixture.fixture.plan.CandidateCoreProfile.Path}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("actual %s CLI CREATE rejected: %v: %s", stage, err, output)
		}
	}
}
