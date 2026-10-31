package model

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNetworkCreateRefusalStatusRequiresExplicitClassification(t *testing.T) {
	for _, test := range []struct {
		status int
		want   string
	}{
		{status: http.StatusBadRequest, want: "400 synthetic refusal"},
		{status: http.StatusConflict, want: "409 synthetic refusal"},
		{status: 0, want: "synthetic refusal"},
		{status: http.StatusOK, want: "synthetic refusal"},
		{status: http.StatusTooManyRequests, want: "synthetic refusal"},
		{status: http.StatusInternalServerError, want: "synthetic refusal"},
	} {
		resultError := &NetworkCreateResultError{Message: "synthetic refusal", refusalStatus: test.status}
		if got := resultError.Error(); got != test.want {
			t.Fatalf("status %d produced %q, want %q", test.status, got, test.want)
		}
		raw, err := json.Marshal(resultError)
		if err != nil || string(raw) != `{"message":"synthetic refusal"}` {
			t.Fatal("internal refusal classification changed the model JSON contract")
		}
	}
	// The same duplicate-looking wording is also used by an ambiguous SSO
	// helper failure. Text alone cannot authorize downgrading it to 409.
	ambiguous := &NetworkCreateResultError{Message: "Account might already exist. Please log in again."}
	if ambiguous.Error() != ambiguous.Message {
		t.Fatal("ambiguous helper failure acquired a client status")
	}
	var decoded NetworkCreateResultError
	if err := json.Unmarshal([]byte(`{"message":"synthetic refusal","refusalStatus":409}`), &decoded); err != nil || decoded.Error() != "synthetic refusal" {
		t.Fatal("JSON input was able to manufacture internal refusal classification")
	}
	legacy := &NetworkCreateResultError{Message: "400 invalid wallet challenge"}
	if legacy.Error() != legacy.Message {
		t.Fatal("existing explicit wallet status was changed or double-prefixed")
	}
}

func TestNetworkCreateAuthShapeRefusalsAreExplicitAndPrivate(t *testing.T) {
	userAuth := "shape-control@example.invalid"
	token := "synthetic-provider-token"
	emptyToken, whitespaceToken := "", " \t\n"
	password := "synthetic-password-not-a-secret"
	google, unsupported := string(AuthTypeGoogle), "synthetic-unsupported-provider"
	for _, test := range []struct {
		args    NetworkCreateArgs
		message string
	}{
		{args: NetworkCreateArgs{UserAuth: &userAuth}, message: "Password is required."},
		{args: NetworkCreateArgs{AuthJwt: &token}, message: "Authentication type is required."},
		{args: NetworkCreateArgs{AuthJwt: &emptyToken, AuthJwtType: &google}, message: "Authentication token is required."},
		{args: NetworkCreateArgs{AuthJwt: &whitespaceToken, AuthJwtType: &google}, message: "Authentication token is required."},
		{args: NetworkCreateArgs{AuthJwt: &token, AuthJwtType: &unsupported}, message: "Unsupported authentication type."},
		{args: NetworkCreateArgs{Password: &password}, message: "Email or phone number is required for password signup."},
		{args: NetworkCreateArgs{AuthJwtType: &google}, message: "Authentication token is required."},
	} {
		refusal := networkCreateAuthShapeError(test.args)
		if refusal == nil || refusal.refusalStatus != http.StatusBadRequest || refusal.Error() != "400 "+test.message {
			t.Fatal("explicit malformed auth shape did not receive its fixed 400 refusal")
		}
		raw, err := json.Marshal(refusal)
		if err != nil || string(raw) != `{"message":"`+test.message+`"}` {
			t.Fatal("auth shape refusal emitted input values or internal classification")
		}
	}
}

func TestNetworkCreateAuthShapePreservesProviderVerificationBoundary(t *testing.T) {
	userAuth, password := "shape-control@example.invalid", "synthetic-password-not-a-secret"
	token, emptyPassword, shortPassword := "synthetic-provider-token", "", "a"
	google, apple, unsupported := string(AuthTypeGoogle), string(AuthTypeApple), "synthetic-unsupported-provider"
	for _, args := range []NetworkCreateArgs{
		{}, // seedphrase creation has no password or provider-token requirement
		{UserAuth: &userAuth, Password: &password},
		{UserAuth: &userAuth, Password: &emptyPassword}, // do not introduce password policy here
		{UserAuth: &userAuth, Password: &shortPassword},
		{AuthJwt: &token, AuthJwtType: &google},
		{AuthJwt: &token, AuthJwtType: &apple},
		{UserAuth: &userAuth, Password: &password, AuthJwt: &token, AuthJwtType: &unsupported},
		{UserAuth: &userAuth, Password: &password, WalletAuth: &WalletAuthArgs{}},
		{AuthJwt: &token, AuthJwtType: &google, WalletAuth: &WalletAuthArgs{}},
		{AuthJwt: &token, WalletAuth: &WalletAuthArgs{}},
		{AuthJwtType: &google, WalletAuth: &WalletAuthArgs{}},
		{Password: &password, WalletAuth: &WalletAuthArgs{}},
	} {
		if networkCreateAuthShapeError(args) != nil {
			t.Fatal("shape validation changed branch precedence, password policy, or supported-provider verification")
		}
	}
	for _, message := range []string{"Could not verify signed token.", "synthetic provider unavailable", "synthetic key-fetch failure"} {
		unclassified := &NetworkCreateResultError{Message: message}
		if unclassified.Error() != message {
			t.Fatal("provider or verification error text acquired a client-refusal status")
		}
	}
}
