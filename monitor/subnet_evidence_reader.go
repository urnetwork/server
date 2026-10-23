// The §22.8 evidence sources. The validator evidence journal is replayed from
// an enrolled start proven to precede the journal; its activations and
// write-once anchors are read back at the finalized checkpoint. Public history
// is read through the actual typed routes at both enrolled origins: the paged
// /sn/artifacts index, /sn/artifact by content hash, and anchored payload
// bytes as attempt-stream metadata. Every page and object is checked
// independently of the server's own validation.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/stabi"
)

// One published dual-key activation, as its event and getter report it.
type subnetEvidenceActivation struct {
	digest [32]byte
	hotkey [32]byte
	noId   uint64
	epoch  uint64
	block  uint64
}

// One write-once evidence commitment, as its event reports it.
type subnetEvidenceAnchor struct {
	slot         [32]byte
	noId         uint64
	epoch        uint64
	headerHash   [32]byte
	payloadHash  [32]byte
	censusHash   [32]byte
	payloadBytes uint64
	block        uint64
}

// Frontier and replayed journal facts for one enrolled journal and start.
// Safe for concurrent use; the lock is never held across a read.
type subnetEvidenceJournal struct {
	stateLock   sync.Mutex
	journal     common.Address
	fromBlock   uint64
	proven      bool
	nextBlock   uint64
	activations []subnetEvidenceActivation
	anchors     map[[32]byte]subnetEvidenceAnchor
	duplicates  int
	malformed   int
}

func newSubnetEvidenceJournal() *subnetEvidenceJournal {
	return &subnetEvidenceJournal{anchors: map[[32]byte]subnetEvidenceAnchor{}}
}

// The frontier position, resetting on a changed journal or start.
func (self *subnetEvidenceJournal) Position(journal common.Address, fromBlock uint64) (bool, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.journal != journal || self.fromBlock != fromBlock {
		self.journal, self.fromBlock, self.proven, self.nextBlock = journal, fromBlock, false, fromBlock
		self.activations, self.anchors, self.duplicates, self.malformed = nil, map[[32]byte]subnetEvidenceAnchor{}, 0, 0
	}
	return self.proven, self.nextBlock
}

// Records that the enrolled start precedes the journal.
func (self *subnetEvidenceJournal) Prove(journal common.Address, fromBlock uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.journal == journal && self.fromBlock == fromBlock {
		self.proven = true
	}
}

// Commits contiguous windows after the checkpoint recheck.
func (self *subnetEvidenceJournal) Apply(journal common.Address, fromBlock uint64, windows []*subnetReplayLogWindow, decoder *subnetEvidenceDecoder) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.journal != journal || self.fromBlock != fromBlock || !self.proven || len(windows) == 0 || self.nextBlock != windows[0].from {
		return false
	}
	for i, window := range windows {
		if 0 < i && windows[i-1].to+1 != window.from {
			return false
		}
	}
	for _, window := range windows {
		for i := range window.logs {
			decoder.apply(self, &window.logs[i])
		}
		self.nextBlock = window.to + 1
	}
	return true
}

// A consistent copy for evaluation, including the not yet committed windows.
func (self *subnetEvidenceJournal) Snapshot(windows []*subnetReplayLogWindow, decoder *subnetEvidenceDecoder) (uint64, *subnetEvidenceJournal) {
	self.stateLock.Lock()
	copy := &subnetEvidenceJournal{
		journal: self.journal, fromBlock: self.fromBlock, proven: self.proven, nextBlock: self.nextBlock,
		activations: append([]subnetEvidenceActivation(nil), self.activations...),
		anchors:     map[[32]byte]subnetEvidenceAnchor{}, duplicates: self.duplicates, malformed: self.malformed,
	}
	for slot, anchor := range self.anchors {
		copy.anchors[slot] = anchor
	}
	self.stateLock.Unlock()
	for _, window := range windows {
		if window.from != copy.nextBlock {
			break
		}
		for i := range window.logs {
			decoder.apply(copy, &window.logs[i])
		}
		copy.nextBlock = window.to + 1
	}
	return copy.nextBlock, copy
}

// Decodes journal events by topic with the pinned binding after an exact
// shape check; the binding leaves fields zero when data is absent.
type subnetEvidenceDecoder struct {
	binding *stabi.STValidatorEvidence
	topics  map[common.Hash]string
	shapes  map[string]subnetSettlementEventShape
}

func newSubnetEvidenceDecoder(contracts subnetSettlementContracts) *subnetEvidenceDecoder {
	topics := map[common.Hash]string{}
	shapes := map[string]subnetSettlementEventShape{}
	for _, name := range []string{"ActivationPublished", "EvidenceCommitted"} {
		event := contracts.evidence.Events[name]
		topics[event.ID] = name
		shape := subnetSettlementEventShape{topics: 1}
		for _, input := range event.Inputs {
			if input.Indexed {
				shape.topics += 1
			} else {
				shape.dataBytes += 32
			}
		}
		shapes[name] = shape
	}
	return &subnetEvidenceDecoder{binding: stabi.NewSTValidatorEvidence(), topics: topics, shapes: shapes}
}

// Applies one log to a journal owned by the caller (locked or a copy).
func (self *subnetEvidenceDecoder) apply(journal *subnetEvidenceJournal, log *types.Log) {
	name, known := self.topics[log.Topics[0]]
	if !known {
		return
	}
	shape := self.shapes[name]
	if len(log.Topics) != shape.topics || len(log.Data) != shape.dataBytes {
		journal.malformed += 1
		return
	}
	switch name {
	case "ActivationPublished":
		event, err := self.binding.UnpackActivationPublishedEvent(log)
		if err != nil {
			journal.malformed += 1
			return
		}
		journal.activations = append(journal.activations, subnetEvidenceActivation{digest: event.ActivationHash, hotkey: event.Hotkey, noId: event.NoId, epoch: event.Epoch, block: log.BlockNumber})
	case "EvidenceCommitted":
		event, err := self.binding.UnpackEvidenceCommittedEvent(log)
		if err != nil {
			journal.malformed += 1
			return
		}
		if _, duplicate := journal.anchors[event.Slot]; duplicate {
			journal.duplicates += 1
			return
		}
		journal.anchors[event.Slot] = subnetEvidenceAnchor{
			slot: event.Slot, noId: event.NoId, epoch: event.Epoch, headerHash: event.HeaderHash,
			payloadHash: event.PayloadHash, censusHash: event.CensusHash, payloadBytes: event.PayloadBytes, block: log.BlockNumber,
		}
	}
}

// Reads the next journal windows and verifies their blocks; commits nothing.
func readSubnetEvidenceJournalWindows(ctx context.Context, rpc *subnetReplayRpc, journal *subnetEvidenceJournal, address common.Address, fromBlock uint64, checkpoint subnetReplayEvmCheckpoint, settings *SubnetReplaySettings) ([]*subnetReplayLogWindow, error) {
	proven, next := journal.Position(address, fromBlock)
	if checkpoint.number+1 < next {
		return nil, newSubnetReplayUnknown("finality-regressed", nil)
	}
	if !proven {
		if err := proveSubnetReplayJournalStart(ctx, rpc, address, fromBlock); err != nil {
			return nil, err
		}
		journal.Prove(address, fromBlock)
	}
	windows := subnetReplayLogWindows(next, checkpoint, settings)
	if len(windows) == 0 {
		return nil, nil
	}
	if err := readSubnetReplayLogs(ctx, rpc, address, settings.LogMaximumEntriesPerRange, windows...); err != nil {
		return nil, err
	}
	if err := verifySubnetReplayLogBlocks(ctx, rpc, windows...); err != nil {
		return nil, err
	}
	return windows, nil
}

// One object of the public payout artifact history index.
type subnetEvidenceIndexEntry struct {
	suffix string
	epoch  uint64
	noId   uint64
	hash   [32]byte
	size   int64
}

// One origin's complete or explicitly incomplete index.
type subnetEvidenceIndex struct {
	entries  []subnetEvidenceIndexEntry
	complete bool
	reason   string
	// A served page that contradicts the index contract: accepted tampering,
	// a forked or duplicate page, or a key outside the pinned history.
	violation string
}

// Pages through /sn/artifacts for the pinned deployment within the page bound.
// Keys are validated independently: one shared prefix, the pinned deployment
// and netuid, numeric epoch and operator segments, a content-hash filename
// equal to its content_hash, strict ordering and exact cursor continuity.
func readSubnetEvidenceIndex(ctx context.Context, client grafanaHTTPClient, origin string, pin *SubnetReplayPin, settings *SubnetReplaySettings) (subnetEvidenceIndex, error) {
	index := subnetEvidenceIndex{}
	marker := "/st/v1/history/" + pin.deploymentId + "/" + strconv.FormatUint(uint64(pin.netuid), 10) + "/"
	prefix := ""
	after := ""
	previous := ""
	for page := 0; page < settings.HistoryMaximumPages; page++ {
		query := url.Values{"deployment_id": {pin.deploymentId}, "netuid": {strconv.FormatUint(uint64(pin.netuid), 10)}, "limit": {strconv.Itoa(settings.HistoryPageObjects)}}
		if after != "" {
			query.Set("after", after)
		}
		response, err := readSubnetReplayApi(ctx, client, origin, "/sn/artifacts", query, settings)
		if err != nil {
			if reason := subnetReplayUnknownReason(err); reason != "" {
				index.reason = reason
				return index, nil
			}
			return index, err
		}
		if response.status != 200 || response.contentType != "application/json" {
			index.reason = "index-unavailable"
			return index, nil
		}
		var body struct {
			Schema  string `json:"schema"`
			Objects []struct {
				Key         string `json:"key"`
				Size        int64  `json:"size"`
				ContentHash string `json:"content_hash"`
			} `json:"objects"`
			More      bool   `json:"more"`
			NextAfter string `json:"next_after"`
		}
		decoder := json.NewDecoder(bytes.NewReader(response.body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || body.Schema != "urnetwork-payout-artifact-history-v1" {
			index.reason = "index-malformed"
			return index, nil
		}
		if _, err := decoder.Token(); err != io.EOF {
			index.reason = "index-malformed"
			return index, nil
		}
		if settings.HistoryPageObjects < len(body.Objects) || (body.More && len(body.Objects) != settings.HistoryPageObjects) {
			index.violation = "page-cardinality"
			return index, nil
		}
		for _, object := range body.Objects {
			at := strings.Index(object.Key, marker)
			if at < 0 || (prefix != "" && object.Key[:at] != prefix) || object.Key <= previous || object.Size < 0 {
				index.violation = "page-order-or-scope"
				return index, nil
			}
			prefix = object.Key[:at]
			segments := strings.Split(object.Key[at+len(marker):], "/")
			if len(segments) != 3 {
				index.violation = "page-path-depth"
				return index, nil
			}
			epoch, epochErr := strconv.ParseUint(segments[0], 10, 64)
			noId, noErr := strconv.ParseUint(segments[1], 10, 64)
			hashHex := strings.TrimSuffix(segments[2], ".json")
			decoded, hashErr := hex.DecodeString(hashHex)
			if epochErr != nil || noErr != nil || noId == 0 || segments[0] != strconv.FormatUint(epoch, 10) || segments[1] != strconv.FormatUint(noId, 10) || hashErr != nil || len(decoded) != 32 || segments[2] != hashHex+".json" || hashHex != strings.ToLower(hashHex) || object.ContentHash != "sha256:"+hashHex {
				index.violation = "page-key-grammar"
				return index, nil
			}
			var hash [32]byte
			copy(hash[:], decoded)
			index.entries = append(index.entries, subnetEvidenceIndexEntry{suffix: object.Key[at:], epoch: epoch, noId: noId, hash: hash, size: object.Size})
			previous = object.Key
		}
		if !body.More {
			if body.NextAfter != "" {
				index.violation = "page-cursor"
				return index, nil
			}
			index.complete = true
			return index, nil
		}
		if body.NextAfter != previous {
			index.violation = "page-cursor"
			return index, nil
		}
		after = body.NextAfter
	}
	index.reason = "index-page-bound"
	return index, nil
}

// The outcome of one content-addressed object read at one origin.
const (
	subnetEvidenceObjectVerified    = "verified"
	subnetEvidenceObjectMissing     = "missing"
	subnetEvidenceObjectUnavailable = "unavailable"
	subnetEvidenceObjectTampered    = "tampered"
)

// Reads anchored payload bytes as attempt-stream metadata. Served bytes must
// be exactly the anchored length and digest; a 404 is a definitive absence
// from that origin's store, while any other failure is only unavailability.
func readSubnetEvidencePayload(ctx context.Context, client grafanaHTTPClient, origin string, anchor subnetEvidenceAnchor, settings *SubnetReplaySettings) (string, error) {
	hash := "0x" + hex.EncodeToString(anchor.payloadHash[:])
	response, err := readSubnetReplayApi(ctx, client, origin, "/sn/attempt-artifact", url.Values{"kind": {"metadata"}, "hash": {hash}}, settings)
	if err != nil {
		if subnetReplayUnknownReason(err) != "" {
			return subnetEvidenceObjectUnavailable, nil
		}
		return "", err
	}
	switch {
	case response.status == 404:
		return subnetEvidenceObjectMissing, nil
	case response.status != 200:
		return subnetEvidenceObjectUnavailable, nil
	}
	digest := sha256.Sum256(response.body)
	if response.contentType != "application/json" || response.etag != `"`+hash+`"` || uint64(len(response.body)) != anchor.payloadBytes || digest != anchor.payloadHash {
		return subnetEvidenceObjectTampered, nil
	}
	return subnetEvidenceObjectVerified, nil
}

// Reads one chain-committed payout artifact by content hash and returns its
// outcome and wire digest. Verification is the full canonical rebuild; a
// verified artifact whose root differs from the chain is §22.7's
// counterexample, not a serving-integrity fault.
func readSubnetEvidenceArtifact(ctx context.Context, client grafanaHTTPClient, origin string, artifactHash [32]byte, settings *SubnetReplaySettings) (string, [32]byte, *payoutartifact.Artifact, error) {
	contentHash := "sha256:" + hex.EncodeToString(artifactHash[:])
	response, err := readSubnetReplayApi(ctx, client, origin, "/sn/artifact", url.Values{"hash": {contentHash}}, settings)
	if err != nil {
		if subnetReplayUnknownReason(err) != "" {
			return subnetEvidenceObjectUnavailable, [32]byte{}, nil, nil
		}
		return "", [32]byte{}, nil, err
	}
	switch {
	case response.status == 404:
		return subnetEvidenceObjectMissing, [32]byte{}, nil, nil
	case response.status != 200:
		return subnetEvidenceObjectUnavailable, [32]byte{}, nil, nil
	}
	if response.contentType != "application/json" || response.etag != `"`+contentHash+`"` {
		return subnetEvidenceObjectTampered, [32]byte{}, nil, nil
	}
	artifact, err := decodeSubnetEvidenceArtifact(ctx, response.body)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", [32]byte{}, nil, err
		}
		return subnetEvidenceObjectTampered, [32]byte{}, nil, nil
	}
	if artifact.ContentHash != contentHash {
		return subnetEvidenceObjectTampered, [32]byte{}, nil, nil
	}
	return subnetEvidenceObjectVerified, sha256.Sum256(response.body), artifact, nil
}

// Full canonical decode and rebuild under the run owner.
func decodeSubnetEvidenceArtifact(ctx context.Context, raw []byte) (*payoutartifact.Artifact, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return payoutartifact.DecodeWithContext(ctx, raw)
}
