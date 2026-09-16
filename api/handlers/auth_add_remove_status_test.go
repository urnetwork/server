package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/gagliardetto/solana-go"
	"github.com/mr-tron/base58"
	"golang.org/x/crypto/blake2b"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Wire-level behaviour of /auth/add-auth and /auth/remove-auth.
//
// The model-layer tests (model/wallet_add_remove_auth_test.go) cannot see any
// of this: they assert the RESULT VALUE, and every defect here is in the
// translation from that value to an HTTP response -- a typed nil marshalled as
// `null` with a 200, a status prefix that should have been spent on the status
// line rendering in a body instead. A test that only looks at the model is
// structurally blind to it.
//
// INTEGRATION: needs the local postgres/redis stack (WARP_ENV=local).

// statusPrefixPattern is the "<code> " RaiseHttpError peels into an HTTP
// status. These two endpoints answer 200 with a structured error object, so
// anything matching this in error.message reached the user as literal digits.
var statusPrefixPattern = regexp.MustCompile(`^[45][0-9]{2}\s`)

// authedRequest builds a request carrying a real signed by-JWT for userId.
//
// There was no shared authenticated-request helper in this package; /auth/
// add-auth and /auth/remove-auth go through WrapWithInputRequireAuth, so no
// handler-layer test of them was possible without one. The JWT shape is the
// one already used by provider_client_verdict_handlers_test.go.
func authedRequest(
	t testing.TB,
	method string,
	path string,
	networkId server.Id,
	userId server.Id,
	networkName string,
	body any,
) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %s", err)
	}
	return authedRequestRaw(t, method, path, networkId, userId, networkName, buf)
}

// authedRequestRaw is the same thing for a body that must go out byte for byte
// (an empty object, a deliberately malformed one).
func authedRequestRaw(
	t testing.TB,
	method string,
	path string,
	networkId server.Id,
	userId server.Id,
	networkName string,
	body []byte,
) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = statusTestClientAddress
	req.Header.Set("Content-Type", "application/json")
	byJwt := jwt.NewByJwt(networkId, userId, networkName, false, false)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", byJwt.Sign()))
	return req
}

// handlerTestAccount creates a network whose user has one email auth, and
// returns everything needed to sign requests for it. Names are unique per
// account: the network-name-taken refusal sits above most branches under test.
func handlerTestAccount(
	t testing.TB,
	ctx context.Context,
	label string,
) (networkId server.Id, userId server.Id, networkName string) {
	t.Helper()
	networkId = server.NewId()
	userId = server.NewId()
	networkName = fmt.Sprintf("%s-%s", label, strings.ToLower(networkId.String()[:8]))
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return
}

// --- signers --------------------------------------------------------------
//
// model's acceptance signers are unexported test helpers, so this package
// carries its own. They produce the same real signatures: ed25519 over the raw
// message for Solana, sr25519 over the <Bytes>-wrapped message for Bittensor.

type handlerWalletSigner struct {
	blockchain string
	address    string
	sign       func(string) (string, error)
}

func newHandlerSolanaSigner(t testing.TB) handlerWalletSigner {
	t.Helper()
	privateKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return handlerWalletSigner{
		blockchain: "SOL",
		address:    privateKey.PublicKey().String(),
		sign: func(message string) (string, error) {
			signature, err := privateKey.Sign([]byte(message))
			if err != nil {
				return "", err
			}
			return base64.StdEncoding.EncodeToString(signature[:]), nil
		},
	}
}

func newHandlerBittensorSigner(t testing.TB) handlerWalletSigner {
	t.Helper()
	secretKey, publicKey, err := schnorrkel.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	encoded := publicKey.Encode()
	// ss58, substrate generic prefix 42 (what bittensor uses)
	data := append([]byte{42}, encoded[:]...)
	hasher, err := blake2b.New512(nil)
	if err != nil {
		t.Fatal(err)
	}
	hasher.Write([]byte("SS58PRE"))
	hasher.Write(data)
	checksum := hasher.Sum(nil)
	return handlerWalletSigner{
		blockchain: "TAO",
		address:    base58.Encode(append(data, checksum[:2]...)),
		sign: func(message string) (string, error) {
			transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte("<Bytes>"+message+"</Bytes>"))
			signature, err := secretKey.Sign(transcript)
			if err != nil {
				return "", err
			}
			raw := signature.Encode()
			return hex.EncodeToString(raw[:]), nil
		},
	}
}

var handlerWalletChains = []struct {
	name      string
	newSigner func(testing.TB) handlerWalletSigner
}{
	{"solana", newHandlerSolanaSigner},
	{"bittensor", newHandlerBittensorSigner},
}

// freshWalletAuth issues a real single-use challenge and signs it.
func freshWalletAuth(
	t testing.TB,
	ctx context.Context,
	signer handlerWalletSigner,
) *model.WalletAuthArgs {
	t.Helper()
	address := signer.address
	blockchain := signer.blockchain
	challenge := model.CreateWalletAuthChallenge(model.WalletAuthChallengeArgs{
		WalletAddress: &address,
		Blockchain:    &blockchain,
	}, ctx)
	if challenge.Error != nil {
		t.Fatalf("create challenge: %s", challenge.Error.Message)
	}
	signature, err := signer.sign(challenge.MessageTemplate)
	if err != nil {
		t.Fatal(err)
	}
	return &model.WalletAuthArgs{
		PublicKey:  signer.address,
		Signature:  signature,
		Message:    challenge.MessageTemplate,
		Blockchain: signer.blockchain,
	}
}

// --- B1 -------------------------------------------------------------------

// WITNESS. Fails on the base branch.
//
// AddAuth returned (nil, nil) for a body naming no auth method. The router
// sees a nil error, so it takes the success path and marshals the typed-nil
// *AddAuthMethodResult -- writeJsonResponse has no nil guard, so the client
// gets HTTP 200, Content-Type application/json, body `null`.
//
// This is the ONLY layer that can see it. The model-layer half
// (TestAddAuthWithNoRecognizedMethodIsRefused) pins the result value; the
// literal `null` on the wire only exists here.
func TestAddAuthEmptyBodyIsNotReportedAsSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId, networkName := handlerTestAccount(t, ctx, "empty-body")

		bodies := map[string]string{
			"empty object":               `{}`,
			"user_auth with no password": `{"user_auth":"someone@example.com"}`,
			"auth_jwt with no type":      `{"auth_jwt":"not-a-real-token"}`,
		}
		for name, body := range bodies {
			w := httptest.NewRecorder()
			AuthAdd(w, authedRequestRaw(t, http.MethodPost, "/auth/add-auth",
				networkId, userId, networkName, []byte(body)))

			raw := strings.TrimSpace(w.Body.String())
			if raw == "null" {
				t.Fatalf(
					"%s: /auth/add-auth answered HTTP %d with the body `null`. A "+
						"status-checking client reads 200 as 'the method was added', and "+
						"there is no error field to read instead",
					name, w.Code,
				)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("%s: HTTP %d (%q); this endpoint answers 200 with a structured error",
					name, w.Code, raw)
			}
			var result controller.AddAuthResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatalf("%s: body %q is not an AddAuthResult: %v", name, raw, err)
			}
			if result.Error == nil || result.Error.Message == "" {
				t.Fatalf("%s: body %q carries no error message", name, raw)
			}
			assertNoStatusPrefixHTTP(t, name, result.Error.Message)
		}
	})
}

func assertNoStatusPrefixHTTP(t testing.TB, what string, message string) {
	t.Helper()
	if statusPrefixPattern.MatchString(message) {
		t.Fatalf(
			"%s: error.message = %q -- the transport's status prefix reached the "+
				"message a person reads",
			what, message,
		)
	}
}

// --- B2 -------------------------------------------------------------------

// The status prefix must not survive into the body, over the real wire, for
// either chain.
//
// This is the case the fix is built around: validateWalletAuth and the
// challenge layer are SHARED with /auth/network-create, which needs those
// prefixes to answer 400 vs 401 vs 409. Making them emit clean text instead
// would fix this endpoint by breaking that one. The peel at this boundary is
// what lets both be right.
func TestAddAuthErrorMessageCarriesNoStatusPrefix(t *testing.T) {
	for _, chain := range handlerWalletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				networkId, userId, networkName := handlerTestAccount(t, ctx, "prefix")
				signer := chain.newSigner(t)

				walletAuth := freshWalletAuth(t, ctx, signer)
				body := map[string]any{"wallet_auth": walletAuth}

				// consume the challenge
				w := httptest.NewRecorder()
				AuthAdd(w, authedRequest(t, http.MethodPost, "/auth/add-auth",
					networkId, userId, networkName, body))
				if w.Code != http.StatusOK {
					t.Fatalf("first add: HTTP %d (%q)", w.Code, strings.TrimSpace(w.Body.String()))
				}

				// replay it
				w = httptest.NewRecorder()
				AuthAdd(w, authedRequest(t, http.MethodPost, "/auth/add-auth",
					networkId, userId, networkName, body))
				if w.Code != http.StatusOK {
					t.Fatalf("replay: HTTP %d (%q), want 200 with a structured error",
						w.Code, strings.TrimSpace(w.Body.String()))
				}
				var result controller.AddAuthResult
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatalf("replay body %q is not an AddAuthResult: %v",
						strings.TrimSpace(w.Body.String()), err)
				}
				if result.Error == nil {
					t.Fatal("a replayed challenge was accepted")
				}
				assertNoStatusPrefixHTTP(t, "replayed challenge", result.Error.Message)
				if !strings.Contains(result.Error.Message, "challenge already used") {
					t.Fatalf("replay refusal = %q", result.Error.Message)
				}
			})
		})
	}
}

// --- B3 -------------------------------------------------------------------

// WIRE CONTRACT PIN (passes on the base branch).
//
// AddAuthResult.error and RemoveAuthResult.error are spec'd objects
// (connect/api/bringyour.yml). Converting these refusals to 4xx + text/plain
// would make those fields unreachable -- which is precisely the defect being
// fixed on /auth/network-create, in reverse. This pins the shape so the
// remove-side work cannot drift into it.
//
// The rate-limit refusal is the one deliberate exception and is NOT exercised
// here, so the two cannot contradict each other.
func TestRemoveAuthRefusalKeepsTheSpecdBodyShape(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId, networkName := handlerTestAccount(t, ctx, "last-method")

		// the account has only the seeded email
		w := httptest.NewRecorder()
		AuthRemove(w, authedRequest(t, http.MethodPost, "/auth/remove-auth",
			networkId, userId, networkName, map[string]any{"auth_type": "email"}))

		if w.Code != http.StatusOK {
			t.Fatalf(
				"a last-method removal answered HTTP %d (%q). RemoveAuthResult.error is a "+
					"spec'd object; a 4xx with text/plain makes it unreachable",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		var result controller.RemoveAuthResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("body %q is not a RemoveAuthResult: %v", strings.TrimSpace(w.Body.String()), err)
		}
		if result.Error == nil || result.Error.Message == "" {
			t.Fatalf("body %q carries no error message", strings.TrimSpace(w.Body.String()))
		}
		if !strings.Contains(result.Error.Message, "cannot remove your last auth method") {
			t.Fatalf("refusal = %q", result.Error.Message)
		}
		assertNoStatusPrefixHTTP(t, "last method", result.Error.Message)
	})
}

// A refused removal over the wire must also not leak the prefix the model uses
// internally: the not-bound guard's message is built as "400 ..." so that the
// same string would render correctly if this endpoint ever did answer with a
// status. If the peel were removed, this is what a user would see.
func TestRemoveAuthNotBoundRefusalCarriesNoStatusPrefix(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId, networkName := handlerTestAccount(t, ctx, "not-bound")
		signer := newHandlerBittensorSigner(t)

		w := httptest.NewRecorder()
		AuthAdd(w, authedRequest(t, http.MethodPost, "/auth/add-auth",
			networkId, userId, networkName,
			map[string]any{"wallet_auth": freshWalletAuth(t, ctx, signer)}))
		if w.Code != http.StatusOK {
			t.Fatalf("add: HTTP %d (%q)", w.Code, strings.TrimSpace(w.Body.String()))
		}

		// a bittensor wallet, asked to remove "solana"
		w = httptest.NewRecorder()
		AuthRemove(w, authedRequest(t, http.MethodPost, "/auth/remove-auth",
			networkId, userId, networkName, map[string]any{"auth_type": "solana"}))
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d (%q), want 200 with a structured error",
				w.Code, strings.TrimSpace(w.Body.String()))
		}
		var result controller.RemoveAuthResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("body %q is not a RemoveAuthResult: %v", strings.TrimSpace(w.Body.String()), err)
		}
		if result.Error == nil {
			t.Fatal("removing solana from a bittensor-only wallet account reported success")
		}
		assertNoStatusPrefixHTTP(t, "not bound", result.Error.Message)
		if !strings.Contains(result.Error.Message, "not a sign-in method on this account") {
			t.Fatalf("refusal = %q", result.Error.Message)
		}
	})
}

// --- the peel/raise agreement --------------------------------------------

// model.PeelStatusPrefix and router.RaiseHttpError must agree on what a status
// prefix IS. They are two copies of one grammar in packages that cannot import
// each other (model deliberately does not import the router), so nothing else
// catches them drifting apart: a message the router would turn into a status
// but the peel leaves alone ships the digits to the user, and the reverse
// strips text that was never a prefix.
//
// This package imports both, so it is where the agreement can be asserted.
//
// PURE UNIT within an integration package: no database is touched.
func TestPeelStatusPrefixAgreesWithRaiseHttpError(t *testing.T) {
	cases := []string{
		"400 Invalid email or phone number.",
		"401 invalid signature",
		"409 This wallet is already linked to another account.",
		"429 Too many attempts.",
		"500 something broke",
		// tagged by an auth wrapper
		"[impl]404 Feedback not found.",
		"[outer][inner]403 Nope.",
		// NOT prefixes, and must survive untouched
		"no auth method supplied",
		"cannot remove your last auth method",
		"[impl]something broke",
		"4000 not a status",
		"400no space",
		"This wallet is already linked to another account.",
		// a multi-line message: only the first line can select a status, and
		// the remainder belongs to the body
		"400 first line\nsecond line",
	}
	for _, message := range cases {
		w := httptest.NewRecorder()
		router.RaiseHttpError(fmt.Errorf("%s", message), w)
		routerBody := strings.TrimRight(w.Body.String(), "\n")

		peeled := model.PeelStatusPrefix(message)
		if peeled != routerBody {
			t.Fatalf(
				"disagreement on %q: router leaves %q in the body, PeelStatusPrefix "+
					"yields %q. The two grammars have drifted",
				message, routerBody, peeled,
			)
		}
	}
}

// --- D1t ------------------------------------------------------------------

// WITNESS. Fails on the base branch with a recovered-panic 500.
//
// POST /auth/verify-send is UNAUTHENTICATED. NormalUserAuthV1 returns nil for
// anything that is neither a parseable email nor a phone number, and the next
// line dereferenced it -- so any caller could panic the handler with a
// three-character body. router.go's recover turns that into a 500 reading
// "Error. Please email support@ur.io for help.", which sends the user to
// support for a typo.
func TestAuthVerifySendMalformedUserAuthDoesNotPanic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, userAuth := range []string{"notanemail", "", "   ", "@", "not a phone either"} {
			body, err := json.Marshal(map[string]any{"user_auth": userAuth})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/auth/verify-send", bytes.NewReader(body))
			req.RemoteAddr = statusTestClientAddress
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			AuthVerifySend(w, req)

			response := strings.TrimSpace(w.Body.String())
			if strings.Contains(response, "email support@ur.io") {
				t.Fatalf(
					"user_auth=%q produced the recovered-panic response %q: an "+
						"unauthenticated caller can panic this handler with a malformed "+
						"address, and the user is told to contact support about a typo",
					userAuth, response,
				)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("user_auth=%q returned HTTP %d (%q), want 400", userAuth, w.Code, response)
			}
		}
	})
}
