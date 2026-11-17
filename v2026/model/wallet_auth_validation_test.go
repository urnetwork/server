package model

import (
	"regexp"
	"strings"
	"testing"
)

// validateWalletAuth is shared by both wallet entry points -- addWalletAuth
// (POST /auth/add-auth) and networkCreateWalletAuth (POST /auth/network-create)
// -- and every refusal it returns is client input, never a server fault.
//
// It is the one piece of this surface that needs no database (it calls only
// ParseBlockchain, the two address decoders and VerifySignature), so this
// matrix is a PURE UNIT test: no docker stack, no WARP_ENV.
//
// Two halves are pinned, and they are pinned together on purpose:
//
//  1. The message each refusal carries, INCLUDING its "<code> " prefix. The
//     prefix is what makes /auth/network-create answer 400/401 instead of the
//     500-with-raw-text it used to; without an assertion on the exact status a
//     future edit can silently downgrade "this signature did not verify" to
//     "the server broke". The vocabulary deliberately matches
//     UseWalletAuthChallenge's ("400 invalid wallet address",
//     "400 invalid signature encoding", "401 invalid signature") so one
//     condition cannot be two different statuses depending on which endpoint
//     the client reached.
//
//  2. That PeelStatusPrefix removes exactly that prefix and nothing else.
//     /auth/add-auth answers HTTP 200 with a spec'd error object, so the same
//     messages travel into a body there; the digits must not reach the user.
//
// Pinning only (1) lets the peel break silently; pinning only (2) lets the
// status break silently.
func TestValidateWalletAuthMatrix(t *testing.T) {
	statusPrefix := regexp.MustCompile(`^[45][0-9]{2}\s`)

	solanaSigner := newSolanaAcceptanceWalletSigner(t)
	bittensorSigner := newBittensorAcceptanceWalletSigner(t)
	// a second keypair per chain, to sign for the wrong identity
	otherSolana := newSolanaAcceptanceWalletSigner(t)
	otherBittensor := newBittensorAcceptanceWalletSigner(t)

	// validateWalletAuth verifies a signature over whatever text it is given;
	// binding that text to a server-issued single-use challenge is
	// UseWalletAuthChallenge's job, exercised separately. Any message works
	// here, which is what keeps this test database-free.
	const message = "urnetwork wallet auth validation matrix"

	sign := func(signer acceptanceWalletSigner, text string) string {
		signature, err := signer.sign(text)
		if err != nil {
			t.Fatal(err)
		}
		return signature
	}

	chains := []struct {
		name string
		// the signer whose address and signature are valid
		signer acceptanceWalletSigner
		// a different keypair on the same chain
		other acceptanceWalletSigner
		// a correctly-formed address that belongs to the OTHER chain
		foreignAddress string
		// a signature string this chain's verifier cannot decode
		undecodableSignature string
	}{
		{
			name:                 "solana",
			signer:               solanaSigner,
			other:                otherSolana,
			foreignAddress:       bittensorSigner.address,
			undecodableSignature: "!!! not base64 !!!",
		},
		{
			name:                 "bittensor",
			signer:               bittensorSigner,
			other:                otherBittensor,
			foreignAddress:       solanaSigner.address,
			undecodableSignature: "zz-not-hex-zz",
		},
	}

	for _, chain := range chains {
		t.Run(chain.name, func(t *testing.T) {
			signer := chain.signer

			// each case is evaluated against a freshly-built args value,
			// because validateWalletAuth canonicalises Blockchain in place
			args := func(mutate func(*WalletAuthArgs)) *WalletAuthArgs {
				a := &WalletAuthArgs{
					PublicKey:  signer.address,
					Signature:  sign(signer, message),
					Message:    message,
					Blockchain: signer.blockchain,
				}
				if mutate != nil {
					mutate(a)
				}
				return a
			}

			tests := []struct {
				name string
				args *WalletAuthArgs
				// "" means the call must succeed
				wantMessage string
			}{
				{
					name:        "nil args",
					args:        nil,
					wantMessage: "400 wallet auth is required",
				},
				{
					name:        "empty signature",
					args:        args(func(a *WalletAuthArgs) { a.Signature = "" }),
					wantMessage: "400 wallet signature and message are required",
				},
				{
					name:        "empty message",
					args:        args(func(a *WalletAuthArgs) { a.Message = "" }),
					wantMessage: "400 wallet signature and message are required",
				},
				{
					// a chain the server knows but wallet auth does not accept
					name:        "ethereum",
					args:        args(func(a *WalletAuthArgs) { a.Blockchain = "ETHEREUM" }),
					wantMessage: "400 wallet auth only supports solana and bittensor",
				},
				{
					name:        "matic",
					args:        args(func(a *WalletAuthArgs) { a.Blockchain = "MATIC" }),
					wantMessage: "400 wallet auth only supports solana and bittensor",
				},
				{
					// ParseBlockchain's own refusal, which used to travel
					// unprefixed and therefore as a 500
					name:        "unparseable blockchain",
					args:        args(func(a *WalletAuthArgs) { a.Blockchain = "dogecoin" }),
					wantMessage: "400 invalid Blockchain: DOGECOIN",
				},
				{
					name:        "malformed address",
					args:        args(func(a *WalletAuthArgs) { a.PublicKey = "not-an-address" }),
					wantMessage: "400 invalid wallet address",
				},
				{
					// the address gate must be chain-aware: a perfectly valid
					// address string for the other chain is still not an
					// address on this one
					name:        "well formed address of the wrong chain",
					args:        args(func(a *WalletAuthArgs) { a.PublicKey = chain.foreignAddress }),
					wantMessage: "400 invalid wallet address",
				},
				{
					name:        "undecodable signature",
					args:        args(func(a *WalletAuthArgs) { a.Signature = chain.undecodableSignature }),
					wantMessage: "400 invalid signature encoding",
				},
				{
					// decodes cleanly, signed by a different key: the one case
					// that is 401 rather than 400
					name:        "signature from the wrong signer",
					args:        args(func(a *WalletAuthArgs) { a.Signature = sign(chain.other, message) }),
					wantMessage: "401 invalid signature",
				},
				{
					name:        "signature over different text",
					args:        args(func(a *WalletAuthArgs) { a.Signature = sign(signer, message+" tampered") }),
					wantMessage: "401 invalid signature",
				},
				{
					name:        "valid",
					args:        args(nil),
					wantMessage: "",
				},
			}

			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					err := validateWalletAuth(test.args)
					if test.wantMessage == "" {
						if err != nil {
							t.Fatalf("validateWalletAuth = %q, want success", err)
						}
						return
					}
					if err == nil {
						t.Fatalf("validateWalletAuth accepted %s", test.name)
					}
					if err.Error() != test.wantMessage {
						t.Fatalf("validateWalletAuth = %q, want %q", err, test.wantMessage)
					}
					// the message a person actually reads, once the status has
					// been spent on the status line
					peeled := PeelStatusPrefix(err.Error())
					if statusPrefix.MatchString(peeled) {
						t.Fatalf("peeled message still carries a status prefix: %q", peeled)
					}
					if want := test.wantMessage[4:]; peeled != want {
						t.Fatalf("PeelStatusPrefix(%q) = %q, want %q", err, peeled, want)
					}
				})
			}
		})
	}
}

// An absent blockchain has always meant Solana on this endpoint, and
// validateWalletAuth canonicalises it in place so every later comparison --
// the byte-exact uniqueness checks, filterWalletAuthsByBlockchain -- sees
// "SOL" rather than "". Pinned because the default is invisible at the call
// site and a Bittensor caller that forgets the field would otherwise be
// silently verified against the wrong curve.
//
// PURE UNIT: no database, no WARP_ENV.
func TestValidateWalletAuthDefaultsEmptyBlockchainToSolana(t *testing.T) {
	signer := newSolanaAcceptanceWalletSigner(t)
	const message = "urnetwork empty blockchain default"
	signature, err := signer.sign(message)
	if err != nil {
		t.Fatal(err)
	}
	args := &WalletAuthArgs{
		PublicKey:  signer.address,
		Signature:  signature,
		Message:    message,
		Blockchain: "",
	}
	if err := validateWalletAuth(args); err != nil {
		t.Fatalf("validateWalletAuth = %q, want success", err)
	}
	if args.Blockchain != SOL.String() {
		t.Fatalf("blockchain = %q, want the canonical %q", args.Blockchain, SOL.String())
	}

	// the same omission from a Bittensor wallet must NOT quietly succeed:
	// defaulting to Solana means the ss58 address is not a valid address here
	bittensor := newBittensorAcceptanceWalletSigner(t)
	bittensorSignature, err := bittensor.sign(message)
	if err != nil {
		t.Fatal(err)
	}
	err = validateWalletAuth(&WalletAuthArgs{
		PublicKey:  bittensor.address,
		Signature:  bittensorSignature,
		Message:    message,
		Blockchain: "",
	})
	if err == nil {
		t.Fatal("a Bittensor wallet with no blockchain field was accepted as Solana")
	}
	if !strings.HasPrefix(err.Error(), "400 invalid wallet address") {
		t.Fatalf("error = %q, want 400 invalid wallet address", err)
	}
}
