package model

import "testing"

// walletAuthType is the single rule that turns a stored
// network_user_auth_wallet.blockchain value into the name clients see in
// auth_types[] and send back to /auth/remove-auth. Three things now depend on
// it agreeing with ParseBlockchain for every spelling: GetNetworkUser's
// auth_types[], RemoveAuth's refusal when the requested chain is not the bound
// one, and RemoveAuth's scalar auth_type reconcile.
//
// The reconcile used to classify the chain with its own SQL
// (CASE WHEN upper(blockchain) = 'TAO' ...), which recognised 'TAO' but not the
// 'BITTENSOR' spelling ParseBlockchain also accepts, and silently answered
// 'solana' for it. This table pins the rule those three now share.
//
// PURE UNIT: no database, no WARP_ENV.
func TestWalletAuthTypeAcceptsEverySpelling(t *testing.T) {
	tests := []struct {
		blockchain string
		want       AuthType
	}{
		// canonical
		{"TAO", AuthTypeBittensor},
		{"SOL", AuthTypeSolana},
		// the spellings ParseBlockchain accepts, in the cases a legacy row or
		// a hand-written client can hold
		{"tao", AuthTypeBittensor},
		{"Tao", AuthTypeBittensor},
		{"BITTENSOR", AuthTypeBittensor},
		{"bittensor", AuthTypeBittensor},
		{"Bittensor", AuthTypeBittensor},
		{"sol", AuthTypeSolana},
		{"solana", AuthTypeSolana},
		{"SOLANA", AuthTypeSolana},
		{"Solana", AuthTypeSolana},
		// Documented default, asserted rather than assumed: anything that is
		// not Bittensor reads as Solana, including a value ParseBlockchain
		// rejects outright and the empty string. Callers rely on this being
		// total -- GetNetworkUser puts the result straight into auth_types[]
		// and has no branch for "unknown".
		{"", AuthTypeSolana},
		{"ETHEREUM", AuthTypeSolana},
		{"MATIC", AuthTypeSolana},
		{"not-a-chain", AuthTypeSolana},
	}
	for _, test := range tests {
		if got := walletAuthType(test.blockchain); got != test.want {
			t.Errorf("walletAuthType(%q) = %q, want %q", test.blockchain, got, test.want)
		}
	}
}

// migratedWalletBlockchain decides the blockchain label the legacy child-auth
// migration writes. Both call sites used to pass SOL unconditionally while the
// legacy value sat in the row they had already read, which would have written
// 'SOL' for a Bittensor account -- filterWalletAuthsByBlockchain then hides
// that row from its own owner.
//
// The column is compared byte-exactly by the uniqueness checks, so the label
// has to be canonicalised, not copied through.
//
// PURE UNIT: no database, no WARP_ENV.
func TestMigratedWalletBlockchainCanonicalises(t *testing.T) {
	ptr := func(s string) *string { return &s }
	tests := []struct {
		name       string
		blockchain *string
		want       string
	}{
		{"canonical tao", ptr("TAO"), "TAO"},
		{"lowercase tao", ptr("tao"), "TAO"},
		{"bittensor spelling", ptr("bittensor"), "TAO"},
		{"canonical sol", ptr("SOL"), "SOL"},
		{"legacy lowercase solana", ptr("solana"), "SOL"},
		// A NULL wallet_blockchain is what the column held before it existed,
		// and an unparseable value is not a chain this code can honour. Both
		// keep the historical SOL default rather than writing something the
		// uniqueness checks cannot see.
		{"null", nil, "SOL"},
		{"unparseable", ptr("not-a-chain"), "SOL"},
		{"empty", ptr(""), "SOL"},
	}
	for _, test := range tests {
		if got := migratedWalletBlockchain(test.blockchain); got != test.want {
			t.Errorf("%s: migratedWalletBlockchain = %q, want %q", test.name, got, test.want)
		}
	}
}
