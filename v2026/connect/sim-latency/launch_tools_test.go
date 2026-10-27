package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Local bare clones can contain objects from unrelated branches. Object
// presence alone must never authenticate a competition-branch checkpoint.
func TestRemoteSourcePreflightRejectsUnreachableCommitObjects(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, name := range sourceRepositoryNames() {
		commits[name] = sourceTestRepository(t, root, name)
		local := filepath.Join(root, name)
		if name == "server" {
			sourceTestGit(t, local, "checkout", "--quiet", "--orphan", "synthetic-unrelated")
			sourceTestGit(t, local, "commit", "--quiet", "--no-gpg-sign", "-m", "unrelated source")
			commits[name] = sourceTestGit(t, local, "rev-parse", "HEAD")
			sourceTestGit(t, local, "checkout", "--quiet", "sim-latency")
		}
		remote := filepath.Join(t.TempDir(), name+".git")
		sourceTestGit(t, root, "clone", "--quiet", "--bare", local, remote)
		sourceTestGit(t, local, "remote", "add", "origin", remote)
	}
	if err := verifyRemoteSourceEpoch(sourceTestManifest(commits), 0, root); err == nil {
		t.Fatal("unreachable commit object authenticated as competition source")
	}
}

// Execute the real preflight against Git fixture remotes, but make a full
// initial-history clone fail deterministically instead of depending on timing.
func TestRemoteSourcePreflightBoundsInitialHistoryTransfer(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	commits := map[string]string{}
	for _, name := range sourceRepositoryNames() {
		commits[name] = sourceTestRepository(t, root, name)
		local := filepath.Join(root, name)
		remote := filepath.Join(t.TempDir(), name+".git")
		sourceTestGit(t, root, "clone", "--quiet", "--bare", local, remote)
		sourceTestGit(t, local, "remote", "add", "origin", "file://"+remote)
	}
	bin := t.TempDir()
	shim := `#!/bin/sh
if test "$1" = clone; then
  depth=no
  filter=no
  for argument do
    test "$argument" != --depth=1 || depth=yes
    test "$argument" != --filter=blob:none || filter=yes
  done
  test "$depth:$filter" = yes:yes || { echo unbounded-initial-history >&2; exit 91; }
fi
exec "$SYNTHETIC_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_REAL_GIT", git)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := verifyRemoteSourceEpoch(sourceTestManifest(commits), 0, root); err != nil {
		t.Fatal("tip verification downloaded unnecessary history:", err)
	}
}

// A genuinely older checkpoint still needs its branch ancestry. File remotes
// force real shallow-clone behavior rather than Git's local-copy shortcut.
func TestRemoteSourcePreflightDeepensHistoricalCheckpoint(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	serverHead := ""
	for _, name := range sourceRepositoryNames() {
		commits[name] = sourceTestRepository(t, root, name)
		local := filepath.Join(root, name)
		if name == "server" {
			if err := os.WriteFile(filepath.Join(local, "newer.txt"), []byte("newer competition source\n"), 0600); err != nil {
				t.Fatal(err)
			}
			sourceTestGit(t, local, "add", "newer.txt")
			sourceTestGit(t, local, "commit", "--quiet", "--no-gpg-sign", "-m", "advance source")
			serverHead = sourceTestGit(t, local, "rev-parse", "HEAD")
		}
		remote := filepath.Join(t.TempDir(), name+".git")
		sourceTestGit(t, root, "clone", "--quiet", "--bare", local, remote)
		sourceTestGit(t, local, "remote", "add", "origin", "file://"+remote)
	}
	manifest := sourceTestManifest(commits)
	if err := verifyRemoteSourceEpoch(manifest, 0, root); err != nil {
		t.Fatal("shallow verification lost a valid historical checkpoint:", err)
	}
	if err := verifyRemoteSourceEpochHead(manifest, 0, root); err == nil {
		t.Fatal("exact-head verification accepted a historical checkpoint")
	}
	if got := sourceTestGit(t, filepath.Join(root, "server"), "rev-parse", "HEAD"); got != serverHead {
		t.Fatal("historical preflight changed the operator checkout")
	}
}

// Git's --branch can also select a tag. Matching bytes are insufficient when
// the required competition branch itself has disappeared.
func TestRemoteSourcePreflightRejectsTagInsteadOfBranch(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, name := range sourceRepositoryNames() {
		commits[name] = sourceTestRepository(t, root, name)
		local := filepath.Join(root, name)
		if name == "server" {
			sourceTestGit(t, local, "tag", "sim-latency")
			sourceTestGit(t, local, "branch", "-m", "synthetic-product")
		}
		remote := filepath.Join(t.TempDir(), name+".git")
		sourceTestGit(t, root, "clone", "--quiet", "--bare", local, remote)
		sourceTestGit(t, local, "remote", "add", "origin", remote)
	}
	manifest := sourceTestManifest(commits)
	if err := verifyRemoteSourceEpoch(manifest, 0, root); err == nil {
		t.Error("tag substituted for the frozen source branch")
	}
	if err := verifyRemoteSourceEpochHead(manifest, 0, root); err == nil {
		t.Error("tag substituted for the active source branch")
	}
}

func TestLaunchPreflightVerifiesRemoteEpochWithoutChangingWorkingTrees(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repositoryName := range sourceRepositoryNames() {
		commit := sourceTestRepository(t, repositoriesRoot, repositoryName)
		repositoryCommits[repositoryName] = commit
		remoteRoot := filepath.Join(t.TempDir(), repositoryName+".git")
		sourceTestGit(t, repositoriesRoot, "clone", "--quiet", "--bare", filepath.Join(repositoriesRoot, repositoryName), remoteRoot)
		sourceTestGit(t, filepath.Join(repositoriesRoot, repositoryName), "remote", "add", "origin", remoteRoot)
	}
	manifest := sourceTestManifest(repositoryCommits)
	if err := verifyRemoteSourceEpoch(manifest, 0, repositoriesRoot); err != nil {
		t.Fatal(err)
	}

	serverRoot := filepath.Join(repositoriesRoot, "server")
	if err := os.WriteFile(filepath.Join(serverRoot, "local-only.txt"), []byte("runner checkout remains untouched\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteSourceEpoch(manifest, 0, repositoriesRoot); err != nil {
		t.Fatalf("dirty runner checkout affected remote verification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(serverRoot, "local-only.txt")); err != nil {
		t.Fatal("remote source verification modified the runner checkout")
	}

	serverHead := sourceTestGit(t, serverRoot, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(serverRoot, "advanced.txt"), []byte("partial promotion\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, serverRoot, "add", "advanced.txt")
	sourceTestGit(t, serverRoot, "commit", "--no-gpg-sign", "-m", "advance branch")
	sourceTestGit(t, serverRoot, "push", "origin", "HEAD:sim-latency")
	if err := verifyRemoteSourceEpoch(manifest, 0, repositoriesRoot); err != nil {
		t.Fatalf("historical reachability failed after branch advance: %v", err)
	}
	if err := verifyRemoteSourceEpochHead(manifest, 0, repositoriesRoot); err == nil {
		t.Fatalf("active source preflight accepted branch ahead of ledger commit %s", serverHead)
	}
}

func TestStagingSourceBranchesExactlyAliasBaseline(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repositoryName := range sourceRepositoryNames() {
		commit := sourceTestRepository(t, repositoriesRoot, repositoryName)
		repositoryCommits[repositoryName] = commit
		localRoot := filepath.Join(repositoriesRoot, repositoryName)
		remoteRoot := filepath.Join(t.TempDir(), repositoryName+".git")
		sourceTestGit(t, repositoriesRoot, "clone", "--quiet", "--bare", localRoot, remoteRoot)
		sourceTestGit(t, localRoot, "remote", "add", "origin", remoteRoot)
		sourceTestGit(t, localRoot, "push", "--quiet", "origin", commit+":refs/heads/"+stagingEvaluationSourceBranch)
	}
	manifest := sourceTestManifest(repositoryCommits)
	if err := verifyRemoteSourceEpochBranchHead(
		manifest,
		0,
		repositoriesRoot,
		stagingEvaluationSourceBranch,
	); err != nil {
		t.Fatal(err)
	}

	serverRoot := filepath.Join(repositoriesRoot, "server")
	if err := os.WriteFile(filepath.Join(serverRoot, "staging-drift.txt"), []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, serverRoot, "add", "staging-drift.txt")
	sourceTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "staging drift")
	sourceTestGit(t, serverRoot, "push", "--quiet", "origin", "HEAD:refs/heads/"+stagingEvaluationSourceBranch)
	if err := verifyRemoteSourceEpochBranchHead(
		manifest,
		0,
		repositoriesRoot,
		stagingEvaluationSourceBranch,
	); err == nil || !strings.Contains(err.Error(), "does not match active epoch commit") {
		t.Fatalf("advanced staging branch was accepted: %v", err)
	}
}

func TestLaunchPreflightChecksGrafanaRoutingAndLiveMetrics(t *testing.T) {
	metricQueries := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer grafana-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/health":
			_, _ = response.Write([]byte(`{"database":"ok"}`))
		case "/api/search":
			_, _ = response.Write([]byte(`[{"uid":"urnetwork-competition"}]`))
		case "/api/v1/provisioning/alert-rules":
			_, _ = response.Write([]byte(`[
                    {"uid":"competition-runner-heartbeat-stale"},
                    {"uid":"competition-minio-capacity-warning"},
                    {"uid":"competition-minio-capacity-critical"}
                ]`))
		case "/api/v1/provisioning/contact-points":
			_, _ = response.Write([]byte(`[{"uid":"urnetwork-default-slack","name":"urnetwork-default","type":"slack","settings":{"url":"[REDACTED]"}}]`))
		case "/api/v1/provisioning/policies":
			_, _ = response.Write([]byte(`{"receiver":"urnetwork-default","routes":[{"receiver":"urnetwork-default","object_matchers":[["service","=","sim-latency"],["severity","=~","warn|page"]]}]}`))
		case "/api/v1/query":
			metricQueries = append(metricQueries, request.URL.Query().Get("query"))
			value := "12"
			if len(metricQueries) == 2 {
				value = "42"
			}
			_, _ = response.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"` + value + `"]}]}}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	config := &launchPreflightConfig{
		GrafanaUrl:   server.URL,
		GrafanaToken: "grafana-secret",
		MetricsUrl:   server.URL,
		MetricsToken: "grafana-secret",
	}
	if _, err := checkCompetitionGrafana(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	metrics, err := checkCompetitionMetrics(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if metrics["runner_heartbeat_age_seconds"] != 12.0 || metrics["minio_used_percent"] != 42.0 || len(metricQueries) != 2 {
		t.Fatalf("metrics evidence = %#v, queries=%v", metrics, metricQueries)
	}
}

func TestHandoffManifestAuthenticatesEveryLocalInput(t *testing.T) {
	temporaryRoot := t.TempDir()
	commits := map[string]string{}
	for index, repositoryName := range sourceRepositoryNames() {
		commits[repositoryName] = fmt.Sprintf("%040x", index+1)
	}
	sourcePath := filepath.Join(temporaryRoot, "sim-latency.yml")
	writeSourceTestManifest(t, sourcePath, sourceTestManifest(commits))
	openApiPath := filepath.Join(temporaryRoot, "competition.yml")
	baselinePath := filepath.Join(temporaryRoot, "MANIFEST.sha256")
	stagingPath := filepath.Join(temporaryRoot, "staging.json")
	for filePath, content := range map[string]string{
		openApiPath:  "openapi: 3.1.0\n",
		baselinePath: "abc  baseline.json\n",
		stagingPath:  "{\"schema\":1}\n",
	} {
		if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sourceEvidence, err := inspectEvidenceFile(sourcePath, maximumSourceConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	openApiEvidence, err := inspectEvidenceFile(openApiPath, maximumSourceConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	evaluatorDigest := "sha256:" + strings.Repeat("a", 64)
	preflightPath := filepath.Join(temporaryRoot, "preflight.json")
	preflight := launchPreflightResult{
		Schema:               launchEvidenceSchema,
		Passed:               true,
		CheckedAt:            time.Date(2026, time.August, 29, 1, 0, 0, 0, time.UTC),
		Epoch:                0,
		SourceConfigSha256:   sourceEvidence.Sha256,
		EvaluatorImageDigest: evaluatorDigest,
		OpenApiSha256:        openApiEvidence.Sha256,
		Checks:               map[string]launchCheck{},
	}
	for _, name := range mandatoryLaunchChecks {
		preflight.Checks[name] = launchCheck{Passed: true}
	}
	preflightBytes, err := json.Marshal(preflight)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preflightPath, preflightBytes, 0600); err != nil {
		t.Fatal(err)
	}

	manifest, err := buildLaunchHandoffManifest(
		0,
		sourcePath,
		evaluatorDigest,
		openApiPath,
		baselinePath,
		preflightPath,
		[]string{stagingPath},
		time.Date(2026, time.August, 29, 2, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Source.Repositories["server"] != commits["server"] || manifest.OpenApi.Sha256 != openApiEvidence.Sha256 ||
		manifest.Apex.RegistryIdentifier != nil || manifest.Apex.MacrocosmosSignature != nil ||
		manifest.Operations.IncidentContact != "support@ur.xyz" || len(manifest.StagingEvidence) != 1 {
		t.Fatalf("handoff manifest = %+v", manifest)
	}

	preflight.Passed = false
	preflightBytes, err = json.Marshal(preflight)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preflightPath, preflightBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildLaunchHandoffManifest(
		0, sourcePath, evaluatorDigest, openApiPath, baselinePath, preflightPath, nil, time.Now(),
	); err == nil {
		t.Fatal("failed preflight evidence entered a handoff manifest")
	}

	preflight.Passed = true
	delete(preflight.Checks, mandatoryLaunchChecks[0])
	preflightBytes, err = json.Marshal(preflight)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preflightPath, preflightBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildLaunchHandoffManifest(
		0, sourcePath, evaluatorDigest, openApiPath, baselinePath, preflightPath, nil, time.Now(),
	); err == nil {
		t.Fatal("incomplete preflight evidence entered a handoff manifest")
	}
}

func TestLaunchCredentialFilesMustBePrivate(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(credentialPath, []byte("secret-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := readPrivateToken(credentialPath); err != nil || token != "secret-token" {
		t.Fatalf("private token = %q, %v", token, err)
	}
	if err := os.Chmod(credentialPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateToken(credentialPath); err == nil {
		t.Fatal("group/world-readable credential passed launch preflight")
	}
}

func TestLaunchRequestsRejectInsecureRemoteEndpointsAndRedirects(t *testing.T) {
	if _, err := getLaunchBytes(
		context.Background(),
		newLaunchHttpClient(),
		"http://example.com",
		"/competition/readyz",
		"secret",
	); err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatalf("insecure remote launch endpoint error = %v", err)
	}

	redirectFollowed := false
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirectFollowed = true
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/credential-capture", http.StatusFound)
	}))
	defer origin.Close()
	if _, err := getLaunchBytes(
		context.Background(),
		newLaunchHttpClient(),
		origin.URL,
		"/competition/readyz",
		"secret",
	); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirecting launch endpoint error = %v", err)
	}
	if redirectFollowed {
		t.Fatal("launch client followed a credential-bearing redirect")
	}
}
