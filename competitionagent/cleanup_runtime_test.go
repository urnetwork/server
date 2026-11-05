//go:build linux

// Default-context admission tests contain only synthetic metadata. They never
// read an operator's Docker configuration or contact a container daemon.
package competitionagent

import "testing"

// Local default metadata is accepted without retaining unrelated credential
// material. Registry fields cannot influence the privileged cleanup route.
func TestCleanupRuntimeAcceptsOnlyDefaultLocalDockerContext(t *testing.T) {
	for _, content := range []string{`{}`, `{"currentContext":""}`, `{"currentContext":"default","aliases":{}}`,
		`{"auths":{"synthetic.invalid":{"auth":"synthetic-not-a-credential"}},"currentContext":"default"}`} {
		if err := validateDefaultDockerContext([]byte(content)); err != nil {
			t.Fatal("fixed default Docker metadata was rejected", err)
		}
	}
}

// A mismatched context must fail before evaluation; an absent guessed build
// slice on the cleanup daemon is never proof about another daemon's workload.
func TestCleanupRuntimeRejectsRemoteAmbiguousAndAliasedDockerContext(t *testing.T) {
	for _, content := range []string{`[]`, `null`, `{"currentContext":"synthetic-other"}`, `{"CurrentContext":"default"}`,
		`{"currentContext":"default","currentContext":"synthetic-other"}`, `{"currentContext":"default","CurrentContext":"default"}`,
		`{"currentContext":null}`, `{"currentContext":3}`, `{"aliases":{"builder":"synthetic-remote-builder"}}`,
		`{"aliases":[]}`, `{"currentContext":"default"} {}`, `{"currentContext":`} {
		if err := validateDefaultDockerContext([]byte(content)); err == nil {
			t.Fatal("expanded Docker cleanup authority was admitted", content)
		}
	}
}

// Docker's cgroupfs driver also supports v2, but its path layout is not the
// systemd slice expansion whose exact targets this cleanup implementation owns.
func TestCleanupRuntimeRequiresActualSystemdCgroupV2Driver(t *testing.T) {
	for _, report := range []string{"systemd 2", "systemd 2\n"} {
		if err := validateCleanupDaemonCgroups([]byte(report)); err != nil {
			t.Fatal("actual qualified systemd hierarchy was refused", err)
		}
	}
	for _, report := range []string{"", "cgroupfs 2", "systemd 1", "systemd 2 extra", "systemd", "2"} {
		if err := validateCleanupDaemonCgroups([]byte(report)); err == nil {
			t.Fatal("v2 alone or unknown daemon hierarchy was accepted", report)
		}
	}
}
