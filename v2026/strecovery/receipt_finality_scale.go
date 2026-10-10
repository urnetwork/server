// Bounded SCALE decoding retains exact native header bytes as hash authority.
// The admitted codec is the reviewed u32/BlakeTwo256 Subtensor header profile.
package strecovery

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const maximumNativeFinalityHeaders = 4096
const maximumNativeFinalityHeaderBytes = 256 * 1024
const maximumNativeFinalityDigests = 256
const maximumNativeFinalityDigestBytes = 64 * 1024
const maximumGrandpaAuthorities = 1024

// A decoder owns only an offset into invocation-owned, bounded bytes.
type finalityScaleReader struct {
	data   []byte
	offset int
}

// Truncation is terminal; no partial field escapes the decoder.
func (self *finalityScaleReader) take(count int) ([]byte, error) {
	if count < 0 || count > len(self.data)-self.offset {
		return nil, errors.New("native finality SCALE field is truncated")
	}
	result := self.data[self.offset : self.offset+count]
	self.offset += count
	return result, nil
}

// All compact fields in this codec are u32 values or bounded vector lengths.
// Canonical encodings prevent alternate byte representations of one number.
func (self *finalityScaleReader) compact() (uint64, error) {
	first, err := self.take(1)
	if err != nil {
		return 0, err
	}
	switch first[0] & 3 {
	case 0:
		return uint64(first[0] >> 2), nil
	case 1:
		rest, err := self.take(1)
		if err != nil {
			return 0, err
		}
		value := uint64(uint16(first[0])|uint16(rest[0])<<8) >> 2
		if value < 64 {
			return 0, errors.New("noncanonical native finality SCALE compact")
		}
		return value, nil
	case 2:
		rest, err := self.take(3)
		if err != nil {
			return 0, err
		}
		value := uint64(uint32(first[0])|uint32(rest[0])<<8|uint32(rest[1])<<16|uint32(rest[2])<<24) >> 2
		if value < 16384 {
			return 0, errors.New("noncanonical native finality SCALE compact")
		}
		return value, nil
	default:
		if first[0] != 3 {
			return 0, errors.New("native finality SCALE compact exceeds u32")
		}
		raw, err := self.take(4)
		if err != nil {
			return 0, err
		}
		value := uint64(binary.LittleEndian.Uint32(raw))
		if value < 1<<30 {
			return 0, errors.New("noncanonical native finality SCALE compact")
		}
		return value, nil
	}
}

// Every externally supplied encoded field consumes one shared budget before
// decoding, including callers that bypass the private file loaders.
type nativeFinalityBudget struct {
	remaining int
	headers   int
}

// Hex inputs are canonical; byte/count limits precede allocations or hashing.
func (self *nativeFinalityBudget) decode(encoded string, maximum int) ([]byte, error) {
	if len(encoded) < 4 || len(encoded)%2 != 0 || len(encoded) > 2+2*maximum || len(encoded) > self.remaining ||
		!strings.HasPrefix(encoded, "0x") || strings.ToLower(encoded) != encoded {
		return nil, errors.New("native finality hex or shared byte bound differs")
	}
	self.remaining -= len(encoded)
	raw, err := hex.DecodeString(encoded[2:])
	if err != nil {
		return nil, errors.New("native finality hex is malformed")
	}
	return raw, nil
}

// Digest payloads borrow their enclosing retained header bytes.
type nativeFinalityDigest struct {
	kind    byte
	engine  string
	payload []byte
}

// Native height is independent of EVM height. StateRoot remains available for
// future storage proofs but confers no authority without an approved anchor.
type nativeFinalityHeader struct {
	identity  ObservedBlockIdentity
	parent    string
	stateRoot string
	digests   []nativeFinalityDigest
}

// Standalone headers must consume all bytes; justification headers are decoded
// consecutively by the same reader and share the same global header count.
func (self *nativeFinalityBudget) header(encoded string) (*nativeFinalityHeader, error) {
	raw, err := self.decode(encoded, maximumNativeFinalityHeaderBytes)
	if err != nil {
		return nil, err
	}
	reader := finalityScaleReader{data: raw}
	header, err := self.readHeader(&reader)
	if err != nil {
		return nil, err
	}
	if reader.offset != len(raw) {
		return nil, errors.New("native finality header has trailing bytes")
	}
	return header, nil
}

// Hash the complete SCALE header, including the canonical compact number and
// each typed digest. Hash substrings and JSON projections never select blocks.
func (self *nativeFinalityBudget) readHeader(reader *finalityScaleReader) (*nativeFinalityHeader, error) {
	self.headers++
	if self.headers > maximumNativeFinalityHeaders {
		return nil, errors.New("native finality shared header count exceeded")
	}
	start := reader.offset
	parent, err := reader.take(32)
	if err != nil {
		return nil, err
	}
	number, err := reader.compact()
	if err != nil {
		return nil, err
	}
	roots, err := reader.take(64)
	if err != nil {
		return nil, err
	}
	count, err := reader.compact()
	if err != nil || count > maximumNativeFinalityDigests {
		return nil, errors.New("native finality digest count differs")
	}
	header := &nativeFinalityHeader{identity: ObservedBlockIdentity{Number: number}, parent: "0x" + hex.EncodeToString(parent), stateRoot: "0x" + hex.EncodeToString(roots[:32])}
	for range count {
		tag, err := reader.take(1)
		if err != nil {
			return nil, err
		}
		digest := nativeFinalityDigest{kind: tag[0]}
		switch digest.kind {
		case 4, 5, 6:
			engine, err := reader.take(4)
			if err != nil {
				return nil, err
			}
			digest.engine = string(engine)
		case 0, 8:
		default:
			return nil, errors.New("native finality digest kind is outside the reviewed profile")
		}
		if digest.kind != 8 {
			length, err := reader.compact()
			if err != nil || length > maximumNativeFinalityDigestBytes {
				return nil, errors.New("native finality digest byte bound differs")
			}
			digest.payload, err = reader.take(int(length))
			if err != nil {
				return nil, err
			}
		}
		if reader.offset-start > maximumNativeFinalityHeaderBytes {
			return nil, errors.New("native finality header byte bound exceeded")
		}
		header.digests = append(header.digests, digest)
	}
	hash := blake2b.Sum256(reader.data[start:reader.offset])
	header.identity.Hash = "0x" + hex.EncodeToString(hash[:])
	return header, nil
}

// Scheduled changes use codec index 1, then (authority vector, u32 delay).
// Forced changes, disabled voters, pause/resume and duplicate logs need a new
// independently approved checkpoint; none silently inherits the old set.
func (self *nativeFinalityHeader) scheduledChange() (*GrandpaScheduledChange, error) {
	var change *GrandpaScheduledChange
	for _, digest := range self.digests {
		if digest.engine != "FRNK" {
			continue
		}
		if digest.kind != 4 || change != nil || len(digest.payload) == 0 || digest.payload[0] != 1 {
			return nil, errors.New("native finality requires a new approved checkpoint for unsupported or duplicate GRANDPA authority logs")
		}
		reader := finalityScaleReader{data: digest.payload, offset: 1}
		count, err := reader.compact()
		if err != nil || count == 0 || count > maximumGrandpaAuthorities {
			return nil, errors.New("native finality scheduled authority count differs")
		}
		change = &GrandpaScheduledChange{ScheduledAt: self.identity.Number, Authorities: make([]GrandpaAuthority, int(count))}
		for index := range change.Authorities {
			raw, err := reader.take(40)
			if err != nil {
				return nil, err
			}
			change.Authorities[index] = GrandpaAuthority{PublicKey: "0x" + hex.EncodeToString(raw[:32]), Weight: binary.LittleEndian.Uint64(raw[32:])}
		}
		raw, err := reader.take(4)
		if err != nil || reader.offset != len(reader.data) {
			return nil, errors.New("native finality scheduled change delay is truncated or has trailing bytes")
		}
		change.EnactmentNumber = self.identity.Number + uint64(binary.LittleEndian.Uint32(raw))
		if change.EnactmentNumber > uint64(^uint32(0)) {
			return nil, errors.New("native finality scheduled change height overflows u32")
		}
		if _, _, err := grandpaAuthorityWeights(change.Authorities); err != nil {
			return nil, err
		}
	}
	return change, nil
}

// A finalized native header must contain exactly one reviewed Frontier hash
// post-log. Variant 1's complete vector is bounded and consumed, never used as
// an alternate authority for transaction membership.
func (self *nativeFinalityHeader) frontierHash() (string, uint8, error) {
	var hash string
	var variant uint8
	for _, digest := range self.digests {
		if digest.engine != "fron" {
			continue
		}
		if hash != "" || digest.kind != 4 || len(digest.payload) < 33 || (digest.payload[0] != 1 && digest.payload[0] != 3) {
			return "", 0, errors.New("native finality Frontier post-log is absent, duplicated or unsupported")
		}
		variant = digest.payload[0]
		hash = "0x" + hex.EncodeToString(digest.payload[1:33])
		if bytes.Equal(digest.payload[1:33], make([]byte, 32)) {
			return "", 0, errors.New("native finality Frontier hash is zero")
		}
		reader := finalityScaleReader{data: digest.payload, offset: 33}
		if variant == 1 {
			count, err := reader.compact()
			if err != nil || count > 2048 || count*32 != uint64(len(reader.data)-reader.offset) {
				return "", 0, errors.New("native finality Frontier transaction vector differs")
			}
			_, _ = reader.take(int(count) * 32)
		}
		if reader.offset != len(reader.data) {
			return "", 0, errors.New("native finality Frontier post-log has trailing bytes")
		}
	}
	if hash == "" {
		return "", 0, fmt.Errorf("native finality header %s has no Frontier post-log", self.identity.Hash)
	}
	return hash, variant, nil
}
