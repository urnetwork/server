//go:build linux

// The trusted client tests use only private scripts/files and fake Unix HTTP
// endpoints. No daemon, privileged account, config/vault or service is accessed.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type dockerClientTestFixture struct {
	config   *DockerClientConfig
	settings dockerClientSettings
	runId    string
	public   string
}

func newDockerClientTestFixture(t *testing.T) *dockerClientTestFixture {
	t.Helper()
	// Unix sockaddr_un is short even when the harness's TMPDIR is long.
	// This fresh0700 leaf is owned solely by this test and removed after join.
	root, err := os.MkdirTemp("/tmp", "dc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error("private Docker client fixture cleanup failed", err)
		}
	})
	release, base := filepath.Join(root, "release"), filepath.Join(root, "r")
	for _, path := range []string{release, filepath.Join(base, "jobs"), filepath.Join(base, "clients"), filepath.Join(release, "docker-client-config")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	agent, docker, compose := filepath.Join(release, "agent"), filepath.Join(release, "docker"), filepath.Join(release, "docker-compose")
	for _, path := range []string{agent, docker, compose} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := &DockerClientConfig{Schema: Schema, Agent: testAgentPin(t, agent), Docker: testAgentPin(t, docker), Compose: testAgentPin(t, compose),
		BaseDirectory: base, WorkDirectory: filepath.Join(base, "jobs"), SocketDirectory: filepath.Join(base, "clients"),
		DockerConfigurationDirectory: filepath.Join(release, "docker-client-config"), WorkerUid: 12345, TrustedOperatorUid: 12346}
	content, err := json.Marshal(map[string]any{"cliPluginsExtraDirs": []string{release}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.DockerConfigurationDirectory, "config.json"), content, 0600); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(release, "docker-client.json")
	writeDockerClientTestConfig(t, public, config)
	settings := defaultDockerClientSettings()
	settings.currentUid = func() uint32 { return config.WorkerUid }
	runId := strings.Repeat("a", 64)
	for _, path := range []string{filepath.Join(config.WorkDirectory, runId), filepath.Join(config.SocketDirectory, runId)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return &dockerClientTestFixture{config: config, settings: settings, runId: runId, public: public}
}

func writeDockerClientTestConfig(t *testing.T, path string, config *DockerClientConfig) {
	t.Helper()
	content, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

// Only the own executable's public sibling can supply immutable local roots.
func TestDockerClientLoadsOnlyBoundPublicSiblingMetadata(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	loaded, err := loadDockerClientConfig(fixture.config.Agent.Path, fixture.settings, false)
	if err != nil || !reflect.DeepEqual(loaded, fixture.config) {
		t.Fatal("fixed public role was not retained exactly", err)
	}
	fixture.config.SocketDirectory = filepath.Join(fixture.config.BaseDirectory, "other-socket")
	writeDockerClientTestConfig(t, fixture.public, fixture.config)
	if _, err := loadDockerClientConfig(fixture.config.Agent.Path, fixture.settings, false); err == nil {
		t.Fatal("public role admitted another endpoint namespace")
	}
}

// A root/public document cannot accidentally accept private signing authority,
// duplicate keys or typed JSON's case-folded aliases.
func TestDockerClientRejectsPrivateOrAmbiguousMetadata(t *testing.T) {
	for _, mutation := range []string{",\"signing_key_path\":\"synthetic-private-key\"", ",\"worker_uid\":12345", ",\"WORKER_UID\":12345"} {
		fixture := newDockerClientTestFixture(t)
		content, err := os.ReadFile(fixture.public)
		if err != nil {
			t.Fatal(err)
		}
		content = append(append(content[:len(content)-1:len(content)-1], []byte(mutation)...), '}')
		if err := os.WriteFile(fixture.public, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadDockerClientConfig(fixture.config.Agent.Path, fixture.settings, false); err == nil {
			t.Fatal("private or noncanonical authority entered the public client")
		}
	}
}

// The dedicated Docker config is not a user's credential/context/plugin store.
func TestDockerClientRejectsCredentialContextAndPluginExpansion(t *testing.T) {
	for _, content := range []string{
		`{"auths":{},"cliPluginsExtraDirs":[]}`, `{"currentContext":"synthetic-remote"}`, `{"cliPluginsExtraDirs":[],"proxies":{}}`,
		`{"cliPluginsExtraDirs":[],"cliPluginsExtraDirs":[]}`, `{"CLIPLUGINSEXTRADIRS":[]}`,
	} {
		fixture := newDockerClientTestFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.config.DockerConfigurationDirectory, "config.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateDockerClientConfiguration(fixture.config, fixture.settings, false); err == nil {
			t.Fatal("Docker config expanded its fixed credential-free plugin authority")
		}
	}
	fixture := newDockerClientTestFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.config.DockerConfigurationDirectory, "cli-plugins"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerClientConfiguration(fixture.config, fixture.settings, false); err == nil {
		t.Fatal("higher-priority mutable plugin directory was accepted")
	}
}

// Endpoint/config flags cannot override the fixed UDS prefix, even when placed
// after an ordinary subcommand. Unknown top-level capabilities are also denied.
func TestDockerClientRejectsEndpointAndCapabilityOverrides(t *testing.T) {
	settings := defaultDockerClientSettings()
	for _, arguments := range [][]string{{"context", "use", "other"}, {"login"}, {"buildx", "build"},
		{"info", "--host=tcp://synthetic"}, {"info", "--host", "unix:///other"}, {"info", "-Hunix:///other"},
		{"info", "--context=other"}, {"info", "-cother"}, {"info", "--config", "/other"}, {"info", "synthetic\nargument"}} {
		if err := validateDockerClientArguments(arguments, settings); err == nil {
			t.Fatal("restricted helper accepted endpoint/config/capability expansion", arguments)
		}
	}
	for _, arguments := range [][]string{{"info"}, {"compose", "--file", "/synthetic/compose.yml", "ps"}, {"run", "--cap-drop", "ALL", "--cgroup-parent", "synthetic.slice", "image"}} {
		if err := validateDockerClientArguments(arguments, settings); err != nil {
			t.Fatal("closed existing helper vocabulary was refused", err)
		}
	}
}

// The actual private subprocess receives only fixed role environment/argv,
// never database, vault, SSH-agent, Docker auth or caller-selected endpoint data.
func TestDockerClientExecStripsEnvironmentAndPinsUnixEndpoint(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	program := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\"\nprintf 'home=%s\\n' \"$HOME\"\nenv\n"
	if err := os.WriteFile(fixture.config.Docker.Path, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGPASSWORD", "synthetic-host-secret")
	t.Setenv("DOCKER_HOST", "synthetic-host-secret")
	t.Setenv("DOCKER_AUTH_CONFIG", "synthetic-host-secret")
	t.Setenv("SSH_AUTH_SOCK", "synthetic-host-secret")
	var output, diagnostic bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := runDockerClient(ctx, fixture.config, fixture.runId, []string{"info"}, nil, &output, &diagnostic, fixture.settings); err != nil {
		t.Fatal(err)
	}
	expectedSocket := "unix://" + filepath.Join(fixture.config.SocketDirectory, fixture.runId, "docker.sock")
	if !strings.HasPrefix(output.String(), "--host\n"+expectedSocket+"\n--config\n"+fixture.config.DockerConfigurationDirectory+"\ninfo\n") ||
		!strings.Contains(output.String(), "DOCKER_HOST="+expectedSocket+"\n") || strings.Contains(output.String(), "synthetic-host-secret") {
		t.Fatal("client did not retain its fresh environment and fixed endpoint")
	}
	fixture.settings.currentUid = func() uint32 { return fixture.config.WorkerUid + 1 }
	if err := runDockerClient(ctx, fixture.config, fixture.runId, []string{"info"}, nil, &output, &diagnostic, fixture.settings); DockerClientExitStatus(err) != 2 {
		t.Fatal("another host uid entered the fixed worker role")
	}
}

// Only the typed RPC can start a build. An authenticated complete compiler
// failure is still acknowledged and keeps its exact submission-level status.
func TestDockerClientBuildUsesTypedOwnerAndPreservesAcknowledgedFailure(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	socket := filepath.Join(fixture.config.SocketDirectory, fixture.runId, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"build", "--network", "none", filepath.Join(fixture.config.WorkDirectory, fixture.runId, "synthetic-context")}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body DockerBuildRequest
		if request.Method != http.MethodPost || request.URL.Path != "/_urnetwork/build" || json.NewDecoder(request.Body).Decode(&body) != nil ||
			body.RunId != fixture.runId || !reflect.DeepEqual(body.Arguments, arguments) || body.ContextDirectory != arguments[len(arguments)-1] {
			t.Error("build escaped its finite typed command boundary")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(DockerBuildResult{RunId: fixture.runId, Acknowledged: true, ExitCode: 1, Stdout: []byte("synthetic-output"), Stderr: []byte("synthetic-compiler-refusal")})
	})}
	joined := make(chan error, 1)
	go func() { joined <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-joined })
	var output, diagnostic bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = runDockerClient(ctx, fixture.config, fixture.runId, arguments, nil, &output, &diagnostic, fixture.settings)
	if DockerClientExitStatus(err) != 1 || output.String() != "synthetic-output" || diagnostic.String() != "synthetic-compiler-refusal" {
		t.Fatal("complete compiler failure was mistaken for unknown infrastructure custody", err)
	}
	_ = server.Close()
	if err := runDockerClient(ctx, fixture.config, fixture.runId, arguments, nil, &output, &diagnostic, fixture.settings); DockerClientExitStatus(err) != 2 {
		t.Fatal("unreachable build owner was mistaken for definite compiler refusal")
	}
}

type dockerClientShortWriter struct{}

func (dockerClientShortWriter) Write(content []byte) (int, error) { return len(content) - 1, nil }

func TestDockerClientOutputRequiresCompleteWrite(t *testing.T) {
	if err := writeDockerClientBytes(dockerClientShortWriter{}, []byte("synthetic")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short compiler output was silently accepted")
	}
}

// A finite host operation uses the same pinned local endpoint without loading
// private role metadata, expanding command authority or accepting unknown work.
func TestHostFilesClientUsesTypedOwnerAndReturnsOnlyNamespaceProof(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	socket := filepath.Join(fixture.config.SocketDirectory, fixture.runId, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	containerId := strings.Repeat("c", 64)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := HostFilesRequest{}
		if r.Method != http.MethodPost || r.URL.Path != "/_urnetwork/host-files" || json.NewDecoder(r.Body).Decode(&request) != nil || request.RunId != fixture.runId ||
			request.Operation != "verify-id-map" || request.ContainerId != containerId || request.Path != "" {
			t.Error("host client expanded its typed namespace role")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(HostFilesResult{RunId: fixture.runId, Operation: request.Operation, Acknowledged: true, ExitCode: 0,
			Namespace: &HostNamespaceMap{ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536, UidMapSha256: strings.Repeat("a", 64), GidMapSha256: strings.Repeat("b", 64)}})
	})}
	joined := make(chan error, 1)
	go func() { joined <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-joined })
	var output bytes.Buffer
	if err := runHostFilesClient(t.Context(), fixture.config, fixture.runId, []string{"verify-id-map", containerId}, &output, fixture.settings); err != nil {
		t.Fatal(err)
	}
	var proof HostNamespaceMap
	if json.Unmarshal(output.Bytes(), &proof) != nil || proof.ContainerRootUid != 200000 || proof.UidMapSha256 != strings.Repeat("a", 64) {
		t.Fatal("host client lost its finite namespace proof")
	}
	_ = server.Close()
	if err := runHostFilesClient(t.Context(), fixture.config, fixture.runId, []string{"mount-evidence"}, &output, fixture.settings); DockerClientExitStatus(err) != 2 {
		t.Fatal("unreachable host owner was mistaken for acknowledged mutation")
	}
}

// Local validation rejects endpoint/config/root-command expansion before even
// opening a socket, and changing host uid cannot borrow the worker role.
func TestHostFilesClientRejectsExpandedArgumentsAndWorkerIdentity(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	for _, arguments := range [][]string{nil, {"/bin/sh"}, {"mount-evidence", "/synthetic/alternate"}, {"verify-id-map", "short"},
		{"prepare-output", "/synthetic/outside"}, {"reclaim-output", filepath.Join(fixture.config.WorkDirectory, fixture.runId, ".evidence-runtime") + "/../worker-request.json"},
		{"mount-evidence", "--config", "/synthetic/private.json"}} {
		if err := runHostFilesClient(t.Context(), fixture.config, fixture.runId, arguments, io.Discard, fixture.settings); DockerClientExitStatus(err) != 2 {
			t.Fatal("expanded host arguments were accepted", arguments)
		}
	}
	fixture.settings.currentUid = func() uint32 { return fixture.config.WorkerUid + 1 }
	if err := runHostFilesClient(t.Context(), fixture.config, fixture.runId, []string{"mount-evidence"}, io.Discard, fixture.settings); DockerClientExitStatus(err) != 2 {
		t.Fatal("another uid borrowed the host worker role")
	}
}
