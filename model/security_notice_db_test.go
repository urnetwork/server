// The security notices against the database (owner decision 2026-10-29): every
// payout wallet change and every sign-in method or password change owes one
// email notice to the admin's sign-in email addresses, in the change's own
// transaction; accounts without an email owe none; repeats inside the window
// join one notice; and a notice that cannot be owed never fails the change.
//
// These tests reach the changes through the entry points the routes call and
// read the outbox by template name, so they hold the behavior apart from the
// notice code that implements it.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gagliardetto/solana-go"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The outbox templates, by name.
const (
	testPayoutWalletNoticeTemplate = "security_payout_wallet_changed"
	testSignInNoticeTemplate       = "security_sign_in_changed"
	testPasswordNoticeTemplate     = "auth_password_set"
)

// One unsent outbox message, as a test reads it.
type testOutboxNotice struct {
	messageId    server.Id
	userAuth     string
	templateJson string
	deliverTime  *time.Time
	createTime   time.Time
}

// The changes a security notice lists.
func (self *testOutboxNotice) changes(t testing.TB) []map[string]any {
	t.Helper()
	var notice struct {
		Changes []map[string]any `json:"changes"`
	}
	if err := json.Unmarshal([]byte(self.templateJson), &notice); err != nil {
		t.Fatalf("notice fields %q do not decode: %v", self.templateJson, err)
	}
	return notice.Changes
}

// The network's unsent messages of the template, oldest first.
func unsentTestNotices(t testing.TB, ctx context.Context, networkId server.Id, templateName string) []*testOutboxNotice {
	t.Helper()
	notices := []*testOutboxNotice{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT message_id, user_auth, template_json::text, deliver_time, create_time
				FROM account_message_outbox
				WHERE
					network_id = $1 AND
					template_name = $2 AND
					sent_time IS NULL AND
					abandon_time IS NULL
				ORDER BY create_time, message_id
			`,
			networkId,
			templateName,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				notice := &testOutboxNotice{}
				server.Raise(result.Scan(&notice.messageId, &notice.userAuth, &notice.templateJson, &notice.deliverTime, &notice.createTime))
				notices = append(notices, notice)
			}
		})
	})
	return notices
}

// Every security notice of the network, sent or not.
func allTestSecurityNotices(ctx context.Context, networkId server.Id) int {
	return countRows(
		ctx,
		`SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1 AND template_name = ANY($2)`,
		networkId,
		[]string{testPayoutWalletNoticeTemplate, testSignInNoticeTemplate},
	)
}

// Marks the network's messages sent, as a delivery would, so the next change
// owes a notice of its own.
func settleTestNotices(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE account_message_outbox SET sent_time = now() WHERE network_id = $1 AND sent_time IS NULL`,
			networkId,
		))
	})
}

// Forgets the user's add and remove attempts, so a test can make more changes
// than the daily limit allows one account.
func resetTestAccountActions(ctx context.Context, userId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_user_action_attempt WHERE user_id = $1`, userId))
	})
}

// A wallet address masked as a notice shows it.
func testMasked(address string) string {
	return address[:4] + "…" + address[len(address)-4:]
}

// Asserts the network owes exactly one unsent notice of the template, to each
// of the recipients, listing exactly one change with the fields; then settles it.
func requireOneTestNotice(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	templateName string,
	recipients []string,
	fields map[string]any,
) {
	t.Helper()
	notices := unsentTestNotices(t, ctx, networkId, templateName)
	if len(notices) != len(recipients) {
		t.Fatalf("%s: owed %d notices, want one to each of %v (fields %v)", templateName, len(notices), recipients, fields)
	}
	for i, notice := range notices {
		if !strings.EqualFold(notice.userAuth, recipients[i]) && !containsFold(recipients, notice.userAuth) {
			t.Fatalf("%s: a notice went to %s, want only %v", templateName, notice.userAuth, recipients)
		}
		if notice.deliverTime == nil {
			t.Fatalf("%s: the notice is held, want due", templateName)
		}
		changes := notice.changes(t)
		if len(changes) != 1 {
			t.Fatalf("%s: the notice lists %d changes, want 1: %s", templateName, len(changes), notice.templateJson)
		}
		for key, want := range fields {
			if got := changes[0][key]; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s: change %s = %v, want %v (%s)", templateName, key, got, want, notice.templateJson)
			}
		}
		if _, ok := changes[0]["change_time"]; !ok {
			t.Fatalf("%s: the change has no time: %s", templateName, notice.templateJson)
		}
	}
	settleTestNotices(ctx, networkId)
}

func containsFold(values []string, value string) bool {
	for _, v := range values {
		if strings.EqualFold(v, value) {
			return true
		}
	}
	return false
}

// Asserts the network owes no unsent security notice.
func requireNoTestNotice(t testing.TB, ctx context.Context, networkId server.Id, what string) {
	t.Helper()
	for _, templateName := range []string{testPayoutWalletNoticeTemplate, testSignInNoticeTemplate} {
		if notices := unsentTestNotices(t, ctx, networkId, templateName); len(notices) != 0 {
			t.Fatalf("%s owed %d %s notices, want none: %s", what, len(notices), templateName, notices[0].templateJson)
		}
	}
}

// A new random Solana address.
func testSolanaAddress(t testing.TB) string {
	t.Helper()
	privateKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return privateKey.PublicKey().String()
}

// A Bittensor key, its ss58 address and its public key, from a seed byte.
func testBittensorKey(t testing.TB, seed byte) (*schnorrkel.MiniSecretKey, string, [32]byte) {
	t.Helper()
	key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{seed})
	if err != nil {
		t.Fatal(err)
	}
	pubkey := key.Public().Encode()
	address, err := ss58.Encode(pubkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return key, address, pubkey
}

// Each sign-in method change owes exactly one notice to the admin's email: a
// phone, a wallet and a seedphrase added, a seedphrase replaced, Google linked at
// sign-in, and each of them removed. A change that changes nothing (a wallet
// added again) owes none, nor does a legacy Apple account's backfill at sign-in.
func TestSecurityNoticeOwedOnceForEachSignInChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, userAuth, clientSession := authParityAccount(t, ctx, "security-sign-in")
		networkId := clientSession.ByJwt.NetworkId
		recipients := []string{userAuth}
		password := "synthetic-password-1"

		phone := "+1 415-555-0123"
		result, err := AddAuth(AddAuthMethod{UserAuth: &phone, Password: &password}, clientSession)
		if err != nil || result.Error != nil {
			t.Fatalf("add phone: %v %+v", err, result)
		}
		requireOneTestNotice(t, ctx, networkId, testSignInNoticeTemplate, recipients, map[string]any{
			"action": "added", "method": "phone", "identity": "…23",
		})

		signer := newSolanaAcceptanceWalletSigner(t)
		if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
			t.Fatalf("add wallet: %+v", result.Error)
		}
		requireOneTestNotice(t, ctx, networkId, testSignInNoticeTemplate, recipients, map[string]any{
			"action": "added", "method": "solana", "identity": testMasked(signer.address),
		})
		// the same wallet again changes nothing
		if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
			t.Fatalf("add the wallet again: %+v", result.Error)
		}
		requireNoTestNotice(t, ctx, networkId, "adding a bound wallet again")

		if _, err := GenerateSeedphrase(ctx, userId); err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, testSignInNoticeTemplate, recipients, map[string]any{
			"action": "added", "method": "seedphrase",
		})
		if _, err := RegenerateSeedphrase(ctx, userId); err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, testSignInNoticeTemplate, recipients, map[string]any{
			"action": "replaced", "method": "seedphrase",
		})

		// signing in with Google for the account's email links Google to it
		googleLogin, err := handleLoginParsedAuthJwt(&HandleLoginParsedAuthJwtArgs{
			AuthJwt: AuthJwt{AuthType: AuthTypeGoogle, UserAuth: userAuth, UserName: "synthetic"},
		}, ctx)
		if err != nil || googleLogin.Network == nil {
			t.Fatalf("google sign-in: %v %+v", err, googleLogin)
		}
		requireOneTestNotice(t, ctx, networkId, testSignInNoticeTemplate, recipients, map[string]any{
			"action": "added", "method": "google", "identity": userAuth[:1] + "…@bringyour.com",
		})
		// signing in again with the linked method changes nothing
		if _, err := handleLoginParsedAuthJwt(&HandleLoginParsedAuthJwtArgs{
			AuthJwt: AuthJwt{AuthType: AuthTypeGoogle, UserAuth: userAuth, UserName: "synthetic"},
		}, ctx); err != nil {
			t.Fatal(err)
		}
		requireNoTestNotice(t, ctx, networkId, "signing in with a linked method")

		resetTestAccountActions(ctx, userId)
		for _, removed := range []struct {
			authType string
			fields   map[string]any
		}{
			{"phone", map[string]any{"action": "removed", "method": "phone", "identity": "…23"}},
			{"solana", map[string]any{"action": "removed", "method": "solana", "identity": testMasked(signer.address)}},
			{"seedphrase", map[string]any{"action": "removed", "method": "seedphrase"}},
			{"google", map[string]any{"action": "removed", "method": "google"}},
		} {
			if err := RemoveAuth(ctx, userId, removed.authType); err != nil {
				t.Fatalf("remove %s: %v", removed.authType, err)
			}
			requireOneTestNotice(t, ctx, networkId, testSignInNoticeTemplate, recipients, removed.fields)
		}
		// a refused removal changes nothing
		if err := RemoveAuth(ctx, userId, "apple"); err == nil {
			t.Fatal("removed a method the account does not have")
		}
		requireNoTestNotice(t, ctx, networkId, "a refused removal")

		// a legacy Apple account found by its own email is backfilled, not linked
		legacyNetworkId, legacyUserId := server.NewId(), server.NewId()
		legacyEmail := fmt.Sprintf("legacy-%s@bringyour.com", legacyNetworkId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`, legacyNetworkId, "legacy-"+legacyNetworkId.String()[:8], legacyUserId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified) VALUES ($1, 'legacy', 'apple', $2, true)`, legacyUserId, legacyEmail))
		})
		if login, err := handleLoginParsedAuthJwt(&HandleLoginParsedAuthJwtArgs{
			AuthJwt: AuthJwt{AuthType: AuthTypeApple, UserAuth: legacyEmail, UserName: "legacy"},
		}, ctx); err != nil || login.Network == nil {
			t.Fatalf("legacy apple sign-in: %v %+v", err, login)
		}
		requireNoTestNotice(t, ctx, legacyNetworkId, "a legacy apple backfill")
	})
}

// Each change of where payouts go owes exactly one notice, with the wallets
// masked and their chains: the USDC payout wallet set, replaced, replaced by a
// promotion when it is removed, and removed; and the subnet's Bittensor wallets
// set, replaced, set for a provider, and moved by a provider or a network
// consent. Selecting the payout wallet again owes none.
func TestSecurityNoticeOwedOnceForEachPayoutWalletChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth, clientSession := authParityAccount(t, ctx, "security-payout")
		networkId := clientSession.ByJwt.NetworkId
		recipients := []string{userAuth}

		createWallet := func(address string) server.Id {
			walletId := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
				NetworkId:        networkId,
				Blockchain:       SOL.String(),
				WalletAddress:    address,
				DefaultTokenType: "USDC",
			})
			if walletId == nil {
				t.Fatal("the wallet was not created")
			}
			return *walletId
		}
		addressA, addressB := testSolanaAddress(t), testSolanaAddress(t)
		walletA, walletB := createWallet(addressA), createWallet(addressB)
		requireNoTestNotice(t, ctx, networkId, "adding a wallet that is not the payout wallet")

		if err := SetPayoutWallet(ctx, networkId, walletA); err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "usdc_payout", "new_chain": "SOL", "new_wallet": testMasked(addressA), "previous_wallet": nil,
		})
		if err := SetPayoutWallet(ctx, networkId, walletB); err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "usdc_payout", "previous_chain": "SOL", "previous_wallet": testMasked(addressA), "new_chain": "SOL", "new_wallet": testMasked(addressB),
		})
		if err := SetPayoutWallet(ctx, networkId, walletB); err != nil {
			t.Fatal(err)
		}
		requireNoTestNotice(t, ctx, networkId, "selecting the payout wallet again")
		// another network's wallet is refused and changes nothing
		if err := SetPayoutWallet(ctx, networkId, server.NewId()); err == nil {
			t.Fatal("selected a wallet the network does not own")
		}
		requireNoTestNotice(t, ctx, networkId, "a refused selection")

		// removing the payout wallet promotes the other one: one change
		if removed := RemoveWallet(walletB, clientSession); !removed.Success || removed.PayoutWalletId == nil || *removed.PayoutWalletId != walletA {
			t.Fatalf("remove the payout wallet: %+v", removed)
		}
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"previous_wallet": testMasked(addressB), "new_wallet": testMasked(addressA),
		})
		// removing the last one leaves none
		if removed := RemoveWallet(walletA, clientSession); !removed.Success || removed.PayoutWalletId != nil {
			t.Fatalf("remove the last wallet: %+v", removed)
		}
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"previous_wallet": testMasked(addressA), "new_wallet": nil,
		})

		// the subnet claim wallet
		_, coldkeyX, pubkeyX := testBittensorKey(t, 201)
		_, coldkeyY, pubkeyY := testBittensorKey(t, 202)
		SetStWalletOriginal(ctx, networkId, nil, coldkeyX, pubkeyX, nil, nil)
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "subnet_network", "new_chain": "TAO", "new_wallet": testMasked(coldkeyX), "previous_wallet": nil,
		})
		SetStWalletOriginal(ctx, networkId, nil, coldkeyX, pubkeyX, nil, nil)
		requireNoTestNotice(t, ctx, networkId, "setting the same subnet wallet again")
		SetStWalletOriginal(ctx, networkId, nil, coldkeyY, pubkeyY, nil, nil)
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "subnet_network", "previous_wallet": testMasked(coldkeyX), "new_wallet": testMasked(coldkeyY),
		})
		// a provider's wallet: the provider's changes, the network's stays
		clientId := server.NewId()
		SetStWalletOriginal(ctx, networkId, &clientId, coldkeyY, pubkeyY, nil, nil)
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "subnet_provider", "previous_wallet": nil, "new_wallet": testMasked(coldkeyY),
		})
	})
}

// A provider wallet mapping consent and a network wallet mapping consent that
// give subnet earnings to another wallet each owe one notice, with the epochs
// the consent covers.
func TestSecurityNoticeOwedForSubnetWalletConsents(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := walletMappingProspectiveFixture(t)
		userAuth := Testing_CreateNetwork(ctx, f.owner.NetworkId, "security-consent-"+f.owner.NetworkId.String()[:8], f.owner.UserId)
		recipients := []string{userAuth}

		accepted, err := AcceptWalletMappingConsent(ctx, f.owner, f.challenge(t, 51), f.address)
		if err != nil || accepted == nil || !accepted.Applied {
			t.Fatal("provider consent", accepted, err)
		}
		notices := unsentTestNotices(t, ctx, f.owner.NetworkId, testPayoutWalletNoticeTemplate)
		if len(notices) != 1 {
			t.Fatalf("a provider consent owed %d notices, want 1", len(notices))
		}
		changes := notices[0].changes(t)
		if len(changes) != 2 ||
			changes[0]["destination"] != "subnet_provider" || changes[0]["new_wallet"] != testMasked(f.address) || fmt.Sprint(changes[0]["from_epoch"]) != "51" || fmt.Sprint(changes[0]["through_epoch"]) != "151" ||
			changes[1]["destination"] != "subnet_network" || changes[1]["new_wallet"] != testMasked(f.address) {
			t.Fatalf("a provider consent listed %s, want the provider's and the network's new wallet", notices[0].templateJson)
		}
		settleTestNotices(ctx, f.owner.NetworkId)

		rootKey, err := crypto.HexToECDSA(strings.Repeat("42", 32))
		if err != nil {
			t.Fatal(err)
		}
		owner := NetworkWalletMappingOwner{
			Domain:    f.owner.Domain,
			UserId:    f.owner.UserId,
			NetworkId: f.owner.NetworkId,
			Prospective: &WalletMappingProspectiveOwner{
				Boundary: protocol.ClientKeyEffectiveBoundary{Epoch: 50, Block: 500, Hash: [32]byte{51}},
				RootKey:  rootKey,
			},
		}
		consent := func(seed byte, fromEpoch uint64) string {
			key, address, pubkey := testBittensorKey(t, seed)
			message, err := CreateNetworkWalletMappingChallenge(ctx, owner, pubkey, fromEpoch, fromEpoch+10)
			if err != nil {
				t.Fatal(err)
			}
			signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := AcceptNetworkWalletMappingConsent(ctx, owner, protocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}, address)
			if err != nil || accepted == nil || !accepted.Applied {
				t.Fatal("network consent", accepted, err)
			}
			return address
		}
		first := consent(211, 51)
		requireOneTestNotice(t, ctx, f.owner.NetworkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "subnet_network_consent", "previous_wallet": nil, "new_wallet": testMasked(first), "from_epoch": 51, "through_epoch": 61,
		})
		second := consent(212, 52)
		requireOneTestNotice(t, ctx, f.owner.NetworkId, testPayoutWalletNoticeTemplate, recipients, map[string]any{
			"destination": "subnet_network_consent", "previous_wallet": testMasked(first), "new_wallet": testMasked(second),
		})
	})
}

// A phone number and a wallet are no email: an account that signs in only with
// those owes no notice, by email or SMS, for any change, while an account with
// an email owes one for each.
func TestSecurityNoticesSkipAccountsWithoutAnEmail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// the same changes on each account: a wallet sign-in, a seedphrase, a
		// payout wallet
		change := func(userId server.Id, networkId server.Id, clientSession *session.ClientSession) {
			if result := addWalletThroughAddAuth(t, ctx, clientSession, newBittensorAcceptanceWalletSigner(t)); result.Error != nil {
				t.Fatalf("add wallet: %+v", result.Error)
			}
			if _, err := GenerateSeedphrase(ctx, userId); err != nil {
				t.Fatal(err)
			}
			walletId := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
				NetworkId:        networkId,
				Blockchain:       SOL.String(),
				WalletAddress:    testSolanaAddress(t),
				DefaultTokenType: "USDC",
			})
			if walletId == nil {
				t.Fatal("the wallet was not created")
			}
			if err := SetPayoutWallet(ctx, networkId, *walletId); err != nil {
				t.Fatal(err)
			}
		}
		// every message the network owes, of any template
		owed := func(networkId server.Id) int {
			return countRows(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, networkId)
		}

		// an email account: the control
		emailUserId, _, emailSession := authParityAccount(t, ctx, "security-email-only")
		change(emailUserId, emailSession.ByJwt.NetworkId, emailSession)
		if count := allTestSecurityNotices(ctx, emailSession.ByJwt.NetworkId); count == 0 {
			t.Fatal("an email account owed no security notice")
		}

		// a phone account
		phoneNetworkId, phoneUserId := server.NewId(), server.NewId()
		phone := "+1 415-555-0177"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`, phoneNetworkId, "phone-"+phoneNetworkId.String()[:8], phoneUserId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified) VALUES ($1, 'phone', 'password', $2, true)`, phoneUserId, phone))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified) VALUES ($1, $2, 'phone', true)`, phoneUserId, phone))
		})
		phoneSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: phoneNetworkId, UserId: phoneUserId, NetworkName: "phone"})
		change(phoneUserId, phoneNetworkId, phoneSession)
		if count := owed(phoneNetworkId); count != 0 {
			t.Fatalf("a phone account owed %d messages, want none", count)
		}

		// a wallet account
		walletNetworkId, walletUserId := server.NewId(), server.NewId()
		walletSigner := newSolanaAcceptanceWalletSigner(t)
		message := "synthetic wallet network " + walletNetworkId.String()
		signature, err := walletSigner.sign(message)
		if err != nil {
			t.Fatal(err)
		}
		Testing_CreateNetworkByWallet(ctx, walletNetworkId, "wallet-"+walletNetworkId.String()[:8], walletUserId, walletSigner.address, signature, message)
		walletSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: walletNetworkId, UserId: walletUserId, NetworkName: "wallet"})
		change(walletUserId, walletNetworkId, walletSession)
		if count := owed(walletNetworkId); count != 0 {
			t.Fatalf("a wallet account owed %d messages, want none", count)
		}
	})
}

// Repeats inside the window join one notice that lists them in order, and the
// notice keeps its due time. A change after the notice is due, or while a
// delivery holds it, owes a new one.
func TestSecurityNoticesCoalesceRepeatsWithinTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, userAuth, clientSession := authParityAccount(t, ctx, "security-coalesce")
		networkId := clientSession.ByJwt.NetworkId
		createWallet := func(address string) server.Id {
			walletId := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
				NetworkId: networkId, Blockchain: SOL.String(), WalletAddress: address, DefaultTokenType: "USDC",
			})
			if walletId == nil {
				t.Fatal("the wallet was not created")
			}
			return *walletId
		}
		addressA, addressB := testSolanaAddress(t), testSolanaAddress(t)
		walletA, walletB := createWallet(addressA), createWallet(addressB)

		for _, walletId := range []server.Id{walletA, walletB, walletA} {
			if err := SetPayoutWallet(ctx, networkId, walletId); err != nil {
				t.Fatal(err)
			}
		}
		notices := unsentTestNotices(t, ctx, networkId, testPayoutWalletNoticeTemplate)
		if len(notices) != 1 {
			t.Fatalf("three changes in a burst owed %d notices, want 1", len(notices))
		}
		changes := notices[0].changes(t)
		if len(changes) != 3 ||
			changes[0]["previous_wallet"] != nil || changes[0]["new_wallet"] != testMasked(addressA) ||
			changes[1]["previous_wallet"] != testMasked(addressA) || changes[1]["new_wallet"] != testMasked(addressB) ||
			changes[2]["previous_wallet"] != testMasked(addressB) || changes[2]["new_wallet"] != testMasked(addressA) {
			t.Fatalf("the burst's notice lists %s, want the three changes in order", notices[0].templateJson)
		}
		window := notices[0].deliverTime.Sub(notices[0].createTime)
		if window < 30*time.Second || 2*time.Minute < window {
			t.Fatalf("the notice is due %s after it was owed, want about a minute", window)
		}
		if notices[0].userAuth != userAuth {
			t.Fatalf("the notice went to %s, want %s", notices[0].userAuth, userAuth)
		}

		// once the notice is due, a change owes a new one
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_message_outbox SET deliver_time = $2 WHERE message_id = $1`, notices[0].messageId, server.NowUtc().Add(-time.Second)))
		})
		if err := SetPayoutWallet(ctx, networkId, walletB); err != nil {
			t.Fatal(err)
		}
		notices = unsentTestNotices(t, ctx, networkId, testPayoutWalletNoticeTemplate)
		if len(notices) != 2 || len(notices[0].changes(t)) != 3 || len(notices[1].changes(t)) != 1 {
			t.Fatalf("a change after the notice was due owed %d notices, want a second one", len(notices))
		}
		// while a delivery holds a notice, a change owes a new one
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_message_outbox SET claim_id = $2, claim_until = $3, attempt_count = 1 WHERE message_id = $1`, notices[1].messageId, server.NewId(), server.NowUtc().Add(time.Minute)))
		})
		if err := SetPayoutWallet(ctx, networkId, walletA); err != nil {
			t.Fatal(err)
		}
		if notices := unsentTestNotices(t, ctx, networkId, testPayoutWalletNoticeTemplate); len(notices) != 3 {
			t.Fatalf("a change while a delivery held the notice owed %d notices, want a third", len(notices))
		}

		// sign-in changes in a burst join one notice too
		phone := "+1 415-555-0199"
		password := "synthetic-password-1"
		if result, err := AddAuth(AddAuthMethod{UserAuth: &phone, Password: &password}, clientSession); err != nil || result.Error != nil {
			t.Fatalf("add phone: %v %+v", err, result)
		}
		if err := RemoveAuth(ctx, userId, "phone"); err != nil {
			t.Fatal(err)
		}
		signIns := unsentTestNotices(t, ctx, networkId, testSignInNoticeTemplate)
		if len(signIns) != 1 {
			t.Fatalf("an add and a remove in a burst owed %d sign-in notices, want 1", len(signIns))
		}
		if changes := signIns[0].changes(t); len(changes) != 2 || changes[0]["action"] != "added" || changes[1]["action"] != "removed" {
			t.Fatalf("the sign-in notice lists %s, want the add then the remove", signIns[0].templateJson)
		}
	})
}

// Fails, as it runs, every statement that adds or changes an account message
// for the network, until the returned function removes the fault.
func failTestOutboxStatements(ctx context.Context, networkId server.Id) (remove func()) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION synthetic_security_notice_fault() RETURNS trigger AS $fault$
			BEGIN
				RAISE EXCEPTION 'synthetic notice failure';
			END
			$fault$ LANGUAGE plpgsql;
			CREATE TRIGGER synthetic_security_notice_fault
				BEFORE INSERT OR UPDATE ON account_message_outbox
				FOR EACH ROW WHEN (NEW.network_id = '%s')
				EXECUTE FUNCTION synthetic_security_notice_fault();
		`, networkId)))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				DROP TRIGGER synthetic_security_notice_fault ON account_message_outbox;
				DROP FUNCTION synthetic_security_notice_fault();
			`))
		})
	}
}

// A notice that cannot be owed never fails or rolls back the change it reports:
// with every outbox write failing, a password reset, a payout wallet change and
// sign-in changes all go through, owing nothing. Without the fault, the next
// change owes its notice.
func TestSecurityNoticeFailureNeverBlocksTheChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, userAuth, clientSession := authParityAccount(t, ctx, "security-fault")
		networkId := clientSession.ByJwt.NetworkId
		walletA := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
			NetworkId: networkId, Blockchain: SOL.String(), WalletAddress: testSolanaAddress(t), DefaultTokenType: "USDC",
		})
		addressB := testSolanaAddress(t)
		walletB := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
			NetworkId: networkId, Blockchain: SOL.String(), WalletAddress: addressB, DefaultTokenType: "USDC",
		})
		resetResult, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, clientSession)
		if err != nil || resetResult.ResetCode == nil {
			t.Fatal("reset code", err)
		}

		removeFault := failTestOutboxStatements(ctx, networkId)
		changeFailure := func(what string, change func()) {
			t.Helper()
			var failure any
			func() {
				defer func() {
					failure = recover()
				}()
				change()
			}()
			if failure != nil {
				t.Fatalf("%s failed with its notice: %v", what, failure)
			}
		}
		changeFailure("the password reset", func() {
			result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *resetResult.ResetCode, Password: "synthetic-new-password"}, clientSession)
			if err != nil || result.Error != nil {
				panic(fmt.Errorf("password set: %v %+v", err, result))
			}
		})
		login, err := AuthLoginWithPassword(AuthLoginWithPasswordArgs{UserAuth: userAuth, Password: "synthetic-new-password"}, clientSession)
		if err != nil || login.Error != nil {
			t.Fatalf("the new password does not sign in: %v %+v", err, login)
		}
		changeFailure("the payout wallet change", func() {
			if err := SetPayoutWallet(ctx, networkId, *walletA); err != nil {
				panic(err)
			}
		})
		if payoutWalletId := GetPayoutWalletId(ctx, networkId); payoutWalletId == nil || *payoutWalletId != *walletA {
			t.Fatalf("the payout wallet is %v, want the selected one", payoutWalletId)
		}
		phone := "+1 415-555-0142"
		password := "synthetic-password-1"
		changeFailure("the sign-in method added", func() {
			if result, err := AddAuth(AddAuthMethod{UserAuth: &phone, Password: &password}, clientSession); err != nil || result.Error != nil {
				panic(fmt.Errorf("add phone: %v %+v", err, result))
			}
		})
		changeFailure("the sign-in method removed", func() {
			if err := RemoveAuth(ctx, userId, "phone"); err != nil {
				panic(err)
			}
		})
		if authTypes := authTypesOf(t, ctx, userId); hasAuthType(authTypes, "phone") {
			t.Fatalf("the phone sign-in is still bound: %v", authTypes)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, networkId); count != 0 {
			t.Fatalf("owed %d messages while every outbox write failed", count)
		}

		removeFault()
		if err := SetPayoutWallet(ctx, networkId, *walletB); err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, testPayoutWalletNoticeTemplate, []string{userAuth}, map[string]any{
			"new_wallet": testMasked(addressB),
		})
	})
}

// A password reset emails each of the admin's sign-in addresses, the legacy one
// and an Apple or Google sign-in's alike, once each.
func TestPasswordChangedNoticeGoesToEverySignInEmail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, userAuth, clientSession := authParityAccount(t, ctx, "security-password")
		networkId := clientSession.ByJwt.NetworkId
		googleEmail := "Owner.Google@Example.com"
		addTestSsoAuth(t, ctx, userId, SsoAuthTypeGoogle, googleEmail)

		resetResult, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, clientSession)
		if err != nil || resetResult.ResetCode == nil {
			t.Fatal("reset code", err)
		}
		result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *resetResult.ResetCode, Password: "synthetic-new-password"}, clientSession)
		if err != nil || result.Error != nil {
			t.Fatalf("password set: %v %+v", err, result)
		}
		notices := unsentTestNotices(t, ctx, networkId, testPasswordNoticeTemplate)
		got := []string{}
		for _, notice := range notices {
			got = append(got, notice.userAuth)
		}
		want := []string{strings.ToLower(googleEmail), userAuth}
		if len(got) != 2 || !containsFold(got, want[0]) || !containsFold(got, want[1]) {
			t.Fatalf("the password-changed notice went to %v, want each of %v once", got, want)
		}
	})
}
