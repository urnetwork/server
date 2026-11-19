// Optional existing-only evidence must not change any ordinary signed frame.
package competitionagent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// This type is the pre-change response contract, including its exact field
// order. A nil capability preserves the old signing bytes in both directions.
func TestExistingEvidenceAbsentPreservesOrdinarySignedResponse(t *testing.T) {
	type legacyResponse struct {
		Schema         int             `json:"schema"`
		HostId         string          `json:"host_id"`
		RunId          string          `json:"run_id,omitempty"`
		FencingToken   int64           `json:"fencing_token,omitempty"`
		Identity       *Identity       `json:"identity,omitempty"`
		State          string          `json:"state"`
		ManifestSha256 string          `json:"manifest_sha256,omitempty"`
		RequestSha256  string          `json:"request_sha256,omitempty"`
		ExitCode       int             `json:"exit_code"`
		ErrorCode      string          `json:"error_code,omitempty"`
		SelfCheck      json.RawMessage `json:"self_check,omitempty"`
		Files          []File          `json:"files,omitempty"`
		Signature      string          `json:"signature"`
	}
	old := legacyResponse{Schema: Schema, HostId: "synthetic-ordinary-host", RunId: "synthetic-run", FencingToken: 7, State: "running", ExitCode: -1}
	oldBytes, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	current := Response{Schema: old.Schema, HostId: old.HostId, RunId: old.RunId, FencingToken: old.FencingToken, State: old.State, ExitCode: old.ExitCode}
	currentBytes, err := json.Marshal(current)
	if err != nil || !bytes.Equal(oldBytes, currentBytes) {
		t.Fatal("nil existing authority changed ordinary response signing bytes", err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	current.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, oldBytes))
	if err := VerifyResponse(current, key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal("old ordinary signature became incompatible", err)
	}
	if err := SignResponse(&current, key); err != nil || current.Signature != base64.StdEncoding.EncodeToString(ed25519.Sign(key, oldBytes)) {
		t.Fatal("ordinary signature changed without existing-only evidence", err)
	}
}
