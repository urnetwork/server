// The reviewed chain RPC serializes each complete native digest as SCALE hex.
// Reconstructing the canonical header and hashing it authenticates every field;
// RPC finality labels and runtime authority replies are never proof inputs.
package strecovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// The pinned SDK's Header and Digest serde contract has no optional fields.
type finalityCaptureHeader struct {
	ParentHash     string `json:"parentHash"`
	Number         string `json:"number"`
	StateRoot      string `json:"stateRoot"`
	ExtrinsicsRoot string `json:"extrinsicsRoot"`
	Digest         struct {
		Logs *[]string `json:"logs"`
	} `json:"digest"`
}

// Native u32 numbers and bounded digest lengths use canonical SCALE compact.
func captureCompact(value uint32) []byte {
	switch {
	case value < 64:
		return []byte{byte(value << 2)}
	case value < 16384:
		return binary.LittleEndian.AppendUint16(nil, uint16(value<<2|1))
	case value < 1<<30:
		return binary.LittleEndian.AppendUint32(nil, value<<2|2)
	default:
		return binary.LittleEndian.AppendUint32([]byte{3}, value)
	}
}

// The hash selector must equal the hash of the complete canonical SCALE bytes.
// Independent decoding also rejects malformed or unsupported digest variants.
func captureHeaderScale(raw json.RawMessage, hash string) (string, *nativeFinalityHeader, error) {
	var input finalityCaptureHeader
	if err := decodeJson(raw, &input); err != nil {
		return "", nil, err
	}
	if !canonicalHex(hash, 32) || !canonicalHex(input.ParentHash, 32) || !canonicalHex(input.StateRoot, 32) || !canonicalHex(input.ExtrinsicsRoot, 32) || input.Digest.Logs == nil || len(*input.Digest.Logs) > maximumNativeFinalityDigests {
		return "", nil, errors.New("native capture header fields are incomplete or exceed the codec bounds")
	}
	number, err := strconv.ParseUint(input.Number, 0, 32)
	if err != nil || input.Number != fmt.Sprintf("0x%x", number) {
		return "", nil, errors.New("native capture header number is not a canonical u32 hex quantity")
	}
	encoded, _ := hex.DecodeString(input.ParentHash[2:])
	encoded = append(encoded, captureCompact(uint32(number))...)
	roots, _ := hex.DecodeString(input.StateRoot[2:] + input.ExtrinsicsRoot[2:])
	encoded = append(encoded, roots...)
	encoded = append(encoded, captureCompact(uint32(len(*input.Digest.Logs)))...)
	for _, item := range *input.Digest.Logs {
		log, err := collectionHex(item, maximumNativeFinalityDigestBytes+10)
		if err != nil || len(encoded)+len(log) > maximumNativeFinalityHeaderBytes {
			return "", nil, errors.New("native capture digest or header byte bound differs")
		}
		encoded = append(encoded, log...)
	}
	scale := "0x" + hex.EncodeToString(encoded)
	budget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
	header, err := budget.header(scale)
	if err != nil {
		return "", nil, err
	}
	if header.identity.Hash != hash {
		return "", nil, errors.New("native capture complete header hash differs from its exact selector")
	}
	return scale, header, nil
}

// Rust's plain Vec<u8> and [u8;4] serialize as JSON number arrays, not strings.
// Explicit element decoding refuses null, base64 and truncated engine arrays.
func captureByteArray(raw json.RawMessage, maximum int) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || bytes.Contains(raw, []byte("null")) {
		return nil, errors.New("native capture justification requires the reviewed byte-array codec")
	}
	var numbers []uint16
	if err := json.Unmarshal(raw, &numbers); err != nil || len(numbers) > maximum {
		return nil, errors.New("native capture justification array exceeds its bound or is malformed")
	}
	result := make([]byte, len(numbers))
	for index, number := range numbers {
		if number > 255 {
			return nil, errors.New("native capture justification byte is outside u8")
		}
		result[index] = byte(number)
	}
	return result, nil
}

// A missing stored certificate is an observation, never proof of non-finality.
// Unknown engines cannot be substituted for GRANDPA; duplicate engines refuse.
func captureBlockCertificate(raw json.RawMessage, hash string) (string, string, error) {
	var input struct {
		Block struct {
			Header     json.RawMessage `json:"header"`
			Extrinsics json.RawMessage `json:"extrinsics"`
		} `json:"block"`
		Justifications json.RawMessage `json:"justifications"`
	}
	if err := decodeJson(raw, &input); err != nil {
		return "", "", err
	}
	scale, _, err := captureHeaderScale(input.Block.Header, hash)
	if err != nil {
		return "", "", err
	}
	if bytes.Equal(bytes.TrimSpace(input.Justifications), []byte("null")) {
		return scale, "", nil
	}
	var entries [][]json.RawMessage
	if err := json.Unmarshal(input.Justifications, &entries); err != nil || len(entries) > 8 {
		return "", "", errors.New("native capture block justification vector differs")
	}
	engines := map[string]bool{}
	var certificate string
	for _, entry := range entries {
		if len(entry) != 2 {
			return "", "", errors.New("native capture justification tuple differs")
		}
		engine, err := captureByteArray(entry[0], 4)
		if err != nil || len(engine) != 4 || engines[string(engine)] {
			return "", "", errors.New("native capture justification engine is malformed or duplicated")
		}
		engines[string(engine)] = true
		encoded, err := captureByteArray(entry[1], 4*1024*1024)
		if err != nil {
			return "", "", err
		}
		if string(engine) == "FRNK" {
			if len(encoded) == 0 {
				return "", "", errors.New("native capture GRANDPA certificate is empty")
			}
			certificate = "0x" + hex.EncodeToString(encoded)
		}
	}
	return scale, certificate, nil
}

// Genesis and descendant discovery are untrusted route observations. All
// selected successors must later match the signed ancestry from the checkpoint.
func (self *finalityCapture) blockHash(ctx context.Context, number uint64) (string, error) {
	var hash string
	validate := func(raw json.RawMessage) error {
		if err := json.Unmarshal(raw, &hash); err != nil {
			return err
		}
		if !canonicalHex(hash, 32) {
			return errors.New("native capture returned a malformed block hash")
		}
		return nil
	}
	raw, err := self.call(ctx, "chain_getBlockHash", []any{number}, validate)
	if err != nil {
		return "", err
	}
	if err := validate(raw); err != nil {
		return "", self.invalid(err)
	}
	return hash, nil
}

// Complete native results are checked before a failed read tail can trigger
// retry. Null remains unavailable. The receipt adapter keeps its old path.
func (self *finalityCapture) call(ctx context.Context, method string, params []any, validate func(json.RawMessage) error) (json.RawMessage, error) {
	if self.nativeExecution {
		previous := self.rpc.validateResult
		self.rpc.validateResult = func(raw json.RawMessage) error {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil
			}
			return self.invalid(validate(raw))
		}
		defer func() { self.rpc.validateResult = previous }()
	}
	return self.rpc.call(ctx, method, params)
}

// Only hash-addressed complete headers are reusable. Every reuse is rehashed;
// height lookups and absent certificates remain fresh observations on restart.
func (self *finalityCapture) header(ctx context.Context, hash string) (string, *nativeFinalityHeader, error) {
	name := "header-" + hash[2:] + ".json"
	var scale string
	found, err := self.load(ctx, name, 2*maximumNativeFinalityHeaderBytes+32, &scale)
	if err != nil {
		return "", nil, err
	}
	if found {
		budget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
		header, err := budget.header(scale)
		if err != nil {
			return "", nil, self.invalid(err)
		}
		if header.identity.Hash != hash {
			return "", nil, self.invalid(errors.New("retained native capture header differs from its hash"))
		}
		return scale, header, nil
	}
	raw, err := self.call(ctx, "chain_getHeader", []any{hash}, func(raw json.RawMessage) error {
		_, _, err := captureHeaderScale(raw, hash)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	scale, header, err := captureHeaderScale(raw, hash)
	if err != nil {
		return "", nil, self.invalid(err)
	}
	if err := self.save(ctx, name, scale); err != nil {
		return "", nil, err
	}
	return scale, header, nil
}

// Retain exact certificate bytes before cryptographic replay so a rejected
// response remains available. Reuse never bypasses the verifier or hash checks.
func (self *finalityCapture) certificate(ctx context.Context, hash, expectedHeader string, verify func(string) error) (string, error) {
	name := "certificate-" + hash[2:] + ".json"
	var certificate string
	found, err := self.load(ctx, name, 8*1024*1024+32, &certificate)
	if err != nil {
		return "", err
	}
	if found {
		if verify != nil && certificate != "" {
			if err := verify(certificate); err != nil {
				return "", self.invalid(err)
			}
		}
		return certificate, nil
	}
	validate := func(raw json.RawMessage) error {
		scale, certificate, err := captureBlockCertificate(raw, hash)
		if err != nil {
			return err
		}
		if scale != expectedHeader {
			return errors.New("native capture block and retained complete header differ")
		}
		if verify != nil && certificate != "" {
			if err := verify(certificate); err != nil {
				// Retain the original well-formed but rejected certificate as
				// evidence. Its publication never grants finality or a cursor.
				return errors.Join(err, self.save(ctx, name, certificate))
			}
		}
		return nil
	}
	raw, err := self.call(ctx, "chain_getBlock", []any{hash}, validate)
	if err != nil {
		return "", err
	}
	scale, certificate, err := captureBlockCertificate(raw, hash)
	if err != nil {
		return "", self.invalid(err)
	}
	if scale != expectedHeader {
		return "", self.invalid(errors.New("native capture block and retained complete header differ"))
	}
	if certificate != "" {
		if err := self.save(ctx, name, certificate); err != nil {
			return "", err
		}
	}
	return certificate, nil
}
