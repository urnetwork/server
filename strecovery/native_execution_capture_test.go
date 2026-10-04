// Public native proof capture has no signing input. Synthetic transport only
// supplies independently signed block bytes; real journals and verifier run.
package strecovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeExecutionCaptureTestInputs(t *testing.T, f *receiptFinalityFixture) (NativeExecutionCaptureConfig, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return NativeExecutionCaptureConfig{Schema: NativeExecutionCaptureSchema, Genesis: f.checkpoint.Genesis, CheckpointHash: f.checkpoint.Hash(), Parent: f.headers[2].identity, Child: f.headers[3].identity, Source: "synthetic-native-archive", RpcUrl: "http://archive.example"}, directory
}

func TestNativeExecutionCaptureRetainsProofAcrossRestartWithoutSigning(t *testing.T) {
	f := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(f)
	config, dir := nativeExecutionCaptureTestInputs(t, f)
	result, err := captureNativeExecutionFinality(t.Context(), f.checkpoint, config, dir, rpc.configure)
	if err != nil || result == nil || result.Finality.Parent != config.Parent || result.Finality.Child != config.Child {
		t.Fatal("actual capture failed", err)
	}
	before, err := os.ReadFile(result.Proof.Path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := captureNativeExecutionFinality(t.Context(), f.checkpoint, config, dir, func(*receiptCollectorRpc) { t.Fatal("completed proof restart attempted another RPC") })
	if err != nil || objectDigest(result) != objectDigest(again) || digest(before) != again.Proof.Sha256 {
		t.Fatal("completed native job authority changed on restart", err)
	}
	config.Source = "different-route"
	if value, err := captureNativeExecutionFinality(t.Context(), f.checkpoint, config, dir, func(*receiptCollectorRpc) { t.Fatal("changed capture context reached RPC") }); err == nil || value != nil {
		t.Fatal("capture route silently changed")
	}
}

func TestNativeExecutionCaptureRetriesMissingRequiredReadPastSixtyLogicalSeconds(t *testing.T) {
	f := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(f)
	config, dir := nativeExecutionCaptureTestInputs(t, f)
	missing, waits := 0, 0
	rpc.intercept = func(method string, params []json.RawMessage) (any, error, bool) {
		if method == "chain_getHeader" && missing < 65 {
			missing++
			return nil, nil, true
		}
		return nil, nil, false
	}
	result, err := captureNativeExecutionFinality(t.Context(), f.checkpoint, config, dir, func(client *receiptCollectorRpc) {
		rpc.configure(client)
		if client.retryWindow != 300*time.Second {
			t.Fatal("default producer read budget was narrowed", client.retryWindow)
		}
		client.wait = func(ctx context.Context, delay time.Duration) error {
			if delay != time.Second {
				t.Fatal("unexpected native retry wait", delay)
			}
			waits++
			return ctx.Err()
		}
	})
	if err != nil || result == nil || waits != 65 || missing != 65 {
		t.Fatal("required null became a permanent finality conflict", err, waits, missing)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "request-") {
			attempts++
		}
	}
	if attempts < 65 {
		t.Fatal("retry reads escaped retained request budget", attempts)
	}
}

func TestNativeExecutionCaptureMissingCertificateResumesWithoutAuthorityReset(t *testing.T) {
	f := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(f)
	config, dir := nativeExecutionCaptureTestInputs(t, f)
	certificate := rpc.certificates[config.Child.Hash]
	delete(rpc.certificates, config.Child.Hash)
	ctx, cancel := context.WithCancel(t.Context())
	result, err := captureNativeExecutionFinality(ctx, f.checkpoint, config, dir, func(client *receiptCollectorRpc) {
		rpc.configure(client)
		client.wait = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	})
	if result != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrNativeFinalityUnavailable) || errors.Is(err, ErrNativeFinalityConflict) {
		t.Fatal("missing certificate invented contradictory finality", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "native-proof.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete finality proof was published", err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "native-capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	rpc.certificates[config.Child.Hash] = certificate
	result, err = captureNativeExecutionFinality(t.Context(), f.checkpoint, config, dir, rpc.configure)
	if err != nil || result == nil || result.Finality.CheckpointHash != f.checkpoint.Hash() {
		t.Fatal("original pending finality could not resume", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "native-capture.json"))
	if err != nil || string(original) != string(after) {
		t.Fatal("resume replaced original authority manifest", err)
	}
}

func TestNativeExecutionCaptureReturnedForgeryRefusesWithoutRetry(t *testing.T) {
	for _, fault := range []string{"header", "signature", "bounds", "context"} {
		f := receiptFinalityTestFixture(t)
		rpc := captureTestRpcFixture(f)
		config, dir := nativeExecutionCaptureTestInputs(t, f)
		waits := 0
		switch fault {
		case "header":
			rpc.intercept = func(method string, _ []json.RawMessage) (any, error, bool) {
				if method == "chain_getHeader" {
					return captureTestHeaderJson(f.headers[1]), nil, true
				}
				return nil, nil, false
			}
		case "signature":
			raw, _ := hex.DecodeString(rpc.certificates[config.Child.Hash][2:])
			raw[81] ^= 1
			rpc.certificates[config.Child.Hash] = "0x" + hex.EncodeToString(raw)
		case "bounds":
			config.RetryWindowSeconds = 59
		case "context":
			config.CheckpointHash = "sha256:" + strings.Repeat("8", 64)
		}
		value, err := captureNativeExecutionFinality(t.Context(), f.checkpoint, config, dir, func(client *receiptCollectorRpc) {
			rpc.configure(client)
			client.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected wait") }
		})
		if !errors.Is(err, ErrNativeFinalityConflict) || value != nil || waits != 0 {
			t.Fatal("positive native conflict retried or admitted", fault, err, waits)
		}
		if _, err := os.Stat(filepath.Join(dir, "native-proof.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused native proof acquired publication", fault, err)
		}
	}
}
