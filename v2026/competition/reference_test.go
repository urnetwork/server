package competition

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type referenceManifest struct {
	Schema                   int                    `json:"schema"`
	Status                   string                 `json:"status"`
	BaseGitSha               string                 `json:"base_git_sha"`
	BaseImageId              string                 `json:"base_image_id"`
	TargetPath               string                 `json:"target_path"`
	TargetBlobSha1           string                 `json:"target_blob_sha1"`
	PolicySha256             string                 `json:"policy_sha256"`
	BuilderSha256            string                 `json:"builder_sha256"`
	LocalBuildVerification   localBuildVerification `json:"local_build_verification"`
	References               []referenceRecord      `json:"references"`
	LowerRawScoreIsBetter    bool                   `json:"lower_raw_score_is_better"`
	OfficialSeparability     string                 `json:"official_separability"`
	RequiredCorrectSeedOrder string                 `json:"required_correct_seed_order"`
	RequiredSeedPassCount    int                    `json:"required_seed_pass_count"`
	RequiredSeedCount        int                    `json:"required_seed_count"`
}

type localBuildVerification struct {
	Status                      string `json:"status"`
	CacheReuse                  string `json:"cache_reuse"`
	CandidateExecutionIsolation string `json:"candidate_execution_isolation"`
	ProtectedPathCount          int    `json:"protected_path_count"`
	Scope                       string `json:"scope"`
	OfficialHost                bool   `json:"official_host"`
}

type referenceRecord struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Sha256          string `json:"sha256"`
	CandidateGitSha string `json:"candidate_git_sha"`
	Image           string `json:"image"`
	ImageId         string `json:"image_id"`
	ImageKey        string `json:"image_key"`
	SimulatorSha256 string `json:"simulator_sha256"`
	ExpectedOrder   int    `json:"expected_order"`
}

// The provisional references stay bound to one exact development base while
// making the unrun official separability gate impossible to mistake for a pass.
func TestReferenceSubmissionsAuthenticate(t *testing.T) {
	referenceRoot := "references"
	manifestBytes := readReferenceFile(t, filepath.Join(referenceRoot, "manifest.json"))
	var manifest referenceManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode reference manifest: %v", err)
	}
	if manifest.Schema != 1 || manifest.Status != "provisional" ||
		manifest.OfficialSeparability != "not_run" || !manifest.LowerRawScoreIsBetter ||
		manifest.RequiredCorrectSeedOrder != "better < noop < worse" ||
		manifest.RequiredSeedPassCount != 19 || manifest.RequiredSeedCount != 20 {
		t.Fatalf("invalid provisional reference contract: %#v", manifest)
	}
	if manifest.LocalBuildVerification.Status != "passed" ||
		manifest.LocalBuildVerification.CacheReuse != "passed" ||
		manifest.LocalBuildVerification.CandidateExecutionIsolation != "passed" ||
		manifest.LocalBuildVerification.ProtectedPathCount != 6 ||
		manifest.LocalBuildVerification.Scope != "development_host" ||
		manifest.LocalBuildVerification.OfficialHost {
		t.Fatalf("invalid local build verification boundary: %#v", manifest.LocalBuildVerification)
	}
	if !gitShaPattern.MatchString(manifest.BaseGitSha) ||
		!imageDigestPattern.MatchString(manifest.BaseImageId) ||
		!sha256Pattern.MatchString(manifest.PolicySha256) ||
		!sha256Pattern.MatchString(manifest.BuilderSha256) {
		t.Fatal("reference manifest contains a malformed pinned identity")
	}

	policyBytes := readReferenceFile(t, filepath.Join("container", "policy.example.json"))
	if digestHex(policyBytes) != manifest.PolicySha256 {
		t.Fatal("reference policy hash does not match the manifest")
	}
	var policy PatchPolicy
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatalf("decode reference patch policy: %v", err)
	}
	builderBytes := readReferenceFile(t, filepath.Join("container", "Dockerfile.submission"))
	if digestHex(builderBytes) != manifest.BuilderSha256 {
		t.Fatal("reference builder hash does not match the manifest")
	}

	targetBytes := readReferenceFile(t, filepath.Join("..", manifest.TargetPath))
	gitBlob := append([]byte(fmt.Sprintf("blob %d\x00", len(targetBytes))), targetBytes...)
	targetDigest := sha1.Sum(gitBlob)
	if hex.EncodeToString(targetDigest[:]) != manifest.TargetBlobSha1 {
		t.Fatal("reference target blob does not match the manifest")
	}

	wantOrders := map[string]int{"better": 0, "noop": 1, "worse": 2}
	wantSnippets := map[string]string{
		"better": "model.GetOpenContractIdsForSourceOrDestination(",
		"noop":   "// All other controller activity moved",
		"worse":  "time.Sleep(25 * time.Millisecond)",
	}
	seen := map[string]bool{}
	seenCandidateShas := map[string]bool{}
	seenImageIds := map[string]bool{}
	seenImageKeys := map[string]bool{}
	for _, reference := range manifest.References {
		wantOrder, ok := wantOrders[reference.Name]
		if !ok || seen[reference.Name] || reference.ExpectedOrder != wantOrder ||
			reference.Path != reference.Name+".patch" || !sha256Pattern.MatchString(reference.Sha256) ||
			!gitShaPattern.MatchString(reference.CandidateGitSha) ||
			!imageDigestPattern.MatchString(reference.ImageId) ||
			!sha256Pattern.MatchString(reference.ImageKey) ||
			!sha256Pattern.MatchString(reference.SimulatorSha256) ||
			reference.Image != "urnetwork/sim-latency-submission:"+reference.ImageKey[:32] ||
			seenCandidateShas[reference.CandidateGitSha] || seenImageIds[reference.ImageId] ||
			seenImageKeys[reference.ImageKey] {
			t.Fatalf("invalid reference record: %#v", reference)
		}
		seen[reference.Name] = true
		seenCandidateShas[reference.CandidateGitSha] = true
		seenImageIds[reference.ImageId] = true
		seenImageKeys[reference.ImageKey] = true
		patchBytes := readReferenceFile(t, filepath.Join(referenceRoot, reference.Path))
		if digestHex(patchBytes) != reference.Sha256 {
			t.Fatalf("%s patch hash does not match the manifest", reference.Name)
		}
		canonical, validationErr := ValidateAndCanonicalizePatch(string(patchBytes), policy)
		if validationErr != nil {
			t.Fatalf("%s patch rejected: %s", reference.Name, validationErr.Code)
		}
		if len(canonical.Paths) != 1 || canonical.Paths[0] != manifest.TargetPath ||
			!strings.Contains(string(patchBytes), wantSnippets[reference.Name]) {
			t.Fatalf("%s patch does not match its declared target/behavior", reference.Name)
		}
	}
	if len(seen) != len(wantOrders) {
		t.Fatalf("reference set incomplete: %#v", seen)
	}
}

// The bulk lookup used by the better reference keys results by unordered
// transfer pair. A directional key silently misses the reverse ID ordering and
// can turn a real contract into a false inactive result.
func TestBetterReferenceUsesLookupCompatibleUnorderedPair(t *testing.T) {
	sourceId := server.RequireParseId("00000000-0000-0000-0000-000000000001")
	destinationId := server.RequireParseId("00000000-0000-0000-0000-000000000002")
	lookupKey := model.NewUnorderedTransferPair(sourceId, destinationId)
	lookup := map[model.TransferPair]bool{lookupKey: true}

	if lookup[model.NewTransferPair(destinationId, sourceId)] {
		t.Fatal("directional reverse key unexpectedly matched an unordered lookup key")
	}
	if !lookup[model.NewUnorderedTransferPair(destinationId, sourceId)] {
		t.Fatal("unordered reverse key did not match the bulk lookup key")
	}

	patch := string(readReferenceFile(t, filepath.Join("references", "better.patch")))
	if !strings.Contains(patch, "-\ttransferPair := model.NewTransferPair(sourceId, destinationId)") ||
		!strings.Contains(patch, "+\ttransferPair := model.NewUnorderedTransferPair(sourceId, destinationId)") {
		t.Fatal("better reference does not replace the directional cache/lookup key")
	}
}

func readReferenceFile(t *testing.T, path string) []byte {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return bytes
}

func digestHex(bytes []byte) string {
	digest := sha256.Sum256(bytes)
	return hex.EncodeToString(digest[:])
}
