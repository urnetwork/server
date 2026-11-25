package handlers

// The SN25 validator rate class mint (POST /sn/validator-class) over the
// reserved fixture's real admission: an admitted validator's class consent
// mints a token the lb front verifies for that client; a refused consent is a
// 403 and an instance that is not current a 503, counted by outcome.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

// One mint request with a class consent for credential, signed by key.
func snValidatorClassTestRequest(tb testing.TB, origin string, authority *snReservedAttemptAuthority, credential string, signedCredential string, key ed25519.PrivateKey, replicaNoID uint64) (int, string) {
	tb.Helper()
	sessionHash, err := protocol.ValidatorAttemptUploadSessionHash(signedCredential)
	if err != nil {
		tb.Fatal(err)
	}
	intent := protocol.ValidatorClassIntent{ActivationHash: authority.digest, ReplicaNoID: replicaNoID, SessionHash: sessionHash, NotAfter: uint64(time.Now().Unix()) + 20}
	copy(intent.VPK[:], key[ed25519.SeedSize:])
	header, err := intent.Sign(key)
	if err != nil {
		tb.Fatal(err)
	}
	request, err := http.NewRequestWithContext(tb.Context(), http.MethodPost, origin+"/sn/validator-class", nil)
	if err != nil {
		tb.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(protocol.ValidatorClassIntentHeader, header)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		tb.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		tb.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// The mint route over lane, with the production data grant.
func newSnValidatorClassEndpoint(tb testing.TB, lane SnReservedAttemptUploadLane) string {
	tb.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSnValidatorClass(w, r, lane, session.MintValidatorClass, controller.GrantSnValidatorData, time.Now)
	}))
	tb.Cleanup(endpoint.Close)
	return endpoint.URL
}

// An admitted validator mints a token for its own client that the front's
// verifier accepts, living no longer than five minutes, naming its hotkey.
// A consent for another session or under another key is refused 403.
func TestSnValidatorClassMintsOnlyForTheAdmittedValidator(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		origin := newSnValidatorClassEndpoint(tb, authority.start(tb, 1))
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		minted := testutil.ToFloat64(snValidatorClassMints.WithLabelValues("minted"))
		refused := testutil.ToFloat64(snValidatorClassMints.WithLabelValues("refused_403"))

		status, body := snValidatorClassTestRequest(tb, origin, authority, credential, credential, authority.key, 1)
		if status != http.StatusOK {
			tb.Fatalf("an admitted validator's mint = %d %q", status, body)
		}
		var result SnValidatorClassResult
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			tb.Fatal(err)
		}
		now := time.Now()
		claims, ok := session.NewValidatorClassVerifier(16, 16).Verify(result.Token, *client.ClientId, now)
		if !ok {
			tb.Fatal("the minted class token does not verify for its client")
		}
		if claims.Hotkey != hex.EncodeToString(authority.record.Hotkey[:]) || claims.NetworkId != client.NetworkId ||
			result.ExpiresAt.After(now.Add(session.ValidatorClassMaximumLifetime)) || !result.ExpiresAt.After(now) {
			tb.Fatalf("class claims %+v expiring %s differ from the admitted validator's", claims, result.ExpiresAt)
		}
		if _, ok := session.NewValidatorClassVerifier(16, 16).Verify(result.Token, server.NewId(), now); ok {
			tb.Fatal("the class token verified for another client")
		}

		otherKey := ed25519.NewKeyFromSeed(append([]byte{0x42}, make([]byte, 31)...))
		for name, request := range map[string]func() (int, string){
			"another session": func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, credential, "another.session.credential", authority.key, 1)
			},
			"another key": func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, credential, credential, otherKey, 1)
			},
			"another replica": func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, credential, credential, authority.key, 2)
			},
		} {
			if status, body := request(); status != http.StatusForbidden {
				tb.Fatalf("%s: mint = %d %q, want 403", name, status, body)
			}
		}
		if got := testutil.ToFloat64(snValidatorClassMints.WithLabelValues("minted")) - minted; got != 1 {
			tb.Errorf("%v mints counted, want 1", got)
		}
		if got := testutil.ToFloat64(snValidatorClassMints.WithLabelValues("refused_403")) - refused; got != 3 {
			tb.Errorf("%v 403 refusals counted, want 3", got)
		}
	})
}

// An instance whose admission has not refreshed cannot judge the validator,
// and answers 503, so the validator retries a sibling. It grants no data.
func TestSnValidatorClassNotCurrentAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		hold := make(chan struct{})
		authority.holdEventScan.Store(&hold)
		// runs after the owner's close would wait on the held read
		tb.Cleanup(func() { close(hold) })
		origin := newSnValidatorClassEndpoint(tb, authority.startUnrefreshed(tb, 1))
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		refused := testutil.ToFloat64(snValidatorClassMints.WithLabelValues("refused_503"))
		if status, body := snValidatorClassTestRequest(tb, origin, authority, credential, credential, authority.key, 1); status != http.StatusServiceUnavailable {
			tb.Fatalf("a mint on an instance that has not refreshed = %d %q, want 503", status, body)
		}
		if got := testutil.ToFloat64(snValidatorClassMints.WithLabelValues("refused_503")) - refused; got != 1 {
			tb.Errorf("%v 503 refusals counted, want 1", got)
		}
		if balances := snValidatorDataGrantTestBalances(tb, client.NetworkId); len(balances) != 0 {
			tb.Errorf("a mint answered 503 left %d balances, want no data grant", len(balances))
		}
	})
}
