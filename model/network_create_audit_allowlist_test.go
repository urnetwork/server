package model

// The network-create audit's allowlist (auditNetworkCreateRequest), checked
// without a database. network_create_audit_secrets_test.go signs up through
// each path and reads the stored rows. Every credential is synthetic.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

// The request keys an audit row may hold under `network_create`, and under its
// `wallet_auth`.
var networkCreateAuditRequestKeys = []string{
	"user_name",
	"user_auth",
	"auth_jwt_type",
	"network_name",
	"terms",
	"verify_use_numeric",
	"referral_code",
	"product_updates",
	"wallet_auth",
}

var networkCreateAuditWalletKeys = []string{
	"wallet_address",
	"blockchain",
}

// Every field of the request set at once: the details keep exactly the
// allowlisted fields, with their values, and no credential.
func TestAuditNetworkCreateDetailsKeepOnlyTheAllowlist(t *testing.T) {
	userAuth := "audit-allowlist@example.com"
	authJwt := "synthetic-header.synthetic-claims.synthetic-signature"
	authJwtType := string(AuthTypeGoogle)
	password := "synthetic-password-allowlist"
	referralCode := "ALLOW1"
	balanceCode := "synthetic-balance-code-allowlist"
	productUpdates := false
	networkCreate := NetworkCreateArgs{
		UserName:         "audit-allowlist-user",
		UserAuth:         &userAuth,
		AuthJwt:          &authJwt,
		AuthJwtType:      &authJwtType,
		Password:         &password,
		NetworkName:      "audit-allowlist-net",
		Terms:            true,
		VerifyUseNumeric: true,
		ReferralCode:     &referralCode,
		BalanceCode:      &balanceCode,
		WalletAuth: &WalletAuthArgs{
			PublicKey:  "synthetic-wallet-address",
			Signature:  "synthetic-wallet-signature",
			Message:    "synthetic signed wallet message",
			Blockchain: SOL.String(),
			Nonce:      "synthetic-wallet-nonce",
		},
		ProductUpdates: &productUpdates,
		ResultErrors:   true,
	}

	clientAddressHash := [32]byte{}
	for i := range clientAddressHash {
		clientAddressHash[i] = byte(i)
	}
	clientSession := session.NewLocalClientSessionWithAddressHash(context.Background(), clientAddressHash, 4321, nil)
	defer clientSession.Cancel()

	eventDetails := auditNetworkCreateDetails(networkCreate, clientSession)
	for _, credential := range []string{authJwt, password, balanceCode, "synthetic-wallet-signature", "synthetic signed wallet message", "synthetic-wallet-nonce"} {
		if strings.Contains(eventDetails, credential) {
			t.Errorf("the details hold a sign-up credential: %q", credential)
		}
	}

	details := map[string]any{}
	if err := json.Unmarshal([]byte(eventDetails), &details); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"network_create": map[string]any{
			"user_name":          "audit-allowlist-user",
			"user_auth":          userAuth,
			"auth_jwt_type":      authJwtType,
			"network_name":       "audit-allowlist-net",
			"terms":              true,
			"verify_use_numeric": true,
			"referral_code":      referralCode,
			"product_updates":    false,
			"wallet_auth": map[string]any{
				"wallet_address": "synthetic-wallet-address",
				"blockchain":     SOL.String(),
			},
		},
		"client_address_hash": hex.EncodeToString(clientAddressHash[:]),
		"client_port":         float64(4321),
	}
	if !reflect.DeepEqual(details, want) {
		t.Errorf("the details are %v, want %v", details, want)
	}
}

// The audit record's json keys are the allowlist, and none names a credential.
// A key added to the record must be added to the allowlist above, which is
// where a reviewer sees it.
func TestAuditNetworkCreateRequestNamesNoCredential(t *testing.T) {
	jsonKeys := func(value any) []string {
		keys := []string{}
		recordType := reflect.TypeOf(value)
		for i := 0; i < recordType.NumField(); i += 1 {
			name, _, _ := strings.Cut(recordType.Field(i).Tag.Get("json"), ",")
			keys = append(keys, name)
		}
		slices.Sort(keys)
		return keys
	}
	for _, c := range []struct {
		record any
		want   []string
	}{
		{record: auditNetworkCreateRequest{}, want: networkCreateAuditRequestKeys},
		{record: auditNetworkCreateWallet{}, want: networkCreateAuditWalletKeys},
	} {
		want := slices.Clone(c.want)
		slices.Sort(want)
		keys := jsonKeys(c.record)
		if !slices.Equal(keys, want) {
			t.Errorf("%T writes the keys %v, want the allowlist %v", c.record, keys, want)
		}
		for _, key := range keys {
			if slices.Contains(networkCreateAuditCredentialKeys, key) {
				t.Errorf("%T writes the credential key %q", c.record, key)
			}
		}
	}
}
