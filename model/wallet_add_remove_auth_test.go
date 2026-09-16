package model

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// The add/remove auth-method surface, exercised the way a user drives it.
//
// Every wallet case is table-driven over {solana, bittensor} rather than
// written once per chain. That is the point of the file: Bittensor parity is
// asserted structurally, so a change that quietly makes one chain behave
// differently from the other fails here instead of being discovered by a user.
// Both signers are real -- a Solana ed25519 keypair and a Bittensor sr25519
// keypair producing genuine <Bytes>-wrapped schnorrkel signatures (see
// network_wallet_lifecycle_test.go) -- so nothing below is mocked.
//
// INTEGRATION: needs the local postgres/redis stack (WARP_ENV=local).

// statusPrefixPattern matches the "<code> " the router peels into an HTTP
// status. /auth/add-auth and /auth/remove-auth answer 200 with a structured
// error object instead, so a message that reaches error.message with one of
// these still attached would render the literal digits in the client's UI.
var statusPrefixPattern = regexp.MustCompile(`^[45][0-9]{2}\s`)

func assertNoStatusPrefix(t testing.TB, what string, message string) {
	t.Helper()
	if statusPrefixPattern.MatchString(message) {
		t.Fatalf(
			"%s: error.message = %q -- the transport's status prefix leaked into "+
				"the message a person reads",
			what, message,
		)
	}
}

type walletChainCase struct {
	name      string
	authType  AuthType
	chainName string
	newSigner func(testing.TB) acceptanceWalletSigner
}

// walletChains is the parity table every wallet test below runs over.
var walletChains = []walletChainCase{
	{name: "solana", authType: AuthTypeSolana, chainName: SOL.String(), newSigner: newSolanaAcceptanceWalletSigner},
	{name: "bittensor", authType: AuthTypeBittensor, chainName: TAO.String(), newSigner: newBittensorAcceptanceWalletSigner},
}

// otherChain returns the case for the chain this one is not, so a test can
// assert the cross-chain behaviour from either direction without a second table.
func (c walletChainCase) otherChain() walletChainCase {
	for _, other := range walletChains {
		if other.name != c.name {
			return other
		}
	}
	panic("walletChains must hold both chains")
}

// authParityAccount creates a network whose user already has one auth method
// (email, via Testing_CreateNetwork) and returns an authenticated session for
// it. Network names are made unique per account: checkNetworkNameAvailability
// and the name-taken refusal sit ABOVE the branch most of these tests are
// aiming at, so a reused name short-circuits the request before it ever
// reaches the code under test.
func authParityAccount(
	t testing.TB,
	ctx context.Context,
	label string,
) (userId server.Id, userAuth string, clientSession *session.ClientSession) {
	t.Helper()
	networkId := server.NewId()
	userId = server.NewId()
	networkName := fmt.Sprintf("%s-%s", label, strings.ToLower(networkId.String()[:8]))
	// Testing_CreateNetwork seeds one email auth and returns its address, so
	// every account here starts with exactly one non-wallet sign-in method
	userAuth = Testing_CreateNetwork(ctx, networkId, networkName, userId)
	clientSession = session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
	})
	return userId, userAuth, clientSession
}

// addWalletThroughAddAuth drives the real endpoint path: a freshly issued,
// freshly signed challenge through AddAuth.
func addWalletThroughAddAuth(
	t testing.TB,
	ctx context.Context,
	clientSession *session.ClientSession,
	signer acceptanceWalletSigner,
) *AddAuthMethodResult {
	t.Helper()
	result, err := AddAuth(AddAuthMethod{
		WalletAuth: signedAcceptanceWalletChallenge(t, ctx, signer),
	}, clientSession)
	if err != nil {
		t.Fatalf("AddAuth returned a transport error: %v", err)
	}
	if result == nil {
		t.Fatal("AddAuth returned a nil result with a nil error")
	}
	return result
}

func authTypesOf(t testing.TB, ctx context.Context, userId server.Id) []string {
	t.Helper()
	networkUser := GetNetworkUser(ctx, userId)
	if networkUser == nil {
		t.Fatal("GetNetworkUser returned nil")
	}
	return networkUser.AuthTypes
}

func hasAuthType(authTypes []string, want string) bool {
	for _, authType := range authTypes {
		if authType == want {
			return true
		}
	}
	return false
}

// networkUserWalletColumns reads the two mirrored scalars on network_user.
// They are not the source of truth (network_user_auth_wallet is), but
// NetworkCreate pre-checks them and RemoveAuth clears them, so a remove/re-add
// cycle that leaves them stale locks the wallet out of creating a fresh
// account later.
func networkUserWalletColumns(
	t testing.TB,
	ctx context.Context,
	userId server.Id,
) (address *string, blockchain *string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`SELECT wallet_address, wallet_blockchain FROM network_user WHERE user_id = $1`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&address, &blockchain))
			}
		})
	})
	return
}

func networkUserAuthTypeScalar(t testing.TB, ctx context.Context, userId server.Id) string {
	t.Helper()
	var authType string
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`SELECT auth_type FROM network_user WHERE user_id = $1`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&authType))
			}
		})
	})
	return authType
}

func walletRowCount(t testing.TB, ctx context.Context, userId server.Id) int {
	t.Helper()
	count := 0
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`SELECT COUNT(*) FROM network_user_auth_wallet WHERE user_id = $1`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// addTestSsoAuth plants an SSO row without a real signed provider token.
// ParseAuthJwt needs a genuine Apple/Google signature, which a test cannot
// produce; addSsoAuth is the same function AddAuth calls once the token has
// been parsed, so the rows and therefore the remove-side behaviour are
// identical. Established pattern -- account_model_test.go does the same.
func addTestSsoAuth(
	t testing.TB,
	ctx context.Context,
	userId server.Id,
	ssoType SsoAuthType,
	userAuth string,
) {
	t.Helper()
	err := addSsoAuth(&AddSsoAuthArgs{
		ParsedAuthJwt: AuthJwt{
			AuthType: ssoType,
			UserAuth: userAuth,
		},
		AuthJwt:     "",
		AuthJwtType: ssoType,
		UserId:      userId,
	}, ctx)
	if err != nil {
		t.Fatalf("addSsoAuth(%s): %v", ssoType, err)
	}
}

// --- A1 -------------------------------------------------------------------

// The user's core ask: bind a wallet to an account that already signs in
// another way, for both chains.
//
// CONTRACT PIN (passes on the base branch): this path already worked for
// Bittensor. It is here because nothing asserted it, and because every other
// test in this file is only meaningful if this one holds.
func TestAddWalletAuthOntoExistingAccount(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "add-wallet")
				signer := chain.newSigner(t)

				result := addWalletThroughAddAuth(t, ctx, clientSession, signer)
				if result.Error != nil {
					t.Fatalf("AddAuth refused a valid %s wallet: %s", chain.name, result.Error.Message)
				}

				networkUser := GetNetworkUser(ctx, userId)
				if len(networkUser.WalletAuths) != 1 {
					t.Fatalf("wallet_auths = %#v, want exactly one row", networkUser.WalletAuths)
				}
				// the chain is reported to clients two independent ways, and
				// both must name Bittensor correctly
				if networkUser.WalletAuths[0].Blockchain != chain.chainName {
					t.Fatalf("wallet_auths[0].blockchain = %q, want %q",
						networkUser.WalletAuths[0].Blockchain, chain.chainName)
				}
				if !hasAuthType(networkUser.AuthTypes, chain.authType) {
					t.Fatalf("auth_types = %#v, want it to contain %q", networkUser.AuthTypes, chain.authType)
				}
				if !hasAuthType(networkUser.AuthTypes, "email") {
					t.Fatalf("auth_types = %#v, want the pre-existing email to survive", networkUser.AuthTypes)
				}

				// the mirrored scalars NetworkCreate pre-checks
				address, blockchain := networkUserWalletColumns(t, ctx, userId)
				if address == nil || *address != signer.address {
					t.Fatalf("network_user.wallet_address = %v, want %q", address, signer.address)
				}
				if blockchain == nil || *blockchain != chain.chainName {
					t.Fatalf("network_user.wallet_blockchain = %v, want %q", blockchain, chain.chainName)
				}
			})
		})
	}
}

// --- A2 -------------------------------------------------------------------

// Binding a wallet is proof of key possession, so it consumes the same
// server-issued single-use challenge as login. A captured signature must not
// bind the wallet to a second account.
//
// Also the D5 assertion: addWalletAuth passes the challenge layer's already
// prefixed message ("403 challenge already used") straight through, and
// /auth/add-auth answers 200 with that text in error.message. Before the fix
// the user saw the literal "403 " in the app's error toast.
func TestAddWalletAuthRequiresAFreshChallenge(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "replay")
				signer := chain.newSigner(t)

				walletAuth := signedAcceptanceWalletChallenge(t, ctx, signer)
				first, err := AddAuth(AddAuthMethod{WalletAuth: walletAuth}, clientSession)
				if err != nil {
					t.Fatal(err)
				}
				if first.Error != nil {
					t.Fatalf("first add refused: %s", first.Error.Message)
				}

				// the identical signed payload, replayed
				replay, err := AddAuth(AddAuthMethod{WalletAuth: walletAuth}, clientSession)
				if err != nil {
					t.Fatalf("a replayed challenge produced a transport error: %v", err)
				}
				if replay.Error == nil {
					t.Fatal("a consumed challenge was accepted a second time")
				}
				assertNoStatusPrefix(t, "replayed challenge", replay.Error.Message)
				if !strings.Contains(replay.Error.Message, "challenge already used") {
					t.Fatalf("replay refusal = %q, want it to name the used challenge", replay.Error.Message)
				}
				// the refusal must not have disturbed the binding that exists
				if walletRowCount(t, ctx, userId) != 1 {
					t.Fatal("the replay refusal changed the bound wallet")
				}
			})
		})
	}
}

// --- A3 -------------------------------------------------------------------

// One wallet, one account. A wallet already bound elsewhere cannot be taken
// over by signing a fresh challenge from a second session.
func TestAddWalletAuthRejectsWalletBoundToAnotherAccount(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				ownerId, _, ownerSession := authParityAccount(t, ctx, "owner")
				takerId, _, takerSession := authParityAccount(t, ctx, "taker")
				signer := chain.newSigner(t)

				owned := addWalletThroughAddAuth(t, ctx, ownerSession, signer)
				if owned.Error != nil {
					t.Fatalf("owner could not bind the wallet: %s", owned.Error.Message)
				}

				taken := addWalletThroughAddAuth(t, ctx, takerSession, signer)
				if taken.Error == nil {
					t.Fatal("a wallet bound to another account was bound a second time")
				}
				assertNoStatusPrefix(t, "wallet taken", taken.Error.Message)
				if !strings.Contains(taken.Error.Message, "already linked to another account") {
					t.Fatalf("refusal = %q", taken.Error.Message)
				}

				// the original binding is intact and the taker gained nothing
				if walletRowCount(t, ctx, ownerId) != 1 {
					t.Fatal("the owner's wallet binding was disturbed")
				}
				if walletRowCount(t, ctx, takerId) != 0 {
					t.Fatal("the taker ended up with a wallet row")
				}
			})
		})
	}
}

// --- A4 -------------------------------------------------------------------

// Re-adding the wallet a user already has is a no-op success, not a conflict.
// A client that retries after a dropped response, or re-runs the bind flow,
// must not be told its own wallet belongs to someone else.
func TestAddWalletAuthReAddingTheSameWalletIsIdempotent(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "re-add")
				signer := chain.newSigner(t)

				if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
					t.Fatalf("first add refused: %s", result.Error.Message)
				}
				// a FRESH challenge -- the replay path is A2's subject
				if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
					t.Fatalf("re-adding the same wallet was refused: %s", result.Error.Message)
				}

				if count := walletRowCount(t, ctx, userId); count != 1 {
					t.Fatalf("wallet rows = %d, want exactly 1 after re-adding the same wallet", count)
				}
				address, blockchain := networkUserWalletColumns(t, ctx, userId)
				if address == nil || *address != signer.address {
					t.Fatalf("network_user.wallet_address = %v after re-add", address)
				}
				if blockchain == nil || *blockchain != chain.chainName {
					t.Fatalf("network_user.wallet_blockchain = %v after re-add", blockchain)
				}
			})
		})
	}
}

// --- A5 -------------------------------------------------------------------

// DESIGN LIMITATION, PINNED ON PURPOSE.
//
// network_user_auth_wallet is PRIMARY KEY (user_id), so an account holds at
// most one wallet: a user cannot keep a Solana wallet and a Bittensor wallet at
// the same time. The verbatim ask ("add and remove a solana, email or sso
// account at will") reads like holding both, so this is pinned rather than
// left implicit -- supporting both chains at once is a schema change
// (PRIMARY KEY(user_id) -> UNIQUE(user_id, blockchain), plus GetNetworkUser's
// walletAuths[0] becoming a loop), and whoever makes it has to come here and
// delete this test deliberately.
//
// Both directions are asserted, because "Bittensor is refused while Solana is
// allowed" would be exactly the asymmetry this file exists to catch.
func TestAddWalletAuthRefusesASecondWalletOfAnotherChain(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name+"-then-"+chain.otherChain().name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "two-chain")

				first := chain.newSigner(t)
				if result := addWalletThroughAddAuth(t, ctx, clientSession, first); result.Error != nil {
					t.Fatalf("first (%s) add refused: %s", chain.name, result.Error.Message)
				}

				second := chain.otherChain().newSigner(t)
				result := addWalletThroughAddAuth(t, ctx, clientSession, second)
				if result.Error == nil {
					t.Fatalf(
						"an account bound to a %s wallet also accepted a %s wallet; "+
							"network_user_auth_wallet is PRIMARY KEY (user_id), so if this "+
							"now succeeds one of the two wallets was silently discarded",
						chain.name, chain.otherChain().name,
					)
				}
				assertNoStatusPrefix(t, "second wallet", result.Error.Message)
				if !strings.Contains(result.Error.Message, "A different wallet is already linked") {
					t.Fatalf("refusal = %q", result.Error.Message)
				}
				// the refusal must say what the user can actually do. A
				// wallet-only account cannot remove its wallet first (that is
				// its last auth method), and a seedphrase is the only escape
				// that needs no external identity.
				if !strings.Contains(result.Error.Message, "seedphrase") {
					t.Fatalf(
						"the swap refusal does not mention generating a seedphrase, which is "+
							"the only escape a wallet-only user has that needs no email or "+
							"phone: %q",
						result.Error.Message,
					)
				}

				// the original wallet survived untouched
				address, _ := networkUserWalletColumns(t, ctx, userId)
				if address == nil || *address != first.address {
					t.Fatalf("the bound wallet changed to %v", address)
				}
			})
		})
	}
}

// --- A6 -------------------------------------------------------------------

// Removing a wallet must clear everything binding it: the child row AND both
// mirrored scalars. A stale network_user.wallet_address survives the global
// unique index and blocks that wallet from ever creating a fresh account.
func TestRemoveWalletAuthClearsEveryRowItCreated(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "remove-wallet")
				signer := chain.newSigner(t)
				if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
					t.Fatalf("add refused: %s", result.Error.Message)
				}

				if err := RemoveAuth(ctx, userId, chain.authType); err != nil {
					t.Fatalf("RemoveAuth(%s) = %v, want success", chain.authType, err)
				}

				if count := walletRowCount(t, ctx, userId); count != 0 {
					t.Fatalf("wallet rows = %d after removal, want 0", count)
				}
				address, blockchain := networkUserWalletColumns(t, ctx, userId)
				if address != nil {
					t.Fatalf("network_user.wallet_address = %q after removal, want NULL", *address)
				}
				if blockchain != nil {
					t.Fatalf("network_user.wallet_blockchain = %q after removal, want NULL", *blockchain)
				}
				authTypes := authTypesOf(t, ctx, userId)
				if hasAuthType(authTypes, chain.authType) {
					t.Fatalf("auth_types = %#v still lists the removed wallet", authTypes)
				}
				if !hasAuthType(authTypes, "email") {
					t.Fatalf("auth_types = %#v lost the surviving email", authTypes)
				}
			})
		})
	}
}

// --- A7 -------------------------------------------------------------------

// The round trip the mirror exists to protect: remove, then bind the same
// wallet again. The scalars have to come back, or the account is in a state
// GetNetworkUser and NetworkCreate disagree about.
func TestRemoveThenReAddWalletRoundTrips(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "round-trip")
				signer := chain.newSigner(t)

				if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
					t.Fatalf("add refused: %s", result.Error.Message)
				}
				if err := RemoveAuth(ctx, userId, chain.authType); err != nil {
					t.Fatalf("RemoveAuth = %v", err)
				}
				if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
					t.Fatalf("re-add after removal refused: %s", result.Error.Message)
				}

				address, blockchain := networkUserWalletColumns(t, ctx, userId)
				if address == nil || *address != signer.address {
					t.Fatalf("network_user.wallet_address = %v after re-add, want %q", address, signer.address)
				}
				if blockchain == nil || *blockchain != chain.chainName {
					t.Fatalf("network_user.wallet_blockchain = %v after re-add, want %q", blockchain, chain.chainName)
				}
				if !hasAuthType(authTypesOf(t, ctx, userId), chain.authType) {
					t.Fatal("auth_types did not regain the re-added wallet")
				}
			})
		})
	}
}

// --- A8 -------------------------------------------------------------------

// WITNESS. Fails on the base branch.
//
// RemoveAuth's wallet DELETE has no blockchain predicate, so "solana" and
// "bittensor" behaved as interchangeable aliases for "whatever wallet this
// user has". An account holding a Bittensor wallet that was sent
// {"auth_type":"solana"} answered 200 and deleted the Bittensor wallet.
//
// This is the sharpest "bittensor is a label, not a predicate" case on the
// surface, and it is the one that can cost a user their wallet binding.
func TestRemoveWalletAuthRejectsTheWrongChain(t *testing.T) {
	for _, chain := range walletChains {
		t.Run("bound-"+chain.name+"-remove-"+chain.otherChain().name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "wrong-chain")
				signer := chain.newSigner(t)
				if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
					t.Fatalf("add refused: %s", result.Error.Message)
				}

				wrong := chain.otherChain().authType
				err := RemoveAuth(ctx, userId, wrong)
				if err == nil {
					t.Fatalf(
						"RemoveAuth(%q) succeeded on an account whose only wallet is %s: "+
							"the chain is being treated as an alias rather than a predicate, "+
							"so the wrong wallet was deleted",
						wrong, chain.name,
					)
				}
				if !strings.Contains(err.Error(), "not a sign-in method on this account") {
					t.Fatalf("refusal = %q", err)
				}

				// the load-bearing half: the wallet is still there
				if count := walletRowCount(t, ctx, userId); count != 1 {
					t.Fatalf("wallet rows = %d after the refused removal, want 1", count)
				}
				address, blockchain := networkUserWalletColumns(t, ctx, userId)
				if address == nil || *address != signer.address {
					t.Fatalf("network_user.wallet_address = %v after a refused removal", address)
				}
				if blockchain == nil || *blockchain != chain.chainName {
					t.Fatalf("network_user.wallet_blockchain = %v after a refused removal", blockchain)
				}
				if !hasAuthType(authTypesOf(t, ctx, userId), chain.authType) {
					t.Fatal("auth_types lost the wallet a refused removal should not have touched")
				}
			})
		})
	}
}

// --- A9 -------------------------------------------------------------------

// WITNESS. All three sub-cases fail on the base branch.
//
// RemoveAuth counted the account's auth methods, guarded only against removing
// the last one, then ran a DELETE and never looked at how many rows it matched.
// Every no-op reported success, so a client that unlinked a method the account
// never had was told it worked -- and then showed the user a sign-in list that
// disagreed with the server.
func TestRemoveAuthRefusesAMethodThatIsNotBound(t *testing.T) {
	t.Run("sso not bound", func(t *testing.T) {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			userId, _, _ := authParityAccount(t, ctx, "no-apple")
			addTestSsoAuth(t, ctx, userId, SsoAuthTypeGoogle, "not-apple@example.com")

			// google + email, asked to remove apple
			err := RemoveAuth(ctx, userId, "apple")
			if err == nil {
				t.Fatal("removing an unbound apple auth reported success")
			}
			if !strings.Contains(err.Error(), "not a sign-in method on this account") {
				t.Fatalf("refusal = %q", err)
			}
			authTypes := authTypesOf(t, ctx, userId)
			if !hasAuthType(authTypes, "google") || !hasAuthType(authTypes, "email") {
				t.Fatalf("auth_types = %#v -- the refused removal deleted something", authTypes)
			}
		})
	})

	t.Run("email not bound", func(t *testing.T) {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			userId, _, clientSession := authParityAccount(t, ctx, "no-email")
			signer := newBittensorAcceptanceWalletSigner(t)
			if result := addWalletThroughAddAuth(t, ctx, clientSession, signer); result.Error != nil {
				t.Fatalf("add refused: %s", result.Error.Message)
			}
			// A seedphrase as well as the wallet, deliberately: the account
			// must still hold TWO methods after the email is gone. With only
			// one left the last-method guard would refuse the second call for
			// an unrelated reason and this case would pass without the
			// not-bound guard existing at all.
			if _, err := GenerateSeedphrase(ctx, userId); err != nil {
				t.Fatalf("GenerateSeedphrase: %v", err)
			}

			// drop the email this account started with...
			if err := RemoveAuth(ctx, userId, "email"); err != nil {
				t.Fatalf("removing the bound email failed: %v", err)
			}
			// ...then ask for it again. There is nothing to delete now, and
			// two other methods remain, so nothing else can refuse this.
			err := RemoveAuth(ctx, userId, "email")
			if err == nil {
				t.Fatal("removing an already-removed email reported success")
			}
			if !strings.Contains(err.Error(), "not a sign-in method on this account") {
				t.Fatalf("refusal = %q", err)
			}
			if count := walletRowCount(t, ctx, userId); count != 1 {
				t.Fatalf("wallet rows = %d, want the wallet untouched", count)
			}
			if !hasAuthType(authTypesOf(t, ctx, userId), "seedphrase") {
				t.Fatal("the refused removal took the seedphrase with it")
			}
		})
	})

	t.Run("seedphrase not bound", func(t *testing.T) {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			userId, _, clientSession := authParityAccount(t, ctx, "no-seed")
			if result := addWalletThroughAddAuth(t, ctx, clientSession, newSolanaAcceptanceWalletSigner(t)); result.Error != nil {
				t.Fatalf("add refused: %s", result.Error.Message)
			}
			err := RemoveAuth(ctx, userId, "seedphrase")
			if err == nil {
				t.Fatal("removing a seedphrase the account never had reported success")
			}
			if !strings.Contains(err.Error(), "not a sign-in method on this account") {
				t.Fatalf("refusal = %q", err)
			}
		})
	})
}

// --- A10 ------------------------------------------------------------------

// CONTRACT PIN (passes on the base branch). The last-method guard is the one
// thing on this surface whose failure loses an account permanently, so it is
// pinned against the new not-bound guard being ordered ahead of it or folded
// into it.
func TestRemoveLastWalletAuthIsRefused(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "last-method")
				if result := addWalletThroughAddAuth(t, ctx, clientSession, chain.newSigner(t)); result.Error != nil {
					t.Fatalf("add refused: %s", result.Error.Message)
				}
				// leave the wallet as the only method
				if err := RemoveAuth(ctx, userId, "email"); err != nil {
					t.Fatalf("removing the email failed: %v", err)
				}

				err := RemoveAuth(ctx, userId, chain.authType)
				if err == nil {
					t.Fatal("the account's last auth method was removed, locking the user out")
				}
				if !strings.Contains(err.Error(), "cannot remove your last auth method") {
					t.Fatalf("refusal = %q", err)
				}
				if count := walletRowCount(t, ctx, userId); count != 1 {
					t.Fatalf("wallet rows = %d after a refused last-method removal", count)
				}
			})
		})
	}
}

// --- A11 ------------------------------------------------------------------

// A wallet-only account can free its wallet by generating a seedphrase, with
// no email or phone involved. This is the escape the swap refusal's wording
// now names (A5 asserts the wording; this asserts the wording is TRUE).
func TestSeedphraseUnlocksRemovingAWalletOnlyAccountsWallet(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "seed-unlock")
				if result := addWalletThroughAddAuth(t, ctx, clientSession, chain.newSigner(t)); result.Error != nil {
					t.Fatalf("add refused: %s", result.Error.Message)
				}
				if err := RemoveAuth(ctx, userId, "email"); err != nil {
					t.Fatalf("removing the email failed: %v", err)
				}
				if err := RemoveAuth(ctx, userId, chain.authType); err == nil {
					t.Fatal("a wallet-only account removed its wallet")
				}

				if _, err := GenerateSeedphrase(ctx, userId); err != nil {
					t.Fatalf("GenerateSeedphrase: %v", err)
				}

				if err := RemoveAuth(ctx, userId, chain.authType); err != nil {
					t.Fatalf(
						"a seedphrase did not unlock removing the wallet (%v) -- the swap "+
							"refusal tells users it will",
						err,
					)
				}
				authTypes := authTypesOf(t, ctx, userId)
				if len(authTypes) != 1 || authTypes[0] != "seedphrase" {
					t.Fatalf("auth_types = %#v, want exactly [seedphrase]", authTypes)
				}
			})
		})
	}
}

// --- A12 ------------------------------------------------------------------

// WITNESS. Fails on the base branch (yields "solana").
//
// RemoveAuth reconciles the legacy network_user.auth_type scalar after a
// removal. It used to classify the chain with its own SQL
// (CASE WHEN upper(blockchain) = 'TAO' ... ELSE 'solana'), a second rule that
// recognised one of ParseBlockchain's two Bittensor spellings and silently
// answered 'solana' for the other.
//
// The non-canonical 'bittensor' label is the shape MigrateNetworkUserChildAuths
// could write, which is exactly why addWalletAuthInTx's conflict pre-check
// deliberately does not filter on blockchain. writeWalletAuthRowForTest plants
// it the way the migration would.
func TestRemoveAuthReconcilesScalarAuthTypeForNonCanonicalBlockchain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, _, _ := authParityAccount(t, ctx, "legacy-label")
		signer := newBittensorAcceptanceWalletSigner(t)
		// the legacy label, not the canonical 'TAO'
		writeWalletAuthRowForTest(t, ctx, userId, signer.address, "bittensor")

		// auth_types[] re-derives on every read and was always right
		if !hasAuthType(authTypesOf(t, ctx, userId), AuthTypeBittensor) {
			t.Fatal("auth_types did not report the legacy-labelled row as bittensor")
		}

		if err := RemoveAuth(ctx, userId, "email"); err != nil {
			t.Fatalf("removing the email failed: %v", err)
		}

		if scalar := networkUserAuthTypeScalar(t, ctx, userId); scalar != AuthTypeBittensor {
			t.Fatalf(
				"network_user.auth_type = %q after removing the other method, want %q: "+
					"the reconcile is classifying the chain with a rule that does not accept "+
					"every spelling ParseBlockchain does",
				scalar, AuthTypeBittensor,
			)
		}
	})
}

// --- A13 ------------------------------------------------------------------

// There are two auth-type vocabularies. network_user.auth_type (the scalar) can
// hold 'password', 'bringyour' or 'guest'; auth_types[] holds only the values
// this endpoint accepts. The iOS client reads auth_types[] on the happy path
// but FALLS BACK to the scalar against an old server, so a legacy account can
// reach here with a value no branch handles.
//
// The refusal must name the surface that carries usable values instead of a
// bare "unknown auth type".
func TestRemoveAuthRejectsScalarVocabularyValues(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, _, clientSession := authParityAccount(t, ctx, "vocabulary")
		if result := addWalletThroughAddAuth(t, ctx, clientSession, newBittensorAcceptanceWalletSigner(t)); result.Error != nil {
			t.Fatalf("add refused: %s", result.Error.Message)
		}

		for _, authType := range []string{"password", "guest", "bringyour", "", "SOLANA"} {
			err := RemoveAuth(ctx, userId, authType)
			if err == nil {
				t.Fatalf("RemoveAuth(%q) succeeded", authType)
			}
			if !strings.Contains(err.Error(), "auth_types") {
				t.Fatalf(
					"RemoveAuth(%q) = %q; the refusal should point the client at the "+
						"auth_types list that carries values it can send",
					authType, err,
				)
			}
			// nothing may have been deleted along the way
			if count := walletRowCount(t, ctx, userId); count != 1 {
				t.Fatalf("RemoveAuth(%q) deleted the wallet", authType)
			}
			if !hasAuthType(authTypesOf(t, ctx, userId), "email") {
				t.Fatalf("RemoveAuth(%q) deleted the email", authType)
			}
		}
	})
}

// --- A14 ------------------------------------------------------------------

// WITNESS at the model layer. Fails on the base branch (returns nil, nil).
//
// AddAuth fell past all three branches and returned (nil, nil) for a body that
// named no method. The wire consequence -- HTTP 200 with the body `null` -- is
// only visible at the handler layer, and is asserted there
// (api/handlers/auth_add_remove_status_test.go). This half pins the model
// contract that makes that possible: a refusal is a result with an Error, never
// a nil result.
func TestAddAuthWithNoRecognizedMethodIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "no-method")

		userAuth := "someone@example.com"
		authJwt := "not-a-real-token"

		cases := map[string]AddAuthMethod{
			"empty body":                   {},
			"user_auth without a password": {UserAuth: &userAuth},
			// the `&&` in AddAuth's second branch means a JWT with no type
			// matches nothing at all
			"auth_jwt without a type": {AuthJwt: &authJwt},
		}
		for name, args := range cases {
			result, err := AddAuth(args, clientSession)
			if err != nil {
				t.Fatalf("%s: AddAuth returned a transport error: %v", name, err)
			}
			if result == nil {
				t.Fatalf(
					"%s: AddAuth returned (nil, nil). The router marshals that typed nil "+
						"as the JSON literal `null` with HTTP 200, which a status-checking "+
						"client reads as success",
					name,
				)
			}
			if result.Error == nil {
				t.Fatalf("%s: AddAuth reported success for a body naming no auth method", name)
			}
			assertNoStatusPrefix(t, name, result.Error.Message)
		}
	})
}

// --- A15 ------------------------------------------------------------------

// Email onto a wallet-only account, and the two ways adding an email is
// refused. This is the other half of the user's ask -- a wallet user adding a
// conventional sign-in method -- and it is run for both chains because the
// account it starts from is wallet-only.
func TestAddAuthEmailOntoWalletOnlyAccount(t *testing.T) {
	for _, chain := range walletChains {
		t.Run(chain.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, clientSession := authParityAccount(t, ctx, "wallet-email")
				if result := addWalletThroughAddAuth(t, ctx, clientSession, chain.newSigner(t)); result.Error != nil {
					t.Fatalf("add refused: %s", result.Error.Message)
				}
				if err := RemoveAuth(ctx, userId, "email"); err != nil {
					t.Fatalf("removing the seeded email failed: %v", err)
				}

				email := fmt.Sprintf("wallet-user-%s@example.com", strings.ToLower(server.NewId().String()[:8]))
				password := "SomeValidPassword123!"

				result, err := AddAuth(AddAuthMethod{UserAuth: &email, Password: &password}, clientSession)
				if err != nil {
					t.Fatal(err)
				}
				if result.Error != nil {
					t.Fatalf("adding an email to a %s-only account was refused: %s", chain.name, result.Error.Message)
				}
				authTypes := authTypesOf(t, ctx, userId)
				if !hasAuthType(authTypes, "email") || !hasAuthType(authTypes, chain.authType) {
					t.Fatalf("auth_types = %#v, want both email and %s", authTypes, chain.authType)
				}

				// the same address again: this account already has an email
				again, err := AddAuth(AddAuthMethod{UserAuth: &email, Password: &password}, clientSession)
				if err != nil {
					t.Fatal(err)
				}
				if again.Error == nil {
					t.Fatal("the same email was added to the same account twice")
				}
				assertNoStatusPrefix(t, "duplicate email", again.Error.Message)

				// an address already bound to a DIFFERENT account. That
				// account's own seeded email is used directly: it already holds
				// one, and adding a SECOND email to it is a different refusal
				// ("User exists with auth type email") that would not exercise
				// the cross-account availability check this case is aiming at.
				_, otherEmail, _ := authParityAccount(t, ctx, "email-owner")
				conflict, err := AddAuth(AddAuthMethod{UserAuth: &otherEmail, Password: &password}, clientSession)
				if err != nil {
					t.Fatal(err)
				}
				if conflict.Error == nil {
					t.Fatal("an email bound to another account was bound a second time")
				}
				assertNoStatusPrefix(t, "email taken", conflict.Error.Message)
			})
		})
	}
}

// --- A16 ------------------------------------------------------------------

// A short password is refused before anything is written, and the refusal is
// the structured body shape -- not a status error and not a bare nil.
//
// SCOPE NOTE: the SSO half of AddAuth cannot be driven end to end from a test.
// ParseAuthJwt requires a genuinely signed Apple/Google token, so a test can
// only reach its refusal path (asserted here) or bypass it via addSsoAuth
// (used by A9 to plant rows). The remove side of SSO is covered by A9.
func TestAddAuthRefusesMalformedCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, _, clientSession := authParityAccount(t, ctx, "malformed")
		before := authTypesOf(t, ctx, userId)

		email := "short-password@example.com"
		short := "abc"
		result, err := AddAuth(AddAuthMethod{UserAuth: &email, Password: &short}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Error == nil {
			t.Fatal("a password below the minimum length was accepted")
		}
		assertNoStatusPrefix(t, "short password", result.Error.Message)

		// an auth_jwt that is not a real signed provider token
		authJwt := "not-a-real-google-token"
		authJwtType := "google"
		ssoResult, err := AddAuth(AddAuthMethod{AuthJwt: &authJwt, AuthJwtType: &authJwtType}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if ssoResult.Error == nil {
			t.Fatal("an unsigned SSO token was accepted")
		}
		assertNoStatusPrefix(t, "bad sso token", ssoResult.Error.Message)

		if after := authTypesOf(t, ctx, userId); len(after) != len(before) {
			t.Fatalf("auth_types went from %#v to %#v across two refusals", before, after)
		}
	})
}
