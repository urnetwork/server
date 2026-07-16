package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	// "strconv"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	// "github.com/urnetwork/glog"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"

	"github.com/gagliardetto/solana-go"

	// "github.com/urnetwork/server/ulid"
	"github.com/urnetwork/server/jwt"
)

// 4 hours
const VerifyCodeTimeout = 4 * time.Hour

type AuthType = string

const (
	AuthTypePassword  AuthType = "password"
	AuthTypeApple     AuthType = "apple"
	AuthTypeGoogle    AuthType = "google"
	AuthTypeBringYour AuthType = "bringyour"
	AuthTypeGuest     AuthType = "guest"
	AuthTypeSolana    AuthType = "solana"
	AuthTypeSeedphrase AuthType = "seedphrase"
)

type WalletAuthArgs struct {
	PublicKey  string `json:"wallet_address,omitempty"`
	Signature  string `json:"wallet_signature,omitempty"`
	Message    string `json:"wallet_message,omitempty"`
	Blockchain string `json:"blockchain,omitempty"`
	// Nonce is a server-issued single-use challenge (see AuthWalletNonceCreate) that
	// the client must embed in Message and echo here for wallet login, to prevent
	// signature replay. Optional during client rollout; enforced when present.
	Nonce string `json:"wallet_nonce,omitempty"`
}

type AuthLoginArgs struct {
	UserAuth    *string         `json:"user_auth,omitempty"`
	AuthJwtType *string         `json:"auth_jwt_type,omitempty"`
	AuthJwt     *string         `json:"auth_jwt,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
	Seedphrase  *string         `json:"seedphrase,omitempty"`
}

type AuthLoginResult struct {
	UserName    *string                 `json:"user_name,omitempty"`
	UserAuth    *string                 `json:"user_auth,omitempty"`
	WalletAuth  *WalletAuthArgs         `json:"wallet_auth,omitempty"`
	AuthAllowed *[]string               `json:"auth_allowed,omitempty"`
	Error       *AuthLoginResultError   `json:"error,omitempty"`
	Network     *AuthLoginResultNetwork `json:"network,omitempty"`
}

// MarshalJSON dual-emits the deprecated `wallet_login` alias alongside the spec
// field `wallet_auth` during the rename migration (be lenient in what we send).
func (r AuthLoginResult) MarshalJSON() ([]byte, error) {
	type alias AuthLoginResult
	b, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if v, ok := m["wallet_auth"]; ok {
		m["wallet_login"] = v
	}
	return json.Marshal(m)
}

type AuthLoginResultError struct {
	SuggestedUserAuth *string `json:"suggested_user_auth,omitempty"`
	Message           string  `json:"message"`
}

type AuthLoginResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

type SsoAuthType = string

const (
	SsoAuthTypeApple  SsoAuthType = "apple"
	SsoAuthTypeGoogle SsoAuthType = "google"
)

func AuthLogin(
	login AuthLoginArgs,
	session *session.ClientSession,
) (*AuthLoginResult, error) {
	userAuth, _ := NormalUserAuthV1(login.UserAuth)

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	if login.UserAuth != nil {

		return loginUserAuth(
			userAuth,
			session.Ctx,
		)

	} else if login.AuthJwt != nil && login.AuthJwtType != nil {

		/**
		 * SSO login
		 * ===========
		 * Users can have multiple SSO auths associated with their account (apple, google, etc.)
		 *
		 * If a user only has email/phone auth, then attempts login with SSO, we allow it.
		 * We check the user auths match and associate the new SSO auth
		 *
		 * If a user has a different SSO auth (ie google in our DB and tries login with apple)
		 * we allow the login and associate the new SSO auth
		 */

		authJwt, _ := ParseAuthJwt(*login.AuthJwt, AuthType(*login.AuthJwtType))

		if authJwt != nil {

			return handleLoginParsedAuthJwt(
				&HandleLoginParsedAuthJwtArgs{
					AuthJwt: *authJwt,
					// AuthJwtType:       SsoAuthType(*login.AuthJwtType),
					AuthJwtStr:        *login.AuthJwt,
					UserAuthAttemptId: userAuthAttemptId,
				},
				session.Ctx,
			)

		}
	} else if login.WalletAuth != nil {

		return handleLoginWallet(
			login.WalletAuth,
			session.Ctx,
		)
	} else if login.Seedphrase != nil && *login.Seedphrase != "" {
		result, err := LoginWithSeedphrase(session.Ctx, *login.Seedphrase)
		if err != nil {
			return &AuthLoginResult{
				Error: &AuthLoginResultError{
					Message: err.Error(),
				},
			}, nil
		}
		return &AuthLoginResult{
			Network: &AuthLoginResultNetwork{
				ByJwt: result.ByJwt,
			},
		}, nil
	}

	return &AuthLoginResult{
		Error: &AuthLoginResultError{
			Message: "Invalid login credentials.",
		},
	}, nil
}

/**
 * Login attempt for email/phone + password
 */
func loginUserAuth(
	userAuth *string,
	ctx context.Context,
) (*AuthLoginResult, error) {
	if userAuth == nil {
		result := &AuthLoginResult{
			Error: &AuthLoginResultError{
				Message: "Invalid email or phone number.",
			},
		}
		return result, nil
	}

	var authType *string

	// check if exists in network_user_auth_password
	// check if exists in network_user_auth_sso

	// check for email/phone user auths
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT auth_type FROM network_user_auth_password WHERE user_auth = $1
			`,
			userAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&authType))
			}
		})
	})

	/**
	 * user exists in network_user_auth_password
	 * forward them along to login with password
	 */
	if authType != nil {

		glog.V(1).Infof("login auth type is %s", *authType)

		isUserAuth := false
		if UserAuthType(*authType) == UserAuthTypeEmail || UserAuthType(*authType) == UserAuthTypePhone {
			isUserAuth = true
		}

		authAllowed := []string{*authType}

		if isUserAuth {
			/**
			 * We can remove this check once UIs are updated
			 * This auth type changed from "password" to "email" or "phone"
			 */
			authAllowed = append(authAllowed, "password")
		}

		result := &AuthLoginResult{
			UserAuth:    userAuth,
			AuthAllowed: &authAllowed,
		}
		return result, nil
	}

	/**
	 * check for sso user auths
	 */
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT auth_type FROM network_user_auth_sso WHERE user_auth = $1
			`,
			userAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&authType))
			}
		})
	})

	if authType == nil {
		/**
		 * new user, neither password nor sso auth exists
		 */

		result := &AuthLoginResult{
			UserAuth: userAuth,
		}
		return result, nil
	} else {
		/**
		 * existing user, sso auth exists
		 */

		result := &AuthLoginResult{
			UserAuth:    userAuth,
			AuthAllowed: &[]string{*authType},
		}
		return result, nil
	}
}

type HandleLoginParsedAuthJwtArgs struct {
	AuthJwt           AuthJwt
	AuthJwtStr        string
	UserAuthAttemptId server.Id
}

func handleLoginParsedAuthJwt(
	args *HandleLoginParsedAuthJwtArgs,
	ctx context.Context,
) (*AuthLoginResult, error) {

	var authJwt = args.AuthJwt

	var userId *server.Id
	var networkId server.Id
	var networkName string

	ssoExists := false
	userAuthExists := false
	userAuthEmailVerified := false

	/**
	 * get sso auths
	 */
	ssoAuths, err := getSsoAuthsByUserAuth(ctx, authJwt.UserAuth)
	if err != nil {
		return nil, fmt.Errorf("failed to get SSO auths: %w", err)
	}
	if len(ssoAuths) > 0 {
		ssoExists = true
		userId = ssoAuths[0].UserId
	}

	/**
	 * check if userAuth exists with this email in network_user_auth_password
	 */
	server.Db(ctx, func(conn server.PgConn) {
		// server.Logger().Printf("Matching user auth %s\n", authJwt.UserAuth)
		result, err := conn.Query(
			ctx,
			`
					SELECT
						user_id,
						verified
					FROM network_user_auth_password
					WHERE user_auth = $1
				`,
			authJwt.UserAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var id *server.Id
				verified := false
				server.Raise(result.Scan(
					&id,
					&verified,
				))
				userAuthExists = true
				userAuthEmailVerified = verified

				if id != nil {
					glog.Infof("setting user id inside of user auth as %s", id.String())
					userId = id
				}
			}
		})
	})

	if userId == nil {

		// new user - direct to create network
		return &AuthLoginResult{
			UserName: &authJwt.UserName,
		}, nil
	}

	server.Db(ctx, func(conn server.PgConn) {
		// server.Logger().Printf("Matching user auth %s\n", authJwt.UserAuth)
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_user.user_id,
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&userId,
					&networkId,
					&networkName,
				))
			}
		})
	})

	if &networkId == nil || &networkName == nil {

		/**
		 * This scenario should not happen
		 * If a user has a child network_user auth added, the parent network_user row should always exist
		 */
		return nil, fmt.Errorf("network not found for user %s", *userId)
	}

	if !userAuthEmailVerified && userAuthExists && ssoExists {
		// todo - mark userauth as verified
	}

	if !ssoExists && !userAuthExists {

		/**
		 * this generally would only happen for guest users
		 * users signing in with SSO would usually have SSO or user auth
		 * no user auth exists, create a new user
		 */
		return &AuthLoginResult{
			UserName: &authJwt.UserName,
		}, nil
	}

	/**
	 * check for matching sso auth types
	 */
	var matchingSso *NetworkUserSsoAuth
	for _, ssoAuth := range ssoAuths {
		if ssoAuth.AuthType == authJwt.AuthType {
			matchingSso = &ssoAuth
			break
		}
	}

	if matchingSso == nil {

		/**
		 * User is logging in with an SSO that does not exist
		 * but user has a different SSO
		 * add the new SSO auth
		 */
		addSsoAuth(
			&AddSsoAuthArgs{
				ParsedAuthJwt: args.AuthJwt,
				AuthJwt:       args.AuthJwtStr,
				AuthJwtType:   args.AuthJwt.AuthType,
				UserId:        *userId,
			},
			ctx,
		)
	}

	SetUserAuthAttemptSuccess(ctx, args.UserAuthAttemptId, true)

	isGuestMode := false

	isPro := IsPro(
		ctx,
		&networkId,
	)

	// successful login
	byJwt := jwt.NewByJwt(
		networkId,
		*userId,
		networkName,
		isGuestMode,
		isPro,
	)
	result := &AuthLoginResult{
		Network: &AuthLoginResultNetwork{
			ByJwt: byJwt.Sign(),
		},
	}
	return result, nil
}

func handleLoginWallet(
	walletAuth *WalletAuthArgs,
	ctx context.Context,
) (result *AuthLoginResult, returnErr error) {
	/**
	 * Handle wallet login by validating the server-issued challenge.
	 * UseWalletAuthChallenge verifies the signature, checks the timestamp,
	 * and marks the challenge as used atomically.
	 */
	useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
		Blockchain: walletAuth.Blockchain,
		PublicKey:  walletAuth.PublicKey,
		Message:    walletAuth.Message,
		Signature:  walletAuth.Signature,
	}, ctx)
	if err != nil {
		returnErr = err
		return
	}
	if !useResult.Valid {
		msg := "401 invalid wallet challenge"
		if useResult.Error != nil {
			msg = useResult.Error.Message
		}
		returnErr = errors.New(msg)
		return
	}

	walletAuths, err := getWalletAuthsByAddress(
		ctx,
		walletAuth.PublicKey,
	)

	if err != nil {
		returnErr = fmt.Errorf("failed to get wallet auths: %w", err)
		return
	}

	if len(walletAuths) <= 0 {

		/**
		 * New wallet user
		 */
		return &AuthLoginResult{
			WalletAuth: walletAuth,
		}, nil
	}

	userId := walletAuths[0].UserId

	if userId == nil {
		returnErr = errors.New("user ID not found for wallet auth")
		return
	}

	/**
	 * Check if the user exists associated with this public key
	 */
	found := false
	var networkId server.Id
	var networkName string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&networkId,
					&networkName,
				))
				found = true
			}
		})
	})

	if found {

		pro := IsPro(
			ctx,
			&networkId,
		)

		byJwt := jwt.NewByJwt(
			networkId,
			*userId,
			networkName,
			false,
			pro,
		)
		result = &AuthLoginResult{
			Network: &AuthLoginResultNetwork{
				ByJwt: byJwt.Sign(),
			},
		}
		return
	} else {
		/**
		 * New wallet user
		 */
		result = &AuthLoginResult{
			WalletAuth: walletAuth,
		}
		return
	}
}

func VerifySignature(blockchain string, publicKey string, message string, signature string) (bool, error) {

	if blockchain == "" || strings.EqualFold(blockchain, "solana") || strings.EqualFold(blockchain, "sol") {
		return VerifySolanaSignature(publicKey, message, signature)
	}

	if strings.EqualFold(blockchain, "ethereum") || strings.EqualFold(blockchain, "eth") || strings.EqualFold(blockchain, "matic") || strings.EqualFold(blockchain, "polygon") || strings.EqualFold(blockchain, "poly") {
		return VerifyEthereumSignature(publicKey, message, signature)
	}

	if strings.EqualFold(blockchain, "tao") || strings.EqualFold(blockchain, "bittensor") {
		return VerifyBittensorSignature(publicKey, message, signature)
	}

	return false, fmt.Errorf("unsupported blockchain: %s", blockchain)

}

/**
 * Verify Ethereum wallet signature
 * =================================
 * publicKey: the wallet address
 * message:  the signature as a hex string (with or without 0x)
 * signature: the signature in hex format
 */
func VerifyEthereumSignature(publicKey string, message string, signature string) (bool, error) {
	// Ethereum prefixes messages before signing
	// https://eips.ethereum.org/EIPS/eip-191
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	msg := []byte(prefix + message)
	msgHash := crypto.Keccak256Hash(msg)

	// Decode signature
	sig := common.FromHex(signature)
	if len(sig) != 65 {
		return false, fmt.Errorf("signature must be 65 bytes")
	}
	// Ethereum uses v = 27 or 28, Go expects 0 or 1
	if sig[64] >= 27 {
		sig[64] -= 27
	}

	pubKey, err := crypto.SigToPub(msgHash.Bytes(), sig)
	if err != nil {
		return false, err
	}
	recoveredAddr := crypto.PubkeyToAddress(*pubKey)
	return recoveredAddr.Hex() == publicKey, nil
}

/**
 * Verify a Solana wallet signature
 */
func VerifySolanaSignature(publicKeyStr string, message string, signatureStr string) (bool, error) {
	// Parse the public key from string
	publicKey, err := solana.PublicKeyFromBase58(publicKeyStr)
	if err != nil {
		return false, fmt.Errorf("invalid public key: %v", err)
	}

	// Parse the signature from string
	signatureBytes, err := base64.StdEncoding.DecodeString(signatureStr)
	if err != nil {
		return false, fmt.Errorf("invalid signature encoding: %v", err)
	}

	// Convert signature bytes to the expected format
	var signature solana.Signature
	copy(signature[:], signatureBytes)

	// Verify the signature against the message and public key
	return solana.SignatureFromBytes(signature[:]).Verify(publicKey, []byte(message)), nil
}

type AuthLoginWithPasswordArgs struct {
	UserAuth         string `json:"user_auth"`
	Password         string `json:"password"`
	VerifyOtpNumeric bool   `json:"verify_otp_numeric,omitempty"`
}

type AuthLoginWithPasswordResult struct {
	VerificationRequired *AuthLoginWithPasswordResultVerification `json:"verification_required,omitempty"`
	Network              *AuthLoginWithPasswordResultNetwork      `json:"network,omitempty"`
	Error                *AuthLoginWithPasswordResultError        `json:"error,omitempty"`
}

type AuthLoginWithPasswordResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt       *string `json:"by_jwt,omitempty"`
	NetworkName *string `json:"name,omitempty"`
}

type AuthLoginWithPasswordResultError struct {
	Message string `json:"message"`
}

func AuthLoginWithPassword(
	loginWithPassword AuthLoginWithPasswordArgs,
	session *session.ClientSession,
) (*AuthLoginWithPasswordResult, error) {
	userAuth, _ := NormalUserAuthV1(&loginWithPassword.UserAuth)

	if userAuth == nil {
		result := &AuthLoginWithPasswordResult{
			Error: &AuthLoginWithPasswordResultError{
				Message: "Invalid user auth.",
			},
		}
		return result, nil
	}

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	var userId *server.Id
	var passwordHash []byte
	var passwordSalt []byte
	var userVerified bool
	var networkId server.Id
	var networkName string

	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					network_user_auth_password.user_id,
					network_user_auth_password.password_hash,
					network_user_auth_password.password_salt,
					network_user_auth_password.verified,
					network.network_id,
					network.network_name
				FROM network_user_auth_password
				INNER JOIN network ON network.admin_user_id = network_user_auth_password.user_id
				WHERE user_auth = $1
			`,
			userAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&userId,
					&passwordHash,
					&passwordSalt,
					&userVerified,
					&networkId,
					&networkName,
				))
			}
		})
	})

	if userId == nil {
		return nil, errors.New("User does not exist.")
	}

	// server.Logger().Printf("Comparing password hashes\n")
	loginPasswordHash := computePasswordHashV1([]byte(loginWithPassword.Password), passwordSalt)
	if bytes.Equal(passwordHash, loginPasswordHash) {

		if userVerified {
			SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

			isGuestMode := false

			pro := IsPro(
				session.Ctx,
				&networkId,
			)

			// success
			byJwt := jwt.NewByJwt(
				networkId,
				*userId,
				networkName,
				isGuestMode,
				pro,
			)

			signedByJwt := byJwt.Sign()
			result := &AuthLoginWithPasswordResult{
				Network: &AuthLoginWithPasswordResultNetwork{
					ByJwt: &signedByJwt,
				},
			}
			return result, nil
		} else {
			result := &AuthLoginWithPasswordResult{
				VerificationRequired: &AuthLoginWithPasswordResultVerification{
					UserAuth: *userAuth,
				},
				Network: &AuthLoginWithPasswordResultNetwork{
					NetworkName: &networkName,
				},
			}
			return result, nil
		}
	}

	result := &AuthLoginWithPasswordResult{
		Error: &AuthLoginWithPasswordResultError{
			Message: "Invalid user or password.",
		},
	}

	return result, nil
}

type AuthVerifyArgs struct {
	UserAuth   string `json:"user_auth"`
	VerifyCode string `json:"verify_code"`
}

type AuthVerifyResult struct {
	Network *AuthVerifyResultNetwork `json:"network,omitempty"`
	Error   *AuthVerifyResultError   `json:"error,omitempty"`
}

type AuthVerifyResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

type AuthVerifyResultError struct {
	Message string `json:"message"`
}

func AuthVerify(
	verify AuthVerifyArgs,
	session *session.ClientSession,
) (*AuthVerifyResult, error) {
	userAuth, _ := NormalUserAuthV1(&verify.UserAuth)

	if userAuth == nil {
		result := &AuthVerifyResult{
			Error: &AuthVerifyResultError{
				Message: "Invalid user auth.",
			},
		}
		return result, nil
	}

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	normalVerifyCode := strings.ToLower(strings.TrimSpace(verify.VerifyCode))

	var userId server.Id
	var userAuthVerifyId *server.Id
	var networkId server.Id
	var networkName string

	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					network_user.user_id,
					user_auth_verify.user_auth_verify_id,
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN user_auth_verify ON
					user_auth_verify.user_id = network_user.user_id AND
					user_auth_verify.verify_code = $1 AND
					used = false AND
					now() - INTERVAL '1 seconds' * $2 <= user_auth_verify.verify_time
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_auth = $3
			`,
			normalVerifyCode,
			int(VerifyCodeTimeout/time.Second),
			userAuth,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				result.Scan(
					&userId,
					&userAuthVerifyId,
					&networkId,
					&networkName,
				)
			}
		})
	})

	if userAuthVerifyId == nil {
		result := &AuthVerifyResult{
			Error: &AuthVerifyResultError{
				Message: "Invalid code.",
			},
		}
		return result, nil
	}

	// verified
	server.Tx(session.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network_user_auth_password
				SET verified = true
				WHERE user_id = $1 AND user_auth = $2
			`,
			userId,
			userAuth,
		))

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_verify
				SET used = true
				WHERE user_auth_verify_id = $1
			`,
			userAuthVerifyId,
		))
	})

	SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

	isGuestMode := false

	isPro := IsPro(
		session.Ctx,
		&networkId,
	)

	byJwt := jwt.NewByJwt(
		networkId,
		userId,
		networkName,
		isGuestMode,
		isPro,
	)
	result := &AuthVerifyResult{
		Network: &AuthVerifyResultNetwork{
			ByJwt: byJwt.Sign(),
		},
	}
	return result, nil
}

type AuthVerifyCreateCodeArgs struct {
	UserAuth string `json:"user_auth"`
	CodeType VerifyCodeType
}

type AuthVerifyCreateCodeResult struct {
	VerifyCode *string                    `json:"verify_code,omitempty"`
	Error      *AuthVerifyCreateCodeError `json:"error,omitempty"`
}

type AuthVerifyCreateCodeError struct {
	Message string `json:"message"`
}

func AuthVerifyCreateCode(
	verifyCreateCode AuthVerifyCreateCodeArgs,
	session *session.ClientSession,
) (*AuthVerifyCreateCodeResult, error) {
	userAuth, _ := NormalUserAuthV1(&verifyCreateCode.UserAuth)

	if userAuth == nil {
		result := &AuthVerifyCreateCodeResult{
			Error: &AuthVerifyCreateCodeError{
				Message: "Invalid user auth.",
			},
		}
		return result, nil
	}

	// Rate-limit code sends (keyed by user_auth + client address) so an attacker
	// cannot bomb a target's email/SMS or repeatedly invalidate their pending code.
	// Each send intentionally consumes attempt budget (not marked success).
	if _, allow := UserAuthAttempt(userAuth, session); !allow {
		return nil, maxUserAuthAttemptsError()
	}

	created := false
	var verifyCode string

	server.Tx(session.Ctx, func(tx server.PgTx) {
		var result server.PgResult
		var err error

		var userId *server.Id
		result, err = tx.Query(
			session.Ctx,
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

		if userId == nil {
			return
		}

		// invalidate existing codes and create a new code.
		// `used = false` bounds the update to the user's live codes; without it
		// every code send rewrote the user's entire code history (already-used
		// rows included), which bloated the table and serialized concurrent sends
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_verify
				SET used = true
				WHERE user_id = $1 AND used = false
			`,
			userId,
		))

		created = true
		userAuthVerifyId := server.NewId()
		verifyCode = createVerifyCode(verifyCreateCode.CodeType)
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO user_auth_verify
				(user_auth_verify_id, user_id, verify_code)
				VALUES ($1, $2, $3)
			`,
			userAuthVerifyId,
			userId,
			verifyCode,
		))
	})

	if created {
		result := &AuthVerifyCreateCodeResult{
			VerifyCode: &verifyCode,
		}
		return result, nil
	}

	return nil, errors.New("Invalid login.")
}

type AuthPasswordResetCreateCodeArgs struct {
	UserAuth string `json:"error"`
}

type AuthPasswordResetCreateCodeResult struct {
	ResetCode *string                           `json:"reset_code,omitempty"`
	Error     *AuthPasswordResetCreateCodeError `json:"error,omitempty"`
}

type AuthPasswordResetCreateCodeError struct {
	Message string `json:"message"`
}

func AuthPasswordResetCreateCode(
	resetCreateCode AuthPasswordResetCreateCodeArgs,
	session *session.ClientSession,
) (*AuthPasswordResetCreateCodeResult, error) {
	userAuth, _ := NormalUserAuthV1(&resetCreateCode.UserAuth)

	if userAuth == nil {
		result := &AuthPasswordResetCreateCodeResult{
			Error: &AuthPasswordResetCreateCodeError{
				Message: "Invalid user auth.",
			},
		}
		return result, nil
	}

	// Rate-limit code sends (keyed by user_auth + client address) so an attacker
	// cannot bomb a target's email/SMS or repeatedly invalidate their pending code.
	// Each send intentionally consumes attempt budget (not marked success).
	if _, allow := UserAuthAttempt(userAuth, session); !allow {
		return nil, maxUserAuthAttemptsError()
	}

	created := false
	var resetCode string

	server.Tx(session.Ctx, func(tx server.PgTx) {
		var result server.PgResult
		var err error

		var userId *server.Id
		result, err = tx.Query(
			session.Ctx,
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

		if userId == nil {
			return
		}

		// delete existing codes and create a new code
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_reset
				SET used = true
				WHERE user_id = $1
			`,
			userId,
		))

		created = true
		userAuthResetId := server.NewId()
		resetCode = createResetCode()
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO user_auth_reset
				(user_auth_reset_id, user_id, reset_code)
				VALUES ($1, $2, $3)
			`,
			userAuthResetId,
			userId,
			resetCode,
		))
	})

	if created {
		result := &AuthPasswordResetCreateCodeResult{
			ResetCode: &resetCode,
		}
		return result, nil
	}

	return nil, errors.New("Invalid login.")
}

type AuthPasswordSetArgs struct {
	ResetCode string `json:"reset_code"`
	Password  string `json:"password"`
}

// IMPORTANT do not return this to the client.
// The result of setting the password must not reveal the user auth to the client, in case the reset code was guessed
type AuthPasswordSetResult struct {
	NetworkId server.Id
}

func AuthPasswordSet(
	passwordSet AuthPasswordSetArgs,
	session *session.ClientSession,
) (*AuthPasswordSetResult, error) {
	userAuthAttemptId, allow := UserAuthAttempt(nil, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	// 4 hours
	resetValidSeconds := 60 * 60 * 4

	var userId server.Id
	var userAuthResetId *server.Id
	var networkId server.Id
	var networkName string

	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					network_user.user_id,
					user_auth_reset.user_auth_reset_id,
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN user_auth_reset ON
					user_auth_reset.user_id = network_user.user_id AND
					user_auth_reset.reset_code = $1 AND
					used = false AND
					now() - INTERVAL '1 seconds' * $2 <= user_auth_reset.reset_time
				INNER JOIN network ON network.admin_user_id = network_user.user_id
			`,
			passwordSet.ResetCode,
			resetValidSeconds,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&userId,
					&userAuthResetId,
					&networkId,
					&networkName,
				))
			}
		})
	})

	if userAuthResetId == nil {
		return nil, errors.New("Invalid login.")
	}

	// valid reset code
	passwordSalt := createPasswordSalt()
	passwordHash := computePasswordHashV1([]byte(passwordSet.Password), passwordSalt)

	server.Tx(session.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network_user_auth_password
				SET password_hash = $1, password_salt = $2
				WHERE user_id = $3
			`,
			passwordHash,
			passwordSalt,
			userId,
		))

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_reset
				SET used = true
				WHERE user_auth_reset_id = $1
			`,
			userAuthResetId,
		))
	})

	SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

	result := &AuthPasswordSetResult{
		NetworkId: networkId,
	}
	return result, nil
}

const ActiveAuthCodeLimitPerNetwork = 500
const DefaultAuthCodeDuration = 1 * time.Minute
const MaxAuthCodeDuration = 24 * time.Hour
const DefaultAuthCodeUses = 1
const MaxAuthCodeUses = 100

type AuthCodeCreateArgs struct {
	DurationMinutes float64 `json:"duration_minutes,omitempty"`
	Uses            int     `json:"uses,omitempty"`

	// identity roles and principal carried by logins minted from this code.
	// Only a network-level non-guest session may set these; when omitted, the
	// session's own roles and principal are inherited.
	Roles     []string `json:"roles,omitempty"`
	Principal string   `json:"principal,omitempty"`
}

type AuthCodeCreateResult struct {
	AuthCode        string               `json:"auth_code,omitempty"`
	DurationMinutes float64              `json:"duration_minutes,omitempty"`
	Uses            int                  `json:"uses,omitempty"`
	Error           *AuthCodeCreateError `json:"error,omitempty"`
}

type AuthCodeCreateError struct {
	AuthCodeLimitExceeded bool   `json:"auth_code_limit_exceeded,omitempty"`
	Message               string `json:"message,omitempty"`
}

func AuthCodeCreate(
	codeCreate *AuthCodeCreateArgs,
	session *session.ClientSession,
) (codeCreateResult *AuthCodeCreateResult, returnErr error) {

	// // todo:  the device needs to be cleaned up. There is an issue where we should keep the admin jwt in the device  and create clients on demand, or when they expire.
	// // when the device is fixed the client id check can come back in
	//
	// if session.ByJwt.ClientId != nil {
	// 	// the clientId is not threaded currently
	// 	// no need to implement this now
	// 	codeCreateResult = &AuthCodeCreateResult{
	// 		Error: &AuthCodeCreateError{
	// 			AuthCodeLimitExceeded: true,
	// 			Message:               "A client JWT cannot create an auth code.",
	// 		},
	// 	}
	// 	return
	// }

	roles, principal, message := validateClientIdentityArgs(codeCreate.Roles, codeCreate.Principal, session)
	if message != "" {
		codeCreateResult = &AuthCodeCreateResult{
			Error: &AuthCodeCreateError{
				Message: message,
			},
		}
		return
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			session.Ctx,
			`
			SELECT COUNT(*) AS auth_code_count
			FROM auth_code
			WHERE
				network_id = $1 AND
				active = true
			`,
			session.ByJwt.NetworkId,
		)

		authCodeCount := 0

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&authCodeCount))
			}
		})
		if ActiveAuthCodeLimitPerNetwork <= authCodeCount {
			codeCreateResult = &AuthCodeCreateResult{
				Error: &AuthCodeCreateError{
					AuthCodeLimitExceeded: true,
					Message:               "Auth code limit exceeded.",
				},
			}
			return
		}

		authCodeId := server.NewId()

		// 4096 bits
		authCodeBytes := make([]byte, 512)
		if _, err := rand.Read(authCodeBytes); err != nil {
			returnErr = err
			return
		}
		authCode := base64.URLEncoding.EncodeToString(authCodeBytes)

		duration := DefaultAuthCodeDuration
		if 0 < codeCreate.DurationMinutes {
			duration = time.Duration(codeCreate.DurationMinutes*60*1000) * time.Millisecond
		}
		if MaxAuthCodeDuration < duration {
			duration = MaxAuthCodeDuration
		}
		uses := DefaultAuthCodeUses
		if 0 < codeCreate.Uses {
			uses = codeCreate.Uses
		}
		if MaxAuthCodeUses < uses {
			uses = MaxAuthCodeUses
		}

		// the auth code assumes the create time of the root jwt
		// this is to enable all derivative auth to be expired by expiring the root
		createTime := session.ByJwt.CreateTime
		endTime := server.NowUtc().Add(duration)

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
			INSERT INTO auth_code (
				auth_code_id,
	            network_id,
	            user_id,
	            auth_code,
	            create_time,
	            end_time,
	            uses,
	            remaining_uses,
	            principal
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8)
			`,
			authCodeId,
			session.ByJwt.NetworkId,
			session.ByJwt.UserId,
			authCode,
			createTime,
			endTime,
			uses,
			principal,
		))

		if 0 < len(roles) {
			server.BatchInTx(session.Ctx, tx, func(batch server.PgBatch) {
				for _, role := range roles {
					batch.Queue(
						`
						INSERT INTO auth_code_role (
							auth_code_id,
							role
						) VALUES ($1, $2)
						`,
						authCodeId,
						role,
					)
				}
			})
		}

		codeCreateResult = &AuthCodeCreateResult{
			AuthCode:        authCode,
			DurationMinutes: float64(duration) / float64(time.Minute),
			Uses:            uses,
		}
	})

	return
}

// removeExpiredAuthCodesBatchSize / removeExpiredVerifyCodesBatchSize bound each
// delete pass so the reap drains in bounded, short-locking transactions instead
// of deleting the whole expired backlog under one long lock. Vars (not consts)
// so tests can drive the multi-batch drain loop with a small batch.
var removeExpiredAuthCodesBatchSize = 10000
var removeExpiredVerifyCodesBatchSize = 50000

func RemoveExpiredAuthCodes(ctx context.Context, minTime time.Time) (authCodeCount int) {
	// LIMIT-batched drain: each pass selects at most a batch of expired auth
	// codes (ordered by end_time) and deletes them from auth_code + auth_code_role
	// in its own tx, until a pass removes fewer than the batch size.
	for {
		batchCount := 0
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				`
					SELECT
						auth_code_id
					FROM auth_code
					WHERE
						NOT active OR
						end_time < $1
					ORDER BY end_time
					LIMIT $2
				`,
				minTime.UTC(),
				removeExpiredAuthCodesBatchSize,
			)
			authCodeIds := []server.Id{}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var authCodeId server.Id
					server.Raise(result.Scan(&authCodeId))
					authCodeIds = append(authCodeIds, authCodeId)
				}
			})

			batchCount = len(authCodeIds)
			if batchCount == 0 {
				return
			}

			server.CreateTempTableInTx(ctx, tx, "temp_auth_code_id(auth_code_id uuid)", authCodeIds...)

			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM auth_code
				USING temp_auth_code_id
				WHERE auth_code.auth_code_id = temp_auth_code_id.auth_code_id
				`,
			))

			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM auth_code_role
				USING temp_auth_code_id
				WHERE auth_code_role.auth_code_id = temp_auth_code_id.auth_code_id
				`,
			))
		})

		authCodeCount += batchCount
		if batchCount < removeExpiredAuthCodesBatchSize {
			break
		}
	}

	return
}

func RemoveExpiredVerifyCodes(ctx context.Context, minTime time.Time) {
	verifyMinTime := server.NowUtc().Add(-VerifyCodeTimeout)
	// LIMIT-batched drain (ordered by verify_time, served by the
	// user_auth_verify_verify_time index), each pass its own tx until a pass
	// removes fewer than the batch size. Raise on error so a silently failing
	// cleanup cannot quietly let the table grow unbounded again (see
	// AuthVerifyCreateCode).
	for {
		batchCount := int64(0)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM user_auth_verify
				USING (
					SELECT user_auth_verify_id
					FROM user_auth_verify
					WHERE
						(used = true AND verify_time < $1)
						OR verify_time < $2
					ORDER BY verify_time
					LIMIT $3
				) t
				WHERE user_auth_verify.user_auth_verify_id = t.user_auth_verify_id
				`,
				minTime.UTC(),
				verifyMinTime.UTC(),
				removeExpiredVerifyCodesBatchSize,
			))
			batchCount = tag.RowsAffected()
		})
		if batchCount < int64(removeExpiredVerifyCodesBatchSize) {
			break
		}
	}

	return
}

type AuthCodeLoginArgs struct {
	AuthCode string `json:"auth_code,omitempty"`
}

type AuthCodeLoginResult struct {
	ByJwt string              `json:"by_jwt,omitempty"`
	Error *AuthCodeLoginError `json:"error,omitempty"`
}

type AuthCodeLoginError struct {
	Message string `json:"message,omitempty"`
}

func AuthCodeLogin(
	codeLogin *AuthCodeLoginArgs,
	session *session.ClientSession,
) (codeLoginResult *AuthCodeLoginResult, returnErr error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			session.Ctx,
			`
				SELECT
					auth_code.auth_code_id,
					auth_code.network_id,
					auth_code.user_id,
					auth_code.create_time,
					auth_code.remaining_uses,
					auth_code.principal,
					network.network_name

				FROM auth_code

				INNER JOIN network ON network.network_id = auth_code.network_id

				WHERE
					auth_code.auth_code = $1 AND
					auth_code.active = true AND
					$2 < auth_code.end_time
			`,
			codeLogin.AuthCode,
			server.NowUtc(),
		)

		exists := false
		var authCodeId server.Id
		var networkId server.Id
		var userId server.Id
		var createTime time.Time
		var remainingUses int
		var principal string
		var networkName string

		server.WithPgResult(result, err, func() {
			if result.Next() {
				exists = true
				result.Scan(
					&authCodeId,
					&networkId,
					&userId,
					&createTime,
					&remainingUses,
					&principal,
					&networkName,
				)
			}
		})
		if !exists {
			codeLoginResult = &AuthCodeLoginResult{
				Error: &AuthCodeLoginError{
					Message: "Invalid auth code.",
				},
			}
			return
		}

		roles := []string{}
		result, err = tx.Query(
			session.Ctx,
			`
				SELECT role FROM auth_code_role
				WHERE auth_code_id = $1
				ORDER BY role
			`,
			authCodeId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var role string
				server.Raise(result.Scan(&role))
				roles = append(roles, role)
			}
		})

		if 1 < remainingUses {
			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					UPDATE auth_code
					SET remaining_uses = remaining_uses - 1
					WHERE auth_code_id = $1
				`,
				authCodeId,
			))
		} else {
			// this was the last use
			// the safest approach is to just delete the auth code

			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					DELETE FROM auth_code
					WHERE auth_code_id = $1
				`,
				authCodeId,
			))

			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					DELETE FROM auth_code_role
					WHERE auth_code_id = $1
				`,
				authCodeId,
			))
		}

		isGuestMode := false

		isPro := IsPro(
			session.Ctx,
			&networkId,
		)

		byJwt := jwt.NewByJwtWithCreateTime(
			networkId,
			userId,
			networkName,
			createTime,
			isGuestMode,
			isPro,
		)
		byJwt.Roles = roles
		byJwt.Principal = principal

		codeLoginResult = &AuthCodeLoginResult{
			ByJwt: byJwt.Sign(),
		}
	})

	return
}

func GetUserAuth(ctx context.Context, networkId server.Id) (userAuth string, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_user.user_auth
				FROM network
				INNER JOIN network_user ON network_user.user_id = network.admin_user_id
				WHERE network.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var userAuth_ *string
				server.Raise(result.Scan(&userAuth_))
				if userAuth_ != nil {
					userAuth = *userAuth_
				} else {
					// jwt auth
					returnErr = errors.New("Missing user auth.")
				}
			}
		})
	})

	return
}
