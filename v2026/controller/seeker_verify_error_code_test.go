package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/server/v2026/session"
)

// Seeker verification answers with a stable error code, so the apps can pick a
// localized message instead of showing the English server message.
//
// Root cause: a wallet with no Seeker or Saga token was answered only with
// the English message "Wallet is not a holder of the Seeker or Saga Genesis
// tokens", with nothing machine-readable for the apps to localize on.
//
// Fixed key seed, recorded Helius fixtures and an injected holder mark: no
// network, database or randomness.

type seekerVerifyTest struct {
	verify    *VerifySeekerNftHolderArgs
	markCount int
}

func newSeekerVerifyTest(t *testing.T) *seekerVerifyTest {
	privateKey := ed25519.NewKeyFromSeed([]byte("seeker-verify-error-code-seed-32"))
	publicKey := solana.PublicKeyFromBytes(privateKey.Public().(ed25519.PublicKey))
	message := "Verify Seeker token holder"
	return &seekerVerifyTest{
		verify: &VerifySeekerNftHolderArgs{
			PublicKey: publicKey.String(),
			Message:   message,
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(message))),
		},
	}
}

func (self *seekerVerifyTest) run(t *testing.T, seekerFixture string, sagaFixture string) *VerifySeekerNftHolderResult {
	loadAssets := func(name string) []HeliusAsset {
		assetBytes, err := os.ReadFile(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var assets []HeliusAsset
		if err := json.Unmarshal(assetBytes, &assets); err != nil {
			t.Fatal(err)
		}
		return assets
	}
	result, err := verifySeekerNftHolder(
		self.verify,
		session.Testing_CreateClientSession(context.Background(), nil),
		seekerHolderLookup{
			searchAssets: func(context.Context, string) ([]HeliusAsset, error) {
				return loadAssets(seekerFixture), nil
			},
			searchSagaAssets: func(context.Context, string) ([]HeliusAsset, error) {
				return loadAssets(sagaFixture), nil
			},
			markHolder: func(string, *session.ClientSession) error {
				self.markCount += 1
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	return result
}

func TestVerifySeekerNftHolderNotHolderCode(t *testing.T) {
	test := newSeekerVerifyTest(t)
	result := test.run(t, "non_holder", "non_holder")
	body, _ := json.Marshal(result)
	if result.Success || result.Error == nil {
		t.Fatalf("non-holder verified: %s", body)
	}
	if !strings.Contains(string(body), `"code":"seeker_token_not_found"`) {
		t.Fatalf("non-holder response has no seeker_token_not_found code: %s", body)
	}
	// older clients still read the message
	if result.Error.Message == "" {
		t.Fatalf("message dropped: %s", body)
	}
	if test.markCount != 0 {
		t.Fatalf("non-holder marked")
	}
}

func TestVerifySeekerNftHolderInvalidSignatureCode(t *testing.T) {
	test := newSeekerVerifyTest(t)
	test.verify.Message = "a different message"
	result := test.run(t, "seeker_genesis_holder", "non_holder")
	body, _ := json.Marshal(result)
	if !strings.Contains(string(body), `"code":"seeker_invalid_signature"`) {
		t.Fatalf("bad signature response has no seeker_invalid_signature code: %s", body)
	}
	if test.markCount != 0 {
		t.Fatalf("bad signature marked")
	}
}

func TestVerifySeekerNftHolderHolder(t *testing.T) {
	test := newSeekerVerifyTest(t)
	result := test.run(t, "seeker_genesis_holder", "non_holder")
	body, _ := json.Marshal(result)
	if string(body) != `{"success":true}` || test.markCount != 1 {
		t.Fatalf("holder = %s, mark count = %d", body, test.markCount)
	}
}
