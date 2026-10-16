package model

// A wallet signature made with another account than the address entered
// (TAO.com manual entry lets the user pick the signing account in the wallet)
// is refused by wallet sign-in, network create and add-auth with the stable
// code `signature_mismatch` next to a plain message, so the apps can say what
// went wrong in the user's language. Every other wallet refusal keeps its
// message and has no code. The cases run over both wallet chains
// (walletChains): the code means the same for a Solana signature.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// the coded refusal's wire form, as the apps read it
const walletAuthSignatureMismatchJson = `"error":{"code":"signature_mismatch","message":"The signature does not match this wallet address. Sign the challenge with this address."}`

// A signature over the challenge by a second signer of the same chain.
func signedByAnotherAccount(t testing.TB, chain walletChainCase, message string) string {
	t.Helper()
	signature, err := chain.newSigner(t).sign(message)
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

// A unique network name for one create; a reused name is refused before the
// wallet is checked.
func signatureMismatchNetworkName(label string) string {
	return fmt.Sprintf("%s-%s", label, strings.ToLower(server.NewId().String()[:8]))
}

// Sign-in answers another account's signature with the code instead of an
// error. Malformed input and a wrong challenge keep their errors. None of these
// reaches the database: the signature and timestamp gates come first.
func TestHandleLoginWalletCodesSignatureMismatch(t *testing.T) {
	ctx := context.Background()
	message := FormatWalletAuthChallengeMessage("bG9naW4tbWlzbWF0Y2g=", server.NowUtc().Unix())
	for _, chain := range walletChains {
		typed := chain.newSigner(t)
		walletAuth := func(message string, signature string) *WalletAuthArgs {
			return &WalletAuthArgs{
				PublicKey:  typed.address,
				Signature:  signature,
				Message:    message,
				Blockchain: chain.chainName,
			}
		}

		result, err := handleLoginWallet(walletAuth(message, signedByAnotherAccount(t, chain, message)), ctx)
		if err != nil {
			t.Fatalf("%s: another account's signature = %v, want a coded result", chain.name, err)
		}
		if result == nil || result.Error == nil || result.Network != nil || result.WalletAuth != nil {
			t.Fatalf("%s: another account's signature = %+v, want only a coded error", chain.name, result)
		}
		connect.AssertEqual(t, result.Error.Code, WalletAuthErrorCodeSignatureMismatch)
		connect.AssertEqual(t, result.Error.Code, "signature_mismatch")
		raw, err := json.Marshal(result)
		connect.AssertEqual(t, err, nil)
		if !strings.Contains(string(raw), walletAuthSignatureMismatchJson) {
			t.Fatalf("%s: wire form %s", chain.name, raw)
		}

		otherTextSignature, err := typed.sign("Sign in to URnetwork")
		connect.AssertEqual(t, err, nil)
		uncoded := []struct {
			name       string
			walletAuth *WalletAuthArgs
			want       string
		}{
			{name: "undecodable", walletAuth: walletAuth(message, "zzzz"), want: "400 invalid signature encoding"},
			{name: "not a challenge", walletAuth: walletAuth("Sign in to URnetwork", otherTextSignature), want: "400 invalid message format"},
		}
		for _, test := range uncoded {
			result, err := handleLoginWallet(test.walletAuth, ctx)
			if result != nil || err == nil || err.Error() != test.want {
				t.Fatalf("%s %s: = %+v, %v, want the error %q", chain.name, test.name, result, err, test.want)
			}
		}
	}
}

// add-auth checks the signature before it uses the challenge
// (validateWalletAuth). That check marks the mismatch for AddAuth and keeps
// its long-standing text for every other caller.
func TestValidateWalletAuthMarksSignatureMismatch(t *testing.T) {
	message := FormatWalletAuthChallengeMessage("dmFsaWRhdGUtbWlzbWF0Y2g=", server.NowUtc().Unix())
	for _, chain := range walletChains {
		typed := chain.newSigner(t)
		typedSignature, err := typed.sign(message)
		connect.AssertEqual(t, err, nil)
		otherTextSignature, err := typed.sign(message + " other text")
		connect.AssertEqual(t, err, nil)
		cases := []struct {
			name         string
			signature    string
			wantMismatch bool
			want         string
		}{
			{name: "another account", signature: signedByAnotherAccount(t, chain, message), wantMismatch: true, want: "401 invalid signature"},
			{name: "the typed account over other text", signature: otherTextSignature, wantMismatch: true, want: "401 invalid signature"},
			{name: "undecodable", signature: "zzzz", want: "400 invalid signature encoding"},
			{name: "valid", signature: typedSignature},
		}
		for _, test := range cases {
			err := validateWalletAuth(&WalletAuthArgs{
				PublicKey:  typed.address,
				Signature:  test.signature,
				Message:    message,
				Blockchain: chain.chainName,
			})
			if errors.Is(err, errWalletSignatureMismatch) != test.wantMismatch {
				t.Fatalf("%s %s: mismatch = %v, want %v (%v)", chain.name, test.name, errors.Is(err, errWalletSignatureMismatch), test.wantMismatch, err)
			}
			if test.want == "" {
				connect.AssertEqual(t, err, nil)
			} else if err == nil || err.Error() != test.want {
				t.Fatalf("%s %s: = %v, want %q", chain.name, test.name, err, test.want)
			}
		}
	}
}

// On a challenge issued for the entered address, another account's signature is
// coded and leaves the challenge unused: the entered account's own signature
// over the same challenge then signs in (a wallet with no network yet comes
// back to create one).
func TestWalletSignInSignatureMismatchLeavesTheChallenge(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, chain := range walletChains {
			typed := chain.newSigner(t)
			walletAuth := signedAcceptanceWalletChallenge(t, ctx, typed)
			typedSignature := walletAuth.Signature

			walletAuth.Signature = signedByAnotherAccount(t, chain, walletAuth.Message)
			mismatch, err := handleLoginWallet(walletAuth, ctx)
			if err != nil || mismatch == nil || mismatch.Error == nil {
				t.Fatalf("%s: another account's signature = %+v, %v, want a coded result", chain.name, mismatch, err)
			}
			connect.AssertEqual(t, mismatch.Error.Code, WalletAuthErrorCodeSignatureMismatch)

			walletAuth.Signature = typedSignature
			login, err := handleLoginWallet(walletAuth, ctx)
			if err != nil {
				t.Fatalf("%s: the entered account's signature after a mismatch = %v", chain.name, err)
			}
			if login.Error != nil || login.WalletAuth == nil {
				t.Fatalf("%s: the entered account's signature after a mismatch = %+v, want the new wallet", chain.name, login)
			}
		}
	})
}

// Network create codes the same refusal. A client without `result_errors`
// still gets an HTTP 401 (the result error's status), now with the plain
// message. The challenge stays unused, so the entered account's signature over
// it then creates the network.
func TestNetworkCreateWalletSignatureMismatchIsCoded(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, chain := range walletChains {
			clientSession := session.Testing_CreateClientSession(ctx, nil)
			typed := chain.newSigner(t)
			walletAuth := signedAcceptanceWalletChallenge(t, ctx, typed)
			typedSignature := walletAuth.Signature
			args := NetworkCreateArgs{
				NetworkName: signatureMismatchNetworkName("mismatch-" + chain.name),
				Terms:       true,
				WalletAuth:  walletAuth,
			}

			walletAuth.Signature = signedByAnotherAccount(t, chain, walletAuth.Message)
			mismatch, err := NetworkCreate(args, clientSession)
			if err != nil || mismatch == nil || mismatch.Error == nil || mismatch.Network != nil {
				t.Fatalf("%s: another account's signature = %+v, %v, want a coded result", chain.name, mismatch, err)
			}
			connect.AssertEqual(t, mismatch.Error.Code, WalletAuthErrorCodeSignatureMismatch)
			connect.AssertEqual(t, mismatch.Error.Error(), "401 "+walletAuthSignatureMismatchMessage)
			raw, err := json.Marshal(mismatch)
			connect.AssertEqual(t, err, nil)
			if !strings.Contains(string(raw), walletAuthSignatureMismatchJson) {
				t.Fatalf("%s: wire form %s", chain.name, raw)
			}

			walletAuth.Signature = typedSignature
			created, err := NetworkCreate(args, clientSession)
			if err != nil {
				t.Fatalf("%s: the entered account's signature after a mismatch = %v", chain.name, err)
			}
			if created.Error != nil || created.Network == nil || created.Network.ByJwt == nil {
				t.Fatalf("%s: the entered account's signature after a mismatch = %+v, want the network", chain.name, created)
			}
		}
	})
}

// add-auth answers in its result as always, now with the code and a plain
// message instead of "invalid signature". Nothing is bound, and the entered
// account's signature over the same challenge then adds the wallet.
func TestAddWalletAuthSignatureMismatchIsCoded(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, chain := range walletChains {
			userId, _, clientSession := authParityAccount(t, ctx, "mismatch-"+chain.name)
			typed := chain.newSigner(t)
			walletAuth := signedAcceptanceWalletChallenge(t, ctx, typed)
			typedSignature := walletAuth.Signature

			walletAuth.Signature = signedByAnotherAccount(t, chain, walletAuth.Message)
			mismatch, err := AddAuth(AddAuthMethod{WalletAuth: walletAuth}, clientSession)
			if err != nil || mismatch == nil || mismatch.Error == nil {
				t.Fatalf("%s: another account's signature = %+v, %v, want a coded result", chain.name, mismatch, err)
			}
			connect.AssertEqual(t, mismatch.Error.Code, WalletAuthErrorCodeSignatureMismatch)
			connect.AssertEqual(t, mismatch.Error.Message, walletAuthSignatureMismatchMessage)
			assertNoStatusPrefix(t, chain.name+" signature mismatch", mismatch.Error.Message)
			if walletRowCount(t, ctx, userId) != 0 {
				t.Fatalf("%s: a refused signature bound the wallet", chain.name)
			}

			walletAuth.Signature = typedSignature
			added, err := AddAuth(AddAuthMethod{WalletAuth: walletAuth}, clientSession)
			if err != nil || added.Error != nil {
				t.Fatalf("%s: the entered account's signature after a mismatch = %+v, %v", chain.name, added, err)
			}
			if walletRowCount(t, ctx, userId) != 1 {
				t.Fatalf("%s: the entered account's signature did not bind the wallet", chain.name)
			}
		}
	})
}
