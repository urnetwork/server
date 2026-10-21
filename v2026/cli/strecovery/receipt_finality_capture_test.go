// The public capture command uses a synthetic local HTTP archive and retained
// pinned input files. The ordinary offline command replays the produced proof.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/strecovery"
)

// Exercise the complete CLI publication/pin/replay path without reopening any
// operator database. The server closes before a completed capture is retried.
func TestRecoveryCommandCapturesAndReplaysPinnedNativeFinality(t *testing.T) {
	verifyArgs, reader, retained, proof, certificate := finalityCommandInputs(t)
	archivePath := verifyArgs[2]
	collection, err := strecovery.LoadReceiptCollection(context.Background(), strecovery.FileReference{Path: verifyArgs[4], Sha256: verifyArgs[6]})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := strecovery.LoadNativeFinalityCheckpoint(context.Background(), strecovery.FileReference{Path: verifyArgs[8], Sha256: verifyArgs[10]})
	if err != nil {
		t.Fatal(err)
	}
	rawHeader, err := hex.DecodeString(proof.Segments[0].Headers[0][2:])
	if err != nil {
		t.Fatal(err)
	}
	header := map[string]any{"parentHash": "0x" + hex.EncodeToString(rawHeader[:32]), "number": "0x2bd", "stateRoot": "0x" + strings.Repeat("27", 32), "extrinsicsRoot": "0x" + strings.Repeat("27", 32), "digest": map[string]any{"logs": []string{"0x" + hex.EncodeToString(rawHeader[99:])}}}
	numbers := make([]uint16, len(certificate))
	for index, value := range certificate {
		numbers[index] = uint16(value)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(writer, "bad fixture request", 400)
			return
		}
		var result any
		switch payload.Method {
		case "chain_getBlockHash":
			if len(payload.Params) != 1 || string(payload.Params[0]) != "0" {
				t.Error("unexpected numbered selector")
			}
			result = checkpoint.Genesis
		case "chain_getHeader", "chain_getBlock":
			var hash string
			if len(payload.Params) != 1 || json.Unmarshal(payload.Params[0], &hash) != nil || hash != collection.Observations.NativeFinalized.Hash {
				t.Error("hash selector changed")
			}
			result = header
			if payload.Method == "chain_getBlock" {
				result = map[string]any{"block": map[string]any{"header": header, "extrinsics": []string{}}, "justifications": []any{[]any{[]uint16{70, 82, 78, 75}, numbers}}}
			}
		default:
			t.Error("capture emitted non-read RPC")
			http.Error(writer, "unavailable", 400)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": payload.Id, "result": result})
	}))
	defer server.Close()
	config := strecovery.ReceiptFinalityCaptureConfig{Schema: strecovery.ReceiptFinalityCaptureConfigSchema, CollectionHash: collection.ContentHash, CheckpointHash: checkpoint.Hash(), Source: "synthetic-cli-archive", RpcUrl: server.URL, MaximumDescendantHeaders: 0, RetryWindowSeconds: 60}
	rawConfig, _ := json.Marshal(config)
	configPath := filepath.Join(filepath.Dir(archivePath), "capture-config.json")
	if err := os.WriteFile(configPath, rawConfig, 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(filepath.Dir(archivePath), "capture")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"capture-finality"}, verifyArgs[1:11]...)
	args = append(args, "--config", configPath, "--config-sha256", fmt.Sprintf("sha256:%x", sha256.Sum256(rawConfig)), "--capture-dir", directory)
	var first bytes.Buffer
	if err := run(context.Background(), args, &first, reader); err != nil {
		t.Fatal(err)
	}
	var result strecovery.ReceiptFinalityCaptureResult
	if err := json.Unmarshal(first.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Reconciliation.FinalityAuthenticated || result.Reconciliation.AuthorityCheckpointAuthenticated || result.Reconciliation.SpendingAuthorized || reader.calls != 2 {
		t.Fatal("capture opened database or approved authority")
	}
	server.Close()
	var resumed bytes.Buffer
	if err := run(context.Background(), args, &resumed, reader); err != nil || !bytes.Equal(first.Bytes(), resumed.Bytes()) {
		t.Fatalf("complete restart used RPC or changed output: %v", err)
	}
	verifyArgs[len(verifyArgs)-3], verifyArgs[len(verifyArgs)-1] = result.Proof.Path, result.Proof.Sha256
	var offline bytes.Buffer
	if err := run(context.Background(), verifyArgs, &offline, reader); err != nil {
		t.Fatal(err)
	}
	for path, before := range retained {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("capture rewrote original retained evidence")
		}
	}
}

// Partial arguments and invented approvals never reach filesystem/network work.
func TestRecoveryCommandRequiresPinnedNativeCaptureInputsWithoutApprovalFlags(t *testing.T) {
	base := []string{"capture-finality", "--archive", "/private.example/archive.json", "--collection", "/private.example/collection.json", "--collection-sha256", "sha256:" + strings.Repeat("1", 64), "--checkpoint", "/private.example/checkpoint.json", "--checkpoint-sha256", "sha256:" + strings.Repeat("2", 64), "--config", "/private.example/config.json", "--config-sha256", "sha256:" + strings.Repeat("3", 64), "--capture-dir", "/private.example/capture"}
	for index := 1; index < len(base); index += 2 {
		args := append(append([]string{}, base[:index]...), base[index+2:]...)
		var output bytes.Buffer
		reader := &commandReader{fail: true}
		if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
			t.Fatalf("incomplete capture inputs accepted: %s", base[index])
		}
	}
	for _, flag := range []string{"--approve-checkpoint", "--approve-mainnet", "--allow-unknown-authority", "--send"} {
		var output bytes.Buffer
		reader := &commandReader{fail: true}
		if err := run(context.Background(), append(append([]string{}, base...), flag), &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
			t.Fatalf("authority or send flag accepted: %s", flag)
		}
	}
}
