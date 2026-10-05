// Bounded native proof capture owns one private journal and one read-only RPC
// client. Immutable partial evidence and pre-request reservations survive a
// restart; only a complete offline-verified proof is published as proof.json.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const ReceiptFinalityCaptureConfigSchema = "urnetwork-native-finality-capture-config-v1"
const ReceiptFinalityCaptureSchema = "urnetwork-native-finality-capture-v1"

// Pins and a source label bind operator-selected input; they grant no node,
// checkpoint or runtime approval. Descendant search never changes the collection.
type ReceiptFinalityCaptureConfig struct {
	Schema                   string `json:"schema"`
	CollectionHash           string `json:"collection_hash"`
	CheckpointHash           string `json:"checkpoint_hash"`
	Source                   string `json:"source"`
	RpcUrl                   string `json:"rpc_url"`
	MaximumDescendantHeaders uint64 `json:"maximum_descendant_headers"`
	RetryWindowSeconds       uint64 `json:"retry_window_seconds"`
}

// The result contains an exact proof-file pin for offline verify-finality.
// Every authority/accounting/spending flag in Reconciliation remains false.
type ReceiptFinalityCaptureResult struct {
	Schema         string                         `json:"schema"`
	Admission      string                         `json:"admission"`
	Source         string                         `json:"source"`
	ConfigHash     string                         `json:"config_hash"`
	Proof          FileReference                  `json:"proof"`
	Reconciliation *ReceiptFinalityReconciliation `json:"reconciliation"`
}

// Configuration must be complete before network or journal ownership begins.
func (self ReceiptFinalityCaptureConfig) validate(collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, anchor *nativeFinalityHeader) error {
	if self.Schema != ReceiptFinalityCaptureConfigSchema || self.CollectionHash != collection.ContentHash || self.CheckpointHash != checkpoint.Hash() || !labelPattern.MatchString(self.Source) ||
		collection.Observations.NativeFinalized.Number <= anchor.identity.Number || collection.Observations.NativeFinalized.Number-anchor.identity.Number >= maximumNativeFinalityHeaders ||
		self.MaximumDescendantHeaders >= maximumNativeFinalityHeaders || self.MaximumDescendantHeaders+collection.Observations.NativeFinalized.Number-anchor.identity.Number >= maximumNativeFinalityHeaders ||
		self.MaximumDescendantHeaders+collection.Observations.NativeFinalized.Number > uint64(^uint32(0)) {
		return errors.New("native capture requires pinned collection/checkpoint and a descendant window within the rolling header bound")
	}
	if self.RetryWindowSeconds != 0 && (self.RetryWindowSeconds < 60 || self.RetryWindowSeconds > 900) {
		return errors.New("native capture retry window must be 60 to 900 seconds (default 300)")
	}
	return collectionRpcUrl(self.RpcUrl)
}

// Exact byte pins and the existing custody reader reject silent route changes.
func LoadReceiptFinalityCaptureConfig(ctx context.Context, reference FileReference) (*ReceiptFinalityCaptureConfig, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("native capture config requires a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, 64*1024)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("native capture config differs from its byte pin")
	}
	var config ReceiptFinalityCaptureConfig
	if err := decodeJson(raw, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// A capture owns the directory lock and client for its entire invocation.
// The private test seam supplies deterministic transport failures, never a signer.
type finalityCapture struct {
	directory       *os.File
	rpc             *receiptCollectorRpc
	nativeExecution bool
}

// The receipt adapter retains its historical error contract. The native role
// separately distinguishes complete contradictory evidence from unread data.
func (self *finalityCapture) invalid(err error) error {
	if self.nativeExecution {
		return nativeFinalityVerificationError(err)
	}
	return err
}

// Journal publications use the existing owner-private create-only fsync path.
func (self *finalityCapture) save(ctx context.Context, name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = publishFile(ctx, self.directory, name, append(raw, '\n'))
	return err
}

// Absence is explicit. Invalid, exposed or replaced partial evidence refuses;
// it is never deleted and fetched again as though the prior read never happened.
func (self *finalityCapture) load(ctx context.Context, name string, maximum int, value any) (bool, error) {
	file, err := openPrivateChild(self.directory, name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, readErr := readOpened(ctx, file, maximum)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return false, err
	}
	return true, self.invalid(decodeJson(raw, value))
}

// A reservation is synced before transport. Missing completion after a crash
// conservatively consumes a full reply allowance; no restart resets the budget.
type finalityCaptureAttempt struct {
	RequestHash string `json:"request_hash"`
}

// A completion releases only the unused reserved response bytes. Partial and
// error bodies count equally; the original request reservation remains intact.
type finalityCaptureRead struct {
	ReplyBytes *int `json:"reply_bytes"`
}

// Replay at most the fixed request budget of small immutable ledger records.
// Directory census bounds orphaned temporaries and unreferenced evidence too.
func (self *finalityCapture) resumeBudget(ctx context.Context) error {
	names, err := directoryNames(self.directory, 4*maximumCollectionRequests+4)
	if err != nil {
		return err
	}
	nameKVs := make(map[string]bool, len(names))
	for _, name := range names {
		nameKVs[name] = true
	}
	requests, remaining := 0, maximumCollectionTotalReplyBytes
	for index := 1; index <= maximumCollectionRequests; index++ {
		name := fmt.Sprintf("request-%05d.json", index)
		if !nameKVs[name] {
			break
		}
		var attempt finalityCaptureAttempt
		if _, err := self.load(ctx, name, 1024, &attempt); err != nil {
			return fmt.Errorf("native capture reading original request reservation: %w", err)
		}
		if !canonicalDigest(attempt.RequestHash) {
			return self.invalid(errors.New("native capture retained request reservation is invalid"))
		}
		var completion finalityCaptureRead
		found, err := self.load(ctx, fmt.Sprintf("read-%05d.json", index), 1024, &completion)
		if err != nil {
			return fmt.Errorf("native capture reading original response debit: %w", err)
		}
		if found && (completion.ReplyBytes == nil || *completion.ReplyBytes < 0 || *completion.ReplyBytes > maximumCollectionReplyBytes+1) {
			return self.invalid(errors.New("native capture retained response debit is invalid"))
		}
		debit := maximumCollectionReplyBytes + 1
		if found {
			debit = *completion.ReplyBytes
		}
		remaining -= debit
		requests++
		delete(nameKVs, name)
		delete(nameKVs, fmt.Sprintf("read-%05d.json", index))
	}
	for name := range nameKVs {
		if strings.HasPrefix(name, "request-") || strings.HasPrefix(name, "read-") {
			return self.invalid(errors.New("native capture request journal has a gap or orphan completion"))
		}
	}
	self.rpc.requests, self.rpc.remaining = requests, remaining
	self.rpc.beforeRead = func(ctx context.Context, id int, payload []byte) error {
		return self.save(ctx, fmt.Sprintf("request-%05d.json", id), finalityCaptureAttempt{RequestHash: digest(payload)})
	}
	self.rpc.afterRead = func(ctx context.Context, id, count int) error {
		return self.save(ctx, fmt.Sprintf("read-%05d.json", id), finalityCaptureRead{ReplyBytes: &count})
	}
	return nil
}

// Capture uses only exact header/block reads and bounded descendant discovery.
// The caller supplies separately pinned checkpoint authority; no RPC can create
// it. Inputs are borrowed immutable, and errors leave partial custody in place.
func CaptureReceiptFinality(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, config ReceiptFinalityCaptureConfig, directory string) (*ReceiptFinalityCaptureResult, error) {
	return captureReceiptFinality(ctx, archive, collection, checkpoint, config, directory, nil)
}

// Only package tests may replace transport/wait behavior before the journal
// installs its mandatory durable attempt hooks and lifetime budget.
func captureReceiptFinality(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, config ReceiptFinalityCaptureConfig, directory string, configure func(*receiptCollectorRpc)) (result *ReceiptFinalityCaptureResult, resultErr error) {
	if ctx == nil {
		return nil, errors.New("native capture context is absent")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if _, err := VerifyReceiptCollection(ctx, archive, collection); err != nil {
		return nil, err
	}
	anchor, err := checkpoint.validate(archive, &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes})
	if err != nil {
		return nil, err
	}
	if err := config.validate(collection, checkpoint, anchor); err != nil {
		return nil, err
	}
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
	capture := &finalityCapture{directory: store}
	manifest := struct {
		Schema     string                       `json:"schema"`
		CensusHash string                       `json:"census_hash"`
		Config     ReceiptFinalityCaptureConfig `json:"config"`
	}{Schema: ReceiptFinalityCaptureSchema, CensusHash: archive.CensusHash, Config: config}
	if err := capture.save(ctx, "capture.json", manifest); err != nil {
		return nil, err
	}
	var proof ReceiptFinalityProof
	found, err := capture.load(ctx, "proof.json", MaximumReceiptFinalityBytes, &proof)
	if err != nil {
		return nil, err
	}
	if !found {
		capture.rpc = newReceiptCollectorRpc(config.RpcUrl)
		capture.rpc.finality = true
		if config.RetryWindowSeconds != 0 {
			capture.rpc.retryWindow = time.Duration(config.RetryWindowSeconds) * time.Second
		}
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
		if genesis != checkpoint.Genesis {
			return nil, errors.New("native capture node genesis differs from the pinned checkpoint")
		}
		collected, err := capture.collect(ctx, collection, checkpoint, anchor, config.MaximumDescendantHeaders)
		if err != nil {
			return nil, err
		}
		proof = *collected
	}
	reconciliation, err := VerifyReceiptFinality(ctx, archive, collection, checkpoint, &proof)
	if err != nil {
		return nil, err
	}
	if proof.Schema != ReceiptFinalityDescendantProofSchema || reconciliation.NativeCertified == nil || reconciliation.NativeCertified.Number-collection.Observations.NativeFinalized.Number > config.MaximumDescendantHeaders {
		return nil, errors.New("native capture retained proof exceeds its pinned descendant window")
	}
	raw, err := json.Marshal(&proof)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaximumReceiptFinalityBytes {
		return nil, errors.New("native capture proof exceeds its publication byte bound")
	}
	if _, err := publishFile(ctx, store, "proof.json", raw); err != nil {
		return nil, err
	}
	return &ReceiptFinalityCaptureResult{Schema: ReceiptFinalityCaptureSchema, Admission: "unapproved_checkpoint_capture", Source: config.Source, ConfigHash: objectDigest(config),
		Proof: FileReference{Path: filepath.Join(directory, "proof.json"), Sha256: digest(raw)}, Reconciliation: reconciliation}, nil
}

// Walk backward by exact parent hashes, then replay forward to discover every
// authority signal before choosing certificates. A later certificate may cover
// an ordinary boundary, but cannot skip an outgoing-set enactment certificate.
func (self *finalityCapture) collect(ctx context.Context, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, anchor *nativeFinalityHeader, maximumDescendants uint64) (*ReceiptFinalityProof, error) {
	segments, err := self.collectWindow(ctx, collection.Observations.NativeFinalized, checkpoint, anchor, maximumDescendants)
	if err != nil {
		return nil, err
	}
	return &ReceiptFinalityProof{Schema: ReceiptFinalityDescendantProofSchema, CollectionHash: collection.ContentHash, CheckpointHash: checkpoint.Hash(), Segments: segments}, nil
}

// Native execution and receipt recovery share exact ancestry discovery and
// outgoing-set certificates. Only their independently bound contexts differ.
func (self *finalityCapture) collectWindow(ctx context.Context, boundary ObservedBlockIdentity, checkpoint *NativeFinalityCheckpoint, anchor *nativeFinalityHeader, maximumDescendants uint64) ([]GrandpaFinalitySegment, error) {
	var scales []string
	selected := boundary
	encodedBytes := len(checkpoint.HeaderScale)
	for selected.Number > anchor.identity.Number {
		scale, header, err := self.header(ctx, selected.Hash)
		if err != nil {
			return nil, err
		}
		if header.identity != selected {
			return nil, self.invalid(errors.New("native capture selected header number differs"))
		}
		encodedBytes += len(scale)
		if encodedBytes > MaximumReceiptFinalityBytes {
			return nil, self.invalid(errors.New("native capture header path exceeds the proof byte bound"))
		}
		scales = append(scales, scale)
		selected = ObservedBlockIdentity{Number: selected.Number - 1, Hash: header.parent}
	}
	if selected != anchor.identity {
		return nil, self.invalid(errors.New("native capture ancestry does not reach the pinned checkpoint"))
	}
	slices.Reverse(scales)
	segments := []GrandpaFinalitySegment{}
	budget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
	if _, err := budget.header(checkpoint.HeaderScale); err != nil {
		return nil, self.invalid(err)
	}
	setId, authorities, pending := checkpoint.SetId, checkpoint.Authorities, checkpoint.PendingChange
	var segment []string
	cursor := anchor
	for index := uint64(0); index < boundary.Number-anchor.identity.Number+maximumDescendants; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var scale string
		if index < uint64(len(scales)) {
			scale = scales[index]
		} else {
			hash, err := self.blockHash(ctx, cursor.identity.Number+1)
			if err != nil {
				return nil, fmt.Errorf("native capture descendant unavailable within pinned window: %w", err)
			}
			var errHeader error
			scale, _, errHeader = self.header(ctx, hash)
			if errHeader != nil {
				return nil, errHeader
			}
		}
		header, err := budget.header(scale)
		if err != nil {
			return nil, self.invalid(err)
		}
		if header.parent != cursor.identity.Hash || header.identity.Number != cursor.identity.Number+1 {
			return nil, self.invalid(errors.New("native capture descendant is disconnected from the selected collection"))
		}
		change, err := header.scheduledChange()
		if err != nil {
			return nil, self.invalid(err)
		}
		if change != nil {
			if pending != nil {
				return nil, self.invalid(errors.New("native capture authority schedules overlap"))
			}
			pending = change
		}
		segment = append(segment, scale)
		cursor = header
		enactment := pending != nil && header.identity.Number == pending.EnactmentNumber
		if !enactment && header.identity.Number < boundary.Number {
			continue
		}
		var verifyCertificate func(string) error
		if self.nativeExecution {
			verifyCertificate = func(encoded string) error {
				validationBudget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
				justification, err := validationBudget.justification(encoded)
				if err != nil {
					return err
				}
				_, err = verifyGrandpaCertificate(ctx, justification, header, setId, authorities, pending)
				return err
			}
		}
		certificate, err := self.certificate(ctx, header.identity.Hash, scale, verifyCertificate)
		if err != nil {
			return nil, err
		}
		if certificate == "" {
			if enactment {
				return nil, &nativeFinalityUnavailableError{reason: "native capture requires the archive's exact outgoing GRANDPA enactment justification; checkpoint authority remains unknown"}
			}
			continue
		}
		justification, err := budget.justification(certificate)
		if err != nil {
			return nil, self.invalid(err)
		}
		if !self.nativeExecution {
			if _, err := verifyGrandpaCertificate(ctx, justification, header, setId, authorities, pending); err != nil {
				return nil, err
			}
		}
		segments = append(segments, GrandpaFinalitySegment{Headers: segment, JustificationScale: certificate})
		if len(segments) > maximumGrandpaCertificates {
			return nil, self.invalid(errors.New("native capture certificate count exceeds the proof bound"))
		}
		segment = nil
		if header.identity.Number >= boundary.Number {
			return segments, nil
		}
		if enactment {
			if setId == ^uint64(0) {
				return nil, self.invalid(errors.New("native capture authority set ID overflows"))
			}
			setId++
			authorities, pending = pending.Authorities, nil
		}
	}
	return nil, &nativeFinalityUnavailableError{reason: "native capture found no stored GRANDPA justification within the pinned descendant window; retain partial evidence and obtain archive capability or a separately pinned wider capture"}
}
