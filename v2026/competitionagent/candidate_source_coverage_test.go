//go:build linux

// The source producer's coverage field survives the private compiler boundary.
package competitionagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Write the actual wire field rather than relying on the consumer's Go struct.
func writeCandidateCoverageIdentity(t *testing.T, fixture *candidateSourceTest, policy string) {
	t.Helper()
	path := filepath.Join(fixture.source, ".evaluation-source.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(content, &object); err != nil {
		t.Fatal(err)
	}
	object["work_coverage_policy"] = json.RawMessage(policy)
	content, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	fixture.labels[candidateSourceLabelPrefix+"candidate-source-identity-sha256"] = digestDockerBytes(content)
}

// The original strict struct rejected every new coverage-enabled source graph.
func TestCandidateSourceBindingAcceptsProducerCoverageIdentity(t *testing.T) {
	for _, stage := range []string{"check", "compile"} {
		fixture := newCandidateSourceTest(t)
		writeCandidateCoverageIdentity(t, fixture, `{"kind":"seeded-crawl-coverage/v1","minimum_page_coverage_ratio":0.95,"minimum_byte_coverage_ratio":0.95}`)
		if err := fixture.trackedBuild(t, stage, nil); err != nil {
			t.Fatalf("producer coverage identity rejected for %s: %v", stage, err)
		}
	}
}

// Accepting the additional field must not turn off strict nested validation.
func TestCandidateSourceBindingRejectsInvalidCoverageIdentity(t *testing.T) {
	for _, policy := range []string{
		`null`, `true`, `[]`, `{}`,
		`{"kind":"other/v1","minimum_page_coverage_ratio":0.95,"minimum_byte_coverage_ratio":0.95}`,
		`{"kind":"seeded-crawl-coverage/v1","minimum_page_coverage_ratio":0.9,"minimum_byte_coverage_ratio":0.95}`,
		`{"kind":"seeded-crawl-coverage/v1","minimum_page_coverage_ratio":0.95,"minimum_byte_coverage_ratio":1}`,
		`{"kind":"seeded-crawl-coverage/v1","minimum_page_coverage_ratio":0.95,"minimum_byte_coverage_ratio":0.95,"extra":true}`,
		`{"kind":"seeded-crawl-coverage/v1","minimum_page_coverage_ratio":0.95,"minimum_page_coverage_ratio":0.95,"minimum_byte_coverage_ratio":0.95}`,
	} {
		fixture := newCandidateSourceTest(t)
		writeCandidateCoverageIdentity(t, fixture, policy)
		if err := fixture.trackedBuild(t, "check", nil); err == nil {
			t.Fatalf("malformed coverage policy accepted: %s", policy)
		}
	}
}
