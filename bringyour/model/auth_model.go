package model


import (
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/pgtype"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
)


type AuthType string

const (
	AuthTypePassword AuthType = "password"
	AuthTypeApple AuthType = "apple"
	AuthTypeGoogle AuthType = "google"
)



func UserAuthAttempt(userAuth *string, session bringyour.ClientSession) (ulid.ULID, bool) {
	// insert attempt with success false
	// select attempts by userAuth in past 1 hour
	// select attempts by clientIp in past 1 hour
	// if more than 10 failed in any, return false

	attemptLookbackCount := 100
	// 1 hour
	attemptLookbackSeconds := 60 * 60
	attemptFailedCountThreshold := 10

	userAuthAttemptId := ulid.Make()

	bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
			`
				INSERT INTO user_auth_attempt
				(user_auth_attempt_id, user_auth, client_ipv4, success)
				VALUES ($1, $2, $3, $4)
			`,
			ulid.ToPg(userAuthAttemptId),
			userAuth,
			session.ClientIpv4DotNotation(),
			false,
		)
	})

	type UserAuthAttemptResult struct {
		attemptTime int64
		success bool
	}

	func parseAttempts(conn *pgxpool.Conn) []UserAuthAttemptResult {
		attempts := []UserAuthAttemptResult{}
		var attemptTime int64
		var success bool
		for conn.Next() {
			conn.Scan(&attemptTime, &success)
			attempts = append(attempts, UserAuthAttemptResult{
				attemptTime: attemptTime,
				success: success,
			})
		}
		return attempts
	}

	func passesThreshold(attempts []UserAuthAttemptResult) bool {
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
		bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
			conn.Exec(
				`
					SELECT 
						attempt_time,
						success
					FROM user_auth_attempt
					WHERE user_auth = $1 AND now() - INTERVAL($2 seconds) <= attempt_time
					ORDER BY attempt_time DESC
					LIMIT $3
				`,
				userAuth,
				attemptLookbackSeconds,
				attemptLookbackCount,
			)
			attempts = parseAttempts(conn)
		})
		if !passesThreshold(attempts) {
			return userAuthAttemptId, false
		}
	}

	clientIpv4DotNotation := session.ClientIpv4DotNotation()
	if clientIpv4DotNotation != nil {
		var attempts []UserAuthAttemptResult
		bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
			conn.Exec(
				`
					SELECT 
						attempt_time,
						success
					FROM user_auth_attempt
					WHERE client_ipv4 = $1 AND now() - INTERVAL($2 seconds) <= attempt_time
					ORDER BY attempt_time DESC
					LIMIT $3
				`,
				clientIpv4DotNotation,
				attemptLookbackSeconds,
				attemptLookbackCount,
			)
			attempts = parseAttempts(conn)
		})
		if !passesThreshold(attempts) {
			return userAuthAttemptId, false
		}
	}

	return userAuthAttemptId, true
}

func SetUserAuthAttemptSuccess(userAuthAttemptId ulid.ULID, success bool) {
	bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
			`
				UPDATE user_auth_attempt
				SET success = $1
				WHERE user_auth_attempt_id = $2
			`,
			success,
			ulid.ToPg(userAuthAttemptId),
		)
	})
}

func maxUserAuthAttemptsError() error {
	return error("User auth attempts exceeded limits.")
}


type AuthLoginArgs struct {
	UserAuth *string `json:"userAuth"`
	AuthJwtType *string `json:"authJwtType"`
	AuthJwt *string `json:"authJwt"`
}

type AuthLoginResult struct {
	AuthAllowed *[]string `json:"authAllowed"`
	Error *AuthLoginResultError `json:"error"`
	Network *AuthLoginResultNetwork `json:"network"`
}

type AuthLoginResultError struct {
	SuggestedUserAuth *string `json:"suggestedUserAuth"`
	Message string `json:"message"`
}

type AuthLoginResultNetwork struct {
	ByJwt string `json:"byJwt"`
}

func AuthLogin(login AuthLoginArgs, session *bringyour.ClientSession) (*AuthLoginResult, error) {
	userAuth, userAuthType := normalUserAuthV1(login.UserAuth)

	if userAuth == nil {
		result := &AuthLoginResult{
			Error: &AuthLoginResultError{
				Message: "Invalid user auth."
			}
		}
		return result, nil
	}

	if session != nil {
		_, allow := UserAuthAttempt(userAuth, *session)
		if !allow {
			return nil, maxUserAuthAttemptsError()
		}
	}

	if userAuth != nil {
		var authType *string
		bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
			conn.Exec(
				`
					SELECT auth_type FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			if conn.Next() {
				conn.Scan(&authType)
			}
		})
		if authType == nil {
			// new user
			result := &AuthLoginResult{}
			return result, nil
		} else {
			// existing user
			result := &AuthLoginResult{
				AuthAllowed: &[]string{*authType},
			}
			return result, nil
		}
	} else if login.authJwt != nil {
		authJwt := ParseAuthJwt(login.authJwt, login.authJwtType)
		if authJwt != nil {
			var userId pgtype.UUID
			var authType *string
			var networkId pgtype.UUID
			var networkName *string
			bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
				conn.Exec(
					`
						SELECT
							network_user.user_id AS user_id,
							network_user.auth_type AS auth_type,
							network.network_id AS network_id
							network.network_name AS network_name
						FROM network_user
						INNER JOIN network ON network.admin_user_id = network_user.user_id
						WHERE user_auth = $1
					`,
					authJwt.userAuth,
				)
				if conn.Next() {
					conn.Scan(&userId, &authType, &networkId)
				}
			})

			if authType == nil {
				// new user
				return &AuthLoginResult{}
			} else if authType == authJwt.authType {
				SetUserAuthAttemptSuccess(userAuthAttemptId, true)

				// successful login
				byJwt := CreateByJwt(
					*ulid.FromPg(networkId),
					*ulid.FromPg(userId),
					*networkName
				)
				result := &AuthLoginResult{
					Network: &AuthLoginResultNetwork{
						ByJwt: byJwt
					}
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

	return nil, error("Invalid login.")
}


type AuthLoginWithPasswordArgs struct {
	UserAuth string `json:"userAuth"`
	Password string `json:"password"`
}

type AuthLoginWithPasswordResult struct {
	ValidationRequired *AuthLoginWithPasswordResultValidation `json:"validationRequired"`
	Network *AuthLoginWithPasswordResultNetwork `json:"network"`
	Error *AuthLoginWithPasswordResultError `json:"error"`
}

type AuthLoginWithPasswordResultValidation struct {
	UserAuth string `json:"userAuth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt *string `json:"byJwt"`
	Name *string `json:"name"`
}

type AuthLoginWithPasswordResultError struct {
	Message string `json:"message"`
}

func AuthLoginWithPassword(
	loginWithPassword AuthLoginWithPasswordArgs,
	session *bringyour.ClientSession,
) (*AuthLoginWithPasswordResult, error) {
	userAuth, _ := normalUserAuthV1(loginWithPassword.userAuth)

	if userAuth == nil {
		result := &AuthLoginWithPasswordResult{
			Error: &AuthLoginWithPasswordResultError{
				Message: "Invalid user auth."
			}
		}
		return result, nil
	}

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	var userId pgtype.UUID
	var passwordHash *[]byte
	var passwordSalt *[]byte
	var userValidated bool
	var networkId pgtype.UUID
	var networkName *string

	bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
			`
				SELECT
					network_user.user_id AS user_id,
					network_user.password_hash AS password_hash,
					network_user.password_salt AS password_salt,
					network_user.validated AS validated,
					network.network_id AS network_id
					network.network_name AS network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_auth = $1
			`,
			userAuth,
		)
		if conn.Next() {
			conn.Scan(&userId, &passwordHash, &passwordSalt, &networkId, &networkName)
		}
	})

	if userId == nil {
		return nil, error("User does not exist.")
	}

	loginPasswordHash := computePasswordHashV1(loginWithPassword.password, passwordSalt)
	if bytes.equal(passwordHash, loginPasswordHash) {
		if userValidated {
			SetUserAuthAttemptSuccess(userAuthAttemptId, true)

			// success
			byJwt := CreateByJwt(
				*ulid.FromPg(networkId),
				*ulid.FromPg(userId),
				*networkName,
			)
			return &AuthLoginWithPasswordResult{
				network: &AuthLoginWithPasswordResultNetwork{
					ByJwt: byJwt
				},
			}
		} else {
			return &AuthLoginWithPasswordResult{
				validationRequired: &AuthLoginWithPasswordResultValidation{
					userAuth: *userAuth
				},
				network: &AuthLoginWithPasswordResultNetwork{
					NetworkName: networkName
				},
			}
		}
	}

	return nil, error("Invalid login.")
}



type AuthValidateArgs struct {
	UserAuth string `json:"userAuth"`
	ValidateCode string `json:"validateCode"`
}

type AuthValidateResult struct {
	Network *AuthValidateResultNetwork `json:"network"`
	Error *AuthValidateResultError `json:"error"`
}

type AuthValidateResultNetwork struct {
	ByJwt string `json:"byJwt"`
}

type AuthValidateResultError struct {
	Message string `json:"message"`
}

func AuthValidate(validate AuthValidateArgs, session *bringyour.ClientSession) (*AuthValidateResult, error) {
	userAuth, _ := normalUserAuthV1(loginWithPassword.userAuth)

	if userAuth == nil {
		result := &AuthValidateResult{
			Error: &AuthValidateResultError{
				Message: "Invalid user auth."
			}
		}
		return result, nil
	}

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	// 4 hours
	validateValidSeconds := 60 * 60 * 4

	var userId pgtype.UUID
	var userAuthValidateId pgtype.UUID
	var networkId pgtype.UUID
	var networkName *string

	bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
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
					now() - ($2 seconds) <= user_auth_validate.validate_time
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE user_auth = $3
			`,
			validate.validateCode,
			validateValidSeconds,
			userAuth,
		)
		if conn.Next() {
			conn.Scan(&userId, &passwordHash, &passwordSalt, &networkId)
		}
	})

	if userAuthValidateId.Valid {
		// validated

		bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
			conn.Exec(
				`
					UPDATE network_user
					SET validated = true
					WHERE user_id = $1
				`,
				userId,
			)
			conn.Exec(
				`
					UPDATE user_auth_validate
					SET used = true
					WHERE user_auth_validate_id = $1
				`,
				userAuthValidateId,
			)
		})

		SetUserAuthAttemptSuccess(userAuthAttemptId, true)

		byJwt := CreateByJwt(
			*ulid.FromPg(networkId),
			*ulid.FromPg(userId),
			*networkName,
		)
		return &AuthValidateResult{
			Network: &AuthValidateResultNetwork{
				ByJwt: byJwt
			},
		}
	} else {
		result := &AuthValidateResult{
			Error: &AuthValidateResultError{
				Message: "Invalid code."
			}
		}
		return result, nil
	}
}




type AuthValidateCreateCodeArgs struct {
	UserAuth string `json:"userAuth"`
}

type AuthValidateCreateCodeResult struct {
	ValidateCode *string `json:"validateCode"`
	Error *AuthValidateCreateCodeError `json:"error"`
}

type AuthValidateCreateCodeError struct {
	Message string `json:"message"`
}

func AuthValidateCreateCode(
	validateCreateCode model.AuthValidateCreateCodeArgs,
	session *bringyour.ClientSession,
) (model.AuthValidateCreateCodeResult, error) {
	userAuth, _ := normalUserAuthV1(validateCreateCode.userAuth)

	if userAuth == nil {
		result := &AuthValidateCreateCodeResult{
			Error: &AuthValidateCreateCodeError{
				Message: "Invalid user auth."
			}
		}
		return result, nil
	}

	var validateCode *string

	bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
		var userId pgtype.UUID
		conn.Exec(
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		if conn.Next() {
			conn.Scan(&userId)
		}

		if userId.Valid {
			// delete existing codes and create a new code

			conn.Exec(
				`
					UPDATE user_auth_validate
					SET used = true
					WHERE user_id = $1
				`,
				userId,
			)

			userAuthValidateId := ulid.Make()
			validateCode = createValidateCode()
			conn.Exec(
				`
					INSERT INTO user_auth_validate
					(user_auth_validate_id, user_id, validate_code)
					VALUES ($1, $2, $3)
				`,
				ulid.ToPg(userAuthValidateId),
				userId,
				validateCode,
			)
		}
	})

	if validateCode != nil {
		result := &AuthValidateCreateCodeResult{
			ValidateCode: validateCode,
		}
		return result, nil
	}

	return nil, error("Invalid login.")
}


type AuthPasswordResetCreateCodeArgs struct {
	UserAuth string `json:"error"`
}

type AuthPasswordResetCreateCodeResult struct {
	ResetCode *string `json:"resetCode"`
	Error *AuthValidateCreateCodeError `json:"error"`
}

type AuthPasswordResetCreateCodeError struct {
	Message string `json:"message"`
}

func AuthPasswordResetCreateCode(
	resetCreateCode model.AuthPasswordResetCreateCodeArgs,
	session *bringyour.ClientSession,
) (model.AuthPasswordResetResult, error) {
	userAuth, _ := normalUserAuthV1(resetCreateCode.userAuth)

	if userAuth == nil {
		result := &AuthPasswordResetCreateCodeResult{
			Error: &AuthPasswordResetCreateCodeError{
				Message: "Invalid user auth."
			}
		}
		return result, nil
	}

	var resetCode *string

	bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
		var userId pgtype.UUID
		conn.Exec(
			`
				SELECT user_id FROM network_user WHERE user_auth = $1
			`,
			userAuth,
		)
		if conn.Next() {
			conn.Scan(&userId)
		}

		if userId.Valid {
			// delete existing codes and create a new code

			conn.Exec(
				`
					UPDATE user_auth_reset
					SET used = true
					WHERE user_id = $1
				`,
				userId,
			)

			userAuthResetId := ulid.Make()
			resetCode = createResetCode()
			conn.Exec(
				`
					INSERT INTO user_auth_reset
					(user_auth_reset_id, user_id, reset_code)
					VALUES ($1, $2, $3)
				`,
				ulid.ToPg(userAuthResetId),
				userId,
				resetCode,
			)
		}
	})

	if resetCode != nil {
		result := &AuthPasswordResetCreateCodeResult{
			ResetCode: resetCode,
		}
		return result, nil
	}

	return nil, error("Invalid login.")
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
	session *bringyour.ClientSession
) (*AuthPasswordSetResult, error) {
	userAuthAttemptId, allow := UserAuthAttempt(nil, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	// 4 hours
	resetValidSeconds := 60 * 60 * 4

	var userId pgtype.UUID
	var userAuthResetId pgtype.UUID
	var networkId pgtype.UUID
	var networkName *string

	bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
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
					now() - ($2 seconds) <= user_auth_reset.reset_time
				INNER JOIN network ON network.admin_user_id = network_user.user_id
			`,
			passwordSet.resetCode,
			resetValidSeconds,
		)
		if conn.Next() {
			conn.Scan(&userId, &userAuthResetId, &networkId, &networkName)
		}
	})

	if userAuthResetId.Valid {
		// valid reset code

		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(passwordSet.password), passwordSalt)

		bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
			conn.Exec(
				`
					UPDATE network_user
					SET password_hash = $1, password_salt = $2
					WHERE user_id = $3
				`,
				passwordHard,
				passwordSalt,
				userId,
			)
			conn.Exec(
				`
					UPDATE user_auth_reset
					SET used = true
					WHERE user_auth_reset_id = $1
				`,
				userAuthResetId,
			)
		})

		SetUserAuthAttemptSuccess(userAuthAttemptId, true)

		result := &AuthPasswordSetResult{
			NetworkId: *ulid.FromPg(networkId)
		}
		return result, nil
	}

	return nil, error("Invalid login.")
}
