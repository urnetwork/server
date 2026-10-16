package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// update refreshes every data set the ARIN resource depends on, as far as the
// upstream sources allow, and publishes one bundle:
//
//	mmdb/                   GeoLite2 resource (as refresh)
//	arindb/                 final arindb resource: arin.mmdb, manifest.json,
//	                        registration-manifest.json, update-manifest.json,
//	                        and for the canary arin-old.mmdb and
//	                        manifest-old.json, byte copies of the previously
//	                        selected release (copyPreviousArinDatabase)
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
		previous := copyPreviousArinDatabase(ctx, options, arinDir)
		manifest["previous_database"] = previous
		return finishUpdate(stage, arinDir, manifest, "registration only (no subscriber catalog); subscriber augmentation skipped; "+previousArinDatabaseSummary(previous))
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
	previous := copyPreviousArinDatabase(ctx, options, arinDir)
	manifest["previous_database"] = previous
	summary += "; " + previousArinDatabaseSummary(previous)
	return finishUpdate(stage, arinDir, manifest, summary)
}

// The outcomes of the copy-forward recorded under previous_database.
const (
	previousArinDatabaseCopied    = "copied"
	previousArinDatabaseIdentical = "identical"
	previousArinDatabaseNone      = "none"
	previousArinDatabaseRefused   = "refused"
)

// Copies the previously selected database beside the new build as
// arin-old.mmdb, with its manifest as manifest-old.json, so every process of
// the release opens both (arindbctl/ARINDB.md, "Files and the arindbctl
// copy-forward"). Previous means exactly the file a process of the named
// environment would open before this release is added: the runtime resolver
// over --previous-config, or the --previous-database pin. The file is opened
// with the runtime decoder and refused unless its type is urnetwork arindb,
// the manifest beside it names it in sha256["arin.mmdb"], the streamed copy
// has that SHA-256, the manifest's built_at second is its build epoch, and
// that epoch is strictly older than the new build's. A refusal, an identical
// previous database or none leaves both files out. None of these fails the
// release: blocking it would keep the suspect database in service. The record
// says what happened and why.
func copyPreviousArinDatabase(ctx context.Context, options commandOptions, arinDir string) map[string]any {
	record := map[string]any{}
	finish := func(status string, reason string) map[string]any {
		record["status"] = status
		if reason != "" {
			record["reason"] = reason
		}
		return record
	}
	source := options.previousDatabase
	switch {
	case source != "":
		record["selected_by"] = "previous-database"
	case options.previousConfig != "":
		record["selected_by"] = "previous-config"
		record["config_root"] = options.previousConfig
		record["env"] = options.previousEnv
		paths, err := server.ResolveResourcePaths(options.previousConfig, options.previousEnv, "arindb/arin.mmdb")
		if errors.Is(err, server.ErrResourceNotFound) {
			return finish(previousArinDatabaseNone, "no arindb/arin.mmdb resolves beneath the previous config")
		}
		if err != nil {
			return finish(previousArinDatabaseRefused, "resolve the previous database: "+err.Error())
		}
		source = paths[0]
	default:
		return finish(previousArinDatabaseNone, "no previous database was named")
	}
	record["source"] = source

	newEpoch, err := server.ReadArinDatabaseBuildEpoch(filepath.Join(arinDir, "arin.mmdb"))
	if err != nil {
		return finish(previousArinDatabaseRefused, "open the new database: "+err.Error())
	}
	newManifest, err := server.ReadArinDatabaseManifest(filepath.Join(arinDir, "manifest.json"))
	if err != nil {
		return finish(previousArinDatabaseRefused, "read the new manifest: "+err.Error())
	}
	previousEpoch, err := server.ReadArinDatabaseBuildEpoch(source)
	if err != nil {
		return finish(previousArinDatabaseRefused, "open the previous database: "+err.Error())
	}
	record["build_epoch"] = previousEpoch
	manifestPath := filepath.Join(filepath.Dir(source), "manifest.json")
	manifestContent, err := os.ReadFile(manifestPath)
	if err != nil {
		return finish(previousArinDatabaseRefused, "read the previous manifest: "+err.Error())
	}
	manifest, err := server.ReadArinDatabaseManifest(manifestPath)
	if err != nil {
		return finish(previousArinDatabaseRefused, "read the previous manifest: "+err.Error())
	}
	record["sha256"] = manifest.Sha256
	if builtAt := manifest.BuiltAt.Unix(); builtAt != previousEpoch {
		return finish(previousArinDatabaseRefused, fmt.Sprintf("the previous manifest's built_at second %d is not the database's build epoch %d", builtAt, previousEpoch))
	}
	if manifest.Sha256 == newManifest.Sha256 {
		return finish(previousArinDatabaseIdentical, "the previous database is the new build")
	}
	if newEpoch <= previousEpoch {
		return finish(previousArinDatabaseRefused, fmt.Sprintf("the previous build epoch %d is not older than the new build's %d", previousEpoch, newEpoch))
	}

	destination := filepath.Join(arinDir, "arin-old.mmdb")
	size, digest, err := copyArinDatabaseFile(ctx, source, destination)
	if err != nil {
		os.Remove(destination)
		return finish(previousArinDatabaseRefused, "copy the previous database: "+err.Error())
	}
	if digest != manifest.Sha256 {
		os.Remove(destination)
		return finish(previousArinDatabaseRefused, "the copied bytes do not have the previous manifest's SHA-256")
	}
	// the epoch and type of the bytes actually copied, not only of the source
	if copiedEpoch, err := server.ReadArinDatabaseBuildEpoch(destination); err != nil || copiedEpoch != previousEpoch {
		os.Remove(destination)
		return finish(previousArinDatabaseRefused, "the copied database does not open with the previous build epoch")
	}
	if err := writeSyncedFile(filepath.Join(arinDir, "manifest-old.json"), manifestContent); err != nil {
		os.Remove(destination)
		return finish(previousArinDatabaseRefused, "write manifest-old.json: "+err.Error())
	}
	record["size"] = size
	return finish(previousArinDatabaseCopied, "")
}

// Streams source to a new destination file, hashing the bytes written.
func copyArinDatabaseFile(ctx context.Context, source string, destination string) (int64, string, error) {
	in, err := os.Open(source)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size := int64(0)
	buffer := make([]byte, 1024*1024)
	copyErr := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			count, readErr := in.Read(buffer)
			if 0 < count {
				if _, err := out.Write(buffer[:count]); err != nil {
					return err
				}
				hash.Write(buffer[:count])
				size += int64(count)
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}()
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return 0, "", copyErr
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// The copy-forward's outcome for the update summary and the release message.
func previousArinDatabaseSummary(record map[string]any) string {
	switch record["status"] {
	case previousArinDatabaseCopied:
		return fmt.Sprintf("canary: previous database copied (build epoch %v)", record["build_epoch"])
	default:
		return fmt.Sprintf("no canary: previous database %v (%v)", record["status"], record["reason"])
	}
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
