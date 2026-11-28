package model

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A verification code is stored per user (user_auth_verify has no address
// column), and a user can hold an email, a phone and a legacy address. So a
// code proves only "sent to one of the user's addresses", and a code mailed to
// one address used to verify another.
//
// The code's row id carries what the code was sent for, with no schema change:
//   - bytes 0-7 are a keyed hash of the user, the code and the address it was
//     sent to, so a code verifies only that address;
//   - bytes 8-15 are, for a code sent right after a request proved the
//     account's password, a keyed hash of the same and of that client's
//     network (the /29 or /56 the attempt limits use); for any other code,
//     random. The low bit of byte 15 says which.
//
// The client binding lets the person who set a sign-up's password keep it when
// they verify (verifyCodeBoundAddressPassword), while a code that reaches the
// address any other way, including one mailed after someone else's password
// sign-in, proves the address alone. Ids written before this have random bytes
// and are unbound: AuthVerify still takes one for an account whose only
// address is the one verified, so codes sent across the deploy keep working.
//
// The version and variant bits of an RFC 9562 version 8 UUID are set in bytes
// 6 and 8, so the id is a well-formed UUID; they are left out of the
// comparisons.

type verifyCodeBinding int

const (
	// a row id no address of the user matches: a code written before codes
	// were bound
	verifyCodeUnbound verifyCodeBinding = iota
	// sent to another of the user's addresses
	verifyCodeBoundElsewhere
	// sent to this address
	verifyCodeBoundAddress
	// sent to this address right after this client proved the account's
	// password
	verifyCodeBoundAddressPassword
)

const (
	verifyCodeAddressDomain  = "auth-verify-code-address\x00"
	verifyCodePasswordDomain = "auth-verify-code-password\x00"
)

// The low bit of byte 15: the code was sent after a password proof.
const verifyCodePasswordFlag = 0x01

// The binding's key, the password pepper. Tests without a vault set their own.
var verifyCodeKey = func() []byte {
	return passwordPepper()
}

func verifyCodeMac(domain string, userId server.Id, verifyCode string, userAuth string, clientKey *[32]byte) []byte {
	mac := hmac.New(sha256.New, verifyCodeKey())
	write := func(b []byte) {
		_, err := mac.Write(b)
		server.Raise(err)
	}
	write([]byte(domain))
	write(userId.Bytes())
	write([]byte(verifyCode))
	write([]byte{0})
	write([]byte(userAuth))
	if clientKey != nil {
		write([]byte{0})
		write(clientKey[:])
	}
	return mac.Sum(nil)
}

// The row id of a verification code sent to userAuth, a normalized email or
// phone of userId. passwordClientKey is the client network of a request that
// proved the account's password, or nil.
func newVerifyCodeId(userId server.Id, verifyCode string, userAuth string, passwordClientKey *[32]byte) server.Id {
	id := make([]byte, 16)
	copy(id[0:8], verifyCodeMac(verifyCodeAddressDomain, userId, verifyCode, userAuth, nil)[0:8])
	if passwordClientKey != nil {
		copy(id[8:16], verifyCodeMac(verifyCodePasswordDomain, userId, verifyCode, userAuth, passwordClientKey)[0:8])
		id[15] |= verifyCodePasswordFlag
	} else {
		_, err := rand.Read(id[8:16])
		server.Raise(err)
		id[15] &^= verifyCodePasswordFlag
	}
	// RFC 9562 version 8, variant 10
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return server.RequireIdFromBytes(id)
}

func verifyCodeAddressMatches(id server.Id, userId server.Id, verifyCode string, userAuth string) bool {
	want := verifyCodeMac(verifyCodeAddressDomain, userId, verifyCode, userAuth, nil)
	return hmac.Equal(id[0:6], want[0:6]) &&
		id[6]&0x0f == want[6]&0x0f &&
		id[7] == want[7]
}

func verifyCodePasswordMatches(id server.Id, userId server.Id, verifyCode string, userAuth string, clientKey [32]byte) bool {
	want := verifyCodeMac(verifyCodePasswordDomain, userId, verifyCode, userAuth, &clientKey)
	return id[8]&0x3f == want[0]&0x3f &&
		hmac.Equal(id[9:15], want[1:7]) &&
		id[15]&^verifyCodePasswordFlag == want[7]&^verifyCodePasswordFlag
}

// What a stored code proves for userAuth: which address it was sent to, and
// whether clientKey is the client network that proved the password before it
// was sent. otherUserAuths are the user's other addresses. issuedAfterPassword
// is whether the code was sent after any password proof, whatever the client.
func classifyVerifyCode(
	id server.Id,
	userId server.Id,
	verifyCode string,
	userAuth string,
	otherUserAuths []string,
	clientKey *[32]byte,
) (binding verifyCodeBinding, issuedAfterPassword bool) {
	if !verifyCodeAddressMatches(id, userId, verifyCode, userAuth) {
		for _, otherUserAuth := range otherUserAuths {
			if otherUserAuth != userAuth && verifyCodeAddressMatches(id, userId, verifyCode, otherUserAuth) {
				return verifyCodeBoundElsewhere, false
			}
		}
		return verifyCodeUnbound, false
	}
	issuedAfterPassword = id[15]&verifyCodePasswordFlag != 0
	if issuedAfterPassword && clientKey != nil && verifyCodePasswordMatches(id, userId, verifyCode, userAuth, *clientKey) {
		return verifyCodeBoundAddressPassword, true
	}
	return verifyCodeBoundAddress, issuedAfterPassword
}

// Whether the user has a live code for userAuth that was sent after clientKey's
// client network proved the account's password, read in the caller's
// transaction. A resend from that client keeps the proof, so a person who asks
// for another code while signing up keeps their password. So does a code that
// wrong tries killed (killVerifyCode) before it expired: the person who
// mistyped it asks for another.
func liveVerifyCodeProvenInTx(ctx context.Context, tx server.PgTx, userId server.Id, userAuth string, clientKey [32]byte) (proven bool) {
	result, err := tx.Query(
		ctx,
		`
			SELECT user_auth_verify_id, verify_code
			FROM user_auth_verify
			WHERE
				user_id = $1 AND
				now() - INTERVAL '1 seconds' * $2 <= verify_time AND
				(
					used = false OR
					EXISTS (
						SELECT 1 FROM network_user_action_attempt
						WHERE
							network_user_action_attempt.user_id = user_auth_verify.user_auth_verify_id AND
							network_user_action_attempt.action = $3
					)
				)
		`,
		userId,
		int(VerifyCodeTimeout/time.Second),
		AccountActionVerifyCodeKilled,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var id server.Id
			var verifyCode string
			server.Raise(result.Scan(&id, &verifyCode))
			binding, _ := classifyVerifyCode(id, userId, verifyCode, userAuth, nil, &clientKey)
			if binding == verifyCodeBoundAddressPassword {
				proven = true
			}
		}
	})
	return
}

// The client network a verification code is bound to: the keyed hash of the
// request's /29 (IPv4) or /56 (IPv6), or nil when the request has no address.
func verifyCodeClientKey(clientSession *session.ClientSession) *[32]byte {
	clientAddressHash, _, err := clientSession.ClientAddressHashPort()
	if err != nil {
		return nil
	}
	return &clientAddressHash
}
