package session

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

func testingUnextendedMints(kind string) float64 {
	return testutil.ToFloat64(sessionMintUnextendedCounter.WithLabelValues(kind))
}

// A tagged credential as it is presented: a session and an expiry.
func testingPresentedCredential(expiresAt time.Time) *ByJwt {
	credential := NewByJwt(server.NewId(), server.NewId(), "carried", false, false)
	sid := server.NewId()
	credential.SessionId = &sid
	credential.ExpiresAt = gojwt.NewNumericDate(expiresAt)
	return credential
}

// A renewal carries its presented credential's session and expiry. A renewal
// of that renewal keeps the presented expiry, not the fresh one between, a
// copy keeps what a credential carries, and a credential without a session,
// or moved off the session it carries, carries nothing.
func TestRenewalCarriesThePresentedSessionAndItsExpiry(t *testing.T) {
	presentedExpiry := server.NowUtc().Add(time.Hour).Truncate(time.Second)
	presented := testingPresentedCredential(presentedExpiry)
	if presented.carried != nil {
		t.Fatal("a presented credential carries a session itself")
	}
	renewed := presented.Renew()
	if renewed.carried == nil || renewed.carried.sessionId != *presented.SessionId || !renewed.carried.expiresAt.Equal(presentedExpiry) {
		t.Fatalf("the renewal carries %+v, want %s until %v", renewed.carried, presented.SessionId, presentedExpiry)
	}
	if !renewed.ExpiresAt.Time.After(presentedExpiry) {
		t.Fatalf("the renewal's own expiry %v is not fresh", renewed.ExpiresAt)
	}
	if again := renewed.Renew(); again.carried == nil || !again.carried.expiresAt.Equal(presentedExpiry) {
		t.Fatalf("a renewal of the renewal carries %+v, want the presented expiry %v", again.carried, presentedExpiry)
	}
	copied := renewed.clone()
	if copied.carried == nil || copied.carried == renewed.carried || *copied.carried != *renewed.carried {
		t.Fatalf("the copy carries %+v, want a copy of %+v", copied.carried, renewed.carried)
	}

	moved := presented.Renew()
	other := server.NewId()
	moved.SessionId = &other
	if carried := moved.Renew().carried; carried != nil {
		t.Fatalf("a credential moved off its carried session carries %+v", carried)
	}
	if carried := NewByJwt(server.NewId(), server.NewId(), "untagged", false, false).Renew().carried; carried != nil {
		t.Fatalf("an untagged credential carries %+v", carried)
	}
}

// The no-extend fallback keeps a mint on the session it carries, expiring no
// later than the carried expiry: a later expiry is lowered to it, an earlier
// one stays, and the mint is counted by kind. A credential signed with another
// session, one that carries none, and one past the carried expiry are left as
// they were, uncounted, and their mints stay unavailable.
func TestUnextendedMintKeepsToTheCarriedExpiry(t *testing.T) {
	presentedExpiry := server.NowUtc().Add(time.Hour).Truncate(time.Second)
	presented := testingPresentedCredential(presentedExpiry)

	signins := testingUnextendedMints(SessionKindSignin)
	renewed := presented.Renew()
	if !unextendCarriedSessionMint(renewed, "legacy") {
		t.Fatal("the renewal of a tagged credential was not kept on its session")
	}
	if !renewed.ExpiresAt.Time.Equal(presentedExpiry) || renewed.SessionId == nil || *renewed.SessionId != *presented.SessionId {
		t.Fatalf("the unextended renewal expires %v on %v, want %v on %s", renewed.ExpiresAt, renewed.SessionId, presentedExpiry, presented.SessionId)
	}
	if got := testingUnextendedMints(SessionKindSignin) - signins; got != 1 {
		t.Errorf("counted %.0f unextended sign-in mints, want 1", got)
	}

	// a client minted under the credential, with an earlier expiry of its own
	clients := testingUnextendedMints(SessionKindClient)
	client := presented.Renew()
	deviceId, clientId := server.NewId(), server.NewId()
	client.DeviceId, client.ClientId = &deviceId, &clientId
	earlier := presentedExpiry.Add(-time.Minute)
	client.ExpiresAt = gojwt.NewNumericDate(earlier)
	if !unextendCarriedSessionMint(client, "legacy") || !client.ExpiresAt.Time.Equal(earlier) {
		t.Fatalf("the client mint expires %v, want its own earlier %v", client.ExpiresAt, earlier)
	}
	if got := testingUnextendedMints(SessionKindClient) - clients; got != 1 {
		t.Errorf("counted %.0f unextended client mints, want 1", got)
	}

	moved := presented.Renew()
	other := server.NewId()
	moved.SessionId = &other
	fabricated := NewByJwt(presented.NetworkId, presented.UserId, "fabricated", false, false)
	fabricated.SessionId = presented.SessionId
	for name, credential := range map[string]*ByJwt{
		"a credential on another session":      moved,
		"a credential that carries no session": fabricated,
		"a renewal past the carried expiry":    testingPresentedCredential(server.NowUtc().Add(-time.Minute)).Renew(),
	} {
		expiry := credential.ExpiresAt.Time
		sessionId := credential.SessionId
		counted := testingUnextendedMints(SessionKindSignin)
		if unextendCarriedSessionMint(credential, "legacy") {
			t.Errorf("%s was kept on its session", name)
		}
		if !credential.ExpiresAt.Time.Equal(expiry) || credential.SessionId != sessionId {
			t.Errorf("%s was changed", name)
		}
		if got := testingUnextendedMints(SessionKindSignin) - counted; got != 0 {
			t.Errorf("%s was counted", name)
		}
	}
}
