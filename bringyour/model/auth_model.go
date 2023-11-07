package model


import (
	"errors"
	"context"
	"bytes"
	// "strings"
	// "strconv"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/jwt"
)


type AuthType = string

const (
	AuthTypePassword AuthType = "password"
	AuthTypeApple AuthType = "apple"
	AuthTypeGoogle AuthType = "google"
	AuthTypeBringYour AuthType = "bringyour"
)


func UserAuthAttempt(
	userAuth *string,
	session *session.ClientSession,
) (bringyour.Id, bool) {
	// insert attempt with success false
	// select attempts by userAuth in past 1 hour
	// select attempts by clientIp in past 1 hour
	// if more than 10 failed in any, return false

	attemptLookbackCount := 100
	// 1 hour
	attemptLookbackSeconds := 60 * 60
	attemptFailedCountThreshold := 100

	userAuthAttemptId := bringyour.NewId()

	clientIp, clientPort := session.ClientIpPort()

	bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO user_auth_attempt
				(user_auth_attempt_id, user_auth, client_ip, client_port, success)
				VALUES ($1, $2, $3, $4, $5)
			`,
			userAuthAttemptId,
			userAuth,
			clientIp,
			clientPort,
			false,
		))
	}))

	type UserAuthAttemptResult struct {
		attemptTime time.Time
		success bool
	}

	parseAttempts := func(result bringyour.PgResult)([]UserAuthAttemptResult) {
		attempts := []UserAuthAttemptResult{}
		for result.Next() {
			var attempt UserAuthAttemptResult
			bringyour.Raise(result.Scan(
				&attempt.attemptTime,
				&attempt.success,
			))
			attempts = append(attempts, attempt)
		}
		return attempts
	}

	passesThreshold := func(attempts []UserAuthAttemptResult)(bool) {
		failedCount := 0
		for i := 0; i < len(attempts); i+= 1 {
			if !attempts[i].success {
				failedCount += 1
			}
		}
		return failedCount <= attemptFailedCountThreshold
	}

	if userAuth != nil {
		// lookback by user auth
		var attempts []UserAuthAttemptResult
		bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
			result, err := conn.Query(
				session.Ctx,
				`
					SELECT 
						attempt_time,
						success
					FROM user_auth_attempt
					WHERE user_auth = $1 AND now() - INTERVAL '1 seconds' * $2 <= attempt_time
					ORDER BY attempt_time DESC
					LIMIT $3
				`,
				userAuth,
				attemptLookbackSeconds,
				attemptLookbackCount,
			)
			bringyour.WithPgResult(result, err, func() {
				attempts = parseAttempts(result)
			})
		}))
		if !passesThreshold(attempts) {
			return userAuthAttemptId, false
		}
	}

	var attempts []UserAuthAttemptResult
	bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT 
					attempt_time,
					success
				FROM user_auth_attempt
				WHERE client_ip = $1 AND now() - INTERVAL '1 seconds' * $2 <= attempt_time
				ORDER BY attempt_time DESC
				LIMIT $3
			`,
			clientIp,
			attemptLookbackSeconds,
			attemptLookbackCount,
		)
		bringyour.WithPgResult(result, err, func() {
			attempts = parseAttempts(result)
		})
	}))
	if !passesThreshold(attempts) {
		return userAuthAttemptId, false
	}

	return userAuthAttemptId, true
}


func SetUserAuthAttemptSuccess(
	ctx context.Context,
	userAuthAttemptId bringyour.Id,
	success bool,
) {
	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE user_auth_attempt
				SET success = $1
				WHERE user_auth_attempt_id = $2
			`,
			success,
			userAuthAttemptId,
		))
	}))
}


func maxUserAuthAttemptsError() error {
	return errors.New("User auth attempts exceeded limits.")
}


type AuthLoginArgs struct {
	UserAuth *string `json:"user_auth,omitempty"`
	AuthJwtType *string `json:"auth_jwt_type,omitempty"`
	AuthJwt *string `json:"auth_jwt,omitempty"`
}

type AuthLoginResult struct {
	UserName *string `json:"user_name,omitempty"`
	UserAuth *string `json:"user_auth,omitempty"`
	AuthAllowed *[]string `json:"auth_allowed,omitempty"`
	Error *AuthLoginResultError `json:"error,omitempty"`
	Network *AuthLoginResultNetwork `json:"network,omitempty"`
}

type AuthLoginResultError struct {
	SuggestedUserAuth *string `json:"suggested_user_auth,omitempty"`
	Message string `json:"message"`
}

type AuthLoginResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

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
		if userAuth == nil {
			result := &AuthLoginResult{
				Error: &AuthLoginResultError{
					Message: "Invalid email or phone number.",
				},
			}
			return result, nil
		}

		var authType *string
		bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
			result, err := conn.Query(
				session.Ctx,
				`
					SELECT auth_type FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			bringyour.WithPgResult(result, err, func() {
				if result.Next() {
					bringyour.Raise(result.Scan(&authType))
				}
			})
		}))
		if authType == nil {
			// new user
			result := &AuthLoginResult{
				UserAuth: userAuth,
			}
			return result, nil
		} else {
			// existing user
			result := &AuthLoginResult{
				UserAuth: userAuth,
				AuthAllowed: &[]string{*authType},
			}
			return result, nil
		}
	} else if login.AuthJwt != nil && login.AuthJwtType != nil {
		bringyour.Logger().Printf("login JWT %s %s\n", *login.AuthJwt, *login.AuthJwtType)
		authJwt := ParseAuthJwt(*login.AuthJwt, AuthType(*login.AuthJwtType))
		if authJwt != nil {
			var userId *bringyour.Id
			var authType string
			var networkId bringyour.Id
			var networkName string
			bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
				bringyour.Logger().Printf("Matching user auth %s\n", authJwt.UserAuth)
				result, err := conn.Query(
					session.Ctx,
					`
						SELECT
							network_user.user_id,
							network_user.auth_type,
							network.network_id,
							network.network_name
						FROM network_user
						INNER JOIN network ON network.admin_user_id = network_user.user_id
						WHERE user_auth = $1
					`,
					authJwt.UserAuth,
				)
				bringyour.WithPgResult(result, err, func() {
					if result.Next() {
						bringyour.Raise(result.Scan(
							&userId,
							&authType,
							&networkId,
							&networkName,
						))
					}
				})
			}))

			if userId == nil {
				// new user
				return &AuthLoginResult{
					UserName: &authJwt.UserName,
				}, nil
			} else if AuthType(authType) == authJwt.AuthType {
				SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

				// successful login
				byJwt := jwt.NewByJwt(
					networkId,
					*userId,
					networkName,
				)
				result := &AuthLoginResult{
					Network: &AuthLoginResultNetwork{
						ByJwt: byJwt.Sign(),
					},
				}
				return result, nil
			} else {
				// existing user, different auth type
				result := &AuthLoginResult{
					UserAuth: &authJwt.UserAuth,
					AuthAllowed: &[]string{authType},
				}
				return result, nil
			}
		}
	}

	return nil, errors.New("Invalid login.")
}


type AuthLoginWithPasswordArgs struct {
	UserAuth string `json:"user_auth"`
	Password string `json:"password"`
}

type AuthLoginWithPasswordResult struct {
	VerificationRequired *AuthLoginWithPasswordResultVerification `json:"verification_required,omitempty"`
	Network *AuthLoginWithPasswordResultNetwork `json:"network,omitempty"`
	Error *AuthLoginWithPasswordResultError `json:"error,omitempty"`
}

type AuthLoginWithPasswordResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt *string `json:"by_jwt,omitempty"`
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

	var userId *bringyour.Id
	var passwordHash []byte
	var passwordSalt []byte
	var userVerified bool
	var networkId bringyour.Id
	var networkName string

	bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					network_user.user_id,
					network_user.password_hash,
					network_user.password_salt,
					network_user.verified,
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_auth = $1
			`,
			userAuth,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(
					&userId,
					&passwordHash,
					&passwordSalt,
					&userVerified,
					&networkId,
					&networkName,
				))
			}
		})
	}))

	if userId == nil {
		return nil, errors.New("User does not exist.")
	}

	bringyour.Logger().Printf("Comparing password hashes\n")
	loginPasswordHash := computePasswordHashV1([]byte(loginWithPassword.Password), passwordSalt)
	if bytes.Equal(passwordHash, loginPasswordHash) {
		if userVerified {
			SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

			// success
			byJwt := jwt.NewByJwt(
				networkId,
				*userId,
				networkName,
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

	// return nil, errors.New("Invalid login.")
}



type AuthVerifyArgs struct {
	UserAuth string `json:"user_auth"`
	VerifyCode string `json:"verify_code"`
}

type AuthVerifyResult struct {
	Network *AuthVerifyResultNetwork `json:"network,omitempty"`
	Error *AuthVerifyResultError `json:"error,omitempty"`
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

	// 4 hours
	verifyValidSeconds := 60 * 60 * 4

	var userId bringyour.Id
	var userAuthVerifyId *bringyour.Id
	var networkId bringyour.Id
	var networkName string

	bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
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
			verify.VerifyCode,
			verifyValidSeconds,
			userAuth,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(
					&userId,
					&userAuthVerifyId,
					&networkId,
					&networkName,
				))
			}
		})
	}))

	if userAuthVerifyId == nil {
		result := &AuthVerifyResult{
			Error: &AuthVerifyResultError{
				Message: "Invalid code.",
			},
		}
		return result, nil
	}

	// verified
	bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network_user
				SET verified = true
				WHERE user_id = $1
			`,
			userId,
		))

		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_verify
				SET used = true
				WHERE user_auth_verify_id = $1
			`,
			userAuthVerifyId,
		))
	}))

	SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

	byJwt := jwt.NewByJwt(
		networkId,
		userId,
		networkName,
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
}

type AuthVerifyCreateCodeResult struct {
	VerifyCode *string `json:"verify_code,omitempty"`
	Error *AuthVerifyCreateCodeError `json:"error,omitempty"`
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

	created := false
	var verifyCode string

	bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		var result bringyour.PgResult
		var err error

		var userId *bringyour.Id
		result, err = tx.Query(
			session.Ctx,
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&userId))
			}
		})

		if userId == nil {
			return
		}
		
		// delete existing codes and create a new code
		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_verify
				SET used = true
				WHERE user_id = $1
			`,
			userId,
		))

		created = true
		userAuthVerifyId := bringyour.NewId()
		verifyCode = createVerifyCode()
		bringyour.RaisePgResult(tx.Exec(
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
	}))

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
	ResetCode *string `json:"reset_code,omitempty"`
	Error *AuthPasswordResetCreateCodeError `json:"error,omitempty"`
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

	created := false
	var resetCode string

	bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		var result bringyour.PgResult
		var err error

		var userId *bringyour.Id
		result, err = tx.Query(
			session.Ctx,
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&userId))
			}
		})

		if userId == nil {
			return
		}

		// delete existing codes and create a new code
		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_reset
				SET used = true
				WHERE user_id = $1
			`,
			userId,
		))

		created = true
		userAuthResetId := bringyour.NewId()
		resetCode = createResetCode()
		bringyour.RaisePgResult(tx.Exec(
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
	}))

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
	Password string `json:"password"`
}

// IMPORTANT do not return this to the client.
// The result of setting the password must not reveal the user auth to the client, in case the reset code was guessed
type AuthPasswordSetResult struct {
	NetworkId bringyour.Id
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

	var userId bringyour.Id
	var userAuthResetId *bringyour.Id
	var networkId bringyour.Id
	var networkName string

	bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
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
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(
					&userId,
					&userAuthResetId,
					&networkId,
					&networkName,
				))
			}
		})
	}))

	if userAuthResetId == nil {
		return nil, errors.New("Invalid login.")
	}

	// valid reset code
	passwordSalt := createPasswordSalt()
	passwordHash := computePasswordHashV1([]byte(passwordSet.Password), passwordSalt)

	bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network_user
				SET password_hash = $1, password_salt = $2
				WHERE user_id = $3
			`,
			passwordHash,
			passwordSalt,
			userId,
		))

		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE user_auth_reset
				SET used = true
				WHERE user_auth_reset_id = $1
			`,
			userAuthResetId,
		))
	}))

	SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

	result := &AuthPasswordSetResult{
		NetworkId: networkId,
	}
	return result, nil
}

