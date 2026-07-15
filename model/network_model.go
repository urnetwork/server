package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	// "github.com/urnetwork/glog"

	goaway "github.com/TwiN/go-away"
	bip39 "github.com/tyler-smith/go-bip39"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"

	// "github.com/urnetwork/server/ulid"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/search"
)

func init() {
	server.OnWarmup(func() {
		networkNameSearch()
		//.WaitForInitialSync(context.Background())
	})
	server.OnReset(func() {
		networkNameSearch().Close()
		networkNameSearch = sync.OnceValue(createNetworkNameSearch)
	})
}

func createNetworkNameSearch() *search.SearchLocal {
	return search.NewSearchLocalWithDefaults(
		context.Background(),
		search.NewSearchDb("network_name", search.SearchTypeFull),
	)
}

var networkNameSearch = sync.OnceValue(createNetworkNameSearch)

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
}

type NetworkCreateResult struct {
	Network              *NetworkCreateResultNetwork      `json:"network,omitempty"`
	UserAuth             *string                          `json:"user_auth,omitempty"`
	Seedphrase           *string                          `json:"seedphrase,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error                *NetworkCreateResultError        `json:"error,omitempty"`
	IsPro                bool                             `json:"is_pro,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt       *string   `json:"by_jwt,omitempty"`
	NetworkId   server.Id `json:"network_id,omitempty"`
	NetworkName string    `json:"network_name,omitempty"`
	IsPro       bool      `json:"is_pro,omitempty"`
}

type NetworkCreateResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
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

func NetworkCreate(
	networkCreate NetworkCreateArgs,
	session *session.ClientSession,
) (*NetworkCreateResult, error) {
	userAuth, _ := NormalUserAuthV1(networkCreate.UserAuth)

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	if !networkCreate.Terms {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: AgreeToTerms,
			},
		}
		return result, nil
	}

	// seedphrase creation: no auth method provided
	if networkCreate.UserAuth == nil && networkCreate.AuthJwt == nil && networkCreate.WalletAuth == nil {

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

	validatedNetworkName, error := ValidateNetworkName(networkCreate.NetworkName)

	if error != nil {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: error.Error(),
			},
		}
		return result, nil
	}

	// check if the network name is already taken
	err := checkNetworkNameAvailability(validatedNetworkName, session)
	if err != nil {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: err.Error(),
			},
		}
		return result, nil
	}

	containsProfanity := goaway.IsProfane(validatedNetworkName)

	if networkCreate.UserAuth != nil {
		// user is creating a network via email/phone + pass
		// validate the user does not exist

		if userAuth == nil {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Invalid email or phone number.",
				},
			}
			return result, nil
		}

		resultNetworkCreate := networkCreateUserAuth(
			session.Ctx,
			&networkCreate,
			userAuth,
			validatedNetworkName,
			containsProfanity,
		)

		if resultNetworkCreate.Created {
			auditNetworkCreate(networkCreate, resultNetworkCreate.NetworkId, session)

			networkNameSearch().Add(session.Ctx, networkCreate.NetworkName, resultNetworkCreate.NetworkId, 0)

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
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Account might already exist. Please start over.",
				},
			}
			return result, nil
		}
	} else if networkCreate.AuthJwt != nil && networkCreate.AuthJwtType != nil {
		// user is creating a network via social login

		authJwt, _ := ParseAuthJwt(*networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))

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

				networkNameSearch().Add(session.Ctx, networkCreate.NetworkName, resultNetworkCreate.NetworkId, 0)

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
				result := &NetworkCreateResult{
					Error: &NetworkCreateResultError{
						Message: "Account might already exist. Please log in again.",
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

		networkCreateResult := networkCreateWalletAuth(
			session.Ctx,
			&networkCreate,
			validatedNetworkName,
			containsProfanity,
		)

		if networkCreateResult.Created {

			auditNetworkCreate(networkCreate, networkCreateResult.NetworkId, session)

			networkNameSearch().Add(session.Ctx, networkCreate.NetworkName, networkCreateResult.NetworkId, 0)

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
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Account might already exist. Please log in again.",
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
}

/**
 * network create wallet auth
 */
func networkCreateWalletAuth(
	ctx context.Context,
	networkCreate *NetworkCreateArgs,
	validatedNetworkName string,
	containsProfanity bool,
) networkCreateResult {

	created := false
	var createdNetworkId server.Id
	var createdUserId server.Id
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
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
			AuthTypeSolana,
			networkCreate.WalletAuth.PublicKey,
			networkCreate.WalletAuth.Blockchain,
			networkCreate.UserName,
		)
		if err != nil {
			panic(err)
		}

		// insert into network_user_auth_wallet
		addWalletAuth(
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
		)

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
		Created:     created,
		NetworkId:   createdNetworkId,
		NetworkName: networkCreate.NetworkName,
		UserId:      createdUserId,
		IsPro:       isPro,
	}

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
	var createdNetworkId server.Id
	var createdUserId server.Id
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
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
			return
		}

		createdUserId = server.NewId()
		createdNetworkId = server.NewId()

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

		// insert into network_user_auth_sso
		err = addSsoAuthInTx(
			tx,
			ctx,
			&AddSsoAuthArgs{
				UserId:        createdUserId,
				AuthJwt:       *networkCreate.AuthJwt,
				ParsedAuthJwt: parsedAuthJwt,
				AuthJwtType:   SsoAuthType(*networkCreate.AuthJwtType),
			},
		)

		if err != nil {
			glog.Infof("Error adding sso auth in tx: %s", err.Error())
			created = false
			return
		}

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
		Created:     created,
		NetworkId:   createdNetworkId,
		NetworkName: networkCreate.NetworkName,
		UserId:      createdUserId,
		IsPro:       isPro,
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
) networkCreateResult {

	created := false
	var createdNetworkId server.Id
	var createdUserId server.Id
	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
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
			return
		}

		var existingNetworkId *server.Id

		result, err = tx.Query(
			ctx,
			`
				SELECT network_id FROM network WHERE network_name = $1
			`,
			networkCreate.NetworkName,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&existingNetworkId))
			}
		})

		if existingNetworkId != nil {
			return
		}

		createdUserId = server.NewId()
		createdNetworkId = server.NewId()

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

		// insert into network_user_auth_password
		addUserAuthInTx(
			tx,
			&AddUserAuthArgs{
				UserId:       createdUserId,
				UserAuth:     userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
			},
			ctx,
		)

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
		Created:     created,
		NetworkId:   createdNetworkId,
		NetworkName: networkCreate.NetworkName,
		UserId:      createdUserId,
		IsPro:       isPro,
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

		// if SSO provided, bind it too
		if networkCreate.AuthJwt != nil && networkCreate.AuthJwtType != nil {
			authJwt, _ := ParseAuthJwt(*networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))
			if authJwt != nil {
				err = addSsoAuthInTx(
					tx, ctx,
					&AddSsoAuthArgs{
						UserId:        createdUserId,
						AuthJwt:       *networkCreate.AuthJwt,
						ParsedAuthJwt: *authJwt,
						AuthJwtType:   SsoAuthType(*networkCreate.AuthJwtType),
					},
				)
				if err != nil {
					glog.Infof("[net]seedphrase create + sso bind error: %s\n", err)
				}
			}
		}

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
	type Details struct {
		NetworkCreate NetworkCreateArgs `json:"network_create"`
		ClientAddress string            `json:"client_address"`
	}

	details := Details{
		NetworkCreate: networkCreate,
		ClientAddress: session.ClientAddress,
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

func checkNetworkNameAvailability(
	networkName string,
	session *session.ClientSession,
) (err error) {

	var existingNetworkId *server.Id

	validatedNetworkName, validationErr := ValidateNetworkName(networkName)
	if validationErr != nil {
		err = validationErr
		return
	}

	taken := networkNameSearch().AnyAround(session.Ctx, validatedNetworkName, 1)

	if taken {
		err = errors.New("Network name not available")
		return
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, queryErr := tx.Query(
			session.Ctx,
			`
				SELECT network_id FROM network WHERE network_name = $1
			`,
			validatedNetworkName,
		)
		server.WithPgResult(result, queryErr, func() {
			if result.Next() {
				server.Raise(result.Scan(&existingNetworkId))
			}
		})

		if existingNetworkId != nil {

			err = errors.New("Network name not available")
			return

		}
	})

	return err
}

func NetworkUpdate(
	networkUpdate NetworkUpdateArgs,
	session *session.ClientSession,
) (*NetworkUpdateResult, error) {
	var networkCreateResult = &NetworkUpdateResult{}
	networkName := strings.TrimSpace(networkUpdate.NetworkName)

	err := checkNetworkNameAvailability(networkName, session)
	if err != nil {
		networkCreateResult = &NetworkUpdateResult{
			Error: &NetworkUpdateError{
				Message: err.Error(),
			},
		}
		return networkCreateResult, nil
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
							UPDATE network
							SET
									network_name = $2
							WHERE
									network_id = $1
					`,
			session.ByJwt.NetworkId,
			networkName,
		))

	})

	return networkCreateResult, nil
}

type UpgradeGuestNetwork struct {
	ByJwt *string `json:"by_jwt,omitempty"`
}

type UpgradeGuestResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type UpgradeGuestError struct {
	Message string `json:"message"`
}

/**
 * Upgrade guest with existing account
 */
type UpgradeGuestExistingError struct {
	Message string `json:"message"`
}

type UpgradeGuestExistingResult struct {
	Error                *UpgradeGuestExistingError                `json:"error,omitempty"`
	VerificationRequired *UpgradeGuestExistingVerificationRequired `json:"verification_required,omitempty"`
	Network              *UpgradeGuestExistingResultNetwork        `json:"network,omitempty"`
	// Error                *AuthLoginWithPasswordResultError        `json:"error,omitempty"`
}

type UpgradeGuestExistingVerificationRequired struct {
	UserAuth string `json:"user_auth"`
}

type UpgradeGuestExistingResultNetwork struct {
	ByJwt *string `json:"by_jwt,omitempty"`
	// NetworkName *string `json:"name,omitempty"`
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

func Testing_CreateNetworkByWallet(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
	publicKey string,
	signature string,
	message string,
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

		addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: &WalletAuthArgs{
					PublicKey:  publicKey,
					Signature:  signature,
					Message:    message,
					Blockchain: AuthTypeSolana,
				},
				UserId: adminUserId,
			},
			ctx,
		)
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
