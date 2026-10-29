// The security notice's own machinery against the database: the savepoint that
// keeps a failed notice from failing its change, the metric of each result, and
// the sign-in flag that tells an account's change from its creation.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

func securityNoticeCount(notice string, result string) float64 {
	return testutil.ToFloat64(securityNoticeCounter.WithLabelValues(notice, result))
}

// A notice that fails after its writes ran is rolled back to its savepoint: the
// change commits, the outbox holds nothing of the notice, and the failure is
// counted. The next change, without the fault, owes its notice.
func TestSecurityNoticeFailureRollsBackTheNoticeAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth, clientSession := authParityAccount(t, ctx, "security-guard")
		networkId := clientSession.ByJwt.NetworkId
		addressA, addressB := testSolanaAddress(t), testSolanaAddress(t)
		walletA := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
			NetworkId: networkId, Blockchain: SOL.String(), WalletAddress: addressA, DefaultTokenType: "USDC",
		})
		walletB := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
			NetworkId: networkId, Blockchain: SOL.String(), WalletAddress: addressB, DefaultTokenType: "USDC",
		})

		failedBefore := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeFailed)
		faultCtx := context.WithValue(ctx, securityNoticeFaultKey{}, func(notice string, result string) {
			// fail after the notice wrote its message
			if notice == SecurityNoticePayoutWallet && result == securityNoticeQueued {
				panic(errors.New("synthetic notice failure after its writes"))
			}
		})
		if err := SetPayoutWallet(faultCtx, networkId, *walletA); err != nil {
			t.Fatal(err)
		}
		if payoutWalletId := GetPayoutWalletId(ctx, networkId); payoutWalletId == nil || *payoutWalletId != *walletA {
			t.Fatalf("the payout wallet is %v after a failed notice, want the selected one", payoutWalletId)
		}
		if count := allTestSecurityNotices(ctx, networkId); count != 0 {
			t.Fatalf("the failed notice left %d messages", count)
		}
		if delta := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeFailed) - failedBefore; delta != 1 {
			t.Fatalf("counted %v failed notices, want 1", delta)
		}

		queuedBefore := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeQueued)
		coalescedBefore := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeCoalesced)
		if err := SetPayoutWallet(ctx, networkId, *walletB); err != nil {
			t.Fatal(err)
		}
		if err := SetPayoutWallet(ctx, networkId, *walletA); err != nil {
			t.Fatal(err)
		}
		if delta := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeQueued) - queuedBefore; delta != 1 {
			t.Fatalf("counted %v queued notices, want 1", delta)
		}
		if delta := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeCoalesced) - coalescedBefore; delta != 1 {
			t.Fatalf("counted %v coalesced notices, want 1", delta)
		}
		notices := unsentTestNotices(t, ctx, networkId, SecurityPayoutWalletTemplateName)
		if len(notices) != 1 || notices[0].userAuth != userAuth || len(notices[0].changes(t)) != 2 {
			t.Fatalf("owed %d notices, want one listing both changes", len(notices))
		}
	})
}

// An account without an email counts its notice as no_email and owes nothing.
func TestSecurityNoticeCountsAnAccountWithoutAnEmail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId := server.NewId(), server.NewId()
		signer := newSolanaAcceptanceWalletSigner(t)
		message := "synthetic wallet network " + networkId.String()
		signature, err := signer.sign(message)
		if err != nil {
			t.Fatal(err)
		}
		Testing_CreateNetworkByWallet(ctx, networkId, "wallet-"+networkId.String()[:8], userId, signer.address, signature, message)

		noEmailBefore := securityNoticeCount(SecurityNoticeSignIn, securityNoticeNoEmail)
		if _, err := GenerateSeedphrase(ctx, userId); err != nil {
			t.Fatal(err)
		}
		if delta := securityNoticeCount(SecurityNoticeSignIn, securityNoticeNoEmail) - noEmailBefore; delta != 1 {
			t.Fatalf("counted %v no_email notices, want 1", delta)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, networkId); count != 0 {
			t.Fatalf("a wallet account owed %d messages", count)
		}
	})
}

// AddAuth's Apple or Google sign-in owes the sign-in notice (Notify); the same
// write for an account's creation or a migration (no Notify) owes none.
func TestSsoSignInAddedOwesTheNoticeOnlyWhenTheAccountGainsIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, userAuth, clientSession := authParityAccount(t, ctx, "security-sso")
		networkId := clientSession.ByJwt.NetworkId

		err := addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: AuthJwt{AuthType: AuthTypeApple, UserAuth: "Relay.Owner@privaterelay.appleid.com"},
			AuthJwtType:   SsoAuthTypeApple,
			UserId:        userId,
			Notify:        true,
		}, ctx)
		if err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, SecuritySignInTemplateName, []string{"relay.owner@privaterelay.appleid.com", userAuth}, map[string]any{
			"action": "added", "method": "apple", "identity": "r…@privaterelay.appleid.com",
		})

		addTestSsoAuth(t, ctx, userId, SsoAuthTypeGoogle, "owner@example.com")
		requireNoTestNotice(t, ctx, networkId, "a sign-in written without Notify")
	})
}
