// Binds source operations to prospectively approved explicit staging policy.
// Environment names and local branch names never choose the execution mode.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// sourceManifestRepositoryPath isolates staging config from historical and
// active production source manifests while preserving config/main ordering.
func sourceManifestRepositoryPath(staging bool) string {
	if staging {
		return "main/sim-latency-staging.yml"
	}
	return "main/sim-latency.yml"
}

// validateSourceActivationMode rejects cross-mode recovery and timing drift
// before a source adapter or remote Git operation can run.
func validateSourceActivationMode(intent *model.CompetitionSourceActivation, settings *controller.Settings) error {
	if intent == nil || settings == nil || intent.Staging != settings.AutomationPolicy.Staging ||
		intent.StagingRoundWindowSeconds != settings.AutomationPolicy.StagingRoundWindowSeconds ||
		intent.StagingPreparationSeconds != settings.AutomationPolicy.StagingPreparationSeconds ||
		!validSourceMargin(intent.Threshold*100, intent.Staging) {
		return errors.New("source activation mode, timing, or margin differs from the frozen policy")
	}
	if intent.Staging {
		if intent.StagingRoundWindowSeconds < 60 || intent.StagingRoundWindowSeconds > 604800 ||
			intent.StagingPreparationSeconds < 0 || intent.StagingPreparationSeconds > 57600 {
			return errors.New("staging source activation timing is outside the approved bounds")
		}
	} else if intent.StagingRoundWindowSeconds != 0 || intent.StagingPreparationSeconds != 0 {
		return errors.New("production source activation cannot carry staging timing")
	}
	manifest, err := decodeSourceManifest(intent.Manifest)
	if err != nil || manifest.Schema != 2 || manifest.Staging != intent.Staging {
		return errors.New("source activation does not bind an exact same-mode v2 manifest")
	}
	digest := sha256.Sum256(intent.Manifest)
	if hex.EncodeToString(digest[:]) != intent.ManifestSha256 {
		return errors.New("source activation manifest digest does not match")
	}
	generation, err := manifest.generation(intent.SourceGeneration)
	if err != nil || generation.SourceLockSha256 != intent.SourceLockSha256 || generation.SignificantImprovementPercent != intent.Threshold*100 {
		return errors.New("source activation differs from its manifest generation and margin")
	}
	return nil
}
