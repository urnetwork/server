package model


import (
	"errors"
	"context"
	"bytes"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/jwt"
)


type AuthType string

const (
	AuthTypePassword AuthType = "password"
	AuthTypeApple AuthType = "apple"
	AuthTypeGoogle AuthType = "google"
	AuthTypeBringYour AuthType = "bringyour"
)


// compose this type into all arguments that must have an auth
type AuthArgs struct {
	ByJwt string  `json:"byJwt"`
}


func UserAuthAttempt(userAuth *string, session *bringyour.ClientSession) (*ulid.ULID, bool) {
	// insert attempt with success false
	// select attempts by userAuth in past 1 hour
	// select attempts by clientIp in past 1 hour
	// if more than 10 failed in any, return false

	bringyour.Logger().Printf("UserAuthAttempt 1")

	attemptLookbackCount := 100
	// 1 hour
	attemptLookbackSeconds := 60 * 60
	attemptFailedCountThreshold := 10

	userAuthAttemptId := ulid.Make()

	var ipv4DotNotation *string
	if session != nil {
		ipv4DotNotation = session.ClientIpv4DotNotation()
	}

	bringyour.Logger().Printf("UserAuthAttempt 2")

	bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
		bringyour.Logger().Printf("UserAuthAttempt 3")
		conn.Exec(
			context,
			`
				INSERT INTO user_auth_attempt
				(user_auth_attempt_id, user_auth, client_ipv4, success)
				VALUES ($1, $2, $3, $4)
			`,
			ulid.ToPg(&userAuthAttemptId),
			userAuth,
			ipv4DotNotation,
			false,
		)
		bringyour.Logger().Printf("UserAuthAttempt 4")
	})

	type UserAuthAttemptResult struct {
		attemptTime int64
		success bool
	}

	parseAttempts := func(result bringyour.PgResult)([]UserAuthAttemptResult) {
		attempts := []UserAuthAttemptResult{}
		var attemptTime int64
		var success bool
		for result.Next() {
			bringyour.Raise(
				result.Scan(&attemptTime, &success),
			)
			attempts = append(attempts, UserAuthAttemptResult{
				attemptTime: attemptTime,
				success: success,
			})
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

	bringyour.Logger().Printf("UserAuthAttempt 5")

	if userAuth != nil {
		bringyour.Logger().Printf("UserAuthAttempt 6")
		// lookback by user auth
		var attempts []UserAuthAttemptResult
		bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
			bringyour.Logger().Printf("UserAuthAttempt 7")
			result, err := conn.Query(
				context,
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
			bringyour.With(result, err, func() {
				attempts = parseAttempts(result)
			})
		})
		if !passesThreshold(attempts) {
			return &userAuthAttemptId, false
		}
	}

	clientIpv4DotNotation := session.ClientIpv4DotNotation()
	if clientIpv4DotNotation != nil {
		var attempts []UserAuthAttemptResult
		bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
			result, err := conn.Query(
				context,
				`
					SELECT 
						attempt_time,
						success
					FROM user_auth_attempt
					WHERE client_ipv4 = $1 AND now() - INTERVAL '1 seconds' * $2 <= attempt_time
					ORDER BY attempt_time DESC
					LIMIT $3
				`,
				clientIpv4DotNotation,
				attemptLookbackSeconds,
				attemptLookbackCount,
			)
			bringyour.With(result, err, func() {
				attempts = parseAttempts(result)
			})
		})
		if !passesThreshold(attempts) {
			return &userAuthAttemptId, false
		}
	}

	return &userAuthAttemptId, true
}

func SetUserAuthAttemptSuccess(userAuthAttemptId ulid.ULID, success bool) {
	bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
		conn.Exec(
			context,
			`
				UPDATE user_auth_attempt
				SET success = $1
				WHERE user_auth_attempt_id = $2
			`,
			success,
			ulid.ToPg(&userAuthAttemptId),
		)
	})
}

func maxUserAuthAttemptsError() error {
	return errors.New("User auth attempts exceeded limits.")
}


type AuthLoginArgs struct {
	UserAuth *string `json:"userAuth"`
	AuthJwtType *string `json:"authJwtType"`
	AuthJwt *string `json:"authJwt"`
}

type AuthLoginResult struct {
	UserName *string `json:"userName,omitempty"`
	UserAuth *string `json:"userAuth,omitempty"`
	AuthAllowed *[]string `json:"authAllowed,omitempty"`
	Error *AuthLoginResultError `json:"error,omitempty"`
	Network *AuthLoginResultNetwork `json:"network,omitempty"`
}

type AuthLoginResultError struct {
	SuggestedUserAuth *string `json:"suggestedUserAuth,omitempty"`
	Message string `json:"message"`
}

type AuthLoginResultNetwork struct {
	ByJwt string `json:"byJwt"`
}

func AuthLogin(login AuthLoginArgs, session *bringyour.ClientSession) (*AuthLoginResult, error) {
	userAuth, _ := NormalUserAuthV1(login.UserAuth)

	var userAuthAttemptId *ulid.ULID
	if session != nil {
		var allow bool
		userAuthAttemptId, allow = UserAuthAttempt(userAuth, session)
		if !allow {
			return nil, maxUserAuthAttemptsError()
		}
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
		bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
			result, err := conn.Query(
				context,
				`
					SELECT auth_type FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			bringyour.With(result, err, func() {
				if result.Next() {
					bringyour.Raise(
						result.Scan(&authType),
					)
				}
			})
		})
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
			var userId bringyour.PgUUID
			var authType *string
			var networkId bringyour.PgUUID
			var networkName *string
			bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
				bringyour.Logger().Printf("Matching user auth %s\n", authJwt.UserAuth)
				result, err := conn.Query(
					context,
					`
						SELECT
							network_user.user_id AS user_id,
							network_user.auth_type AS auth_type,
							network.network_id AS network_id,
							network.network_name AS network_name
						FROM network_user
						INNER JOIN network ON network.admin_user_id = network_user.user_id
						WHERE user_auth = $1
					`,
					authJwt.UserAuth,
				)
				bringyour.With(result, err, func() {
					if result.Next() {
						bringyour.Raise(
							result.Scan(&userId, &authType, &networkId, &networkName),
						)
					}
				})
			})

			if authType == nil {
				// new user
				return &AuthLoginResult{
					UserName: &authJwt.UserName,
				}, nil
			} else if AuthType(*authType) == authJwt.AuthType {
				if userAuthAttemptId != nil {
					SetUserAuthAttemptSuccess(*userAuthAttemptId, true)
				}

				// successful login
				byJwt := jwt.NewByJwt(
					*ulid.FromPg(networkId),
					*ulid.FromPg(userId),
					*networkName,
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
					AuthAllowed: &[]string{*authType},
				}
				return result, nil
			}
		}
	}

	return nil, errors.New("Invalid login.")
}


type AuthLoginWithPasswordArgs struct {
	UserAuth string `json:"userAuth"`
	Password string `json:"password"`
}

type AuthLoginWithPasswordResult struct {
	ValidationRequired *AuthLoginWithPasswordResultValidation `json:"validationRequired,omitempty"`
	Network *AuthLoginWithPasswordResultNetwork `json:"network,omitempty"`
	Error *AuthLoginWithPasswordResultError `json:"error,omitempty"`
}

type AuthLoginWithPasswordResultValidation struct {
	UserAuth string `json:"userAuth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt *string `json:"byJwt,omitempty"`
	NetworkName *string `json:"name,omitempty"`
}

type AuthLoginWithPasswordResultError struct {
	Message string `json:"message"`
}

func AuthLoginWithPassword(
	loginWithPassword AuthLoginWithPasswordArgs,
	session *bringyour.ClientSession,
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

	var userId bringyour.PgUUID
	var passwordHash *[]byte
	var passwordSalt *[]byte
	var userValidated bool
	var networkId bringyour.PgUUID
	var networkName *string

	bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
		result, err := conn.Query(
			context,
			`
				SELECT
					network_user.user_id AS user_id,
					network_user.password_hash AS password_hash,
					network_user.password_salt AS password_salt,
					network_user.validated AS validated,
					network.network_id AS network_id,
					network.network_name AS network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_auth = $1
			`,
			userAuth,
		)
		bringyour.With(result, err, func() {
			if result.Next() {
				bringyour.Raise(
					result.Scan(&userId, &passwordHash, &passwordSalt, &userValidated, &networkId, &networkName),
				)
			}
		})
	})

	if !userId.Valid {
		return nil, errors.New("User does not exist.")
	}

	bringyour.Logger().Printf("Comparing password hashes\n")
	loginPasswordHash := computePasswordHashV1([]byte(loginWithPassword.Password), *passwordSalt)
	if bytes.Equal(*passwordHash, loginPasswordHash) {
		if userValidated {
			SetUserAuthAttemptSuccess(*userAuthAttemptId, true)

			// success
			byJwt := jwt.NewByJwt(
				*ulid.FromPg(networkId),
				*ulid.FromPg(userId),
				*networkName,
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
				ValidationRequired: &AuthLoginWithPasswordResultValidation{
					UserAuth: *userAuth,
				},
				Network: &AuthLoginWithPasswordResultNetwork{
					NetworkName: networkName,
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



type AuthValidateArgs struct {
	UserAuth string `json:"userAuth"`
	ValidateCode string `json:"validateCode"`
}

type AuthValidateResult struct {
	Network *AuthValidateResultNetwork `json:"network,omitempty"`
	Error *AuthValidateResultError `json:"error,omitempty"`
}

type AuthValidateResultNetwork struct {
	ByJwt string `json:"byJwt"`
}

type AuthValidateResultError struct {
	Message string `json:"message"`
}

func AuthValidate(validate AuthValidateArgs, session *bringyour.ClientSession) (*AuthValidateResult, error) {
	userAuth, _ := NormalUserAuthV1(&validate.UserAuth)

	if userAuth == nil {
		result := &AuthValidateResult{
			Error: &AuthValidateResultError{
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
	validateValidSeconds := 60 * 60 * 4

	var userId bringyour.PgUUID
	var userAuthValidateId bringyour.PgUUID
	var networkId bringyour.PgUUID
	var networkName *string

	bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
		result, err := conn.Query(
			context,
			`
				SELECT
					network_user.user_id AS user_id,
					user_auth_validate.user_auth_validate_id AS user_auth_validate_id,
					network.network_id AS network_id,
					network.network_name AS network_name
				FROM network_user
				INNER JOIN user_auth_validate ON
					user_auth_validate.user_id = network_user.user_id AND 
					user_auth_validate.validate_code = $1 AND
					used = false AND
					now() - INTERVAL '1 seconds' * $2 <= user_auth_validate.validate_time
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_auth = $3
			`,
			validate.ValidateCode,
			validateValidSeconds,
			userAuth,
		)
		bringyour.With(result, err, func() {
			if result.Next() {
				bringyour.Raise(
					result.Scan(&userId, &userAuthValidateId, &networkId, &networkName),
				)
			}
		})
	})

	if userAuthValidateId.Valid {
		// validated

		bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
			var err error

			_, err = tx.Exec(
				context,
				`
					UPDATE network_user
					SET validated = true
					WHERE user_id = $1
				`,
				userId,
			)
			bringyour.Raise(err)

			_, err = tx.Exec(
				context,
				`
					UPDATE user_auth_validate
					SET used = true
					WHERE user_auth_validate_id = $1
				`,
				userAuthValidateId,
			)
			bringyour.Raise(err)
		})

		SetUserAuthAttemptSuccess(*userAuthAttemptId, true)

		byJwt := jwt.NewByJwt(
			*ulid.FromPg(networkId),
			*ulid.FromPg(userId),
			*networkName,
		)
		result := &AuthValidateResult{
			Network: &AuthValidateResultNetwork{
				ByJwt: byJwt.Sign(),
			},
		}
		return result, nil
	} else {
		result := &AuthValidateResult{
			Error: &AuthValidateResultError{
				Message: "Invalid code.",
			},
		}
		return result, nil
	}
}




type AuthValidateCreateCodeArgs struct {
	UserAuth string `json:"userAuth"`
}

type AuthValidateCreateCodeResult struct {
	ValidateCode *string `json:"validateCode,omitempty"`
	Error *AuthValidateCreateCodeError `json:"error,omitempty"`
}

type AuthValidateCreateCodeError struct {
	Message string `json:"message"`
}

func AuthValidateCreateCode(
	validateCreateCode AuthValidateCreateCodeArgs,
	session *bringyour.ClientSession,
) (*AuthValidateCreateCodeResult, error) {
	userAuth, _ := NormalUserAuthV1(&validateCreateCode.UserAuth)

	if userAuth == nil {
		result := &AuthValidateCreateCodeResult{
			Error: &AuthValidateCreateCodeError{
				Message: "Invalid user auth.",
			},
		}
		return result, nil
	}

	created := false
	var validateCode string

	bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
		var result bringyour.PgResult
		var err error

		var userId bringyour.PgUUID
		result, err = tx.Query(
			context,
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		bringyour.With(result, err, func() {
			if result.Next() {
				bringyour.Raise(
					result.Scan(&userId),
				)
			}
		})

		if userId.Valid {
			// delete existing codes and create a new code

			_, err = tx.Exec(
				context,
				`
					UPDATE user_auth_validate
					SET used = true
					WHERE user_id = $1
				`,
				userId,
			)
			bringyour.Raise(err)

			created = true
			userAuthValidateId := ulid.Make()
			validateCode = createValidateCode()
			_, err = tx.Exec(
				context,
				`
					INSERT INTO user_auth_validate
					(user_auth_validate_id, user_id, validate_code)
					VALUES ($1, $2, $3)
				`,
				ulid.ToPg(&userAuthValidateId),
				userId,
				validateCode,
			)
			bringyour.Raise(err)
		}
	})

	if created {
		result := &AuthValidateCreateCodeResult{
			ValidateCode: &validateCode,
		}
		return result, nil
	}

	return nil, errors.New("Invalid login.")
}


type AuthPasswordResetCreateCodeArgs struct {
	UserAuth string `json:"error"`
}

type AuthPasswordResetCreateCodeResult struct {
	ResetCode *string `json:"resetCode,omitempty"`
	Error *AuthPasswordResetCreateCodeError `json:"error,omitempty"`
}

type AuthPasswordResetCreateCodeError struct {
	Message string `json:"message"`
}

func AuthPasswordResetCreateCode(
	resetCreateCode AuthPasswordResetCreateCodeArgs,
	session *bringyour.ClientSession,
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

	bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
		var result bringyour.PgResult
		var err error

		var userId bringyour.PgUUID
		result, err = tx.Query(
			context,
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		bringyour.With(result, err, func() {
			if result.Next() {
				bringyour.Raise(
					result.Scan(&userId),
				)
			}
		})

		if userId.Valid {
			// delete existing codes and create a new code

			_, err = tx.Exec(
				context,
				`
					UPDATE user_auth_reset
					SET used = true
					WHERE user_id = $1
				`,
				userId,
			)
			bringyour.Raise(err)

			created = true
			userAuthResetId := ulid.Make()
			resetCode = createResetCode()
			_, err = tx.Exec(
				context,
				`
					INSERT INTO user_auth_reset
					(user_auth_reset_id, user_id, reset_code)
					VALUES ($1, $2, $3)
				`,
				ulid.ToPg(&userAuthResetId),
				userId,
				resetCode,
			)
			bringyour.Raise(err)
		}
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
	ResetCode string `json:"resetCode"`
	Password string `json:"password"`
}

// IMPORTANT do not return this to the client.
// The result of setting the password must not reveal the user auth to the client, in case the reset code was guessed
type AuthPasswordSetResult struct {
	NetworkId ulid.ULID
}

func AuthPasswordSet(
	passwordSet AuthPasswordSetArgs,
	session *bringyour.ClientSession,
) (*AuthPasswordSetResult, error) {
	userAuthAttemptId, allow := UserAuthAttempt(nil, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	// 4 hours
	resetValidSeconds := 60 * 60 * 4

	var userId bringyour.PgUUID
	var userAuthResetId bringyour.PgUUID
	var networkId bringyour.PgUUID
	var networkName *string

	bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
		result, err := conn.Query(
			context,
			`
				SELECT
					network_user.user_id AS user_id,
					user_auth_reset.user_auth_reset_id AS user_auth_reset_id,
					network.network_id AS network_id,
					network.network_name AS network_name
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
		bringyour.With(result, err, func() {
			if result.Next() {
				bringyour.Raise(
					result.Scan(&userId, &userAuthResetId, &networkId, &networkName),
				)
			}
		})
	})

	if userAuthResetId.Valid {
		// valid reset code

		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(passwordSet.Password), passwordSalt)

		bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
			var err error

			_, err = tx.Exec(
				context,
				`
					UPDATE network_user
					SET password_hash = $1, password_salt = $2
					WHERE user_id = $3
				`,
				passwordHash,
				passwordSalt,
				userId,
			)
			bringyour.Raise(err)

			_, err = tx.Exec(
				context,
				`
					UPDATE user_auth_reset
					SET used = true
					WHERE user_auth_reset_id = $1
				`,
				userAuthResetId,
			)
			bringyour.Raise(err)
		})

		SetUserAuthAttemptSuccess(*userAuthAttemptId, true)

		result := &AuthPasswordSetResult{
			NetworkId: *ulid.FromPg(networkId),
		}
		return result, nil
	}

	return nil, errors.New("Invalid login.")
}

