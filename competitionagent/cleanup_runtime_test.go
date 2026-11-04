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
