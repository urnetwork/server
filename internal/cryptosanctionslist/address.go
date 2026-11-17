// Package cryptosanctionslist is the OFAC crypto-address table shared by its
// writer, cryptoctl, and its reader, the cryptosanctions loader: the assets it
// covers, each chain's address rules, and the files of a table directory.
//
// An address is compared in a canonical form, so two spellings of one address
// are one entry:
//
//   - btc: base58check (P2PKH and P2SH) as given, since base58 is
//     case-sensitive; bech32 and bech32m segwit addresses (BIP 173, BIP 350) in
//     lowercase. Checksums, versions and witness programs are verified.
//     Testnet, signet and regtest forms are valid; OFAC lists none of them.
//   - eth: "0x" and 40 hex digits in lowercase; case carries only the EIP-55
//     checksum.
//   - sol and usdc_solana: base58 of a 32-byte public key, as given. USDC on
//     Solana is paid from a Solana wallet, so both take the Solana addresses.
//   - xmr: Monero's block base58, as given, with the Keccak checksum verified.
//   - zec: base58check transparent addresses as given; Sapling (bech32),
//     unified and TEX (bech32m) addresses in lowercase.
//   - tao: the 32-byte account of an SS58 address under any network prefix,
//     or of a 0x-prefixed hex account id, written as SS58 with Bittensor's
//     prefix 42: one key is one account whatever prefix renders it.
//
// An address that is not valid on the asset's chain is an error, never "not
// listed": a caller that cannot screen an address holds the payment.
package cryptosanctionslist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mr-tron/base58"
	"github.com/urfoundation/sn/ss58"
)

// The rail's assets (model.CryptoAsset*), in table order.
const (
	AssetBtc        = "btc"
	AssetEth        = "eth"
	AssetSol        = "sol"
	AssetUsdcSolana = "usdc_solana"
	AssetXmr        = "xmr"
	AssetZec        = "zec"
	AssetTao        = "tao"
)

// Every asset a table covers, in table order.
func Assets() []string {
	return []string{AssetBtc, AssetEth, AssetSol, AssetUsdcSolana, AssetXmr, AssetZec, AssetTao}
}

// ErrUnknownAsset is the error for an asset no table covers.
var ErrUnknownAsset = errors.New("unknown asset")

// ErrInvalidAddress is the error for an address that is not valid on the
// asset's chain.
var ErrInvalidAddress = errors.New("invalid address")

type chain int

const (
	chainBitcoin chain = iota
	chainEthereum
	chainSolana
	chainMonero
	chainZcash
	chainBittensor
)

var assetChains = map[string]chain{
	AssetBtc:        chainBitcoin,
	AssetEth:        chainEthereum,
	AssetSol:        chainSolana,
	AssetUsdcSolana: chainSolana,
	AssetXmr:        chainMonero,
	AssetZec:        chainZcash,
	AssetTao:        chainBittensor,
}

// OFAC's currency code for each chain's own coin, as in its "Digital Currency
// Address - XBT" identifiers. OFAC has listed no TAO address; "TAO" is the
// code it would use.
var chainOfacCurrencies = map[chain]string{
	chainBitcoin:   "XBT",
	chainEthereum:  "ETH",
	chainSolana:    "SOL",
	chainMonero:    "XMR",
	chainZcash:     "ZEC",
	chainBittensor: "TAO",
}

// Whether a table covers asset.
func KnownAsset(asset string) bool {
	_, ok := assetChains[asset]
	return ok
}

// OfacCurrency is OFAC's currency code for the coin of asset's chain: the
// identifiers cryptoctl expects to be valid on that chain.
func OfacCurrency(asset string) (string, error) {
	chain, ok := assetChains[asset]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownAsset, asset)
	}
	return chainOfacCurrencies[chain], nil
}

// Normalize returns the canonical form of address on asset's chain, the form
// a table stores and a lookup compares. Surrounding white space is ignored.
// It is idempotent: a canonical form normalizes to itself.
func Normalize(asset string, address string) (string, error) {
	chain, ok := assetChains[asset]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownAsset, asset)
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidAddress)
	}
	var canonical string
	var err error
	switch chain {
	case chainBitcoin:
		canonical, err = normalizeBitcoin(address)
	case chainEthereum:
		canonical, err = normalizeEthereum(address)
	case chainSolana:
		canonical, err = normalizeSolana(address)
	case chainMonero:
		canonical, err = normalizeMonero(address)
	case chainZcash:
		canonical, err = normalizeZcash(address)
	case chainBittensor:
		canonical, err = normalizeBittensor(address)
	}
	if err != nil {
		return "", fmt.Errorf("%w for %s: %s", ErrInvalidAddress, asset, err.Error())
	}
	return canonical, nil
}

// Bitcoin's base58check versions: P2PKH and P2SH on mainnet, then on testnet,
// signet and regtest.
var bitcoinBase58Versions = [][]byte{{0x00}, {0x05}, {0x6f}, {0xc4}}

// Bitcoin's segwit human-readable parts: mainnet, testnet and signet, regtest.
var bitcoinSegwitHrps = []string{"bc", "tb", "bcrt"}

func normalizeBitcoin(address string) (string, error) {
	lower := strings.ToLower(address)
	for _, hrp := range bitcoinSegwitHrps {
		if strings.HasPrefix(lower, hrp+"1") {
			return normalizeSegwit(address, hrp)
		}
	}
	if err := checkBase58Check(address, bitcoinBase58Versions, 20); err != nil {
		return "", err
	}
	return address, nil
}

// A segwit address of hrp: witness version 0 is bech32 with a 20- or 32-byte
// program; versions 1 to 16 are bech32m with 2 to 40 bytes.
func normalizeSegwit(address string, hrp string) (string, error) {
	decoded, err := bech32Decode(address, bech32MaxLength)
	if err != nil {
		return "", err
	}
	if decoded.hrp != hrp {
		return "", fmt.Errorf("human-readable part %q, want %q", decoded.hrp, hrp)
	}
	if len(decoded.data) == 0 {
		return "", errors.New("no witness version")
	}
	version := decoded.data[0]
	if 16 < version {
		return "", fmt.Errorf("witness version %d", version)
	}
	program, err := convertBits(decoded.data[1:], 5, 8, false)
	if err != nil {
		return "", err
	}
	if len(program) < 2 || 40 < len(program) {
		return "", fmt.Errorf("witness program of %d bytes", len(program))
	}
	if version == 0 {
		if len(program) != 20 && len(program) != 32 {
			return "", fmt.Errorf("version 0 witness program of %d bytes", len(program))
		}
		if decoded.variant != bech32Variant {
			return "", errors.New("version 0 witness program needs a bech32 checksum")
		}
	} else if decoded.variant != bech32mVariant {
		return "", fmt.Errorf("version %d witness program needs a bech32m checksum", version)
	}
	return strings.ToLower(address), nil
}

func normalizeEthereum(address string) (string, error) {
	digits := address
	if strings.HasPrefix(digits, "0x") || strings.HasPrefix(digits, "0X") {
		digits = digits[2:]
	}
	if len(digits) != 40 {
		return "", fmt.Errorf("%d hex digits, want 40", len(digits))
	}
	if _, err := hex.DecodeString(digits); err != nil {
		return "", errors.New("not hex")
	}
	return "0x" + strings.ToLower(digits), nil
}

func normalizeSolana(address string) (string, error) {
	raw, err := base58.Decode(address)
	if err != nil {
		return "", errors.New("not base58")
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("%d bytes, want a 32-byte public key", len(raw))
	}
	return base58.Encode(raw), nil
}

// Zcash's transparent base58check versions: t1 (P2PKH) and t3 (P2SH) on
// mainnet, tm and t2 on testnet.
var zcashTransparentVersions = [][]byte{{0x1c, 0xb8}, {0x1c, 0xbd}, {0x1d, 0x25}, {0x1c, 0xba}}

func normalizeZcash(address string) (string, error) {
	lower := strings.ToLower(address)
	switch {
	case strings.HasPrefix(lower, "zs1") || strings.HasPrefix(lower, "ztestsapling1"):
		// Sapling: bech32 of an 11-byte diversifier and a 32-byte key
		decoded, err := bech32Decode(address, bech32MaxLength)
		if err != nil {
			return "", err
		}
		if decoded.variant != bech32Variant {
			return "", errors.New("Sapling address needs a bech32 checksum")
		}
		payload, err := convertBits(decoded.data, 5, 8, false)
		if err != nil {
			return "", err
		}
		if len(payload) != 43 {
			return "", fmt.Errorf("Sapling address of %d bytes, want 43", len(payload))
		}
		return lower, nil
	case strings.HasPrefix(lower, "u1") || strings.HasPrefix(lower, "utest1"):
		// unified (ZIP 316): bech32m without bech32's length limit
		decoded, err := bech32Decode(address, unifiedMaxLength)
		if err != nil {
			return "", err
		}
		if decoded.variant != bech32mVariant {
			return "", errors.New("unified address needs a bech32m checksum")
		}
		payload, err := convertBits(decoded.data, 5, 8, false)
		if err != nil {
			return "", err
		}
		// F4Jumbled receivers and the 16-byte padding
		if len(payload) < 48 {
			return "", fmt.Errorf("unified address of %d bytes", len(payload))
		}
		return lower, nil
	case strings.HasPrefix(lower, "tex1") || strings.HasPrefix(lower, "textest1"):
		// TEX (ZIP 320): bech32m of a 20-byte transparent key hash
		decoded, err := bech32Decode(address, bech32MaxLength)
		if err != nil {
			return "", err
		}
		if decoded.variant != bech32mVariant {
			return "", errors.New("TEX address needs a bech32m checksum")
		}
		payload, err := convertBits(decoded.data, 5, 8, false)
		if err != nil {
			return "", err
		}
		if len(payload) != 20 {
			return "", fmt.Errorf("TEX address of %d bytes, want 20", len(payload))
		}
		return lower, nil
	}
	if err := checkBase58Check(address, zcashTransparentVersions, 20); err != nil {
		return "", err
	}
	return address, nil
}

func normalizeBittensor(address string) (string, error) {
	var account [32]byte
	if strings.HasPrefix(address, "0x") || strings.HasPrefix(address, "0X") {
		raw, err := hex.DecodeString(address[2:])
		if err != nil || len(raw) != len(account) {
			return "", errors.New("hex account id needs 64 hex digits")
		}
		copy(account[:], raw)
	} else {
		decoded, _, err := ss58.Decode(address)
		if err != nil {
			return "", err
		}
		account = decoded
	}
	return ss58.Encode(account, ss58.BittensorPrefix)
}

// Verifies a base58check string: a version from versions, then size bytes,
// then the first four bytes of the payload's double SHA-256.
func checkBase58Check(address string, versions [][]byte, size int) error {
	raw, err := base58.Decode(address)
	if err != nil {
		return errors.New("not base58")
	}
	if len(raw) < 4 {
		return fmt.Errorf("%d bytes", len(raw))
	}
	payload, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	if !bytes.Equal(second[:4], sum) {
		return errors.New("bad base58check checksum")
	}
	if !slices.ContainsFunc(versions, func(version []byte) bool {
		return len(payload) == len(version)+size && bytes.HasPrefix(payload, version)
	}) {
		return fmt.Errorf("unknown version or length (%d bytes)", len(payload))
	}
	return nil
}
