package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// update refreshes every data set the ARIN resource depends on, as far as the
// upstream sources allow, and publishes one bundle:
//
//	mmdb/                   GeoLite2 resource (as refresh)
//	arindb/                 final arindb resource: arin.mmdb, manifest.json,
//	                        registration-manifest.json, update-manifest.json
//	subscriber-evidence/    not a resource: the refreshed catalog and its
//	                        pinned snapshots, the registration base and the
//	                        catalog audit, kept for review and reproduction
//	update-summary.txt      one line for release notifications
//
// GeoLite2, ARIN and, when a reviewed operator catalog is supplied, the RIS
// origin snapshots are required: without them nothing is published, as with
// refresh. Every other evidence source is best effort. A source that cannot be
// downloaded or fails its own validation is left out and named in the update
// manifest; the build never substitutes stale or unvalidated data for it.
// Without a catalog the result is the registration database alone, which the
// manifest states, because publishing it replaces an augmented resource.
type updateUnavailable struct {
	Id    string `json:"id"`
	Url   string `json:"url"`
	Error string `json:"error"`
}

// Sources the augmentation cannot run without.
var subscriberEvidenceRequired = []string{"ris-ipv4", "ris-ipv6"}

func runUpdate(ctx context.Context, options commandOptions, dependencies commandDependencies, stage string) error {
	if options.geoipConfig == "" || options.credentials == "" || options.rules == "" {
		return errors.New("update requires geoip-config, credentials and the reviewed registration rules")
	}
	started := dependencies.clock()
	geoDir := filepath.Join(stage, "mmdb")
	arinDir := filepath.Join(stage, "arindb")
	evidenceDir := filepath.Join(stage, "subscriber-evidence")
	for _, dir := range []string{geoDir, arinDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	}
	if err := refreshGeolite2(ctx, options.geoipConfig, geoDir, dependencies.geoipUpdate); err != nil {
		return fmt.Errorf("GeoLite2 refresh: %w", err)
	}
	geolite2 := filepath.Join(geoDir, "geolite2.mmdb")
	raw, err := os.CreateTemp(stage, ".arin-source-*.xml")
	if err != nil {
		return err
	}
	rawPath := raw.Name()
	if err := raw.Close(); err != nil {
		return err
	}
	defer os.Remove(rawPath)
	if err := refreshArin(ctx, options.credentials, rawPath, dependencies.arinClient); err != nil {
		return fmt.Errorf("ARIN refresh: %w", err)
	}
	manifest := map[string]any{"source": "arindbctl update", "started_at": started, "registration_rules": options.rules}
	if options.subscriberCatalog == "" {
		if err := buildArinDatabase(ctx, rawPath, geolite2, options.rules, arinDir); err != nil {
			return fmt.Errorf("registration build: %w", err)
		}
		manifest["subscriber_augmentation"] = "skipped: no reviewed subscriber catalog was supplied; this resource has registration evidence only"
		return finishUpdate(stage, arinDir, manifest, "registration only (no subscriber catalog); subscriber augmentation skipped")
	}
	catalogHash, err := hashArinBuildInput(ctx, options.subscriberCatalog)
	if err != nil {
		return err
	}
	manifest["subscriber_catalog"] = map[string]string{"path": options.subscriberCatalog, "sha256": catalogHash}
	registrationDir := filepath.Join(evidenceDir, "registration")
	catalogDir := filepath.Join(evidenceDir, "catalog")
	auditDir := filepath.Join(evidenceDir, "audit")
	for _, dir := range []string{evidenceDir, registrationDir, catalogDir, auditDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	}
	if err := buildArinDatabase(ctx, rawPath, geolite2, options.rules, registrationDir); err != nil {
		return fmt.Errorf("registration build: %w", err)
	}
	evidence := options.evidence
	evidence.LabelSources, evidence.HostingPrefixes, evidence.RegistryAssignments, evidence.VpnServers = true, true, true, true
	unavailable, err := pinSubscriberEvidence(ctx, options.subscriberCatalog, evidence, true, catalogDir, dependencies.evidenceClient, dependencies.clock())
	if err != nil {
		return fmt.Errorf("subscriber evidence: %w", err)
	}
	manifest["unavailable_evidence"] = unavailable
	refreshed := filepath.Join(catalogDir, "catalog.yml")
	catalog, err := loadSubscriberOriginCatalog(refreshed)
	if err != nil {
		return fmt.Errorf("refreshed subscriber catalog: %w", err)
	}
	countryGeolite := ""
	if catalog.OriginCountryPolicy != "" {
		countryGeolite = geolite2
	}
	// The audit is a review aid: its failure is recorded, not fatal.
	if err := auditSubscriberCatalog(ctx, refreshed, geolite2, auditDir, dependencies.clock()); err != nil {
		manifest["catalog_audit"] = "failed: " + err.Error()
	} else {
		manifest["catalog_audit"] = "subscriber-evidence/audit/catalog-audit.json"
	}
	if err := augmentSubscriberDatabase(ctx, filepath.Join(registrationDir, "arin.mmdb"), refreshed, countryGeolite, arinDir, dependencies.clock()); err != nil {
		return fmt.Errorf("subscriber augmentation: %w", err)
	}
	validation := validateUpdate(ctx, dependencies, filepath.Join(arinDir, "arin.mmdb"), filepath.Join(evidenceDir, "validation"))
	manifest["validation"] = validation
	registrationManifest, err := os.ReadFile(filepath.Join(registrationDir, "manifest.json"))
	if err != nil {
		return err
	}
	if err := writeSyncedFile(filepath.Join(arinDir, "registration-manifest.json"), registrationManifest); err != nil {
		return err
	}
	manifest["subscriber_augmentation"] = "applied"
	summary := "registration and subscriber augmentation applied"
	if line, ok := validation["summary"].(string); ok {
		summary += "; " + line
	}
	if len(unavailable) != 0 {
		ids := []string{}
		for _, source := range unavailable {
			ids = append(ids, source.Id)
		}
		summary += "; unavailable evidence: " + strings.Join(ids, ", ")
	}
	return finishUpdate(stage, arinDir, manifest, summary)
}

// Validation is best effort: the Atlas archive may be unavailable, and the
// estimate never blocks a release. Its outcome is recorded either way.
func validateUpdate(ctx context.Context, dependencies commandDependencies, database, output string) map[string]any {
	if err := os.Mkdir(output, 0o755); err != nil {
		return map[string]any{"status": "failed", "error": err.Error()}
	}
	client := http.Client{Timeout: 10 * time.Minute}
	if dependencies.evidenceClient != nil {
		client = *dependencies.evidenceClient
	}
	at := dependencies.clock()
	download := subscriberEvidenceDownload{id: "atlas-probes", url: subscriberEvidenceAtlasProbes, file: "atlas-probes.json.bz2",
		validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
			_, err := readAtlasProbes(ctx, reader)
			return at, err
		}}
	digest, _, err := fetchSubscriberEvidence(ctx, client, download, output, at)
	if err != nil {
		return map[string]any{"status": "unavailable", "error": err.Error()}
	}
	if err := validateArinDatabase(ctx, database, filepath.Join(output, download.file), output, at); err != nil {
		return map[string]any{"status": "failed", "error": err.Error(), "atlas_sha256": digest}
	}
	line, err := readValidationSummary(output)
	if err != nil {
		return map[string]any{"status": "failed", "error": err.Error(), "atlas_sha256": digest}
	}
	return map[string]any{"status": "completed", "report": "subscriber-evidence/validation/validation.json", "atlas_sha256": digest, "summary": line}
}

func finishUpdate(stage, arinDir string, manifest map[string]any, summary string) error {
	manifest["finished_at"] = time.Now().UTC()
	manifest["summary"] = summary
	manifest["builder_version"] = Version
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSyncedFile(filepath.Join(arinDir, "update-manifest.json"), append(content, '\n')); err != nil {
		return err
	}
	return writeSyncedFile(filepath.Join(stage, "update-summary.txt"), []byte(summary+"\n"))
}
