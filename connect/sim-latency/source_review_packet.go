// Materializes exact immutable v2 evidence before any review exists. This is a
// private trusted read path, not an approval or a public artifact endpoint.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Creates a new private directory only after authenticating exact score and
// patch bytes. It neither rewrites an existing packet nor invents dispositions.
func materializeSourceReviewPacket(packet *controller.CompetitionSourceReviewPacket, requestedDirectory string) (string, error) {
	if packet == nil {
		return "", errors.New("sealed source review packet is absent")
	}
	patchDigest, scoreDigest := sha256.Sum256(packet.Patch), sha256.Sum256(packet.Score)
	if len(packet.Patch) == 0 || len(packet.Score) == 0 || hex.EncodeToString(patchDigest[:]) != packet.Entry.PatchSha256 || hex.EncodeToString(scoreDigest[:]) != packet.Entry.ScoreSha256 {
		return "", errors.New("review packet differs from authenticated sealed evidence")
	}
	directory, err := filepath.Abs(requestedDirectory)
	if err != nil || requestedDirectory == "" {
		return "", errors.New("review packet requires a fresh --out-dir")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", err
	}
	metadata := *packet
	metadata.Patch, metadata.Score = nil, nil
	// Review one patch, not every other submission. Preserve the authenticated
	// full-seal digest as provenance, explicitly label this smaller projection,
	// and retain the selected candidate and original score file separately.
	metadata.Seal.Entries = nil
	metadataBytes, err := json.MarshalIndent(struct {
		controller.CompetitionSourceReviewPacket
		SealMetadataOnly bool `json:"seal_metadata_only"`
	}{CompetitionSourceReviewPacket: metadata, SealMetadataOnly: true}, "", "  ")
	if err != nil {
		return "", err
	}
	templateBytes, err := json.MarshalIndent(sourceReviewTemplate(packet), "", "  ")
	if err != nil {
		return "", err
	}
	for name, content := range map[string][]byte{"candidate.json": append(metadataBytes, '\n'), "canonical.patch": packet.Patch, "score.json": packet.Score, "review-template.json": append(templateBytes, '\n')} {
		if err := writePrivateReviewFile(filepath.Join(directory, name), content); err != nil {
			return "", err
		}
	}
	file, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		return "", err
	}
	return directory, nil
}

// Reads a successful sealed candidate without requiring prior source approval.
// The store releases its database read before authenticating archive objects.
func runSourceReviewPacket(opts docopt.Opts) {
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("review packet settings: %s", err)
	}
	roundId, err := server.ParseId(optString(opts, "--round-id", ""))
	if err != nil {
		fatalf("review packet round: %s", err)
	}
	jobId, err := server.ParseId(optString(opts, "--job-id", ""))
	if err != nil {
		fatalf("review packet job: %s", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	packet, err := store.SourceReviewPacket(ctx, settings, roundId, jobId)
	if err != nil {
		fatalf("authenticated review evidence: %s", err)
	}
	directory, err := materializeSourceReviewPacket(packet, optString(opts, "--out-dir", ""))
	if err != nil {
		fatalf("private review packet: %s", err)
	}
	result, _ := json.Marshal(struct {
		Directory   string    `json:"directory"`
		RoundId     server.Id `json:"round_id"`
		JobId       server.Id `json:"job_id"`
		ScoreSha256 string    `json:"score_sha256"`
	}{Directory: directory, RoundId: roundId, JobId: jobId, ScoreSha256: packet.Entry.ScoreSha256})
	fmt.Printf("%s\n", result)
}
