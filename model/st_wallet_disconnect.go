package model

// Disconnecting a network's payout wallet (controller/sn_wallet_disconnect.go,
// support inbox 1099). The network owner asks for a code, which goes to the
// account's sign-in email addresses, and the code confirms the disconnect: the
// re-auth that a stolen session alone cannot pass.
//
// The disconnect removes what pays the network without a signed consent:
//   - the network's side copy (st_wallet), which the legacy settlement pays
//     and the apps show as the connected wallet;
//   - the account-history wallet of each provider of the network that has one
//     set without a provider consent: a disconnect row (an empty address and
//     the zero key) is appended to st_provider_wallet_history, which is
//     append-only, so the history keeps every earlier wallet and the release
//     settlement reads the provider as unmapped from then on
//     (controller/provider_wallet_epoch.go). A later wallet set appends a new
//     row and pays again.
//
// A provider's own signed consent stays: it is an independent signer's, and
// the protocol has no unsigned revocation. The controller refuses while a
// signed network consent or hotkey delegation is in force, for the same
// reason. The disconnect owes the payout wallet security notice in its
// transaction (security_notice_model.go).

import (
	"context"
	"errors"
	"time"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
)

// The purpose of the disconnect's confirmation code (account_code.go).
const accountCodePurposeWalletDisconnect = "wallet_disconnect"

// The account action budget of the disconnect codes (account_action_rate_limit.go).
const AccountActionWalletDisconnectCodeSend = "wallet_disconnect_code_send"

var (
	// not the network's admin: the session's user does not administer it
	ErrStWalletDisconnectOwnerOnly = errors.New("only the network's own sign-in can disconnect its payout wallet")
	// nothing the disconnect removes: no side copy and no unsigned provider wallet
	ErrStWalletDisconnectNoWallet = errors.New("the network has no wallet to disconnect")
	// the account has no sign-in email to send the code to
	ErrStWalletDisconnectEmailRequired = errors.New("the account has no sign-in email for the confirmation code")
	// the code is wrong; it still works for its remaining tries
	ErrStWalletDisconnectCodeInvalid = errors.New("the confirmation code is wrong")
	// no code is waiting: it expired, was used, or too many wrong tries ended it
	ErrStWalletDisconnectCodeExpired = errors.New("no confirmation code is waiting")
)

type StWalletDisconnectSettings struct {
	// how long a code works. A person reads it from their inbox at once, and a
	// short life bounds what a lost code is worth.
	CodeTtl time.Duration
	// wrong tries a code takes; the last ends it. Five, as a verification code
	// takes (auth_verify_code_limit.go).
	TryLimit int
	// codes sent per account in CodeSendWindow, which also bounds the guesses:
	// at most TryLimit for each
	CodeSendLimit  int
	CodeSendWindow time.Duration
}

func DefaultStWalletDisconnectSettings() *StWalletDisconnectSettings {
	return &StWalletDisconnectSettings{
		CodeTtl:        15 * time.Minute,
		TryLimit:       5,
		CodeSendLimit:  5,
		CodeSendWindow: 24 * time.Hour,
	}
}

// A sign-in email address the code goes to, and how the apps show it.
type StWalletDisconnectRecipient struct {
	Address string
	Masked  string
}

// A disconnect code to send.
type StWalletDisconnectCode struct {
	Code       string
	ExpireTime time.Time
	Recipients []*StWalletDisconnectRecipient
}

// What the disconnect removed.
type StWalletDisconnected struct {
	// the network's side copy was removed
	NetworkWallet bool
	// providers whose account-history wallet was disconnected
	ProviderWalletCount int
}

// A provider's newest account-history wallet the disconnect ends.
type stDisconnectProviderWallet struct {
	clientId server.Id
	coldkey  [32]byte
}

// What a disconnect of the network removes, read in the caller's transaction:
// the side copy's key (nil for none) and each provider of the network whose
// newest account-history wallet names the network, is a key, and is not the
// projection of the provider's own signed consent.
func stWalletsToDisconnectInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (networkColdkey *[32]byte, providers []*stDisconnectProviderWallet) {
	result, err := tx.Query(ctx, `SELECT coldkey_pubkey FROM st_wallet WHERE network_id = $1`, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var pubkey []byte
			server.Raise(result.Scan(&pubkey))
			if len(pubkey) == 32 && [32]byte(pubkey) != ([32]byte{}) {
				coldkey := [32]byte(pubkey)
				networkColdkey = &coldkey
			}
		}
	})
	result, err = tx.Query(
		ctx,
		`
			SELECT DISTINCT ON (history.client_id)
				history.client_id,
				history.network_id,
				history.coldkey_pubkey,
				history.original_message
			FROM st_provider_wallet_history history
			WHERE history.client_id IN (
				SELECT DISTINCT client_id FROM st_provider_wallet_history WHERE network_id = $1
			)
			ORDER BY history.client_id, history.set_time DESC, history.wallet_version DESC
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId, rowNetworkId server.Id
			var pubkey []byte
			var originalMessage *string
			server.Raise(result.Scan(&clientId, &rowNetworkId, &pubkey, &originalMessage))
			if rowNetworkId != networkId || len(pubkey) != 32 || [32]byte(pubkey) == ([32]byte{}) {
				continue
			}
			if originalMessage != nil {
				// the projection of the provider's own consent: an independent
				// signer's, which the disconnect leaves
				if statement, err := protocol.DecodeWalletMappingStatement(*originalMessage); err == nil && statement.ClientId == [16]byte(clientId) {
					continue
				}
			}
			providers = append(providers, &stDisconnectProviderWallet{clientId: clientId, coldkey: [32]byte(pubkey)})
		}
	})
	return
}

// The user must administer the network, checked in the caller's transaction.
func stWalletDisconnectOwnerInTx(ctx context.Context, tx server.PgTx, userId server.Id, networkId server.Id) bool {
	owner := false
	result, err := tx.Query(ctx, `SELECT 1 FROM network WHERE network_id = $1 AND admin_user_id = $2`, networkId, userId)
	server.WithPgResult(result, err, func() {
		owner = result.Next()
	})
	return owner
}

// Makes and stores the disconnect's code for the network owner and returns it
// with the sign-in addresses to send it to. Spends one code of the account's
// budget. A new code replaces the one waiting.
func SendStWalletDisconnectCode(ctx context.Context, settings *StWalletDisconnectSettings, userId server.Id, networkId server.Id) (code *StWalletDisconnectCode, returnErr error) {
	var recipients []string
	server.Tx(ctx, func(tx server.PgTx) {
		recipients = nil
		returnErr = nil
		if !stWalletDisconnectOwnerInTx(ctx, tx, userId, networkId) {
			returnErr = ErrStWalletDisconnectOwnerOnly
			return
		}
		networkColdkey, providers := stWalletsToDisconnectInTx(ctx, tx, networkId)
		if networkColdkey == nil && len(providers) == 0 {
			returnErr = ErrStWalletDisconnectNoWallet
			return
		}
		recipients = securityNoticeRecipientsInTx(ctx, tx, networkId)
		if len(recipients) == 0 {
			returnErr = ErrStWalletDisconnectEmailRequired
			return
		}
		allow, retryAfter := checkAndRecordAccountActionRateLimitInTx(ctx, tx, userId, AccountActionWalletDisconnectCodeSend, settings.CodeSendLimit, settings.CodeSendWindow)
		if !allow {
			returnErr = &rateLimitError{
				message:           "429 " + maxAccountActionAttemptsRetryError(AccountActionWalletDisconnectCodeSend, settings.CodeSendWindow, retryAfter).Error(),
				retryAfterSeconds: int((retryAfter + time.Second - 1) / time.Second),
			}
			return
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	request := newAccountCodeRequest(accountCodePurposeWalletDisconnect, userId, 1, server.NowUtc().Add(settings.CodeTtl), map[string]string{
		"network_id": networkId.String(),
	})
	putAccountCodeRequest(ctx, request)
	code = &StWalletDisconnectCode{
		Code:       request.Codes[0],
		ExpireTime: request.ExpireTime,
	}
	for _, recipient := range recipients {
		code.Recipients = append(code.Recipients, &StWalletDisconnectRecipient{
			Address: recipient,
			Masked:  maskEmailAddress(recipient),
		})
	}
	return code, nil
}

// With the code, disconnects the network's payout wallet: removes its side
// copy and disconnects the account-history wallet of each provider that has
// one without a consent, owing the payout wallet notice. The code is used up
// before the change, so it confirms one disconnect at most.
func DisconnectStWallets(ctx context.Context, settings *StWalletDisconnectSettings, userId server.Id, networkId server.Id, code string) (disconnected *StWalletDisconnected, returnErr error) {
	code = normalizeAccountCode(code)
	state := getAccountCodeRequest(ctx, accountCodePurposeWalletDisconnect, userId, 1)
	if state == nil || state.Fields["network_id"] != networkId.String() {
		return nil, ErrStWalletDisconnectCodeExpired
	}
	if code == "" {
		return nil, ErrStWalletDisconnectCodeInvalid
	}
	tried := tryAccountCodeRequest(ctx, accountCodePurposeWalletDisconnect, userId, state.Id, []string{code}, settings.TryLimit)
	switch {
	case tried.Missing, tried.Spent:
		return nil, ErrStWalletDisconnectCodeExpired
	case tried.Wrong || !tried.Confirmed[0]:
		return nil, ErrStWalletDisconnectCodeInvalid
	}
	if !removeAccountCodeRequest(ctx, accountCodePurposeWalletDisconnect, userId, state.Id) {
		// another try used the code first
		return nil, ErrStWalletDisconnectCodeExpired
	}

	server.Tx(ctx, func(tx server.PgTx) {
		disconnected = nil
		returnErr = nil
		if !stWalletDisconnectOwnerInTx(ctx, tx, userId, networkId) {
			returnErr = ErrStWalletDisconnectOwnerOnly
			return
		}
		networkColdkey, providers := stWalletsToDisconnectInTx(ctx, tx, networkId)
		if networkColdkey == nil && len(providers) == 0 {
			returnErr = ErrStWalletDisconnectNoWallet
			return
		}
		now := server.NowUtc()
		if networkColdkey != nil {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM st_wallet WHERE network_id = $1`, networkId))
		}
		zero := make([]byte, 32)
		for _, provider := range providers {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO st_provider_wallet_history (
						client_id, network_id, coldkey_ss58, coldkey_pubkey, set_time, original_message, original_signature
					) VALUES ($1, $2, '', $3, $4, NULL, NULL)
				`,
				provider.clientId,
				networkId,
				zero,
				now,
			))
		}
		oweSecurityWalletNoticeInTx(ctx, tx, networkId, SecurityNoticeSubnetWallet, func() []*SecurityWalletChange {
			changes := []*SecurityWalletChange{}
			if networkColdkey != nil {
				changes = append(changes, newSecurityWalletChange(ctx, SecurityWalletDestinationSubnetNetwork, subnetKeyWallet(*networkColdkey), nil, 0, 0))
			}
			for _, provider := range providers {
				changes = append(changes, newSecurityWalletChange(ctx, SecurityWalletDestinationSubnetProvider, subnetKeyWallet(provider.coldkey), nil, 0, 0))
			}
			return changes
		})
		disconnected = &StWalletDisconnected{
			NetworkWallet:       networkColdkey != nil,
			ProviderWalletCount: len(providers),
		}
	})
	return
}

// Whether the network has a wallet the disconnect would remove.
func HasDisconnectableStWallet(ctx context.Context, networkId server.Id) (has bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		networkColdkey, providers := stWalletsToDisconnectInTx(ctx, tx, networkId)
		has = networkColdkey != nil || 0 < len(providers)
	})
	return
}
