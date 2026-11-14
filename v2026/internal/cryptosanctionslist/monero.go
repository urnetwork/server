package cryptosanctionslist

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Monero's base58 encodes 8-byte blocks as 11 characters each, and a shorter
// last block as the length here at its byte count (src/common/base58.cpp).
var moneroEncodedBlockSizes = [...]int{0, 2, 3, 5, 6, 7, 9, 10, 11}

const (
	moneroBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	moneroFullBlockSize        = 8
	moneroFullEncodedBlockSize = 11
)

// The network tag that leads an address and whether it carries an 8-byte
// payment id, for mainnet, testnet and stagenet.
var moneroTags = map[byte]bool{
	// standard
	18: false, 53: false, 24: false,
	// integrated
	19: true, 54: true, 25: true,
	// subaddress
	42: false, 63: false, 36: false,
}

// A standard or subaddress (95 characters) or an integrated address (106): a
// network tag, the spend and view keys, an integrated address's payment id,
// and the first four bytes of the Keccak-256 of all of that.
func normalizeMonero(address string) (string, error) {
	if len(address) != 95 && len(address) != 106 {
		return "", fmt.Errorf("%d characters, want 95 or 106", len(address))
	}
	raw, err := moneroBase58Decode(address)
	if err != nil {
		return "", err
	}
	integrated, ok := moneroTags[raw[0]]
	if !ok {
		return "", fmt.Errorf("network tag %d", raw[0])
	}
	want := 1 + 64 + 4
	if integrated {
		want += 8
	}
	if len(raw) != want {
		return "", fmt.Errorf("%d bytes for network tag %d, want %d", len(raw), raw[0], want)
	}
	payload, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	hash := sha3.NewLegacyKeccak256()
	hash.Write(payload)
	if !bytes.Equal(hash.Sum(nil)[:4], sum) {
		return "", errors.New("bad Monero checksum")
	}
	return address, nil
}

func moneroBase58Decode(s string) ([]byte, error) {
	fullBlocks := len(s) / moneroFullEncodedBlockSize
	lastEncoded := len(s) % moneroFullEncodedBlockSize
	lastSize := -1
	for size, encoded := range moneroEncodedBlockSizes {
		if encoded == lastEncoded {
			lastSize = size
		}
	}
	if lastSize < 0 {
		return nil, fmt.Errorf("Monero base58 of %d characters", len(s))
	}
	out := make([]byte, 0, fullBlocks*moneroFullBlockSize+lastSize)
	for i := 0; i < fullBlocks; i += 1 {
		block, err := moneroBase58DecodeBlock(s[i*moneroFullEncodedBlockSize:(i+1)*moneroFullEncodedBlockSize], moneroFullBlockSize)
		if err != nil {
			return nil, err
		}
		out = append(out, block...)
	}
	if 0 < lastSize {
		block, err := moneroBase58DecodeBlock(s[fullBlocks*moneroFullEncodedBlockSize:], lastSize)
		if err != nil {
			return nil, err
		}
		out = append(out, block...)
	}
	return out, nil
}

// One block, big-endian, which must fit size bytes.
func moneroBase58DecodeBlock(block string, size int) ([]byte, error) {
	value := uint64(0)
	for i := 0; i < len(block); i += 1 {
		digit := strings.IndexByte(moneroBase58Alphabet, block[i])
		if digit < 0 {
			return nil, fmt.Errorf("Monero base58 character %q", block[i])
		}
		high, low := bits.Mul64(value, 58)
		sum, carry := bits.Add64(low, uint64(digit), 0)
		if high != 0 || carry != 0 {
			return nil, errors.New("Monero base58 block overflow")
		}
		value = sum
	}
	if size < moneroFullBlockSize && uint64(1)<<(8*size) <= value {
		return nil, errors.New("Monero base58 block overflow")
	}
	out := make([]byte, size)
	for i := size - 1; 0 <= i; i -= 1 {
		out[i] = byte(value)
		value >>= 8
	}
	return out, nil
}
