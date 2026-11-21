package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Root cause (owner decision 2026-11-20): an email or phone row that nobody
// verified was trusted as the account's identity. Google or Apple sign-in
// joined the account that held it, a verification code signed in to that
// account and made the password whoever made the row chose work, and a row
// added to an account with no email of its own waited there for the address's
// owner. These tests pin the decisions alone, without a database.

// A pending hold rests on an unverified row alone; one the account also holds
// by a proof is not pending, nor is a legacy identity without a row.
func TestUserAuthHoldPending(t *testing.T) {
	now := server.NowUtc()
	fresh := now.Add(-time.Hour)
	old := now.Add(-UnverifiedUserAuthClaimTimeout - time.Minute)
	for _, c := range []struct {
		name    string
		hold    userAuthHold
		pending bool
		signUp  bool
		stale   bool
	}{
		{"unverified sign-up", userAuthHold{passwordRow: true, passwordCreateTime: fresh, legacy: true, createAuthType: AuthTypePassword}, true, true, false},
		{"stale unverified sign-up", userAuthHold{passwordRow: true, passwordCreateTime: old, legacy: true, createAuthType: AuthTypePassword}, true, true, true},
		{"unverified added email", userAuthHold{passwordRow: true, passwordCreateTime: fresh, createAuthType: AuthTypeSeedphrase}, true, false, false},
		{"stale unverified added email", userAuthHold{passwordRow: true, passwordCreateTime: old, createAuthType: AuthTypeSeedphrase}, true, false, true},
		{"verified sign-up", userAuthHold{passwordRow: true, passwordVerified: true, passwordCreateTime: old, legacy: true, createAuthType: AuthTypePassword}, false, true, false},
		{"unverified row beside a google sign-in", userAuthHold{passwordRow: true, passwordCreateTime: old, sso: true, legacy: true, createAuthType: AuthTypePassword}, false, true, false},
		{"unverified row of a google account's own email", userAuthHold{passwordRow: true, passwordCreateTime: old, legacy: true, createAuthType: AuthTypeGoogle}, false, false, false},
		{"legacy identity without a row", userAuthHold{legacy: true, createAuthType: AuthTypePassword}, false, true, false},
		{"apple sign-in", userAuthHold{sso: true}, false, false, false},
	} {
		if got := c.hold.pending(); got != c.pending {
			t.Errorf("%s: pending = %t, want %t", c.name, got, c.pending)
		}
		if got := c.hold.signUp(); got != c.signUp {
			t.Errorf("%s: signUp = %t, want %t", c.name, got, c.signUp)
		}
		if got := c.hold.stale(now); got != c.stale {
			t.Errorf("%s: stale = %t, want %t", c.name, got, c.stale)
		}
		// a proof releases any pending hold at once; another claim only a
		// stale one; nothing releases a hold that is not pending
		if got := c.hold.yields(true, now); got != c.pending {
			t.Errorf("%s: yields to a proof = %t, want %t", c.name, got, c.pending)
		}
		if got := c.hold.yields(false, now); got != c.stale {
			t.Errorf("%s: yields to a claim = %t, want %t", c.name, got, c.stale)
		}
	}
}

// The hold expires exactly UnverifiedUserAuthClaimTimeout after its row was
// added.
func TestUserAuthHoldStaleBoundary(t *testing.T) {
	created := server.NowUtc()
	hold := userAuthHold{passwordRow: true, passwordCreateTime: created, legacy: true, createAuthType: AuthTypePassword}
	if hold.stale(created.Add(UnverifiedUserAuthClaimTimeout - time.Microsecond)) {
		t.Fatal("stale before the timeout")
	}
	if !hold.stale(created.Add(UnverifiedUserAuthClaimTimeout)) {
		t.Fatal("not stale at the timeout")
	}
	if UnverifiedUserAuthClaimTimeout != 72*time.Hour {
		t.Fatalf("timeout = %s, want 72h", UnverifiedUserAuthClaimTimeout)
	}
}

// What a valid code does, for every hold and proof: a verified address signs
// in; a pending one is verified only for a caller who controls the account
// (signed in, or proved its password on this network) or when the account
// holds the address by a proof; otherwise a sign-up verified for the first
// time goes to its verifier with every earlier credential ended, and an
// address added to an account never joins it.
func TestDecideAuthVerify(t *testing.T) {
	signUp := userAuthHold{passwordRow: true, legacy: true, createAuthType: AuthTypePassword}
	signUpWithGoogle := userAuthHold{passwordRow: true, legacy: true, createAuthType: AuthTypePassword, sso: true}
	added := userAuthHold{passwordRow: true, createAuthType: AuthTypeSeedphrase}
	addedToGoogleAccount := userAuthHold{passwordRow: true, legacy: true, createAuthType: AuthTypeGoogle}
	verified := userAuthHold{passwordRow: true, passwordVerified: true, createAuthType: AuthTypeSeedphrase}
	legacyIdentity := userAuthHold{legacy: true, createAuthType: AuthTypeGoogle}

	none := authVerifyProof{}
	signedIn := authVerifyProof{signedIn: true}
	password := authVerifyProof{password: true}
	passwordElsewhere := authVerifyProof{passwordElsewhere: true}

	for _, c := range []struct {
		name string
		hold userAuthHold
		// the account holds another address by a proof
		accountProven bool
		proof         authVerifyProof
		want          authVerifyOutcome
	}{
		{"verified, stranger", verified, false, none, authVerifySignIn},
		{"legacy identity, stranger", legacyIdentity, false, none, authVerifySignIn},

		{"sign-up, its own client", signUp, false, password, authVerifyVerify},
		{"sign-up, signed in", signUp, false, signedIn, authVerifyVerifyRetiringPassword},
		{"sign-up, stranger", signUp, false, none, authVerifyTakeOver},
		{"sign-up, password proved elsewhere", signUp, false, passwordElsewhere, authVerifyTakeOver},
		{"sign-up with a verified phone, stranger", signUp, true, none, authVerifyRelease},
		{"sign-up joined by google, stranger", signUpWithGoogle, false, none, authVerifyVerifyRetiringPassword},
		{"sign-up joined by google, its own client", signUpWithGoogle, false, password, authVerifyVerify},

		{"added, signed in", added, false, signedIn, authVerifyVerify},
		{"added, password proved here", added, false, password, authVerifyVerify},
		{"added, password proved elsewhere", added, false, passwordElsewhere, authVerifyRefuse},
		{"added, stranger", added, false, none, authVerifyRelease},
		{"added to an account with a verified phone, stranger", added, true, none, authVerifyRelease},
		{"added to a google account with its email, stranger", addedToGoogleAccount, false, none, authVerifyVerify},
	} {
		hold := c.hold
		if got := decideAuthVerify(&hold, c.accountProven, c.proof); got != c.want {
			t.Errorf("%s: outcome = %d, want %d", c.name, got, c.want)
		}
	}
}

// A code is bound to the address it was sent to, and when it was sent after a
// password proof, to that client network.
func TestVerifyCodeBinding(t *testing.T) {
	previousKey := verifyCodeKey
	verifyCodeKey = func() []byte { return []byte("synthetic-verify-code-key") }
	defer func() { verifyCodeKey = previousKey }()

	userId := server.NewId()
	email := "binding@example.com"
	phone := "+1 609-737-0031"
	code := "123456"
	here := [32]byte{1}
	there := [32]byte{2}

	plain := newVerifyCodeId(userId, code, email, nil)
	if binding, after := classifyVerifyCode(plain, userId, code, email, []string{phone}, &here); binding != verifyCodeBoundAddress || after {
		t.Fatalf("plain code for its address = %d %t, want bound to the address", binding, after)
	}
	// another of the user's addresses
	if binding, _ := classifyVerifyCode(plain, userId, code, phone, []string{email}, &here); binding != verifyCodeBoundElsewhere {
		t.Fatalf("plain code for another address = %d, want bound elsewhere", binding)
	}
	// another user, or another code, is unbound
	if binding, _ := classifyVerifyCode(plain, server.NewId(), code, email, nil, &here); binding != verifyCodeUnbound {
		t.Fatalf("code of another user = %d, want unbound", binding)
	}
	if binding, _ := classifyVerifyCode(plain, userId, "654321", email, nil, &here); binding != verifyCodeUnbound {
		t.Fatalf("another code = %d, want unbound", binding)
	}
	// a row id from before the binding
	if binding, _ := classifyVerifyCode(server.NewId(), userId, code, email, []string{phone}, &here); binding != verifyCodeUnbound {
		t.Fatalf("legacy row id = %d, want unbound", binding)
	}

	proven := newVerifyCodeId(userId, code, email, &here)
	if binding, after := classifyVerifyCode(proven, userId, code, email, nil, &here); binding != verifyCodeBoundAddressPassword || !after {
		t.Fatalf("proven code on its client = %d %t, want bound to the address and password", binding, after)
	}
	if binding, after := classifyVerifyCode(proven, userId, code, email, nil, &there); binding != verifyCodeBoundAddress || !after {
		t.Fatalf("proven code on another client = %d %t, want bound to the address, sent after a password", binding, after)
	}
	if binding, after := classifyVerifyCode(proven, userId, code, email, nil, nil); binding != verifyCodeBoundAddress || !after {
		t.Fatalf("proven code on no client = %d %t, want bound to the address", binding, after)
	}
	if binding, _ := classifyVerifyCode(proven, userId, code, phone, []string{email}, &here); binding != verifyCodeBoundElsewhere {
		t.Fatalf("proven code for another address = %d, want bound elsewhere", binding)
	}

	// a plain code never reads as proven, whatever the client
	for i := range 64 {
		id := newVerifyCodeId(userId, code, email, nil)
		key := [32]byte{byte(i)}
		if binding, after := classifyVerifyCode(id, userId, code, email, nil, &key); binding != verifyCodeBoundAddress || after {
			t.Fatalf("plain code %d = %d %t, want bound to the address only", i, binding, after)
		}
	}

	// well-formed version 8 UUIDs
	for _, id := range []server.Id{plain, proven} {
		if id[6]>>4 != 8 || id[8]>>6 != 2 {
			t.Fatalf("id %s is not a version 8 UUID", id)
		}
	}
}
