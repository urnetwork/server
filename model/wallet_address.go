// The address rule of an account wallet on its chain, for every wallet a
// network adds or selects for payouts:
//   - SOL: a base58 Solana public key of exactly 32 bytes, not all zero and
//     not the USDC mint
//   - MATIC and ETHEREUM: 0x and 40 hex characters, not the zero address. An
//     address in mixed case must carry its EIP-55 checksum; one case
//     throughout carries none.
//   - TAO: a Bittensor ss58 address (prefix 42)
//
// A refusal is a 400 that says what the address is instead, or what an
// address on the chain looks like; the apps show it as the reason. Rows
// stored before this rule (a Solana key saved as a Polygon wallet, support
// inbox 8045) are left as they are; they are only refused for a payout.
package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gagliardetto/solana-go"
)

// The mint of USDC on Solana. A token mint is not a wallet.
const SolanaUsdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// Every refusal of ValidateWalletAddress wraps it.
var ErrInvalidWalletAddress = errors.New("400 invalid wallet address")

// 0x and 40 hex characters, in any case.
func isEvmAddressShape(address string) bool {
	return len(address) == 42 &&
		(strings.HasPrefix(address, "0x") || strings.HasPrefix(address, "0X")) &&
		common.IsHexAddress(address)
}

func isSolanaPublicKey(address string) bool {
	_, err := solana.PublicKeyFromBase58(address)
	return err == nil
}

// nil when the address is an address on the chain, else a refusal wrapping
// ErrInvalidWalletAddress.
func ValidateWalletAddress(blockchain Blockchain, address string) error {
	refuse := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrInvalidWalletAddress, reason)
	}
	if address == "" {
		return refuse("the address is empty")
	}
	if strings.TrimSpace(address) != address {
		return refuse("the address has spaces around it")
	}
	switch blockchain {
	case SOL:
		publicKey, err := solana.PublicKeyFromBase58(address)
		switch {
		case err == nil && publicKey == (solana.PublicKey{}):
			return refuse("the all-zero key is not a Solana wallet")
		case err == nil && address == SolanaUsdcMint:
			return refuse("this is the USDC token mint, not a Solana wallet")
		case err == nil:
			return nil
		case isEvmAddressShape(address):
			return refuse("this is a Polygon or Ethereum (0x) address, not a Solana address")
		case IsValidBittensorAddress(address):
			return refuse("this is a Bittensor address, not a Solana address")
		default:
			return refuse("a Solana address is a base58 public key of 32 bytes")
		}
	case MATIC, ETHEREUM:
		chainName, article := "Polygon", "a"
		if blockchain == ETHEREUM {
			chainName, article = "Ethereum", "an"
		}
		if !isEvmAddressShape(address) {
			switch {
			case isSolanaPublicKey(address):
				return refuse(fmt.Sprintf("this is a Solana address, not %s %s address", article, chainName))
			case IsValidBittensorAddress(address):
				return refuse(fmt.Sprintf("this is a Bittensor address, not %s %s address", article, chainName))
			default:
				return refuse(fmt.Sprintf("%s %s address is 0x followed by 40 hex characters", article, chainName))
			}
		}
		hexPart := address[2:]
		mixedCase := strings.ToLower(hexPart) != hexPart && strings.ToUpper(hexPart) != hexPart
		evmAddress := common.HexToAddress(address)
		if mixedCase && evmAddress.Hex()[2:] != hexPart {
			return refuse(fmt.Sprintf("the mixed-case checksum of this %s address does not match; check it for a typo", chainName))
		}
		if evmAddress == (common.Address{}) {
			return refuse(fmt.Sprintf("the zero address is not %s %s wallet", article, chainName))
		}
		return nil
	case TAO:
		switch {
		case IsValidBittensorAddress(address):
			return nil
		case isSolanaPublicKey(address):
			return refuse("this is a Solana address, not a Bittensor address")
		case isEvmAddressShape(address):
			return refuse("this is a Polygon or Ethereum (0x) address, not a Bittensor address")
		default:
			return refuse("a Bittensor address is an ss58 address with prefix 42, starting with 5")
		}
	default:
		return refuse("the chain has no wallet addresses")
	}
}

// Whether a stored wallet can receive a payout by its address: false only
// when its chain is a payout chain (SOL, MATIC or ETHEREUM) and the address is
// not an address on it. Stored chain names vary in case and spelling, so the
// chain is parsed; a chain that does not parse, or TAO, is left to the payout
// wallet's chain rule.
func PayoutWalletAddressValid(blockchain string, address string) bool {
	chain, err := ParseBlockchain(blockchain)
	if err != nil {
		return true
	}
	switch chain {
	case SOL, MATIC, ETHEREUM:
		return ValidateWalletAddress(chain, address) == nil
	default:
		return true
	}
}
