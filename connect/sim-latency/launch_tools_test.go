package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchPreflightVerifiesRemoteEpochWithoutChangingWorkingTrees(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repositoryName := range []string{"connect", "sdk", "server", "proxy"} {
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
	sourceTestGit(t, serverRoot, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "advance branch")
	sourceTestGit(t, serverRoot, "push", "origin", "HEAD:sim-latency")
	if err := verifyRemoteSourceEpoch(manifest, 0, repositoriesRoot); err != nil {
		t.Fatalf("historical reachability failed after branch advance: %v", err)
	}
	if err := verifyRemoteSourceEpochHead(manifest, 0, repositoriesRoot); err == nil {
		t.Fatalf("active source preflight accepted branch ahead of ledger commit %s", serverHead)
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
			_, _ = response.Write([]byte(`[{"name":"sim-latency-support","address":"support@ur.xyz"}]`))
		case "/api/v1/provisioning/policies":
			_, _ = response.Write([]byte(`{"receiver":"sim-latency-support","matchers":["severity"]}`))
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
	commits := map[string]string{
		"connect": strings.Repeat("1", 40),
		"sdk":     strings.Repeat("2", 40),
		"server":  strings.Repeat("3", 40),
		"proxy":   strings.Repeat("4", 40),
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
