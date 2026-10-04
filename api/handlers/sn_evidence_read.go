// Retrieval admits large plan carriers from a bounded header, then verifies
// the full original envelope and source before exposing any response bytes.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/urnetwork/server/startifact"
)

const snEvidenceHeaderBytes = 64 * 1024

const maximumSnEvidenceEmptyReads = 100

// A recognized typed header with invalid size or identity is a hard refusal,
// rather than an invitation to consume the ordinary fallback allowance.
var errSnEvidencePlanHeader = errors.New("invalid typed evidence header")

// One large response owns its buffers through the final write. Ordinary
// evidence never competes for this finite memory/concurrency owner.
var snEvidencePlanReadSlots = make(chan struct{}, 1)

var errSnEvidencePlanReadBusy = errors.New("large evidence read is busy")

// Request-scoped admission holds the large owner through authentication and
// response completion; no external call executes under a state lock.
type snEvidenceReadOperation struct {
	writer http.ResponseWriter
	cancel context.CancelFunc
	active bool
}

// Fixed overhead plus an explicit transfer/verification allowance has a
// finite ceiling; a caller's earlier deadline remains authoritative.
func snEvidencePlanReadDuration(size uint64) time.Duration {
	seconds := (size + 8*1024*1024 - 1) / (8 * 1024 * 1024)
	return min(20*time.Minute, 2*time.Minute+time.Duration(seconds)*time.Second)
}

// Only a validated large header extends the native per-request write timer.
// Refusal is immediate and does not consume another large body.
func (self *snEvidenceReadOperation) admit(ctx context.Context, size uint64) (context.Context, error) {
	if size <= maximumSnEvidenceBytes {
		return ctx, nil
	}
	if self.active || self.writer == nil {
		return nil, errors.New("large evidence read owner is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case snEvidencePlanReadSlots <- struct{}{}:
		self.active = true
	default:
		return nil, errSnEvidencePlanReadBusy
	}
	operationCtx, cancel := context.WithTimeout(ctx, snEvidencePlanReadDuration(size))
	self.cancel = cancel
	deadline, exists := operationCtx.Deadline()
	if !exists {
		return nil, errors.New("large evidence read has no finite deadline")
	}
	if err := http.NewResponseController(self.writer).SetWriteDeadline(deadline); err != nil {
		return nil, errors.Join(errors.New("large evidence write deadline unavailable"), err)
	}
	return operationCtx, nil
}

// Every handler exit releases the response owner, including read, close,
// authentication and write failures. The method is safe to call twice.
func (self *snEvidenceReadOperation) close() {
	if self.cancel != nil {
		self.cancel()
		self.cancel = nil
	}
	if self.active {
		<-snEvidencePlanReadSlots
		self.active = false
	}
}

// Cancellation is checked before every underlying read, including header
// admission. The surrounding read owner handles interruption and Close.
type snEvidenceContextReader struct {
	ctx         context.Context
	reader      io.Reader
	terminalErr error
	emptyReads  int
}

// A canceled request never consumes another store chunk.
func (self *snEvidenceContextReader) Read(value []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := self.reader.Read(value)
	if count < 0 || count > len(value) {
		count, err = 0, errors.New("evidence reader returned an invalid byte count")
	}
	if count == 0 && err == nil && len(value) != 0 {
		self.emptyReads++
		if self.emptyReads >= maximumSnEvidenceEmptyReads {
			err = io.ErrNoProgress
		}
	} else if count > 0 {
		self.emptyReads = 0
	}
	if err != nil && !errors.Is(err, io.EOF) {
		self.terminalErr = errors.Join(self.terminalErr, err)
	}
	return count, err
}

// The bounded prefix is retained once so decoder read-ahead cannot discard or
// substitute bytes. Noncanonical/unrecognized headers retain the ordinary cap.
func readSnEvidenceEnvelope(ctx context.Context, reader io.Reader) ([]byte, *startifact.EvidenceEnvelope, error) {
	if reader == nil {
		return nil, nil, errors.New("evidence reader is absent")
	}
	return readSnEvidenceEnvelopeWithAdmission(ctx, io.NopCloser(reader), nil)
}

// The endpoint supplies a request-scoped write/memory owner. Pure byte-reader
// callers leave admission nil. Takes ownership: cancellation interrupts the
// actual blob reader, and every outcome joins exactly one Close before return.
func readSnEvidenceEnvelopeWithAdmission(ctx context.Context, reader io.ReadCloser, admit func(context.Context, uint64) (context.Context, error)) (resultRaw []byte, resultEnvelope *startifact.EvidenceEnvelope, resultErr error) {
	if ctx == nil || reader == nil {
		return nil, nil, errors.New("evidence reader context is absent")
	}
	readCtx, cancelRead := context.WithCancel(ctx)
	closed := make(chan struct{})
	var closeErr error
	stopClose := context.AfterFunc(readCtx, func() {
		defer close(closed)
		closeErr = reader.Close()
	})
	var stopAdmission func() bool
	defer func() {
		if stopAdmission != nil {
			stopAdmission()
		}
		if stopClose() {
			closeErr = reader.Close()
		} else {
			<-closed
		}
		cancelRead()
		resultErr = errors.Join(resultErr, closeErr, ctx.Err())
		if resultErr != nil {
			resultRaw, resultEnvelope = nil, nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	owned := &snEvidenceContextReader{ctx: ctx, reader: reader}
	var prefix bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(io.LimitReader(owned, snEvidenceHeaderBytes), &prefix))
	maximum, headerErr := readSnEvidencePlanHeader(decoder)
	if owned.terminalErr != nil {
		return nil, nil, errors.Join(owned.terminalErr, ctx.Err())
	}
	if errors.Is(headerErr, errSnEvidencePlanHeader) {
		return nil, nil, headerErr
	}
	if headerErr != nil {
		maximum = maximumSnEvidenceBytes
	}
	maximum = max(maximum, uint64(maximumSnEvidenceBytes))
	if admit != nil && maximum > maximumSnEvidenceBytes {
		operationCtx, err := admit(ctx, maximum)
		if err != nil {
			return nil, nil, err
		}
		if operationCtx == nil {
			return nil, nil, errors.New("large evidence admission returned no context")
		}
		ctx = operationCtx
		stopAdmission = context.AfterFunc(ctx, cancelRead)
		owned.ctx = ctx
	}
	raw, err := readSnEvidenceBytes(io.MultiReader(bytes.NewReader(prefix.Bytes()), owned), int64(maximum))
	if err != nil {
		return nil, nil, errors.Join(err, ctx.Err())
	}
	var envelope startifact.EvidenceEnvelope
	typed := headerErr == nil
	if typed {
		err = decodeSnEvidenceObject(raw, &envelope, true)
	} else {
		err = json.Unmarshal(raw, &envelope)
	}
	if err != nil {
		return nil, nil, err
	}
	if !typed && (envelope.Kind == snEvidenceCampaignFileKind || envelope.Kind == snEvidenceSemanticFileKind) {
		var header snEvidenceFileHeader
		if json.Unmarshal(envelope.Payload, &header) == nil && snEvidencePlanPathBytes(header.Path) != 0 {
			typed = true
			if err := decodeSnEvidenceObject(raw, &envelope, true); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := startifact.VerifyEvidence(&envelope); err != nil {
		return nil, nil, err
	}
	if typed {
		if err := validateSnEvidencePlanFile(&envelope); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return raw, &envelope, nil
}

// Producer ordering puts envelope identity and payload metadata before data.
// A missing, repeated or reordered field never increases an allocation cap.
func readSnEvidencePlanHeader(decoder *json.Decoder) (uint64, error) {
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return 0, errors.New("evidence header is not an object")
	}
	fields := []string{"schema", "deployment_id", "chain_id", "genesis_hash", "netuid", "kind", "run_id", "created_at", "payload"}
	var kind, runId string
	for _, expected := range fields {
		key, err := decoder.Token()
		if err != nil || key != expected {
			return 0, errors.New("evidence header is not the canonical producer prefix")
		}
		if expected == "payload" {
			break
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return 0, err
		}
		switch expected {
		case "kind":
			if err := json.Unmarshal(value, &kind); err != nil {
				return 0, err
			}
		case "run_id":
			if err := json.Unmarshal(value, &runId); err != nil {
				return 0, err
			}
		}
	}
	if kind != snEvidenceCampaignFileKind && kind != snEvidenceSemanticFileKind {
		return 0, errors.New("evidence kind has no large plan owner")
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return 0, errors.New("evidence file header is not an object")
	}
	header := snEvidenceFileHeader{}
	fields = []string{"schema", "run_id"}
	if kind == snEvidenceCampaignFileKind {
		fields = append(fields, "scope")
	}
	fields = append(fields, "path", "content_hash", "size", "data")
	for _, expected := range fields {
		key, err := decoder.Token()
		if err != nil || key != expected {
			return 0, errors.New("evidence file header is not the canonical producer prefix")
		}
		if expected == "data" {
			maximum, err := snEvidenceFileReadBytes(kind, runId, header)
			if err != nil && snEvidencePlanPathBytes(header.Path) != 0 {
				return 0, errors.Join(errSnEvidencePlanHeader, err)
			}
			return maximum, err
		}
		var target any
		switch expected {
		case "schema":
			target = &header.Schema
		case "run_id":
			target = &header.RunId
		case "scope":
			target = &header.Scope
		case "path":
			target = &header.Path
		case "content_hash":
			target = &header.ContentHash
		case "size":
			target = &header.Size
		}
		if err := decoder.Decode(target); err != nil {
			return 0, err
		}
	}
	return 0, errors.New("evidence file header has no data boundary")
}
