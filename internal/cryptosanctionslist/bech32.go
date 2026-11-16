package cryptosanctionslist

import (
	"errors"
	"fmt"
	"strings"
)

// Bech32 (BIP 173) and bech32m (BIP 350), which differ only in the constant
// the checksum ends on.
const (
	bech32Variant  = 1
	bech32mVariant = 2

	bech32Constant  = 1
	bech32mConstant = 0x2bc830a3

	bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

	// BIP 173's limit
	bech32MaxLength = 90
	// ZIP 316 lifts the limit for unified addresses; this only bounds the work
	unifiedMaxLength = 2048
)

type bech32Decoded struct {
	// lowercase
	hrp string
	// 5-bit groups, without the checksum
	data    []byte
	variant int
}

func bech32Polymod(values []byte) uint32 {
	generator := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	checksum := uint32(1)
	for _, value := range values {
		top := checksum >> 25
		checksum = (checksum&0x1ffffff)<<5 ^ uint32(value)
		for i := 0; i < 5; i += 1 {
			if (top>>i)&1 == 1 {
				checksum ^= generator[i]
			}
		}
	}
	return checksum
}

func bech32HrpExpand(hrp string) []byte {
	expanded := make([]byte, 0, 2*len(hrp)+1)
	for i := 0; i < len(hrp); i += 1 {
		expanded = append(expanded, hrp[i]>>5)
	}
	expanded = append(expanded, 0)
	for i := 0; i < len(hrp); i += 1 {
		expanded = append(expanded, hrp[i]&31)
	}
	return expanded
}

// Decodes a bech32 or bech32m string of at most maxLength characters. Mixed
// case is invalid (BIP 173); the human-readable part is returned in lowercase.
func bech32Decode(s string, maxLength int) (bech32Decoded, error) {
	if maxLength < len(s) {
		return bech32Decoded{}, fmt.Errorf("bech32 string of %d characters", len(s))
	}
	for i := 0; i < len(s); i += 1 {
		if s[i] < 33 || 126 < s[i] {
			return bech32Decoded{}, errors.New("bech32 character out of range")
		}
	}
	lower := strings.ToLower(s)
	if s != lower && s != strings.ToUpper(s) {
		return bech32Decoded{}, errors.New("mixed-case bech32")
	}
	separator := strings.LastIndexByte(lower, '1')
	if separator < 1 || len(lower) < separator+7 {
		return bech32Decoded{}, errors.New("bech32 separator out of place")
	}
	hrp := lower[:separator]
	data := make([]byte, 0, len(lower)-separator-1)
	for i := separator + 1; i < len(lower); i += 1 {
		value := strings.IndexByte(bech32Charset, lower[i])
		if value < 0 {
			return bech32Decoded{}, fmt.Errorf("bech32 character %q", lower[i])
		}
		data = append(data, byte(value))
	}
	var variant int
	switch bech32Polymod(append(bech32HrpExpand(hrp), data...)) {
	case bech32Constant:
		variant = bech32Variant
	case bech32mConstant:
		variant = bech32mVariant
	default:
		return bech32Decoded{}, errors.New("bad bech32 checksum")
	}
	return bech32Decoded{hrp: hrp, data: data[:len(data)-6], variant: variant}, nil
}

// Regroups bits, as BIP 173's convertbits. Without padding, leftover bits must
// be fewer than from and all zero.
func convertBits(data []byte, from uint, to uint, pad bool) ([]byte, error) {
	accumulator := uint32(0)
	bits := uint(0)
	maxValue := uint32(1)<<to - 1
	maxAccumulator := uint32(1)<<(from+to-1) - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, value := range data {
		if value>>from != 0 {
			return nil, errors.New("bech32 group out of range")
		}
		accumulator = (accumulator<<from | uint32(value)) & maxAccumulator
		bits += from
		for to <= bits {
			bits -= to
			out = append(out, byte(accumulator>>bits&maxValue))
		}
	}
	if pad {
		if 0 < bits {
			out = append(out, byte(accumulator<<(to-bits)&maxValue))
		}
	} else if from <= bits || accumulator<<(to-bits)&maxValue != 0 {
		return nil, errors.New("bech32 padding")
	}
	return out, nil
}
