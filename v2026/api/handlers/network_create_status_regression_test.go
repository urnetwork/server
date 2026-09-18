package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
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
	statusTestSameBucketAddress       = "203.0.113.14:41002"
	statusTestOtherAddress            = "198.51.100.20:41003"
	statusTestWalletDupAddress        = "203.0.113.41:41011"
	statusTestWalletMirrorlessAddress = "203.0.113.45:41015"
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

// Exercise the actual model -> controller -> unauthenticated HTTP wrapper.
// Previously the controller turned unclassified refusal messages into Go
// errors, so ordinary mistakes became 500s. Keep the established plain-text
// error transport, not a success-shaped 200 with an error buried in JSON.
func TestNetworkCreateExpectedRefusalsUseClientStatuses(t *testing.T) {
	userAuth := "signup-status@example.invalid"
	invalidAuth := "not-an-email-or-phone"
	password := "synthetic-password-not-a-secret"
	for _, test := range []struct {
		name, message string
		args          model.NetworkCreateArgs
		status        int
		seedUser      bool
		seedNetwork   bool
	}{
		{name: "seedphrase-terms", message: model.AgreeToTerms, args: model.NetworkCreateArgs{}, status: http.StatusBadRequest},
		{name: "email-terms", message: model.AgreeToTerms, args: model.NetworkCreateArgs{UserAuth: &userAuth}, status: http.StatusBadRequest},
		{name: "name-syntax", message: "Network name must have at least 5 characters", args: model.NetworkCreateArgs{UserAuth: &userAuth, Terms: true, NetworkName: "x"}, status: http.StatusBadRequest},
		{name: "invalid-contact", message: "Invalid email or phone number.", args: model.NetworkCreateArgs{UserAuth: &invalidAuth, Terms: true, NetworkName: "fresh-status-network"}, status: http.StatusBadRequest},
		{name: "name-conflict", message: "Network name not available", args: model.NetworkCreateArgs{UserAuth: &userAuth, Terms: true, NetworkName: "occupied-status-network"}, status: http.StatusConflict, seedNetwork: true},
		{name: "account-conflict", message: "Account might already exist. Please start over.", args: model.NetworkCreateArgs{UserAuth: &userAuth, Password: &password, Terms: true, NetworkName: "fresh-status-network"}, status: http.StatusConflict, seedUser: true},
	} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			if test.seedUser || test.seedNetwork {
				server.Tx(ctx, func(tx server.PgTx) {
					if test.seedUser {
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user (user_id, user_name, auth_type, user_auth) VALUES ($1, 'synthetic', $2, $3)`, server.NewId(), model.AuthTypePassword, userAuth))
					}
					if test.seedNetwork {
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`, server.NewId(), test.args.NetworkName, server.NewId()))
					}
				})
			}
			before := model.CountNetworks(ctx)
			w := httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, test.args))
			if w.Code != test.status || strings.TrimSpace(w.Body.String()) != test.message {
				t.Fatalf("%s refusal returned status=%d body=%q, want status=%d fixed message=%q", test.name, w.Code, strings.TrimSpace(w.Body.String()), test.status, test.message)
			}
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("Retry-After") != "" {
				t.Fatal("ordinary refusal changed the error transport or acquired a retry hint")
			}
			if after := model.CountNetworks(ctx); after != before {
				t.Fatal("a refused signup created a network")
			}
			for _, private := range []string{userAuth, password, "by_jwt", "seedphrase", "refusalStatus"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatal("refusal response included credentials, identity, or internal classification")
				}
			}
		})
	}
}

func TestNetworkCreateUnclassifiedFailureRemainsServerError(t *testing.T) {
	for _, message := range []string{"Failed to generate network name.", "Account might already exist. Please log in again.", "synthetic internal creation failure", "invalid login", "Could not verify signed token.", "synthetic provider unavailable", "synthetic key-fetch failure"} {
		w := httptest.NewRecorder()
		impl := func(model.NetworkCreateArgs, *session.ClientSession) (*model.NetworkCreateResult, error) {
			return nil, &model.NetworkCreateResultError{Message: message}
		}
		router.WrapWithInputNoAuth(impl, w, networkCreateRequest(t, statusTestClientAddress, model.NetworkCreateArgs{}))
		if w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != message {
			t.Fatal("unclassified failure was inferred to be a client error from its text")
		}
	}
}

func TestNetworkCreateMalformedAuthShapesDoNotConsumeAttempts(t *testing.T) {
	userAuth := "signup-shape@example.invalid"
	password, token := "synthetic-password-not-a-secret", "synthetic-provider-token"
	emptyToken, whitespaceToken := "", " \t\n"
	google, unsupported := string(model.AuthTypeGoogle), "synthetic-unsupported-provider"
	for _, test := range []struct {
		name, message string
		args          model.NetworkCreateArgs
	}{
		{name: "missing-password", message: "Password is required.", args: model.NetworkCreateArgs{UserAuth: &userAuth}},
		{name: "missing-provider", message: "Authentication type is required.", args: model.NetworkCreateArgs{AuthJwt: &token}},
		{name: "empty-token", message: "Authentication token is required.", args: model.NetworkCreateArgs{AuthJwt: &emptyToken, AuthJwtType: &google}},
		{name: "whitespace-token", message: "Authentication token is required.", args: model.NetworkCreateArgs{AuthJwt: &whitespaceToken, AuthJwtType: &google}},
		{name: "unsupported-provider", message: "Unsupported authentication type.", args: model.NetworkCreateArgs{AuthJwt: &token, AuthJwtType: &unsupported}},
	} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			args := test.args
			args.Terms, args.NetworkName = true, "fresh-shape-network"
			before := model.CountNetworks(ctx)
			// More malformed submissions than the entire budget must remain
			// ordinary refusals, without reaching password hashing or a provider.
			for i := 0; i <= model.AttemptFailedCountThreshold; i++ {
				w := httptest.NewRecorder()
				NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, args))
				if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != test.message || w.Header().Get("Retry-After") != "" {
					t.Fatalf("%s did not remain a fixed 400 refusal without a retry hint", test.name)
				}
				if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
					t.Fatal("malformed auth shape changed the established error transport")
				}
				for _, private := range []string{userAuth, password, token, unsupported, "refusalStatus", "by_jwt", "seedphrase"} {
					if strings.Contains(w.Body.String(), private) {
						t.Fatal("malformed auth refusal emitted identity, credentials, or internal status")
					}
				}
			}
			clientSession := session.NewLocalClientSession(ctx, statusTestClientAddress, nil)
			defer clientSession.Cancel()
			for i := 0; i < model.AttemptFailedCountThreshold; i++ {
				if _, allow := model.UserAuthAttempt(args.UserAuth, clientSession); !allow {
					t.Fatalf("%s spent auth budget before a valid attempt", test.name)
				}
			}
			// Correcting the shape must preserve the existing exhausted-budget
			// 429/Retry-After path. The synthetic token never reaches verification.
			if args.UserAuth != nil {
				args.Password = &password
			} else {
				args.AuthJwt, args.AuthJwtType = &token, &google
			}
			w := httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, args))
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("%s corrected exhausted request lost its 429 status", test.name)
			}
			assertRateLimitBody(t, w, test.name)
			if model.CountNetworks(ctx) != before {
				t.Fatal("malformed or rate-limited auth shape created a network")
			}
		})
	}
}

func TestNetworkCreateOrphanCredentialsDoNotCreateSeedphraseOrConsumeBudget(t *testing.T) {
	password, google := "synthetic-password-not-a-secret", string(model.AuthTypeGoogle)
	for _, test := range []struct {
		name, message string
		args          model.NetworkCreateArgs
	}{
		{name: "orphan-password", message: "Email or phone number is required for password signup.", args: model.NetworkCreateArgs{Password: &password}},
		{name: "orphan-provider", message: "Authentication token is required.", args: model.NetworkCreateArgs{AuthJwtType: &google}},
	} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			before := model.CountNetworks(ctx)
			args := test.args
			w := httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, args))
			if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != model.AgreeToTerms {
				t.Fatal("orphan credential guard changed terms refusal precedence")
			}
			args.Terms = true
			// No network name is needed: these must never enter the named-auth
			// path or silently turn into random-name seedphrase creation.
			for i := 0; i <= model.NetworkCreateDailyLimit; i++ {
				w = httptest.NewRecorder()
				NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, args))
				if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != test.message || w.Header().Get("Retry-After") != "" {
					t.Fatalf("%s was not a fixed pre-selection 400 refusal", test.name)
				}
			}
			if model.CountNetworks(ctx) != before {
				t.Fatal("orphan credentials created an unintended seedphrase account")
			}
			clientSession := session.NewLocalClientSession(ctx, statusTestClientAddress, nil)
			defer clientSession.Cancel()
			for i := 0; i < model.NetworkCreateDailyLimit; i++ {
				if err := model.CheckNetworkCreateRateLimit(ctx, clientSession); err != nil {
					t.Fatal("orphan credentials consumed the seedphrase creation budget")
				}
			}
			for i := 0; i < model.AttemptFailedCountThreshold; i++ {
				if _, allow := model.UserAuthAttempt(nil, clientSession); !allow {
					t.Fatal("orphan credentials consumed the shared authentication budget")
				}
			}
			w = httptest.NewRecorder()
			NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, model.NetworkCreateArgs{Terms: true}))
			if w.Code != http.StatusTooManyRequests {
				t.Fatal("valid exhausted seedphrase request lost its 429 status")
			}
			assertRateLimitBody(t, w, test.name)
			if model.CountNetworks(ctx) != before {
				t.Fatal("exhausted seedphrase request created an account")
			}
		})
	}
}

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

// The OTHER duplicate-wallet path, which the test above cannot reach.
//
// networkCreateWalletAuth has two conflict checks. The ordinary duplicate is
// caught by the network_user.wallet_address pre-check and answers through
// NetworkCreate's Created==false branch. This one -- a binding in
// network_user_auth_wallet whose mirror column was never set -- is caught by
// the second check and carries a different message. Both checks must attach
// the explicit private 409 classification; classifying only the ordinary path
// leaves this divergent row shape answering 500 while the first test goes green.
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
