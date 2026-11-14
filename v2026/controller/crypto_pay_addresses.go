package controller

// Address checks and canonical forms of the Coinbase coins: a Coinbase
// deposit address is checked before it enters the pool, and an address a
// chain source reports is put in the same form before it is looked up.
//
//   - btc: mainnet base58check (P2PKH, P2SH) as given; bech32 (segwit v0, BIP
//     173) and bech32m (v1+, BIP 350) in lowercase
//   - eth: "0x" and 40 hex digits, written in EIP-55's checksum case
//   - sol: base58 of a 32-byte public key, as given
//   - tao: an SS58 account under any prefix, written with Bittensor's prefix
//     42 (the chain and its wallets use 42; one key is one account whatever
//     prefix renders it, so accounts are compared by their 32 bytes)

import (
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcutil/base58"
	"github.com/ethereum/go-ethereum/common"
	mrbase58 "github.com/mr-tron/base58"

	"github.com/urfoundation/sn/v2026/ss58"
)

// Bittensor's SS58 prefix.
const cryptoTaoSs58Prefix = 42

// cryptoBitcoinAddressCanonical checks a mainnet Bitcoin address and gives its
// canonical form.
func cryptoBitcoinAddressCanonical(address string) (string, error) {
	address = strings.TrimSpace(address)
	lower := strings.ToLower(address)
	if strings.HasPrefix(lower, "bc1") {
		if address != lower && address != strings.ToUpper(address) {
			return "", errors.New("a bech32 address in mixed case")
		}
		version, program, err := cryptoSegwitDecode("bc", lower)
		if err != nil {
			return "", err
		}
		if version == 0 && len(program) != 20 && len(program) != 32 {
			return "", errors.New("a segwit v0 program of the wrong length")
		}
		return lower, nil
	}
	decoded, version, err := base58.CheckDecode(address)
	if err != nil {
		return "", errors.New("not a base58check address")
	}
	if (version != 0x00 && version != 0x05) || len(decoded) != 20 {
		return "", errors.New("not a mainnet P2PKH or P2SH address")
	}
	return address, nil
}

// The bech32 alphabet and checksum constants (BIP 173, BIP 350).
const cryptoBech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

const (
	cryptoBech32Constant  = 1
	cryptoBech32mConstant = 0x2bc830a3
)

func cryptoBech32Polymod(values []byte) uint32 {
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

// cryptoSegwitDecode decodes a lowercase segwit address of the given human
// readable part: its witness version and program, the checksum's constant
// matching the version (bech32 for v0, bech32m from v1).
func cryptoSegwitDecode(hrp string, address string) (version int, program []byte, err error) {
	separator := strings.LastIndexByte(address, '1')
	if separator < 1 || separator+7 > len(address) || 90 < len(address) || address[:separator] != hrp {
		return 0, nil, errors.New("not a segwit address of this network")
	}
	data := []byte{}
	for _, r := range address[separator+1:] {
		index := strings.IndexRune(cryptoBech32Charset, r)
		if index < 0 {
			return 0, nil, errors.New("a character outside the bech32 alphabet")
		}
		data = append(data, byte(index))
	}
	expanded := []byte{}
	for i := 0; i < len(hrp); i += 1 {
		expanded = append(expanded, hrp[i]>>5)
	}
	expanded = append(expanded, 0)
	for i := 0; i < len(hrp); i += 1 {
		expanded = append(expanded, hrp[i]&31)
	}
	constant := cryptoBech32Polymod(append(expanded, data...))
	if len(data) < 7 {
		return 0, nil, errors.New("a segwit address too short")
	}
	version = int(data[0])
	if 16 < version {
		return 0, nil, errors.New("an unknown witness version")
	}
	if (version == 0 && constant != cryptoBech32Constant) || (0 < version && constant != cryptoBech32mConstant) {
		return 0, nil, errors.New("a bad checksum")
	}
	// the 5-bit groups after the version, less the 6-character checksum
	groups := data[1 : len(data)-6]
	accumulator := 0
	bits := 0
	for _, group := range groups {
		accumulator = accumulator<<5 | int(group)
		bits += 5
		for 8 <= bits {
			bits -= 8
			program = append(program, byte(accumulator>>bits))
		}
		accumulator &= (1 << bits) - 1
	}
	if 5 <= bits || accumulator != 0 {
		return 0, nil, errors.New("non-zero padding")
	}
	if len(program) < 2 || 40 < len(program) {
		return 0, nil, errors.New("a witness program of the wrong length")
	}
	return version, program, nil
}

// cryptoEthereumAddressOk reports "0x" and 40 hex digits whose case, when
// mixed, is EIP-55's checksum.
func cryptoEthereumAddressOk(address string) bool {
	_, err := cryptoEthereumAddressCanonical(address)
	return err == nil
}

// cryptoEthereumAddressCanonical checks an ETH address and gives it in
// EIP-55's checksum case.
func cryptoEthereumAddressCanonical(address string) (string, error) {
	address = strings.TrimSpace(address)
	if len(address) != 42 || !common.IsHexAddress(address) || !strings.HasPrefix(address, "0x") {
		return "", errors.New("not 0x and 40 hex digits")
	}
	checksummed := common.HexToAddress(address).Hex()
	digits := address[2:]
	if digits != strings.ToLower(digits) && digits != strings.ToUpper(digits) && address != checksummed {
		return "", errors.New("an EIP-55 checksum that does not match")
	}
	return checksummed, nil
}

// cryptoSolanaAddressCanonical checks a Solana address: base58 of 32 bytes.
func cryptoSolanaAddressCanonical(address string) (string, error) {
	address = strings.TrimSpace(address)
	decoded, err := mrbase58.Decode(address)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("not a base58 public key")
	}
	return address, nil
}

// cryptoTaoAccountOf decodes an SS58 address under any prefix to its account.
func cryptoTaoAccountOf(address string) ([32]byte, error) {
	account, _, err := ss58.Decode(strings.TrimSpace(address))
	if err != nil {
		return [32]byte{}, fmt.Errorf("not an SS58 account: %w", err)
	}
	return account, nil
}

// cryptoTaoAddressOf writes an account with Bittensor's prefix 42.
func cryptoTaoAddressOf(account [32]byte) string {
	address, err := ss58.Encode(account, cryptoTaoSs58Prefix)
	if err != nil {
		// 42 is a valid prefix: Encode refuses only an invalid one
		panic(err)
	}
	return address
}

// cryptoTaoAddressCanonical checks a TAO address and writes it with prefix 42.
func cryptoTaoAddressCanonical(address string) (string, error) {
	account, err := cryptoTaoAccountOf(address)
	if err != nil {
		return "", err
	}
	return cryptoTaoAddressOf(account), nil
}
