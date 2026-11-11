// Future owner stage diagnostics remain root metadata, not scoring evidence or
// a reconstruction of any earlier discarded error.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDockerOwnerFailureKeepsFiniteStageAndDigestWithoutRawCause(t *testing.T) {
	fixture, _, authorization, _, _ := newLegacyPrelaunchTestFixture(t)
	if fixture.agent.RunDockerOwner(t.Context(), authorization.RunId) == nil {
		t.Fatal("recovery-held owner unexpectedly executed")
	}
	path := filepath.Join(fixture.agent.stateDirectory(authorization.RunId), "docker-owner-failure.json")
	content, err := os.ReadFile(path)
	failure := dockerOwnerFailure{}
	if err != nil || decodeStrict(content, &failure) != nil || failure.Stage != dockerOwnerFailureAuthority || failure.RunId != authorization.RunId || failure.ManifestSha256 != authorization.ManifestSha256 || failure.PlanSha256 != authorization.CleanupPlanSha256 || !digestPattern.MatchString(failure.CauseSha256) {
		t.Fatal("genuine future failure omitted finite authenticated diagnostics", err)
	}
	state, err := fixture.agent.loadState(authorization.RunId)
	if err != nil {
		t.Fatal(err)
	}
	privateCause := "synthetic-private-cause-not-for-diagnostics"
	if err := fixture.agent.recordDockerOwnerFailure(authorization.RunId, state, dockerOwnerFailureProxy, errors.New(privateCause)); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(content, retained) || bytes.Contains(retained, []byte(privateCause)) || fixture.runner.launchCount() != 1 {
		t.Fatal("later failure overwrote the first stage, exposed cause or replayed work", err)
	}
	if err := legacyPrelaunchSealedFile(path, false); err != nil {
		t.Fatal("future diagnostic was not sealed root-style metadata", err)
	}
}

func TestDockerOwnerFailureRejectsUnknownStageAndCorruptRetainedEvidence(t *testing.T) {
	fixture, _, authorization, _, _ := newLegacyPrelaunchTestFixture(t)
	state, err := fixture.agent.loadState(authorization.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.agent.recordDockerOwnerFailure(authorization.RunId, state, "raw-error-as-stage", errors.New("synthetic cause")) == nil {
		t.Fatal("arbitrary diagnostic stage was accepted")
	}
	if err := fixture.agent.recordDockerOwnerFailure(authorization.RunId, state, dockerOwnerFailureMarker, errors.New("synthetic cause")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.agent.stateDirectory(authorization.RunId), "docker-owner-failure.json")
	content, err := os.ReadFile(path)
	failure := dockerOwnerFailure{}
	if err != nil || decodeStrict(content, &failure) != nil {
		t.Fatal(err)
	}
	failure.Stage = "unknown-unbounded-stage"
	content, err = json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, path, content)
	if fixture.agent.recordDockerOwnerFailure(authorization.RunId, state, dockerOwnerFailureProxy, errors.New("synthetic later cause")) == nil {
		t.Fatal("corrupt retained failure evidence was treated as authentic")
	}
}
