//go:build linux

// Launch binding must use the independently admitted endpoint and actual
// bounded candidate device, not merely a separately valid private engine.
package competitionagent

import "testing"

func TestCleanupRuntimeBindingRequiresExactAdmittedEndpointAndRoots(t *testing.T) {
	config := Config{DockerSocketPath: "/synthetic/runtime/docker.sock", WorkDirectory: "/synthetic/candidate/work",
		DockerRole: DockerRoleConfig{ScratchDirectory: "/synthetic/candidate/scratch"}}
	boundary := RuntimeResourceBoundary{PrivateSocketPath: config.DockerSocketPath, WorkRoot: config.WorkDirectory,
		ScratchRoot: config.DockerRole.ScratchDirectory, WorkDeviceId: 41}
	if err := validateCleanupRuntimeBinding(config, &boundary); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*RuntimeResourceBoundary){
		func(value *RuntimeResourceBoundary) { value.PrivateSocketPath = "/synthetic/other/docker.sock" },
		func(value *RuntimeResourceBoundary) { value.WorkRoot = "/synthetic/other/work" },
		func(value *RuntimeResourceBoundary) { value.ScratchRoot = "/synthetic/other/scratch" },
		func(value *RuntimeResourceBoundary) { value.WorkDeviceId = 0 },
	} {
		changed := boundary
		change(&changed)
		if err := validateCleanupRuntimeBinding(config, &changed); err == nil {
			t.Fatal("cleanup accepted a runtime other than its admitted private device and role")
		}
	}
	if err := validateCleanupRuntimeBinding(config, nil); err == nil {
		t.Fatal("cleanup accepted absent native admission")
	}
}
