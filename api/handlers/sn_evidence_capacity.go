// Public retrieval has finite owners for simulator plan carriers. These are
// transport bounds, not approval or semantic acceptance; ordinary proofs and
// uploads keep their existing limits.
package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/urnetwork/server/startifact"
)

const (
	snEvidenceOrdinaryFileBytes  uint64 = 32 * 1024 * 1024
	snEvidencePlanBytes          uint64 = 128 * 1024 * 1024
	snEvidenceLineageBytes       uint64 = 4 * 1024 * 1024 * 1024
	snEvidenceCarrierOverhead    uint64 = 1024 * 1024
	snEvidencePlanBundleBytes           = ((snEvidencePlanBytes + 2) / 3 * 4) + snEvidenceCarrierOverhead
	snEvidencePriorCarrierBytes         = ((snEvidenceLineageBytes + 2) / 3 * 4) + snEvidenceCarrierOverhead
	snEvidenceCampaignFileKind          = "scenario-evidence-file"
	snEvidenceSemanticFileKind          = "scenario-semantic-file"
	snEvidenceCampaignFileSchema        = "urnetwork-sim-campaign-evidence-file-v1"
	snEvidenceSemanticFileSchema        = "urnetwork-final-semantic-supplement-file-v1"
)

// Fields preceding data choose a finite read owner; their claims are checked
// again against the complete signed envelope and decoded bytes.
type snEvidenceFileHeader struct {
	Schema      string `json:"schema"`
	RunId       string `json:"run_id"`
	Scope       string `json:"scope,omitempty"`
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Size        uint64 `json:"size"`
}

// The signed source body retains its exact original hash and size.
type snEvidenceFilePayload struct {
	Schema      string `json:"schema"`
	RunId       string `json:"run_id"`
	Scope       string `json:"scope,omitempty"`
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Size        uint64 `json:"size"`
	Data        []byte `json:"data"`
}

// Compound files retain ordered exact source bytes rather than widening the
// bounds of individual proofs inside the compound object.
type snEvidenceSourceFile struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	SizeBytes   uint64 `json:"size_bytes"`
	Data        []byte `json:"data"`
}

// Closed bundles use their original `bytes` field; lineage files use
// `size_bytes`. Separate types prevent either spelling becoming an alias.
type snEvidenceBundleSourceFile struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	SizeBytes   uint64 `json:"bytes"`
	Data        []byte `json:"data"`
}

// Hash-bearing path components must have one lowercase canonical spelling.
func snEvidenceHashFile(name, prefix, suffix string) bool {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// Only the producer's exact class and numbered chunk spelling own plan space.
func snEvidencePlanBundleClass(name string) string {
	for _, class := range []string{"launch-foundation", "plan-history"} {
		if name == class {
			return class
		}
		if !strings.HasPrefix(name, class+"-") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(name, class+"-"), "-of-")
		if len(parts) != 2 {
			continue
		}
		index, indexErr := strconv.ParseUint(parts[0], 10, 64)
		count, countErr := strconv.ParseUint(parts[1], 10, 64)
		if indexErr == nil && countErr == nil && index > 0 && index <= count && count >= 2 && name == fmt.Sprintf("%s-%03d-of-%03d", class, index, count) {
			return class
		}
	}
	return ""
}

// Zero preserves the ordinary envelope ceiling. A directory prefix alone
// never grants additional allocation authority.
func snEvidencePlanPathBytes(name string) uint64 {
	switch name {
	case "final-derived/setup-plan.json":
		return snEvidencePlanBytes
	case "final-derived/fleet-generation-lineage.json", "final-derived/fleet-lifecycle-lineage.json":
		return snEvidenceLineageBytes
	}
	if snEvidenceHashFile(name, "final-derived/historical-coordinator/plans/", ".json") || snEvidenceHashFile(name, "final-derived/validator-activation-plan-", ".json") {
		return snEvidencePlanBytes
	}
	if snEvidenceHashFile(name, "final-inputs/prior-release/semantic-files/", ".plan.evidence.json") {
		return snEvidencePriorCarrierBytes
	}
	const prefix = "final-derived/fleet-generation/renewal-"
	const suffix = "-approval.json"
	if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
		round, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix), 10, 64)
		if err == nil && round > 0 && name == fmt.Sprintf("%s%d%s", prefix, round, suffix) {
			return snEvidencePlanBytes
		}
	}
	if strings.HasPrefix(name, "final-inputs/bundles/") && strings.HasSuffix(name, ".json") {
		bundleName := strings.TrimSuffix(strings.TrimPrefix(name, "final-inputs/bundles/"), ".json")
		if snEvidencePlanBundleClass(bundleName) != "" {
			return snEvidencePlanBundleBytes
		}
	}
	return 0
}

// The maximum comes from the declared, later authenticated source size; a
// four-gibibyte owner never preallocates four gibibytes for a smaller file.
func snEvidenceFileReadBytes(kind, runId string, header snEvidenceFileHeader) (uint64, error) {
	maximum := snEvidencePlanPathBytes(header.Path)
	if maximum == 0 || header.Size == 0 || header.Size > maximum || header.RunId == "" || header.RunId != runId || !snEvidenceHashFile(header.ContentHash, "sha256:", "") {
		return 0, errors.New("evidence file has no finite plan owner")
	}
	switch kind {
	case snEvidenceCampaignFileKind:
		if header.Schema != snEvidenceCampaignFileSchema || header.Scope != "run" && header.Scope != "reference" || !strings.HasPrefix(header.Path, "final-inputs/") {
			return 0, errors.New("campaign plan carrier has an invalid scope or schema")
		}
	case snEvidenceSemanticFileKind:
		if header.Schema != snEvidenceSemanticFileSchema || header.Scope != "" || !strings.HasPrefix(header.Path, "final-derived/") {
			return 0, errors.New("semantic plan carrier has an invalid scope or schema")
		}
	default:
		return 0, errors.New("evidence kind has no plan capacity")
	}
	return ((header.Size + 2) / 3 * 4) + snEvidenceCarrierOverhead, nil
}

// Syntax validation precedes a constant-space scan of the object's own keys.
// Data strings are skipped without allocating another copy. Nested documents
// have their own typed decoder and duplicate/path checks.
func decodeSnEvidenceObject(raw []byte, target any, strict bool) error {
	if !json.Valid(raw) {
		return errors.New("evidence contains invalid json")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("evidence is not an object")
	}
	seenKVs := map[string]bool{}
	depth, keyExpected := 0, false
	for index := 0; index < len(trimmed); index++ {
		switch trimmed[index] {
		case '{', '[':
			depth++
			if depth == 1 {
				keyExpected = true
			}
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				keyExpected = true
			}
		case '"':
			start := index
			for index++; index < len(trimmed); index++ {
				if trimmed[index] == '\\' {
					index++
				} else if trimmed[index] == '"' {
					break
				}
			}
			if depth == 1 && keyExpected {
				var key string
				if err := json.Unmarshal(trimmed[start:index+1], &key); err != nil {
					return err
				}
				if seenKVs[key] {
					return errors.New("evidence repeats an object field")
				}
				seenKVs[key], keyExpected = true, false
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		decoder.DisallowUnknownFields()
	}
	return decoder.Decode(target)
}

// Complete envelope and exact inner bytes authenticate the earlier sizing
// claim. Prior wrappers have one level and bind the hash of the inner path.
func validateSnEvidencePlanFile(envelope *startifact.EvidenceEnvelope) error {
	var payload snEvidenceFilePayload
	if err := decodeSnEvidenceObject(envelope.Payload, &payload, true); err != nil {
		return err
	}
	header := snEvidenceFileHeader{Schema: payload.Schema, RunId: payload.RunId, Scope: payload.Scope, Path: payload.Path, ContentHash: payload.ContentHash, Size: payload.Size}
	if _, err := snEvidenceFileReadBytes(envelope.Kind, envelope.RunID, header); err != nil {
		return err
	}
	if payload.Size != uint64(len(payload.Data)) || snEvidenceDigest(payload.Data) != payload.ContentHash {
		return errors.New("evidence plan file differs from its committed bytes")
	}
	return validateSnEvidencePlanBody(payload.Path, payload.Data, envelope, true)
}

// Every source is hash checked, ordered and individually bounded. This is
// structural transport admission; the simulator independently replays plans.
func validateSnEvidencePlanBody(name string, raw []byte, envelope *startifact.EvidenceEnvelope, priorAllowed bool) error {
	maximum := snEvidencePlanPathBytes(name)
	if maximum == 0 || uint64(len(raw)) > maximum {
		return errors.New("plan source exceeds its typed capacity")
	}
	if maximum == snEvidencePriorCarrierBytes {
		if !priorAllowed {
			return errors.New("prior evidence carriers cannot nest")
		}
		var inner startifact.EvidenceEnvelope
		if err := decodeSnEvidenceObject(raw, &inner, true); err != nil {
			return err
		}
		if err := startifact.VerifyEvidence(&inner); err != nil {
			return err
		}
		if inner.Kind != snEvidenceSemanticFileKind || inner.DeploymentID != envelope.DeploymentID || inner.ChainID != envelope.ChainID || inner.GenesisHash != envelope.GenesisHash || inner.Netuid != envelope.Netuid || inner.Signer != envelope.Signer {
			return errors.New("prior plan carrier has a different signed authority")
		}
		var file snEvidenceFilePayload
		if err := decodeSnEvidenceObject(inner.Payload, &file, true); err != nil {
			return err
		}
		if file.Schema != snEvidenceSemanticFileSchema || file.Scope != "" || file.RunId != inner.RunID || !strings.HasPrefix(file.Path, "final-derived/") || file.Size != uint64(len(file.Data)) || snEvidenceDigest(file.Data) != file.ContentHash {
			return errors.New("prior plan carrier differs from its signed file")
		}
		digest := sha256.Sum256([]byte(file.Path))
		if name != "final-inputs/prior-release/semantic-files/"+hex.EncodeToString(digest[:])+".plan.evidence.json" {
			return errors.New("prior plan carrier has a different path hash")
		}
		return validateSnEvidencePlanBody(file.Path, file.Data, &inner, false)
	}
	if maximum == snEvidencePlanBytes {
		var plan struct {
			Schema       string `json:"schema"`
			DeploymentId string `json:"deployment_id"`
			PlanHash     string `json:"plan_hash"`
		}
		if err := decodeSnEvidenceObject(raw, &plan, false); err != nil {
			return err
		}
		version, err := strconv.ParseUint(strings.TrimPrefix(plan.Schema, "urnetwork-sim-plan-v"), 10, 64)
		if err != nil || version < 1 || version > 12 || plan.Schema != fmt.Sprintf("urnetwork-sim-plan-v%d", version) || plan.DeploymentId != envelope.DeploymentID || !snEvidenceHashFile(plan.PlanHash, "0x", "") {
			return errors.New("plan source has a different schema or deployment")
		}
		return nil
	}
	var compound struct {
		Schema       string                 `json:"schema"`
		DeploymentId string                 `json:"deployment_id,omitempty"`
		PlanHash     string                 `json:"plan_hash,omitempty"`
		RunId        string                 `json:"run_id,omitempty"`
		Files        []snEvidenceSourceFile `json:"files"`
	}
	class := ""
	if maximum == snEvidencePlanBundleBytes {
		var bundle struct {
			Schema string                       `json:"schema"`
			Name   string                       `json:"name"`
			Files  []snEvidenceBundleSourceFile `json:"files"`
		}
		if err := decodeSnEvidenceObject(raw, &bundle, true); err != nil {
			return err
		}
		class = snEvidencePlanBundleClass(bundle.Name)
		if class == "" || bundle.Schema != "urnetwork-final-collected-file-bundle-v1" || name != "final-inputs/bundles/"+bundle.Name+".json" {
			return errors.New("plan bundle differs from its exact source class")
		}
		for _, file := range bundle.Files {
			compound.Files = append(compound.Files, snEvidenceSourceFile{Path: file.Path, ContentHash: file.ContentHash, SizeBytes: file.SizeBytes, Data: file.Data})
		}
	} else {
		if err := decodeSnEvidenceObject(raw, &compound, true); err != nil {
			return err
		}
		if compound.DeploymentId != envelope.DeploymentID || !snEvidenceHashFile(compound.PlanHash, "0x", "") {
			return errors.New("plan lineage has a different source identity")
		}
		if name == "final-derived/fleet-generation-lineage.json" && (compound.Schema != "urnetwork-final-fleet-generation-lineage-v1" || compound.RunId != "") || name == "final-derived/fleet-lifecycle-lineage.json" && (compound.Schema != "urnetwork-final-fleet-lifecycle-lineage-v1" || compound.RunId != envelope.RunID) {
			return errors.New("plan lineage has a different schema or run")
		}
	}
	if len(compound.Files) == 0 {
		return errors.New("plan compound has no source files")
	}
	previous := ""
	for _, file := range compound.Files {
		bound := snEvidenceOrdinaryFileBytes
		if class != "" {
			bound = 24 * 1024 * 1024
		}
		if class == "launch-foundation" && file.Path == "plan.json" || class == "plan-history" && snEvidenceHashFile(file.Path, "", ".json") || class == "" && (file.Path == "launch-foundation/plan.json" || snEvidenceHashFile(file.Path, "plan-history/", ".json")) {
			bound = snEvidencePlanBytes
		}
		if file.Path == "" || file.Path == "." || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "../") || strings.ContainsAny(file.Path, "\\\r\n\x00") || previous != "" && previous >= file.Path || class == "" && file.SizeBytes == 0 || file.SizeBytes > bound || file.SizeBytes != uint64(len(file.Data)) || snEvidenceDigest(file.Data) != file.ContentHash {
			return errors.New("plan compound has an invalid, changed or oversized source")
		}
		previous = file.Path
	}
	return nil
}

// Content hashes commit raw bytes, independently of envelope signatures.
func snEvidenceDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}
