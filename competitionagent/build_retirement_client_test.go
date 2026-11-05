//go:build linux

// The public fixed client sees only fake Unix HTTP and finite proof values.
// It cannot pass a cgroup, ceiling, command or path to build retirement.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRetirementClientUsesNoArgumentRoleAndRejectsExpandedProof(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	listener, err := net.Listen("unix", filepath.Join(fixture.config.SocketDirectory, fixture.runId, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan HostFilesResult, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body HostFilesRequest
		if request.Method != http.MethodPost || request.URL.Path != "/_urnetwork/host-files" || json.NewDecoder(request.Body).Decode(&body) != nil ||
			body.Schema != Schema || body.RunId != fixture.runId || body.Operation != "retire-build" || body.Path != "" || body.ContainerId != "" || body.NetworkId != "" {
			t.Error("retirement client expanded the fixed native role")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case result := <-results:
			_ = json.NewEncoder(writer).Encode(result)
		case <-request.Context().Done():
		}
	})}
	joined := make(chan error, 1)
	go func() { joined <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-joined })
	proof := BuildRetirementSummary{OutcomeKind: "acknowledged-build", RetainedMemoryMaximumBytes: 4096, MemoryCurrentBytes: 128,
		MemoryStatSha256: strings.Repeat("a", 64), ReceiptSha256: strings.Repeat("b", 64)}
	for _, change := range []struct {
		valid bool
		apply func(*HostFilesResult)
	}{
		{valid: true, apply: func(*HostFilesResult) {}},
		{valid: true, apply: func(value *HostFilesResult) { value.BuildRetirement.OutcomeKind = "no-build-issued" }},
		{apply: func(value *HostFilesResult) { value.BuildRetirement = nil }},
		{apply: func(value *HostFilesResult) { value.BuildRetirement.OutcomeKind = "synthetic-unknown" }},
		{apply: func(value *HostFilesResult) { value.BuildRetirement.RetainedMemoryMaximumBytes = 0 }},
		{apply: func(value *HostFilesResult) { value.BuildRetirement.MemoryCurrentBytes = 4097 }},
		{apply: func(value *HostFilesResult) { value.BuildRetirement.MemoryStatSha256 = "synthetic-missing" }},
		{apply: func(value *HostFilesResult) { value.BuildRetirement.ReceiptSha256 = "synthetic-missing" }},
		{apply: func(value *HostFilesResult) { value.ErrorCode = "synthetic-unknown" }},
		{apply: func(value *HostFilesResult) { value.Namespace = &HostNamespaceMap{} }},
		{apply: func(value *HostFilesResult) { value.Network = &HostNetworkBoundary{} }},
	} {
		copy := proof
		result := HostFilesResult{RunId: fixture.runId, Operation: "retire-build", Acknowledged: true, BuildRetirement: &copy}
		change.apply(&result)
		results <- result
		var output bytes.Buffer
		err := runHostFilesClient(t.Context(), fixture.config, fixture.runId, []string{"retire-build"}, &output, fixture.settings)
		if change.valid {
			var actual BuildRetirementSummary
			if err != nil || json.Unmarshal(output.Bytes(), &actual) != nil || actual != *result.BuildRetirement {
				t.Fatal("finite native retirement proof was lost", err)
			}
		} else if DockerClientExitStatus(err) != 2 || output.Len() != 0 {
			t.Fatal("unknown or expanded native proof reached the trusted helper", err)
		}
	}
	for _, arguments := range [][]string{{"retire-build", "/synthetic/cgroup"}, {"retire-build", "8192"}, {"retire-build", "--config", "/synthetic/config"}} {
		if err := runHostFilesClient(t.Context(), fixture.config, fixture.runId, arguments, &bytes.Buffer{}, fixture.settings); DockerClientExitStatus(err) != 2 {
			t.Fatal("retirement accepted caller-selected native authority", arguments)
		}
	}
}
