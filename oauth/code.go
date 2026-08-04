package oauth

// Authorization codes and refresh tokens.
//
// Both are opaque random values stored only as a hash: the raw value exists in
// the response to the client and nowhere else, so a database read cannot
// recover a usable credential.
//
// Codes are single use. A second redemption is not merely refused -- under
// oauth 2.1 it means the code leaked, so the redemption is recorded and the
// replay is detectable. Refresh tokens rotate for the same reason: each use
// issues a successor in the same family and retires its predecessor, and
// presenting a retired token revokes the entire family.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

var (
	ErrInvalidCode         = errors.New("invalid authorization code")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrPkceFailed          = errors.New("pkce verification failed")
)

const (
	PkceMethodS256 = "S256"
	// oauth 2.1 removes `plain`; only S256 is accepted
)

// A minted authorization code, and everything the token exchange must check
// it against.
type AuthorizationCode struct {
	ClientId            string
	UserId              server.Id
	NetworkId           server.Id
	RedirectUri         string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	Scopes              []string
	Nonce               string
	Principal           string
	Roles               []string
	AuthTime            time.Time
}

// Mints a single use authorization code and returns the raw value, which is
// the only place it exists in the clear.
func CreateAuthorizationCode(ctx context.Context, code *AuthorizationCode) (string, error) {
	if code.CodeChallengeMethod != PkceMethodS256 {
		return "", fmt.Errorf("unsupported code_challenge_method %q", code.CodeChallengeMethod)
	}
	if code.Resource == "" {
		return "", fmt.Errorf("resource is required")
	}

	codeStr, err := newOpaqueToken()
	if err != nil {
		return "", err
	}

	rolesJson, err := json.Marshal(code.Roles)
	if err != nil {
		return "", err
	}

	now := server.NowUtc()
	codeHash := tokenHash(codeStr)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO oauth_authorization_code (
				code_hash,
				client_id,
				user_id,
				network_id,
				redirect_uri,
				code_challenge,
				code_challenge_method,
				resource,
				scope,
				nonce,
				principal,
				roles_json,
				auth_time,
				create_time,
				expire_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			`,
			codeHash,
			code.ClientId,
			code.UserId,
			code.NetworkId,
			code.RedirectUri,
			code.CodeChallenge,
			code.CodeChallengeMethod,
			code.Resource,
			FormatScope(code.Scopes),
			code.Nonce,
			code.Principal,
			string(rolesJson),
			code.AuthTime,
			now,
			now.Add(AuthorizationCodeDuration),
		))
	})

	return codeStr, nil
}

// Redeems a code, verifying the pkce verifier, the redirect uri, the client,
// and the resource. The redemption and all checks happen in one transaction so
// two concurrent exchanges cannot both succeed.
func RedeemAuthorizationCode(
	ctx context.Context,
	codeStr string,
	clientId string,
	redirectUri string,
	codeVerifier string,
	resource string,
) (code *AuthorizationCode, returnErr error) {
	codeHash := tokenHash(codeStr)

	server.Tx(ctx, func(tx server.PgTx) {
		var (
			storedClientId    string
			userId            server.Id
			networkId         server.Id
			storedRedirectUri string
			codeChallenge     string
			challengeMethod   string
			storedResource    string
			scope             string
			nonce             *string
			principal         *string
			rolesJson         *string
			authTime          time.Time
			expireTime        time.Time
			redeemTime        *time.Time
		)

		result, err := tx.Query(
			ctx,
			`
			SELECT
				client_id,
				user_id,
				network_id,
				redirect_uri,
				code_challenge,
				code_challenge_method,
				resource,
				scope,
				nonce,
				principal,
				roles_json,
				auth_time,
				expire_time,
				redeem_time
			FROM oauth_authorization_code
			WHERE code_hash = $1
			FOR UPDATE
			`,
			codeHash,
		)

		found := false
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(
					&storedClientId,
					&userId,
					&networkId,
					&storedRedirectUri,
					&codeChallenge,
					&challengeMethod,
					&storedResource,
					&scope,
					&nonce,
					&principal,
					&rolesJson,
					&authTime,
					&expireTime,
					&redeemTime,
				))
			}
		})

		if !found {
			returnErr = ErrInvalidCode
			return
		}

		if redeemTime != nil {
			// oauth 2.1: a replayed code means it leaked. Nothing is issued,
			// and the caller is expected to revoke anything already derived
			// from it.
			returnErr = fmt.Errorf("%w: already redeemed", ErrInvalidCode)
			return
		}
		if server.NowUtc().After(expireTime) {
			returnErr = fmt.Errorf("%w: expired", ErrInvalidCode)
			return
		}
		if storedClientId != clientId {
			returnErr = fmt.Errorf("%w: client mismatch", ErrInvalidCode)
			return
		}
		if storedRedirectUri != redirectUri {
			returnErr = fmt.Errorf("%w: redirect_uri mismatch", ErrInvalidCode)
			return
		}
		// the resource the token is minted for must be the one consent was
		// given for, or the audience binding means nothing
		if storedResource != resource {
			returnErr = fmt.Errorf("%w: resource mismatch", ErrInvalidCode)
			return
		}
		if !verifyPkce(codeChallenge, codeVerifier) {
			returnErr = ErrPkceFailed
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE oauth_authorization_code
			SET redeem_time = $2
			WHERE code_hash = $1
			`,
			codeHash,
			server.NowUtc(),
		))

		roles := []string{}
		if rolesJson != nil {
			json.Unmarshal([]byte(*rolesJson), &roles)
		}

		code = &AuthorizationCode{
			ClientId:            storedClientId,
			UserId:              userId,
			NetworkId:           networkId,
			RedirectUri:         storedRedirectUri,
			CodeChallenge:       codeChallenge,
			CodeChallengeMethod: challengeMethod,
			Resource:            storedResource,
			Scopes:              ParseScope(scope),
			AuthTime:            authTime,
		}
		if nonce != nil {
			code.Nonce = *nonce
		}
		if principal != nil {
			code.Principal = *principal
		}
		code.Roles = roles
	})

	return
}

// The grant a refresh token carries forward.
type RefreshToken struct {
	FamilyId  server.Id
	ClientId  string
	UserId    server.Id
	NetworkId server.Id
	Resource  string
	Scopes    []string
	Principal string
	Roles     []string
	AuthTime  time.Time
}

// Issues a refresh token in a new family. Used at code exchange.
func CreateRefreshToken(ctx context.Context, refreshToken *RefreshToken) (string, error) {
	refreshToken.FamilyId = server.NewId()
	return insertRefreshToken(ctx, refreshToken)
}

// Rotates a refresh token: verifies the presented one, retires it, and issues
// a successor in the same family.
//
// Presenting an already-rotated or revoked token revokes the whole family.
// That is the reuse detection oauth 2.1 requires for public clients: a token
// that has already been exchanged should never be seen again, so seeing it
// means either the client or an attacker has a copy, and neither should keep
// working.
//
// `requestedScopes` narrows the grant, and is validated INSIDE the transaction
// before the token is marked rotated. Checking it afterwards would consume the
// token on a request that then fails, leaving the caller holding a dead token
// and tripping reuse detection on its next honest attempt.
func RotateRefreshToken(
	ctx context.Context,
	tokenStr string,
	clientId string,
	requestedScopes []string,
) (refreshToken *RefreshToken, nextTokenStr string, returnErr error) {
	tokenHashValue := tokenHash(tokenStr)

	var (
		familyId    server.Id
		reuseFamily bool
	)

	server.Tx(ctx, func(tx server.PgTx) {
		var (
			storedClientId string
			userId         server.Id
			networkId      server.Id
			resource       string
			scope          string
			principal      *string
			rolesJson      *string
			authTime       time.Time
			expireTime     time.Time
			rotateTime     *time.Time
			revokeTime     *time.Time
		)

		result, err := tx.Query(
			ctx,
			`
			SELECT
				family_id,
				client_id,
				user_id,
				network_id,
				resource,
				scope,
				principal,
				roles_json,
				auth_time,
				expire_time,
				rotate_time,
				revoke_time
			FROM oauth_refresh_token
			WHERE token_hash = $1
			FOR UPDATE
			`,
			tokenHashValue,
		)

		found := false
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(
					&familyId,
					&storedClientId,
					&userId,
					&networkId,
					&resource,
					&scope,
					&principal,
					&rolesJson,
					&authTime,
					&expireTime,
					&rotateTime,
					&revokeTime,
				))
			}
		})

		if !found {
			returnErr = ErrInvalidRefreshToken
			return
		}

		if rotateTime != nil || revokeTime != nil {
			// reuse of a retired token: treat the family as compromised
			reuseFamily = true
			returnErr = fmt.Errorf("%w: reused", ErrInvalidRefreshToken)
			return
		}
		if server.NowUtc().After(expireTime) {
			returnErr = fmt.Errorf("%w: expired", ErrInvalidRefreshToken)
			return
		}
		if storedClientId != clientId {
			returnErr = fmt.Errorf("%w: client mismatch", ErrInvalidRefreshToken)
			return
		}

		grantedScopes := ParseScope(scope)
		// a refresh may narrow the grant, never widen it. Checked before the
		// update so a rejected request leaves the token usable: consuming it
		// here would leave an honest client holding a dead token, and its next
		// attempt would look like reuse and revoke the family
		if 0 < len(requestedScopes) {
			for _, requested := range requestedScopes {
				if !HasScope(grantedScopes, requested) {
					returnErr = fmt.Errorf("%w: scope exceeds the grant", ErrInvalidRefreshToken)
					return
				}
			}
			grantedScopes = requestedScopes
		}
		scope = FormatScope(grantedScopes)

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE oauth_refresh_token
			SET rotate_time = $2
			WHERE token_hash = $1
			`,
			tokenHashValue,
			server.NowUtc(),
		))

		roles := []string{}
		if rolesJson != nil {
			json.Unmarshal([]byte(*rolesJson), &roles)
		}

		refreshToken = &RefreshToken{
			FamilyId:  familyId,
			ClientId:  storedClientId,
			UserId:    userId,
			NetworkId: networkId,
			Resource:  resource,
			Scopes:    ParseScope(scope),
			AuthTime:  authTime,
			Roles:     roles,
		}
		if principal != nil {
			refreshToken.Principal = *principal
		}
	})

	if reuseFamily {
		// outside the transaction above, which is rolling back its own row
		RevokeRefreshTokenFamily(ctx, familyId)
		return nil, "", returnErr
	}
	if returnErr != nil {
		return nil, "", returnErr
	}

	// the successor slides the window forward from now
	nextTokenStr, err := insertRefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, "", err
	}

	return refreshToken, nextTokenStr, nil
}

func insertRefreshToken(ctx context.Context, refreshToken *RefreshToken) (string, error) {
	tokenStr, err := newOpaqueToken()
	if err != nil {
		return "", err
	}

	rolesJson, err := json.Marshal(refreshToken.Roles)
	if err != nil {
		return "", err
	}

	now := server.NowUtc()

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO oauth_refresh_token (
				token_hash,
				family_id,
				client_id,
				user_id,
				network_id,
				resource,
				scope,
				principal,
				roles_json,
				auth_time,
				create_time,
				expire_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			`,
			tokenHash(tokenStr),
			refreshToken.FamilyId,
			refreshToken.ClientId,
			refreshToken.UserId,
			refreshToken.NetworkId,
			refreshToken.Resource,
			FormatScope(refreshToken.Scopes),
			refreshToken.Principal,
			string(rolesJson),
			refreshToken.AuthTime,
			now,
			now.Add(RefreshTokenDuration),
		))
	})

	return tokenStr, nil
}

// Revokes every token in a family. Used on reuse detection and on an explicit
// disconnect.
func RevokeRefreshTokenFamily(ctx context.Context, familyId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE oauth_refresh_token
			SET revoke_time = $2
			WHERE family_id = $1 AND revoke_time IS NULL
			`,
			familyId,
			server.NowUtc(),
		))
	})
}

// Revokes a single refresh token by value, per rfc 7009. Revoking one token of
// a family retires the family: the client is telling us this grant is done.
func RevokeRefreshToken(ctx context.Context, tokenStr string) {
	var familyId server.Id
	found := false

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT family_id
			FROM oauth_refresh_token
			WHERE token_hash = $1
			`,
			tokenHash(tokenStr),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&familyId))
			}
		})
	})

	if found {
		RevokeRefreshTokenFamily(ctx, familyId)
	}
}

// Removes codes and refresh tokens that have expired, returning how many of
// each. Redeemed and rotated rows are kept until expiry so replay stays
// detectable; after that they carry no information.
//
// The counts are returned rather than logged here so the caller decides what
// to report -- and so a reaper that silently does nothing is distinguishable
// from one that is not running.
func ReapOauthTokens(ctx context.Context) (codeCount int64, refreshTokenCount int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()

		codeTag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM oauth_authorization_code WHERE expire_time < $1`,
			now,
		))
		codeCount = codeTag.RowsAffected()

		refreshTokenTag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM oauth_refresh_token WHERE expire_time < $1`,
			now,
		))
		refreshTokenCount = refreshTokenTag.RowsAffected()
	})
	return
}

// rfc 7636: BASE64URL(SHA256(ASCII(code_verifier))) == code_challenge.
// Compared in constant time; the challenge is not a secret but the comparison
// is cheap and the habit is worth keeping.
func verifyPkce(codeChallenge string, codeVerifier string) bool {
	if codeVerifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(codeVerifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(codeChallenge)) == 1
}

// 256 bits of entropy, url safe.
func newOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenHash(tokenStr string) []byte {
	sum := sha256.Sum256([]byte(tokenStr))
	return sum[:]
}
