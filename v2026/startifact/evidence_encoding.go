// One bounded canonical encoder serves both server publication and simulator
// verification. A publication hashes and owns the same emitted payload bytes.
package startifact

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
)

const evidenceCanonicalChunkBytes = 64 * 1024

// Callers must validate the same unchanged payload with json.Valid immediately
// before this call. This is encoding only, never authentication or admission.
// Preserve string contents and existing escapes byte-for-byte; only outside
// whitespace and the standard encoder's literal Html escapes are transformed.
// Buffering bounds both retained bytes and downstream writes for dense escapes.
func WriteValidatedEvidencePayload(writer io.Writer, payload []byte) error {
	output := bufio.NewWriterSize(writer, evidenceCanonicalChunkBytes)
	emit := func(value []byte) error {
		for len(value) != 0 {
			size := min(len(value), evidenceCanonicalChunkBytes)
			written, err := output.Write(value[:size])
			if err != nil {
				return err
			}
			if written != size {
				return io.ErrShortWrite
			}
			value = value[size:]
		}
		return nil
	}
	start := 0
	quoted, escaped := false, false
	const digits = "0123456789abcdef"
	var replacement [6]byte
	for index := 0; index < len(payload); index++ {
		value := payload[index]
		if quoted {
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
				continue
			}
			if value == '"' {
				quoted = false
				continue
			}
		} else {
			if value == '"' {
				quoted = true
				continue
			}
			if value == ' ' || value == '\t' || value == '\r' || value == '\n' {
				if err := emit(payload[start:index]); err != nil {
					return err
				}
				start = index + 1
				continue
			}
		}
		if value == '<' || value == '>' || value == '&' {
			if err := emit(payload[start:index]); err != nil {
				return err
			}
			replacement = [6]byte{'\\', 'u', '0', '0', digits[value>>4], digits[value&15]}
			if err := emit(replacement[:]); err != nil {
				return err
			}
			start = index + 1
		} else if value == 0xe2 && index+2 < len(payload) && payload[index+1] == 0x80 && payload[index+2]&^1 == 0xa8 {
			if err := emit(payload[start:index]); err != nil {
				return err
			}
			replacement = [6]byte{'\\', 'u', '2', '0', '2', digits[payload[index+2]&15]}
			if err := emit(replacement[:]); err != nil {
				return err
			}
			index += 2
			start = index + 1
		}
	}
	if err := emit(payload[start:]); err != nil {
		return err
	}
	return output.Flush()
}

// Marshal the actual metadata type, replacing only its nil payload token.
// String escaping and future envelope fields stay owned by encoding/json.
func evidenceWireFraming(envelope *EvidenceEnvelope) ([]byte, []byte, error) {
	metadata := *envelope
	metadata.Payload = nil
	encoded, err := json.Marshal(&metadata)
	if err != nil {
		return nil, nil, err
	}
	const marker = "\"payload\":null"
	index := bytes.Index(encoded, []byte(marker))
	if index < 0 {
		return nil, nil, errors.New("evidence payload framing is missing")
	}
	start := index + len(marker) - len("null")
	return encoded[:start], encoded[start+len("null"):], nil
}

// The caller has freshly validated identity and grammar for this invocation.
func evidenceDigestFromValidatedPayload(envelope *EvidenceEnvelope) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	unsigned := *envelope
	unsigned.ContentHash, unsigned.Signature = "", ""
	prefix, suffix, err := evidenceWireFraming(&unsigned)
	if err != nil {
		return digest, err
	}
	hash := sha256.New()
	_, _ = hash.Write(prefix)
	if err := WriteValidatedEvidencePayload(hash, envelope.Payload); err != nil {
		return digest, err
	}
	_, _ = hash.Write(suffix)
	copy(digest[:], hash.Sum(digest[:0]))
	return digest, nil
}

// A single canonical traversal feeds the returned wire and its unsigned hash.
// No separately marshaled unsigned envelope or canonical payload is allocated.
// The wire is never returned before fresh hash and signature authentication.
func evidenceBytesFromValidatedPayload(envelope *EvidenceEnvelope) ([]byte, error) {
	unsigned := *envelope
	unsigned.ContentHash, unsigned.Signature = "", ""
	unsignedPrefix, unsignedSuffix, err := evidenceWireFraming(&unsigned)
	if err != nil {
		return nil, err
	}
	prefix, suffix, err := evidenceWireFraming(envelope)
	if err != nil {
		return nil, err
	}
	// Own the initial wire directly. Buffer.Grow's append/make path allocates
	// another full-sized temporary when compiler instrumentation is enabled.
	wire := bytes.NewBuffer(make([]byte, 0, len(prefix)+len(envelope.Payload)+len(suffix)))
	_, _ = wire.Write(prefix)
	hash := sha256.New()
	_, _ = hash.Write(unsignedPrefix)
	if err := WriteValidatedEvidencePayload(io.MultiWriter(wire, hash), envelope.Payload); err != nil {
		return nil, err
	}
	_, _ = hash.Write(unsignedSuffix)
	_, _ = wire.Write(suffix)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(digest[:0]))
	if err := verifyEvidenceSignature(envelope, digest); err != nil {
		return nil, err
	}
	return wire.Bytes(), nil
}
