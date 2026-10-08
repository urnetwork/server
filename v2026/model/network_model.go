package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	// "github.com/urnetwork/glog/v2026"

	goaway "github.com/TwiN/go-away"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	bip39 "github.com/tyler-smith/go-bip39"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"

	// "github.com/urnetwork/server/v2026/ulid"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/search"
)

func init() {
	server.OnWarmup(server.WarmupTargetNetworkNameSearch, func() {
		networkNameSearch()
		//.WaitForInitialSync(context.Background())
	})
	server.OnReset(func() {
		networkNameSearch().Close()
		networkNameSearch = sync.OnceValue(createNetworkNameSearch)
	})
}

// The database index of the network name search.
func newNetworkNameSearchDb() *search.SearchDb {
	return search.NewSearchDb("network_name", search.SearchTypeFull)
}

func createNetworkNameSearch() *search.SearchLocal {
	return search.NewSearchLocalWithDefaults(
		context.Background(),
		newNetworkNameSearchDb(),
	)
}

var networkNameSearch = sync.OnceValue(createNetworkNameSearch)

// Testing_InMemoryNetworkNameSearch replaces the network name search with one
// whose in-memory index changes only through the writes a test makes: its
// context is canceled before it starts, so its background load fails at once
// and it never loads or polls the database index, and it answers queries from
// memory. Its writes still reach the database index. Returns the search, and
// the func that restores the previous one.
func Testing_InMemoryNetworkNameSearch(ctx context.Context) (inMemorySearch *search.SearchLocal, restore func()) {
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	inMemorySearch = search.NewSearchLocalWithDefaults(canceledCtx, newNetworkNameSearchDb())
	previousNetworkNameSearch := networkNameSearch
	networkNameSearch = func() *search.SearchLocal {
		return inMemorySearch
	}
	restore = func() {
		networkNameSearch = previousNetworkNameSearch
	}
	return
}

const MinPasswordLength = 6

type NetworkCheckArgs struct {
	NetworkName string `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool `json:"available"`
}

type NetworkCreateError = string

const (
	AgreeToTerms NetworkCreateError = "The terms of service and privacy policy must be accepted."
)

func NetworkCheck(check *NetworkCheckArgs, session *session.ClientSession) (*NetworkCheckResult, error) {

	_, err := ValidateNetworkName(check.NetworkName)
	if err != nil {
		return &NetworkCheckResult{
			Available: false,
		}, nil
	}

	taken := networkNameSearch().AnyAround(session.Ctx, check.NetworkName, 1)

	result := &NetworkCheckResult{
		Available: !taken,
	}
	return result, nil
}

type NetworkCreateArgs struct {
	UserName         string          `json:"user_name"`
	UserAuth         *string         `json:"user_auth,omitempty"`
	AuthJwt          *string         `json:"auth_jwt,omitempty"`
	AuthJwtType      *string         `json:"auth_jwt_type,omitempty"`
	Password         *string         `json:"password,omitempty"`
	NetworkName      string          `json:"network_name"`
	Terms            bool            `json:"terms"`
	VerifyUseNumeric bool            `json:"verify_use_numeric"`
	ReferralCode     *string         `json:"referral_code,omitempty"`
	BalanceCode      *string         `json:"balance_code,omitempty"`
	WalletAuth       *WalletAuthArgs `json:"wallet_auth,omitempty"`
	// ProductUpdates is the sign-up form's "Periodic product updates" line.
	// Absent = on (the line ships ticked); false turns the preference off from
	// the first moment, before any campaign mail can go out.
	ProductUpdates *bool `json:"product_updates,omitempty"`
	// answer a coded refusal (`NetworkCreateResultError.Code`) with the result
	// as the body of its HTTP status (401), instead of the plain text message
	// older clients expect; the status stays a refusal either way
	ResultErrors bool `json:"result_errors,omitempty"`
}

type NetworkCreateResult struct {
	Network              *NetworkCreateResultNetwork      `json:"network,omitempty"`
	UserAuth             *string                          `json:"user_auth,omitempty"`
	Seedphrase           *string                          `json:"seedphrase,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error                *NetworkCreateResultError        `json:"error,omitempty"`
	IsPro                bool                             `json:"is_pro,omitempty"`
	// SuppressAccountMessages is server-internal. Dedicated acceptance auth
	// identities can authenticate immediately without causing verification or
	// welcome-message sends from production infrastructure.
	SuppressAccountMessages bool `json:"-"`
}

type NetworkCreateResultNetwork struct {
	ByJwt       *string   `json:"by_jwt,omitempty"`
	NetworkId   server.Id `json:"network_id,omitempty"`
	NetworkName string    `json:"network_name,omitempty"`
	IsPro       bool      `json:"is_pro,omitempty"`
}

type NetworkCreateResultVerification struct {
	UserAuth string `json:"user_auth"`
	// set when no code was sent for this verification (rate limited or the
	// send failed). Clients that predate the field ignore it.
	SendError *AuthVerifySendError `json:"send_error,omitempty"`
}

type NetworkCreateResultError struct {
	// `WalletAuthErrorCodeSignatureMismatch` for a wallet signature that does
	// not verify for its address, "" for every other refusal. Only a coded
	// refusal reaches a client that asked for `result_errors` as a result, in
	// the body of its refusal status.
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	// Only an explicit input/duplicate refusal may become a client status.
	// Internal and ambiguous creation failures retain the existing 500 path.
	// This marker is server-internal; the model's JSON/message contract stays
	// unchanged, and client input cannot set the classification.
	refusalStatus int
}

// Preserve the existing numeric-prefix HTTP error transport without changing JSON.
func (self *NetworkCreateResultError) Error() string {
	switch self.refusalStatus {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusConflict:
		return fmt.Sprintf("%d %s", self.refusalStatus, self.Message)
	}
	return self.Message
}

func ValidateNetworkName(networkName string) (string, error) {
	trimmed := strings.TrimSpace(networkName)

	// to lowercase
	normalized := strings.ToLower(trimmed)

	// replace spaces with underscores
	normalized = strings.ReplaceAll(normalized, " ", "-")

	// ensure length is at least 5 characters
	if len(normalized) < 5 {
		return "", errors.New("Network name must have at least 5 characters")
	}

	// ensure length is less than 50 characters
	if len(normalized) > 50 {
		return "", errors.New("Network name must be less than 50 characters")
	}

	// ensure ASCII characters only
	for _, char := range normalized {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
			return "", errors.New("Network name must contain only lowercase letters, numbers, and dashes")
		}
	}

	return normalized, nil
}

// Reject only explicit request-shape errors here. A nonempty token for a
// supported provider still goes through ParseAuthJwt below the limiter; a
// verification, key-fetch, or provider failure is not classified by its text.
func networkCreateAuthShapeError(networkCreate NetworkCreateArgs) *NetworkCreateResultError {
	message := ""
	switch {
	case networkCreate.UserAuth != nil:
		// Keep the existing email/phone branch precedence and password policy.
		// A missing pointer would otherwise panic in networkCreateUserAuth.
		if networkCreate.Password == nil {
			message = "Password is required."
		}
	case networkCreate.AuthJwt != nil && (networkCreate.AuthJwtType != nil || networkCreate.WalletAuth == nil):
		// A complete SSO pair precedes wallet auth. Preserve the existing
		// wallet selection when only an incomplete, unused SSO field is set.
		switch {
		case networkCreate.AuthJwtType == nil:
			message = "Authentication type is required."
		case strings.TrimSpace(*networkCreate.AuthJwt) == "":
			message = "Authentication token is required."
		case AuthType(*networkCreate.AuthJwtType) != AuthTypeApple && AuthType(*networkCreate.AuthJwtType) != AuthTypeGoogle:
			message = "Unsupported authentication type."
		}
	case networkCreate.WalletAuth != nil:
		// Unused fields on an explicitly selected wallet method do not change
		// its existing validation or challenge semantics.
	case networkCreate.Password != nil:
		message = "Email or phone number is required for password signup."
	case networkCreate.AuthJwtType != nil:
		message = "Authentication token is required."
	}
	if message == "" {
		return nil
	}
	return &NetworkCreateResultError{Message: message, refusalStatus: http.StatusBadRequest}
}

func NetworkCreate(
	networkCreate NetworkCreateArgs,
	session *session.ClientSession,
) (*NetworkCreateResult, error) {
	userAuth, _ := NormalUserAuthV1(networkCreate.UserAuth)

	seedphraseSignup := networkCreate.UserAuth == nil && networkCreate.AuthJwt == nil && networkCreate.WalletAuth == nil
	// Orphan credential fields are not a request for a seedphrase account.
	// Keep terms refusal precedence and reject before selecting that path or
	// spending its independent daily budget.
	if seedphraseSignup && networkCreate.Terms {
		if shapeError := networkCreateAuthShapeError(networkCreate); shapeError != nil {
			return &NetworkCreateResult{Error: shapeError}, nil
		}
	}

	// seedphrase path: no auth method provided
	if seedphraseSignup {

		if !networkCreate.Terms {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message:       AgreeToTerms,
					refusalStatus: http.StatusBadRequest,
				},
			}
			return result, nil
		}

		// rate limit: max 5 seedphrase accounts per IP per day
		if err := CheckNetworkCreateRateLimit(session.Ctx, session); err != nil {
			return nil, err
		}

		validatedNetworkName, err := generateRandomNetworkName()
		if err != nil {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Failed to generate network name.",
				},
			}
			return result, nil
		}

		resultNetworkCreate := networkCreateSeedphrase(
			session.Ctx,
			&networkCreate,
			validatedNetworkName,
		)

		if resultNetworkCreate.Created {
			auditNetworkCreate(networkCreate, resultNetworkCreate.NetworkId, session)

			isPro := false
			byJwt := jwt.NewByJwt(
				resultNetworkCreate.NetworkId,
				resultNetworkCreate.UserId,
				validatedNetworkName,
				false,
				isPro,
			)
			byJwtSigned := byJwt.Sign()
			result := &NetworkCreateResult{
				Seedphrase: &resultNetworkCreate.Seedphrase,
				Network: &NetworkCreateResultNetwork{
					ByJwt:       &byJwtSigned,
					NetworkName: validatedNetworkName,
					NetworkId:   resultNetworkCreate.NetworkId,
					IsPro:       resultNetworkCreate.IsPro,
				},
			}
			return result, nil
		} else {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Account might already exist. Please start over.",
				},
			}
			return result, nil
		}
	}

	// INPUT VALIDATION RUNS FIRST, BEFORE ANY LIMITER IS CHARGED.
	//
	// UserAuthAttempt used to be consumed here, above every check below. That
	// meant an unticked terms box, a network name that was too short, or a
	// network name someone else already has each burned one of five slots in a
	// five-minute window -- and for a signup with no user auth (SSO, wallet)
	// those slots are shared by everyone at the client address, so ordinary
	// form mistakes refused strangers. Four mistakes and the user's own
	// corrected submission was refused too. A form mistake must not spend a
	// shared budget.
	//
	// Nothing between here and the limiter WRITES. ValidateNetworkName and the
	// user-auth normalisation are pure; checkNetworkNameAvailability is two
	// reads. Everything that creates or mutates state -- networkCreateUserAuth,
	// ParseAuthJwt, networkCreateAuthJwt, UseWalletAuthChallenge,
	// networkCreateWalletAuth, the search index Add, auditNetworkCreate, JWT
	// minting -- stays below the limiter.
	//
	// Be precise about what the reorder does hand out unmetered, because the
	// obvious sentence ("it is the same lookup /auth/network-check already
	// serves") is not quite true and this comment is load-bearing for anyone
	// moving code across the limiter later. checkNetworkNameAvailability is a
	// SUPERSET of NetworkCheck: both run the fuzzy networkNameSearch, and it
	// adds an exact `SELECT network_id FROM network WHERE network_name = $1`
	// inside a transaction. So the reorder does widen the free oracle slightly
	// -- exact-name existence for names the fuzzy search misses -- and costs
	// one extra unmetered read transaction per unauthenticated request. That is
	// acceptable because POST /auth/network-check (api/api.go) is already
	// unauthenticated, unlimited and DB-backed, so no new capability appears;
	// it is not acceptable as licence to move anything else up here.

	if !networkCreate.Terms {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message:       AgreeToTerms,
				refusalStatus: http.StatusBadRequest,
			},
		}
		return result, nil
	}

	validatedNetworkName, error := ValidateNetworkName(networkCreate.NetworkName)

	if error != nil {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message:       error.Error(),
				refusalStatus: http.StatusBadRequest,
			},
		}
		return result, nil
	}

	// check if the network name is already taken
	err := checkNetworkNameAvailability(validatedNetworkName, session)
	if err != nil {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message:       err.Error(),
				refusalStatus: http.StatusConflict,
			},
		}
		return result, nil
	}

	// an unparseable email or phone number is a form mistake like any other,
	// so it is refused here rather than after the limiter below
	if networkCreate.UserAuth != nil && userAuth == nil {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message:       "Invalid email or phone number.",
				refusalStatus: http.StatusBadRequest,
			},
		}
		return result, nil
	}

	if shapeError := networkCreateAuthShapeError(networkCreate); shapeError != nil {
		return &NetworkCreateResult{Error: shapeError}, nil
	}

	containsProfanity := goaway.IsProfane(validatedNetworkName)

	// email/SSO/wallet paths use the existing auth rate limit. The input is
	// valid by this point, so a slot is only ever spent on a submission that
	// would otherwise create an account.
	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError(userAuth)
	}

	if networkCreate.UserAuth != nil {
		// user is creating a network via email/phone + pass
		// validate the user does not exist
		testAuthPolicy := testAuthPolicyForUserAuth(userAuth)

		resultNetworkCreate := networkCreateUserAuth(
			session.Ctx,
			&networkCreate,
			userAuth,
			validatedNetworkName,
			containsProfanity,
			testAuthPolicy.BypassVerification,
		)

		if resultNetworkCreate.Created {
			auditNetworkCreate(networkCreate, resultNetworkCreate.NetworkId, session)

			networkNameSearch().Add(session.Ctx, validatedNetworkName, resultNetworkCreate.NetworkId, 0)

			if testAuthPolicy.BypassVerification {
				SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

				byJwt := jwt.NewByJwt(
					resultNetworkCreate.NetworkId,
					resultNetworkCreate.UserId,
					validatedNetworkName,
					false,
					resultNetworkCreate.IsPro,
				)
				byJwtSigned := byJwt.Sign()
				return &NetworkCreateResult{
					Network: &NetworkCreateResultNetwork{
						ByJwt:       &byJwtSigned,
						NetworkName: validatedNetworkName,
						NetworkId:   resultNetworkCreate.NetworkId,
						IsPro:       resultNetworkCreate.IsPro,
					},
					UserAuth:                userAuth,
					SuppressAccountMessages: testAuthPolicy.SuppressAccountMessages,
				}, nil
			}

			result := &NetworkCreateResult{
				VerificationRequired: &NetworkCreateResultVerification{
					UserAuth: *userAuth,
				},
				Network: &NetworkCreateResultNetwork{
					NetworkName: networkCreate.NetworkName,
					NetworkId:   resultNetworkCreate.NetworkId,
					IsPro:       resultNetworkCreate.IsPro,
				},
			}
			return result, nil
		} else {
			message := "Account might already exist. Please start over."
			if resultNetworkCreate.refusalMessage != "" {
				message = resultNetworkCreate.refusalMessage
			}
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message:       message,
					refusalStatus: resultNetworkCreate.refusalStatus,
				},
			}
			return result, nil
		}
	} else if networkCreate.AuthJwt != nil && networkCreate.AuthJwtType != nil {
		// user is creating a network via social login

		authJwt := parseSsoAuthJwt("network create", *networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))

		if authJwt != nil {

			normalJwtUserAuth, _ := NormalUserAuth(authJwt.UserAuth)

			resultNetworkCreate := networkCreateAuthJwt(
				session.Ctx,
				&networkCreate,
				containsProfanity,
				*authJwt,
				validatedNetworkName,
				normalJwtUserAuth,
			)

			if resultNetworkCreate.Created {
				auditNetworkCreate(networkCreate, resultNetworkCreate.NetworkId, session)

				networkNameSearch().Add(session.Ctx, validatedNetworkName, resultNetworkCreate.NetworkId, 0)

				SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

				guestMode := false

				// successful login
				byJwt := jwt.NewByJwt(
					resultNetworkCreate.NetworkId,
					resultNetworkCreate.UserId,
					networkCreate.NetworkName,
					guestMode, // false
					resultNetworkCreate.IsPro,
				)
				byJwtSigned := byJwt.Sign()
				result := &NetworkCreateResult{
					Network: &NetworkCreateResultNetwork{
						ByJwt:       &byJwtSigned,
						NetworkName: networkCreate.NetworkName,
						NetworkId:   resultNetworkCreate.NetworkId,
						IsPro:       resultNetworkCreate.IsPro,
					},
					UserAuth: &authJwt.UserAuth,
				}
				return result, nil
			} else {
				message := "Account might already exist. Please log in again."
				if resultNetworkCreate.refusalMessage != "" {
					message = resultNetworkCreate.refusalMessage
				}
				result := &NetworkCreateResult{
					Error: &NetworkCreateResultError{
						Message:       message,
						refusalStatus: resultNetworkCreate.refusalStatus,
					},
				}
				return result, nil
			}
		}
	} else if networkCreate.WalletAuth != nil {

		/**
		 * User is authenticating with a crypto wallet
		 */

		/**
		 * default empty blockchain to solana
		 */
		if networkCreate.WalletAuth.Blockchain == "" {
			networkCreate.WalletAuth.Blockchain = SOL.String()
		}

		parsedBlockchain, err := ParseBlockchain(networkCreate.WalletAuth.Blockchain)
		if err != nil {
			return &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "400 unsupported blockchain for wallet authentication",
				},
			}, nil
		}
		// Wallet authentication supports Solana and Bittensor (TAO) for
		// network creation. Note this does NOT make Bittensor eligible for
		// a payout wallet below - payouts remain Solana/Polygon (USDC) only.
		if parsedBlockchain != SOL && parsedBlockchain != TAO {
			return &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "400 unsupported blockchain for wallet authentication",
				},
			}, nil
		}
		networkCreate.WalletAuth.Blockchain = parsedBlockchain.String()

		/**
		 * validate the wallet challenge
		 */
		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: networkCreate.WalletAuth.Blockchain,
			PublicKey:  networkCreate.WalletAuth.PublicKey,
			Message:    networkCreate.WalletAuth.Message,
			Signature:  networkCreate.WalletAuth.Signature,
		}, session.Ctx)
		if err != nil {
			return nil, err
		}
		if useResult.SignatureMismatch {
			// coded, so the apps can say the wallet signed with another
			// account; a client without `result_errors` gets the 401
			return &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Code:          WalletAuthErrorCodeSignatureMismatch,
					Message:       walletAuthSignatureMismatchMessage,
					refusalStatus: http.StatusUnauthorized,
				},
			}, nil
		}
		if !useResult.Valid {
			msg := "400 invalid wallet challenge"
			if useResult.Error != nil {
				msg = useResult.Error.Message
			}
			return &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: msg,
				},
			}, nil
		}

		networkCreateResult, err := networkCreateWalletAuth(
			session.Ctx,
			&networkCreate,
			validatedNetworkName,
			containsProfanity,
		)
		if err != nil {
			return &NetworkCreateResult{
				Error: &NetworkCreateResultError{Message: err.Error()},
			}, nil
		}

		if networkCreateResult.Created {

			auditNetworkCreate(networkCreate, networkCreateResult.NetworkId, session)

			networkNameSearch().Add(session.Ctx, validatedNetworkName, networkCreateResult.NetworkId, 0)

			SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

			/**
			 * Create new payout wallet
			 */

			if networkCreate.WalletAuth.Blockchain == SOL.String() || networkCreate.WalletAuth.Blockchain == MATIC.String() {
				/**
				 * since we only support payouts on solana and polygon for now
				 * only create payout wallets for those blockchains
				 */

				walletId := CreateAccountWalletExternal(
					session,
					&CreateAccountWalletExternalArgs{
						NetworkId:        networkCreateResult.NetworkId,
						Blockchain:       networkCreate.WalletAuth.Blockchain,
						WalletAddress:    networkCreate.WalletAuth.PublicKey,
						DefaultTokenType: "USDC",
					},
				)

				/**
				 * Set the payout wallet for the network
				 */
				if walletId != nil {
					err := SetPayoutWallet(
						session.Ctx,
						networkCreateResult.NetworkId,
						*walletId,
					)
					if err != nil {
						glog.Errorf("[net]could not set payout wallet for network %s: %s\n", networkCreateResult.NetworkId, err)
					}
				} else {
					glog.Errorf("[net]could not create payout wallet for network %s\n", networkCreateResult.NetworkId)
				}
			}

			isGuest := false

			// successful login
			byJwt := jwt.NewByJwt(
				networkCreateResult.NetworkId,
				networkCreateResult.UserId,
				networkCreate.NetworkName,
				isGuest,
				networkCreateResult.IsPro,
			)
			byJwtSigned := byJwt.Sign()
			result := &NetworkCreateResult{
				Network: &NetworkCreateResultNetwork{
					ByJwt:       &byJwtSigned,
					NetworkName: networkCreate.NetworkName,
					NetworkId:   networkCreateResult.NetworkId,
					IsPro:       networkCreateResult.IsPro,
				},
			}
			return result, nil
		} else {
			message := "Account might already exist. Please log in again."
			if networkCreateResult.refusalMessage != "" {
				message = networkCreateResult.refusalMessage
			}
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message:       message,
					refusalStatus: networkCreateResult.refusalStatus,
				},
			}
			return result, nil
		}

	}

	return nil, errors.New("invalid login")
}

type networkCreateResult struct {
	Created     bool
	NetworkId   server.Id
	NetworkName string
	UserId      server.Id
	Seedphrase  string
	IsPro       bool
	// Created=false alone also covers internal helper failures; only a
	// completed existing-account/name check establishes a client refusal.
	refusalStatus  int
	refusalMessage string
}

/**
 * network create wallet auth
 */
func networkCreateWalletAuth(
	ctx context.Context,
	networkCreate *NetworkCreateArgs,
	validatedNetworkName string,
	containsProfanity bool,
) (networkCreateResult, error) {
	if err := validateWalletAuth(networkCreate.WalletAuth); err != nil {
		return networkCreateResult{}, err
	}

	created := false
	refusalStatus := 0
	refusalMessage := ""
	var createdNetworkId server.Id
	var createdUserId server.Id
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		created = false
		refusalStatus = 0
		refusalMessage = ""
		isPro = false
		var userId *server.Id

		result, err := tx.Query(
			ctx,
			`
				SELECT user_id FROM network_user WHERE wallet_address = $1
			`,
			networkCreate.WalletAuth.PublicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userId))
			}
		})

		if userId != nil {
			glog.Infof("Network user already exists with this wallet address")
			refusalStatus = http.StatusConflict
			return
		}

		// The current writers mirror wallet ownership in network_user and
		// network_user_auth_wallet, but legacy or partially repaired rows may
		// exist only in the child table. Detect that shape before inserting a
		// new user so the transaction returns the same explicit client conflict
		// instead of raising an unclassified uniqueness error after the insert.
		var conflictUserId *server.Id
		result, err = tx.Query(
			ctx,
			`
				SELECT user_id FROM network_user_auth_wallet WHERE wallet_address = $1
			`,
			networkCreate.WalletAuth.PublicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&conflictUserId))
			}
		})
		if conflictUserId != nil {
			refusalStatus = http.StatusConflict
			refusalMessage = "This wallet is already linked to another account."
			return
		}

		// NetworkCreate checked the name before this transaction (see
		// networkNameHeldInTx)
		if networkNameHeldInTx(ctx, tx, validatedNetworkName) {
			refusalStatus = http.StatusConflict
			refusalMessage = networkNameNotAvailableMessage
			return
		}

		createdUserId = server.NewId()
		createdNetworkId = server.NewId()

		_, err = tx.Exec(
			ctx,
			`
				INSERT INTO network_user
				(user_id, auth_type, wallet_address, wallet_blockchain, user_name)
				VALUES ($1, $2, $3, $4, $5)
			`,
			createdUserId,
			walletAuthType(networkCreate.WalletAuth.Blockchain),
			networkCreate.WalletAuth.PublicKey,
			networkCreate.WalletAuth.Blockchain,
			networkCreate.UserName,
		)
		if err != nil {
			panic(err)
		}

		// insert into network_user_auth_wallet
		server.Raise(addWalletAuthInTx(
			tx,
			&AddWalletAuthArgs{
				WalletAuth: &WalletAuthArgs{
					PublicKey:  networkCreate.WalletAuth.PublicKey,
					Blockchain: networkCreate.WalletAuth.Blockchain,
					Message:    networkCreate.WalletAuth.Message,
					Signature:  networkCreate.WalletAuth.Signature,
				},
				UserId: createdUserId,
			},
			ctx,
		))

		_, err = tx.Exec(
			ctx,
			`
				INSERT INTO network
				(network_id, network_name, admin_user_id, contains_profanity)
				VALUES ($1, $2, $3, $4)
			`,
			createdNetworkId,
			validatedNetworkName,
			createdUserId,
			containsProfanity,
		)
		if err != nil {
			panic(err)
		}

		CreateNetworkReferralCodeInTx(ctx, tx, createdNetworkId)

		isPro = networkCreateRedeemBalanceCodeInTx(
			networkCreate,
			createdNetworkId,
			ctx,
			tx,
		)

		created = true
	})

	return networkCreateResult{
		Created:        created,
		NetworkId:      createdNetworkId,
		NetworkName:    networkCreate.NetworkName,
		UserId:         createdUserId,
		IsPro:          isPro,
		refusalStatus:  refusalStatus,
		refusalMessage: refusalMessage,
	}, nil

}

/**
 * network create authjwt (social login)
 */

func networkCreateAuthJwt(
	ctx context.Context,
	networkCreate *NetworkCreateArgs,
	containsProfanity bool,
	parsedAuthJwt AuthJwt,
	validatedNetworkName string,
	normalizedUserAuth string,
) networkCreateResult {

	created := false
	refusalStatus := 0
	refusalMessage := ""
	var createdNetworkId server.Id
	var createdUserId server.Id
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		created = false
		refusalStatus = 0
		refusalMessage = ""
		isPro = false
		var userId *server.Id

		result, err := tx.Query(
			ctx,
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			normalizedUserAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userId))
			}
		})

		if userId != nil {
			// server.Logger().Printf("User already exists\n")
			refusalStatus = http.StatusConflict
			return
		}

		createdUserId = server.NewId()
		createdNetworkId = server.NewId()

		// Another user can hold the identity in a child table only, without it
		// on their network_user row (a sign-in or an email added to an existing
		// account). addSsoAuthInTx would refuse it after the user is written,
		// so it is refused here, before anything is written.
		if validateUserAuthAvailability(ctx, tx, normalizedUserAuth, createdUserId) != nil {
			refusalStatus = http.StatusConflict
			return
		}

		// NetworkCreate checked the name before this transaction (see
		// networkNameHeldInTx)
		if networkNameHeldInTx(ctx, tx, validatedNetworkName) {
			refusalStatus = http.StatusConflict
			refusalMessage = networkNameNotAvailableMessage
			return
		}

		_, err = tx.Exec(
			ctx,
			`
				INSERT INTO network_user
				(user_id, user_name, auth_type, user_auth, auth_jwt)
				VALUES ($1, $2, $3, $4, $5)
			`,
			createdUserId,
			networkCreate.UserName,
			parsedAuthJwt.AuthType,
			normalizedUserAuth,
			networkCreate.AuthJwt,
		)
		if err != nil {
			panic(err)
		}

		// insert into network_user_auth_sso. Its availability check passed
		// above in this snapshot, so a refusal here raises: returning it would
		// commit the user without its sign-in.
		server.Raise(addSsoAuthInTx(
			tx,
			ctx,
			&AddSsoAuthArgs{
				UserId:        createdUserId,
				AuthJwt:       *networkCreate.AuthJwt,
				ParsedAuthJwt: parsedAuthJwt,
				AuthJwtType:   SsoAuthType(*networkCreate.AuthJwtType),
			},
		))

		_, err = tx.Exec(
			ctx,
			`
				INSERT INTO network
				(network_id, network_name, admin_user_id, contains_profanity)
				VALUES ($1, $2, $3, $4)
			`,
			createdNetworkId,
			validatedNetworkName,
			createdUserId,
			containsProfanity,
		)

		if err != nil {
			panic(err)
		}

		CreateNetworkReferralCodeInTx(ctx, tx, createdNetworkId)

		isPro = networkCreateRedeemBalanceCodeInTx(
			networkCreate,
			createdNetworkId,
			ctx,
			tx,
		)

		created = true
	})

	return networkCreateResult{
		Created:        created,
		NetworkId:      createdNetworkId,
		NetworkName:    networkCreate.NetworkName,
		UserId:         createdUserId,
		IsPro:          isPro,
		refusalStatus:  refusalStatus,
		refusalMessage: refusalMessage,
	}

}

/**
 * network create userauth
 */

func networkCreateUserAuth(
	ctx context.Context,
	networkCreate *NetworkCreateArgs,
	userAuth *string,
	validatedNetworkName string,
	containsProfanity bool,
	verified bool,
) networkCreateResult {

	created := false
	refusalStatus := 0
	refusalMessage := ""
	var createdNetworkId server.Id
	var createdUserId server.Id
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		created = false
		refusalStatus = 0
		refusalMessage = ""
		isPro = false
		var result server.PgResult
		var err error

		var userId *server.Id

		result, err = tx.Query(
			ctx,
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userId))
			}
		})

		if userId != nil {
			refusalStatus = http.StatusConflict
			return
		}

		createdUserId = server.NewId()
		createdNetworkId = server.NewId()

		// Another user can hold the user auth in a child table only, without it
		// on their network_user row (an auth added to an existing account).
		// addUserAuthInTx would refuse it after the user is written, so it is
		// refused here, before anything is written.
		if validateUserAuthAvailability(ctx, tx, *userAuth, createdUserId) != nil {
			refusalStatus = http.StatusConflict
			return
		}

		// the name as it is stored. NetworkCreate checked it before this
		// transaction (see networkNameHeldInTx).
		if networkNameHeldInTx(ctx, tx, validatedNetworkName) {
			refusalStatus = http.StatusConflict
			refusalMessage = networkNameNotAvailableMessage
			return
		}

		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(*networkCreate.Password), passwordSalt)

		// todo - cleanup network_user once UIs are updated
		_, err = tx.Exec(
			ctx,
			`
				INSERT INTO network_user
				(user_id, user_name, auth_type, user_auth, password_hash, password_salt)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			createdUserId,
			networkCreate.UserName,
			AuthTypePassword,
			userAuth,
			passwordHash,
			passwordSalt,
		)
		server.Raise(err)

		// insert into network_user_auth_password. Its checks passed above in
		// this snapshot, so a refusal here raises: ignoring it created the
		// network without its password auth.
		server.Raise(addUserAuthInTx(
			tx,
			&AddUserAuthArgs{
				UserId:       createdUserId,
				UserAuth:     userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
				Verified:     verified,
			},
			ctx,
		))

		_, err = tx.Exec(
			ctx,
			`
				INSERT INTO network
				(network_id, network_name, admin_user_id, contains_profanity)
				VALUES ($1, $2, $3, $4)
			`,
			createdNetworkId,
			validatedNetworkName,
			createdUserId,
			containsProfanity,
		)
		server.Raise(err)

		CreateNetworkReferralCodeInTx(ctx, tx, createdNetworkId)

		isPro = networkCreateRedeemBalanceCodeInTx(
			networkCreate,
			createdNetworkId,
			ctx,
			tx,
		)

		created = true
	})

	return networkCreateResult{
		Created:        created,
		NetworkId:      createdNetworkId,
		NetworkName:    networkCreate.NetworkName,
		UserId:         createdUserId,
		IsPro:          isPro,
		refusalStatus:  refusalStatus,
		refusalMessage: refusalMessage,
	}

}

/**
 * network create seedphrase
 */
func networkCreateSeedphrase(
	ctx context.Context,
	networkCreate *NetworkCreateArgs,
	validatedNetworkName string,
) networkCreateResult {
	created := false
	var createdNetworkId server.Id
	var createdUserId server.Id
	var seedphrase string
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
		createdUserId = server.NewId()
		createdNetworkId = server.NewId()

		// generate seedphrase
		entropy, err := bip39.NewEntropy(256)
		server.Raise(err)
		seedphrase, err = bip39.NewMnemonic(entropy)
		server.Raise(err)

		_, err = tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type)
			 VALUES ($1, $2, $3)`,
			createdUserId, validatedNetworkName, AuthTypeSeedphrase,
		)
		server.Raise(err)

		err = CreateSeedphraseAuthInTx(tx, ctx, createdUserId, seedphrase)
		server.Raise(err)

		_, err = tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id)
			 VALUES ($1, $2, $3)`,
			createdNetworkId, validatedNetworkName, createdUserId,
		)
		server.Raise(err)

		CreateNetworkReferralCodeInTx(ctx, tx, createdNetworkId)

		isPro = networkCreateRedeemBalanceCodeInTx(
			networkCreate, createdNetworkId, ctx, tx,
		)

		created = true
	})

	return networkCreateResult{
		Created:     created,
		NetworkId:   createdNetworkId,
		NetworkName: validatedNetworkName,
		UserId:      createdUserId,
		Seedphrase:  seedphrase,
		IsPro:       isPro,
	}
}

/**
 * we use this in all flavors of network create to potentially redeem balance code
 */
func networkCreateRedeemBalanceCodeInTx(
	networkCreate *NetworkCreateArgs,
	createdNetworkId server.Id,
	ctx context.Context,
	tx server.PgTx,
) bool {
	isPro := false
	if networkCreate.BalanceCode != nil {
		balanceCode := &RedeemBalanceCodeArgs{
			Secret:    *networkCreate.BalanceCode,
			NetworkId: createdNetworkId,
		}

		// this will add transfer balance and mark the user as paid if successful
		redeemBalanceCode, err := RedeemBalanceCodeInTx(balanceCode, ctx, tx)

		if err == nil && redeemBalanceCode.Error == nil {
			// successfully redeemed balance code
			isPro = true
		}

	}
	return isPro
}

func auditNetworkCreate(
	networkCreate NetworkCreateArgs,
	networkId server.Id,
	session *session.ClientSession,
) {
	// The peppered address hash (server.ClientIpHash), never the raw ip:port.
	// audit_network_event rows are permanent — there is no reaper — so a raw
	// address stored here would outlive every retention promise we make. The
	// hash keeps the abuse-correlation value (same /29 or /56 bucket ⇒ same
	// hash) without holding the address itself. Rows written before this
	// change are rewritten by migration_20260807_ScrubTaskAndAuditClientAddresses.
	type Details struct {
		NetworkCreate     NetworkCreateArgs `json:"network_create"`
		ClientAddressHash string            `json:"client_address_hash,omitempty"`
		ClientPort        int               `json:"client_port,omitempty"`
	}

	details := Details{
		NetworkCreate: networkCreate,
	}
	if clientAddressHash, clientPort, err := session.ClientAddressHashPort(); err == nil {
		details.ClientAddressHash = hex.EncodeToString(clientAddressHash[:])
		details.ClientPort = clientPort
	}

	detailsJson, err := json.Marshal(details)
	if err != nil {
		panic(err)
	}
	detailsJsonString := string(detailsJson)

	auditNetworkEvent := NewAuditNetworkEvent(AuditEventTypeNetworkCreated)
	auditNetworkEvent.NetworkId = networkId
	auditNetworkEvent.EventDetails = &detailsJsonString
	AddAuditEvent(session.Ctx, auditNetworkEvent)
}

type NetworkUpdateArgs struct {
	NetworkName string
}

type NetworkUpdateError struct {
	Message string `json:"message"`
}

type NetworkUpdateResult struct {
	Error *NetworkUpdateError `json:"error,omitempty"`
}

// The refusal of a name another network holds, from the name checks and the
// endpoints that answer them as is.
const networkNameNotAvailableMessage = "Network name not available"

// The unique constraint on network.network_name (postgres's name for the
// table's UNIQUE (network_name)).
const networkNameUniqueConstraint = "network_network_name_key"

// Raised by RaiseNetworkNameWrite in place of a unique violation on
// network.network_name. It is not a database error, so server.Tx rolls the
// transaction back and raises it without a rerun, and NetworkNameTx answers it
// as the name being taken.
var errNetworkNameTaken = errors.New("network name taken")

// Runs a transaction whose callback writes network.network_name through
// RaiseNetworkNameWrite, and reports whether the write met another network
// holding the name. Such a network committed the name after the transaction's
// snapshot, so the callback's own availability check in the same transaction
// could not see it. The transaction is rolled back, so nothing the callback
// wrote remains; every other failure raises as usual.
func NetworkNameTx(ctx context.Context, callback func(server.PgTx)) (taken bool) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && errors.Is(err, errNetworkNameTaken) {
				taken = true
				return
			}
			panic(r)
		}
	}()
	server.Tx(ctx, callback)
	return
}

// Raises the error of a write to network.network_name. A unique violation on
// the name raises errNetworkNameTaken, which NetworkNameTx answers as the name
// being taken.
func RaiseNetworkNameWrite[T any](result T, err error) T {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == networkNameUniqueConstraint {
		panic(errNetworkNameTaken)
	}
	server.Raise(err)
	return result
}

// Whether a network holds the name, as of the transaction's snapshot. A write
// of the name checks this in its own transaction: a check before it leaves a
// window in which another network takes the name, and the write then meets
// the unique index. A network that commits the name after the snapshot still
// meets the index. A create's insert raises that unique violation and
// server.Tx's rerun repeats this check, which then sees the name; a rename
// answers it at once through RaiseNetworkNameWrite.
func networkNameHeldInTx(ctx context.Context, tx server.PgTx, networkName string) bool {
	held := false
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM network WHERE network_name = $1)`,
		networkName,
	).Scan(&held))
	return held
}

// Replaces the network's entry in the network name search with the name, in
// the caller's transaction, so the database entry commits or rolls back with
// the rename. Network create adds the same entry (the network id, variant 0)
// after its commit. Returns the post that moves the network in this process's
// in-memory index, which the caller runs only once the transaction has
// committed (server.RunPosts); other processes load the committed entry from
// the search's update log.
func IndexNetworkNameInTx(ctx context.Context, tx server.PgTx, networkId server.Id, networkName string) server.PostFunction {
	return networkNameSearch().AddInTxPost(ctx, networkName, networkId, 0, tx)
}

func checkNetworkNameAvailability(
	networkName string,
	session *session.ClientSession,
) (err error) {

	validatedNetworkName, validationErr := ValidateNetworkName(networkName)
	if validationErr != nil {
		err = validationErr
		return
	}

	taken := networkNameSearch().AnyAround(session.Ctx, validatedNetworkName, 1)

	if taken {
		err = errors.New(networkNameNotAvailableMessage)
		return
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {
		err = nil
		if networkNameHeldInTx(session.Ctx, tx, validatedNetworkName) {
			err = errors.New(networkNameNotAvailableMessage)
		}
	})

	return err
}

// Test-only scheduling hook between NetworkUpdate's availability check and its
// write, in the write's transaction. Production leaves it nil; tests use it to
// commit the name for another network in between, without sleeps or scheduler
// luck.
var networkUpdateBeforeWrite func()

// Renames the session's network to the validated (normalized) form of the
// name, as network create stores it. A name that fails validation, or that
// the fuzzy name search or the exact name check finds taken, is refused before
// anything is written. The exact check is repeated in the write's own
// transaction, and a write that meets another network holding the name on the
// unique index (committed after that transaction's snapshot) is refused the
// same way, instead of failing the call. The rename's transaction also
// replaces the network's entry in the network name search, and this process's
// in-memory index follows once it has committed.
func NetworkUpdate(
	networkUpdate NetworkUpdateArgs,
	session *session.ClientSession,
) (*NetworkUpdateResult, error) {
	refusal := func(message string) (*NetworkUpdateResult, error) {
		return &NetworkUpdateResult{
			Error: &NetworkUpdateError{
				Message: message,
			},
		}, nil
	}

	validatedNetworkName, err := ValidateNetworkName(networkUpdate.NetworkName)
	if err != nil {
		return refusal(err.Error())
	}
	if err := checkNetworkNameAvailability(validatedNetworkName, session); err != nil {
		return refusal(err.Error())
	}

	held := false
	var posts []server.PostFunction
	taken := NetworkNameTx(session.Ctx, func(tx server.PgTx) {
		// a rerun starts over
		posts = nil
		held = networkNameHeldInTx(session.Ctx, tx, validatedNetworkName)
		if held {
			return
		}
		if networkUpdateBeforeWrite != nil {
			networkUpdateBeforeWrite()
		}
		tag := RaiseNetworkNameWrite(tx.Exec(
			session.Ctx,
			`
				UPDATE network
				SET network_name = $2
				WHERE network_id = $1
			`,
			session.ByJwt.NetworkId,
			validatedNetworkName,
		))
		if tag.RowsAffected() == 1 {
			posts = append(posts, IndexNetworkNameInTx(session.Ctx, tx, session.ByJwt.NetworkId, validatedNetworkName))
		}
	})
	if held || taken {
		return refusal(networkNameNotAvailableMessage)
	}
	// the rename committed
	server.RunPosts(session.Ctx, posts...)

	return &NetworkUpdateResult{}, nil
}

type Network struct {
	NetworkId             *server.Id `json:"network_id"`
	NetworkName           string     `json:"network_name"`
	ContainsProfanity     bool       `json:"contains_profanity"`
	AdminUserId           *server.Id `json:"admin_user_id"`
	GuestUpgradeNetworkId *server.Id `json:"guest_upgrade_network_id"`
}

func GetNetwork(
	session *session.ClientSession,
) *Network {
	var network *Network

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
			SELECT
				network_id,
				network_name,
				admin_user_id,
				guest_upgrade_network_id,
				contains_profanity
			FROM network
			WHERE network_id = $1
		`,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {

				network = &Network{}

				server.Raise(result.Scan(
					&network.NetworkId,
					&network.NetworkName,
					&network.AdminUserId,
					&network.GuestUpgradeNetworkId,
					&network.ContainsProfanity,
				))
			}
		})

	})

	return network
}

/**
 * todo - better password validation
 */
func passwordValid(password string) bool {
	if len(password) < MinPasswordLength {
		return false
	}
	return true
}

/**
 * ===
 * Testing util functions
 * ===
 */
func Testing_CreateNetwork(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
) (userAuth string) {
	userAuth = fmt.Sprintf("%s@bringyour.com", networkId)
	password := "password"

	passwordSalt := createPasswordSalt()
	passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

	// FIXME this lib is not thread safe
	// containsProfanity := goaway.IsProfane(networkName)
	containsProfanity := false

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id, contains_profanity)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			networkName,
			adminUserId,
			containsProfanity,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified, password_hash, password_salt)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			adminUserId,
			"test",
			AuthTypePassword,
			userAuth,
			true,
			passwordHash,
			passwordSalt,
		))

		addUserAuthInTx(
			tx,
			&AddUserAuthArgs{
				UserId:       adminUserId,
				UserAuth:     &userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
				Verified:     true,
			}, ctx,
		)
	})

	return
}

// Inserts a network_user/network pair shaped exactly like the pre-removal
// networkCreateGuest function used to create: auth_type='guest', no user_auth,
// no password, no rows in any auth table. Guest signup is retired, but rows
// shaped like this still exist in production and this fork's code must not
// mistreat them.
func Testing_CreateLegacyGuestNetwork(ctx context.Context, networkId server.Id, userId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type) VALUES ($1, $2, $3)`,
			userId, "guest", "guest",
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
			networkId, "g"+networkId.String(), userId,
		))
	})
}

func Testing_CreateNetworkByWallet(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
	publicKey string,
	signature string,
	message string,
) {
	walletArgs := &AddWalletAuthArgs{
		WalletAuth: &WalletAuthArgs{
			PublicKey:  publicKey,
			Signature:  signature,
			Message:    message,
			Blockchain: AuthTypeSolana,
		},
		UserId: adminUserId,
	}
	server.Raise(validateWalletAuth(walletArgs.WalletAuth))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			networkName,
			adminUserId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, verified, wallet_address, wallet_blockchain)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			adminUserId,
			"test",
			AuthTypeSolana,
			true,
			publicKey,
			AuthTypeSolana,
		))

		server.Raise(addWalletAuthInTx(tx, walletArgs, ctx))
	})

}

func Testing_CreateGuestNetwork(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
) {

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			networkName,
			adminUserId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, verified)
				VALUES ($1, $2, $3, $4)
			`,
			adminUserId,
			"test",
			AuthTypeGuest,
			false,
		))

	})

}

func Testing_CreateNetworkSso(
	networkId server.Id,
	userId server.Id,
	authJwt AuthJwt,
	// authJwtType SsoAuthType,
	ctx context.Context,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			"network_name",
			userId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, verified)
				VALUES ($1, $2, $3, $4)
			`,
			userId,
			"user_name",
			AuthTypeGoogle,
			true,
		))

		addSsoAuth(
			&AddSsoAuthArgs{
				ParsedAuthJwt: authJwt,
				AuthJwt:       "",
				AuthJwtType:   authJwt.AuthType,
				UserId:        userId,
			},
			ctx,
		)
	})

}

// FindNetworkByName resolves a network by the name the apps display. The name is
// normalized the same way network creation normalizes it (ValidateNetworkName:
// trimmed, lower-cased, spaces to dashes), so the match is case-insensitive and
// exact -- unlike NetworkCheck, which is a sign-up similarity check and reads a
// name within a few characters of an existing one as taken.
//
// Used by the unauthenticated buy-data checkout to apply purchased data to a
// named network. Unknown or malformed name: nil id, empty name, nil error.
func FindNetworkByName(ctx context.Context, networkName string) (networkId *server.Id, name string) {
	validated, err := ValidateNetworkName(networkName)
	if err != nil {
		return nil, ""
	}
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`SELECT network_id, network_name FROM network WHERE network_name = $1`,
			validated,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id, &name))
				networkId = &id
			}
		})
	})
	return
}

// GetNetworkAdminUserAuth returns a network's name and its admin user's login
// (an email or a phone number, as stored), for a note to a network that was
// bought data for by name. ok = false when the network does not exist; userAuth
// is empty for an admin with no email or phone login (wallet, seed phrase).
func GetNetworkAdminUserAuth(ctx context.Context, networkId server.Id) (networkName string, userAuth string, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		networkName, userAuth, ok = getNetworkAdminUserAuth(ctx, conn, networkId)
	})
	return
}

// The network's name and admin login read on the caller's transaction, for a
// note written in the transaction that owes it.
func GetNetworkAdminUserAuthInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (networkName string, userAuth string, ok bool) {
	return getNetworkAdminUserAuth(ctx, tx, networkId)
}

// The network's name and admin login, read with `query`.
func getNetworkAdminUserAuth(ctx context.Context, query server.PgCanQuery, networkId server.Id) (networkName string, userAuth string, ok bool) {
	result, err := query.Query(
		ctx,
		`
		SELECT network.network_name, network_user.user_auth
		FROM network
		LEFT JOIN network_user ON network_user.user_id = network.admin_user_id
		WHERE network.network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var adminUserAuth *string
			server.Raise(result.Scan(&networkName, &adminUserAuth))
			if adminUserAuth != nil {
				userAuth = *adminUserAuth
			}
			ok = true
		}
	})
	return
}
