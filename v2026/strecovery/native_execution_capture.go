// A native producer retains every complete header/certificate and durable RPC
// debit before publishing an independently verified proof. No key is present:
// ordinary finality advances by GRANDPA, never by per-block operator signatures.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/urnetwork/server/v2026"
)

const NativeExecutionCaptureSchema = "urnetwork-native-execution-capture-v1"

// Missing archive coverage is retryable evidence unavailability. Positive
// disconnected headers, invalid votes and unsupported transitions stay errors.
var ErrNativeFinalityUnavailable = errors.New("native finality evidence unavailable")

type nativeFinalityUnavailableError struct{ reason string }

func (self *nativeFinalityUnavailableError) Error() string { return self.reason }
func (self *nativeFinalityUnavailableError) Unwrap() error { return ErrNativeFinalityUnavailable }

// Missing certificate coverage may retry only after a complete, nonempty
// observation of that exact cause. Joined conflicts and absent receivers hold.
func nativeFinalityUnavailableOnly(err error) bool {
	inspection := server.InspectErrorCauses(err)
	if !inspection.Complete {
		return false
	}
	seen := false
	for _, node := range inspection.Nodes {
		if node.Leaf {
			seen = true
			if node.Err != ErrNativeFinalityUnavailable {
				return false
			}
		}
	}
	return seen
}

// The route is an explicit observation source, not finality authority. Zero
// retry budget retains the established 300-second default; accepted overrides
// are 60..900 seconds. Partial journals keep the same config across retries.
type NativeExecutionCaptureConfig struct {
	Schema                   string                `json:"schema"`
	Genesis                  string                `json:"genesis"`
	CheckpointHash           string                `json:"checkpoint_hash"`
	Parent                   ObservedBlockIdentity `json:"parent"`
	Child                    ObservedBlockIdentity `json:"child"`
	Source                   string                `json:"source"`
	RpcUrl                   string                `json:"rpc_url"`
	MaximumDescendantHeaders uint64                `json:"maximum_descendant_headers"`
	RetryWindowSeconds       uint64                `json:"retry_window_seconds"`
}

type NativeExecutionCaptureResult struct {
	Schema     string                   `json:"schema"`
	Source     string                   `json:"source"`
	ConfigHash string                   `json:"config_hash"`
	Proof      FileReference            `json:"proof"`
	Finality   *NativeExecutionFinality `json:"finality"`
}

func (self NativeExecutionCaptureConfig) validate(checkpoint *NativeFinalityCheckpoint, anchor *nativeFinalityHeader) error {
	if self.Schema != NativeExecutionCaptureSchema || self.CheckpointHash != checkpoint.Hash() || !labelPattern.MatchString(self.Source) || !canonicalHex(self.Parent.Hash, 32) || !canonicalHex(self.Child.Hash, 32) || self.Parent.Number >= uint64(^uint32(0)) || self.Parent.Number+1 != self.Child.Number || self.Child.Number <= anchor.identity.Number || self.Child.Number-anchor.identity.Number >= maximumNativeFinalityHeaders || self.MaximumDescendantHeaders >= maximumNativeFinalityHeaders || self.MaximumDescendantHeaders+self.Child.Number-anchor.identity.Number >= maximumNativeFinalityHeaders || self.MaximumDescendantHeaders+self.Child.Number > uint64(^uint32(0)) {
		return errors.New("native execution capture requires an exact checkpoint and consecutive bounded parent/child")
	}
	if self.RetryWindowSeconds != 0 && (self.RetryWindowSeconds < 60 || self.RetryWindowSeconds > 900) {
		return errors.New("native execution capture retry budget must be60..900seconds (default300)")
	}
	return collectionRpcUrl(self.RpcUrl)
}

// Completed proof bytes are reused only after fresh cryptographic verification.
// No stored result flag replaces that check, and close failures return no report.
func CaptureNativeExecutionFinality(ctx context.Context, checkpoint *NativeFinalityCheckpoint, config NativeExecutionCaptureConfig, directory string) (*NativeExecutionCaptureResult, error) {
	return captureNativeExecutionFinality(ctx, checkpoint, config, directory, nil)
}

func captureNativeExecutionFinality(ctx context.Context, checkpoint *NativeFinalityCheckpoint, config NativeExecutionCaptureConfig, directory string, configure func(*receiptCollectorRpc)) (result *NativeExecutionCaptureResult, resultErr error) {
	if ctx == nil {
		return nil, errors.New("native execution capture context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	anchor, err := checkpoint.validateGenesis(config.Genesis, &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes})
	if err != nil {
		return nil, nativeFinalityVerificationError(err)
	}
	if err := config.validate(checkpoint, anchor); err != nil {
		return nil, nativeFinalityVerificationError(err)
	}
	seconds := config.RetryWindowSeconds
	if seconds == 0 {
		seconds = 300
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	store, err := openPrivatePath(directory, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, store.Close())
		if resultErr != nil {
			result = nil
		}
	}()
	unlock, err := lockDirectory(store)
	if err != nil {
		return nil, err
	}
	defer unlock()
	capture := &finalityCapture{directory: store, nativeExecution: true}
	if err := capture.save(ctx, "native-capture.json", config); err != nil {
		return nil, err
	}
	var proof NativeExecutionFinalityProof
	found, err := capture.load(ctx, "native-proof.json", MaximumReceiptFinalityBytes, &proof)
	if err != nil {
		return nil, err
	}
	if !found {
		capture.rpc = newReceiptCollectorRpc(config.RpcUrl)
		capture.rpc.finality, capture.rpc.requiredFinality, capture.rpc.nativeExecution = true, true, true
		capture.rpc.retryWindow = time.Duration(seconds) * time.Second
		defer capture.rpc.client.CloseIdleConnections()
		if configure != nil {
			configure(capture.rpc)
		}
		if err := capture.resumeBudget(ctx); err != nil {
			return nil, err
		}
		genesis, err := capture.blockHash(ctx, 0)
		if err != nil {
			return nil, err
		}
		if genesis != config.Genesis {
			return nil, nativeFinalityVerificationError(errors.New("native execution capture route returned a different genesis"))
		}
		for {
			segments, err := capture.collectWindow(ctx, config.Child, checkpoint, anchor, config.MaximumDescendantHeaders)
			if err == nil {
				proof = NativeExecutionFinalityProof{Schema: NativeExecutionFinalitySchema, CheckpointHash: checkpoint.Hash(), Parent: config.Parent, Child: config.Child, Segments: segments}
				break
			}
			if !nativeFinalityUnavailableOnly(err) {
				return nil, err
			}
			if err := capture.rpc.wait(ctx, time.Second); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrNativeFinalityUnavailable, err)
			}
		}
	}
	if proof.Parent != config.Parent || proof.Child != config.Child {
		return nil, nativeFinalityVerificationError(errors.New("native execution retained proof changed its exact boundary"))
	}
	finality, err := VerifyNativeExecutionFinality(ctx, config.Genesis, checkpoint, &proof)
	if err != nil {
		return nil, err
	}
	if finality.Certified.Number < config.Child.Number || finality.Certified.Number-config.Child.Number > config.MaximumDescendantHeaders {
		return nil, nativeFinalityVerificationError(errors.New("native execution retained certificate exceeds the reviewed descendant window"))
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaximumReceiptFinalityBytes {
		return nil, errors.New("native execution proof exceeds the final byte bound")
	}
	if _, err := publishFile(ctx, store, "native-proof.json", raw); err != nil {
		return nil, err
	}
	return &NativeExecutionCaptureResult{Schema: NativeExecutionCaptureSchema, Source: config.Source, ConfigHash: objectDigest(config), Proof: FileReference{Path: filepath.Join(directory, "native-proof.json"), Sha256: digest(raw)}, Finality: finality}, nil
}
