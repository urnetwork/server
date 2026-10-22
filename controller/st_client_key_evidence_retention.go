// Client-key evidence keeps only its content route. The history copies written
// before those kinds left the history index expire through the code-owned
// bucket lifecycle; no other rule or prefix changes.
package controller

import (
	"math"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// Nothing reads the client-key history copies (startifact.EvidenceHistoryIndexed),
// so they expire as soon as the store's lifecycle allows: MinIO rounds up to
// one whole day.
const stClientKeyEvidenceHistoryTtl = 24 * time.Hour

// One rule per kind outside the history index, for the configured deployment's
// run-less history. Each prefix ends in that kind's own run directory, so a rule
// never reaches the content route, another kind, or another deployment. Client-
// key evidence began after the run-less sentinel replaced the legacy run word,
// so the legacy directory holds none of it.
func stClientKeyEvidenceHistoryLifecycleRules(store server.BlobStore) []server.BlobLifecycleRule {
	cfg := stConfig()
	if store == nil || cfg == nil || cfg.Netuid == 0 || math.MaxUint16 < cfg.Netuid {
		return nil
	}
	kinds := startifact.EvidenceHistoryUnindexedKinds()
	rules := make([]server.BlobLifecycleRule, 0, len(kinds))
	for _, kind := range kinds {
		prefix, err := startifact.EvidenceHistoryRunPrefix(store, cfg.DeploymentId, uint16(cfg.Netuid), kind, startifact.EvidenceDeploymentHistoryRunID)
		if err != nil {
			return nil
		}
		rules = append(rules, server.BlobLifecycleRule{KeyPrefix: prefix, TTL: stClientKeyEvidenceHistoryTtl})
	}
	return rules
}
