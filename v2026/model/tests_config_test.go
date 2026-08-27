package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func pushTestsAuthPolicy(t testing.TB, yaml string) {
	t.Helper()
	t.Cleanup(server.Vault.PushSimpleResource(testsVaultResourceName, []byte(yaml)))
}

func TestTestAuthPolicyExactIdentityAndFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		userAuth string
		bypass   bool
		suppress bool
	}{
		{
			name:     "exact domain",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\n  suppress_account_messages: true\n",
			userAuth: "Acceptance@SIGNUP-TEST.EXAMPLE",
			bypass:   true,
			suppress: true,
		},
		{
			name:     "configured trailing dot",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example.]\n",
			userAuth: "acceptance@signup-test.example",
			bypass:   true,
		},
		{
			name:     "subdomain does not match",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\n",
			userAuth: "acceptance@sub.signup-test.example",
		},
		{
			name:     "suffix attack does not match",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\n",
			userAuth: "acceptance@signup-test.example.attacker.invalid",
		},
		{
			name:     "exact normalized phone",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\n  suppress_account_messages: true\nsignup:\n  phone:\n    number: '+13125550100'\n",
			userAuth: "312-555-0100",
			bypass:   true,
			suppress: true,
		},
		{
			name:     "different phone does not match",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\nsignup:\n  phone:\n    number: '+13125550100'\n",
			userAuth: "+13125550101",
		},
		{
			name:     "wrong version fails closed",
			yaml:     "version: 2\nemail_verification:\n  bypass_domains: [signup-test.example]\n",
			userAuth: "acceptance@signup-test.example",
		},
		{
			name:     "malformed yaml fails closed",
			yaml:     "version: [\n",
			userAuth: "acceptance@signup-test.example",
		},
		{
			name:     "one malformed domain rejects whole policy",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example, '*.example']\n",
			userAuth: "acceptance@signup-test.example",
		},
		{
			name:     "malformed phone rejects whole policy",
			yaml:     "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\nsignup:\n  phone:\n    number: definitely-not-a-phone\n",
			userAuth: "acceptance@signup-test.example",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pushTestsAuthPolicy(t, test.yaml)
			policy := testAuthPolicyForUserAuth(&test.userAuth)
			if policy.BypassVerification != test.bypass {
				t.Fatalf("BypassVerification = %t, want %t", policy.BypassVerification, test.bypass)
			}
			if policy.SuppressAccountMessages != test.suppress {
				t.Fatalf("SuppressAccountMessages = %t, want %t", policy.SuppressAccountMessages, test.suppress)
			}
		})
	}
}

func TestNetworkCreateTestAuthIsImmediatelyVerified(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pushTestsAuthPolicy(t, "version: 1\nemail_verification:\n  bypass_domains: [signup-test.example]\n  suppress_account_messages: true\nsignup:\n  phone:\n    number: '+13125550100'\n")

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		email := "acceptance@signup-test.example"
		password := "Acceptance-password-123!"
		result, err := NetworkCreate(NetworkCreateArgs{
			UserAuth:    &email,
			Password:    &password,
			NetworkName: "acceptance-email-bypass",
			Terms:       true,
		}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.VerificationRequired != nil {
			t.Fatal("test-domain signup unexpectedly required verification")
		}
		if result.Network == nil || result.Network.ByJwt == nil || *result.Network.ByJwt == "" {
			t.Fatal("test-domain signup did not return a JWT")
		}
		if !result.SuppressAccountMessages {
			t.Fatal("test-domain signup did not retain message-suppression policy")
		}

		login, err := AuthLoginWithPassword(AuthLoginWithPasswordArgs{
			UserAuth: email,
			Password: password,
		}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if login.Network == nil || login.Network.ByJwt == nil || *login.Network.ByJwt == "" {
			t.Fatalf("verified test-domain account could not log in: %#v", login)
		}

		phone := "+13125550100"
		phoneResult, err := NetworkCreate(NetworkCreateArgs{
			UserAuth:    &phone,
			Password:    &password,
			NetworkName: "acceptance-phone-bypass",
			Terms:       true,
		}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if phoneResult.VerificationRequired != nil {
			t.Fatal("configured phone signup unexpectedly required verification")
		}
		if phoneResult.Network == nil || phoneResult.Network.ByJwt == nil || *phoneResult.Network.ByJwt == "" {
			t.Fatal("configured phone signup did not return a JWT")
		}
		if !phoneResult.SuppressAccountMessages {
			t.Fatal("configured phone signup did not suppress account messages")
		}

		// Simulate a fixture left pending by the previous server version. Correct
		// password login must repair it without sending or requesting a code.
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(
				ctx,
				"UPDATE network_user_auth_password SET verified = false WHERE user_auth = $1",
				phone,
			))
		})
		phoneLogin, err := AuthLoginWithPassword(AuthLoginWithPasswordArgs{
			UserAuth: phone,
			Password: password,
		}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if phoneLogin.VerificationRequired != nil {
			t.Fatal("configured phone login unexpectedly required verification")
		}
		if phoneLogin.Network == nil || phoneLogin.Network.ByJwt == nil || *phoneLogin.Network.ByJwt == "" {
			t.Fatalf("configured phone could not log in: %#v", phoneLogin)
		}

		ordinaryEmail := "acceptance@ordinary.example"
		ordinary, err := NetworkCreate(NetworkCreateArgs{
			UserAuth:    &ordinaryEmail,
			Password:    &password,
			NetworkName: "acceptance-normal-email",
			Terms:       true,
		}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if ordinary.VerificationRequired == nil {
			t.Fatal("ordinary email signup unexpectedly bypassed verification")
		}
		if ordinary.Network == nil || ordinary.Network.ByJwt != nil {
			t.Fatal("ordinary unverified signup returned an authenticated network")
		}
	})
}
