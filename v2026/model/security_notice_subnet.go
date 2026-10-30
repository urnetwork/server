package model

// The subnet wallets' security notices (security_notice_model.go): a set of the
// subnet claim wallet, a provider wallet mapping consent, a network wallet
// mapping consent or a hotkey network delegation that gives subnet earnings to
// another key than before owes the payout wallet notice. Keys are compared by
// their public key and shown in their canonical Bittensor ss58 form, masked.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/ss58"

	"github.com/urnetwork/server/v2026"
)

// A Bittensor key as a notice compares and shows it.
func subnetKeyWallet(key [32]byte) *securityWallet {
	address, err := ss58.Encode(key, ss58.BittensorPrefix)
	server.Raise(err)
	return &securityWallet{chain: TAO.String(), address: address}
}

// A stored public key, or nil when it is not 32 bytes.
func subnetStoredKeyWallet(pubkey []byte) *securityWallet {
	if len(pubkey) != 32 {
		return nil
	}
	return subnetKeyWallet([32]byte(pubkey))
}

// The network's subnet claim wallet (st_wallet), or nil.
func securitySubnetNetworkWalletInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (wallet *securityWallet) {
	result, err := tx.Query(ctx, `SELECT coldkey_pubkey FROM st_wallet WHERE network_id = $1`, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var pubkey []byte
			server.Raise(result.Scan(&pubkey))
			wallet = subnetStoredKeyWallet(pubkey)
		}
	})
	return
}

// A provider's latest subnet wallet (st_provider_wallet_history), or nil.
func securitySubnetProviderWalletInTx(ctx context.Context, tx server.PgTx, clientId server.Id) (wallet *securityWallet) {
	result, err := tx.Query(
		ctx,
		`
			SELECT coldkey_pubkey
			FROM st_provider_wallet_history
			WHERE client_id = $1
			ORDER BY set_time DESC, wallet_version DESC
			LIMIT 1
		`,
		clientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var pubkey []byte
			server.Raise(result.Scan(&pubkey))
			wallet = subnetStoredKeyWallet(pubkey)
		}
	})
	return
}

// Owes the payout wallet notice for a consent or delegation accepted in the
// caller's transaction, when its key differs from the one the previous
// generation gave the earnings to (`previous`, read under the notice's
// savepoint; nil for the first generation).
func oweSubnetConsentNoticeInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	destination string,
	previous func() *securityWallet,
	nextKey [32]byte,
	fromEpoch uint64,
	throughEpoch uint64,
) {
	oweSecurityWalletNoticeInTx(ctx, tx, networkId, SecurityNoticeSubnetWallet, func() []*SecurityWalletChange {
		return []*SecurityWalletChange{
			newSecurityWalletChange(ctx, destination, previous(), subnetKeyWallet(nextKey), fromEpoch, throughEpoch),
		}
	})
}

// The coldkey of the network wallet mapping consent before `generation`, or nil
// for the first.
func previousNetworkConsentColdkeyInTx(ctx context.Context, tx server.PgTx, domain [32]byte, networkId server.Id, generation uint64) *securityWallet {
	original := subnetConsentOriginalInTx(
		ctx,
		tx,
		`SELECT original FROM network_wallet_mapping_consent WHERE domain_hash = $1 AND network_id = $2 AND generation = $3`,
		domain,
		networkId,
		generation,
	)
	if original == nil {
		return nil
	}
	statement, err := protocol.DecodeNetworkWalletMappingStatement(original.Message)
	server.Raise(err)
	return subnetKeyWallet(statement.Coldkey)
}

// The hotkey of the hotkey network delegation before `generation`, or nil for
// the first.
func previousHotkeyDelegationInTx(ctx context.Context, tx server.PgTx, domain [32]byte, networkId server.Id, generation uint64) *securityWallet {
	original := subnetConsentOriginalInTx(
		ctx,
		tx,
		`SELECT original FROM hotkey_network_delegation WHERE domain_hash = $1 AND network_id = $2 AND generation = $3`,
		domain,
		networkId,
		generation,
	)
	if original == nil {
		return nil
	}
	statement, err := protocol.DecodeHotkeyNetworkDelegationStatement(original.Message)
	server.Raise(err)
	return subnetKeyWallet(statement.Hotkey)
}

// The retained original of the generation before `generation` in a network's
// chain, or nil for the first generation.
func subnetConsentOriginalInTx(
	ctx context.Context,
	tx server.PgTx,
	sql string,
	domain [32]byte,
	networkId server.Id,
	generation uint64,
) *protocol.WalletMappingConsent {
	if generation <= 1 {
		return nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, sql, domain[:], networkId, generation-1).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	server.Raise(err)
	original := &protocol.WalletMappingConsent{}
	server.Raise(json.Unmarshal(raw, original))
	return original
}
