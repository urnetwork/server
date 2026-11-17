// A scoped operator can approve a proved evaluator incident without turning
// every unstable or timed-out submission into an automatic resampling chance.
package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/urnetwork/server/model"
)

// Diagnostic reads have a per-invocation bound, independent of evaluation time.
type competitionReevaluationEvidenceSettings struct {
	// Maximum retained log bytes inspected; non-positive refuses inspection.
	MaximumLogBytes int64
}

// Large logs fail closed rather than allocating an unbounded operator input.
func defaultCompetitionReevaluationEvidenceSettings() competitionReevaluationEvidenceSettings {
	return competitionReevaluationEvidenceSettings{MaximumLogBytes: 8 * 1024 * 1024}
}

// An exact terminal attempt and candidate namespace prevent another job or a
// baseline log from being offered as evidence for a submitted patch's failure.
func validateCompetitionReevaluationBackingServiceShape(settings *Settings, a model.CompetitionJobReevaluationAuthorization) error {
	proof := a.BackingServiceEvidence
	if settings == nil || proof == nil || proof.Schema != 1 || proof.Service != "postgresql" || proof.ArtifactBytes <= 0 ||
		!sha256Pattern.MatchString(proof.ArtifactSha256) || a.IncidentAttempt != a.ExpectedAttemptCount ||
		a.IncidentAttempt < 1 || a.FailureManifestSha256 != "" || a.RejectedArtifactPath != "" {
		return ErrConflict
	}
	for replicate := 1; replicate <= settings.EvaluationPolicy.Replicates; replicate++ {
		if proof.ArtifactPath == fmt.Sprintf("evidence/scorer-input/candidate-%02d-%s/stderr.log", replicate, a.JobId.String()[:8]) {
			return nil
		}
	}
	return ErrConflict
}

// A matching message proves only retained diagnostic content. The independent
// operator dossier must establish cause and patch honesty before authorization.
func validateCompetitionReevaluationBackingService(settings *Settings, a model.CompetitionJobReevaluationAuthorization, limits competitionReevaluationEvidenceSettings) error {
	if validateCompetitionReevaluationBackingServiceShape(settings, a) != nil || limits.MaximumLogBytes <= 0 ||
		a.BackingServiceEvidence.ArtifactBytes > limits.MaximumLogBytes {
		return ErrConflict
	}
	proof := a.BackingServiceEvidence
	root := filepath.Join(settings.ArtifactRoot, a.JobId.String(), fmt.Sprintf("attempt-%02d", a.IncidentAttempt))
	content, artifact, err := readFailureArtifact(root, proof.ArtifactPath, limits.MaximumLogBytes)
	if err != nil || artifact.Sha256 != proof.ArtifactSha256 || int64(len(content)) != proof.ArtifactBytes ||
		!competitionReevaluationHasBackingServiceRecovery(content, limits) {
		return ErrConflict
	}
	return nil
}

// Exactly one recovered panic must identify the known PostgreSQL statement
// cleanup timeout and database stack in the same structured record. A second
// panic, malformed record or mixed product failure is not silently forgiven.
func competitionReevaluationHasBackingServiceRecovery(content []byte, limits competitionReevaluationEvidenceSettings) bool {
	if limits.MaximumLogBytes <= 0 || int64(len(content)) > limits.MaximumLogBytes {
		return false
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(nil, len(content)+1)
	recoveries, matched := 0, 0
	for scanner.Scan() {
		line := scanner.Text()
		at := strings.Index(line, "Unexpected error:")
		if at < 0 {
			continue
		}
		recoveries++
		encoded := []byte(strings.TrimSpace(line[at+len("Unexpected error:"):]))
		var recovery struct {
			Error string   `json:"error"`
			Stack []string `json:"stack"`
		}
		if _, err := CompetitionJobReevaluationSnapshotDigest(encoded); err != nil || json.Unmarshal(encoded, &recovery) != nil {
			return false
		}
		if !strings.HasPrefix(recovery.Error, "*fmt.wrapError=failed to deallocate cached statement(s): timeout:") ||
			!strings.HasSuffix(recovery.Error, "i/o timeout") {
			continue
		}
		for _, frame := range recovery.Stack {
			if strings.HasPrefix(frame, "github.com/urnetwork/server.dbWithPool(") {
				matched++
				break
			}
		}
	}
	return scanner.Err() == nil && recoveries == 1 && matched == 1
}
