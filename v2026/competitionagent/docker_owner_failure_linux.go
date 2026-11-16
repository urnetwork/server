// Future owner failures retain only a finite stage and a cause digest in root
// custody. These records cannot reconstruct a historical discarded failure.
package competitionagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Stage values are finite evidence-format identities, never error messages.
const (
	dockerOwnerFailureAuthority  = "execution_authority"
	dockerOwnerFailureMarker     = "attempt_marker"
	dockerOwnerFailureSocket     = "socket_authority"
	dockerOwnerFailureProxy      = "proxy_custody"
	dockerOwnerFailureRuntime    = "runtime_boundary"
	dockerOwnerFailureWorker     = "worker_listener"
	dockerOwnerFailureBuild      = "build_listener"
	dockerOwnerFailureDurability = "listener_durability"
	dockerOwnerFailureServe      = "serve"
)

// The original manifest/plan remain content-bound; no request, key, hidden seed,
// raw error or candidate output is copied into this diagnostic document.
type dockerOwnerFailure struct {
	Schema         int       `json:"schema"`
	Kind           string    `json:"kind"`
	RunId          string    `json:"run_id"`
	ManifestSha256 string    `json:"manifest_sha256"`
	PlanSha256     string    `json:"plan_sha256"`
	Stage          string    `json:"stage"`
	CauseSha256    string    `json:"cause_sha256"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// Reading a retained receipt uses the same finite format as writing one.
func validDockerOwnerFailureStage(stage string) bool {
	switch stage {
	case dockerOwnerFailureAuthority, dockerOwnerFailureMarker, dockerOwnerFailureSocket, dockerOwnerFailureProxy, dockerOwnerFailureRuntime,
		dockerOwnerFailureWorker, dockerOwnerFailureBuild, dockerOwnerFailureDurability, dockerOwnerFailureServe:
		return true
	default:
		return false
	}
}

// The first authentic future failure wins. A later denied restart cannot erase
// its earlier stage or manufacture a claim about a failure before this code ran.
func (self *Agent) recordDockerOwnerFailure(runId string, state *runState, stage string, cause error) error {
	if cause == nil || state == nil || !digestPattern.MatchString(runId) || !digestPattern.MatchString(state.ManifestSha256) || !digestPattern.MatchString(state.CleanupPlanSha256) {
		return errors.New("owner failure diagnostic has no authenticated scope")
	}
	if !validDockerOwnerFailureStage(stage) {
		return errors.New("owner failure diagnostic has no finite stage")
	}
	failure := dockerOwnerFailure{Schema: Schema, Kind: "competition-docker-owner-failure/v1", RunId: runId, ManifestSha256: state.ManifestSha256,
		PlanSha256: state.CleanupPlanSha256, Stage: stage, CauseSha256: digestDockerBytes([]byte(cause.Error())), RecordedAt: self.settings.Now().UTC()}
	content, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	path := filepath.Join(self.stateDirectory(runId), "docker-owner-failure.json")
	if err := exclusiveFile(path, content, 0400); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		prior, readErr := readBounded(path, self.settings.ConfigBytes)
		retained := dockerOwnerFailure{}
		if readErr != nil || legacyPrelaunchSealedFile(path, self.protected) != nil || validateDocumentKeys(prior) != nil || decodeStrict(prior, &retained) != nil || retained.Schema != Schema || retained.Kind != failure.Kind || retained.RunId != runId || retained.ManifestSha256 != state.ManifestSha256 || retained.PlanSha256 != state.CleanupPlanSha256 ||
			!validDockerOwnerFailureStage(retained.Stage) || !digestPattern.MatchString(retained.CauseSha256) || retained.RecordedAt.IsZero() {
			return errors.New("retained owner failure diagnostic differs from its exact run")
		}
		return nil
	}
	return syncDirectory(self.stateDirectory(runId))
}
