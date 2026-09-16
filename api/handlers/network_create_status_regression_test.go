package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Status-code regressions for POST /auth/network-create.
//
// The status is what every SDK, proxy and retry loop acts on. A rate limit
// reported as 503 tells a well-behaved client "the server is broken, retry",
// and each retry records another attempt (model/auth_model_attempt.go:52-58),
// so the retry makes the condition worse. A validation refusal reported as 500
// tells the user their form mistake crashed the server.

// the request's own address; not loopback, so ResolveClientAddress uses it
// verbatim and the handler buckets on exactly this client
const (
	statusTestClientAddress = "203.0.113.9:41001"
	// same /29 as statusTestClientAddress, so a session built here shares the
	// bucket the handler will compute
	statusTestSameBucketAddress = "203.0.113.14:41002"
	statusTestOtherAddress      = "198.51.100.20:41003"
)

func networkCreateRequest(t testing.TB, remoteAddr string, args model.NetworkCreateArgs) *http.Request {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal network create args: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/network-create", strings.NewReader(string(body)))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestNetworkCreateEmitsOneStatusForOneRateLimit.
//
// WHAT THIS USED TO PIN. One endpoint answered the same class of event -- "you
// are rate limited on account creation" -- with 429 on the seedphrase branch
// and 503 on the email/SSO/wallet branch. This test asserted that divergence.
//
// WHY THAT WAS WRONG. A client cannot tell "you are limited" from "the server
// is down", and the 503 branch was the one with the tighter budget (5 per 5
// minutes, not 5 per 24h). A 5xx instructs every well-behaved SDK to retry;
// each retry records another attempt, so the status spent the remaining budget
// faster and the refusal reinforced itself. Both branches now answer 429, and
// both carry Retry-After so a client waits instead of guessing.
func TestNetworkCreateEmitsOneStatusForOneRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// --- seedphrase branch: exhausted CheckNetworkCreateRateLimit ---
		seedphraseSession := session.NewLocalClientSession(ctx, statusTestSameBucketAddress, nil)
		defer seedphraseSession.Cancel()
		for i := 0; i < model.NetworkCreateDailyLimit; i += 1 {
			if err := model.CheckNetworkCreateRateLimit(ctx, seedphraseSession); err != nil {
				t.Fatalf("priming create attempt %d was refused: %v", i+1, err)
			}
		}

		w := httptest.NewRecorder()
		NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, model.NetworkCreateArgs{
			Terms: true,
		}))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf(
				"seedphrase signup over the daily limit returned HTTP %d (%q), want 429",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		assertRateLimitBody(t, w, "seedphrase")

		// --- email/SSO/wallet branch: exhausted UserAuthAttempt ---
		ssoSession := session.NewLocalClientSession(ctx, statusTestOtherAddress, nil)
		defer ssoSession.Cancel()
		for i := 0; i < model.AttemptFailedCountThreshold; i += 1 {
			model.UserAuthAttempt(nil, ssoSession)
		}

		authJwt := "sso-token-for-a-first-time-user"
		authJwtType := "google"
		w = httptest.NewRecorder()
		NetworkCreate(w, networkCreateRequest(t, statusTestOtherAddress, model.NetworkCreateArgs{
			AuthJwt:     &authJwt,
			AuthJwtType: &authJwtType,
			NetworkName: "newcomernet",
			Terms:       true,
		}))
		if w.Code == http.StatusServiceUnavailable {
			t.Fatalf(
				"the auth-attempt limit still answers 503 (%q). Every SDK reads that as "+
					"'the server is broken, retry', and each retry records another attempt",
				strings.TrimSpace(w.Body.String()),
			)
		}
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf(
				"rate-limited SSO signup returned HTTP %d (%q), want 429 -- the same status "+
					"the seedphrase branch gives for the same class of event",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		if strings.Contains(w.Body.String(), "User auth attempts exceeded limits") {
			t.Fatalf("429 body still carries the old opaque message: %q", strings.TrimSpace(w.Body.String()))
		}
		assertRateLimitBody(t, w, "sso")
	})
}

// assertRateLimitBody checks the two properties item 4 is about: the refusal
// says the limit is scoped to the network address rather than accusing the
// caller, and it carries a Retry-After so a client waits a known interval
// instead of retrying immediately into the same limit.
func assertRateLimitBody(t testing.TB, w *httptest.ResponseRecorder, branch string) {
	t.Helper()
	body := strings.TrimSpace(w.Body.String())

	if strings.Contains(body, "You have reached the maximum number of account creations") {
		t.Fatalf(
			"the %s refusal still tells the user they created the accounts: %q. The "+
				"budget is address-scoped, so the usual recipient created none of them",
			branch, body,
		)
	}
	if !strings.Contains(body, "address") {
		t.Fatalf(
			"the %s refusal body %q never says the limit is scoped to the network "+
				"address; support cannot tell a wrongly-refused user from abuse",
			branch, body,
		)
	}

	retryAfter := w.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatalf(
			"the %s 429 carries no Retry-After; the client can only guess when to try "+
				"again, and guessing early costs it more of the same budget",
			branch,
		)
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds <= 0 {
		t.Fatalf("the %s 429 sent Retry-After: %q, want a positive number of seconds", branch, retryAfter)
	}
}

// TestNetworkCreateValidationRefusalIsNotReportedAsSuccess covers the
// error-return convention (task item 5).
//
// The model returns validation refusals in the body as
// NetworkCreateResult.Error with a nil Go error, and controller.NetworkCreate
// converts that body error into a Go error. RaiseHttpError then turns a
// leading "<code> " into the status and drops it from the body -- so the model
// now prefixes its user-caused refusals ("400 " + AgreeToTerms) and this
// endpoint answers 400 instead of the 500 it used to.
//
// This test previously pinned that 500 and said in its own text: "if this is
// now 400 that is the fix, update this test". This is that update.
//
// Two properties are pinned. The first is the genuine safety property, and is
// unchanged: a refusal must never reach the client as 200 with the error buried
// in the body, which a status-checking client reads as a created account. The
// second is that the status now discriminates a form mistake (4xx) from a
// server fault (5xx).
//
// RESIDUAL, pinned deliberately: the body is still text/plain, so
// NetworkCreateResult.Error remains unpopulated over HTTP. Making it a
// structured body is a separate, larger change (it would need the spec's
// NetworkCreateResult to become the refusal carrier); pinned here so that
// change is made on purpose rather than by accident.
func TestNetworkCreateValidationRefusalIsNotReportedAsSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		w := httptest.NewRecorder()
		NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, model.NetworkCreateArgs{
			Terms: false,
		}))

		if w.Code == http.StatusOK {
			t.Fatalf(
				"a signup refused for %q returned HTTP 200 with body %q: a status-checking "+
					"client reads that as a created account",
				model.AgreeToTerms, strings.TrimSpace(w.Body.String()),
			)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf(
				"terms-not-accepted signup returned HTTP %d (%q), want 400: an unticked "+
					"terms box is a form mistake, and 5xx tells every SDK the server broke "+
					"and to retry",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		// the status prefix is the transport's, not the user's: it must not
		// survive into the message a person reads
		if strings.TrimSpace(w.Body.String()) != string(model.AgreeToTerms) {
			t.Fatalf("refusal body was %q, want the plain message %q with no status prefix",
				strings.TrimSpace(w.Body.String()), model.AgreeToTerms)
		}
	})
}

// Status-code coverage for the rest of /auth/network-create's refusals.
//
// The change that makes these possible is one line wide in effect -- the model
// now prefixes its user-caused NetworkCreateResult.Error messages with the
// router's "<code> " convention -- but it reaches EVERY signup branch: email,
// phone, SSO, seedphrase and wallet. The blast radius is why the email and SSO
// siblings are tested here alongside the wallet case, not just the wallet case
// that prompted the work.
//
// Each test uses its own client address. The auth-attempt limiter is scoped to
// the address and shared across everyone on it, so tests that make several
// signup attempts would otherwise spend each other's budget.
const (
	statusTestWalletDupAddress        = "203.0.113.41:41011"
	statusTestSsoMalformedAddress     = "203.0.113.42:41012"
	statusTestEmailDupAddress         = "203.0.113.43:41013"
	statusTestBadNameAddress          = "203.0.113.44:41014"
	statusTestWalletMirrorlessAddress = "203.0.113.45:41015"
)

// THE REGRESSION TEST FOR THE CONFIRMED DEFECT.
//
// Signing up with a wallet that is already bound to another account is an
// ordinary thing for a user to do -- it is what happens when someone who
// already has an account taps "create account" instead of "sign in". It
// answered HTTP 500 with the raw text "Account might already exist. Please log
// in again." in a text/plain body.
//
// It must live at the handler layer. The model-layer equivalent
// (model/wallet_auth_binding_test.go TestNetworkCreateDuplicateWalletReturnsStructuredError)
// PASSES on the base branch while the endpoint 500s, because the model really
// does return a structured result -- the controller flattens it afterwards.
// Model-layer tests are structurally blind to this entire bug class.
//
// WITNESS: fails on the base branch (500, not 409).
//
// The DISTINCT NETWORK NAME is load-bearing. checkNetworkNameAvailability and
// the name-taken refusal sit ABOVE the wallet branch, and name-taken is ALSO a
// 409 now -- reusing the first name would short-circuit there and the test
// would go green without ever reaching the wallet path. The body is the only
// thing that tells the two 409s apart, so it is asserted too.
func TestNetworkCreateDuplicateWalletAnswers409(t *testing.T) {
	for _, chain := range handlerWalletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				signer := chain.newSigner(t)

				w := httptest.NewRecorder()
				NetworkCreate(w, networkCreateRequest(t, statusTestWalletDupAddress, model.NetworkCreateArgs{
					NetworkName: "walletdup-first",
					Terms:       true,
					WalletAuth:  freshWalletAuth(t, ctx, signer),
				}))
				if w.Code != http.StatusOK {
					t.Fatalf("first wallet signup: HTTP %d (%q)", w.Code, strings.TrimSpace(w.Body.String()))
				}

				// same wallet, FRESH challenge, DIFFERENT network name
				w = httptest.NewRecorder()
				NetworkCreate(w, networkCreateRequest(t, statusTestWalletDupAddress, model.NetworkCreateArgs{
					NetworkName: "walletdup-second",
					Terms:       true,
					WalletAuth:  freshWalletAuth(t, ctx, signer),
				}))

				body := strings.TrimSpace(w.Body.String())
				if w.Code == http.StatusInternalServerError {
					t.Fatalf(
						"signing up with an already-registered %s wallet still answers 500 "+
							"(%q). A user who taps 'create account' instead of 'sign in' is "+
							"told the server crashed, and every SDK retries",
						chain.name, body,
					)
				}
				if w.Code != http.StatusConflict {
					t.Fatalf("HTTP %d (%q), want 409", w.Code, body)
				}
				// discriminates this 409 from the name-taken 409 above it
				if body != "Account might already exist. Please log in again." {
					t.Fatalf(
						"body = %q; this must be the wallet-already-bound refusal, not the "+
							"network-name-taken one that also answers 409",
						body,
					)
				}
			})
		})
	}
}

// WITNESS: fails on the base branch (500, not 400).
//
// Two ways an SSO signup falls past every branch and lands on NetworkCreate's
// "invalid login" fallthrough:
//   - auth_jwt present, auth_jwt_type absent -- the branch is guarded by `&&`
//   - a token ParseAuthJwt cannot verify, which leaves authJwt nil and the
//     branch has no else
//
// Both are ordinary client mistakes (an expired Apple token is the common one),
// and both answered 500.
func TestNetworkCreateMalformedSsoAnswers400(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		authJwt := "not-a-real-signed-sso-token"
		authJwtType := "google"

		cases := map[string]model.NetworkCreateArgs{
			"auth_jwt with no type": {
				AuthJwt:     &authJwt,
				NetworkName: "ssobad-notype",
				Terms:       true,
			},
			"unverifiable token": {
				AuthJwt:     &authJwt,
				AuthJwtType: &authJwtType,
				NetworkName: "ssobad-badtoken",
				Terms:       true,
			},
		}
		for name, args := range cases {
			w := httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestSsoMalformedAddress, args))
			body := strings.TrimSpace(w.Body.String())
			if w.Code == http.StatusInternalServerError {
				t.Fatalf("%s: still answers 500 (%q)", name, body)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: HTTP %d (%q), want 400", name, w.Code, body)
			}
			if body != "invalid login" {
				t.Fatalf("%s: body = %q, want the plain message with no status prefix", name, body)
			}
		}
	})
}

// The shared-blast-radius siblings. The wallet fix is one change to a function
// every signup path goes through, so the email and network-name branches are
// asserted here to show the change is right for them too -- and that a server
// fault is still a 5xx.
//
// WITNESS: both fail on the base branch (500).
func TestNetworkCreateUserInputRefusalsCarryClientStatuses(t *testing.T) {
	t.Run("duplicate email answers 409", func(t *testing.T) {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			userAuth := "networkcreate-dup@example.com"
			password := "SomeValidPassword123!"

			w := httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestEmailDupAddress, model.NetworkCreateArgs{
				UserAuth:    &userAuth,
				Password:    &password,
				NetworkName: "emaildup-first",
				Terms:       true,
			}))
			if w.Code != http.StatusOK {
				t.Fatalf("first email signup: HTTP %d (%q)", w.Code, strings.TrimSpace(w.Body.String()))
			}

			// the same address, a DIFFERENT network name (see the note on the
			// wallet test: name-taken is also a 409 and sits above this)
			w = httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestEmailDupAddress, model.NetworkCreateArgs{
				UserAuth:    &userAuth,
				Password:    &password,
				NetworkName: "emaildup-second",
				Terms:       true,
			}))
			body := strings.TrimSpace(w.Body.String())
			if w.Code == http.StatusInternalServerError {
				t.Fatalf("a duplicate email signup still answers 500 (%q)", body)
			}
			if w.Code != http.StatusConflict {
				t.Fatalf("HTTP %d (%q), want 409", w.Code, body)
			}
			if body != "Account might already exist. Please start over." {
				t.Fatalf("body = %q, want the duplicate-account message", body)
			}
		})
	})

	t.Run("bad network name answers 400", func(t *testing.T) {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			userAuth := "networkcreate-badname@example.com"
			password := "SomeValidPassword123!"

			w := httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestBadNameAddress, model.NetworkCreateArgs{
				UserAuth:    &userAuth,
				Password:    &password,
				NetworkName: "ab", // below the 5 character minimum
				Terms:       true,
			}))
			body := strings.TrimSpace(w.Body.String())
			if w.Code == http.StatusInternalServerError {
				t.Fatalf("a too-short network name still answers 500 (%q)", body)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d (%q), want 400", w.Code, body)
			}
			if body != "Network name must have at least 5 characters" {
				t.Fatalf("body = %q, want the validation message with no status prefix", body)
			}
		})
	})
}

// The OTHER duplicate-wallet path, which the test above cannot reach.
//
// networkCreateWalletAuth has two conflict checks. The ordinary duplicate is
// caught by the network_user.wallet_address pre-check and answers through
// NetworkCreate's Created==false branch. This one -- a binding in
// network_user_auth_wallet whose mirror column was never set -- is caught by
// the second check and answers through the `err` return instead, a completely
// different line with its own message literal. Prefixing one and not the other
// leaves this path answering 500 while the test above goes green.
//
// The state is not reachable through today's writers (they keep the two tables
// in step), so it is constructed directly, the same way
// model/wallet_auth_binding_test.go TestNetworkCreateDuplicateWalletReturnsStructuredError
// does -- and that model test is exactly the one that passes while this
// endpoint 500s, which is why this exists at the handler layer.
//
// WITNESS: fails on the base branch (500, not 409).
func TestNetworkCreateDuplicateWalletWithoutMirrorAnswers409(t *testing.T) {
	for _, chain := range handlerWalletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				signer := chain.newSigner(t)

				// an account that owns the wallet in the child table only
				ownerNetworkId := server.NewId()
				ownerUserId := server.NewId()
				model.Testing_CreateNetwork(ctx, ownerNetworkId, "mirrorless-owner", ownerUserId)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`INSERT INTO network_user_auth_wallet (user_id, wallet_address, blockchain)
						 VALUES ($1, $2, $3)`,
						ownerUserId, signer.address, signer.blockchain,
					))
				})

				w := httptest.NewRecorder()
				NetworkCreate(w, networkCreateRequest(t, statusTestWalletMirrorlessAddress, model.NetworkCreateArgs{
					NetworkName: "mirrorless-taker",
					Terms:       true,
					WalletAuth:  freshWalletAuth(t, ctx, signer),
				}))

				body := strings.TrimSpace(w.Body.String())
				if w.Code == http.StatusInternalServerError {
					t.Fatalf(
						"the divergent-mirror duplicate still answers 500 (%q). The two "+
							"conflict checks in networkCreateWalletAuth carry separate message "+
							"literals, and only one of them states its status",
						body,
					)
				}
				if w.Code != http.StatusConflict {
					t.Fatalf("HTTP %d (%q), want 409", w.Code, body)
				}
				if body != "This wallet is already linked to another account." {
					t.Fatalf("body = %q, want the wallet-taken message with no status prefix", body)
				}
			})
		})
	}
}
