package model

// The network-create audit (audit_network_event.event_details) stored the whole
// sign-up request, so every sign-up path wrote its credentials into rows that
// outlive the request: the password, the sign-in token, the balance code, and
// the wallet's signature and signed message. The audit stores an allowlist of
// the request instead (auditNetworkCreateRequest). These tests sign up through
// each path of NetworkCreate with synthetic credentials and read the stored
// row; network_create_audit_allowlist_test.go checks the allowlist itself.
// Addresses are documentation addresses and every credential is synthetic.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The request keys that carry a credential. None may appear in an audit row.
var networkCreateAuditCredentialKeys = []string{
	"password",
	"auth_jwt",
	"balance_code",
	"wallet_signature",
	"wallet_message",
}

// The event details of the network's created event.
func networkCreateAuditEventDetails(t testing.TB, ctx context.Context, networkId server.Id) string {
	t.Helper()
	eventDetails := ""
	found := false
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT event_details
				FROM audit_network_event
				WHERE network_id = $1 AND event_type = $2
			`,
			networkId,
			AuditEventTypeNetworkCreated,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&eventDetails))
				found = true
			}
		})
	})
	if !found {
		t.Fatalf("network %s has no created audit event", networkId)
	}
	return eventDetails
}

// Checks a network-create audit row: the stored request is exactly
// wantRequest, the client address is the hash and port of clientAddress, and
// no credential key or value appears anywhere in the row.
func requireNetworkCreateAuditAllowlist(
	t testing.TB,
	eventDetails string,
	wantRequest map[string]any,
	clientAddress string,
	credentials []string,
) {
	t.Helper()
	for _, credential := range credentials {
		if credential == "" {
			continue
		}
		// as written, or as json escapes it inside the row
		escaped, err := json.Marshal(credential)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(eventDetails, credential) || strings.Contains(eventDetails, strings.Trim(string(escaped), `"`)) {
			t.Errorf("the audit row holds a sign-up credential (%d bytes)", len(credential))
		}
	}
	for _, key := range networkCreateAuditCredentialKeys {
		if strings.Contains(eventDetails, `"`+key+`"`) {
			t.Errorf("the audit row holds the credential key %q", key)
		}
	}

	details := map[string]any{}
	if err := json.Unmarshal([]byte(eventDetails), &details); err != nil {
		t.Fatalf("the audit row is not json: %s", err)
	}
	if request := details["network_create"]; !reflect.DeepEqual(request, wantRequest) {
		t.Errorf("the audit row's request is %v, want %v", request, wantRequest)
	}
	clientIp, clientPort, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		t.Fatal(err)
	}
	clientAddressHash, err := server.ClientIpHash(clientIp)
	if err != nil {
		t.Fatal(err)
	}
	if details["client_address_hash"] != hex.EncodeToString(clientAddressHash[:]) || details["client_port"] != float64(clientPort) {
		t.Errorf("the audit row's client address is %v:%v, want the hash and port of %s", details["client_address_hash"], details["client_port"], clientAddress)
	}
}

// A seedphrase sign-up sends no credential of its own; its balance code is the
// one it can carry.
func TestNetworkCreateAuditSeedphraseHoldsNoCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientAddress := "192.0.2.31:40001"
		clientSession := session.NewLocalClientSession(ctx, clientAddress, nil)
		defer clientSession.Cancel()

		balanceCode := "synthetic-balance-code-seedphrase-audit"
		productUpdates := false
		result, err := NetworkCreate(NetworkCreateArgs{
			Terms:          true,
			BalanceCode:    &balanceCode,
			ProductUpdates: &productUpdates,
			ResultErrors:   true,
		}, clientSession)
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("seedphrase sign-up = %v, %v", result, err)
		}

		requireNetworkCreateAuditAllowlist(
			t,
			networkCreateAuditEventDetails(t, ctx, result.Network.NetworkId),
			map[string]any{
				"user_name":          "",
				"network_name":       "",
				"terms":              true,
				"verify_use_numeric": false,
				"product_updates":    false,
			},
			clientAddress,
			[]string{balanceCode},
		)
	})
}

func TestNetworkCreateAuditPasswordHoldsNoCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientAddress := "192.0.2.32:40002"
		clientSession := session.NewLocalClientSession(ctx, clientAddress, nil)
		defer clientSession.Cancel()

		userAuth := "audit-password@example.com"
		password := "synthetic-password-audit-sentinel"
		referralCode := "AUDIT7"
		balanceCode := "synthetic-balance-code-password-audit"
		productUpdates := true
		result, err := NetworkCreate(NetworkCreateArgs{
			UserName:         "audit-password-user",
			UserAuth:         &userAuth,
			Password:         &password,
			NetworkName:      "audit-password-net",
			Terms:            true,
			VerifyUseNumeric: true,
			ReferralCode:     &referralCode,
			BalanceCode:      &balanceCode,
			ProductUpdates:   &productUpdates,
		}, clientSession)
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("password sign-up = %v, %v", result, err)
		}

		requireNetworkCreateAuditAllowlist(
			t,
			networkCreateAuditEventDetails(t, ctx, result.Network.NetworkId),
			map[string]any{
				"user_name":          "audit-password-user",
				"user_auth":          userAuth,
				"network_name":       "audit-password-net",
				"terms":              true,
				"verify_use_numeric": true,
				"referral_code":      referralCode,
				"product_updates":    true,
			},
			clientAddress,
			[]string{password, balanceCode},
		)
	})
}

// A Google or Apple sign-in sign-up through the real token parsers, with a
// synthetic signing key and audience.
func TestNetworkCreateAuditSignInHoldsNoCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Testing_SetSsoJwkKeys([]any{&key.PublicKey})()
		defer session.Testing_SetSsoClientIds([]string{"audit-sign-in-client.example"})()

		for _, c := range []struct {
			authJwtType   string
			issuer        string
			emailVerified any
			networkName   string
			clientAddress string
		}{
			{
				authJwtType:   string(AuthTypeGoogle),
				issuer:        "https://accounts.google.com",
				emailVerified: true,
				networkName:   "audit-google-net",
				clientAddress: "192.0.2.33:40003",
			},
			{
				authJwtType:   string(AuthTypeApple),
				issuer:        "https://appleid.apple.com",
				emailVerified: "true",
				networkName:   "audit-apple-net",
				clientAddress: "192.0.2.34:40004",
			},
		} {
			authJwt, err := gojwt.NewWithClaims(gojwt.SigningMethodRS256, gojwt.MapClaims{
				"iss":            c.issuer,
				"aud":            "audit-sign-in-client.example",
				"sub":            "synthetic-subject-" + c.authJwtType,
				"email":          "audit-" + c.authJwtType + "@example.com",
				"email_verified": c.emailVerified,
				"iat":            time.Now().Add(-time.Minute).Unix(),
				"exp":            time.Now().Add(time.Hour).Unix(),
			}).SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			clientSession := session.NewLocalClientSession(ctx, c.clientAddress, nil)
			defer clientSession.Cancel()

			authJwtType := c.authJwtType
			balanceCode := "synthetic-balance-code-" + c.authJwtType + "-audit"
			result, err := NetworkCreate(NetworkCreateArgs{
				UserName:    "audit-sign-in-user",
				AuthJwt:     &authJwt,
				AuthJwtType: &authJwtType,
				NetworkName: c.networkName,
				Terms:       true,
				BalanceCode: &balanceCode,
			}, clientSession)
			if err != nil || result.Error != nil || result.Network == nil {
				t.Fatalf("%s sign-up = %v, %v", c.authJwtType, result, err)
			}

			requireNetworkCreateAuditAllowlist(
				t,
				networkCreateAuditEventDetails(t, ctx, result.Network.NetworkId),
				map[string]any{
					"user_name":          "audit-sign-in-user",
					"auth_jwt_type":      c.authJwtType,
					"network_name":       c.networkName,
					"terms":              true,
					"verify_use_numeric": false,
				},
				c.clientAddress,
				// the token's signature alone would authenticate nothing, but
				// the whole token must not appear, nor any of its parts
				append([]string{authJwt, balanceCode}, strings.Split(authJwt, ".")...),
			)
		}
	})
}

func TestNetworkCreateAuditWalletHoldsNoCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			newSigner     func(testing.TB) acceptanceWalletSigner
			networkName   string
			clientAddress string
		}{
			{
				newSigner:     newSolanaAcceptanceWalletSigner,
				networkName:   "audit-solana-net",
				clientAddress: "192.0.2.35:40005",
			},
			{
				newSigner:     newBittensorAcceptanceWalletSigner,
				networkName:   "audit-bittensor-net",
				clientAddress: "192.0.2.36:40006",
			},
		} {
			signer := c.newSigner(t)
			walletAuth := signedAcceptanceWalletChallenge(t, ctx, signer)
			signature := walletAuth.Signature
			message := walletAuth.Message
			clientSession := session.NewLocalClientSession(ctx, c.clientAddress, nil)
			defer clientSession.Cancel()

			balanceCode := "synthetic-balance-code-" + signer.blockchain + "-audit"
			result, err := NetworkCreate(NetworkCreateArgs{
				NetworkName: c.networkName,
				Terms:       true,
				WalletAuth:  walletAuth,
				BalanceCode: &balanceCode,
			}, clientSession)
			if err != nil || result.Error != nil || result.Network == nil {
				t.Fatalf("%s wallet sign-up = %v, %v", signer.blockchain, result, err)
			}

			requireNetworkCreateAuditAllowlist(
				t,
				networkCreateAuditEventDetails(t, ctx, result.Network.NetworkId),
				map[string]any{
					"user_name":          "",
					"network_name":       c.networkName,
					"terms":              true,
					"verify_use_numeric": false,
					"wallet_auth": map[string]any{
						"wallet_address": signer.address,
						"blockchain":     signer.blockchain,
					},
				},
				c.clientAddress,
				[]string{signature, message, balanceCode},
			)
		}
	})
}
